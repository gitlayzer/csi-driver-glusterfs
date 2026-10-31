package mounter

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"k8s.io/klog/v2"
	mount "k8s.io/mount-utils"
)

var (
	// ErrMountConflict means the target is already mounted with something else:
	// a different volume or subdirectory, a different file system type or the
	// opposite read-only setting. The Node service maps it to
	// codes.AlreadyExists, because the target is not free for this request.
	ErrMountConflict = errors.New("target is already mounted with a different source or flags")

	// ErrCorruptedMount means the target is a mount point whose file system
	// stopped responding, typically because the FUSE process exited (ENOTCONN).
	// It is reported instead of a plain error so that the caller can decide to
	// unmount and mount again; the startup recovery of T-08 does exactly that.
	ErrCorruptedMount = errors.New("target is a corrupted mount point")
)

// corruptedErrs are the errors that make a mount point unusable. mount-utils
// has the same list in IsCorruptedMnt, but that function only looks at the
// outermost error type, so a wrapped error ("fmt.Errorf("%w", ...)") would slip
// through. Checked with errors.Is below, they are recognised at any depth.
var corruptedErrs = []error{
	syscall.ENOTCONN,
	syscall.ESTALE,
	syscall.EIO,
	syscall.EHOSTDOWN,
	syscall.EACCES,
	syscall.EWOULDBLOCK,
}

// glusterfsTypes are the two spellings of a glusterfs mount: mount(8) is called
// with "glusterfs", the kernel reports "fuse.glusterfs" in /proc/mounts.
var glusterfsTypes = []string{FSType, "fuse.glusterfs"}

// Mounter mounts and unmounts GlusterFS volumes. Every method is idempotent:
// repeating a successful call succeeds with the same result, which is what the
// CSI idempotency requirements demand.
type Mounter interface {
	// MountGlusterfs mounts spec at target with a glusterfs FUSE mount. It is
	// used by NodeStageVolume, once per volume and node.
	MountGlusterfs(ctx context.Context, spec Spec, target string) error
	// BindMount bind-mounts source at target, read only if readOnly is set. It
	// is used by NodePublishVolume, once per Pod.
	BindMount(ctx context.Context, source, target string, readOnly bool) error
	// Unmount unmounts target, including a corrupted mount point, and keeps the
	// directory. It is used by NodeUnstageVolume.
	Unmount(ctx context.Context, target string) error
	// Cleanup unmounts target and then removes the directory, because the CSI
	// spec expects the plugin to delete the target_path it created. It is used
	// by NodeUnpublishVolume.
	Cleanup(ctx context.Context, target string) error
	// IsMountPoint reports whether target is a mount point. A corrupted mount
	// point returns (false, error wrapping ErrCorruptedMount), which
	// NodeGetVolumeStats (T-07) reports as VOLUME_CONDITION.
	IsMountPoint(target string) (bool, error)
}

// mounter is the container local implementation of [Mounter]. The FUSE process
// it starts lives as long as the node plugin Pod; M5 adds a host implementation
// of the same interface (docs/architecture.md §5.3).
type mounter struct {
	// inner performs the actual mount and unmount operations. Production uses
	// mount.New(""), tests use mount.NewFakeMounter.
	inner mount.Interface
	// logDir is the directory of the per-mount log file, empty for none.
	logDir string
}

// The compiler has to reject any drift between the interface and the
// implementation before a caller notices it.
var _ Mounter = (*mounter)(nil)

// New returns a [Mounter] backed by m and logging the glusterfs mounts into
// logDir. Production passes mount.New(""), tests pass mount.NewFakeMounter.
// New performs no I/O; logDir is handed to [BuildMountArgs] unchanged.
func New(m mount.Interface, logDir string) Mounter {
	return &mounter{inner: m, logDir: logDir}
}

// MountGlusterfs mounts spec at target with a glusterfs FUSE mount.
//
// An existing mount is accepted when it already is the same mount: glusterfs
// type, the same ":/volume[/subdir]" part and the same read-only flag. The
// volfile server may differ, because failover can connect to any server of the
// spec. Anything else at target is an [ErrMountConflict]; a corrupted mount is
// reported as [ErrCorruptedMount] and never unmounted here, because deciding
// whether the data is still usable belongs to the caller.
func (m *mounter) MountGlusterfs(ctx context.Context, spec Spec, target string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validatePath("target", target); err != nil {
		return err
	}
	source, options, err := BuildMountArgs(spec, m.logDir)
	if err != nil {
		return err
	}

	mounted, err := m.IsMountPoint(target)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Creating the staging directory is the job of the Node service, so a
		// missing target is an error instead of an implicit mkdir.
		return fmt.Errorf("target %q does not exist: %w", target, err)
	case err != nil:
		return err
	case mounted:
		return m.checkExistingGlusterfsMount(target, source, options)
	}

	if err := m.inner.Mount(source, target, FSType, options); err != nil {
		return fmt.Errorf("mount glusterfs %s at %s: %w", source, target, err)
	}
	klog.V(2).InfoS("mounted glusterfs volume", "source", source, "target", target, "options", options)
	return nil
}

