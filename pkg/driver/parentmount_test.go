package driver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitlayzer/csi-driver-glusterfs/pkg/config"
	"github.com/gitlayzer/csi-driver-glusterfs/pkg/mounter"
	"github.com/gitlayzer/csi-driver-glusterfs/pkg/volumeid"
)

// testCluster is the cluster the parent mount tests mount parents of.
var testCluster = clusterOf("c1", "g1", "g2", "g3")

// clusterOf builds a config.Cluster with the given clusterID and servers.
func clusterOf(clusterID string, servers ...string) config.Cluster {
	return config.Cluster{ClusterID: clusterID, Servers: servers}
}

// newTestParentMounts returns a cache with a fake mounter and a temporary mount
// directory, together with the fake.
//
// The fake reports MountStateMounted, so the mount point of a parent volume is a
// plain directory of the temporary directory and the fake never changes it.
func newTestParentMounts(t *testing.T) (*parentMounts, *fakeMounter) {
	t.Helper()

	fake := &fakeMounter{mountState: mounter.MountStateMounted}
	return newParentMounts(fake, t.TempDir()), fake
}

func TestParentMountsRoot(t *testing.T) {
	tests := []struct {
		name string
		// state is the mount state the fake reports, stateErr its error.
		state    mounter.MountState
		stateErr error
		// mounted is true when the path has to be mounted by the call.
		mounted bool
		// wantCode is the error the call must produce, checked by the caller.
		wantErr   bool
		wantMount int
		wantState int
		// prepare caches a root of the path beforehand, standing in for an earlier
		// successful call.
		prepare bool
	}{
		{
			name:      "not mounted is mounted once",
			state:     mounter.MountStateNotMounted,
			mounted:   true,
			wantMount: 1,
			wantState: 1,
		},
		{
			name:    "mounted with a cached root is reused",
			state:   mounter.MountStateMounted,
			prepare: true,
			// The state is checked on every call, so a cached root still costs one
			// VolumeMountState call and no mount.
			wantMount: 0,
			wantState: 1,
		},
		{
			name:      "mounted without a cached root is opened",
			state:     mounter.MountStateMounted,
			wantMount: 0,
			wantState: 1,
		},
		{
			name:      "corrupted is detached and mounted again",
			state:     mounter.MountStateCorrupted,
			prepare:   true,
			wantMount: 1,
			wantState: 1,
		},
		{
			name:      "mismatch is an error and changes nothing",
			state:     mounter.MountStateMismatch,
			prepare:   true,
			wantMount: 0,
			wantState: 1,
			wantErr:   true,
		},
		{
			name:      "a mount state error is an error",
			state:     mounter.MountStateMounted,
			stateErr:  errors.New("mount table unreadable"),
			wantMount: 0,
			wantState: 1,
			wantErr:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parents, fake := newTestParentMounts(t)
			fake.mountState = tc.state
			fake.mountStateErr = tc.stateErr
			path := parents.mountPath("c1", "vol")
			if err := os.MkdirAll(path, 0o750); err != nil {
				t.Fatalf("MkdirAll(%q) = %v, want nil", path, err)
			}

			var old *os.Root
			if tc.prepare {
				// A cached root of an earlier call. Its presence is what makes the
				// difference between reusing and opening a root observable.
				var openErr error
				old, openErr = os.OpenRoot(path)
				if openErr != nil {
					t.Fatalf("OpenRoot(%q) = %v, want nil", path, openErr)
				}
				parents.roots[path] = old
			}

			root, err := parents.root(context.Background(), clusterOf("c1", "g1", "g2", "g3"), "vol")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("root() = %v, nil, want an error", root)
				}
				if root != nil {
					t.Fatalf("root() = %v with an error, want nil", root)
				}
				// A failure of the mount state (or a mount that failed) must never
				// leave a root cached ...
				if _, ok := parents.roots[path]; ok != tc.prepare {
					t.Fatalf("roots[%q] = %t, want %t", path, ok, tc.prepare)
				}
				// ... and nothing may be unmounted or mounted for a mismatch.
				if tc.state == mounter.MountStateMismatch && len(fake.unmountCalls)+len(fake.detachCalls) != 0 {
					t.Fatalf("mounter calls = %v/%v, want none for a mismatch",
						fake.detachCalls, fake.unmountCalls)
				}
				return
			}
			if err != nil {
				t.Fatalf("root() = %v, want nil", err)
			}
			if got := fake.mountCount(); got != tc.wantMount {
				t.Fatalf("MountGlusterfs calls = %d, want %d", got, tc.wantMount)
			}
			if got := fake.mountStateCount(); got != tc.wantState {
				t.Fatalf("VolumeMountState calls = %d, want %d", got, tc.wantState)
			}
			if tc.wantMount == 1 {
				call := fake.mountCalls[0]
				if call.target != path {
					t.Fatalf("mount target = %q, want %q", call.target, path)
				}
				if call.spec.Volume != "vol" {
					t.Fatalf("mount volume = %q, want %q", call.spec.Volume, "vol")
				}
				if len(call.spec.Servers) != 3 || call.spec.Servers[0] != "g1" {
					t.Fatalf("mount servers = %v, want the servers of the cluster", call.spec.Servers)
				}
			}
			if tc.state == mounter.MountStateCorrupted {
				if fake.detachCount() != 1 {
					t.Fatalf("Detach calls = %d, want 1", fake.detachCount())
				}
				if _, statErr := old.Stat("."); !errors.Is(statErr, os.ErrClosed) {
					t.Fatalf("the replaced root is still usable (%v), want os.ErrClosed", statErr)
				}
			}
			if tc.state == mounter.MountStateMounted && tc.prepare {
				if root != old {
					t.Fatal("root() = a new root, want the cached one")
				}
			}
			if got, ok := parents.roots[path]; !ok || got != root {
				t.Fatalf("roots[%q] = %v (%t), want the returned root", path, got, ok)
			}
			if name := root.Name(); name != path {
				t.Fatalf("root.Name() = %q, want %q", name, path)
			}
		})
	}
}

