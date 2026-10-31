//go:build linux || darwin

package driver

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	mount "k8s.io/mount-utils"

	"github.com/gitlayzer/csi-driver-glusterfs/pkg/mounter"
)

// TestStatFS exercises the real statfs(2) call on the platforms that have one.
func TestStatFS(t *testing.T) {
	t.Run("temporary directory", func(t *testing.T) {
		dir := t.TempDir()

		st, err := statFS(dir)
		if err != nil {
			t.Fatalf("statFS(%q) = %v, want nil", dir, err)
		}
		if st.totalBytes <= 0 {
			t.Fatalf("totalBytes = %d, want > 0", st.totalBytes)
		}
		if st.availableBytes > st.totalBytes {
			t.Fatalf("availableBytes = %d, want <= totalBytes %d", st.availableBytes, st.totalBytes)
		}
		if st.usedBytes < 0 || st.usedBytes > st.totalBytes {
			t.Fatalf("usedBytes = %d, want within [0, totalBytes]", st.usedBytes)
		}
		if st.totalInodes <= 0 {
			t.Errorf("totalInodes = %d, want > 0 on a local file system", st.totalInodes)
		}
		if st.usedInodes < 0 || st.usedInodes > st.totalInodes {
			t.Errorf("usedInodes = %d, want within [0, totalInodes] %d", st.usedInodes, st.totalInodes)
		}
	})

	t.Run("missing path", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "does-not-exist")

		_, err := statFS(path)
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("statFS(%q) = %v, want an error wrapping fs.ErrNotExist", path, err)
		}
		// A PathError carries the operation and the path into the Internal
		// message that NodeGetVolumeStats returns.
		var pathErr *os.PathError
		if !errors.As(err, &pathErr) {
			t.Fatalf("statFS(%q) = %v, want an *os.PathError", path, err)
		}
		if pathErr.Op != "statfs" || pathErr.Path != path {
			t.Errorf("PathError = %q of %q, want \"statfs\" of %q", pathErr.Op, pathErr.Path, path)
		}
		if !strings.Contains(err.Error(), "statfs") {
			t.Errorf("error %q does not name the failed operation", err)
		}
	})
}

// resolvedTempDir returns a fresh temporary directory with symbolic links
// resolved, so that the path is the one mount-utils records in its mount table.
func resolvedTempDir(t *testing.T) string {
	t.Helper()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks() = %v, want nil", err)
	}
	return dir
}

// TestNodeVolumeStatsAndHealthLifecycle drives a real stage and publish on a
// mount-utils FakeMounter and then asks for statistics and health, before and
// after the staging FUSE mount breaks. It is in this file because it needs the
// real statfs(2) of the host file system, and this file only builds on the
// platforms that have one.
func TestNodeVolumeStatsAndHealthLifecycle(t *testing.T) {
	if _, err := statFS(t.TempDir()); err != nil {
		t.Skipf("statFS is not supported here: %v", err)
	}

	node, _ := newTestNode(t)
	fake := mount.NewFakeMounter(nil)
	node.mounter = mounter.New(fake, "")
	node.statFS = statFS
	ctx := context.Background()

	// The resolved paths: FakeMounter only replaces the source of a bind mount by
	// the original device when it matches the recorded mount point exactly
	// (on macOS /var is a symlink to /private/var).
	staging := resolvedTempDir(t)
	target := resolvedTempDir(t)
	if _, err := node.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: staging,
		VolumeCapability:  mountCapability,
	}); err != nil {
		t.Fatalf("NodeStageVolume() = %v, want nil", err)
	}
	if _, err := node.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: staging,
		TargetPath:        target,
		VolumeCapability:  mountCapability,
	}); err != nil {
		t.Fatalf("NodePublishVolume() = %v, want nil", err)
	}

	// FakeMounter records a bind mount with an empty fs type, while Linux reports
	// the type of the original mount in /proc/mounts. The entry is fixed up to
	// model a real node; the production check is not relaxed for it.
	for i := range fake.MountPoints {
		if fake.MountPoints[i].Path == target {
			fake.MountPoints[i].Type = "fuse.glusterfs"
		}
	}

	stats, err := node.NodeGetVolumeStats(ctx, &csi.NodeGetVolumeStatsRequest{
		VolumeId:          testVolumeID,
		VolumePath:        target,
		StagingTargetPath: staging,
	})
	if err != nil {
		t.Fatalf("NodeGetVolumeStats(%q) = %v, want nil", target, err)
	}
	usage := stats.GetUsage()
	if len(usage) == 0 {
		t.Fatal("usage = nil, want at least the BYTES entry")
	}
	if got := usage[0].GetUnit(); got != csi.VolumeUsage_BYTES {
		t.Fatalf("usage[0].Unit = %v, want %v", got, csi.VolumeUsage_BYTES)
	}
	if got := usage[0].GetTotal(); got <= 0 {
		t.Fatalf("usage[0].Total = %d, want > 0", got)
	}
	if got, total := usage[0].GetAvailable(), usage[0].GetTotal(); got > total {
		t.Fatalf("usage[0].Available = %d, want <= Total %d", got, total)
	}

	health, err := node.NodeGetVolumeHealth(ctx, &csi.NodeGetVolumeHealthRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: staging,
		VolumePublishPath: target,
	})
	if err != nil {
		t.Fatalf("NodeGetVolumeHealth() = %v, want nil", err)
	}
	if got := health.GetVolumeHealth().GetHealthStatuses(); len(got) != 0 {
		t.Fatalf("health statuses = %v, want none for a healthy volume", got)
	}

	// The FUSE process of the staging mount died: every check of the target path
	// reports ENOTCONN from now on, which is what a corrupted mount point looks
	// like to the real mounter.
	fake.MountCheckErrors = map[string]error{target: syscall.ENOTCONN}

	health, err = node.NodeGetVolumeHealth(ctx, &csi.NodeGetVolumeHealthRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: staging,
		VolumePublishPath: target,
	})
	if err != nil {
		t.Fatalf("NodeGetVolumeHealth() after the FUSE process died = %v, want nil", err)
	}
	statuses := health.GetVolumeHealth().GetHealthStatuses()
	if len(statuses) != 1 {
		t.Fatalf("health statuses = %v, want exactly one entry", statuses)
	}
	if got := statuses[0].GetStatus(); got != csi.VolumeHealthErrorType_INACCESSIBLE {
		t.Errorf("status = %v, want %v", got, csi.VolumeHealthErrorType_INACCESSIBLE)
	}
	if got := statuses[0].GetReason(); got != healthReasonMountCorrupted {
		t.Errorf("reason = %q, want %q", got, healthReasonMountCorrupted)
	}
	if !strings.Contains(statuses[0].GetMessage(), target) {
		t.Errorf("message = %q, want it to name %q", statuses[0].GetMessage(), target)
	}

	_, err = node.NodeGetVolumeStats(ctx, &csi.NodeGetVolumeStatsRequest{
		VolumeId:   testVolumeID,
		VolumePath: target,
	})
	wantCode(t, err, codes.Internal)
}
