package driver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"k8s.io/klog/v2"

	"github.com/gitlayzer/csi-driver-glusterfs/pkg/config"
	"github.com/gitlayzer/csi-driver-glusterfs/pkg/mounter"
	"github.com/gitlayzer/csi-driver-glusterfs/pkg/volumeid"
)

// Volume attribute keys of a PersistentVolume. They are the keys of
// CreateVolumeResponse.Volume.VolumeContext (T-11) and the keys the CO may echo
// back in ValidateVolumeCapabilitiesRequest.VolumeContext.
const (
	// attrClusterID is the clusterID the volume lives in.
	attrClusterID = paramClusterID
	// attrVolume is the GlusterFS volume of the volume.
	attrVolume = "volume"
	// attrSubdir is the subdirectory of the GlusterFS volume, present for a
	// subdir volume only.
	attrSubdir = "subdir"
)

// controllerService implements csi.ControllerServer. Like [nodeService] it is
// a field of [Driver], not embedded: both UnimplementedControllerServer and
// UnimplementedNodeServer provide testEmbeddedByValue, so embedding the two in
// one type would make every selector on them ambiguous.
type controllerService struct {
	// UnimplementedControllerServer keeps ControllerExpandVolume, GetCapacity,
	// the snapshot RPCs and every RPC added by a later version of the protocol
	// Unimplemented instead of answering them with a nil error. CreateVolume,
	// DeleteVolume and ControllerGetCapabilities are implemented in this file.
	csi.UnimplementedControllerServer

	driver *Driver
	// locks rejects concurrent operations on the same volume id, so that two
	// CreateVolume or DeleteVolume calls for one volume serialize instead of
	// racing on the same directory (docs/architecture.md §5.5).
	locks lockTable
	// parents mounts the parent volumes and hands out the roots the directory and
	// metadata work goes through (D-24).
	parents *parentMounts
	// now is the clock of the timestamps an archived volume directory gets (D-26).
	// It is a field so that a test can archive with a fixed time.
	now func() time.Time
}

// nowTime returns the current time of the controller service, from now when it is set.
func (s *controllerService) nowTime() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

// controllerRPCapabilities is the fixed capability list of
// ControllerGetCapabilities. The order is part of the response, so it lives in
// one place.
//
// CREATE_DELETE_VOLUME is advertised because CreateVolume and DeleteVolume are
// implemented (D-23). SINGLE_NODE_MULTI_WRITER is advertised because the node
// service already accepts several Pods on one node using the same GlusterFS
// volume, and the controller is the side that has to tell the CO about it.
//
// EXPAND_VOLUME, GET_CAPACITY, CREATE_DELETE_SNAPSHOT and CLONE_VOLUME stay
// absent: announcing a capability before its RPCs exist would make the CO call
// methods that answer Unimplemented. They arrive with M3 and M4.
var controllerRPCapabilities = []csi.ControllerServiceCapability_RPC_Type{
	csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
	csi.ControllerServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
}

// ControllerGetCapabilities reports the controller capabilities.
func (s *controllerService) ControllerGetCapabilities(_ context.Context, _ *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	// An empty, non-nil slice for the same reason as in GetPluginCapabilities: a
	// nil slice is too easy to mistake for an uninitialised response.
	capabilities := make([]*csi.ControllerServiceCapability, 0, len(controllerRPCapabilities))
	for _, t := range controllerRPCapabilities {
		capabilities = append(capabilities, &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{Type: t},
			},
		})
	}
	return &csi.ControllerGetCapabilitiesResponse{Capabilities: capabilities}, nil
}

