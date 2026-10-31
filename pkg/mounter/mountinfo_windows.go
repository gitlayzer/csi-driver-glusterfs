//go:build windows

package mounter

import (
	"errors"
	"fmt"
)

// mountInfo is one entry of the mount table on a platform that has no mountinfo
// file.
//
// mount-utils does not define mount.MountInfo on Windows, but [bindTargetsFrom]
// is a pure function that the unit tests want to run everywhere. The fields are
// the ones the matching uses; nothing on Windows ever fills them in, because
// [parseMountInfo] fails.
type mountInfo struct {
	// Major and Minor are the two halves of the device number.
	Major, Minor int
	// Root is the directory of the file system that forms the root of the mount.
	Root string
	// MountPoint is the path the file system is mounted at.
	MountPoint string
	// MountOptions are the per-mount options, "ro" among them.
	MountOptions []string
}

// parseMountInfo is not available on Windows: there is no mountinfo file. The
// driver does not run on Windows (GlusterFS is Linux only); the stub keeps the
// package building and makes a caller get a comprehensible error instead of a
// compile error.
func parseMountInfo(path string) ([]mountInfo, error) {
	return nil, fmt.Errorf("parse mountinfo %q: %w", path, errors.ErrUnsupported)
}
