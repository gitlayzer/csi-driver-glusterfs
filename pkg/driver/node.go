package driver

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
	mount "k8s.io/mount-utils"

	"github.com/gitlayzer/csi-driver-glusterfs/pkg/config"
	"github.com/gitlayzer/csi-driver-glusterfs/pkg/mounter"
	"github.com/gitlayzer/csi-driver-glusterfs/pkg/volumeid"
)

const (
	// nodeStageLockPrefix and nodePublishLockPrefix are the in-flight lock key
	// prefixes of the mount operations. Stage and Unstage of a volume serialize
	// with each other, Publish and Unpublish of a target path do. Keying Publish
	// by the target path rather than by the volume id means the same volume can
	// be published to several Pods at the same time (docs/architecture.md §5.5).
	nodeStageLockPrefix   = "stage/"
	nodePublishLockPrefix = "publish/"
	// nodePublishVolumeLockPrefix additionally serializes the publishes of a
	// volume with a single target access mode, so that two concurrent
	// publishes at different targets cannot both pass the single target check.
	nodePublishVolumeLockPrefix = "publish-volume/"

	// stagingDirMode and targetDirMode are the permissions of the directories
	// the node service creates: kubelet owned mount points, no world access.
	stagingDirMode = 0o750
	targetDirMode  = 0o750

	// mountLogDirMode is the permission of the per-volume mount log directory,
	// which an operator may want to read without being root.
	mountLogDirMode = 0o755
)

// nodeService implements csi.NodeServer.
//
// It is a field of [Driver] rather than an embedded type: both
// UnimplementedIdentityServer and UnimplementedNodeServer provide
// testEmbeddedByValue, so embedding the two in one type would make every
// selector on them ambiguous.
type nodeService struct {
	// UnimplementedNodeServer keeps NodeExpandVolume and every RPC added by a
	// later version of the protocol Unimplemented instead of answering them with
	// a nil error. NodeGetVolumeStats and NodeGetVolumeHealth are implemented in
	// volumestats.go and volumehealth.go.
	csi.UnimplementedNodeServer

	driver *Driver
	// mounter performs the FUSE and bind mounts. It is a field so that tests can
	// replace it with a fake one.
	mounter mounter.Mounter
	// logDir is the directory of the per-volume mount logs, empty for none.
	logDir string
	// records is the store of the staging records the recovery works from, nil
	// when --state-dir is empty, which disables writing records and the recovery
	// (D-20).
	records *recordStore
	// locks rejects concurrent operations on the same volume or target.
	locks lockTable
	// stat is os.Stat; a field so that tests can simulate a corrupted mount
	// point, which no real directory of a unit test can be.
	stat func(name string) (os.FileInfo, error)
	// statFS reads the file system statistics of a path; a field so that tests
	// can simulate statfs results and failures, which a unit test cannot rely on
	// the host file system for. NodeGetVolumeStats uses it.
	statFS func(path string) (fsStats, error)
}

// validatePath validates a path that is used as a mount point. An empty value is
// handled by the RPC itself, because each field has its own error message.
//
// A relative path would be resolved against the working directory of the plugin,
// and an unclean one ("/a/../b", "//a", "/a/") addresses a different directory
// than the one the CO recorded, so both are rejected instead of being
// normalized silently.
func validatePath(field, p string) error {
	switch {
	case !filepath.IsAbs(p):
		return status.Errorf(codes.InvalidArgument, "%s %q must be an absolute path", field, p)
	case filepath.Clean(p) != p:
		return status.Errorf(codes.InvalidArgument, "%s %q must be a clean path", field, p)
	}
	return nil
}

// decodeVolumeID decodes and validates a volume_id.
//
// A malformed id is NotFound, not InvalidArgument: the CSI convention is that an
// id the plugin does not know means "no such volume", and csi-sanity probes the
// node RPCs with ids this driver never issued (docs/architecture.md §5.1).
func decodeVolumeID(volumeID string) (volumeid.ID, error) {
	id, err := volumeid.Decode(volumeID)
	if err != nil {
		return volumeid.ID{}, status.Errorf(codes.NotFound, "volume %s not found: %v",
			echoVolumeID(volumeID), err)
	}
	return id, nil
}

