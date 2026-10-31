package mounter

import (
	"errors"
	"strings"
	"testing"
)

// validSpec returns a spec that passes validation, so that each test only has
// to change the field it is about.
func validSpec() Spec {
	return Spec{Servers: []string{"g1"}, Volume: "vol"}
}

func TestBuildMountArgs(t *testing.T) {
	const logDir = "/var/log/glusterfs/csi"

	tests := []struct {
		name        string
		spec        Spec
		logDir      string
		wantSource  string
		wantOptions []string
	}{
		{
			name:        "single server without subdir and without log dir",
			spec:        Spec{Servers: []string{"10.0.0.1"}, Volume: "vol"},
			wantSource:  "10.0.0.1:/vol",
			wantOptions: []string{"log-level=WARNING"},
		},
		{
			name:       "three servers with subdir and log dir",
			spec:       Spec{Servers: []string{"g1", "g2", "g3"}, Volume: "vol", Subdir: "pvc-1"},
			logDir:     logDir,
			wantSource: "g1:/vol/pvc-1",
			wantOptions: []string{
				"log-level=WARNING",
				"backup-volfile-servers=g2:g3",
				"log-file=/var/log/glusterfs/csi/vol_pvc-1.log",
			},
		},
		{
			name:        "log dir without subdir",
			spec:        validSpec(),
			logDir:      logDir,
			wantSource:  "g1:/vol",
			wantOptions: []string{"log-level=WARNING", "log-file=/var/log/glusterfs/csi/vol.log"},
		},
		{
			name:        "read only appends ro before the default log level",
			spec:        Spec{Servers: []string{"g1"}, Volume: "vol", ReadOnly: true},
			wantSource:  "g1:/vol",
			wantOptions: []string{"ro", "log-level=WARNING"},
		},
		{
			name:        "read only with a user supplied ro keeps a single ro",
			spec:        Spec{Servers: []string{"g1"}, Volume: "vol", ReadOnly: true, Options: []string{"acl,ro"}},
			wantSource:  "g1:/vol",
			wantOptions: []string{"acl", "ro", "log-level=WARNING"},
		},
		{
			name:        "user log level replaces the default",
			spec:        Spec{Servers: []string{"g1"}, Volume: "vol", Options: []string{"log-level=DEBUG"}},
			wantSource:  "g1:/vol",
			wantOptions: []string{"log-level=DEBUG"},
		},
		{
			name:        "user options are split and keep their order",
			spec:        Spec{Servers: []string{"g1"}, Volume: "vol", Options: []string{"acl,use-readdirp=no", "attribute-timeout=0.5"}},
			wantSource:  "g1:/vol",
			wantOptions: []string{"acl", "use-readdirp=no", "attribute-timeout=0.5", "log-level=WARNING"},
		},
		{
			name:       "every allowlisted option is accepted",
			spec:       Spec{Servers: []string{"g1"}, Volume: "vol", Options: []string{"ro,acl,selinux,auto_unmount,fopen-keep-cache,enable-ino32"}},
			wantSource: "g1:/vol",
			wantOptions: []string{
				"ro", "acl", "selinux", "auto_unmount", "fopen-keep-cache", "enable-ino32", "log-level=WARNING",
			},
		},
		{
			name:        "numeric and enumerated values at their bounds",
			spec:        Spec{Servers: []string{"g1"}, Volume: "vol", Options: []string{"lru-limit=999999999", "entry-timeout=123456.123", "direct-io-mode=disable"}},
			wantSource:  "g1:/vol",
			wantOptions: []string{"lru-limit=999999999", "entry-timeout=123456.123", "direct-io-mode=disable", "log-level=WARNING"},
		},
		{
			name:        "backup servers keep the configured order",
			spec:        Spec{Servers: []string{"g1", "g2"}, Volume: "vol"},
			wantSource:  "g1:/vol",
			wantOptions: []string{"log-level=WARNING", "backup-volfile-servers=g2"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			source, options, err := BuildMountArgs(tc.spec, tc.logDir)
			if err != nil {
				t.Fatalf("BuildMountArgs(%+v, %q) returned error %v, want nil", tc.spec, tc.logDir, err)
			}
			if source != tc.wantSource {
				t.Errorf("BuildMountArgs(%+v, %q) source = %q, want %q", tc.spec, tc.logDir, source, tc.wantSource)
			}
			if len(options) != len(tc.wantOptions) {
				t.Fatalf("BuildMountArgs(%+v, %q) options = %q, want %q", tc.spec, tc.logDir, options, tc.wantOptions)
			}
			for i := range options {
				if options[i] != tc.wantOptions[i] {
					t.Fatalf("BuildMountArgs(%+v, %q) options = %q, want %q", tc.spec, tc.logDir, options, tc.wantOptions)
				}
			}
		})
	}
}

