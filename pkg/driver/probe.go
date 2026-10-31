package driver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
)

const (
	// fuseDevice is the character device the FUSE mount helper needs. Without
	// it the node plugin cannot mount a GlusterFS volume at all.
	fuseDevice = "/dev/fuse"
	// mountHelper is the GlusterFS FUSE mount helper. It has to be on PATH;
	// k8s.io/mount-utils would otherwise fall back to a plain mount(2) that the
	// kernel rejects for a gluster:// source.
	mountHelper = "mount.glusterfs"
)

// check is a single Probe readiness check. The name is part of the error
// message, so it must read as "<name> is not ready: <cause>".
type check struct {
	name string
	fn   func(ctx context.Context) error
}

// readinessChecks returns the checks of the current mode.
//
// The node and all modes mount volumes locally and therefore check both
// dependencies (docs/architecture.md §5.7). Controller mode registers no check
// yet because it has no service that mounts anything; the subdir provisioner
// (T-11) FUSE-mounts the parent volume and must add the same checks then.
func (d *Driver) readinessChecks() []check {
	if !d.mode.runsNode() {
		return nil
	}

	return []check{
		{name: fuseDevice, fn: d.checkFuseDevice},
		{name: mountHelper, fn: d.checkMountHelper},
	}
}

// checkFuseDevice verifies that /dev/fuse exists and is a character device.
//
// A regular file named /dev/fuse would pass a plain existence check but fail at
// mount time with an error that is much harder to diagnose.
func (d *Driver) checkFuseDevice(_ context.Context) error {
	info, err := d.stat(fuseDevice)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return fmt.Errorf("%s is not a character device (mode %s)", fuseDevice, info.Mode())
	}
	return nil
}

// checkMountHelper verifies that mount.glusterfs is on PATH.
func (d *Driver) checkMountHelper(_ context.Context) error {
	if _, err := d.lookPath(mountHelper); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("%s not found in PATH", mountHelper)
		}
		return err
	}
	return nil
}
