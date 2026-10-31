//go:build !windows

package mounter

import (
	mount "k8s.io/mount-utils"
)

// mountInfo is one entry of the mount table. It is an alias of mount.MountInfo so
// that [bindTargetsFrom] can keep its signature on every platform; only Windows
// has to spell the fields out (see mountinfo_windows.go).
type mountInfo = mount.MountInfo

// parseMountInfo reads and parses the mount table at path.
//
// It is a separate function so that the Windows stub can report the same
// signature: mount.MountInfo only exists on the platforms that have
// /proc/<pid>/mountinfo.
func parseMountInfo(path string) ([]mountInfo, error) {
	return mount.ParseMountInfo(path)
}