// echoVolumeID renders a volume_id for an error message. An oversized id is
// replaced by its length: the field has no size limit in the request, and
// echoing it would put an unbounded string into an error message and a log line.
func echoVolumeID(volumeID string) string {
	if len(volumeID) > volumeid.MaxLength {
		return fmt.Sprintf("of length %d", len(volumeID))
	}
	return fmt.Sprintf("%q", volumeID)
}

// mountErrorToStatus maps an error of [mounter.Mounter] to a gRPC status. op
// names the failed operation ("stage volume", ...) and prefixes the message.
//
// A cancelled or expired ctx is reported as such instead of Internal: the CO
// gave up, the operation did not fail on its own. InvalidArgument means the
// request itself is wrong (a spec or an unknown mount option), AlreadyExists
// that the target is used by something else. Everything else, including a
// corrupted mount, is Internal with a short cause and without any secret.
func mountErrorToStatus(op string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return status.FromContextError(err).Err()
	case errors.Is(err, mounter.ErrInvalidSpec), errors.Is(err, mounter.ErrInvalidOption):
		return status.Errorf(codes.InvalidArgument, "%s: %v", op, err)
	case errors.Is(err, mounter.ErrMountConflict):
		return status.Errorf(codes.AlreadyExists, "%s: %v", op, err)
	default:
		return status.Errorf(codes.Internal, "%s: %v", op, err)
	}
}

// ctxStatus returns the status error of a cancelled or expired ctx, or nil.
func ctxStatus(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	return nil
}

// NodeStageVolume mounts the GlusterFS volume at the staging target path.
//
// One FUSE mount per volume and node; NodePublishVolume then only bind mounts it,
// so all Pods of the node share a single FUSE process
// (docs/architecture.md §5.3).
func (s *nodeService) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	switch {
	case req.GetVolumeId() == "":
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	case req.GetStagingTargetPath() == "":
		return nil, status.Error(codes.InvalidArgument, "staging target path is required")
	}
	if err := validatePath("staging target path", req.GetStagingTargetPath()); err != nil {
		return nil, err
	}
	capability, err := validateNodeCapability(req.GetVolumeCapability())
	if err != nil {
		return nil, err
	}
	if cerr := ctxStatus(ctx); cerr != nil {
		return nil, cerr
	}

	id, err := decodeVolumeID(req.GetVolumeId())
	if err != nil {
		return nil, err
	}

	// The lock is taken after the validation: acquiring it first would report an
	// InvalidArgument as Aborted whenever the same volume happens to be busy.
	key := nodeStageLockPrefix + req.GetVolumeId()
	if !s.locks.tryAcquire(key) {
		return nil, status.Errorf(codes.Aborted, "an operation on %s is already in progress",
			echoVolumeID(req.GetVolumeId()))
	}
	defer s.locks.release(key)

	cluster, ok := s.cluster(id.ClusterID)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "cluster %q of volume %s is not configured",
			id.ClusterID, echoVolumeID(req.GetVolumeId()))
	}

	staging := req.GetStagingTargetPath()
	if err := s.ensureLogDir(); err != nil {
		return nil, err
	}
	// The CO is responsible for creating the staging directory, but creating it
	// as well is harmless and makes the plugin survive a CO that did not.
	if mkErr := s.ensureDir(staging, stagingDirMode); mkErr != nil {
		return nil, status.Errorf(codes.Internal, "create staging directory %q: %v", staging, mkErr)
	}

	spec := mounter.Spec{
		Servers:  cluster.Servers,
		Volume:   id.Volume,
		Subdir:   id.Subdir,
		ReadOnly: capability.readOnly,
		Options:  capability.mountFlags,
	}
	rec := stageRecord{
		Version:           stageRecordVersion,
		VolumeID:          req.GetVolumeId(),
		StagingTargetPath: staging,
		ReadOnly:          capability.readOnly,
		MountFlags:        capability.mountFlags,
		Targets:           s.previousTargets(req.GetVolumeId()),
	}
	if err := s.mountGlusterfs(ctx, spec, &rec); err != nil {
		return nil, err
	}

	// Bind targets of an interrupted recovery, or of the corrupted mount that
	// mountGlusterfs just replaced, still show the dead FUSE mount. They are
	// refreshed now rather than by the next recovery pass, which may be a
	// whole interval away or, with --recovery-interval=0, never come.
	if len(rec.Targets) > 0 {
		rec.Targets, _ = s.refreshTargets(ctx, rec, id)
	}

	// The record is what the recovery needs to find the volumes that are staged
	// on this node (D-20). It is written after the successful mount, so a stage
	// that failed leaves no record behind; Targets keeps only the bind targets
	// that could not be refreshed. A failure to write the record fails the
	// call, but the mount is kept: repeating the request is idempotent and only
	// rewrites the record.
	if s.records != nil {
		if saveErr := s.records.save(rec); saveErr != nil {
			return nil, status.Errorf(codes.Internal, "record staged volume %s: %v",
				echoVolumeID(req.GetVolumeId()), saveErr)
		}
	}

	// volume_context and publish_context are ignored on purpose: the volume_id is
	// the single source of truth, so a PV whose attributes drifted from its id
	// cannot stage a different volume. Secrets are ignored as well (a glusterfs
	// FUSE mount needs none) and are never logged.
	klog.V(2).InfoS("staged volume", "volumeID", req.GetVolumeId(), "stagingTargetPath", staging,
		"readOnly", capability.readOnly, "servers", cluster.Servers)

	return &csi.NodeStageVolumeResponse{}, nil
}

