package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitlayzer/csi-driver-glusterfs/pkg/driver"
	"github.com/gitlayzer/csi-driver-glusterfs/pkg/version"
)

// validConfig is a configuration document that passes config.Parse.
const validConfig = `[{"clusterID":"prod","servers":["gluster-1","10.0.0.13"]}]`

func validOptions() options {
	return options{
		endpoint:         DefaultEndpoint,
		nodeID:           "node-1",
		mode:             string(driver.ModeAll),
		driverName:       DriverName,
		configPath:       DefaultConfigPath,
		mountLogDir:      DefaultMountLogDir,
		stateDir:         DefaultStateDir,
		recoveryInterval: DefaultRecoveryInterval,
	}
}

// writeConfig writes content to a file in a temporary directory and returns its
// path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestValidateFlags(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(o *options)
		wantErr string // substring of the expected error; empty means no error
	}{
		{
			name:   "defaults in all mode",
			mutate: func(_ *options) {},
		},
		{
			name:   "controller mode",
			mutate: func(o *options) { o.mode = string(driver.ModeController) },
		},
		{
			name:   "controller mode does not need nodeid",
			mutate: func(o *options) { o.mode, o.nodeID = string(driver.ModeController), "" },
		},
		{
			name:   "node mode with nodeid",
			mutate: func(o *options) { o.mode = string(driver.ModeNode) },
		},
		{
			name:    "node mode without nodeid",
			mutate:  func(o *options) { o.mode, o.nodeID = string(driver.ModeNode), "" },
			wantErr: "--nodeid is required",
		},
		{
			name:    "node mode with blank nodeid",
			mutate:  func(o *options) { o.mode, o.nodeID = string(driver.ModeNode), "   " },
			wantErr: "--nodeid is required",
		},
		{
			name:    "all mode without nodeid",
			mutate:  func(o *options) { o.nodeID = "" },
			wantErr: "--nodeid is required",
		},
		{
			name:    "invalid mode",
			mutate:  func(o *options) { o.mode = "server" },
			wantErr: "invalid --mode",
		},
		{
			name:    "empty mode",
			mutate:  func(o *options) { o.mode = "" },
			wantErr: "invalid --mode",
		},
		{
			name:    "empty endpoint",
			mutate:  func(o *options) { o.endpoint = "" },
			wantErr: "--endpoint must not be empty",
		},
		{
			name:    "endpoint without scheme",
			mutate:  func(o *options) { o.endpoint = "/csi/csi.sock" },
			wantErr: "invalid --endpoint",
		},
		{
			name:    "endpoint with unsupported scheme",
			mutate:  func(o *options) { o.endpoint = "http://127.0.0.1:10000" },
			wantErr: "invalid --endpoint",
		},
		{
			name:    "unix endpoint without path",
			mutate:  func(o *options) { o.endpoint = "unix://" },
			wantErr: "address must not be empty",
		},
		{
			name:    "tcp endpoint without address",
			mutate:  func(o *options) { o.endpoint = "tcp://" },
			wantErr: "address must not be empty",
		},
		{
			name:    "tcp endpoint without port",
			mutate:  func(o *options) { o.endpoint = "tcp://127.0.0.1" },
			wantErr: "invalid --endpoint",
		},
		{
			name:   "tcp endpoint",
			mutate: func(o *options) { o.endpoint = "tcp://127.0.0.1:10000" },
		},
		{
			name:    "empty drivername",
			mutate:  func(o *options) { o.driverName = "" },
			wantErr: "--drivername must not be empty",
		},
		{
			name:    "drivername with leading dash",
			mutate:  func(o *options) { o.driverName = "-abc" },
			wantErr: "invalid --drivername",
		},
		{
			name:    "drivername with trailing dash",
			mutate:  func(o *options) { o.driverName = "abc-" },
			wantErr: "invalid --drivername",
		},
		{
			name:    "drivername with underscore",
			mutate:  func(o *options) { o.driverName = "a_b" },
			wantErr: "invalid --drivername",
		},
		{
			name:    "drivername with space",
			mutate:  func(o *options) { o.driverName = "a b" },
			wantErr: "invalid --drivername",
		},
		{
			name:    "drivername longer than 63 characters",
			mutate:  func(o *options) { o.driverName = strings.Repeat("a", 64) },
			wantErr: "invalid --drivername",
		},
		{
			name:   "drivername with 63 characters",
			mutate: func(o *options) { o.driverName = strings.Repeat("a", 63) },
		},
		{
			name:    "empty config path",
			mutate:  func(o *options) { o.configPath = "" },
			wantErr: "--config must not be empty",
		},
		{
			name:    "relative mount log dir",
			mutate:  func(o *options) { o.mountLogDir = "relative" },
			wantErr: "invalid --mount-log-dir",
		},
		{
			name:    "unclean mount log dir",
			mutate:  func(o *options) { o.mountLogDir = "/a/../b" },
			wantErr: "invalid --mount-log-dir",
		},
		{
			name:   "empty mount log dir disables the log file",
			mutate: func(o *options) { o.mountLogDir = "" },
		},
		{
			name:    "relative state dir",
			mutate:  func(o *options) { o.stateDir = "staged" },
			wantErr: "invalid --state-dir",
		},
		{
			name:    "unclean state dir",
			mutate:  func(o *options) { o.stateDir = "/a/../b" },
			wantErr: "invalid --state-dir",
		},
		{
			name:    "state dir with a trailing slash",
			mutate:  func(o *options) { o.stateDir = "/staged/" },
			wantErr: "invalid --state-dir",
		},
		{
			name:   "empty state dir disables the records and the recovery",
			mutate: func(o *options) { o.stateDir = "" },
		},
		{
			name:    "negative recovery interval",
			mutate:  func(o *options) { o.recoveryInterval = -time.Second },
			wantErr: "invalid --recovery-interval",
		},
		{
			name:   "zero recovery interval scans at startup only",
			mutate: func(o *options) { o.recoveryInterval = 0 },
		},
		{
			name:   "a long recovery interval",
			mutate: func(o *options) { o.recoveryInterval = 24 * time.Hour },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := validOptions()
			tc.mutate(&opts)

			err := validateFlags(opts)

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateFlags() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateFlags() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateFlags() = %q, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestParseFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    options
		wantErr bool
	}{
		{
			name: "defaults",
			args: nil,
			want: options{
				endpoint:         DefaultEndpoint,
				mode:             string(driver.ModeAll),
				driverName:       DriverName,
				configPath:       DefaultConfigPath,
				mountLogDir:      DefaultMountLogDir,
				stateDir:         DefaultStateDir,
				recoveryInterval: DefaultRecoveryInterval,
			},
		},
		{
			name: "all flags",
			args: []string{
				"--endpoint=tcp://127.0.0.1:10000",
				"--nodeid=node-1",
				"--mode=node",
				"--drivername=example.csi.io",
				"--config=/tmp/config.json",
				"--mount-log-dir=/tmp/logs",
				"--state-dir=/tmp/staged",
				"--recovery-interval=90s",
			},
			want: options{
				endpoint:         "tcp://127.0.0.1:10000",
				nodeID:           "node-1",
				mode:             string(driver.ModeNode),
				driverName:       "example.csi.io",
				configPath:       "/tmp/config.json",
				mountLogDir:      "/tmp/logs",
				stateDir:         "/tmp/staged",
				recoveryInterval: 90 * time.Second,
			},
		},
		{
			name: "an empty state dir and a zero interval",
			args: []string{"--state-dir=", "--recovery-interval=0"},
			want: options{
				endpoint:    DefaultEndpoint,
				mode:        string(driver.ModeAll),
				driverName:  DriverName,
				configPath:  DefaultConfigPath,
				mountLogDir: DefaultMountLogDir,
			},
		},
		{
			name:    "a recovery interval without a unit",
			args:    []string{"--recovery-interval=60"},
			wantErr: true,
		},
		{
			name: "version flag",
			args: []string{"--version"},
			want: options{
				endpoint:         DefaultEndpoint,
				mode:             string(driver.ModeAll),
				driverName:       DriverName,
				configPath:       DefaultConfigPath,
				mountLogDir:      DefaultMountLogDir,
				stateDir:         DefaultStateDir,
				recoveryInterval: DefaultRecoveryInterval,
				showVer:          true,
			},
		},
		{
			name:    "unknown flag",
			args:    []string{"--nope"},
			wantErr: true,
		},
		{
			name:    "positional argument",
			args:    []string{"extra"},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			got, err := parseFlags(tc.args, &buf)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseFlags(%v) = nil error, want error", tc.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFlags(%v) = %v, want nil", tc.args, err)
			}
			if got != tc.want {
				t.Fatalf("parseFlags(%v) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer

	if err := run(context.Background(), []string{"--version"}, &stdout, &stderr); err != nil {
		t.Fatalf("run(--version) = %v, want nil", err)
	}

	want := version.Get().String()
	if got := strings.TrimSpace(stdout.String()); got != want {
		t.Fatalf("run(--version) printed %q, want %q", got, want)
	}
	if stdout.String() != want+"\n" {
		t.Fatalf("run(--version) output = %q, want a single line %q", stdout.String(), want)
	}
}

// TestRunServesUntilCancelled starts the real server on a kernel assigned TCP
// port (port 0) and checks that cancelling the context makes run return
// cleanly within 2s.
func TestRunServesUntilCancelled(t *testing.T) {
	tests := []struct {
		name string
		mode driver.Mode
	}{
		{name: "controller mode", mode: driver.ModeController},
		{name: "node mode", mode: driver.ModeNode},
		{name: "all mode", mode: driver.ModeAll},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var stdout, stderr bytes.Buffer
			done := make(chan error, 1)
			go func() {
				done <- run(ctx, []string{
					"--mode=" + string(tc.mode),
					"--nodeid=node-1",
					"--endpoint=tcp://127.0.0.1:0",
					"--config=" + writeConfig(t, validConfig),
				}, &stdout, &stderr)
			}()

			// Give the server time to bind before cancelling, so the test also
			// covers the normal shutdown path rather than a failed start.
			time.Sleep(100 * time.Millisecond)
			cancel()

			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("run() = %v, want nil after cancellation (stderr: %q)", err, stderr.String())
				}
			case <-time.After(2 * time.Second):
				t.Fatal("run() did not return within 2s of cancellation")
			}
		})
	}
}

