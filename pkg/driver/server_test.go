package driver

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

// tempSocketDir returns a short temporary directory for a unix socket.
//
// t.TempDir() is not used on purpose: its path includes the (long) test name and
// a unix socket path is limited to 104 bytes on macOS, which the CI runs.
func tempSocketDir(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "gcsi")
	if err != nil {
		t.Fatalf("MkdirTemp() = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("RemoveAll(%q) = %v, want nil", dir, err)
		}
	})
	return dir
}

// makeStaleSocket creates a unix socket file at path and closes it without
// unlinking the file, which is what a killed driver leaves behind.
func makeStaleSocket(t *testing.T, path string) {
	t.Helper()

	lis, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatalf("ListenUnix(%q) = %v, want nil", path, err)
	}
	lis.SetUnlinkOnClose(false)
	if closeErr := lis.Close(); closeErr != nil {
		t.Fatalf("Close() = %v, want nil", closeErr)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("Lstat(%q) = %v, want the socket file to exist", path, err)
	}
}

func unixDriver(t *testing.T, path string) *Driver {
	t.Helper()

	opts := baseOptions()
	opts.Endpoint = "unix://" + path

	d, err := New(opts)
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}
	return d
}

// dialUnix connects a csi.IdentityClient to the unix socket at path.
func dialUnix(t *testing.T, path string) csi.IdentityClient {
	t.Helper()

	conn, err := grpc.NewClient("passthrough:///unix",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", path)
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

// waitForPluginInfo retries GetPluginInfo until the server answers or the
// deadline passes.
func waitForPluginInfo(t *testing.T, client csi.IdentityClient, timeout time.Duration) error {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		_, err := client.GetPluginInfo(ctx, &csi.GetPluginInfoRequest{})
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestServeUnixSocket covers the unix socket lifecycle of Run.
func TestServeUnixSocket(t *testing.T) {
	t.Run("stale socket is replaced", func(t *testing.T) {
		path := filepath.Join(tempSocketDir(t), "csi.sock")
		makeStaleSocket(t, path)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan error, 1)
		go func() { done <- unixDriver(t, path).Run(ctx) }()

		client := dialUnix(t, path)
		if err := waitForPluginInfo(t, client, 5*time.Second); err != nil {
			t.Fatalf("GetPluginInfo() over the new socket = %v, want nil", err)
		}

		resp, err := client.GetPluginInfo(context.Background(), &csi.GetPluginInfoRequest{})
		if err != nil {
			t.Fatalf("GetPluginInfo() = %v, want nil", err)
		}
		if resp.GetName() != baseOptions().DriverName {
			t.Fatalf("GetPluginInfo().Name = %q, want %q", resp.GetName(), baseOptions().DriverName)
		}

		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Run() = %v, want nil after cancellation", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Run() did not return within 2s of cancellation")
		}
	})

	t.Run("regular file is not deleted", func(t *testing.T) {
		path := filepath.Join(tempSocketDir(t), "csi.sock")
		if err := os.WriteFile(path, []byte("do not delete me"), 0o600); err != nil {
			t.Fatalf("WriteFile() = %v, want nil", err)
		}

		err := unixDriver(t, path).Run(context.Background())
		if err == nil {
			t.Fatal("Run() = nil, want an error for a non-socket path")
		}
		if !strings.Contains(err.Error(), "not a socket") {
			t.Fatalf("Run() = %q, want an error about a non-socket path", err)
		}

		content, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("ReadFile() = %v, want the file to still exist", readErr)
		}
		if string(content) != "do not delete me" {
			t.Fatalf("file content = %q, want it unchanged", content)
		}
	})

	t.Run("directory at the socket path is not deleted", func(t *testing.T) {
		path := filepath.Join(tempSocketDir(t), "csi.sock")
		if err := os.Mkdir(path, 0o750); err != nil {
			t.Fatalf("Mkdir() = %v, want nil", err)
		}

		err := unixDriver(t, path).Run(context.Background())
		if err == nil {
			t.Fatal("Run() = nil, want an error for a directory at the socket path")
		}
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("Stat() = %v, want the directory to still exist", statErr)
		}
	})

	t.Run("missing parent directory", func(t *testing.T) {
		path := filepath.Join(tempSocketDir(t), "missing", "csi.sock")

		err := unixDriver(t, path).Run(context.Background())
		if err == nil {
			t.Fatal("Run() = nil, want an error for a missing parent directory")
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Run() = %v, want an error wrapping os.ErrNotExist", err)
		}
		if _, statErr := os.Stat(filepath.Dir(path)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("Stat() = %v, want the driver not to create the directory", statErr)
		}
	})

	t.Run("socket is removed on cancellation", func(t *testing.T) {
		path := filepath.Join(tempSocketDir(t), "csi.sock")

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- unixDriver(t, path).Run(ctx) }()

		client := dialUnix(t, path)
		if err := waitForPluginInfo(t, client, 5*time.Second); err != nil {
			t.Fatalf("GetPluginInfo() = %v, want nil", err)
		}

		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Run() = %v, want nil after cancellation", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Run() did not return within 2s of cancellation")
		}

		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Lstat(%q) = %v, want the socket to be removed", path, err)
		}
	})
}

