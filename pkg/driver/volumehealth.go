package driver

import (
	"context"
	"fmt"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"

	"github.com/gitlayzer/csi-driver-glusterfs/pkg/mounter"
)

// Reasons of the INACCESSIBLE health entries. They are the machine readable part
// of the entry, so they are constants with a stable spelling; the message of the
// entry carries the human readable detail.
const (
	// healthReasonMountCorrupted means the mount point exists but its file
	// system does not answer (a dead FUSE process, ENOTCONN).
	healthReasonMountCorrupted = "MountCorrupted"
	// healthReasonNotMounted means nothing is mounted at the path, although the
	// CO expects the volume there.
	healthReasonNotMounted = "NotMounted"
	// healthReasonVolumeMismatch means something else is mounted at the path:
	// another volume, another subdirectory or another file system type.
	healthReasonVolumeMismatch = "VolumeMismatch"
)

// healthCheck is one path of the request together with the name the message of
// its health entry uses.
type healthCheck struct {
	// field names the path in a message ("staging target path").
	field string
	// path is the value of the request field; never empty.
	path string
}

// healthEntry accumulates one (status, reason) pair while the paths are checked.
type healthEntry struct {
	// status and reason are the identity of the entry, see [healthEntryOf].
	status csi.VolumeHealthErrorType
	reason string
	// messages are the per path sentences, joined with "; " in the response.
	messages []string
}

// NodeGetVolumeHealth reports the health of the volume from the point of view of
// this node.
//
// CSI v1.13 has no VOLUME_CONDITION any more (the capability value 4 and the
// volume_condition field of NodeGetVolumeStatsResponse are reserved), the
// replacement is this alpha RPC with the GET_VOLUME_HEALTH capability (D-18).
//
// Like NodeGetVolumeStats it takes no in-flight lock: a health probe must not
// report Aborted because a stage or publish of the same volume is running.
func (s *nodeService) NodeGetVolumeHealth(ctx context.Context, req *csi.NodeGetVolumeHealthRequest) (*csi.NodeGetVolumeHealthResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	id, err := decodeVolumeID(req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	// Both paths are OPTIONAL, but a value that is set has to be usable: the
	// spec says "If not empty, it MUST be an absolute path".
	if p := req.GetStagingTargetPath(); p != "" {
		if err := validatePath("staging target path", p); err != nil {
			return nil, err
		}
	}
	if p := req.GetVolumePublishPath(); p != "" {
		if err := validatePath("publish path", p); err != nil {
			return nil, err
		}
	}
	if err := ctxStatus(ctx); err != nil {
		return nil, err
	}

	// The staging path comes first: it is the FUSE mount the publish paths are
	// derived from, so a problem there explains the others.
	checks := make([]healthCheck, 0, 2)
	if p := req.GetStagingTargetPath(); p != "" {
		checks = append(checks, healthCheck{field: "staging target path", path: p})
	}
	if p := req.GetVolumePublishPath(); p != "" {
		checks = append(checks, healthCheck{field: "publish path", path: p})
	}

	// The plugin keeps no record of where a volume is mounted, so with both
	// paths empty there is nothing to look at. An empty list means "no adverse
	// condition known", which is the honest answer in that case.
	statuses := make([]*csi.VolumeHealth_VolumeHealthEntry, 0, len(checks))
	if len(checks) == 0 {
		klog.V(4).InfoS("volume health requested without a path", "volumeID", req.GetVolumeId())
		return volumeHealthResponse(req.GetVolumeId(), statuses), nil
	}

	// Entries are merged by (status, reason), because the spec forbids returning
	// the same combination twice for one volume. The order is the order of the
	// first occurrence, which is the staging path first.
	entries := make([]*healthEntry, 0, len(checks))
	index := make(map[[2]string]*healthEntry, len(checks))

	for _, check := range checks {
		state, merr := s.mounter.VolumeMountState(check.path, id.Volume, id.Subdir)
		if merr != nil {
			// One unreadable path invalidates the whole answer: reporting health
			// for the other path would suggest the CO knows the state of the
			// volume, which it does not.
			return nil, status.Errorf(codes.Internal, "check volume %s at %q: %v",
				echoVolumeID(req.GetVolumeId()), check.path, merr)
		}

		entryStatus, reason, message, ok := healthEntryOf(check, state)
		if !ok {
			continue
		}
		key := [2]string{entryStatus.String(), reason}
		if existing, found := index[key]; found {
			existing.messages = append(existing.messages, message)
			continue
		}
		entry := &healthEntry{status: entryStatus, reason: reason, messages: []string{message}}
		index[key] = entry
		entries = append(entries, entry)
	}

	for _, entry := range entries {
		statuses = append(statuses, &csi.VolumeHealth_VolumeHealthEntry{
			Status:  entry.status,
			Reason:  entry.reason,
			Message: strings.Join(entry.messages, "; "),
		})
	}

	if len(statuses) > 0 {
		klog.InfoS("reported an adverse volume health condition", "volumeID", req.GetVolumeId(),
			"entries", len(statuses))
	}

	return volumeHealthResponse(req.GetVolumeId(), statuses), nil
}

// healthEntryOf maps a mount state to the health entry of one path. ok is false
// for [mounter.MountStateMounted], because a healthy mount is not reported: the
// response only lists adverse conditions.
func healthEntryOf(check healthCheck, state mounter.MountState) (csi.VolumeHealthErrorType, string, string, bool) {
	switch state {
	case mounter.MountStateMounted:
		return csi.VolumeHealthErrorType_UNKNOWN_VOLUME_HEALTH_TYPE, "", "", false
	case mounter.MountStateCorrupted:
		return csi.VolumeHealthErrorType_INACCESSIBLE, healthReasonMountCorrupted,
			fmt.Sprintf("%s %q is a corrupted mount point: the file system does not respond", check.field, check.path), true
	case mounter.MountStateNotMounted:
		return csi.VolumeHealthErrorType_INACCESSIBLE, healthReasonNotMounted,
			fmt.Sprintf("%s %q is not a mount point", check.field, check.path), true
	case mounter.MountStateMismatch:
		return csi.VolumeHealthErrorType_INACCESSIBLE, healthReasonVolumeMismatch,
			fmt.Sprintf("%s %q is mounted from something else", check.field, check.path), true
	default:
		// A state this package does not define cannot be reported as healthy, so
		// it is reported like a mismatch: the conservative answer, because the
		// volume is certainly not known to be usable at that path.
		return csi.VolumeHealthErrorType_INACCESSIBLE, healthReasonVolumeMismatch,
			fmt.Sprintf("%s %q is in an unknown mount state", check.field, check.path), true
	}
}

// volumeHealthResponse wraps the entries and echoes the volume id, which the spec
// requires and csi-sanity asserts.
func volumeHealthResponse(volumeID string, statuses []*csi.VolumeHealth_VolumeHealthEntry) *csi.NodeGetVolumeHealthResponse {
	return &csi.NodeGetVolumeHealthResponse{
		VolumeHealth: &csi.VolumeHealth{
			VolumeId:       volumeID,
			HealthStatuses: statuses,
		},
	}
}
