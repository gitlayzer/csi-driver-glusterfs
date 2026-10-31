//go:build linux

package driver

import (
	"os"
	"syscall"
)

// statFS reads the file system statistics of path with statfs(2).
//
// The standard library's syscall package is used instead of
// golang.org/x/sys/unix so that the driver needs no dependency beyond the ones
// it already has; the numbers are the same that kubelet's fs.Info reports.
func statFS(path string) (fsStats, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		// A PathError, so that callers can use errors.Is(err, fs.ErrNotExist)
		// and mount.IsCorruptedMnt(err) on it.
		return fsStats{}, &os.PathError{Op: "statfs", Path: path, Err: err}
	}
	// Statfs_t.Bsize is a signed int64 on Linux. A negative or zero block size
	// is nonsense and is treated as 0, which keeps the conversion below sound
	// instead of turning it into a huge number.
	var bsize uint64
	if st.Bsize > 0 {
		bsize = uint64(st.Bsize)
	}
	return statsFromStatfs(st.Blocks, st.Bfree, st.Bavail, bsize, st.Files, st.Ffree), nil
}