// TestRunEmptyMountLogDir checks that an empty --mount-log-dir is accepted: it
// disables the log-file mount option instead of being rejected.
func TestRunEmptyMountLogDir(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stdout, stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{
			"--mode=node",
			"--nodeid=node-1",
			"--endpoint=tcp://127.0.0.1:0",
			"--config=" + writeConfig(t, validConfig),
			"--mount-log-dir=",
		}, &stdout, &stderr)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run() = %v, want nil after cancellation (stderr: %q)", err, stderr.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run() did not return within 2s of cancellation")
	}
}

// TestRunConfigErrors checks that a broken configuration fails the start
// instead of leaving a running plugin without any cluster.
func TestRunConfigErrors(t *testing.T) {
	tests := []struct {
		name    string
		conf    string
		missing bool
		wantErr string
	}{
		{
			name:    "missing file",
			missing: true,
			wantErr: "load config",
		},
		{
			name:    "invalid document",
			conf:    `{"clusters": []}`,
			wantErr: "load config",
		},
		{
			name:    "empty document",
			conf:    "",
			wantErr: "load config",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if !tc.missing {
				path = writeConfig(t, tc.conf)
			}

			var stdout, stderr bytes.Buffer
			err := run(context.Background(), []string{
				"--mode=controller",
				"--endpoint=tcp://127.0.0.1:0",
				"--config=" + path,
			}, &stdout, &stderr)

			if err == nil {
				t.Fatalf("run() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("run() = %q, want error containing %q", err, tc.wantErr)
			}
			if stdout.Len() != 0 {
				t.Fatalf("run() wrote to stdout: %q", stdout.String())
			}
		})
	}
}

