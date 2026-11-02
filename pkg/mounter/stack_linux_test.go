//go:build linux

package mounter

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	mount "k8s.io/mount-utils"
)

// TestStackBindMountRealMounts checks the two properties the recovery relies on
// against a real mount table: a stacked bind mount is a second entry for the same
// path (so a container that sees the path through a HostToContainer mount gets a
// new child mount instead of a replaced one), and the read-only layer on top is
// the one that governs writing.
//
// Only root may mount, so the test is skipped for everybody else; it is meant for
// the privileged container the architect runs the suite in. Everything happens
// below a temporary directory, and the tmpfs is unmounted again even when an
// assertion fails.
func TestStackBindMountRealMounts(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("a real bind mount needs root")
	}

	ctx := context.Background()
	inner := mount.New("")
	m := New(inner, "")

	// The source is a tmpfs of its own: the stacked layer has to be read only
	// although the file system it comes from is writable.
	source := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) = %v, want nil", source, err)
	}
	if err := inner.Mount("tmpfs", source, "tmpfs", []string{"size=1m"}); err != nil {
		t.Fatalf("mount tmpfs at %q = %v, want nil", source, err)
	}
	t.Cleanup(func() {
		if err := inner.Unmount(source); err != nil {
			t.Logf("cleanup: unmount tmpfs %q = %v", source, err)
		}
	})

	target := filepath.Join(t.TempDir(), "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) = %v, want nil", target, err)
	}

	// The first layer is the ordinary bind mount NodePublishVolume makes.
	if err := m.BindMount(ctx, source, target, false); err != nil {
		t.Fatalf("BindMount(%q, %q) = %v, want nil", source, target, err)
	}
	// The second layer is the repair of a target that looks corrupted; the tests
	// use a healthy one, because a corrupted mount point cannot be produced
	// without a FUSE process.
	if err := m.StackBindMount(ctx, source, target, true); err != nil {
		t.Fatalf("StackBindMount(%q, %q, ro) = %v, want nil", source, target, err)
	}

	entries := mountInfoLinesOf(t, target)
	if len(entries) != 2 {
		t.Fatalf("mountinfo entries of %q = %d, want 2: %v", target, len(entries), entries)
	}
	// The last entry is the top-most mount.
	if top := entries[len(entries)-1]; !strings.Contains(top, " ro,") {
		t.Errorf("top-most mountinfo entry = %q, want it read only", top)
	}

	// The read-only layer is the one that decides: writing has to fail. Root
	// passes every permission check, so the error is EROFS, which fs.ErrPermission
	// does not match.
	if err := os.WriteFile(filepath.Join(target, "file"), []byte("x"), 0o600); !errors.Is(err, syscall.EROFS) {
		t.Errorf("WriteFile() through the read only layer = %v, want EROFS", err)
	}

	// Cleanup removes both layers and the directory, which is what
	// NodeUnpublishVolume needs after the recovery stacked a layer.
	if err := m.Cleanup(ctx, target); err != nil {
		t.Fatalf("Cleanup(%q) = %v, want nil", target, err)
	}
	if _, err := os.Stat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat(%q) = %v, want the directory to be gone", target, err)
	}
	if entries := mountInfoLinesOf(t, target); len(entries) != 0 {
		t.Errorf("mountinfo entries of %q after Cleanup = %d, want 0: %v", target, len(entries), entries)
	}
}

// mountInfoLinesOf returns the /proc/self/mountinfo lines whose mount point is
// path, in the order of the file, i.e. the top-most mount last.
func mountInfoLinesOf(t *testing.T, path string) []string {
	t.Helper()

	infos, err := mount.ParseMountInfo("/proc/self/mountinfo")
	if err != nil {
		t.Fatalf("ParseMountInfo(/proc/self/mountinfo) = %v, want nil", err)
	}
	var lines []string
	for _, info := range infos {
		if info.MountPoint == path {
			lines = append(lines, strings.Join([]string{
				"mount", "point", info.MountPoint,
				"options", strings.Join(info.MountOptions, ","),
				"type", info.FsType,
			}, " "))
		}
	}
	return lines
}
