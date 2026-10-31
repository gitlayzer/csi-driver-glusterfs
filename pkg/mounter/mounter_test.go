package mounter

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	mount "k8s.io/mount-utils"
)

// testLogDir is a valid, clean, absolute log directory used by the tests that do
// not care about the log file.
const testLogDir = "/var/log/glusterfs/csi"

// actionCount returns how many log entries of the given action kind are
// recorded in fake.
func actionCount(fake *mount.FakeMounter, action string) int {
	count := 0
	for _, entry := range fake.GetLog() {
		if entry.Action == action {
			count++
		}
	}
	return count
}

// newTestTarget creates a directory for target and returns it together with the
// path the FakeMounter records, which is the symbolic link resolved one (on
// macOS /var is a symlink to /private/var, so they differ).
func newTestTarget(t *testing.T) (target, resolved string) {
	t.Helper()
	target = t.TempDir()
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", target, err)
	}
	return target, resolved
}

func TestMountGlusterfs(t *testing.T) {
	spec := Spec{Servers: []string{"g1", "g2"}, Volume: "vol", Subdir: "pvc-1"}
	wantSource := "g1:/vol/pvc-1"
	_, wantOptions, err := BuildMountArgs(spec, testLogDir)
	if err != nil {
		t.Fatalf("BuildMountArgs(%+v): %v", spec, err)
	}

	t.Run("first mount then idempotent repeat", func(t *testing.T) {
		target, resolved := newTestTarget(t)
		fake := mount.NewFakeMounter(nil)
		m := New(fake, testLogDir)
		ctx := context.Background()

		if err := m.MountGlusterfs(ctx, spec, target); err != nil {
			t.Fatalf("MountGlusterfs(%q) = %v, want nil", target, err)
		}
		log := fake.GetLog()
		if len(log) != 1 || log[0].Action != mount.FakeActionMount {
			t.Fatalf("mount log = %+v, want exactly one mount", log)
		}
		if log[0].Source != wantSource || log[0].FSType != FSType {
			t.Errorf("mount log source/fstype = %q/%q, want %q/%q", log[0].Source, log[0].FSType, wantSource, FSType)
		}
		points := fake.MountPoints
		if len(points) != 1 {
			t.Fatalf("MountPoints = %+v, want exactly one entry", points)
		}
		if points[0].Path != resolved {
			t.Errorf("MountPoints[0].Path = %q, want %q", points[0].Path, resolved)
		}
		assertOptions(t, points[0].Opts, wantOptions)

		// The same call again must not mount a second time.
		if err := m.MountGlusterfs(ctx, spec, target); err != nil {
			t.Fatalf("second MountGlusterfs(%q) = %v, want nil", target, err)
		}
		if got := actionCount(fake, mount.FakeActionMount); got != 1 {
			t.Fatalf("mount count after the repeated call = %d, want 1", got)
		}
	})

	t.Run("existing mount from another server of the same volume is accepted", func(t *testing.T) {
		tests := []struct {
			name   string
			device string
			fsType string
			opts   []string
		}{
			{name: "backup server, fuse.glusterfs", device: "g2:/vol/pvc-1", fsType: "fuse.glusterfs", opts: []string{"rw"}},
			{name: "primary server, glusterfs", device: "g1:/vol/pvc-1", fsType: FSType, opts: nil},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				target, resolved := newTestTarget(t)
				fake := mount.NewFakeMounter([]mount.MountPoint{{
					Device: tc.device, Path: resolved, Type: tc.fsType, Opts: tc.opts,
				}})
				m := New(fake, testLogDir)

				if err := m.MountGlusterfs(context.Background(), spec, target); err != nil {
					t.Fatalf("MountGlusterfs(%q) = %v, want nil", target, err)
				}
				if got := actionCount(fake, mount.FakeActionMount); got != 0 {
					t.Fatalf("mount count = %d, want 0 for an already mounted target", got)
				}
			})
		}
	})

	t.Run("conflicts", func(t *testing.T) {
		tests := []struct {
			name string
			spec Spec
			have mount.MountPoint
		}{
			{
				name: "different volume",
				spec: spec,
				have: mount.MountPoint{Device: "g1:/other/pvc-1", Type: "fuse.glusterfs"},
			},
			{
				name: "different subdir",
				spec: spec,
				have: mount.MountPoint{Device: "g1:/vol/pvc-2", Type: "fuse.glusterfs"},
			},
			{
				name: "different file system type",
				spec: spec,
				have: mount.MountPoint{Device: "g1:/vol/pvc-1", Type: "nfs"},
			},
			{
				name: "read only requested but mounted read write",
				spec: Spec{Servers: []string{"g1"}, Volume: "vol", ReadOnly: true},
				have: mount.MountPoint{Device: "g1:/vol", Type: "fuse.glusterfs"},
			},
			{
				name: "read write requested but mounted read only",
				spec: Spec{Servers: []string{"g1"}, Volume: "vol"},
				have: mount.MountPoint{Device: "g1:/vol", Type: "fuse.glusterfs", Opts: []string{"ro"}},
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				target, resolved := newTestTarget(t)
				tc.have.Path = resolved
				fake := mount.NewFakeMounter([]mount.MountPoint{tc.have})
				m := New(fake, testLogDir)

				err := m.MountGlusterfs(context.Background(), tc.spec, target)
				if !errors.Is(err, ErrMountConflict) {
					t.Fatalf("MountGlusterfs(%q) = %v, want ErrMountConflict", target, err)
				}
				if !strings.Contains(err.Error(), target) {
					t.Errorf("error %q does not mention the target %q", err, target)
				}
				if got := actionCount(fake, mount.FakeActionMount); got != 0 {
					t.Errorf("mount count = %d, want 0", got)
				}
			})
		}
	})

	t.Run("mount point without a mount table entry is a conflict", func(t *testing.T) {
		target, _ := newTestTarget(t)
		fake := &lyingMounter{FakeMounter: mount.NewFakeMounter(nil)}
		m := New(fake, testLogDir)

		err := m.MountGlusterfs(context.Background(), spec, target)
		if !errors.Is(err, ErrMountConflict) {
			t.Fatalf("MountGlusterfs(%q) = %v, want ErrMountConflict", target, err)
		}
	})

	t.Run("corrupted mount point", func(t *testing.T) {
		tests := []struct {
			name string
			err  error
		}{
			{name: "plain errno", err: syscall.ENOTCONN},
			{name: "os.PathError", err: &os.PathError{Op: "stat", Path: "/x", Err: syscall.ENOTCONN}},
			{name: "wrapped errno", err: fmt.Errorf("x: %w", syscall.ESTALE)},
			{name: "wrapped path error", err: fmt.Errorf("x: %w", &os.PathError{Op: "stat", Path: "/x", Err: syscall.EIO})},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				target, _ := newTestTarget(t)
				fake := mount.NewFakeMounter(nil)
				fake.MountCheckErrors = map[string]error{target: tc.err}
				m := New(fake, testLogDir)

				err := m.MountGlusterfs(context.Background(), spec, target)
				if !errors.Is(err, ErrCorruptedMount) {
					t.Fatalf("MountGlusterfs(%q) = %v, want ErrCorruptedMount", target, err)
				}
				if got := len(fake.GetLog()); got != 0 {
					t.Errorf("mount log = %+v, want no mount and no unmount", fake.GetLog())
				}
			})
		}
	})

	t.Run("missing target", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "does-not-exist")
		fake := mount.NewFakeMounter(nil)
		m := New(fake, testLogDir)

		err := m.MountGlusterfs(context.Background(), spec, target)
		if err == nil {
			t.Fatalf("MountGlusterfs(%q) = nil, want an error", target)
		}
		if errors.Is(err, ErrCorruptedMount) {
			t.Errorf("MountGlusterfs(%q) = %v, want a not-exist error instead of ErrCorruptedMount", target, err)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("MountGlusterfs(%q) = %v, want it to wrap fs.ErrNotExist", target, err)
		}
		if got := actionCount(fake, mount.FakeActionMount); got != 0 {
			t.Errorf("mount count = %d, want 0", got)
		}
	})

	t.Run("invalid spec", func(t *testing.T) {
		target, _ := newTestTarget(t)
		fake := mount.NewFakeMounter(nil)
		m := New(fake, testLogDir)

		err := m.MountGlusterfs(context.Background(), Spec{Volume: "vol"}, target)
		if !errors.Is(err, ErrInvalidSpec) {
			t.Fatalf("MountGlusterfs with an empty server list = %v, want ErrInvalidSpec", err)
		}
		if got := actionCount(fake, mount.FakeActionMount); got != 0 {
			t.Errorf("mount count = %d, want 0", got)
		}
	})

	t.Run("invalid target", func(t *testing.T) {
		tests := []struct {
			name   string
			target string
		}{
			{name: "empty", target: ""},
			{name: "relative", target: "target"},
			{name: "unclean", target: "/a/../b"},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				fake := mount.NewFakeMounter(nil)
				m := New(fake, testLogDir)

				err := m.MountGlusterfs(context.Background(), spec, tc.target)
				if !errors.Is(err, ErrInvalidSpec) {
					t.Fatalf("MountGlusterfs(%q) = %v, want ErrInvalidSpec", tc.target, err)
				}
				if got := len(fake.GetLog()); got != 0 {
					t.Errorf("mount log = %+v, want no action", fake.GetLog())
				}
			})
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		target, _ := newTestTarget(t)
		fake := mount.NewFakeMounter(nil)
		m := New(fake, testLogDir)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := m.MountGlusterfs(ctx, spec, target)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("MountGlusterfs with a cancelled context = %v, want context.Canceled", err)
		}
		if got := actionCount(fake, mount.FakeActionMount); got != 0 {
			t.Errorf("mount count = %d, want 0", got)
		}
	})

	t.Run("mount failure is wrapped", func(t *testing.T) {
		target, _ := newTestTarget(t)
		injected := errors.New("injected mount failure")
		fake := &failingMountMounter{FakeMounter: mount.NewFakeMounter(nil), err: injected}
		m := New(fake, testLogDir)

		err := m.MountGlusterfs(context.Background(), spec, target)
		if !errors.Is(err, injected) {
			t.Fatalf("MountGlusterfs(%q) = %v, want it to wrap %v", target, err, injected)
		}
		if !strings.Contains(err.Error(), target) {
			t.Errorf("error %q does not mention the target %q", err, target)
		}
	})

	t.Run("list failure is returned", func(t *testing.T) {
		target, resolved := newTestTarget(t)
		injected := errors.New("injected list failure")
		fake := &failingListMounter{FakeMounter: mount.NewFakeMounter([]mount.MountPoint{
			{Device: "g1:/vol/pvc-1", Path: resolved, Type: "fuse.glusterfs"},
		}), err: injected}
		m := New(fake, testLogDir)

		err := m.MountGlusterfs(context.Background(), spec, target)
		if !errors.Is(err, injected) {
			t.Fatalf("MountGlusterfs(%q) = %v, want it to wrap %v", target, err, injected)
		}
	})
}