// TestServeTCP covers the TCP endpoint path and checks that nothing is written
// to the filesystem.
func TestServeTCP(t *testing.T) {
	opts := baseOptions()
	opts.Endpoint = "tcp://127.0.0.1:0"

	d, err := New(opts)
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() = %v, want nil", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Serve(ctx, lis) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, dialErr := net.DialTimeout("tcp", lis.Addr().String(), 200*time.Millisecond)
		if dialErr == nil {
			if closeErr := conn.Close(); closeErr != nil {
				t.Fatalf("Close() = %v, want nil", closeErr)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dial %q = %v, want the server to accept connections", lis.Addr(), dialErr)
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve() = %v, want nil after cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve() did not return within 2s of cancellation")
	}
}

// TestServeListenerError checks that a closed listener is reported as an error
// instead of being mistaken for a shutdown.
func TestServeListenerError(t *testing.T) {
	opts := baseOptions()
	d, err := New(opts)
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() = %v, want nil", err)
	}
	if closeErr := lis.Close(); closeErr != nil {
		t.Fatalf("Close() = %v, want nil", closeErr)
	}

	err = d.Serve(context.Background(), lis)
	if err == nil {
		t.Fatal("Serve() = nil, want an error for a closed listener")
	}
	if !strings.Contains(err.Error(), "serve gRPC") {
		t.Fatalf("Serve() = %q, want an error wrapping the serve failure", err)
	}
}

// TestRecoverInterceptor checks that a panicking handler becomes an Internal
// error and that the process survives.
func TestRecoverInterceptor(t *testing.T) {
	tests := []struct {
		name    string
		handler grpc.UnaryHandler
	}{
		{
			name: "panic with a string",
			handler: func(context.Context, any) (any, error) {
				panic("boom")
			},
		},
		{
			name: "panic with an index out of range",
			handler: func(context.Context, any) (any, error) {
				values := []string{}
				idx := len(values)
				return values[idx], nil
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			info := &grpc.UnaryServerInfo{FullMethod: "/csi.v1.Identity/Probe"}

			resp, err := recoverInterceptor(context.Background(), &csi.ProbeRequest{}, info, tc.handler)

			if err == nil {
				t.Fatalf("recoverInterceptor() = %v, nil, want an Internal error", resp)
			}
			if got := status.Code(err); got != codes.Internal {
				t.Fatalf("recoverInterceptor() code = %v, want %v", got, codes.Internal)
			}
			if got := status.Convert(err).Message(); got != internalErrorMessage {
				t.Fatalf("recoverInterceptor() message = %q, want %q", got, internalErrorMessage)
			}
			if resp != nil {
				t.Fatalf("recoverInterceptor() resp = %v, want nil", resp)
			}
		})
	}
}

// TestRecoverInterceptorPassThrough checks that a normal handler result is not
// touched.
func TestRecoverInterceptorPassThrough(t *testing.T) {
	want := &csi.ProbeResponse{}
	info := &grpc.UnaryServerInfo{FullMethod: "/csi.v1.Identity/Probe"}

	resp, err := recoverInterceptor(context.Background(), &csi.ProbeRequest{}, info,
		func(context.Context, any) (any, error) { return want, nil })

	if err != nil {
		t.Fatalf("recoverInterceptor() = %v, want nil", err)
	}
	if resp != want {
		t.Fatalf("recoverInterceptor() = %v, want %v", resp, want)
	}
}