// ValidateVolumeCapabilities reports whether the driver supports a set of
// capabilities, the parameters and the volume context of an existing volume.
//
// The RPC is read only: it modifies nothing, so it takes no lock. A volume
// exists when its volume_id decodes, its cluster is configured and, for a subdir
// volume, its directory is really there in the parent volume (D-23). The
// directory check is why the controller has to mount the parent volume; a static
// volume is not checked on the backend, because the controller never touches one.
//
// An unsupported but well formed request is not an error: the CSI spec requires
// OK with Confirmed unset and a message, so that a CO can tell "this driver
// cannot do it" from "the request was broken". Only a malformed request
// (InvalidArgument) and an unknown volume (NotFound) are errors.
func (s *controllerService) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	// The request is validated completely before the context and the volume are
	// looked at: a malformed capability is an error about the request, and
	// reporting it as NotFound because the volume happens to be unknown would
	// hide a broken CO.
	switch {
	case req.GetVolumeId() == "":
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	case len(req.GetVolumeCapabilities()) == 0:
		return nil, status.Error(codes.InvalidArgument, "volume capabilities are required")
	}

	// reasons collects, in the order of the request, why the request is not
	// supported. The first of them is reported in the response, all of them in
	// the log, so that a CO or an administrator sees every problem at once
	// instead of one per retry.
	var reasons []string
	for i, capability := range req.GetVolumeCapabilities() {
		supported, reason, err := checkControllerCapability(capability)
		if err != nil {
			return nil, err
		}
		if !supported {
			// Indexed, so that a request with several capabilities says which
			// one was refused.
			reasons = append(reasons, fmt.Sprintf("capability %d: %s", i, reason))
		}
	}

	if err := ctxStatus(ctx); err != nil {
		return nil, err
	}

	id, err := decodeVolumeID(req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	cluster, ok := s.cluster(id.ClusterID)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "volume %s not found: cluster %q is not configured",
			echoVolumeID(req.GetVolumeId()), id.ClusterID)
	}

	// The existence check runs before the reasons are collected: a volume that is
	// not there is NotFound whatever its other problems are, and the CO uses that
	// answer to decide whether it has to create the volume again.
	if id.Mode == volumeid.ModeSubdir {
		if err := s.checkSubdirVolumeExists(ctx, cluster, id, req.GetVolumeId()); err != nil {
			return nil, err
		}
	}

	reasons = append(reasons, s.checkMountFlags(req.GetVolumeCapabilities(), cluster, id)...)
	if reason := s.checkParameters(req.GetParameters(), id); reason != "" {
		reasons = append(reasons, reason)
	}
	if len(req.GetMutableParameters()) > 0 {
		// MODIFY_VOLUME is not advertised: accepting mutable parameters would
		// promise that a later change of them is applied.
		reasons = append(reasons, "mutable parameters are not supported")
	}
	reasons = append(reasons, s.checkVolumeContext(req.GetVolumeContext(), id)...)

	if len(reasons) > 0 {
		// The CSI spec puts the first unsupported reason into Message; the whole
		// list goes to the log, where an administrator can see all of them at
		// once. The interceptors log the RPC and its status code too, but not the
		// reason, which is why it is logged here.
		klog.V(4).InfoS("volume capabilities are not confirmed",
			"volumeID", req.GetVolumeId(), "reason", reasons[0], "reasons", reasons)
		return &csi.ValidateVolumeCapabilitiesResponse{Message: reasons[0]}, nil
	}

	return &csi.ValidateVolumeCapabilitiesResponse{Confirmed: confirmedResponse(req)}, nil
}

// checkMountFlags reports whether every mount capability of req has mount flags
// the node service would accept.
//
// The flags are validated by building the mount arguments of the volume, which
// is exactly what NodeStageVolume does: a mountOptions list that the node
// rejects then fails the PVC here, while the StorageClass is still being
// resolved, instead of when a Pod is first scheduled.
func (s *controllerService) checkMountFlags(capabilities []*csi.VolumeCapability, cluster config.Cluster, id volumeid.ID) []string {
	var reasons []string
	for i, capability := range capabilities {
		flags := capability.GetMount().GetMountFlags()
		if len(flags) == 0 {
			continue
		}
		spec := mounter.Spec{
			Servers: cluster.Servers,
			Volume:  id.Volume,
			Subdir:  id.Subdir,
			Options: flags,
		}
		if _, _, err := mounter.BuildMountArgs(spec, ""); err != nil {
			reasons = append(reasons, fmt.Sprintf("capability %d: mount flags are not supported: %v", i, err))
		}
	}
	return reasons
}

