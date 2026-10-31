package driver

import (
	"context"
	"errors"
	"io/fs"
	"math"
	"syscall"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
	mount "k8s.io/mount-utils"

	"github.com/gitlayzer/csi-driver-glusterfs/pkg/mounter"
)

// corruptedErrnos are the errnos that mean a mount stopped responding. Their
// values are the ones of the mounter package; they are repeated here because
// that list is unexported, and because statFS wraps the raw errno in an
// os.PathError that only errors.Is can see through.
var corruptedErrnos = []error{
	syscall.ENOTCONN,
	syscall.ESTALE,
	syscall.EIO,
	syscall.EHOSTDOWN,
}

// fsStats are the statfs numbers of a file system. Every field is >= 0.
type fsStats struct {
	// totalBytes is the size of the file system in bytes.
	totalBytes int64
	// availableBytes is what an unprivileged process may still use.
	availableBytes int64
	// usedBytes is totalBytes minus the free bytes, i.e. what is stored.
	usedBytes int64
	// totalInodes is the number of inodes of the file system, 0 when unknown.
	totalInodes int64
	// freeInodes is the number of unused inodes.
	freeInodes int64
	// usedInodes is totalInodes minus the free inodes.
	usedInodes int64
}

// statsFromStatfs converts raw statfs numbers. bsize is the block size in bytes.
// used = blocks - bfree (0 when bfree > blocks); bytes = blocks * bsize.
// A product or value that does not fit into int64 is clamped to math.MaxInt64,
// so no field is ever negative (the CSI spec forbids negative values).
//
// The clamp keeps a file system that reports absurd numbers (a broken FUSE
// mount, a 32 bit wrap in the kernel) from turning into a negative usage in the
// response, which the CO would read as a corrupted statistic.
func statsFromStatfs(blocks, bfree, bavail, bsize, files, ffree uint64) fsStats {
	return fsStats{
		totalBytes:     byteCount(blocks, bsize),
		availableBytes: byteCount(bavail, bsize),
		usedBytes:      byteCount(usedBlocks(blocks, bfree), bsize),
		totalInodes:    countValue(files),
		freeInodes:     countValue(ffree),
		usedInodes:     countValue(usedBlocks(files, ffree)),
	}
}

// usedBlocks returns blocks - bfree, or 0 when the file system reports more free
// blocks than blocks. Subtracting in uint64 would wrap around in that case.
func usedBlocks(blocks, free uint64) uint64 {
	if free >= blocks {
		return 0
	}
	return blocks - free
}

// byteCount returns blocks * bsize clamped to math.MaxInt64.
func byteCount(blocks, bsize uint64) int64 {
	if blocks != 0 && bsize > math.MaxInt64/blocks {
		return math.MaxInt64
	}
	return countValue(blocks * bsize)
}

// countValue returns a count clamped to math.MaxInt64.
func countValue(n uint64) int64 {
	if n > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(n)
}

// volumeUsageFromStats builds the usage list of NodeGetVolumeStats.
//
// The BYTES entry is always returned. The INODES entry is omitted when the file
// system reports no inodes at all: the CSI spec allows omitting inode
// information when it is unavailable, and a made up "0 of 0 inodes" would look
// like a full file system to some consumers.
func volumeUsageFromStats(st fsStats) []*csi.VolumeUsage {
	usage := []*csi.VolumeUsage{{
		Unit:      csi.VolumeUsage_BYTES,
		Total:     st.totalBytes,
		Available: st.availableBytes,
		Used:      st.usedBytes,
	}}
	if st.totalInodes > 0 {
		usage = append(usage, &csi.VolumeUsage{
			Unit:      csi.VolumeUsage_INODES,
			Total:     st.totalInodes,
			Available: st.freeInodes,
			Used:      st.usedInodes,
		})
	}
	return usage
}

