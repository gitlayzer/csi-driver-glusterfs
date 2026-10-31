package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/gitlayzer/csi-driver-glusterfs/pkg/version"
)

func validOptions() options {
	return options{
		endpoint:   DefaultEndpoint,
		nodeID:     "node-1",
		mode:       modeAll,
		driverName: DriverName,
		configPath: DefaultConfigPath,
	}
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
			mutate: func(o *options) { o.mode = modeController },
		},
		{
			name:   "controller mode does not need nodeid",
			mutate: func(o *options) { o.mode, o.nodeID = modeController, "" },
		},
		{
			name:   "node mode with nodeid",
			mutate: func(o *options) { o.mode = modeNode },
		},
		{
			name:    "node mode without nodeid",
			mutate:  func(o *options) { o.mode, o.nodeID = modeNode, "" },
			wantErr: "--nodeid is required",
		},
		{
			name:    "node mode with blank nodeid",
			mutate:  func(o *options) { o.mode, o.nodeID = modeNode, "   " },
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
			name:   "tcp endpoint",
			mutate: func(o *options) { o.endpoint = "tcp://127.0.0.1:10000" },
		},
		{
			name:    "empty drivername",
			mutate:  func(o *options) { o.driverName = "" },
			wantErr: "--drivername must not be empty",
		},
		{
			name:    "empty config path",
			mutate:  func(o *options) { o.configPath = "" },
			wantErr: "--config must not be empty",
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
				endpoint:   DefaultEndpoint,
				mode:       modeAll,
				driverName: DriverName,
				configPath: DefaultConfigPath,
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
			},
			want: options{
				endpoint:   "tcp://127.0.0.1:10000",
				nodeID:     "node-1",
				mode:       modeNode,
				driverName: "example.csi.io",
				configPath: "/tmp/config.json",
			},
		},
		{
			name: "version flag",
			args: []string{"--version"},
			want: options{
				endpoint:   DefaultEndpoint,
				mode:       modeAll,
				driverName: DriverName,
				configPath: DefaultConfigPath,
				showVer:    true,
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

	if err := run([]string{"--version"}, &stdout, &stderr); err != nil {
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

func TestRunNotImplemented(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{
			name: "controller mode needs no nodeid",
			args: []string{"--mode=controller"},
		},
		{
			name: "node mode",
			args: []string{"--mode=node", "--nodeid=node-1"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			if err := run(tc.args, &stdout, &stderr); err != nil {
				t.Fatalf("run(%v) = %v, want nil", tc.args, err)
			}
			if got := strings.TrimSpace(stdout.String()); got != "not implemented yet" {
				t.Fatalf("run(%v) printed %q, want %q", tc.args, got, "not implemented yet")
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
			name:     "unknown flag",
			args:     []string{"--bogus"},
			parseErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			err := run(tc.args, &stdout, &stderr)
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