// checkParameters reports whether req.Parameters describes id.
//
// An empty parameter set is not a mismatch: the CO may check the capabilities
// of a volume without repeating the StorageClass. A set that does not parse is
// a mismatch too, not an error: the volume exists, it just was not created with
// these parameters.
func (s *controllerService) checkParameters(params map[string]string, id volumeid.ID) string {
	if len(params) == 0 {
		return ""
	}
	parsed, err := parseVolumeParameters(params)
	if err != nil {
		return fmt.Sprintf("parameters are not supported: %v", err)
	}
	// The volume id is the source of truth for the mode, the cluster and the
	// parent volume, so a parameter set that disagrees with it describes a
	// different volume.
	switch {
	case parsed.Mode != id.Mode:
		return fmt.Sprintf("parameters request mode %q, but the volume is a %q volume",
			string(parsed.Mode), string(id.Mode))
	case parsed.ClusterID != id.ClusterID:
		return fmt.Sprintf("parameters request cluster %q, but the volume is in cluster %q",
			parsed.ClusterID, id.ClusterID)
	case id.Mode == volumeid.ModeSubdir && parsed.ParentVolume != id.Volume:
		return fmt.Sprintf("parameters request parent volume %q, but the volume is a subdirectory of %q",
			parsed.ParentVolume, id.Volume)
	}
	return ""
}

// checkVolumeContext reports whether the volume context of req agrees with id.
//
// Only the three keys this driver writes into the volume context of a volume
// are compared. The volume_context is information for the node service, which
// takes everything it needs from the volume_id instead, so an unknown key says
// nothing about whether the request is supported and is ignored.
func (s *controllerService) checkVolumeContext(volumeContext map[string]string, id volumeid.ID) []string {
	// The keys are checked in a fixed order rather than by ranging over the map,
	// so that the message of the response is deterministic. A volume without a
	// subdirectory must not claim one; an empty value is the same as no value at
	// all.
	expected := []struct{ key, want string }{
		{attrClusterID, id.ClusterID},
		{attrVolume, id.Volume},
		{attrSubdir, id.Subdir},
	}

	var reasons []string
	for _, e := range expected {
		value, ok := volumeContext[e.key]
		if !ok || value == e.want {
			continue
		}
		reasons = append(reasons, fmt.Sprintf("volume context %s %s does not match the volume (%s)",
			e.key, echoParamValue(value), echoParamValue(e.want)))
	}
	return reasons
}

// confirmedResponse builds the Confirmed part of a successful response.
//
// Nothing in the response shares memory with the request: the request belongs to
// the gRPC layer, which may reuse or mutate it once the handler returns, so a
// shallow copy of the capability slice would not do.
func confirmedResponse(req *csi.ValidateVolumeCapabilitiesRequest) *csi.ValidateVolumeCapabilitiesResponse_Confirmed {
	confirmed := &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
		VolumeCapabilities: copyVolumeCapabilities(req.GetVolumeCapabilities()),
	}
	if volumeContext := req.GetVolumeContext(); len(volumeContext) > 0 {
		confirmed.VolumeContext = copyStringMap(volumeContext)
	}
	if parameters := req.GetParameters(); len(parameters) > 0 {
		confirmed.Parameters = copyStringMap(parameters)
	}
	if mutable := req.GetMutableParameters(); len(mutable) > 0 {
		confirmed.MutableParameters = copyStringMap(mutable)
	}
	return confirmed
}

// copyVolumeCapabilities deep copies the capabilities of a request, for the same
// reason as copyStringMap: the response must not alias the request.
func copyVolumeCapabilities(capabilities []*csi.VolumeCapability) []*csi.VolumeCapability {
	if len(capabilities) == 0 {
		return nil
	}
	out := make([]*csi.VolumeCapability, 0, len(capabilities))
	for _, capability := range capabilities {
		if capability == nil {
			out = append(out, nil)
			continue
		}
		out = append(out, proto.CloneOf(capability))
	}
	return out
}

// copyStringMap returns a copy of m, or nil for an empty map.
//
// A nil map is what an empty request field reads as: the response then
// confirms exactly the fields the CO sent.
func copyStringMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for key, value := range m {
		out[key] = value
	}
	return out
}

// cluster returns the configured GlusterFS cluster of a decoded volume id.
//
// A driver without a configuration has no clusters, so the lookup reports "not
// found" instead of dereferencing a nil store. That case is only reachable for
// a Driver built by hand: New rejects a controller mode without a
// configuration.
func (s *controllerService) cluster(clusterID string) (config.Cluster, bool) {
	if s.driver == nil || s.driver.config == nil {
		return config.Cluster{}, false
	}
	return s.driver.config.Current().Get(clusterID)
}