func TestBuildMountArgsInvalidSpec(t *testing.T) {
	tests := []struct {
		name   string
		spec   Spec
		logDir string
		wantIn string
	}{
		{
			name:   "no servers",
			spec:   Spec{Volume: "vol"},
			wantIn: "servers",
		},
		{
			name:   "nil servers",
			spec:   Spec{Servers: nil, Volume: "vol"},
			wantIn: "servers",
		},
		{
			name:   "too many servers",
			spec:   Spec{Servers: manyServers(33), Volume: "vol"},
			wantIn: "servers",
		},
		{
			name:   "exactly the maximum number of servers",
			spec:   Spec{Servers: manyServers(32), Volume: "vol"},
			wantIn: "", // valid
		},
		{
			name:   "ipv6 server",
			spec:   Spec{Servers: []string{"::1"}, Volume: "vol"},
			wantIn: "servers[0]",
		},
		{
			name:   "server with a comma",
			spec:   Spec{Servers: []string{"g1,g2"}, Volume: "vol"},
			wantIn: "servers[0]",
		},
		{
			name:   "server with a slash",
			spec:   Spec{Servers: []string{"g1/g2"}, Volume: "vol"},
			wantIn: "servers[0]",
		},
		{
			name:   "server with whitespace",
			spec:   Spec{Servers: []string{"g 1"}, Volume: "vol"},
			wantIn: "servers[0]",
		},
		{
			name:   "server with a tab",
			spec:   Spec{Servers: []string{"g\t1"}, Volume: "vol"},
			wantIn: "servers[0]",
		},
		{
			name:   "server leading hyphen",
			spec:   Spec{Servers: []string{"-o"}, Volume: "vol"},
			wantIn: "servers[0]",
		},
		{
			name:   "server with equals",
			spec:   Spec{Servers: []string{"g1=x"}, Volume: "vol"},
			wantIn: "servers[0]",
		},
		{
			name:   "empty server",
			spec:   Spec{Servers: []string{""}, Volume: "vol"},
			wantIn: "servers[0]",
		},
		{
			name:   "second server invalid",
			spec:   Spec{Servers: []string{"g1", "g:2"}, Volume: "vol"},
			wantIn: "servers[1]",
		},
		{
			name:   "server longer than 253 bytes",
			spec:   Spec{Servers: []string{strings.Repeat("a", 254)}, Volume: "vol"},
			wantIn: "servers[0]",
		},
		{
			name:   "case insensitive duplicate server",
			spec:   Spec{Servers: []string{"G1", "g1"}, Volume: "vol"},
			wantIn: "duplicate",
		},
		{
			name:   "exact duplicate server",
			spec:   Spec{Servers: []string{"g1", "g1"}, Volume: "vol"},
			wantIn: "duplicate",
		},
		{
			name:   "volume is dot dot",
			spec:   Spec{Servers: []string{"g1"}, Volume: ".."},
			wantIn: "volume",
		},
		{
			name:   "volume is empty",
			spec:   Spec{Servers: []string{"g1"}, Volume: ""},
			wantIn: "volume",
		},
		{
			name:   "volume with a slash",
			spec:   Spec{Servers: []string{"g1"}, Volume: "a/b"},
			wantIn: "volume",
		},
		{
			name:   "subdir with a slash",
			spec:   Spec{Servers: []string{"g1"}, Volume: "vol", Subdir: "a/b"},
			wantIn: "subdir",
		},
		{
			name:   "subdir is dot dot",
			spec:   Spec{Servers: []string{"g1"}, Volume: "vol", Subdir: ".."},
			wantIn: "subdir",
		},
		{
			name:   "relative log dir",
			spec:   validSpec(),
			logDir: "var/log",
			wantIn: "logDir",
		},
		{
			name:   "unclean log dir",
			spec:   validSpec(),
			logDir: "/a/../b",
			wantIn: "logDir",
		},
		{
			name:   "log dir with a trailing slash",
			spec:   validSpec(),
			logDir: "/var/log/",
			wantIn: "logDir",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			source, options, err := BuildMountArgs(tc.spec, tc.logDir)

			if tc.wantIn == "" {
				if err != nil {
					t.Fatalf("BuildMountArgs(%+v, %q) returned error %v, want nil", tc.spec, tc.logDir, err)
				}
				if source == "" || options == nil {
					t.Fatalf("BuildMountArgs(%+v, %q) = %q, %q, want a source and options", tc.spec, tc.logDir, source, options)
				}
				return
			}

			if !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("BuildMountArgs(%+v, %q) error = %v, want ErrInvalidSpec", tc.spec, tc.logDir, err)
			}
			if source != "" || options != nil {
				t.Errorf("BuildMountArgs(%+v, %q) = %q, %q with an error, want empty results", tc.spec, tc.logDir, source, options)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("BuildMountArgs(%+v, %q) error = %v, want it to mention %q", tc.spec, tc.logDir, err, tc.wantIn)
			}
		})
	}
}