func TestParentMountsRootMountFailureIsNotCached(t *testing.T) {
	parents, fake := newTestParentMounts(t)
	fake.mountState = mounter.MountStateNotMounted
	fake.mountErrs = []error{errors.New("mount failed")}

	if _, err := parents.root(context.Background(), testCluster, "vol"); err == nil {
		t.Fatal("root() = nil, want the mount error")
	}
	if len(parents.roots) != 0 {
		t.Fatalf("roots = %v, want none after a failed mount", parents.roots)
	}
	if _, tracked := parents.mounted[parents.mountPath("c1", "vol")]; tracked {
		t.Fatal("a failed mount is recorded as mounted")
	}

	// The next call retries the mount instead of returning a broken root.
	if _, err := parents.root(context.Background(), testCluster, "vol"); err != nil {
		t.Fatalf("root() = %v, want nil after the retry", err)
	}
	if got := fake.mountCount(); got != 2 {
		t.Fatalf("MountGlusterfs calls = %d, want 2", got)
	}
}

func TestParentMountsRootRejectsInvalidNames(t *testing.T) {
	tests := []struct {
		name      string
		clusterID string
		volume    string
	}{
		{name: "relative cluster", clusterID: "..", volume: "vol"},
		{name: "separator in cluster", clusterID: "a/b", volume: "vol"},
		{name: "relative volume", clusterID: "c1", volume: ".."},
		{name: "separator in volume", clusterID: "c1", volume: "a/b"},
		{name: "empty cluster", clusterID: "", volume: "vol"},
		{name: "empty volume", clusterID: "c1", volume: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parents, fake := newTestParentMounts(t)

			_, err := parents.root(context.Background(), clusterOf(tc.clusterID), tc.volume)
			if err == nil {
				t.Fatal("root() = nil, want a validation error")
			}
			if !errors.Is(err, volumeid.ErrInvalid) {
				t.Fatalf("root() = %v, want an error wrapping volumeid.ErrInvalid", err)
			}
			if got := fake.mountStateCount() + fake.mountCount(); got != 0 {
				t.Fatalf("mounter calls = %d, want none for an invalid name", got)
			}
		})
	}
}

func TestParentMountsRootRespectsCancellation(t *testing.T) {
	parents, fake := newTestParentMounts(t)

	// Hold the lock so that the call has to wait for it.
	parents.sem <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := parents.root(ctx, testCluster, "vol"); !errors.Is(err, context.Canceled) {
		t.Fatalf("root() = %v, want context.Canceled", err)
	}
	if got := fake.mountStateCount(); got != 0 {
		t.Fatalf("VolumeMountState calls = %d, want none while the lock is held", got)
	}

	// Releasing the lock lets the next call through.
	<-parents.sem
	if err := os.MkdirAll(parents.mountPath("c1", "vol"), 0o750); err != nil {
		t.Fatalf("MkdirAll() = %v, want nil", err)
	}
	if _, err := parents.root(context.Background(), testCluster, "vol"); err != nil {
		t.Fatalf("root() = %v, want nil", err)
	}
}