// backendError maps a failure of the parent volume mount or of the file system
// below it to a gRPC status.
//
// A cancelled or expired ctx is reported as such instead of Internal: the CO gave
// up, the operation did not fail on its own. Everything else is Internal with a
// short cause and without any secret, like [mountErrorToStatus] for the node
// service.
func backendError(ctx context.Context, op string, err error) error {
	if ctx.Err() != nil {
		return ctxStatus(ctx)
	}
	return status.Errorf(codes.Internal, "%s: %v", op, err)
}

// CreateVolume provisions a subdirectory of a parent volume (D-22, D-25).
//
// The volume is a directory named after the request; when the name is too long to
// fit into a volume_id (the CSI limit of 128 bytes is shared by the name and the
// id), the directory is named after the derived name of [volumeid.SubdirName]
// instead (D-30). Its metadata is written before the directory is created, and the
// parent volume is mounted by the controller and cached (D-24). Capacity is
// recorded, not enforced: a subdirectory of a shared GlusterFS volume has no size
// of its own until quota is implemented in M3, so a request with no capacity range
// is served with capacity 0 ("unknown") and a repeated request with a larger one is
// refused as AlreadyExists instead of silently promising space that does not exist.
//
// The write order is what makes the RPC idempotent and crash safe: a metadata file
// without a directory is repaired by the next call, while a directory without a
// metadata file is not this driver's volume and is never touched (D-25).
func (s *controllerService) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	name := req.GetName()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "volume name is required")
	}
	if err := volumeid.ValidateName("name", name); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid volume name: %v", err)
	}
	// The metadata and the archive live in .glusterfs-csi, so a volume whose name
	// starts with "." could shadow them.
	if strings.HasPrefix(name, ".") {
		return nil, status.Errorf(codes.InvalidArgument,
			"invalid volume name %q: names starting with %q are reserved", name, ".")
	}

	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities are required")
	}
	for i, capability := range req.GetVolumeCapabilities() {
		supported, reason, err := checkControllerCapability(capability)
		if err != nil {
			return nil, err
		}
		if !supported {
			// Indexed, so that a request with several capabilities says which one
			// was refused.
			return nil, status.Errorf(codes.InvalidArgument, "capability %d: %s", i, reason)
		}
	}

	capacity, err := requestedCapacity(req.GetCapacityRange())
	if err != nil {
		return nil, err
	}
	if req.GetVolumeContentSource() != nil {
		// CREATE_DELETE_SNAPSHOT and CLONE_VOLUME are not advertised: a subdirectory
		// cannot be snapshotted, and honoring the request would produce an empty
		// volume that the CO believes to be pre-populated.
		return nil, status.Error(codes.InvalidArgument, "volume content source is not supported")
	}
	if len(req.GetMutableParameters()) > 0 {
		// MODIFY_VOLUME is not advertised, so mutable parameters cannot be promised.
		return nil, status.Error(codes.InvalidArgument, "mutable parameters are not supported")
	}
	// accessibility_requirements are ignored: VOLUME_ACCESSIBILITY_CONSTRAINTS is
	// not advertised, every node reaches every cluster. The secrets of the request
	// are neither used (a glusterfs FUSE mount needs none) nor logged.

	params, err := parseVolumeParameters(req.GetParameters())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	cluster, ok := s.cluster(params.ClusterID)
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "cluster %q is not configured", params.ClusterID)
	}

	// The directory name is derived before the parent volume is mounted: a name that
	// cannot be turned into a volume id is a broken request, and a broken request
	// must not cost a mount.
	subdir, err := volumeid.SubdirName(params.ClusterID, params.ParentVolume, name)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "cannot derive a directory name for volume %q: %v", name, err)
	}
	if subdir != name {
		// The CO sees the id, not the directory: say why the two differ.
		klog.InfoS("volume name does not fit into a volume id, using a derived directory name",
			"name", name, "subdir", subdir)
	}

	id := volumeid.ID{
		Mode:      volumeid.ModeSubdir,
		ClusterID: params.ClusterID,
		Volume:    params.ParentVolume,
		Subdir:    subdir,
	}
	encoded, err := volumeid.Encode(id)
	if err != nil {
		// Defensive only: SubdirName already chose a name that fits, so the encoded
		// form cannot be too long any more.
		return nil, status.Errorf(codes.InvalidArgument, "cannot encode the volume id: %v", err)
	}

	if reasons := s.checkMountFlags(req.GetVolumeCapabilities(), cluster, id); len(reasons) > 0 {
		return nil, status.Errorf(codes.InvalidArgument, "%s", reasons[0])
	}

	if ctxErr := ctxStatus(ctx); ctxErr != nil {
		return nil, ctxErr
	}

	// The lock is taken after the validation: acquiring it first would report an
	// InvalidArgument as Aborted whenever the same volume happens to be busy.
	if !s.locks.tryAcquire(encoded) {
		return nil, status.Errorf(codes.Aborted, "an operation on %s is already in progress", echoVolumeID(encoded))
	}
	defer s.locks.release(encoded)

	root, err := s.parentRoot(ctx, cluster, id.Volume)
	if err != nil {
		return nil, err
	}

	meta, found, err := readVolumeMeta(root, id.Subdir)
	if err != nil {
		return nil, backendError(ctx, "read the volume metadata", err)
	}

	if !found {
		exists, existsErr := volumeDirExists(root, id.Subdir)
		if existsErr != nil {
			return nil, backendError(ctx, "look for the volume directory", existsErr)
		}
		if exists {
			// A directory without metadata was not created by this driver: adopting
			// it would mean overwriting somebody's data, so it is refused and left
			// untouched (D-25).
			return nil, status.Errorf(codes.AlreadyExists,
				"directory %q exists in parent volume %q but was not created by this driver", id.Subdir, id.Volume)
		}

		// Metadata first: a crash in between leaves a volume that the next call
		// completes, whereas the reverse order would leave a directory DeleteVolume
		// refuses to touch.
		meta = volumeMeta{Version: metaVersion, CapacityBytes: capacity, OnDelete: params.OnDelete}
		if wErr := writeVolumeMeta(root, id.Subdir, meta); wErr != nil {
			return nil, backendError(ctx, "write the volume metadata", wErr)
		}
		if dErr := ensureVolumeDir(root, id.Subdir); dErr != nil {
			return nil, backendError(ctx, "create the volume directory", dErr)
		}
		klog.InfoS("created volume", "volumeID", encoded, "parentVolume", id.Volume,
			"clusterID", id.ClusterID, "name", name, "subdir", id.Subdir,
			"capacityBytes", capacity, "onDelete", string(meta.OnDelete))
		return createVolumeResponse(encoded, id, meta), nil
	}

	// The volume exists. The request is served again only when it is compatible
	// with what is recorded, which is what makes a retried CreateVolume idempotent
	// without ever changing a volume behind the back of its PVC.
	if reason := metaMismatch(meta, params.OnDelete, req.GetCapacityRange()); reason != "" {
		// The reason names the recorded value and the requested one, so that an
		// administrator sees which part of the StorageClass or of the PVC differs.
		return nil, status.Errorf(codes.AlreadyExists,
			"volume %q already exists with onDelete %q and capacity %d, which does not satisfy the request: %s",
			id.Subdir, string(meta.OnDelete), meta.CapacityBytes, reason)
	}
	// The Chmod repairs the permission of a volume created under a strict umask,
	// and the directory itself repairs the crash between the metadata write and the
	// Mkdir above.
	if dErr := ensureVolumeDir(root, id.Subdir); dErr != nil {
		return nil, backendError(ctx, "create the volume directory", dErr)
	}
	klog.V(4).InfoS("volume already exists, returning it", "volumeID", encoded,
		"parentVolume", id.Volume, "name", name, "subdir", id.Subdir, "capacityBytes", meta.CapacityBytes)
	return createVolumeResponse(encoded, id, meta), nil
}