// checkExistingGlusterfsMount decides whether the mount that is already at
// target is the mount that was requested.
func (m *mounter) checkExistingGlusterfsMount(target, source string, options []string) error {
	entry, found, err := m.findMountEntry(target)
	if err != nil {
		return err
	}
	if !found {
		// IsMountPoint said yes but List does not know the target: the mount is
		// there but not describable, so it cannot be assumed to be ours.
		return fmt.Errorf("%w: target %q is a mount point that is not listed in the mount table", ErrMountConflict, target)
	}

	wantRO := slices.Contains(options, "ro")
	haveRO := slices.Contains(entry.Opts, "ro")
	if !matchesGlusterfs(entry, source, wantRO) {
		return fmt.Errorf("%w: target %q is already mounted from %q (type %s, ro=%t), requested %q (ro=%t)",
			ErrMountConflict, target, entry.Device, entry.Type, haveRO, source, wantRO)
	}
	klog.V(4).InfoS("glusterfs volume is already mounted, skipping", "source", source, "target", target)
	return nil
}

// matchesGlusterfs reports whether entry describes the requested glusterfs
// mount: same file system type, same volume and subdirectory and the same
// read-only flag. The server part of the device is not compared, because the
// mount may have been established through any of the backup volfile servers.
func matchesGlusterfs(entry mount.MountPoint, source string, wantRO bool) bool {
	if !slices.Contains(glusterfsTypes, entry.Type) {
		return false
	}
	if volumePath(entry.Device) != volumePath(source) {
		return false
	}
	return slices.Contains(entry.Opts, "ro") == wantRO
}

// BindMount bind-mounts source at target, read only if readOnly is set.
//
// A bind mount shows the original device in /proc/mounts, so the device is not
// compared; only the read-only flag has to match. A bind mount at target with
// the other read-only setting is an [ErrMountConflict]: mount(8) cannot change
// it in place.
func (m *mounter) BindMount(ctx context.Context, source, target string, readOnly bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validatePath("source", source); err != nil {
		return err
	}
	if err := validatePath("target", target); err != nil {
		return err
	}

	mounted, err := m.IsMountPoint(target)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("target %q does not exist: %w", target, err)
	case err != nil:
		return err
	case mounted:
		return m.checkExistingBindMount(target, readOnly)
	}

	options := []string{"bind"}
	if readOnly {
		// mount-utils turns "bind,ro" into bind + remount,ro on Linux, because
		// a bind mount does not honour the read-only flag on its own.
		options = append(options, "ro")
	}
	if err := m.inner.Mount(source, target, "", options); err != nil {
		return fmt.Errorf("bind mount %q at %q: %w", source, target, err)
	}
	klog.V(2).InfoS("bind mounted volume", "source", source, "target", target, "options", options)
	return nil
}

// checkExistingBindMount decides whether the mount that is already at target
// has the requested read-only setting.
func (m *mounter) checkExistingBindMount(target string, readOnly bool) error {
	entry, found, err := m.findMountEntry(target)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: target %q is a mount point that is not listed in the mount table", ErrMountConflict, target)
	}
	if haveRO := slices.Contains(entry.Opts, "ro"); haveRO != readOnly {
		return fmt.Errorf("%w: target %q is already bind mounted with ro=%t, requested ro=%t",
			ErrMountConflict, target, haveRO, readOnly)
	}
	klog.V(4).InfoS("target is already bind mounted, skipping", "target", target)
	return nil
}

// Unmount unmounts target if it is mounted and keeps the directory.
//
// A missing target is success, so that NodeUnstageVolume can be repeated. A
// corrupted mount point is unmounted instead of being reported as an error,
// because that is the only way to get rid of a mount whose FUSE process is
// gone. The directory itself is kept: it belongs to kubelet, not to the driver.
func (m *mounter) Unmount(ctx context.Context, target string) error {
	return m.unmount(ctx, target, false)
}

// Cleanup unmounts target if it is mounted and removes the directory, because
// the CSI spec requires NodeUnpublishVolume to delete the target_path that the
// plugin created.
func (m *mounter) Cleanup(ctx context.Context, target string) error {
	return m.unmount(ctx, target, true)
}