func TestBindMount(t *testing.T) {
	t.Run("first mount", func(t *testing.T) {
		tests := []struct {
			name        string
			readOnly    bool
			wantOptions []string
		}{
			{name: "read write", readOnly: false, wantOptions: []string{"bind"}},
			{name: "read only", readOnly: true, wantOptions: []string{"bind", "ro"}},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				source, _ := newTestTarget(t)
				target, resolved := newTestTarget(t)
				fake := mount.NewFakeMounter(nil)
				m := New(fake, testLogDir)

				if err := m.BindMount(context.Background(), source, target, tc.readOnly); err != nil {
					t.Fatalf("BindMount(%q, %q, %t) = %v, want nil", source, target, tc.readOnly, err)
				}
				log := fake.GetLog()
				if len(log) != 1 || log[0].Action != mount.FakeActionMount {
					t.Fatalf("mount log = %+v, want exactly one mount", log)
				}
				if log[0].Source != source || log[0].FSType != "" {
					t.Errorf("mount log source/fstype = %q/%q, want %q/\"\"", log[0].Source, log[0].FSType, source)
				}
				points := fake.MountPoints
				if len(points) != 1 || points[0].Path != resolved {
					t.Fatalf("MountPoints = %+v, want one entry for %q", points, resolved)
				}
				assertOptions(t, points[0].Opts, tc.wantOptions)

				// Repeating the call must not mount again.
				if err := m.BindMount(context.Background(), source, target, tc.readOnly); err != nil {
					t.Fatalf("second BindMount = %v, want nil", err)
				}
				if got := actionCount(fake, mount.FakeActionMount); got != 1 {
					t.Errorf("mount count after the repeated call = %d, want 1", got)
				}
			})
		}
	})

	t.Run("read only mismatch is a conflict", func(t *testing.T) {
		tests := []struct {
			name     string
			have     mount.MountPoint
			readOnly bool
		}{
			{
				name:     "mounted read write, requested read only",
				have:     mount.MountPoint{Device: "/dev/sda", Type: "ext4"},
				readOnly: true,
			},
			{
				name:     "mounted read only, requested read write",
				have:     mount.MountPoint{Device: "/dev/sda", Type: "ext4", Opts: []string{"rw", "ro"}},
				readOnly: false,
			},
			{
				name:     "bind mount of a glusterfs volume has the original device",
				have:     mount.MountPoint{Device: "g1:/vol", Type: "fuse.glusterfs"},
				readOnly: true,
			},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				source, _ := newTestTarget(t)
				target, resolved := newTestTarget(t)
				tc.have.Path = resolved
				fake := mount.NewFakeMounter([]mount.MountPoint{tc.have})
				m := New(fake, testLogDir)

				err := m.BindMount(context.Background(), source, target, tc.readOnly)
				if !errors.Is(err, ErrMountConflict) {
					t.Fatalf("BindMount(%q, %t) = %v, want ErrMountConflict", target, tc.readOnly, err)
				}
				if got := actionCount(fake, mount.FakeActionMount); got != 0 {
					t.Errorf("mount count = %d, want 0", got)
				}
			})
		}
	})

	t.Run("mount point without a mount table entry is a conflict", func(t *testing.T) {
		source, _ := newTestTarget(t)
		target, _ := newTestTarget(t)
		fake := &lyingMounter{FakeMounter: mount.NewFakeMounter(nil)}
		m := New(fake, testLogDir)

		err := m.BindMount(context.Background(), source, target, false)
		if !errors.Is(err, ErrMountConflict) {
			t.Fatalf("BindMount(%q) = %v, want ErrMountConflict", target, err)
		}
	})

	t.Run("list failure is returned", func(t *testing.T) {
		source, _ := newTestTarget(t)
		target, resolved := newTestTarget(t)
		injected := errors.New("injected list failure")
		fake := &lyingListMounter{
			FakeMounter: mount.NewFakeMounter([]mount.MountPoint{{Device: source, Path: resolved, Type: "ext4"}}),
			err:         injected,
		}
		m := New(fake, testLogDir)

		if err := m.BindMount(context.Background(), source, target, false); !errors.Is(err, injected) {
			t.Fatalf("BindMount(%q) = %v, want it to wrap %v", target, err, injected)
		}
	})

	t.Run("corrupted mount point", func(t *testing.T) {
		source, _ := newTestTarget(t)
		target, _ := newTestTarget(t)
		fake := mount.NewFakeMounter(nil)
		fake.MountCheckErrors = map[string]error{target: syscall.ENOTCONN}
		m := New(fake, testLogDir)

		err := m.BindMount(context.Background(), source, target, false)
		if !errors.Is(err, ErrCorruptedMount) {
			t.Fatalf("BindMount(%q) = %v, want ErrCorruptedMount", target, err)
		}
		if got := len(fake.GetLog()); got != 0 {
			t.Errorf("mount log = %+v, want no action", fake.GetLog())
		}
	})

	t.Run("mount point check failure is returned", func(t *testing.T) {
		source, _ := newTestTarget(t)
		target, _ := newTestTarget(t)
		injected := errors.New("injected check failure")
		fake := mount.NewFakeMounter(nil)
		fake.MountCheckErrors = map[string]error{target: injected}
		m := New(fake, testLogDir)

		err := m.BindMount(context.Background(), source, target, false)
		if !errors.Is(err, injected) {
			t.Fatalf("BindMount(%q) = %v, want it to wrap %v", target, err, injected)
		}
		if got := len(fake.GetLog()); got != 0 {
			t.Errorf("mount log = %+v, want no action", fake.GetLog())
		}
	})

	t.Run("missing target", func(t *testing.T) {
		source, _ := newTestTarget(t)
		target := filepath.Join(t.TempDir(), "does-not-exist")
		fake := mount.NewFakeMounter(nil)
		m := New(fake, testLogDir)

		err := m.BindMount(context.Background(), source, target, false)
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("BindMount(%q) = %v, want an error wrapping fs.ErrNotExist", target, err)
		}
	})

	t.Run("invalid source", func(t *testing.T) {
		tests := []struct {
			name   string
			source string
		}{
			{name: "empty", source: ""},
			{name: "relative", source: "source"},
			{name: "unclean", source: "/a/../b"},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				target, _ := newTestTarget(t)
				fake := mount.NewFakeMounter(nil)
				m := New(fake, testLogDir)

				err := m.BindMount(context.Background(), tc.source, target, false)
				if !errors.Is(err, ErrInvalidSpec) {
					t.Fatalf("BindMount(%q) = %v, want ErrInvalidSpec", tc.source, err)
				}
				if got := len(fake.GetLog()); got != 0 {
					t.Errorf("mount log = %+v, want no action", fake.GetLog())
				}
			})
		}
	})

	t.Run("invalid target", func(t *testing.T) {
		tests := []struct {
			name   string
			target string
		}{
			{name: "empty", target: ""},
			{name: "relative", target: "target"},
			{name: "unclean", target: "/a/../b"},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				source, _ := newTestTarget(t)
				fake := mount.NewFakeMounter(nil)
				m := New(fake, testLogDir)

				err := m.BindMount(context.Background(), source, tc.target, false)
				if !errors.Is(err, ErrInvalidSpec) {
					t.Fatalf("BindMount(%q) = %v, want ErrInvalidSpec", tc.target, err)
				}
				if got := len(fake.GetLog()); got != 0 {
					t.Errorf("mount log = %+v, want no action", fake.GetLog())
				}
			})
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		source, _ := newTestTarget(t)
		target, _ := newTestTarget(t)
		fake := mount.NewFakeMounter(nil)
		m := New(fake, testLogDir)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := m.BindMount(ctx, source, target, false)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("BindMount with a cancelled context = %v, want context.Canceled", err)
		}
		if got := len(fake.GetLog()); got != 0 {
			t.Errorf("mount log = %+v, want no action", fake.GetLog())
		}
	})

	t.Run("mount failure is wrapped", func(t *testing.T) {
		source, _ := newTestTarget(t)
		target, _ := newTestTarget(t)
		injected := errors.New("injected mount failure")
		fake := &failingMountMounter{FakeMounter: mount.NewFakeMounter(nil), err: injected}
		m := New(fake, testLogDir)

		err := m.BindMount(context.Background(), source, target, false)
		if !errors.Is(err, injected) {
			t.Fatalf("BindMount(%q) = %v, want it to wrap %v", target, err, injected)
		}
	})
}