func TestBuildMountArgsInvalidOption(t *testing.T) {
	tests := []struct {
		name   string
		option string
		// wantMsg, when set, must appear in the error message: the forbidden
		// options are rejected with the reason, not with the generic allowlist
		// message.
		wantMsg string
	}{
		{name: "log-file is controlled by the driver", option: "log-file=/x", wantMsg: "controlled by the driver"},
		{name: "backup-volfile-servers is controlled by the driver", option: "backup-volfile-servers=a", wantMsg: "controlled by the driver"},
		{name: "backupvolfile-server is controlled by the driver", option: "backupvolfile-server=a", wantMsg: "controlled by the driver"},
		{name: "volfile-server is controlled by the driver", option: "volfile-server=a", wantMsg: "controlled by the driver"},
		{name: "volfile-id is controlled by the driver", option: "volfile-id=a", wantMsg: "controlled by the driver"},
		{name: "xlator-option is not allowed", option: "xlator-option=a.b=c", wantMsg: "not allowed"},
		{name: "fuse-mountopts is not allowed", option: "fuse-mountopts=nosuid", wantMsg: "not allowed"},
		{name: "process-name is not allowed", option: "process-name=x", wantMsg: "not allowed"},
		{name: "transport is not allowed", option: "transport=tcp", wantMsg: "not allowed"},
		{name: "rw is not allowed", option: "rw", wantMsg: "not allowed"},
		{name: "unknown option", option: "foo"},
		{name: "unknown option that looks like an allowlisted one", option: "acl2"},
		{name: "case differs from the allowlist", option: "ACL"},
		{name: "flag with a value", option: "acl=1"},
		{name: "flag with an empty value", option: "acl="},
		{name: "unknown log level", option: "log-level=LOUD"},
		{name: "lowercase log level", option: "log-level=debug"},
		{name: "empty log level", option: "log-level="},
		{name: "log level without value", option: "log-level"},
		{name: "yes/no value out of range", option: "use-readdirp=maybe"},
		{name: "direct-io-mode value out of range", option: "direct-io-mode=on"},
		{name: "timeout not a number", option: "attribute-timeout=abc"},
		{name: "timeout with too many fractional digits", option: "attribute-timeout=1.2345"},
		{name: "timeout with too many integral digits", option: "entry-timeout=1234567"},
		{name: "negative counter", option: "lru-limit=-1"},
		{name: "counter with a sign", option: "lru-limit=+1"},
		{name: "counter too large", option: "background-qlen=1234567890"},
		{name: "empty fragment in the middle", option: "acl,,selinux", wantMsg: "empty option"},
		{name: "empty fragment only", option: ""},
		{name: "leading comma", option: ",acl", wantMsg: "empty option"},
		{name: "trailing comma", option: "acl,", wantMsg: "empty option"},
		{name: "duplicate flag", option: "acl,acl"},
		{name: "duplicate key with different values", option: "log-level=INFO,log-level=DEBUG"},
		{name: "duplicate key with the same value", option: "log-level=INFO,log-level=INFO"},
		{name: "empty key", option: "=1"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := Spec{Servers: []string{"g1"}, Volume: "vol", Options: []string{tc.option}}
			source, options, err := BuildMountArgs(spec, "")

			if !errors.Is(err, ErrInvalidOption) {
				t.Fatalf("BuildMountArgs(Options: %q) error = %v, want ErrInvalidOption", tc.option, err)
			}
			if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("BuildMountArgs(Options: %q) error = %q, want it to contain %q", tc.option, err, tc.wantMsg)
			}
			if source != "" || options != nil {
				t.Errorf("BuildMountArgs(Options: %q) = %q, %q with an error, want empty results", tc.option, source, options)
			}
		})
	}
}