// requestedCapacity returns the capacity a request asks for.
//
// required_bytes is the capacity that is recorded, unless it is 0: a CO that only
// sends a limit gets that limit, and a request without any capacity range is served
// with 0, which the CSI spec defines as "unknown". A negative value is malformed,
// and a required capacity above the limit cannot be satisfied.
func requestedCapacity(r *csi.CapacityRange) (int64, error) {
	required, limit := r.GetRequiredBytes(), r.GetLimitBytes()
	switch {
	case required < 0:
		return 0, status.Errorf(codes.InvalidArgument, "required capacity %d must not be negative", required)
	case limit < 0:
		return 0, status.Errorf(codes.InvalidArgument, "capacity limit %d must not be negative", limit)
	case limit > 0 && required > limit:
		return 0, status.Errorf(codes.InvalidArgument,
			"required capacity %d exceeds the limit %d", required, limit)
	}
	if required > 0 {
		return required, nil
	}
	return limit, nil
}

// metaMismatch reports why a recorded volume does not satisfy a request, or "" when
// it does.
//
// The delete policy has to match exactly: it decides what happens to the data, so a
// second StorageClass that asks for another policy describes another volume. The
// capacity comparison follows the CSI spec - the recorded capacity has to satisfy
// the range - and a range that does not mention a bound does not constrain it.
func metaMismatch(meta volumeMeta, policy deletePolicy, r *csi.CapacityRange) string {
	if meta.OnDelete != policy {
		return fmt.Sprintf("onDelete %q was requested, the volume uses %q", string(policy), string(meta.OnDelete))
	}
	if required := r.GetRequiredBytes(); required > 0 && meta.CapacityBytes < required {
		return fmt.Sprintf("capacity %d is below the required %d", meta.CapacityBytes, required)
	}
	if limit := r.GetLimitBytes(); limit > 0 && meta.CapacityBytes > limit {
		return fmt.Sprintf("capacity %d is above the limit %d", meta.CapacityBytes, limit)
	}
	return ""
}

