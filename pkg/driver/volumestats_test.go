package driver

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/gitlayzer/csi-driver-glusterfs/pkg/mounter"
)

func TestStatsFromStatfs(t *testing.T) {
	tests := []struct {
		name   string
		blocks uint64
		bfree  uint64
		bavail uint64
		bsize  uint64
		files  uint64
		ffree  uint64
		want   fsStats
	}{
		{
			name:   "regular file system",
			blocks: 1000, bfree: 400, bavail: 350, bsize: 4096, files: 500, ffree: 100,
			want: fsStats{
				totalBytes:     1000 * 4096,
				availableBytes: 350 * 4096,
				usedBytes:      600 * 4096,
				totalInodes:    500,
				freeInodes:     100,
				usedInodes:     400,
			},
		},
		{
			name:   "empty file system",
			blocks: 0, bfree: 0, bavail: 0, bsize: 4096, files: 0, ffree: 0,
			want: fsStats{},
		},
		{
			name:   "more free blocks than blocks",
			blocks: 100, bfree: 200, bavail: 200, bsize: 512, files: 10, ffree: 20,
			want: fsStats{
				totalBytes:     100 * 512,
				availableBytes: 200 * 512,
				usedBytes:      0,
				totalInodes:    10,
				freeInodes:     20,
				usedInodes:     0,
			},
		},
		{
			name:   "byte product overflows",
			blocks: 1 << 60, bfree: 0, bavail: 0, bsize: 1 << 10, files: 1, ffree: 1,
			want: fsStats{
				totalBytes:     math.MaxInt64,
				availableBytes: 0,
				usedBytes:      math.MaxInt64,
				totalInodes:    1,
				freeInodes:     1,
				usedInodes:     0,
			},
		},
		{
			name:   "inode count above MaxInt64",
			blocks: 1, bfree: 0, bavail: 0, bsize: 1, files: math.MaxUint64, ffree: 0,
			want: fsStats{
				totalBytes:     1,
				availableBytes: 0,
				usedBytes:      1,
				totalInodes:    math.MaxInt64,
				freeInodes:     0,
				usedInodes:     math.MaxInt64,
			},
		},
		{
			name:   "zero block size",
			blocks: 1000, bfree: 400, bavail: 400, bsize: 0, files: 5, ffree: 5,
			want: fsStats{
				totalBytes:     0,
				availableBytes: 0,
				usedBytes:      0,
				totalInodes:    5,
				freeInodes:     5,
				usedInodes:     0,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := statsFromStatfs(tc.blocks, tc.bfree, tc.bavail, tc.bsize, tc.files, tc.ffree)
			if got != tc.want {
				t.Fatalf("statsFromStatfs(%d, %d, %d, %d, %d, %d) = %+v, want %+v",
					tc.blocks, tc.bfree, tc.bavail, tc.bsize, tc.files, tc.ffree, got, tc.want)
			}
			// The CSI spec forbids negative values, so the property is asserted
			// for every case instead of only for the clamped ones.
			for name, value := range map[string]int64{
				"totalBytes":     got.totalBytes,
				"availableBytes": got.availableBytes,
				"usedBytes":      got.usedBytes,
				"totalInodes":    got.totalInodes,
				"freeInodes":     got.freeInodes,
				"usedInodes":     got.usedInodes,
			} {
				if value < 0 {
					t.Fatalf("%s = %d, want >= 0", name, value)
				}
			}
		})
	}
}

func TestVolumeUsageFromStats(t *testing.T) {
	tests := []struct {
		name  string
		stats fsStats
		want  []*csi.VolumeUsage
	}{
		{
			name: "bytes and inodes",
			stats: fsStats{
				totalBytes: 1000, availableBytes: 400, usedBytes: 600,
				totalInodes: 100, freeInodes: 60, usedInodes: 40,
			},
			want: []*csi.VolumeUsage{
				{Unit: csi.VolumeUsage_BYTES, Total: 1000, Available: 400, Used: 600},
				{Unit: csi.VolumeUsage_INODES, Total: 100, Available: 60, Used: 40},
			},
		},
		{
			name:  "bytes only when the inode count is unknown",
			stats: fsStats{totalBytes: 1000, availableBytes: 1000},
			want: []*csi.VolumeUsage{
				{Unit: csi.VolumeUsage_BYTES, Total: 1000, Available: 1000, Used: 0},
			},
		},
		{
			name:  "no inodes and no bytes",
			stats: fsStats{},
			want: []*csi.VolumeUsage{
				{Unit: csi.VolumeUsage_BYTES},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := volumeUsageFromStats(tc.stats)
			assertUsage(t, got, tc.want)
		})
	}
}

