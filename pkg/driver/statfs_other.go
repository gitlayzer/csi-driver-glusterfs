//go:build !linux && !darwin

package driver

import (
	"errors"
	"fmt"
)

// statFS is not available on this platform: there is no statfs(2) to call
// through the standard library. The binary does not run on such a platform
// (GlusterFS is Linux only), the stub exists so that the package still builds
// and so that a caller gets a comprehensible Internal error instead of a
// compile error.
func statFS(path string) (fsStats, error) {
	return fsStats{}, fmt.Errorf("statfs %q: %w", path, errors.ErrUnsupported)
}