func TestRunInvalidFlags(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantErr  string
		parseErr bool
	}{
		{
			name:    "invalid mode",
			args:    []string{"--mode=bogus", "--nodeid=node-1"},
			wantErr: "invalid --mode",
		},
		{
			name:    "missing nodeid",
			args:    []string{"--mode=node"},
			wantErr: "--nodeid is required",
		},
		{
			name:    "empty endpoint",
			args:    []string{"--mode=controller", "--endpoint="},
			wantErr: "--endpoint must not be empty",
		},
		{
			name:    "bad endpoint scheme",
			args:    []string{"--mode=controller", "--endpoint=http://example"},
			wantErr: "invalid --endpoint",
		},
		{
			name:    "invalid drivername",
			args:    []string{"--mode=controller", "--drivername=_abc"},
			wantErr: "invalid --drivername",
		},
		{
			name:    "relative mount log dir",
			args:    []string{"--mode=controller", "--mount-log-dir=relative"},
			wantErr: "invalid --mount-log-dir",
		},
		{
			name:    "unclean mount log dir",
			args:    []string{"--mode=controller", "--mount-log-dir=/a/../b"},
			wantErr: "invalid --mount-log-dir",
		},
		{
			name:    "relative state dir",
			args:    []string{"--mode=controller", "--state-dir=staged"},
			wantErr: "invalid --state-dir",
		},
		{
			name:    "negative recovery interval",
			args:    []string{"--mode=controller", "--recovery-interval=-1s"},
			wantErr: "invalid --recovery-interval",
		},
		{
			name:     "unknown flag",
			args:     []string{"--bogus"},
			parseErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			err := run(context.Background(), tc.args, &stdout, &stderr)
			if err == nil {
				t.Fatalf("run(%v) = nil, want error", tc.args)
			}
			if stdout.Len() != 0 {
				t.Fatalf("run(%v) wrote to stdout: %q", tc.args, stdout.String())
			}
			if !tc.parseErr && !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("run(%v) = %q, want error containing %q", tc.args, err, tc.wantErr)
			}
			if tc.parseErr {
				var usageErr *usageError
				if !errors.As(err, &usageErr) {
					t.Fatalf("run(%v) = %T, want *usageError", tc.args, err)
				}
			}
		})
	}
}