// createVolumeResponse builds the response of a successful CreateVolume.
//
// Nothing but the volume id, the capacity and the three volume context keys is set:
// the CO caches them in the PersistentVolume, so an empty field is better than one
// that is guessed.
func createVolumeResponse(encoded string, id volumeid.ID, meta volumeMeta) *csi.CreateVolumeResponse {
	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      encoded,
			CapacityBytes: meta.CapacityBytes,
			VolumeContext: map[string]string{
				attrClusterID: id.ClusterID,
				attrVolume:    id.Volume,
				attrSubdir:    id.Subdir,
			},
		},
	}
}

// DeleteVolume removes the directory of a subdir volume according to the delete
// policy that CreateVolume recorded (D-25, docs/architecture.md §5.6).
//
// Unlike the other RPCs it succeeds for a volume it does not know: a volume_id that
// does not decode, a static volume (which this driver never created) and a missing
// volume are all reported as success, because the CO is deleting a volume that is
// already gone. A cluster missing from the configuration is the one exception: it is
// FailedPrecondition, because silently answering OK would delete the
// PersistentVolume while the data stays on the backend.
//
// The directory is handled before the metadata is removed, so a failure in between
// leaves a volume that the next call finishes; removing the metadata first would
// turn the directory into one this driver refuses to touch.
func (s *controllerService) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}

	id, err := volumeid.Decode(volumeID)
	if err != nil {
		// An id this driver never issued is a volume that does not exist, and
		// deleting a volume that does not exist is success (docs/architecture.md
		// §5.5).
		klog.V(4).InfoS("deleting a volume with an undecodable id, nothing to do", "volumeID", echoVolumeID(volumeID))
		return &csi.DeleteVolumeResponse{}, nil
	}
	if id.Mode != volumeid.ModeSubdir {
		// A static volume is an administrator managed directory this driver must not
		// delete; a volume mode volume does not exist in M2.
		klog.V(2).InfoS("not deleting a volume this driver did not create", "volumeID", echoVolumeID(volumeID),
			"mode", string(id.Mode))
		return &csi.DeleteVolumeResponse{}, nil
	}

	cluster, ok := s.cluster(id.ClusterID)
	if !ok {
		return nil, status.Errorf(codes.FailedPrecondition,
			"cluster %q of volume %s is not configured", id.ClusterID, echoVolumeID(volumeID))
	}

	if ctxErr := ctxStatus(ctx); ctxErr != nil {
		return nil, ctxErr
	}

	if !s.locks.tryAcquire(volumeID) {
		return nil, status.Errorf(codes.Aborted, "an operation on %s is already in progress", echoVolumeID(volumeID))
	}
	defer s.locks.release(volumeID)

	root, err := s.parentRoot(ctx, cluster, id.Volume)
	if err != nil {
		return nil, err
	}

	// Both reads happen before anything is changed: a failure here must not remove
	// half of the volume.
	meta, found, err := readVolumeMeta(root, id.Subdir)
	if err != nil {
		return nil, backendError(ctx, "read the volume metadata", err)
	}
	exists, err := volumeDirExists(root, id.Subdir)
	if err != nil {
		return nil, backendError(ctx, "look for the volume directory", err)
	}

	if !found {
		if exists {
			// Not this driver's volume: kept, and reported so that an administrator
			// can see why the data of a deleted PVC is still there (D-25).
			klog.InfoS("keeping a directory that was not created by this driver",
				"volumeID", volumeID, "parentVolume", id.Volume, "subdir", id.Subdir)
		} else {
			klog.V(4).InfoS("volume does not exist, nothing to delete", "volumeID", volumeID)
		}
		return &csi.DeleteVolumeResponse{}, nil
	}

	switch meta.OnDelete {
	case deletePolicyDelete:
		if dErr := removeVolumeDir(ctx, root, id.Subdir); dErr != nil {
			// The metadata is kept, so a retried DeleteVolume continues where this
			// one stopped.
			return nil, backendError(ctx, "remove the volume directory", dErr)
		}
	case deletePolicyRetain:
		// The directory is the administrator's from now on: only the metadata goes
		// away, and with it the driver's claim to the name.
	case deletePolicyArchive:
		if exists {
			if aErr := archiveVolumeDir(root, id.Subdir, s.nowTime()); aErr != nil {
				return nil, backendError(ctx, "archive the volume directory", aErr)
			}
		}
	default:
		// Unreachable: readVolumeMeta rejects an unknown policy.
		return nil, status.Errorf(codes.Internal, "volume %s has an unsupported delete policy %q",
			echoVolumeID(volumeID), string(meta.OnDelete))
	}

	if rErr := removeVolumeMeta(root, id.Subdir); rErr != nil {
		return nil, backendError(ctx, "remove the volume metadata", rErr)
	}

	klog.InfoS("deleted volume", "volumeID", volumeID, "parentVolume", id.Volume,
		"subdir", id.Subdir, "onDelete", string(meta.OnDelete))
	return &csi.DeleteVolumeResponse{}, nil
}

