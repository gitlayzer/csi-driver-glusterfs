//go:build darwin

package driver

import (
	"os"
	"syscall"
)

// statFS reads the file system statistics of path with statfs(2).
//
// macOS has the call as well, which keeps the unit tests of the driver working
// on a developer machine. The production target is Linux; Statfs_t.Bsize is an
// unsigned uint32 here, so the conversion below cannot produce a negative block
// size.
func statFS(path string) (fsStats, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		// A PathError, so that callers can use errors.Is(err, fs.ErrNotExist)
		// and mount.IsCorruptedMnt(err) on it.
		return fsStats{}, &os.PathError{Op: "statfs", Path: path, Err: err}
	}
	return statsFromStatfs(st.Blocks, st.Bfree, st.Bavail, uint64(st.Bsize), st.Files, st.Ffree), nil
}
