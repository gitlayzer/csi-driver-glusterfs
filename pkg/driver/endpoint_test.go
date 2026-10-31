package driver

import (
	"strings"
	"testing"
)

func TestParseEndpoint(t *testing.T) {
	tests := []struct {
		name        string
		endpoint    string
		wantNetwork string
		wantAddress string
		wantErr     bool
	}{
		{name: "absolute unix path", endpoint: "unix:///csi/csi.sock", wantNetwork: "unix", wantAddress: "/csi/csi.sock"},
		{name: "relative unix path", endpoint: "unix://relative.sock", wantNetwork: "unix", wantAddress: "relative.sock"},
		{name: "tcp ipv4", endpoint: "tcp://127.0.0.1:10000", wantNetwork: "tcp", wantAddress: "127.0.0.1:10000"},
		{name: "tcp ipv6 port 0", endpoint: "tcp://[::1]:0", wantNetwork: "tcp", wantAddress: "[::1]:0"},
		{name: "tcp without host", endpoint: "tcp://:10000", wantNetwork: "tcp", wantAddress: ":10000"},
		{name: "tcp max port", endpoint: "tcp://h:65535", wantNetwork: "tcp", wantAddress: "h:65535"},

		{name: "empty", endpoint: "", wantErr: true},
		{name: "blank", endpoint: "   ", wantErr: true},
		{name: "unix without path", endpoint: "unix://", wantErr: true},
		{name: "unix with blank path", endpoint: "unix://   ", wantErr: true},
		{name: "tcp without address", endpoint: "tcp://", wantErr: true},
		{name: "tcp without port", endpoint: "tcp://127.0.0.1", wantErr: true},
		{name: "tcp port out of range", endpoint: "tcp://h:99999", wantErr: true},
		{name: "tcp port not numeric", endpoint: "tcp://h:abc", wantErr: true},
		{name: "tcp port negative", endpoint: "tcp://h:-1", wantErr: true},
		{name: "tcp port signed", endpoint: "tcp://h:+1", wantErr: true},
		{name: "unsupported scheme", endpoint: "http://x", wantErr: true},
		{name: "uppercase scheme", endpoint: "UNIX:///a", wantErr: true},
		{name: "no scheme", endpoint: "/csi/csi.sock", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			network, address, err := ParseEndpoint(tc.endpoint)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseEndpoint(%q) = %q, %q, nil, want error", tc.endpoint, network, address)
				}
				if network != "" || address != "" {
					t.Fatalf("ParseEndpoint(%q) returned %q, %q with an error, want empty results", tc.endpoint, network, address)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseEndpoint(%q) = %v, want nil", tc.endpoint, err)
			}
			if network != tc.wantNetwork || address != tc.wantAddress {
				t.Fatalf("ParseEndpoint(%q) = %q, %q, want %q, %q",
					tc.endpoint, network, address, tc.wantNetwork, tc.wantAddress)
			}
		})
	}
}

func TestParseEndpointErrorMentionsEndpoint(t *testing.T) {
	_, _, err := ParseEndpoint("tcp://h:99999")
	if err == nil {
		t.Fatal("ParseEndpoint() = nil, want error")
	}
	if !strings.Contains(err.Error(), "tcp://h:99999") {
		t.Fatalf("ParseEndpoint() = %q, want it to mention the endpoint", err)
	}
}

func TestParseMode(t *testing.T) {
	tests := []struct {
		in      string
		want    Mode
		wantErr bool
	}{
		{in: "controller", want: ModeController},
		{in: "node", want: ModeNode},
		{in: "all", want: ModeAll},
		{in: "", wantErr: true},
		{in: "Controller", wantErr: true},
		{in: "nodes", wantErr: true},
		{in: " server ", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseMode(tc.in)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseMode(%q) = %q, nil, want error", tc.in, got)
				}
				if got != "" {
					t.Fatalf("ParseMode(%q) = %q with an error, want empty", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseMode(%q) = %v, want nil", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParseMode(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestModeRunsServices(t *testing.T) {
	tests := []struct {
		mode     Mode
		wantCtrl bool
		wantNode bool
	}{
		{mode: ModeController, wantCtrl: true},
		{mode: ModeNode, wantNode: true},
		{mode: ModeAll, wantCtrl: true, wantNode: true},
	}

	for _, tc := range tests {
		t.Run(string(tc.mode), func(t *testing.T) {
			if got := tc.mode.runsController(); got != tc.wantCtrl {
				t.Fatalf("runsController() = %t, want %t", got, tc.wantCtrl)
			}
			if got := tc.mode.runsNode(); got != tc.wantNode {
				t.Fatalf("runsNode() = %t, want %t", got, tc.wantNode)
			}
		})
	}
}

func TestValidateDriverName(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "default driver name", in: "glusterfs.csi.io"},
		{name: "single character", in: "a"},
		{name: "digits", in: "1"},
		{name: "dash and dot", in: "a-b.c1"},
		{name: "63 characters", in: strings.Repeat("a", 63)},
		{name: "mixed case", in: "GlusterFS.CSI.io"},

		{name: "empty", in: "", wantErr: true},
		{name: "leading dash", in: "-abc", wantErr: true},
		{name: "trailing dash", in: "abc-", wantErr: true},
		{name: "leading dot", in: ".abc", wantErr: true},
		{name: "trailing dot", in: "abc.", wantErr: true},
		{name: "underscore", in: "a_b", wantErr: true},
		{name: "space", in: "a b", wantErr: true},
		{name: "slash", in: "a/b", wantErr: true},
		{name: "slash traversal", in: "a/../b", wantErr: true},
		{name: "non ascii", in: "glüsterfs", wantErr: true},
		{name: "64 characters", in: strings.Repeat("a", 64), wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateDriverName(tc.in)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("ValidateDriverName(%q) = nil, want error", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateDriverName(%q) = %v, want nil", tc.in, err)
			}
		})
	}
}