// mountGlusterfs performs the FUSE mount of rec and recovers once from a
// corrupted staging mount point.
//
// A target that is mounted but returns ENOTCONN means the FUSE process of a
// previous instance of this plugin died. Unmounting and mounting again is the
// only way out, and it is retried exactly once: a second failure is a real
// backend problem, and retrying it in a loop would only block the CO.
//
// The recovery follows the order of [nodeService.recoverVolume]: the bind
// targets of the dead mount are read from mountinfo and added to rec.Targets
// before the unmount removes the mountinfo entry that identifies them, and the
// record is saved before the unmount, so that a crash in between does not lose
// them. kubelet calls NodeStageVolume again for a staged volume after its own
// restart, so this path competes with the recovery for the same dead mounts.
func (s *nodeService) mountGlusterfs(ctx context.Context, spec mounter.Spec, rec *stageRecord) error {
	staging := rec.StagingTargetPath
	err := s.mounter.MountGlusterfs(ctx, spec, staging)
	if !errors.Is(err, mounter.ErrCorruptedMount) {
		return mountErrorToStatus("stage volume", err)
	}

	klog.InfoS("staging mount is corrupted, unmounting and mounting again",
		"volumeID", rec.VolumeID, "stagingTargetPath", staging)
	found, ferr := s.mounter.BindTargets(staging)
	if ferr != nil {
		// Not fatal: the volume can still be staged, only the Pods that use the
		// dead mount keep seeing ENOTCONN until they are restarted.
		klog.ErrorS(ferr, "cannot find the bind mounts of a corrupted staging mount, they are not refreshed",
			"volumeID", rec.VolumeID, "stagingTargetPath", staging)
	}
	rec.Targets = mergeTargets(rec.Targets, found)
	if s.records != nil && len(rec.Targets) > 0 {
		if serr := s.records.save(*rec); serr != nil {
			return status.Errorf(codes.Internal, "record the bind targets of volume %s: %v",
				echoVolumeID(rec.VolumeID), serr)
		}
	}

	if uerr := s.mounter.Unmount(ctx, staging); uerr != nil {
		return mountErrorToStatus("stage volume", uerr)
	}
	return mountErrorToStatus("stage volume", s.mounter.MountGlusterfs(ctx, spec, staging))
}

// previousTargets returns the bind targets an interrupted recovery left in the
// record of a volume. A missing or unreadable record has none: NodeStageVolume
// replaces it anyway.
func (s *nodeService) previousTargets(volumeID string) []recordTarget {
	if s.records == nil {
		return nil
	}
	rec, err := s.records.load(volumeID)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			klog.V(2).InfoS("ignoring an unreadable staging record", "volumeID", volumeID, "err", err)
		}
		return nil
	}
	return rec.Targets
}