func TestUnmountAndCleanup(t *testing.T) {
	spec := Spec{Servers: []string{"g1"}, Volume: "vol"}

	t.Run("mounted target", func(t *testing.T) {
		for _, cleanup := range []bool{false, true} {
			name := "Unmount"
			if cleanup {
				name = "Cleanup"
			}
			t.Run(name, func(t *testing.T) {
				target, _ := newTestTarget(t)
				fake := mount.NewFakeMounter(nil)
				m := New(fake, testLogDir)
				ctx := context.Background()

				if err := m.MountGlusterfs(ctx, spec, target); err != nil {
					t.Fatalf("MountGlusterfs(%q) = %v, want nil", target, err)
				}

				var err error
				if cleanup {
					err = m.Cleanup(ctx, target)
				} else {
					err = m.Unmount(ctx, target)
				}
				if err != nil {
					t.Fatalf("%s(%q) = %v, want nil", name, target, err)
				}
				if got := actionCount(fake, mount.FakeActionUnmount); got != 1 {
					t.Errorf("unmount count = %d, want 1", got)
				}
				// After Cleanup the directory is gone, so the mount point check
				// reports a missing path instead of "not mounted".
				mounted, merr := m.IsMountPoint(target)
				if mounted {
					t.Errorf("IsMountPoint(%q) = true after unmounting, want false", target)
				}
				if merr != nil && !errors.Is(merr, fs.ErrNotExist) {
					t.Errorf("IsMountPoint(%q) = %v, want nil or fs.ErrNotExist", target, merr)
				}

				exists := pathExists(t, target)
				if cleanup && exists {
					t.Errorf("%s(%q) kept the directory, want it removed", name, target)
				}
				if !cleanup && !exists {
					t.Errorf("%s(%q) removed the directory, want it kept", name, target)
				}

				// Both are idempotent, also when the directory is already gone.
				if err := m.Unmount(ctx, target); err != nil {
					t.Fatalf("second Unmount(%q) = %v, want nil", target, err)
				}
				if err := m.Cleanup(ctx, target); err != nil {
					t.Fatalf("second Cleanup(%q) = %v, want nil", target, err)
				}
				if got := actionCount(fake, mount.FakeActionUnmount); got != 1 {
					t.Errorf("unmount count after the repeated calls = %d, want 1", got)
				}
			})
		}
	})

	t.Run("not mounted", func(t *testing.T) {
		target, _ := newTestTarget(t)
		fake := mount.NewFakeMounter(nil)
		m := New(fake, testLogDir)
		ctx := context.Background()

		if err := m.Unmount(ctx, target); err != nil {
			t.Fatalf("Unmount(%q) = %v, want nil", target, err)
		}
		if got := actionCount(fake, mount.FakeActionUnmount); got != 0 {
			t.Errorf("unmount count = %d, want 0 for a target that is not mounted", got)
		}
		if !pathExists(t, target) {
			t.Fatalf("Unmount(%q) removed a directory that was not a mount point", target)
		}

		if err := m.Cleanup(ctx, target); err != nil {
			t.Fatalf("Cleanup(%q) = %v, want nil", target, err)
		}
		if pathExists(t, target) {
			t.Errorf("Cleanup(%q) kept the directory, want it removed", target)
		}
	})

	t.Run("corrupted mount is unmounted", func(t *testing.T) {
		target, resolved := newTestTarget(t)
		fake := mount.NewFakeMounter([]mount.MountPoint{{
			Device: "g1:/vol", Path: resolved, Type: "fuse.glusterfs",
		}})
		// FakeMounter looks MountCheckErrors up by the exact path it is given,
		// so the key is the target and not the resolved path.
		fake.MountCheckErrors = map[string]error{target: syscall.ENOTCONN}
		m := New(fake, testLogDir)

		if err := m.Unmount(context.Background(), target); err != nil {
			t.Fatalf("Unmount(%q) = %v, want nil", target, err)
		}
		if got := actionCount(fake, mount.FakeActionUnmount); got != 1 {
			t.Errorf("unmount count = %d, want 1", got)
		}
		if _, err := m.IsMountPoint(target); errors.Is(err, ErrCorruptedMount) {
			t.Errorf("IsMountPoint(%q) still reports a corrupted mount: %v", target, err)
		}
	})

	t.Run("corrupted mount, unmount fails", func(t *testing.T) {
		target, resolved := newTestTarget(t)
		injected := errors.New("injected unmount failure")
		fake := mount.NewFakeMounter([]mount.MountPoint{{
			Device: "g1:/vol", Path: resolved, Type: "fuse.glusterfs",
		}})
		fake.MountCheckErrors = map[string]error{target: syscall.EIO}
		fake.UnmountFunc = func(string) error { return injected }
		m := New(fake, testLogDir)

		err := m.Unmount(context.Background(), target)
		if !errors.Is(err, injected) {
			t.Fatalf("Unmount(%q) = %v, want it to wrap %v", target, err, injected)
		}
	})

	t.Run("non-empty directory is not removed", func(t *testing.T) {
		target, _ := newTestTarget(t)
		file := filepath.Join(target, "data")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile(%q): %v", file, err)
		}
		fake := mount.NewFakeMounter(nil)
		m := New(fake, testLogDir)

		if err := m.Cleanup(context.Background(), target); err == nil {
			t.Fatalf("Cleanup(%q) = nil, want an error for a non-empty directory", target)
		}
		if !pathExists(t, target) {
			t.Errorf("Cleanup(%q) removed the directory, want it kept", target)
		}
		if !pathExists(t, file) {
			t.Errorf("Cleanup(%q) removed %q, want it kept", target, file)
		}
	})

	t.Run("unmount failure keeps the directory", func(t *testing.T) {
		target, resolved := newTestTarget(t)
		injected := errors.New("injected unmount failure")
		fake := mount.NewFakeMounter([]mount.MountPoint{{
			Device: "g1:/vol", Path: resolved, Type: "fuse.glusterfs",
		}})
		fake.UnmountFunc = func(string) error { return injected }
		m := New(fake, testLogDir)

		err := m.Cleanup(context.Background(), target)
		if !errors.Is(err, injected) {
			t.Fatalf("Cleanup(%q) = %v, want it to wrap %v", target, err, injected)
		}
		if !strings.Contains(err.Error(), target) {
			t.Errorf("error %q does not mention the target %q", err, target)
		}
		if !pathExists(t, target) {
			t.Errorf("Cleanup(%q) removed the directory after a failed unmount, want it kept", target)
		}
	})

	t.Run("invalid target", func(t *testing.T) {
		tests := []struct {
			name   string
			target string
		}{
			{name: "empty", target: ""},
			{name: "relative", target: "target"},
			{name: "unclean", target: "/a/../b"},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				fake := mount.NewFakeMounter(nil)
				m := New(fake, testLogDir)

				if err := m.Unmount(context.Background(), tc.target); !errors.Is(err, ErrInvalidSpec) {
					t.Fatalf("Unmount(%q) = %v, want ErrInvalidSpec", tc.target, err)
				}
				if err := m.Cleanup(context.Background(), tc.target); !errors.Is(err, ErrInvalidSpec) {
					t.Fatalf("Cleanup(%q) = %v, want ErrInvalidSpec", tc.target, err)
				}
			})
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		target, resolved := newTestTarget(t)
		fake := mount.NewFakeMounter([]mount.MountPoint{{
			Device: "g1:/vol", Path: resolved, Type: "fuse.glusterfs",
		}})
		m := New(fake, testLogDir)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if err := m.Unmount(ctx, target); !errors.Is(err, context.Canceled) {
			t.Fatalf("Unmount with a cancelled context = %v, want context.Canceled", err)
		}
		if err := m.Cleanup(ctx, target); !errors.Is(err, context.Canceled) {
			t.Fatalf("Cleanup with a cancelled context = %v, want context.Canceled", err)
		}
		if got := actionCount(fake, mount.FakeActionUnmount); got != 0 {
			t.Errorf("unmount count = %d, want 0", got)
		}
	})

	t.Run("mount point check failure is returned", func(t *testing.T) {
		target, _ := newTestTarget(t)
		injected := errors.New("injected check failure")
		fake := mount.NewFakeMounter(nil)
		fake.MountCheckErrors = map[string]error{target: injected}
		m := New(fake, testLogDir)

		if err := m.Unmount(context.Background(), target); !errors.Is(err, injected) {
			t.Fatalf("Unmount(%q) = %v, want it to wrap %v", target, err, injected)
		}
		if got := actionCount(fake, mount.FakeActionUnmount); got != 0 {
			t.Errorf("unmount count = %d, want 0", got)
		}
	})
}

