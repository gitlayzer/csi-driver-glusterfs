package driver

import (
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/gitlayzer/csi-driver-glusterfs/pkg/mounter"
)

// supportedAccessModes are the VolumeCapability access modes the node service
// accepts, i.e. every mode of csi.proto except UNKNOWN.
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

// validateNodeCapability returns a gRPC status error on failure.
//
// A malformed capability (no access mode, an unknown access mode, no access
// type) is InvalidArgument: retrying cannot help. A capability the driver
// understands but does not support is FailedPrecondition, which is what the
// CSI spec calls "Exceeds capabilities" for the node RPCs.
func validateNodeCapability(c *csi.VolumeCapability) (nodeCapability, error) {
	if c == nil {
		return nodeCapability{}, status.Error(codes.InvalidArgument, "volume capability is required")
	}

	mode := c.GetAccessMode()
	if mode == nil {
		return nodeCapability{}, status.Error(codes.InvalidArgument, "volume capability access mode is required")
	}
	if mode.GetMode() == csi.VolumeCapability_AccessMode_UNKNOWN {
		return nodeCapability{}, status.Errorf(codes.InvalidArgument,
			"volume capability access mode %s is not supported", mode.GetMode().String())
	}
	if _, ok := supportedAccessModes[mode.GetMode()]; !ok {
		return nodeCapability{}, status.Errorf(codes.InvalidArgument,
			"volume capability access mode %d is not defined by the CSI specification", int32(mode.GetMode()))
	}

	switch c.GetAccessType().(type) {
	case nil:
		return nodeCapability{}, status.Error(codes.InvalidArgument, "volume capability access type is required")
	case *csi.VolumeCapability_Block:
		return nodeCapability{}, status.Error(codes.FailedPrecondition, "block access type is not supported")
	case *csi.VolumeCapability_Mount:
		// The mount type is the only kind this driver supports; the fs_type is
		// checked below.
	}

	if fsType := c.GetMount().GetFsType(); fsType != "" && fsType != mounter.FSType {
		return nodeCapability{}, status.Errorf(codes.FailedPrecondition,
			"file system type %q is not supported, only %q is", fsType, mounter.FSType)
	}

	_, readOnly := readerOnlyAccessModes[mode.GetMode()]
	_, singleTarget := singleTargetAccessModes[mode.GetMode()]
	return nodeCapability{
		readOnly:     readOnly,
		singleTarget: singleTarget,
		// Copied so that the caller never shares memory with the request, which
		// the gRPC layer may reuse after the handler returns.
		mountFlags: append([]string(nil), c.GetMount().GetMountFlags()...),
	}, nil
}