func TestBuildMountArgsOversizedOption(t *testing.T) {
	fragment := strings.Repeat("a", 300)
	spec := Spec{Servers: []string{"g1"}, Volume: "vol", Options: []string{fragment}}
	_, _, err := BuildMountArgs(spec, "")

	if !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("BuildMountArgs with a 300 byte option returned error %v, want ErrInvalidOption", err)
	}
	if strings.Contains(err.Error(), fragment) {
		t.Errorf("error message %q echoes the oversized option, want its length only", err)
	}
	if !strings.Contains(err.Error(), "300") {
		t.Errorf("error message %q does not report the length of the option", err)
	}
}

// manyServers returns n distinct server names.
func manyServers(n int) []string {
	servers := make([]string, 0, n)
	for i := 0; i < n; i++ {
		servers = append(servers, "g"+strings.Repeat("a", i))
	}
	return servers
}

func TestLogFileName(t *testing.T) {
	tests := []struct {
		name string
		spec Spec
		want string
	}{
		{name: "volume only", spec: Spec{Volume: "vol"}, want: "vol.log"},
		{name: "volume with subdir", spec: Spec{Volume: "vol", Subdir: "pvc-1"}, want: "vol_pvc-1.log"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := logFileName(tc.spec); got != tc.want {
				t.Fatalf("logFileName(%+v) = %q, want %q", tc.spec, got, tc.want)
			}
		})
	}
}

func TestValidateServer(t *testing.T) {
	tests := []struct {
		name    string
		server  string
		wantErr bool
	}{
		{name: "hostname", server: "gluster-1.example.com"},
		{name: "ipv4", server: "10.0.0.1"},
		{name: "underscore and dot", server: "a_b.c"},
		{name: "253 bytes", server: strings.Repeat("a", 253)},

		{name: "empty", server: "", wantErr: true},
		{name: "254 bytes", server: strings.Repeat("a", 254), wantErr: true},
		{name: "ipv6", server: "::1", wantErr: true},
		{name: "colon", server: "g1:24007", wantErr: true},
		{name: "comma", server: "g1,g2", wantErr: true},
		{name: "slash", server: "g1/g2", wantErr: true},
		{name: "space", server: "g 1", wantErr: true},
		{name: "equals", server: "g1=x", wantErr: true},
		{name: "leading hyphen", server: "-g1", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateServer(tc.server)
			if tc.wantErr != (err != nil) {
				t.Fatalf("validateServer(%q) = %v, want error: %t", tc.server, err, tc.wantErr)
			}
		})
	}
}
