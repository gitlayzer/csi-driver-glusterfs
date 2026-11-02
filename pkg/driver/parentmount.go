package driver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/klog/v2"

	"github.com/gitlayzer/csi-driver-glusterfs/pkg/config"
	"github.com/gitlayzer/csi-driver-glusterfs/pkg/mounter"
	"github.com/gitlayzer/csi-driver-glusterfs/pkg/volumeid"
)

// parentMountDirMode is the permission of the per parent volume mount points the
// controller creates below --controller-mount-dir. They are only traversed by the
// plugin itself, so no other user needs access.
const parentMountDirMode = 0o750

// parentMounts keeps one glusterfs mount per parent volume for the controller
// service (D-24) and hands out an *os.Root opened on each mount.
//
// Mounting a parent volume once and keeping the mount is what makes provisioning
// cheap: the parent volume is mounted on the first RPC that needs it and after
// that every CreateVolume or DeleteVolume only does local file system calls
// instead of starting another FUSE process. All directory and metadata work goes
// through the *os.Root of the mount, never through a path string joined onto the
// mount point: an *os.Root cannot be made to leave the directory it was opened
// on, so a parent volume whose content is a symlink farm still cannot touch
// anything outside of it (D-24).
type parentMounts struct {
	// mounter performs the FUSE mounts. It is a field so that tests can replace
	// the production mounter with a fake one.
	mounter mounter.Mounter
	// dir is the root directory of the mount points, Options.ControllerMountDir.
	dir string
	// sem serializes the mount bookkeeping and gives up waiting when the RPC
	// context ends: a channel instead of a sync.Mutex because a sync.Mutex cannot
	// be abandoned on cancellation.
	sem chan struct{}
	// roots are the roots handed out so far, keyed by mount path. A nested map
	// would not help: the only thing that matters is the path on disk.
	roots map[string]*os.Root
	// mounted are the mount paths this process mounted, so that close unmounts
	// exactly those and not a mount that was already there when the plugin
	// started.
	mounted map[string]struct{}
}

// newParentMounts returns an empty cache that will mount below dir.
func newParentMounts(m mounter.Mounter, dir string) *parentMounts {
	return &parentMounts{
		mounter: m,
		dir:     dir,
		sem:     make(chan struct{}, 1),
		roots:   make(map[string]*os.Root),
		mounted: make(map[string]struct{}),
	}
}