// NodeGetVolumeStats reports the file system usage of the volume that is mounted
// at volume_path.
//
// It takes no in-flight lock and reads no cluster configuration: a monitoring
// call must not be turned into Aborted by a concurrent stage or publish, and must
// not depend on the ConfigMap. The mount state check below is the only state it
// needs.
func (s *nodeService) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	switch {
	case req.GetVolumeId() == "":
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	case req.GetVolumePath() == "":
		return nil, status.Error(codes.InvalidArgument, "volume path is required")
	}
	id, err := decodeVolumeID(req.GetVolumeId())
	if err != nil {
		return nil, err
	}

	// An unusable volume_path is NotFound, not InvalidArgument: this RPC defines
	// no other error than NotFound ("the volume does not exist on the specified
	// volume_path"), and a relative or unclean path cannot be where the volume
	// is. csi-sanity asserts NotFound for "some/path".
	volumePath := req.GetVolumePath()
	if verr := validatePath("volume path", volumePath); verr != nil {
		klog.V(4).InfoS("volume path of a statistics request is unusable",
			"volumeID", req.GetVolumeId(), "volumePath", volumePath, "reason", verr)
		return nil, status.Errorf(codes.NotFound, "volume %s does not exist at %q",
			echoVolumeID(req.GetVolumeId()), volumePath)
	}
	// staging_target_path is deliberately neither used nor validated: it is
	// OPTIONAL, the statistics only describe volume_path, and rejecting a
	// malformed value would refuse a call that can be served.

	if cerr := ctxStatus(ctx); cerr != nil {
		return nil, cerr
	}

	state, err := s.mounter.VolumeMountState(volumePath, id.Volume, id.Subdir)
	switch {
	case err != nil:
		return nil, status.Errorf(codes.Internal, "check volume %s at %q: %v",
			echoVolumeID(req.GetVolumeId()), volumePath, err)
	case state == mounter.MountStateNotMounted:
		return nil, status.Errorf(codes.NotFound, "volume %s is not mounted at %q",
			echoVolumeID(req.GetVolumeId()), volumePath)
	case state == mounter.MountStateMismatch:
		return nil, status.Errorf(codes.NotFound,
			"volume %s is not mounted at %q, another file system is", echoVolumeID(req.GetVolumeId()), volumePath)
	case state == mounter.MountStateCorrupted:
		// Internal, not NotFound: the mount is there, its file system just does
		// not answer. Reporting "does not exist" would make the CO believe the
		// data is gone, although it is still on the GlusterFS volume.
		return nil, status.Errorf(codes.Internal,
			"mount of volume %s at %q is corrupted; NodeGetVolumeHealth reports it",
			echoVolumeID(req.GetVolumeId()), volumePath)
	}

	st, err := s.statFS(volumePath)
	if err != nil {
		return nil, statfsErrorToStatus(req.GetVolumeId(), volumePath, err)
	}

	klog.V(2).InfoS("reported volume statistics", "volumeID", req.GetVolumeId(),
		"volumePath", volumePath, "totalBytes", st.totalBytes, "availableBytes", st.availableBytes)

	return &csi.NodeGetVolumeStatsResponse{Usage: volumeUsageFromStats(st)}, nil
}

// statfsErrorToStatus maps a statfs failure of a checked mount point to a gRPC
// status. A path that disappeared is NotFound, because the volume was unmounted
// after the state check above. A corrupted mount is Internal with the same
// message as the MountStateCorrupted case, so that a caller always sees the same
// answer for a dead FUSE process.
func statfsErrorToStatus(volumeID, volumePath string, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return status.Errorf(codes.NotFound, "volume %s is not mounted at %q",
			echoVolumeID(volumeID), volumePath)
	case isCorruptedError(err):
		return status.Errorf(codes.Internal,
			"mount of volume %s at %q is corrupted; NodeGetVolumeHealth reports it",
			echoVolumeID(volumeID), volumePath)
	default:
		return status.Errorf(codes.Internal, "statfs %q: %v", volumePath, err)
	}
}

// isCorruptedError reports whether err says that a mounted file system stopped
// responding. mount.IsCorruptedMnt knows the platform specific list but only
// looks at the outermost error type, so the errnos are checked with errors.Is as
// well, which sees through the os.PathError that statFS builds and through any
// fmt.Errorf("%w") wrapping.
func isCorruptedError(err error) bool {
	if mount.IsCorruptedMnt(err) {
		return true
	}
	for _, target := range corruptedErrnos {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}
