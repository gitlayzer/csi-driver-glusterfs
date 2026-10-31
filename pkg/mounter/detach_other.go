//go:build !linux

package mounter

import (
	"errors"
	"fmt"
)

// detachMount is not available off Linux: MNT_DETACH is a Linux extension of
// umount2(2), and the standard library offers no portable lazy unmount. The
// driver runs on Linux only (GlusterFS is Linux only); the stub keeps the package
// building and gives a caller a comprehensible error instead of a compile error.
func detachMount(target string) error {
	return fmt.Errorf("detach %q: %w", target, errors.ErrUnsupported)
}