// cluster returns the configured GlusterFS cluster of a decoded volume id.
//
// A driver without a configuration (only possible in controller mode, which
// never calls this) has no clusters, so the lookup reports "not found" instead of
// dereferencing a nil store.
func (s *nodeService) cluster(clusterID string) (config.Cluster, bool) {
	if s.driver.config == nil {
		return config.Cluster{}, false
	}
	return s.driver.config.Current().Get(clusterID)
}

// ensureLogDir creates the mount log directory.
//
// It is created here rather than at startup so that a start with a log directory
// whose parent is mounted later still works, and so that an unwritable directory
// is reported per request instead of preventing the plugin from starting at all.
func (s *nodeService) ensureLogDir() error {
	if s.logDir == "" {
		return nil
	}
	// #nosec G301 -- a log directory that an operator has to be able to read.
	if err := os.MkdirAll(s.logDir, mountLogDirMode); err != nil {
		return status.Errorf(codes.Internal, "create mount log directory %q: %v", s.logDir, err)
	}
	return nil
}

// ensureDir creates dir when nothing exists at that path yet.
//
// os.MkdirAll is not called on an existing path on purpose: on a corrupted FUSE
// mount point stat fails with ENOTCONN, MkdirAll would turn that into an error,
// and the mounter would never get the chance to detect the corrupted mount and
// recover from it. A corrupted mount point is therefore left to the mounter;
// any other stat failure is a real error.
func (s *nodeService) ensureDir(dir string, perm os.FileMode) error {
	_, err := s.stat(dir)
	switch {
	case err == nil, mount.IsCorruptedMnt(err):
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return os.MkdirAll(dir, perm)
	default:
		return err
	}
}

// NodeUnstageVolume unmounts the staging target path and keeps the directory.
//
// The cluster configuration is deliberately not consulted: a cluster that was
// removed from the ConfigMap must still be unmountable, otherwise decommissioning
// a cluster would leave a stale FUSE mount on every node.
func (s *nodeService) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	switch {
	case req.GetVolumeId() == "":
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	case req.GetStagingTargetPath() == "":
		return nil, status.Error(codes.InvalidArgument, "staging target path is required")
	}
	if err := validatePath("staging target path", req.GetStagingTargetPath()); err != nil {
		return nil, err
	}
	if _, err := decodeVolumeID(req.GetVolumeId()); err != nil {
		return nil, err
	}
	if err := ctxStatus(ctx); err != nil {
		return nil, err
	}

	key := nodeStageLockPrefix + req.GetVolumeId()
	if !s.locks.tryAcquire(key) {
		return nil, status.Errorf(codes.Aborted, "an operation on %s is already in progress",
			echoVolumeID(req.GetVolumeId()))
	}
	defer s.locks.release(key)

	// Unmount, not Cleanup: the staging directory belongs to kubelet.
	if err := s.mounter.Unmount(ctx, req.GetStagingTargetPath()); err != nil {
		return nil, mountErrorToStatus("unstage volume", err)
	}

	// The record is removed only after a successful unmount: as long as the mount
	// is there, the recovery must still know about it.
	if s.records != nil {
		if rmErr := s.records.remove(req.GetVolumeId()); rmErr != nil {
			return nil, status.Errorf(codes.Internal, "remove the record of staged volume %s: %v",
				echoVolumeID(req.GetVolumeId()), rmErr)
		}
	}

	klog.V(2).InfoS("unstaged volume", "volumeID", req.GetVolumeId(),
		"stagingTargetPath", req.GetStagingTargetPath())
	return &csi.NodeUnstageVolumeResponse{}, nil
}

