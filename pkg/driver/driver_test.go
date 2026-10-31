package driver

import (
	"context"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

// baseOptions returns a valid option set in controller mode: the mode with the
// fewest requirements, so each test can vary exactly one field.
func baseOptions() Options {
	return Options{
		DriverName: "glusterfs.csi.io",
		Endpoint:   "unix:///tmp/csi.sock",
		Mode:       ModeController,
		Version:    "v0.1.0",
	}
}

func TestNew(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(o *Options)
		wantErr string
	}{
		{
			name:   "controller mode",
			mutate: func(_ *Options) {},
		},
		{
			name:   "node mode with nodeid",
			mutate: func(o *Options) { o.Mode, o.NodeID = ModeNode, "node-1" },
		},
		{
			name:   "all mode with nodeid",
			mutate: func(o *Options) { o.Mode, o.NodeID = ModeAll, "node-1" },
		},
		{
			name:   "controller mode without nodeid",
			mutate: func(o *Options) { o.NodeID = "" },
		},
		{
			name:   "tcp endpoint",
			mutate: func(o *Options) { o.Endpoint = "tcp://127.0.0.1:0" },
		},
		{
			name:   "empty version falls back to unknown",
			mutate: func(o *Options) { o.Version = "" },
		},
		{
			name:   "blank version falls back to unknown",
			mutate: func(o *Options) { o.Version = "   " },
		},

		{
			name:    "node mode without nodeid",
			mutate:  func(o *Options) { o.Mode = ModeNode },
			wantErr: "node id is required",
		},
		{
			name:    "all mode with blank nodeid",
			mutate:  func(o *Options) { o.Mode, o.NodeID = ModeAll, "  " },
			wantErr: "node id is required",
		},
		{
			name:    "invalid mode",
			mutate:  func(o *Options) { o.Mode = "bogus" },
			wantErr: "invalid mode",
		},
		{
			name:    "empty mode",
			mutate:  func(o *Options) { o.Mode = "" },
			wantErr: "invalid mode",
		},
		{
			name:    "invalid driver name",
			mutate:  func(o *Options) { o.DriverName = "a_b" },
			wantErr: "invalid driver name",
		},
		{
			name:    "empty driver name",
			mutate:  func(o *Options) { o.DriverName = "" },
			wantErr: "invalid driver name",
		},
		{
			name:    "invalid endpoint",
			mutate:  func(o *Options) { o.Endpoint = "http://x" },
			wantErr: "endpoint",
		},
		{
			name:    "empty endpoint",
			mutate:  func(o *Options) { o.Endpoint = "" },
			wantErr: "endpoint",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := baseOptions()
			tc.mutate(&opts)

			d, err := New(opts)

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("New() = %+v, nil, want error containing %q", d, tc.wantErr)
				}
				if d != nil {
					t.Fatalf("New() returned a driver together with the error %v", err)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("New() = %q, want error containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("New() = %v, want nil", err)
			}
			if d == nil {
				t.Fatal("New() = nil, want a driver")
			}
			if d.driverName != opts.DriverName {
				t.Fatalf("driverName = %q, want %q", d.driverName, opts.DriverName)
			}
			if d.mode != opts.Mode {
				t.Fatalf("mode = %q, want %q", d.mode, opts.Mode)
			}
			if d.controllerRegistered {
				t.Fatal("controllerRegistered = true in T-04, want false")
			}
		})
	}
}

// TestNewChecks covers the readiness check registration per mode.
func TestNewChecks(t *testing.T) {
	tests := []struct {
		mode      Mode
		wantNames []string
	}{
		{mode: ModeController, wantNames: nil},
		{mode: ModeNode, wantNames: []string{fuseDevice, mountHelper}},
		{mode: ModeAll, wantNames: []string{fuseDevice, mountHelper}},
	}

	for _, tc := range tests {
		t.Run(string(tc.mode), func(t *testing.T) {
			opts := baseOptions()
			opts.Mode, opts.NodeID = tc.mode, "node-1"

			d, err := New(opts)
			if err != nil {
				t.Fatalf("New() = %v, want nil", err)
			}

			var names []string
			for _, c := range d.checks {
				names = append(names, c.name)
			}
			if len(names) != len(tc.wantNames) {
				t.Fatalf("checks = %v, want %v", names, tc.wantNames)
			}
			for i := range names {
				if names[i] != tc.wantNames[i] {
					t.Fatalf("check[%d] = %q, want %q", i, names[i], tc.wantNames[i])
				}
			}
		})
	}
}

// TestNewVersionFallback checks the vendor_version fallback through the public
// Identity API, which is what csi-sanity inspects.
func TestNewVersionFallback(t *testing.T) {
	tests := []struct {
		name        string
		version     string
		wantVersion string
	}{
		{name: "configured version", version: "v1.2.3", wantVersion: "v1.2.3"},
		{name: "empty version", version: "", wantVersion: "unknown"},
		{name: "blank version", version: "  ", wantVersion: "unknown"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := baseOptions()
			opts.Version = tc.version

			d, err := New(opts)
			if err != nil {
				t.Fatalf("New() = %v, want nil", err)
			}

			resp, err := d.GetPluginInfo(context.Background(), &csi.GetPluginInfoRequest{})
			if err != nil {
				t.Fatalf("GetPluginInfo() = %v, want nil", err)
			}
			if resp.GetName() != opts.DriverName {
				t.Fatalf("GetPluginInfo().Name = %q, want %q", resp.GetName(), opts.DriverName)
			}
			if resp.GetVendorVersion() != tc.wantVersion {
				t.Fatalf("GetPluginInfo().VendorVersion = %q, want %q", resp.GetVendorVersion(), tc.wantVersion)
			}
			if resp.GetManifest() != nil {
				t.Fatalf("GetPluginInfo().Manifest = %v, want nil", resp.GetManifest())
			}
		})
	}
}

// TestNewGRPCServerControllerModeMismatch checks the guard that keeps a future
// task from advertising a controller service that the mode does not run.
func TestNewGRPCServerControllerModeMismatch(t *testing.T) {
	opts := baseOptions()
	opts.Mode, opts.NodeID = ModeNode, "node-1"

	d, err := New(opts)
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}
	d.controllerRegistered = true

	if _, srvErr := d.newGRPCServer(); srvErr == nil {
		t.Fatal("newGRPCServer() = nil error, want an error for controller service in node mode")
	}

	d.mode = ModeController
	srv, err := d.newGRPCServer()
	if err != nil {
		t.Fatalf("newGRPCServer() = %v, want nil in controller mode", err)
	}
	if srv == nil {
		t.Fatal("newGRPCServer() = nil server, want a server")
	}
}