// assertUsage compares two usage lists entry by entry, including the order.
func assertUsage(t *testing.T, got, want []*csi.VolumeUsage) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("usage = %v, want %v", got, want)
	}
	for i := range want {
		if got[i].GetUnit() != want[i].GetUnit() {
			t.Errorf("usage[%d].Unit = %v, want %v", i, got[i].GetUnit(), want[i].GetUnit())
		}
		if got[i].GetTotal() != want[i].GetTotal() {
			t.Errorf("usage[%d].Total = %d, want %d", i, got[i].GetTotal(), want[i].GetTotal())
		}
		if got[i].GetAvailable() != want[i].GetAvailable() {
			t.Errorf("usage[%d].Available = %d, want %d", i, got[i].GetAvailable(), want[i].GetAvailable())
		}
		if got[i].GetUsed() != want[i].GetUsed() {
			t.Errorf("usage[%d].Used = %d, want %d", i, got[i].GetUsed(), want[i].GetUsed())
		}
	}
}

// TestNodeGetVolumeStats checks the whole request and error handling table of
// NodeGetVolumeStats against the fake mounter and an injected statFS.
func TestNodeGetVolumeStats(t *testing.T) {
	// stagedVolumePath is a clean absolute path for the requests below.
	stagedVolumePath := "/var/lib/kubelet/pods/uid/volumes/glusterfs/mount"

	tests := []struct {
		name   string
		mutate func(t *testing.T, req *csi.NodeGetVolumeStatsRequest, node *nodeService, fake *fakeMounter)
		// wantCode is the expected gRPC code.
		wantCode codes.Code
		// wantMountState is the expected number of VolumeMountState calls.
		wantMountState int
		// wantStatFS is the expected number of statFS calls.
		wantStatFS int
		// wantUsage is the expected usage list on success.
		wantUsage []*csi.VolumeUsage
		// wantMsg is a substring the error message must contain.
		wantMsg string
	}{
		{
			name: "missing volume id",
			mutate: func(_ *testing.T, req *csi.NodeGetVolumeStatsRequest, _ *nodeService, _ *fakeMounter) {
				req.VolumeId = ""
			},
			wantCode: codes.InvalidArgument,
		},
		{
			name: "missing volume path",
			mutate: func(_ *testing.T, req *csi.NodeGetVolumeStatsRequest, _ *nodeService, _ *fakeMounter) {
				req.VolumePath = ""
			},
			wantCode: codes.InvalidArgument,
		},
		{
			name: "garbage volume id",
			mutate: func(_ *testing.T, req *csi.NodeGetVolumeStatsRequest, _ *nodeService, _ *fakeMounter) {
				req.VolumeId = "garbage"
			},
			wantCode: codes.NotFound,
			wantMsg:  `volume "garbage"`,
		},
		{
			name: "relative volume path",
			mutate: func(_ *testing.T, req *csi.NodeGetVolumeStatsRequest, _ *nodeService, _ *fakeMounter) {
				req.VolumePath = "some/path"
			},
			wantCode: codes.NotFound,
			wantMsg:  `volume "gfs:v1:subdir:c1:vol:pvc-1" does not exist at "some/path"`,
		},
		{
			name: "unclean volume path",
			mutate: func(_ *testing.T, req *csi.NodeGetVolumeStatsRequest, _ *nodeService, _ *fakeMounter) {
				req.VolumePath = "/a/../b"
			},
			wantCode: codes.NotFound,
			wantMsg:  "does not exist at",
		},
		{
			name: "not mounted",
			mutate: func(_ *testing.T, _ *csi.NodeGetVolumeStatsRequest, _ *nodeService, fake *fakeMounter) {
				fake.mountState = mounter.MountStateNotMounted
			},
			wantCode:       codes.NotFound,
			wantMountState: 1,
			wantMsg:        "is not mounted at",
		},
		{
			name: "another file system is mounted",
			mutate: func(_ *testing.T, _ *csi.NodeGetVolumeStatsRequest, _ *nodeService, fake *fakeMounter) {
				fake.mountState = mounter.MountStateMismatch
			},
			wantCode:       codes.NotFound,
			wantMountState: 1,
			wantMsg:        "another file system is",
		},
		{
			name: "corrupted mount",
			mutate: func(_ *testing.T, _ *csi.NodeGetVolumeStatsRequest, _ *nodeService, fake *fakeMounter) {
				fake.mountState = mounter.MountStateCorrupted
			},
			wantCode:       codes.Internal,
			wantMountState: 1,
			wantMsg:        "is corrupted; NodeGetVolumeHealth reports it",
		},
		{
			name: "mount state check fails",
			mutate: func(_ *testing.T, _ *csi.NodeGetVolumeStatsRequest, _ *nodeService, fake *fakeMounter) {
				fake.mountStateErr = errors.New("mount table unavailable")
			},
			wantCode:       codes.Internal,
			wantMountState: 1,
			wantMsg:        "check volume",
		},
		{
			name: "volume disappeared before statfs",
			mutate: func(_ *testing.T, _ *csi.NodeGetVolumeStatsRequest, node *nodeService, _ *fakeMounter) {
				node.statFS = func(string) (fsStats, error) {
					return fsStats{}, &os.PathError{Op: "statfs", Path: stagedVolumePath, Err: syscall.ENOENT}
				}
			},
			wantCode:       codes.NotFound,
			wantMountState: 1,
			wantStatFS:     1,
			wantMsg:        "is not mounted at",
		},
		{
			name: "statfs on a corrupted mount",
			mutate: func(_ *testing.T, _ *csi.NodeGetVolumeStatsRequest, node *nodeService, _ *fakeMounter) {
				node.statFS = func(string) (fsStats, error) {
					return fsStats{}, &os.PathError{Op: "statfs", Path: stagedVolumePath, Err: syscall.ENOTCONN}
				}
			},
			wantCode:       codes.Internal,
			wantMountState: 1,
			wantStatFS:     1,
			wantMsg:        "is corrupted; NodeGetVolumeHealth reports it",
		},
		{
			name: "statfs fails",
			mutate: func(_ *testing.T, _ *csi.NodeGetVolumeStatsRequest, node *nodeService, _ *fakeMounter) {
				node.statFS = func(string) (fsStats, error) {
					return fsStats{}, errors.New("injected statfs failure")
				}
			},
			wantCode:       codes.Internal,
			wantMountState: 1,
			wantStatFS:     1,
			wantMsg:        "statfs",
		},
		{
			name: "success",
			mutate: func(_ *testing.T, req *csi.NodeGetVolumeStatsRequest, _ *nodeService, _ *fakeMounter) {
				// staging_target_path is optional and is not validated, so a
				// relative value must not stop the call.
				req.StagingTargetPath = "not/absolute"
			},
			wantCode:       codes.OK,
			wantMountState: 1,
			wantStatFS:     1,
			wantUsage: []*csi.VolumeUsage{
				{Unit: csi.VolumeUsage_BYTES, Total: 1000, Available: 400, Used: 600},
				{Unit: csi.VolumeUsage_INODES, Total: 100, Available: 60, Used: 40},
			},
		},
		{
			name: "success without inode information",
			mutate: func(_ *testing.T, _ *csi.NodeGetVolumeStatsRequest, node *nodeService, _ *fakeMounter) {
				node.statFS = func(string) (fsStats, error) {
					return fsStats{totalBytes: 2048, availableBytes: 1024, usedBytes: 1024}, nil
				}
			},
			wantCode:       codes.OK,
			wantMountState: 1,
			wantStatFS:     1,
			wantUsage: []*csi.VolumeUsage{
				{Unit: csi.VolumeUsage_BYTES, Total: 2048, Available: 1024, Used: 1024},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node, fake := newTestNode(t)
			req := &csi.NodeGetVolumeStatsRequest{
				VolumeId:   testVolumeID,
				VolumePath: stagedVolumePath,
			}
			if tc.mutate != nil {
				tc.mutate(t, req, node, fake)
			}

			resp, err := node.NodeGetVolumeStats(context.Background(), req)
			wantCode(t, err, tc.wantCode)
			if tc.wantMsg != "" && !strings.Contains(statusMessage(err), tc.wantMsg) {
				t.Fatalf("message = %q, want it to contain %q", statusMessage(err), tc.wantMsg)
			}
			if got := fake.mountStateCount(); got != tc.wantMountState {
				t.Fatalf("VolumeMountState calls = %d, want %d", got, tc.wantMountState)
			}
			if tc.wantCode != codes.OK {
				if resp != nil {
					t.Fatalf("response = %v, want nil on error", resp)
				}
				return
			}
			assertUsage(t, resp.GetUsage(), tc.wantUsage)
			// The mount state question is asked with the fields of the volume id,
			// not with anything from volume_context.
			call := fake.mountStateCalls[0]
			if call.target != req.GetVolumePath() || call.volume != "vol" || call.subdir != "pvc-1" {
				t.Fatalf("VolumeMountState(%q, %q, %q), want (%q, %q, %q)",
					call.target, call.volume, call.subdir, req.GetVolumePath(), "vol", "pvc-1")
			}
		})
	}

	t.Run("statfs is asked about the volume path", func(t *testing.T) {
		node, _ := newTestNode(t)
		var asked []string
		node.statFS = func(path string) (fsStats, error) {
			asked = append(asked, path)
			return testFSStats, nil
		}

		if _, err := node.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
			VolumeId:   testVolumeID,
			VolumePath: stagedVolumePath,
		}); err != nil {
			t.Fatalf("NodeGetVolumeStats() = %v, want nil", err)
		}
		if len(asked) != 1 || asked[0] != stagedVolumePath {
			t.Fatalf("statFS calls = %q, want exactly [%q]", asked, stagedVolumePath)
		}
	})

	t.Run("no in-flight lock is taken", func(t *testing.T) {
		node, fake := newTestNode(t)
		// Both locks that a stage or publish of the same volume would hold.
		for _, key := range []string{nodeStageLockPrefix + testVolumeID, nodePublishVolumeLockPrefix + testVolumeID} {
			if !node.locks.tryAcquire(key) {
				t.Fatalf("could not pre-acquire %q", key)
			}
			defer node.locks.release(key)
		}

		req := &csi.NodeGetVolumeStatsRequest{VolumeId: testVolumeID, VolumePath: stagedVolumePath}
		if _, err := node.NodeGetVolumeStats(context.Background(), req); err != nil {
			t.Fatalf("NodeGetVolumeStats() = %v, want nil although the volume locks are held", err)
		}
		if got := fake.mountStateCount(); got != 1 {
			t.Fatalf("VolumeMountState calls = %d, want 1", got)
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		node, fake := newTestNode(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := node.NodeGetVolumeStats(ctx, &csi.NodeGetVolumeStatsRequest{
			VolumeId: testVolumeID, VolumePath: stagedVolumePath,
		})
		wantCode(t, err, codes.Canceled)
		if got := fake.mountStateCount(); got != 0 {
			t.Fatalf("VolumeMountState calls = %d, want 0 with a cancelled context", got)
		}
	})
}

// TestIsCorruptedError checks the errno list that decides whether a statfs
// failure is reported as a corrupted mount. The wrapped cases matter: mount's
// IsCorruptedMnt only looks at the outermost error type, the errno check of this
// package has to see through fmt.Errorf("%w").
func TestIsCorruptedError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "other", err: errors.New("boom"), want: false},
		{name: "not exist", err: &os.PathError{Op: "statfs", Path: "/x", Err: syscall.ENOENT}, want: false},
		{name: "ENOTCONN", err: &os.PathError{Op: "statfs", Path: "/x", Err: syscall.ENOTCONN}, want: true},
		{name: "ESTALE", err: &os.PathError{Op: "statfs", Path: "/x", Err: syscall.ESTALE}, want: true},
		{name: "EIO", err: &os.PathError{Op: "statfs", Path: "/x", Err: syscall.EIO}, want: true},
		{name: "EHOSTDOWN", err: &os.PathError{Op: "statfs", Path: "/x", Err: syscall.EHOSTDOWN}, want: true},
		{name: "wrapped ENOTCONN", err: fmt.Errorf("check: %w", syscall.ENOTCONN), want: true},
		{name: "doubly wrapped EIO", err: fmt.Errorf("a: %w", fmt.Errorf("b: %w", syscall.EIO)), want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCorruptedError(tc.err); got != tc.want {
				t.Fatalf("isCorruptedError(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}

// statusMessage returns the message of a gRPC status error, or of any other
// error, without failing on a nil error.
func statusMessage(err error) string {
	if err == nil {
		return ""
	}
	return status.Convert(err).Message()
}