func TestParentMountsPathsPerClusterAndVolume(t *testing.T) {
	parents, _ := newTestParentMounts(t)

	// D-24 puts the mount point at <controller mount dir>/<clusterID>/<volume>, so
	// two clusters or two parent volumes never share a mount.
	tests := []struct {
		clusterID, volume string
		want              string
	}{
		{clusterID: "c1", volume: "vol", want: filepath.Join(parents.dir, "c1", "vol")},
		{clusterID: "c1", volume: "other", want: filepath.Join(parents.dir, "c1", "other")},
		{clusterID: "c2", volume: "vol", want: filepath.Join(parents.dir, "c2", "vol")},
	}

	seen := make(map[string]bool, len(tests))
	for _, tc := range tests {
		got := parents.mountPath(tc.clusterID, tc.volume)
		if got != tc.want {
			t.Fatalf("mountPath(%q, %q) = %q, want %q", tc.clusterID, tc.volume, got, tc.want)
		}
		if seen[got] {
			t.Fatalf("mountPath(%q, %q) = %q, which another cluster or volume uses too",
				tc.clusterID, tc.volume, got)
		}
		seen[got] = true
	}
}

func TestParentMountsClose(t *testing.T) {
	parents, fake := newTestParentMounts(t)
	volPath := parents.mountPath("c1", "vol")
	otherPath := parents.mountPath("c1", "other")
	fake.mountState = mounter.MountStateNotMounted

	// A successful mount creates its mount point, so the fake has to stand in for
	// that part as well.
	fake.onCreate = func(target string) error { return os.MkdirAll(target, 0o750) }

	first, err := parents.root(context.Background(), testCluster, "vol")
	if err != nil {
		t.Fatalf("root() = %v, want nil", err)
	}
	fake.mountState = mounter.MountStateMounted
	// The second volume is already mounted by somebody else, so only its mount point
	// has to exist; close unmounts it because this process mounted the first one.
	if err := os.MkdirAll(otherPath, 0o750); err != nil {
		t.Fatalf("MkdirAll(%q) = %v, want nil", otherPath, err)
	}
	if _, err := parents.root(context.Background(), testCluster, "other"); err != nil {
		t.Fatalf("root() = %v, want nil", err)
	}

	parents.close(context.Background())

	// Only the path this process mounted is unmounted: a mount that was already
	// there belongs to whoever made it.
	if got := fake.unmountCount(); got != 1 {
		t.Fatalf("Unmount calls = %d (%v), want 1", got, fake.unmountCalls)
	}
	if fake.unmountCalls[0] != volPath {
		t.Fatalf("Unmount target = %q, want %q", fake.unmountCalls[0], volPath)
	}
	if len(parents.roots) != 0 || len(parents.mounted) != 0 {
		t.Fatalf("roots = %v, mounted = %v, want both empty", parents.roots, parents.mounted)
	}
	if _, statErr := first.Stat("."); !errors.Is(statErr, os.ErrClosed) {
		t.Fatalf("the root is still usable after close (%v), want os.ErrClosed", statErr)
	}

	// A second close has nothing left to do.
	parents.close(context.Background())
	if got := fake.unmountCount(); got != 1 {
		t.Fatalf("Unmount calls after the second close = %d, want 1", got)
	}
}