// unmount is the shared implementation of [Mounter.Unmount] and
// [Mounter.Cleanup]. mount-utils has CleanupMountPoint, but it unconditionally
// removes the directory, and the staging directory of an unstaged volume is
// owned by kubelet, so the removal is done here, conditionally.
func (m *mounter) unmount(ctx context.Context, target string, removeDir bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validatePath("target", target); err != nil {
		return err
	}

	mounted, err := m.IsMountPoint(target)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// The path is gone, and with it the mount: nothing left to do.
		klog.V(4).InfoS("target does not exist, nothing to unmount", "target", target)
	case err != nil && errors.Is(err, ErrCorruptedMount):
		// The recovery path: a broken FUSE mount point can only be removed by
		// unmounting it, and that works even when it cannot be stat'ed.
		if uerr := m.inner.Unmount(target); uerr != nil {
			return fmt.Errorf("unmount corrupted mount %q: %w", target, uerr)
		}
		klog.V(2).InfoS("unmounted corrupted mount", "target", target)
	case err != nil:
		return err
	case mounted:
		if uerr := m.inner.Unmount(target); uerr != nil {
			return fmt.Errorf("unmount %q: %w", target, uerr)
		}
		klog.V(2).InfoS("unmounted volume", "target", target)
	default:
		klog.V(4).InfoS("target is not a mount point, skipping unmount", "target", target)
	}

	if !removeDir {
		return nil
	}
	// os.Remove, not os.RemoveAll: a non-empty directory means the unmount did
	// not happen or somebody still writes into it, and deleting the data would
	// hide that. A directory that is already gone is the expected result.
	if rerr := os.Remove(target); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
		return fmt.Errorf("remove target directory %q: %w", target, rerr)
	}
	return nil
}

// IsMountPoint reports whether target is a mount point. A corrupted mount point
// returns false plus an error wrapping [ErrCorruptedMount].
func (m *mounter) IsMountPoint(target string) (bool, error) {
	mounted, err := m.inner.IsMountPoint(target)
	switch {
	case err == nil:
		return mounted, nil
	case isCorrupted(err):
		return false, fmt.Errorf("%w: %q: %w", ErrCorruptedMount, target, err)
	default:
		// Wrapped, not replaced: callers use errors.Is(err, fs.ErrNotExist) to
		// tell a missing path from a real failure.
		return false, fmt.Errorf("check whether %q is a mount point: %w", target, err)
	}
}

// findMountEntry returns the mount table entry of target, preferring the last
// match because the top-most mount of a stacked target comes last in
// /proc/mounts. The target is compared both as given and with symbolic links
// resolved: /proc/mounts lists the resolved path (on macOS /var is a symlink to
// /private/var, on Linux a kubelet directory may be a symlink as well).
func (m *mounter) findMountEntry(target string) (mount.MountPoint, bool, error) {
	entries, err := m.inner.List()
	if err != nil {
		return mount.MountPoint{}, false, fmt.Errorf("list mount points to find %q: %w", target, err)
	}
	resolved := target
	if r, rerr := filepath.EvalSymlinks(target); rerr == nil {
		resolved = r
	}

	var entry mount.MountPoint
	var found bool
	for _, mp := range entries {
		if mp.Path == target || mp.Path == resolved {
			entry, found = mp, true
		}
	}
	return entry, found, nil
}

// isCorrupted reports whether err says the mount point is unusable. Both
// mount.IsCorruptedMnt and an errors.Is check are needed: the former knows the
// platform specific list but only looks at the outermost error type, the latter
// sees through fmt.Errorf("%w") wrapping.
func isCorrupted(err error) bool {
	if mount.IsCorruptedMnt(err) {
		return true
	}
	for _, target := range corruptedErrs {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// validatePath validates a path that is passed to mount-utils: it must be
// absolute and already clean. A relative path would be resolved against the
// working directory of the plugin, and an unclean one would silently address a
// different directory than the one the caller recorded.
func validatePath(field, path string) error {
	switch {
	case path == "":
		return specErrf(field, "must not be empty")
	case !filepath.IsAbs(path):
		return specErrf(field, "%s must be an absolute path", quotedValue(path))
	case filepath.Clean(path) != path:
		return specErrf(field, "%s must be a clean path", quotedValue(path))
	}
	return nil
}

// volumePath returns the part of a mount source or device that identifies the
// volume: everything after the first ':', i.e. "/volume[/subdir]". The server in
// front of it is intentionally dropped, because a glusterfs mount may use any
// of the configured servers.
func volumePath(s string) string {
	if _, rest, ok := strings.Cut(s, ":"); ok {
		return rest
	}
	return s
}
