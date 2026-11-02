package driver

import (
	"fmt"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/gitlayzer/csi-driver-glusterfs/pkg/mounter"
)

// supportedAccessModes are the VolumeCapability access modes the driver
// accepts, i.e. every mode of csi.proto except UNKNOWN. Both the node and the
// controller service use the table, through checkCapabilityShape.
//
// GlusterFS volumes are natively read-write-many, so no mode has to be refused
// for backend reasons: the single node restriction of the SINGLE_NODE_* modes
// and the single writer restriction of MULTI_NODE_SINGLE_WRITER are enforced by
// the CO. The one restriction the plugin enforces is the single target path of
// singleTargetAccessModes. The list exists to reject values that are not
// defined in the protocol at all (for example 99), which a broken CO could send
// and which must not be mistaken for a writeable mode.
var supportedAccessModes = map[csi.VolumeCapability_AccessMode_Mode]struct{}{
	csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER:        {},
	csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY:   {},
	csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY:    {},
	csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER:  {},
	csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER:   {},
	csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER: {},
	csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER:  {},
}

// readerOnlyAccessModes are the access modes that mean "never written". They
// make the node service mount the volume read only, so that a CO that
// mistakenly schedules a writer fails at the filesystem level instead of
// corrupting data.
var readerOnlyAccessModes = map[csi.VolumeCapability_AccessMode_Mode]struct{}{
	csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY: {},
	csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY:  {},
}

// singleTargetAccessModes are the access modes whose volume may be published at
// one target path per node only.
//
// The CSI spec (NodePublishVolume errors, for a plugin that advertises
// SINGLE_NODE_MULTI_WRITER) requires FailedPrecondition when a volume of
// SINGLE_NODE_SINGLE_WRITER or of a non MULTI_NODE mode is published at a
// second target path. SINGLE_NODE_MULTI_WRITER is the mode that allows several
// targets, which Kubernetes uses for ReadWriteOnce once the capability is
// advertised; MULTI_NODE_* volumes may be published anywhere.
var singleTargetAccessModes = map[csi.VolumeCapability_AccessMode_Mode]struct{}{
	csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER:        {},
	csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY:   {},
	csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER: {},
}

// nodeCapability is what a validated VolumeCapability means for the node
// service.
type nodeCapability struct {
	// readOnly is true when the access mode forbids writing.
	readOnly bool
	// singleTarget is true when the volume may be published at one target
	// path of the node only.
	singleTarget bool
	// mountFlags is VolumeCapability.Mount.MountFlags, passed to the mounter
	// unchanged; the mounter validates every fragment against its allowlist.
	mountFlags []string
}

// capabilityShape is a VolumeCapability whose malformed parts have been
// rejected, classified by access type.
type capabilityShape struct {
	// mode is the access mode. It is always one of supportedAccessModes.
	mode csi.VolumeCapability_AccessMode_Mode
	// block is true when the requested access type is Block.
	block bool
	// mount is the mount access type, nil for a Block capability.
	mount *csi.VolumeCapability_MountVolume
}

// checkCapabilityShape rejects the parts of a VolumeCapability that are
// malformed for the Controller and for the Node service alike: a missing
// access mode, an access mode the protocol does not define and a missing
// access type.
//
// Whether an access type is supported depends on the service: the Controller
// answers InvalidArgument for Block, the Node service FailedPrecondition
// (D-16). The shape therefore only classifies the access type and lets the
// caller decide, so that the classification exists exactly once.
func checkCapabilityShape(c *csi.VolumeCapability) (capabilityShape, error) {
	if c == nil {
		return capabilityShape{}, status.Error(codes.InvalidArgument, "volume capability is required")
	}

	mode := c.GetAccessMode()
	if mode == nil {
		return capabilityShape{}, status.Error(codes.InvalidArgument, "volume capability access mode is required")
	}
	if mode.GetMode() == csi.VolumeCapability_AccessMode_UNKNOWN {
		return capabilityShape{}, status.Errorf(codes.InvalidArgument,
			"volume capability access mode %s is not supported", mode.GetMode().String())
	}
	if _, ok := supportedAccessModes[mode.GetMode()]; !ok {
		return capabilityShape{}, status.Errorf(codes.InvalidArgument,
			"volume capability access mode %d is not defined by the CSI specification", int32(mode.GetMode()))
	}

	switch c.GetAccessType().(type) {
	case *csi.VolumeCapability_Block:
		return capabilityShape{mode: mode.GetMode(), block: true}, nil
	case *csi.VolumeCapability_Mount:
		return capabilityShape{mode: mode.GetMode(), mount: c.GetMount()}, nil
	default:
		// No access type at all, or a oneof case a future protocol version adds.
		return capabilityShape{}, status.Error(codes.InvalidArgument, "volume capability access type is required")
	}
}

// validateNodeCapability returns a gRPC status error on failure.
//
// A malformed capability (no access mode, an unknown access mode, no access
// type) is InvalidArgument: retrying cannot help. A capability the driver
// understands but does not support is FailedPrecondition, which is what the
// CSI spec calls "Exceeds capabilities" for the node RPCs.
func validateNodeCapability(c *csi.VolumeCapability) (nodeCapability, error) {
	shape, err := checkCapabilityShape(c)
	if err != nil {
		return nodeCapability{}, err
	}
	if shape.block {
		return nodeCapability{}, status.Error(codes.FailedPrecondition, "block access type is not supported")
	}

	if fsType := shape.mount.GetFsType(); fsType != "" && fsType != mounter.FSType {
		return nodeCapability{}, status.Errorf(codes.FailedPrecondition,
			"file system type %q is not supported, only %q is", fsType, mounter.FSType)
	}

	_, readOnly := readerOnlyAccessModes[shape.mode]
	_, singleTarget := singleTargetAccessModes[shape.mode]
	return nodeCapability{
		readOnly:     readOnly,
		singleTarget: singleTarget,
		// Copied so that the caller never shares memory with the request, which
		// the gRPC layer may reuse after the handler returns.
		mountFlags: append([]string(nil), shape.mount.GetMountFlags()...),
	}, nil
}

// checkControllerCapability validates c for the controller service. A
// malformed capability is an InvalidArgument status error. A well formed one
// the driver does not support returns supported=false and the reason, which
// ValidateVolumeCapabilities reports in its message and CreateVolume (T-11)
// turns into InvalidArgument.
//
// The mount flags are not checked here: they are validated by building the
// mount arguments of the volume, which needs the cluster and the volume id.
func checkControllerCapability(c *csi.VolumeCapability) (supported bool, reason string, err error) {
	shape, err := checkCapabilityShape(c)
	if err != nil {
		return false, "", err
	}

	if shape.block {
		return false, "block access type is not supported", nil
	}
	if fsType := shape.mount.GetFsType(); fsType != "" && fsType != mounter.FSType {
		return false, fmt.Sprintf("file system type %q is not supported, only %q is",
			fsType, mounter.FSType), nil
	}
	return true, "", nil
}