// checkSubdirVolumeExists reports NotFound unless both the metadata and the
// directory of a subdir volume exist in its parent volume.
//
// ValidateVolumeCapabilities uses it to answer the CO truthfully after a restart of
// the controller, when an in-memory record would be gone (D-23). It reads the parent
// volume instead of a record so that the answer survives that restart.
func (s *controllerService) checkSubdirVolumeExists(ctx context.Context, cluster config.Cluster, id volumeid.ID, volumeID string) error {
	root, err := s.parentRoot(ctx, cluster, id.Volume)
	if err != nil {
		return err
	}
	_, found, err := readVolumeMeta(root, id.Subdir)
	if err != nil {
		return backendError(ctx, "read the volume metadata", err)
	}
	exists, err := volumeDirExists(root, id.Subdir)
	if err != nil {
		return backendError(ctx, "look for the volume directory", err)
	}
	// Both are required, and both are needed to tell "this driver created the volume"
	// from "a directory somebody else made": CreateVolume writes the metadata before
	// the directory, so a volume of this driver always has both (D-25).
	if !found || !exists {
		return status.Errorf(codes.NotFound, "volume %s not found: %q is not in parent volume %q",
			echoVolumeID(volumeID), id.Subdir, id.Volume)
	}
	return nil
}

// parentRoot returns the root of a parent volume.
//
// A controller service built by hand may have no parent volume cache; an RPC then
// reports Internal instead of dereferencing a nil pointer. New never produces that
// state in controller mode, where --controller-mount-dir is required.
func (s *controllerService) parentRoot(ctx context.Context, cluster config.Cluster, volume string) (*os.Root, error) {
	if s.parents == nil {
		return nil, backendError(ctx, "mount parent volume", errors.New("no parent volume cache"))
	}
	root, err := s.parents.root(ctx, cluster, volume)
	if err != nil {
		return nil, backendError(ctx, "mount parent volume", err)
	}
	return root, nil
}
