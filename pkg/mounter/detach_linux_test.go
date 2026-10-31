//go:build linux

package mounter

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestDetachMount checks the real MNT_DETACH call.
//
// Only root may unmount, so the test is skipped for everybody else; it is meant
// for the privileged container the architect runs the suite in. It only ever
// calls detachMount on paths that are not mount points, so it cannot unmount
// anything of the machine it runs on.
func TestDetachMount(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("detachMount needs root to unmount")
	}

	t.Run("a directory that is not a mount point is success", func(t *testing.T) {
		dir := t.TempDir()
		if err := detachMount(dir); err != nil {
			t.Fatalf("detachMount(%q) = %v, want nil", dir, err)
		}
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("detachMount(%q) removed the directory: %v", dir, err)
		}
	})

	t.Run("a missing path is success", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "does-not-exist")
		if err := detachMount(path); err != nil {
			t.Fatalf("detachMount(%q) = %v, want nil", path, err)
		}
	})

	t.Run("an empty path does not panic", func(t *testing.T) {
		// Unmount("") fails with ENOENT on Linux, which detachMount treats as
		// success; what matters is that it does not panic and that the caller
		// protects the path argument (Mounter.Detach validates it first).
		if err := detachMount(""); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("detachMount(\"\") = %v, want nil or fs.ErrNotExist", err)
		}
	})
}