func TestParentMountsCloseRespectsCancellation(t *testing.T) {
	parents, fake := newTestParentMounts(t)
	parents.sem <- struct{}{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	parents.close(ctx)

	if got := fake.unmountCount(); got != 0 {
		t.Fatalf("Unmount calls = %d, want none while the lock is held", got)
	}
	<-parents.sem
}

func TestParentMountsCloseLogsUnmountErrors(t *testing.T) {
	parents, fake := newTestParentMounts(t)
	fake.mountState = mounter.MountStateNotMounted
	fake.onCreate = func(target string) error { return os.MkdirAll(target, 0o750) }
	if _, err := parents.root(context.Background(), testCluster, "vol"); err != nil {
		t.Fatalf("root() = %v, want nil", err)
	}
	fake.unmountErrs = []error{errors.New("device is busy")}

	// The error is only logged: the process is shutting down.
	parents.close(context.Background())
	if got := fake.unmountCount(); got != 1 {
		t.Fatalf("Unmount calls = %d, want 1", got)
	}
	// The path is forgotten even though the unmount failed: close runs once, at
	// shutdown, and there is nothing left that could retry it.
	if _, tracked := parents.mounted[parents.mountPath("c1", "vol")]; tracked {
		t.Fatal("a failed unmount keeps the path in mounted, want it forgotten")
	}
}

// TestParentMountsRootDetachFailure checks that a corrupted mount whose detach fails
// is reported instead of being mounted over.
func TestParentMountsRootDetachFailure(t *testing.T) {
	parents, fake := newTestParentMounts(t)
	fake.mountState = mounter.MountStateCorrupted
	fake.detachErrs = []error{errors.New("device is busy")}
	if err := os.MkdirAll(parents.mountPath("c1", "vol"), 0o750); err != nil {
		t.Fatalf("MkdirAll() = %v, want nil", err)
	}

	_, err := parents.root(context.Background(), testCluster, "vol")
	if err == nil {
		t.Fatal("root() = nil, want the detach error")
	}
	if !strings.Contains(err.Error(), "detach the corrupted mount") {
		t.Fatalf("root() = %q, want it to name the detach", err)
	}
	if got := fake.mountCount(); got != 0 {
		t.Fatalf("MountGlusterfs calls = %d, want none after a failed detach", got)
	}
	if len(parents.roots) != 0 {
		t.Fatalf("roots = %v, want none", parents.roots)
	}
}

// TestParentMountsRootMountPointErrors checks the two failures of the mount point
// itself: it cannot be created, and a mount that reported success left nothing to
// open.
func TestParentMountsRootMountPointErrors(t *testing.T) {
	t.Run("the mount point cannot be created", func(t *testing.T) {
		parents, fake := newTestParentMounts(t)
		fake.mountState = mounter.MountStateNotMounted
		// A regular file where the directory of the cluster belongs.
		writeTestFile(t, parents.dir, "c1", "not a directory")

		_, err := parents.root(context.Background(), testCluster, "vol")
		if err == nil {
			t.Fatal("root() = nil, want an error")
		}
		if !strings.Contains(err.Error(), "create parent volume mount point") {
			t.Fatalf("root() = %q, want it to name the mount point", err)
		}
		if got := fake.mountCount(); got != 0 {
			t.Fatalf("MountGlusterfs calls = %d, want none", got)
		}
	})

	t.Run("a successful mount that leaves no mount point", func(t *testing.T) {
		parents, fake := newTestParentMounts(t)
		fake.mountState = mounter.MountStateNotMounted
		// The fake does not create the target, so the root cannot be opened. A real
		// mount always leaves the target behind, so this only checks that the
		// failure is reported and nothing is cached.
		parents.mounter = &targetRemovingMounter{Mounter: fake}

		_, err := parents.root(context.Background(), testCluster, "vol")
		if err == nil {
			t.Fatal("root() = nil, want an error for a mount point that does not exist")
		}
		if !strings.Contains(err.Error(), "open parent volume mount point") {
			t.Fatalf("root() = %q, want it to name the root", err)
		}
		if len(parents.roots) != 0 {
			t.Fatalf("roots = %v, want none", parents.roots)
		}
		if _, tracked := parents.mounted[parents.mountPath("c1", "vol")]; !tracked {
			t.Fatal("the successful mount is not recorded, so close would not unmount it")
		}
	})
}

// targetRemovingMounter is a [mounter.Mounter] that mounts successfully and then
// removes the mount point again, which no real mount does: it exists to check that a
// root that cannot be opened is reported and not cached.
type targetRemovingMounter struct {
	mounter.Mounter
}

// MountGlusterfs removes the target after the embedded mounter reported success.
func (m *targetRemovingMounter) MountGlusterfs(ctx context.Context, spec mounter.Spec, target string) error {
	if err := m.Mounter.MountGlusterfs(ctx, spec, target); err != nil {
		return err
	}
	return os.Remove(target)
}

// TestParentMountsRootUnexpectedState checks the default branch of the mount state
// switch: a state this package does not define must not be treated as "mount it".
func TestParentMountsRootUnexpectedState(t *testing.T) {
	parents, fake := newTestParentMounts(t)
	fake.mountState = mounter.MountState(42)

	_, err := parents.root(context.Background(), testCluster, "vol")
	if err == nil {
		t.Fatal("root() = nil, want an error for an unknown mount state")
	}
	if !strings.Contains(err.Error(), "unexpected mount state") {
		t.Fatalf("root() = %q, want it to name the state", err)
	}
	if got := fake.mountCount(); got != 0 {
		t.Fatalf("MountGlusterfs calls = %d, want none", got)
	}
}