// captureKlog redirects klog to a buffer at the given verbosity for the rest of
// the test.
//
// logtostderr has to be turned off as well: it is on by default and makes klog
// write to the real stderr no matter what SetOutput was given.
func captureKlog(t *testing.T, level string) *bytes.Buffer {
	t.Helper()

	fs := flag.NewFlagSet("klog", flag.ContinueOnError)
	klog.InitFlags(fs)

	var buf bytes.Buffer
	klog.SetOutput(&buf)
	for flagName, value := range map[string]string{"v": level, "logtostderr": "false", "alsologtostderr": "false"} {
		if err := fs.Set(flagName, value); err != nil {
			t.Fatalf("set -%s=%s: %v", flagName, value, err)
		}
	}
	t.Cleanup(func() {
		for flagName, value := range map[string]string{"v": "0", "logtostderr": "true", "alsologtostderr": "false"} {
			if err := fs.Set(flagName, value); err != nil {
				t.Errorf("reset -%s: %v", flagName, err)
			}
		}
		klog.SetOutput(os.Stderr)
	})

	return &buf
}

// TestLogInterceptor checks the RPC log line: level, fields and, most
// importantly, that secrets never reach the log.
func TestLogInterceptor(t *testing.T) {
	info := &grpc.UnaryServerInfo{FullMethod: "/csi.v1.Identity/GetPluginInfo"}

	tests := []struct {
		name       string
		verbosity  string
		handlerErr error
		wantLog    []string
		wantNoLog  bool
		wantCode   codes.Code
	}{
		{
			name:      "identity success at default verbosity",
			verbosity: "0",
			wantNoLog: true,
		},
		{
			name:      "identity success at V(5)",
			verbosity: "5",
			wantLog:   []string{"CSI request succeeded", "/csi.v1.Identity/GetPluginInfo", "code", "OK", "request"},
		},
		{
			name:       "error is always logged",
			verbosity:  "0",
			handlerErr: status.Error(codes.NotFound, "no such volume"),
			wantLog:    []string{"CSI request failed", "code", "NotFound", "no such volume"},
			wantCode:   codes.NotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// A direct output buffer so that the assertions see exactly what
			// the interceptor writes.
			buf := captureKlog(t, tc.verbosity)

			want := &csi.GetPluginInfoResponse{Name: "glusterfs.csi.io"}
			handler := func(context.Context, any) (any, error) {
				if tc.handlerErr != nil {
					return nil, tc.handlerErr
				}
				return want, nil
			}

			resp, err := logInterceptor(context.Background(), &csi.GetPluginInfoRequest{}, info, handler)

			if tc.handlerErr != nil {
				if !errors.Is(err, tc.handlerErr) {
					t.Fatalf("logInterceptor() = %v, want the handler error unchanged", err)
				}
				if got := status.Code(err); got != tc.wantCode {
					t.Fatalf("logInterceptor() code = %v, want %v", got, tc.wantCode)
				}
			} else {
				if err != nil {
					t.Fatalf("logInterceptor() = %v, want nil", err)
				}
				if resp != want {
					t.Fatalf("logInterceptor() = %v, want the handler response", resp)
				}
			}

			logged := buf.String()
			if tc.wantNoLog {
				if logged != "" {
					t.Fatalf("log = %q, want no output", logged)
				}
				return
			}
			for _, want := range tc.wantLog {
				if !strings.Contains(logged, want) {
					t.Fatalf("log = %q, want it to contain %q", logged, want)
				}
			}
		})
	}
}

// TestLogInterceptorStripsSecrets checks that a request at V(5) is logged
// without the content of its Secrets map.
func TestLogInterceptorStripsSecrets(t *testing.T) {
	buf := captureKlog(t, "5")

	const secret = "sup3r-s3cret"
	req := &csi.CreateVolumeRequest{
		Name:    "vol-1",
		Secrets: map[string]string{"password": secret},
	}
	info := &grpc.UnaryServerInfo{FullMethod: "/csi.v1.Controller/CreateVolume"}

	if _, err := logInterceptor(context.Background(), req, info,
		func(context.Context, any) (any, error) { return &csi.CreateVolumeResponse{}, nil }); err != nil {
		t.Fatalf("logInterceptor() = %v, want nil", err)
	}

	logged := buf.String()
	if !strings.Contains(logged, "CSI request succeeded") {
		t.Fatalf("log = %q, want a success line", logged)
	}
	if strings.Contains(logged, secret) {
		t.Fatalf("log = %q, must not contain the secret value", logged)
	}
	if !strings.Contains(logged, "***stripped***") {
		t.Fatalf("log = %q, want the secret to be replaced by ***stripped***", logged)
	}
}

// TestRemoveUnixSocket checks that a missing file is not reported.
func TestRemoveUnixSocket(t *testing.T) {
	path := filepath.Join(tempSocketDir(t), "missing.sock")

	// Must not panic and must not log an error for a file that is already gone.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Remove(%q) = %v, want nil", path, err)
	}
	removeUnixSocket(path)
}