// NodePublishVolume bind mounts the staged volume at the target path of a Pod.
func (s *nodeService) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	switch {
	case req.GetVolumeId() == "":
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	case req.GetTargetPath() == "":
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	case req.GetVolumeCapability() == nil:
		return nil, status.Error(codes.InvalidArgument, "volume capability is required")
	case req.GetStagingTargetPath() == "":
		// The CSI spec calls this case "Staging target path not set" and requires
		// FailedPrecondition: STAGE_UNSTAGE_VOLUME is advertised, so the CO has to
		// stage the volume before it publishes it.
		return nil, status.Error(codes.FailedPrecondition, "staging target path is required")
	}
	if err := validatePath("target path", req.GetTargetPath()); err != nil {
		return nil, err
	}
	if err := validatePath("staging target path", req.GetStagingTargetPath()); err != nil {
		return nil, err
	}
	capability, err := validateNodeCapability(req.GetVolumeCapability())
	if err != nil {
		return nil, err
	}
	if err := ctxStatus(ctx); err != nil {
		return nil, err
	}
	if _, err := decodeVolumeID(req.GetVolumeId()); err != nil {
		return nil, err
	}

	target, staging := req.GetTargetPath(), req.GetStagingTargetPath()
	key := nodePublishLockPrefix + target
	if !s.locks.tryAcquire(key) {
		return nil, status.Errorf(codes.Aborted, "an operation on %q is already in progress", target)
	}
	defer s.locks.release(key)

	if err := s.checkStaged(staging, req.GetVolumeId()); err != nil {
		return nil, err
	}
	if capability.singleTarget {
		vkey := nodePublishVolumeLockPrefix + req.GetVolumeId()
		if !s.locks.tryAcquire(vkey) {
			return nil, status.Errorf(codes.Aborted, "an operation on %s is already in progress",
				echoVolumeID(req.GetVolumeId()))
		}
		defer s.locks.release(vkey)

		if err := s.checkSingleTarget(staging, target, req.GetVolumeId()); err != nil {
			return nil, err
		}
	}

	// Either the CO asked for a read only mount or the access mode forbids
	// writing; the mount is read only when one of the two says so.
	readOnly := req.GetReadonly() || capability.readOnly

	// Creation of target_path is the responsibility of the SP.
	if mkErr := s.ensureDir(target, targetDirMode); mkErr != nil {
		return nil, status.Errorf(codes.Internal, "create target directory %q: %v", target, mkErr)
	}

	if err := s.bindMount(ctx, staging, target, readOnly); err != nil {
		return nil, err
	}

	// publish_context and volume_context are ignored for the same reason as in
	// NodeStageVolume: the paths carry everything the bind mount needs. Secrets
	// are neither used nor logged.
	klog.V(2).InfoS("published volume", "volumeID", req.GetVolumeId(), "targetPath", target,
		"stagingTargetPath", staging, "readOnly", readOnly)

	return &csi.NodePublishVolumeResponse{}, nil
}

// checkStaged verifies that the volume is staged at staging, i.e. that
// NodeStageVolume ran and its FUSE process is alive.
func (s *nodeService) checkStaged(staging, volumeID string) error {
	mounted, err := s.mounter.IsMountPoint(staging)
	switch {
	case errors.Is(err, os.ErrNotExist), err == nil && !mounted:
		return status.Errorf(codes.FailedPrecondition, "volume %s is not staged at %q",
			echoVolumeID(volumeID), staging)
	case errors.Is(err, mounter.ErrCorruptedMount):
		// Internal, not FailedPrecondition: the CO cannot fix a dead FUSE process
		// by staging again, the recovery of T-08 has to run first.
		return status.Errorf(codes.Internal, "staging mount of volume %s is corrupted: %v",
			echoVolumeID(volumeID), err)
	case err != nil:
		return status.Errorf(codes.Internal, "check whether %q is a mount point: %v", staging, err)
	}
	return nil
}

// checkSingleTarget verifies that a volume with a single target access mode is
// not published at a target path other than target yet.
//
// The mount table is the record of where the volume is published, so the check
// is stateless and still correct after a restart of the plugin. Publishing at
// target itself again is not refused here: it is the idempotent repetition, or
// a conflicting read-only flag that BindMount reports as AlreadyExists.
func (s *nodeService) checkSingleTarget(staging, target, volumeID string) error {
	others, err := s.mounter.OtherBindMounts(staging, target)
	switch {
	case err != nil:
		return status.Errorf(codes.Internal, "find other targets of volume %s: %v", echoVolumeID(volumeID), err)
	case len(others) > 0:
		return status.Errorf(codes.FailedPrecondition,
			"volume %s has a single node access mode and is already published at %q",
			echoVolumeID(volumeID), others[0])
	}
	return nil
}