// lock takes the cache lock, giving up when ctx ends.
//
// A cancelled context must not leave an RPC waiting behind a mount that takes
// long: the CO has already given up on the request by then.
func (p *parentMounts) lock(ctx context.Context) error {
	select {
	case p.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// unlock releases the cache lock.
func (p *parentMounts) unlock() {
	<-p.sem
}

// mountPath returns the mount point of a parent volume:
// <controller mount dir>/<clusterID>/<volume> (D-24).
func (p *parentMounts) mountPath(clusterID, volume string) string {
	return filepath.Join(p.dir, clusterID, volume)
}

// root returns an *os.Root opened on the mount of volume in cluster, mounting the
// parent volume first if this process has not done so yet.
//
// The mount state is checked on every call, so a mount that died with its FUSE
// process is repaired instead of being used: a corrupted mount is detached and
// mounted again. A path that is mounted with something else is reported as an
// error and left alone, because unmounting a mount this driver did not make could
// destroy data.
//
// The root is only opened after a successful mount, and a failed call never
// caches a root. A root that is replaced by a remount is closed, which makes every
// operation another goroutine still holds it for fail with os.ErrClosed instead of
// running against the local directory below the mount point: the operation fails,
// the CO retries, and the retry gets the fresh root. That is deliberate, the
// alternative would be to write into the container file system where the parent
// volume is supposed to be.
func (p *parentMounts) root(ctx context.Context, cluster config.Cluster, volume string) (*os.Root, error) {
	// Both names end up in a path, so they are validated even though they come from
	// a decoded volume id or from the configuration: a path built from unchecked
	// input is exactly the kind of bug this check exists to prevent.
	if err := volumeid.ValidateName("clusterID", cluster.ClusterID); err != nil {
		return nil, err
	}
	if err := volumeid.ValidateName("volume", volume); err != nil {
		return nil, err
	}

	if err := p.lock(ctx); err != nil {
		return nil, err
	}
	defer p.unlock()

	path := p.mountPath(cluster.ClusterID, volume)
	state, err := p.mounter.VolumeMountState(path, volume, "")
	if err != nil {
		return nil, fmt.Errorf("check the mount state of parent volume %q at %q: %w", volume, path, err)
	}

	switch state {
	case mounter.MountStateMounted:
		if r, ok := p.roots[path]; ok {
			return r, nil
		}
		// Mounted by an earlier instance of this plugin (or by an operator). The
		// mount is there, so opening a root on it is all that is missing.
		return p.openRoot(path)
	case mounter.MountStateMismatch:
		// Not unmounted and not mounted over: whatever is there is not ours.
		return nil, fmt.Errorf("something else is mounted at %q, refusing to use it as parent volume %q",
			path, volume)
	case mounter.MountStateCorrupted:
		p.dropRoot(path)
		if err := p.mounter.Detach(path); err != nil {
			return nil, fmt.Errorf("detach the corrupted mount of parent volume %q at %q: %w", volume, path, err)
		}
	case mounter.MountStateNotMounted:
		// A cached root of a path that is not mounted any more points at the local
		// directory below the mount point, not at the parent volume.
		p.dropRoot(path)
	default:
		return nil, fmt.Errorf("unexpected mount state %s for parent volume %q at %q",
			state, volume, path)
	}

	// #nosec G301 -- the mount point is traversed by the plugin only; the data of
	// the parent volume appears below it.
	if err := os.MkdirAll(path, parentMountDirMode); err != nil {
		return nil, fmt.Errorf("create parent volume mount point %q: %w", path, err)
	}
	// The mounter requires the target to exist, which is what the MkdirAll above
	// is for. No root is opened here: only a successful mount may be cached.
	if err := p.mounter.MountGlusterfs(ctx, mounter.Spec{Servers: cluster.Servers, Volume: volume}, path); err != nil {
		return nil, fmt.Errorf("mount parent volume %q at %q: %w", volume, path, err)
	}
	p.mounted[path] = struct{}{}

	return p.openRoot(path)
}

// openRoot opens and caches the root of a mount path. The caller holds the lock.
func (p *parentMounts) openRoot(path string) (*os.Root, error) {
	r, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("open parent volume mount point %q: %w", path, err)
	}
	p.roots[path] = r
	return r, nil
}

// dropRoot closes and forgets the root of a mount path, if there is one. The
// caller holds the lock.
//
// A close failure is only logged: the root is being invalidated, and the error
// says nothing about the volume.
func (p *parentMounts) dropRoot(path string) {
	r, ok := p.roots[path]
	if !ok {
		return
	}
	delete(p.roots, path)
	if err := r.Close(); err != nil {
		klog.ErrorS(err, "failed to close the root of a parent volume", "path", path)
	}
}

// close unmounts every parent volume this process mounted and closes the roots.
//
// It is the shutdown path of the controller service and therefore best effort:
// every failure is logged and none of them stops the remaining work. The mount
// point directories are kept, so a restarted plugin finds them again. Calling
// close twice is a no-op, because the second call has nothing left to close or
// unmount.
func (p *parentMounts) close(ctx context.Context) {
	if err := p.lock(ctx); err != nil {
		klog.ErrorS(err, "cannot lock the parent volume cache, leaving the parent volumes mounted")
		return
	}
	defer p.unlock()

	for path, r := range p.roots {
		if err := r.Close(); err != nil {
			klog.ErrorS(err, "failed to close the root of a parent volume", "path", path)
		}
		delete(p.roots, path)
	}

	for path := range p.mounted {
		if err := p.mounter.Unmount(ctx, path); err != nil {
			klog.ErrorS(err, "failed to unmount a parent volume", "path", path)
		}
		delete(p.mounted, path)
	}
}
