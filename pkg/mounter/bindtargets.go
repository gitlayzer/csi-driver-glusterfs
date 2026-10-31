package mounter

import (
	"fmt"
	"path/filepath"
	"slices"
)

// procMountInfoPath is the mount table of the current mount namespace, the file
// [Mounter.BindTargets] parses. A node plugin container shares the mount
// namespace of the host for the paths kubelet hands it, so this file lists the
// staging mount and every bind mount made from it.
const procMountInfoPath = "/proc/self/mountinfo"

// BindTargets implements [Mounter.BindTargets].
//
// mount-utils' GetMountRefs (and therefore [Mounter.OtherBindMounts]) returns an
// empty list for a corrupted mount point, which is exactly the case the recovery
// has to handle, so the mount table is parsed here instead.
//
// Only the parent directory of source has its symbolic links resolved, never
// source itself: resolving or stat'ing a corrupted mount point fails with
// ENOTCONN, and the recovery would never learn about the bind mounts it has to
// refresh.
func (m *mounter) BindTargets(source string) ([]BindTarget, error) {
	if err := validatePath("source", source); err != nil {
		return nil, err
	}

	infos, err := parseMountInfo(m.mountInfoPath)
	if err != nil {
		return nil, fmt.Errorf("read mountinfo %q: %w", m.mountInfoPath, err)
	}

	resolved := source
	if dir, evalErr := filepath.EvalSymlinks(filepath.Dir(source)); evalErr == nil {
		resolved = filepath.Join(dir, filepath.Base(source))
	}
	return bindTargetsFrom(infos, source, resolved), nil
}

// bindTargetsFrom returns the other mount points of the file system mounted at
// source, which is listed in infos as source or as resolved.
//
// A bind mount of a mount point has the same device number (major:minor) and the
// same root as the mount point itself, which is what identifies it in mountinfo:
// a separate FUSE mount of the same GlusterFS volume gets a device number of its
// own and is therefore not mistaken for a bind mount.
//
// The result is never nil, so that a caller can range over it unconditionally.
func bindTargetsFrom(infos []mountInfo, source, resolved string) []BindTarget {
	// The last matching entry wins: a target can be mounted over, and the
	// top-most mount comes last in mountinfo.
	var staging mountInfo
	found := false
	for _, info := range infos {
		if info.MountPoint == source || info.MountPoint == resolved {
			staging, found = info, true
		}
	}
	if !found {
		return []BindTarget{}
	}

	// The order is the first appearance of a path, the value of a repeated path
	// is the last one: mountinfo can list the same mount point twice, and the
	// later entry describes the mount that is visible.
	targets := make([]BindTarget, 0, len(infos))
	index := make(map[string]int, len(infos))
	for _, info := range infos {
		if info.Major != staging.Major || info.Minor != staging.Minor || info.Root != staging.Root {
			continue
		}
		if info.MountPoint == source || info.MountPoint == resolved {
			continue
		}
		// A mount point that is not an absolute, clean path cannot be used as a
		// bind mount source: mount(8) would resolve it against the working
		// directory of the plugin.
		if !filepath.IsAbs(info.MountPoint) || filepath.Clean(info.MountPoint) != info.MountPoint {
			continue
		}

		target := BindTarget{Path: info.MountPoint, ReadOnly: slices.Contains(info.MountOptions, "ro")}
		if i, seen := index[info.MountPoint]; seen {
			targets[i] = target
			continue
		}
		index[info.MountPoint] = len(targets)
		targets = append(targets, target)
	}
	return targets
}
