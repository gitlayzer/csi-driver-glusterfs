package driver

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

const bufSize = 1024 * 1024

// identityServer starts a real gRPC server on a bufconn listener and returns a
// csi.IdentityClient connected to it. The interceptors of the production server
// are used, so the tests exercise them too.
//
// It takes a *testing.T rather than a context so that a leaked server fails the
// test instead of hanging it.
func identityServer(t *testing.T, d *Driver) csi.IdentityClient {
	t.Helper()

	lis := bufconn.Listen(bufSize)
	srv, err := d.newGRPCServer()
	if err != nil {
		t.Fatalf("newGRPCServer() = %v, want nil", err)
	}

	served := make(chan error, 1)
	go func() { served <- srv.Serve(lis) }()

	// The stop path has to run before the listener is closed, otherwise
	// srv.Serve never returns; t.Cleanup runs it in reverse order of
	// registration, so this is the innermost one.
	t.Cleanup(func() {
		srv.Stop()
		if closeErr := lis.Close(); closeErr != nil {
			t.Errorf("close listener: %v", closeErr)
		}
		select {
		case serveErr := <-served:
			if serveErr != nil && !isServerStopped(serveErr) {
				t.Errorf("Serve() = %v, want nil or ErrServerStopped", serveErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve() did not return within 5s of Stop()")
		}
	})

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient() = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close connection: %v", err)
		}
	})

	return csi.NewIdentityClient(conn)
}

// TestIdentityGetPluginInfo covers GetPluginInfo over gRPC.
func TestIdentityGetPluginInfo(t *testing.T) {
	opts := baseOptions()
	opts.DriverName = "example.csi.io"
	opts.Version = "v9.9.9"

	d, err := New(opts)
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}
	client := identityServer(t, d)

	resp, err := client.GetPluginInfo(context.Background(), &csi.GetPluginInfoRequest{})
	if err != nil {
		t.Fatalf("GetPluginInfo() = %v, want nil", err)
	}
	if resp.GetName() != "example.csi.io" {
		t.Fatalf("Name = %q, want %q", resp.GetName(), "example.csi.io")
	}
	if resp.GetVendorVersion() != "v9.9.9" {
		t.Fatalf("VendorVersion = %q, want %q", resp.GetVendorVersion(), "v9.9.9")
	}
	if resp.GetManifest() != nil {
		t.Fatalf("Manifest = %v, want nil", resp.GetManifest())
	}
}

// TestIdentityGetPluginCapabilities covers the capability list.
func TestIdentityGetPluginCapabilities(t *testing.T) {
	tests := []struct {
		name             string
		controllerReg    bool
		wantCapabilities []csi.PluginCapability_Service_Type
	}{
		{
			name:             "no controller service registered",
			controllerReg:    false,
			wantCapabilities: nil,
		},
		{
			name:             "controller service registered",
			controllerReg:    true,
			wantCapabilities: []csi.PluginCapability_Service_Type{csi.PluginCapability_Service_CONTROLLER_SERVICE},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, err := New(baseOptions())
			if err != nil {
				t.Fatalf("New() = %v, want nil", err)
			}
			d.controllerRegistered = tc.controllerReg
			client := identityServer(t, d)

			resp, err := client.GetPluginCapabilities(context.Background(), &csi.GetPluginCapabilitiesRequest{})
			if err != nil {
				t.Fatalf("GetPluginCapabilities() = %v, want nil", err)
			}

			caps := resp.GetCapabilities()
			if len(caps) != len(tc.wantCapabilities) {
				t.Fatalf("capabilities = %v, want %d entries", caps, len(tc.wantCapabilities))
			}
			for i, want := range tc.wantCapabilities {
				got := caps[i].GetService().GetType()
				if got != want {
					t.Fatalf("capability[%d] = %v, want %v", i, got, want)
				}
			}
		})
	}
}

// TestIdentityProbe covers Probe over gRPC for every readiness outcome.
func TestIdentityProbe(t *testing.T) {
	fuseCharDev := fakeFileInfo{mode: 0o666 | modeCharDevice}

	tests := []struct {
		name        string
		mode        Mode
		stat        func(string) (fakeFileInfo, error)
		lookPath    func(string) (string, error)
		cancelled   bool
		wantCode    codes.Code
		wantMessage string // substring of the error message; empty for ready
	}{
		{
			name: "controller mode has no checks",
			mode: ModeController,
			stat: func(string) (fakeFileInfo, error) {
				return fakeFileInfo{}, errFakeStat
			},
			lookPath: func(string) (string, error) { return "", errFakeLookPath },
		},
		{
			name: "node mode with all dependencies present",
			mode: ModeNode,
			stat: func(string) (fakeFileInfo, error) { return fuseCharDev, nil },
			lookPath: func(string) (string, error) {
				return "/usr/sbin/mount.glusterfs", nil
			},
		},
		{
			name:        "fuse device missing",
			mode:        ModeNode,
			stat:        func(string) (fakeFileInfo, error) { return fakeFileInfo{}, errFakeStat },
			lookPath:    func(string) (string, error) { return "/usr/sbin/mount.glusterfs", nil },
			wantCode:    codes.FailedPrecondition,
			wantMessage: fuseDevice + " is not ready",
		},
		{
			name:        "fuse device not a character device",
			mode:        ModeNode,
			stat:        func(string) (fakeFileInfo, error) { return fakeFileInfo{mode: 0o666}, nil },
			lookPath:    func(string) (string, error) { return "/usr/sbin/mount.glusterfs", nil },
			wantCode:    codes.FailedPrecondition,
			wantMessage: "not a character device",
		},
		{
			name:        "mount helper missing",
			mode:        ModeAll,
			stat:        func(string) (fakeFileInfo, error) { return fuseCharDev, nil },
			lookPath:    func(string) (string, error) { return "", errFakeLookPath },
			wantCode:    codes.FailedPrecondition,
			wantMessage: mountHelper + " not found in PATH",
		},
		{
			name: "cancelled context",
			mode: ModeNode,
			stat: func(string) (fakeFileInfo, error) {
				t.Error("stat called with a cancelled context")
				return fuseCharDev, nil
			},
			lookPath: func(string) (string, error) {
				t.Error("lookPath called with a cancelled context")
				return "", nil
			},
			cancelled: true,
			wantCode:  codes.Canceled,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := baseOptions()
			opts.Mode = tc.mode
			opts.NodeID = "node-1"

			d, err := New(opts)
			if err != nil {
				t.Fatalf("New() = %v, want nil", err)
			}
			// Replace the real dependencies: the test must not depend on the
			// machine it runs on.
			d.stat = fakeStatFunc(tc.stat)
			d.lookPath = tc.lookPath
			client := identityServer(t, d)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelled {
				cancel()
			}

			resp, err := client.Probe(ctx, &csi.ProbeRequest{})

			if tc.wantCode != codes.OK {
				if err == nil {
					t.Fatalf("Probe() = %+v, nil, want %v", resp, tc.wantCode)
				}
				if got := status.Code(err); got != tc.wantCode {
					t.Fatalf("Probe() = %v (code %v), want code %v", err, got, tc.wantCode)
				}
				if !strings.Contains(status.Convert(err).Message(), tc.wantMessage) {
					t.Fatalf("Probe() message = %q, want it to contain %q", status.Convert(err).Message(), tc.wantMessage)
				}
				return
			}

			if err != nil {
				t.Fatalf("Probe() = %v, want nil", err)
			}
			if !resp.GetReady().GetValue() {
				t.Fatalf("Probe().Ready = %v, want true", resp.GetReady())
			}
		})
	}
}