func TestIsMountPoint(t *testing.T) {
	spec := Spec{Servers: []string{"g1"}, Volume: "vol"}

	t.Run("mounted", func(t *testing.T) {
		target, _ := newTestTarget(t)
		fake := mount.NewFakeMounter(nil)
		m := New(fake, testLogDir)
		if err := m.MountGlusterfs(context.Background(), spec, target); err != nil {
			t.Fatalf("MountGlusterfs(%q) = %v, want nil", target, err)
		}

		mounted, err := m.IsMountPoint(target)
		if err != nil || !mounted {
			t.Fatalf("IsMountPoint(%q) = %t, %v, want true, nil", target, mounted, err)
		}
	})

	t.Run("not mounted", func(t *testing.T) {
		target, _ := newTestTarget(t)
		m := New(mount.NewFakeMounter(nil), testLogDir)

		mounted, err := m.IsMountPoint(target)
		if err != nil || mounted {
			t.Fatalf("IsMountPoint(%q) = %t, %v, want false, nil", target, mounted, err)
		}
	})

	t.Run("corrupted", func(t *testing.T) {
		target, _ := newTestTarget(t)
		fake := mount.NewFakeMounter(nil)
		fake.MountCheckErrors = map[string]error{target: syscall.ENOTCONN}
		m := New(fake, testLogDir)

		mounted, err := m.IsMountPoint(target)
		if mounted {
			t.Errorf("IsMountPoint(%q) = true, want false for a corrupted mount", target)
		}
		if !errors.Is(err, ErrCorruptedMount) {
			t.Fatalf("IsMountPoint(%q) = %v, want ErrCorruptedMount", target, err)
		}
		if !strings.Contains(err.Error(), target) {
			t.Errorf("error %q does not mention the target %q", err, target)
		}
	})

	t.Run("missing", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "does-not-exist")
		m := New(mount.NewFakeMounter(nil), testLogDir)

		mounted, err := m.IsMountPoint(target)
		if mounted {
			t.Errorf("IsMountPoint(%q) = true, want false", target)
		}
		if !errors.Is(err, fs.ErrNotExist) && !os.IsNotExist(err) {
			t.Fatalf("IsMountPoint(%q) = %v, want an error that satisfies fs.ErrNotExist", target, err)
		}
	})
}

