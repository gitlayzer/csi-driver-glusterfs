package mounter

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mount "k8s.io/mount-utils"
)

// mountInfoLine renders one line of /proc/self/mountinfo.
//
// The real format is
//
//	36 35 0:24 / / rw,noatime master:1 - ext4 /dev/vda1 rw,errors=continue
//
// i.e. ID, parent ID, major:minor, root, mount point, per-mount options, zero or
// more optional fields, "-", file system type, source and per-superblock options.
// The optional fields are left out here, the parser accepts the "-" right after
// the per-mount options.
func mountInfoLine(id, parentID, device, root, mountPoint, options, fsType, source string) string {
	return strings.Join([]string{id, parentID, device, root, mountPoint, options, "-", fsType, source, "rw"}, " ") + "\n"
}

// writeMountInfo writes lines to a mountinfo file in a temporary directory and
// returns its path.
func writeMountInfo(t *testing.T, lines ...string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "")), 0o600); err != nil {
		t.Fatalf("write mountinfo: %v", err)
	}
	return path
}

// TestBindTargetsFrom checks the pure matching function, which every platform
// compiles: what counts as a bind target of the staging mount and what does not.
func TestBindTargetsFrom(t *testing.T) {
	const (
		staging  = "/var/lib/kubelet/plugins/staging"
		resolved = "/private/var/lib/kubelet/plugins/staging"
	)

	// entry builds a mount table entry with the given identity.
	entry := func(major, minor int, root, mountPoint string, readOnly bool) mountInfo {
		options := []string{"rw"}
		if readOnly {
			options = []string{"rw", "ro"}
		}
		return mountInfo{Major: major, Minor: minor, Root: root, MountPoint: mountPoint, MountOptions: options}
	}

	tests := []struct {
		name     string
		infos    []mountInfo
		source   string
		resolved string
		want     []BindTarget
	}{
		{
			name:     "empty mount table",
			infos:    nil,
			source:   staging,
			resolved: staging,
			want:     []BindTarget{},
		},
		{
			name: "staging is not in the table",
			infos: []mountInfo{
				entry(1, 1, "/", "/", false),
				entry(37, 1, "/pvc-1", "/somewhere/else", false),
			},
			source:   staging,
			resolved: staging,
			want:     []BindTarget{},
		},
		{
			name: "staging with two bind targets, one read only",
			infos: []mountInfo{
				entry(1, 1, "/", "/", false),
				entry(37, 1, "/pvc-1", staging, false),
				entry(37, 1, "/pvc-1", "/target/rw", false),
				entry(37, 1, "/pvc-1", "/target/ro", true),
			},
			source:   staging,
			resolved: staging,
			want:     []BindTarget{{Path: "/target/rw"}, {Path: "/target/ro", ReadOnly: true}},
		},
		{
			name: "a different root is not a bind target",
			infos: []mountInfo{
				entry(37, 1, "/pvc-1", staging, false),
				entry(37, 1, "/pvc-2", "/other/subdir", false),
			},
			source:   staging,
			resolved: staging,
			want:     []BindTarget{},
		},
		{
			name: "a different minor device is not a bind target",
			infos: []mountInfo{
				entry(37, 1, "/pvc-1", staging, false),
				entry(37, 2, "/pvc-1", "/other/volume", false),
			},
			source:   staging,
			resolved: staging,
			want:     []BindTarget{},
		},
		{
			name: "a different major device is not a bind target",
			infos: []mountInfo{
				entry(37, 1, "/pvc-1", staging, false),
				entry(38, 1, "/pvc-1", "/other/volume", false),
			},
			source:   staging,
			resolved: staging,
			want:     []BindTarget{},
		},
		{
			name: "a stacked staging mount uses the top-most entry",
			infos: []mountInfo{
				entry(37, 1, "/pvc-1", staging, false),
				entry(37, 5, "/pvc-1", "/target/of/the/upper/mount", false),
				entry(37, 9, "/pvc-1", staging, false),
				entry(37, 9, "/pvc-1", "/target/of/the/top/mount", false),
			},
			source:   staging,
			resolved: staging,
			want:     []BindTarget{{Path: "/target/of/the/top/mount"}},
		},
		{
			name: "the resolved path matches a symbolic link",
			infos: []mountInfo{
				entry(37, 1, "/pvc-1", resolved, false),
				entry(37, 1, "/pvc-1", "/target", false),
			},
			source:   staging,
			resolved: resolved,
			want:     []BindTarget{{Path: "/target"}},
		},
		{
			name: "a repeated mount point is deduplicated, the last value wins",
			infos: []mountInfo{
				entry(37, 1, "/pvc-1", staging, false),
				entry(37, 1, "/pvc-1", "/target", false),
				entry(37, 1, "/pvc-1", "/other", false),
				entry(37, 1, "/pvc-1", "/target", true),
			},
			source:   staging,
			resolved: staging,
			want:     []BindTarget{{Path: "/target", ReadOnly: true}, {Path: "/other"}},
		},
		{
			name: "an unclean or relative mount point is skipped",
			infos: []mountInfo{
				entry(37, 1, "/pvc-1", staging, false),
				entry(37, 1, "/pvc-1", "target", false),
				entry(37, 1, "/pvc-1", "/target/../unclean", false),
				entry(37, 1, "/pvc-1", "/target", false),
			},
			source:   staging,
			resolved: staging,
			want:     []BindTarget{{Path: "/target"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := bindTargetsFrom(tc.infos, tc.source, tc.resolved)
			if got == nil {
				t.Fatal("bindTargetsFrom() = nil, want an empty slice")
			}
			if len(got) != len(tc.want) {
				t.Fatalf("bindTargetsFrom() = %+v, want %+v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("bindTargetsFrom()[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestBindTargets checks the file based wrapper: it reads a mountinfo fixture and
// rejects an unusable source path.
func TestBindTargets(t *testing.T) {
	const staging = "/var/lib/kubelet/plugins/staging"

	// The device numbers are deliberately not the ones mount-utils reports: only
	// the fixture is read, never the mount table of the test machine.
	fixture := writeMountInfo(t,
		mountInfoLine("36", "35", "0:24", "/", "/", "rw,nosuid", "ext4", "/dev/vda1"),
		mountInfoLine("37", "36", "0:55", "/pvc-1", staging, "rw,nosuid,nodev", "fuse.glusterfs", "g1:/vol/pvc-1"),
		mountInfoLine("38", "37", "0:55", "/pvc-1", "/target/rw", "rw,relatime", "fuse.glusterfs", "g1:/vol/pvc-1"),
		mountInfoLine("39", "37", "0:55", "/pvc-1", "/target/ro", "ro,relatime", "fuse.glusterfs", "g1:/vol/pvc-1"),
		mountInfoLine("40", "37", "0:56", "/pvc-1", "/other/volume", "rw,relatime", "fuse.glusterfs", "g1:/vol/pvc-2"),
	)

	tests := []struct {
		name       string
		mountInfo  string
		source     string
		broken     bool
		invalid    bool
		wantErrMsg string
		want       []BindTarget
	}{
		{
			name:      "reads the bind targets of the staging mount",
			mountInfo: fixture,
			source:    staging,
			want:      []BindTarget{{Path: "/target/rw"}, {Path: "/target/ro", ReadOnly: true}},
		},
		{
			name:      "a source that is not a mount point",
			mountInfo: fixture,
			source:    "/var/lib/kubelet/plugins/unknown",
			want:      []BindTarget{},
		},
		{
			name:       "missing mountinfo file",
			mountInfo:  filepath.Join(t.TempDir(), "does-not-exist"),
			source:     staging,
			broken:     true,
			wantErrMsg: "read mountinfo",
		},
		{
			name:       "broken mountinfo file",
			mountInfo:  writeMountInfo(t, "this is not a mountinfo line\n"),
			source:     staging,
			broken:     true,
			wantErrMsg: "read mountinfo",
		},
		{
			name:      "relative source",
			mountInfo: fixture,
			source:    "relative/staging",
			broken:    true,
			invalid:   true,
		},
		{
			name:      "unclean source",
			mountInfo: fixture,
			source:    "/a/../b",
			broken:    true,
			invalid:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := New(mount.NewFakeMounter(nil), testLogDir).(*mounter)
			m.mountInfoPath = tc.mountInfo

			got, err := m.BindTargets(tc.source)

			if tc.invalid {
				if !errors.Is(err, ErrInvalidSpec) {
					t.Fatalf("BindTargets(%q) = %v, want an error wrapping ErrInvalidSpec", tc.source, err)
				}
			}
			if tc.wantErrMsg != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErrMsg)) {
				t.Fatalf("BindTargets(%q) = %v, want an error containing %q", tc.source, err, tc.wantErrMsg)
			}
			if tc.broken {
				if err == nil {
					t.Fatalf("BindTargets(%q) = nil, want an error", tc.source)
				}
				if got != nil {
					t.Fatalf("BindTargets(%q) = %+v, want nil on error", tc.source, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("BindTargets(%q) = %v, want nil", tc.source, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("BindTargets(%q) = %+v, want %+v", tc.source, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("BindTargets(%q)[%d] = %+v, want %+v", tc.source, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestBindTargetsProcMountInfo checks the default of the production mounter: the
// real mount table is read and the root file system never reports itself.
func TestBindTargetsProcMountInfo(t *testing.T) {
	if _, err := os.Stat(procMountInfoPath); err != nil {
		t.Skipf("%s is not available: %v", procMountInfoPath, err)
	}

	m := New(mount.NewFakeMounter(nil), testLogDir)
	if got := m.(*mounter).mountInfoPath; got != procMountInfoPath {
		t.Fatalf("mountInfoPath = %q, want %q", got, procMountInfoPath)
	}

	targets, err := m.BindTargets("/")
	if err != nil {
		t.Fatalf("BindTargets(/) = %v, want nil", err)
	}
	for _, target := range targets {
		if target.Path == "/" {
			t.Fatal("BindTargets(/) returned the source itself")
		}
	}
}

// TestDetach checks the in-package wrapper around the platform detach: it
// validates the path and passes the call through to the injected function.
func TestDetach(t *testing.T) {
	injected := errors.New("injected detach failure")

	tests := []struct {
		name     string
		target   string
		failErr  error
		invalid  bool
		wantErrs []error
		wantCall []string
	}{
		{name: "detaches the target", target: "/staging", wantCall: []string{"/staging"}},
		{
			name:     "injected failure is wrapped",
			target:   "/staging",
			failErr:  injected,
			wantErrs: []error{injected},
			wantCall: []string{"/staging"},
		},
		{name: "relative target", target: "staging", invalid: true},
		{name: "empty target", target: "", invalid: true},
		{name: "unclean target", target: "/a/../b", invalid: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := New(mount.NewFakeMounter(nil), testLogDir).(*mounter)
			var calls []string
			m.detach = func(target string) error {
				calls = append(calls, target)
				return tc.failErr
			}

			err := m.Detach(tc.target)

			if tc.invalid {
				if !errors.Is(err, ErrInvalidSpec) {
					t.Fatalf("Detach(%q) = %v, want ErrInvalidSpec", tc.target, err)
				}
			}
			for _, want := range tc.wantErrs {
				if !errors.Is(err, want) {
					t.Fatalf("Detach(%q) = %v, want an error wrapping %v", tc.target, err, want)
				}
			}
			if !tc.invalid && tc.wantErrs == nil && err != nil {
				t.Fatalf("Detach(%q) = %v, want nil", tc.target, err)
			}

			if len(calls) != len(tc.wantCall) {
				t.Fatalf("detach calls = %v, want %v", calls, tc.wantCall)
			}
			for i := range tc.wantCall {
				if calls[i] != tc.wantCall[i] {
					t.Fatalf("detach calls = %v, want %v", calls, tc.wantCall)
				}
			}
		})
	}
}