// bindMount performs the bind mount and recovers once from a corrupted target
// mount point. It is the same recovery as in NodeStageVolume, for the case where
// the bind mount of a Pod outlived the FUSE process it was made from.
func (s *nodeService) bindMount(ctx context.Context, staging, target string, readOnly bool) error {
	err := s.mounter.BindMount(ctx, staging, target, readOnly)
	if !errors.Is(err, mounter.ErrCorruptedMount) {
		return mountErrorToStatus("publish volume", err)
	}

	klog.InfoS("target mount is corrupted, unmounting and bind mounting again",
		"targetPath", target, "stagingTargetPath", staging)
	if uerr := s.mounter.Unmount(ctx, target); uerr != nil {
		return mountErrorToStatus("publish volume", uerr)
	}
	return mountErrorToStatus("publish volume", s.mounter.BindMount(ctx, staging, target, readOnly))
}

// NodeUnpublishVolume unmounts the target path and deletes it.
//
// [mounter.Mounter.Cleanup] is used instead of Unmount because the CSI spec
// expects the plugin to delete the target_path it created.
func (s *nodeService) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	switch {
	case req.GetVolumeId() == "":
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	case req.GetTargetPath() == "":
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}
	if err := validatePath("target path", req.GetTargetPath()); err != nil {
		return nil, err
	}
	if _, err := decodeVolumeID(req.GetVolumeId()); err != nil {
		return nil, err
	}
	if err := ctxStatus(ctx); err != nil {
		return nil, err
	}

	target := req.GetTargetPath()
	key := nodePublishLockPrefix + target
	if !s.locks.tryAcquire(key) {
		return nil, status.Errorf(codes.Aborted, "an operation on %q is already in progress", target)
	}
	defer s.locks.release(key)

	if err := s.mounter.Cleanup(ctx, target); err != nil {
		return nil, mountErrorToStatus("unpublish volume", err)
	}

	klog.V(2).InfoS("unpublished volume", "volumeID", req.GetVolumeId(), "targetPath", target)
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// nodeRPCapabilities is the fixed capability list of NodeGetCapabilities. The
// order is part of the response, so it lives in one place.
//
// GET_VOLUME_STATS is advertised because NodeGetVolumeStats is implemented, and
// GET_VOLUME_HEALTH because of D-18: CSI v1.13 deleted VOLUME_CONDITION (both the
// capability value 4 and the volume_condition field of the statistics response
// are reserved) and replaced it with this alpha RPC, which is the only way to
// report a dead FUSE mount to the CO. SINGLE_NODE_MULTI_WRITER is advertised
// because several Pods on one node may use the same GlusterFS volume.
//
// EXPAND_VOLUME stays absent: resizing a GlusterFS volume happens on the
// controller side (quota or LV), the node never has to do anything for it.
var nodeRPCapabilities = []csi.NodeServiceCapability_RPC_Type{
	csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
	csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
	csi.NodeServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
	csi.NodeServiceCapability_RPC_GET_VOLUME_HEALTH,
}

// NodeGetCapabilities reports the node capabilities.
func (s *nodeService) NodeGetCapabilities(_ context.Context, _ *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	// An empty, non-nil slice for the same reason as in GetPluginCapabilities: a
	// nil slice is too easy to mistake for an uninitialised response.
	capabilities := make([]*csi.NodeServiceCapability, 0, len(nodeRPCapabilities))
	for _, t := range nodeRPCapabilities {
		capabilities = append(capabilities, &csi.NodeServiceCapability{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{Type: t},
			},
		})
	}
	return &csi.NodeGetCapabilitiesResponse{Capabilities: capabilities}, nil
}

// NodeGetInfo reports the node id.
//
// MaxVolumesPerNode is 0, which tells the CO to decide: GlusterFS has no per node
// volume limit of its own. No accessible topology is returned, because every node
// can reach every cluster and VOLUME_ACCESSIBILITY_CONSTRAINTS is not advertised
// (docs/architecture.md §5.4).
func (s *nodeService) NodeGetInfo(_ context.Context, _ *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{
		NodeId:            s.driver.nodeID,
		MaxVolumesPerNode: 0,
	}, nil
}

// newMounter returns the production [mounter.Mounter] of a driver.
//
// mount.New("") only records the mount helper path and performs no I/O, so it can
// be built in [New] before any RPC arrives.
func newMounter(logDir string) mounter.Mounter {
	return mounter.New(mount.New(""), logDir)
}