func TestIsCorrupted(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "other error", err: errors.New("boom"), want: false},
		{name: "not exist", err: &os.PathError{Op: "stat", Path: "/x", Err: fs.ErrNotExist}, want: false},
		{name: "ENOTCONN", err: syscall.ENOTCONN, want: true},
		{name: "ESTALE", err: syscall.ESTALE, want: true},
		{name: "EIO", err: syscall.EIO, want: true},
		{name: "EHOSTDOWN", err: syscall.EHOSTDOWN, want: true},
		{name: "EACCES", err: syscall.EACCES, want: true},
		{name: "EWOULDBLOCK", err: syscall.EWOULDBLOCK, want: true},
		{name: "os.PathError ENOTCONN", err: &os.PathError{Op: "stat", Path: "/x", Err: syscall.ENOTCONN}, want: true},
		{name: "wrapped ENOTCONN", err: fmt.Errorf("read dir: %w", syscall.ENOTCONN), want: true},
		{name: "double wrapped ESTALE", err: fmt.Errorf("a: %w", fmt.Errorf("b: %w", syscall.ESTALE)), want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCorrupted(tc.err); got != tc.want {
				t.Fatalf("isCorrupted(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}

func TestVolumePath(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "server and volume", in: "g1:/vol", want: "/vol"},
		{name: "server and subdir", in: "g1:/vol/pvc-1", want: "/vol/pvc-1"},
		{name: "ipv4 server", in: "10.0.0.1:/vol", want: "/vol"},
		{name: "no colon", in: "/dev/sda", want: "/dev/sda"},
		{name: "empty", in: "", want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := volumePath(tc.in); got != tc.want {
				t.Fatalf("volumePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// assertOptions compares two option slices element by element, because the
// tests assert the complete slice and not just its contents.
func assertOptions(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("options = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("options = %q, want %q", got, want)
		}
	}
}

// pathExists reports whether path exists.
func pathExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return true
	case errors.Is(err, fs.ErrNotExist):
		return false
	default:
		t.Fatalf("Stat(%q): %v", path, err)
		return false
	}
}

// failingMountMounter is a FakeMounter whose Mount always fails, to exercise the
// error path around the real mount(8) call.
type failingMountMounter struct {
	*mount.FakeMounter
	err error
}

func (f *failingMountMounter) Mount(string, string, string, []string) error { return f.err }

// failingListMounter is a FakeMounter whose List always fails.
type failingListMounter struct {
	*mount.FakeMounter
	err error
}

func (f *failingListMounter) List() ([]mount.MountPoint, error) { return nil, f.err }

// lyingListMounter is a lyingMounter whose List fails.
type lyingListMounter struct {
	*mount.FakeMounter
	err error
}

func (l *lyingListMounter) IsMountPoint(file string) (bool, error) {
	if _, err := os.Stat(file); err != nil {
		return false, err
	}
	return true, nil
}

func (l *lyingListMounter) List() ([]mount.MountPoint, error) { return nil, l.err }

// lyingMounter reports every existing directory as a mount point without
// listing it, which is what a mount point that is missing from /proc/mounts
// would look like.
type lyingMounter struct {
	*mount.FakeMounter
}

func (l *lyingMounter) IsMountPoint(file string) (bool, error) {
	if _, err := os.Stat(file); err != nil {
		return false, err
	}
	return true, nil
}

// refsErrMounter is a FakeMounter whose GetMountRefs fails, which the real
// FakeMounter never does.
type refsErrMounter struct {
	*mount.FakeMounter
	err error
}

func (r refsErrMounter) GetMountRefs(string) ([]string, error) { return nil, r.err }

func TestOtherBindMounts(t *testing.T) {
	spec := Spec{Servers: []string{"g1"}, Volume: "vol"}

	tests := []struct {
		name string
		// bindTargets is how many targets bind mount the staging path.
		bindTargets int
		// exceptIndex is the target passed as except, -1 for one that is not
		// mounted at all.
		exceptIndex int
		// otherVolume mounts a second glusterfs volume that must not count.
		otherVolume bool
		wantOthers  int
	}{
		{name: "staged, not published", bindTargets: 0, exceptIndex: -1, wantOthers: 0},
		{name: "published only at except", bindTargets: 1, exceptIndex: 0, wantOthers: 0},
		{name: "published at another target", bindTargets: 1, exceptIndex: -1, wantOthers: 1},
		{name: "published at except and another target", bindTargets: 2, exceptIndex: 1, wantOthers: 1},
		{name: "another volume does not count", bindTargets: 0, exceptIndex: -1, otherVolume: true, wantOthers: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			fake := mount.NewFakeMounter(nil)
			m := New(fake, testLogDir)

			// The resolved path: FakeMounter turns a bind mount source into the
			// device of the mount only when it equals the recorded (resolved)
			// mount point, so an unresolved source would hide the reference on
			// macOS. The Linux mounter matches by major:minor instead.
			_, staging := newTestTarget(t)
			if err := m.MountGlusterfs(ctx, spec, staging); err != nil {
				t.Fatalf("MountGlusterfs(%q) = %v, want nil", staging, err)
			}
			if tc.otherVolume {
				otherStaging, _ := newTestTarget(t)
				other := Spec{Servers: []string{"g1"}, Volume: "other"}
				if err := m.MountGlusterfs(ctx, other, otherStaging); err != nil {
					t.Fatalf("MountGlusterfs(%q) = %v, want nil", otherStaging, err)
				}
			}
			var targets, resolvedTargets []string
			for range tc.bindTargets {
				target, resolved := newTestTarget(t)
				if err := m.BindMount(ctx, staging, target, false); err != nil {
					t.Fatalf("BindMount(%q, %q) = %v, want nil", staging, target, err)
				}
				targets = append(targets, target)
				resolvedTargets = append(resolvedTargets, resolved)
			}
			except := filepath.Join(t.TempDir(), "unmounted")
			if tc.exceptIndex >= 0 {
				// The unresolved path, as the CO passes it: OtherBindMounts has to
				// match it against the resolved path of the mount table.
				except = targets[tc.exceptIndex]
			}

			others, err := m.OtherBindMounts(staging, except)
			if err != nil {
				t.Fatalf("OtherBindMounts(%q, %q) = %v, want nil", staging, except, err)
			}
			if len(others) != tc.wantOthers {
				t.Fatalf("OtherBindMounts(%q, %q) = %q, want %d entries", staging, except, others, tc.wantOthers)
			}
			for _, o := range others {
				if tc.exceptIndex >= 0 && o == resolvedTargets[tc.exceptIndex] {
					t.Errorf("OtherBindMounts returned except %q", o)
				}
				if !slicesContains(resolvedTargets, o) {
					t.Errorf("OtherBindMounts returned %q, which is not a bind target of %q", o, staging)
				}
			}
		})
	}

	t.Run("invalid paths", func(t *testing.T) {
		m := New(mount.NewFakeMounter(nil), testLogDir)
		for _, args := range [][2]string{{"relative", "/abs"}, {"/abs", "/abs/../x"}} {
			if _, err := m.OtherBindMounts(args[0], args[1]); !errors.Is(err, ErrInvalidSpec) {
				t.Errorf("OtherBindMounts(%q, %q) = %v, want ErrInvalidSpec", args[0], args[1], err)
			}
		}
	})

	t.Run("mount table error", func(t *testing.T) {
		cause := errors.New("mountinfo unreadable")
		m := New(refsErrMounter{FakeMounter: mount.NewFakeMounter(nil), err: cause}, testLogDir)
		_, err := m.OtherBindMounts("/staging", "/target")
		if !errors.Is(err, cause) {
			t.Fatalf("OtherBindMounts() = %v, want an error wrapping %v", err, cause)
		}
		if !strings.Contains(err.Error(), "/staging") {
			t.Errorf("error %q does not mention the source", err)
		}
	})
}

func slicesContains(s []string, v string) bool {
	for _, e := range s {
		if e == v {
			return true
		}
	}
	return false
}