// TestDefaultRecoveryFlags checks the documented defaults of the two recovery
// flags (D-20): the staging record directory below the kubelet plugin directory
// and a 60s interval.
func TestDefaultRecoveryFlags(t *testing.T) {
	if DefaultStateDir != "/var/lib/kubelet/plugins/glusterfs.csi.io/staged" {
		t.Fatalf("DefaultStateDir = %q, want the kubelet plugin directory", DefaultStateDir)
	}
	if DefaultRecoveryInterval != 60*time.Second {
		t.Fatalf("DefaultRecoveryInterval = %s, want 60s", DefaultRecoveryInterval)
	}

	opts, err := parseFlags(nil, io.Discard)
	if err != nil {
		t.Fatalf("parseFlags() = %v, want nil", err)
	}
	if opts.stateDir != DefaultStateDir {
		t.Fatalf("stateDir = %q, want %q", opts.stateDir, DefaultStateDir)
	}
	if opts.recoveryInterval != DefaultRecoveryInterval {
		t.Fatalf("recoveryInterval = %s, want %s", opts.recoveryInterval, DefaultRecoveryInterval)
	}
}

// TestRunRecoveryFlags checks that the two flags reach the driver: a state
// directory the recovery can use and a zero interval, both accepted at startup.
func TestRunRecoveryFlags(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stdout, stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{
			"--mode=node",
			"--nodeid=node-1",
			"--endpoint=tcp://127.0.0.1:0",
			"--config=" + writeConfig(t, validConfig),
			"--state-dir=" + filepath.Join(t.TempDir(), "staged"),
			"--recovery-interval=0",
		}, &stdout, &stderr)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run() = %v, want nil after cancellation (stderr: %q)", err, stderr.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run() did not return within 2s of cancellation")
	}
}
