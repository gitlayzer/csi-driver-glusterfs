package driver

import (
	"context"
	"fmt"

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
	// UnimplementedControllerServer keeps CreateVolume, DeleteVolume and every
	// other RPC this milestone does not implement (yet) at Unimplemented
	// instead of answering them with a nil error. CreateVolume and DeleteVolume
	// arrive with T-11.
	csi.UnimplementedControllerServer

	driver *Driver
	// locks rejects concurrent operations on the same volume name or id; the
	// RPCs that need it arrive with T-11.
	locks lockTable
}

// controllerRPCapabilities is the fixed capability list of
// ControllerGetCapabilities. The order is part of the response, so it lives in
// one place.
//
// CREATE_DELETE_VOLUME is deliberately absent: announcing a capability before
// CreateVolume and DeleteVolume exist would make the external-provisioner call
// RPCs that answer Unimplemented, which leaves a PVC in a failed state instead
// of being served by another driver. It is added together with those two RPCs
// in T-11 (D-23).
//
// SINGLE_NODE_MULTI_WRITER is advertised because the node service already
// accepts several Pods on one node using the same GlusterFS volume, and the
// controller is the side that has to tell the CO about it.
//
// EXPAND_VOLUME, GET_CAPACITY, CREATE_DELETE_SNAPSHOT and CLONE_VOLUME stay
// absent for the same reason: they arrive with M3 and M4.
var controllerRPCapabilities = []csi.ControllerServiceCapability_RPC_Type{
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
// The RPC is read only: it modifies nothing, so it takes no lock. It also does
// not talk to GlusterFS. A volume exists when its volume_id decodes and its
// cluster is configured; the live directory check arrives with T-11, once the
// controller can mount the parent volume (D-23).
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
