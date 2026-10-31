//go:build linux

package mounter

import (
	"errors"
	"fmt"
	"syscall"
)

// detachMount lazily unmounts target with umount2(2) and MNT_DETACH.
//
// The lazy form is what makes the recovery possible: an ordinary unmount of a
// mount point whose FUSE process died fails with EBUSY while any process still
// has the mount in its namespace, i.e. exactly when the recovery is needed. A
// lazy unmount detaches the mount from the tree at once and releases it when the
// last user is gone.
//
// ENOENT (the path is gone) and EINVAL (the path is not a mount point) are not
// errors here: both mean there is nothing left to detach, which is the state the
// caller wants.
func detachMount(target string) error {
	err := syscall.Unmount(target, syscall.MNT_DETACH)
	switch {
	case err == nil, errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.EINVAL):
		return nil
	default:
		return fmt.Errorf("detach %q: %w", target, err)
	}
}
