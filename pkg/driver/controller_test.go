package driver

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/gitlayzer/csi-driver-glusterfs/pkg/config"
	"github.com/gitlayzer/csi-driver-glusterfs/pkg/mounter"
	"github.com/gitlayzer/csi-driver-glusterfs/pkg/volumeid"
)

// Volume ids of the controller tests: the test configuration defines cluster
// "c1" with the servers g1..g3 (see testConfigJSON).
const (
	// testStaticVolumeID addresses a whole GlusterFS volume.
	testStaticVolumeID = "gfs:v1:static:c1:vol"
	// testUnknownClusterVolumeID is a well formed id of a cluster the test
	// configuration does not define.
	testUnknownClusterVolumeID = "gfs:v1:static:missing:vol"
	// testVolumeModeVolumeID is a well formed id of the volume mode, which no
	// StorageClass parameter set of M2 can describe.
	testVolumeModeVolumeID = "gfs:v1:volume:c1:vol"
)

// blockCapability builds a Block capability with the given access mode.
func blockCapability(mode csi.VolumeCapability_AccessMode_Mode) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode},
	}
}

// mountCapabilityOfType builds a mount capability with the given fs type.
func mountCapabilityOfType(fsType string, flags ...string) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{
			Mount: &csi.VolumeCapability_MountVolume{FsType: fsType, MountFlags: flags},
		},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
	}
}

// withoutAccessType builds a capability that has an access mode but no access
// type at all.
func withoutAccessType() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
	}
}

func TestCheckControllerCapability(t *testing.T) {
	tests := []struct {
		name          string
		capability    *csi.VolumeCapability
		wantSupported bool
		// wantReason is a substring of the reason of an unsupported capability.
		wantReason string
		wantCode   codes.Code
	}{
		{
			name:       "nil capability is malformed",
			capability: nil,
			wantCode:   codes.InvalidArgument,
		},
		{
			name:       "no access mode is malformed",
			capability: withoutAccessType(),
			wantCode:   codes.InvalidArgument,
		},
		{
			name:       "unknown access mode is malformed",
			capability: accessModeCapability(csi.VolumeCapability_AccessMode_UNKNOWN),
			wantCode:   codes.InvalidArgument,
		},
		{
			name:       "access mode outside the protocol is malformed",
			capability: accessModeCapability(csi.VolumeCapability_AccessMode_Mode(99)),
			wantCode:   codes.InvalidArgument,
		},
		{
			name: "no access type is malformed",
			capability: &csi.VolumeCapability{
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
				},
			},
			wantCode: codes.InvalidArgument,
		},
		{
			name:          "single node writer mount",
			capability:    accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
			wantSupported: true,
		},
		{
			name:          "single node reader only mount",
			capability:    accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY),
			wantSupported: true,
		},
		{
			name:          "multi node reader only mount",
			capability:    accessModeCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY),
			wantSupported: true,
		},
		{
			name:          "multi node single writer mount",
			capability:    accessModeCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER),
			wantSupported: true,
		},
		{
			name:          "multi node multi writer mount",
			capability:    accessModeCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER),
			wantSupported: true,
		},
		{
			name:          "single node single writer mount",
			capability:    accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER),
			wantSupported: true,
		},
		{
			name:          "single node multi writer mount",
			capability:    accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER),
			wantSupported: true,
		},
		{
			name:          "mount without a file system type",
			capability:    mountCapabilityOfType(""),
			wantSupported: true,
		},
		{
			name:          "glusterfs file system type",
			capability:    mountCapabilityOfType(mounter.FSType),
			wantSupported: true,
		},
		{
			// The fs type is checked here, the flags need the cluster and are
			// checked by ValidateVolumeCapabilities.
			name:          "mount flags are not checked by this function",
			capability:    mountCapabilityOfType(mounter.FSType, "definitely-not-an-option"),
			wantSupported: true,
		},
		{
			name:       "block access type is unsupported",
			capability: blockCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
			wantReason: "block access type is not supported",
		},
		{
			name:       "block is unsupported for every access mode",
			capability: blockCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER),
			wantReason: "block access type is not supported",
		},
		{
			name:       "ext4 is unsupported",
			capability: mountCapabilityOfType("ext4"),
			wantReason: `file system type "ext4" is not supported, only "glusterfs" is`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			supported, reason, err := checkControllerCapability(tc.capability)

			if tc.wantCode != codes.OK {
				if err == nil {
					t.Fatalf("checkControllerCapability() = %t, %q, nil, want %v", supported, reason, tc.wantCode)
				}
				if got := status.Code(err); got != tc.wantCode {
					t.Fatalf("checkControllerCapability() code = %v (%v), want %v", got, err, tc.wantCode)
				}
				if supported || reason != "" {
					t.Fatalf("checkControllerCapability() = %t, %q with an error, want false and no reason",
						supported, reason)
				}
				return
			}

			if err != nil {
				t.Fatalf("checkControllerCapability() = %v, want nil", err)
			}
			if supported != tc.wantSupported {
				t.Fatalf("checkControllerCapability() = %t (%q), want %t", supported, reason, tc.wantSupported)
			}
			if tc.wantSupported {
				if reason != "" {
					t.Fatalf("checkControllerCapability() reason = %q, want none", reason)
				}
				return
			}
			if !strings.Contains(reason, tc.wantReason) {
				t.Fatalf("checkControllerCapability() reason = %q, want it to contain %q", reason, tc.wantReason)
			}
		})
	}
}

// TestCheckControllerCapabilityMatchesNodeShape checks that the two sides agree
// on what is malformed: a capability the controller reports as malformed must be
// malformed for the node service too, and the other way round. Only the
// supported/unsupported verdict may differ (D-16).
func TestCheckControllerCapabilityMatchesNodeShape(t *testing.T) {
	capabilities := map[string]*csi.VolumeCapability{
		"nil":            nil,
		"no access mode": withoutAccessType(),
		"unknown mode":   accessModeCapability(csi.VolumeCapability_AccessMode_UNKNOWN),
		"mode 99":        accessModeCapability(csi.VolumeCapability_AccessMode_Mode(99)),
		"no access type": {
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
		"block":       blockCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		"ext4":        mountCapabilityOfType("ext4"),
		"glusterfs":   mountCapabilityOfType(mounter.FSType),
		"empty fs":    mountCapabilityOfType(""),
		"rwx mount":   accessModeCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER),
		"bad fs type": mountCapabilityOfType("xfs"),
	}

	for name, capability := range capabilities {
		t.Run(name, func(t *testing.T) {
			_, _, controllerErr := checkControllerCapability(capability)
			_, nodeErr := validateNodeCapability(capability)

			controllerMalformed := status.Code(controllerErr) == codes.InvalidArgument
			nodeMalformed := status.Code(nodeErr) == codes.InvalidArgument
			if controllerMalformed != nodeMalformed {
				t.Fatalf("controller err = %v, node err = %v, want both or neither to be InvalidArgument",
					controllerErr, nodeErr)
			}
		})
	}
}

func TestControllerGetCapabilities(t *testing.T) {
	client := csi.NewControllerClient(bufconnConn(t, mustControllerDriver(t)))

	resp, err := client.ControllerGetCapabilities(context.Background(), &csi.ControllerGetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("ControllerGetCapabilities() = %v, want nil", err)
	}

	// The order is part of the response: CREATE_DELETE_VOLUME first, then
	// SINGLE_NODE_MULTI_WRITER (D-23).
	want := []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
	}
	caps := resp.GetCapabilities()
	if len(caps) != len(want) {
		t.Fatalf("capabilities = %v, want %d entries", caps, len(want))
	}
	for i, wantType := range want {
		if got := caps[i].GetRpc().GetType(); got != wantType {
			t.Fatalf("capability[%d] = %v, want %v", i, got, wantType)
		}
	}
}

// mustControllerDriver returns a driver in controller mode with the test
// configuration.
func mustControllerDriver(t *testing.T) *Driver {
	t.Helper()

	d, err := New(controllerOptions(t))
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}
	return d
}

// mustControllerService returns the controller service of a driver in
// controller mode, for the tests that call the RPC directly.
//
// The parent volume cache of the service is replaced by one with a fake mounter
// and a temporary mount directory, so that a test only has to set up the mount
// state and the content of the parent volume it is about.
func mustControllerService(t *testing.T) *controllerService {
	t.Helper()

	service, _ := newTestController(t)
	return service
}

// mustControllerDriverWithFake returns a driver in controller mode whose parent
// volume cache uses a fake mounter and whose mount point for the parent volume of
// testVolumeID exists, together with that fake.
func mustControllerDriverWithFake(t *testing.T) (*Driver, *fakeMounter) {
	t.Helper()

	d := mustControllerDriver(t)
	fake := installFakeParents(t, d.controller)
	return d, fake
}

// installFakeParents replaces the parent volume cache of a controller service by
// one with a fake mounter and creates the mount point of the parent volume of
// testVolumeID below it.
func installFakeParents(t *testing.T, service *controllerService) *fakeMounter {
	t.Helper()

	fake := &fakeMounter{mountState: mounter.MountStateMounted}
	service.parents = newParentMounts(fake, t.TempDir())
	// A mount creates its mount point; the fake does not, so it is created here for
	// the parent volume the test volume ids address.
	createVolumeDir(t, service.parents.dir, filepath.Join("c1", "vol"), 0o750)
	return fake
}

// newTestController returns a controller service whose parent volume cache uses a
// fake mounter, together with that fake.
//
// The fake reports MountStateMounted, so the mount point of a parent volume is a
// plain directory of a temporary directory and every directory and metadata
// operation the RPCs do is a real file system operation of the test.
func newTestController(t *testing.T) (*controllerService, *fakeMounter) {
	t.Helper()

	d, fake := mustControllerDriverWithFake(t)
	return d.controller, fake
}

// parentVolumePath returns the path of the mount point the controller service of a
// test uses for a parent volume.
func parentVolumePath(t *testing.T, service *controllerService, clusterID, volume string) string {
	t.Helper()
	return service.parents.mountPath(clusterID, volume)
}

// createExistingVolume creates the directory and the metadata of a volume in the
// parent volume of a service, i.e. the state a successful CreateVolume leaves
// behind, without going through CreateVolume. It returns the volume directory.
//
// ValidateVolumeCapabilities and the tests of other RPCs use it; the tests of
// CreateVolume itself call CreateVolume.
func createExistingVolume(t *testing.T, service *controllerService, name string) string {
	t.Helper()

	return seedVolume(t, service, name, volumeMeta{Version: metaVersion, OnDelete: deletePolicyDelete}, nil)
}

// createVolumeDir creates a directory inside dir with the given permission,
// including the missing parents. It stands in for the directory of a volume that
// CreateVolume would have made.
func createVolumeDir(t *testing.T, dir, name string, perm os.FileMode) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.MkdirAll(path, perm); err != nil {
		t.Fatalf("MkdirAll(%q) = %v, want nil", path, err)
	}
	// MkdirAll is subject to the umask; the permission is part of what the test
	// asserts, so it is set explicitly.
	if err := os.Chmod(path, perm); err != nil {
		t.Fatalf("Chmod(%q) = %v, want nil", path, err)
	}
	return path
}

// subdirParameters are the StorageClass parameters of testVolumeID: the subdir
// volume gfs:v1:subdir:c1:vol:pvc-1.
func subdirParameters() map[string]string {
	return map[string]string{
		paramClusterID:    "c1",
		paramParentVolume: "vol",
	}
}

func TestValidateVolumeCapabilities(t *testing.T) {
	tests := []struct {
		name     string
		req      *csi.ValidateVolumeCapabilitiesRequest
		ctx      func() (context.Context, context.CancelFunc)
		wantCode codes.Code
		// wantConfirmed is true when the response must confirm the request.
		wantConfirmed bool
		// wantMessage are substrings the Message of an unconfirmed response
		// must contain.
		wantMessage []string
		// wantErrMessage is a substring of the error message of a failure.
		wantErrMessage string
		// setup creates the state of the parent volume the request needs, for
		// example the directory of a subdir volume (D-23).
		setup func(t *testing.T, service *controllerService)
	}{
		{
			name: "subdir volume is confirmed",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
			},
			setup: func(t *testing.T, service *controllerService) {
				createExistingVolume(t, service, "pvc-1")
			},
			wantConfirmed: true,
		},
		{
			name: "never created subdir volume is not found",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
			},
			wantCode:       codes.NotFound,
			wantErrMessage: "not found",
		},
		{
			name: "static volume is confirmed",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testStaticVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
			},
			wantConfirmed: true,
		},
		{
			name: "several capabilities are confirmed",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId: testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{
					accessModeCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER),
					accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, "acl"),
					mountCapabilityOfType(""),
				},
			},
			setup: func(t *testing.T, service *controllerService) {
				createExistingVolume(t, service, "pvc-1")
			},
			wantConfirmed: true,
		},
		{
			name: "every field of the request is echoed",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
				VolumeContext: map[string]string{
					attrClusterID: "c1",
					attrVolume:    "vol",
					attrSubdir:    "pvc-1",
					"extra":       "ignored",
				},
				Parameters:        subdirParameters(),
				MutableParameters: nil,
			},
			setup: func(t *testing.T, service *controllerService) {
				createExistingVolume(t, service, "pvc-1")
			},
			wantConfirmed: true,
		},
		{
			name: "empty volume id",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
			},
			wantCode:       codes.InvalidArgument,
			wantErrMessage: "volume id is required",
		},
		{
			name: "no capabilities",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId: testVolumeID,
			},
			wantCode:       codes.InvalidArgument,
			wantErrMessage: "volume capabilities are required",
		},
		{
			name: "malformed capability",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{accessModeCapability(csi.VolumeCapability_AccessMode_UNKNOWN)},
			},
			wantCode: codes.InvalidArgument,
		},
		{
			// The order matters: a malformed capability is a problem of the
			// request, not of the volume, so it is reported even though the
			// volume does not exist.
			name: "malformed capability wins over an unknown volume",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           "some-vol-id",
				VolumeCapabilities: []*csi.VolumeCapability{accessModeCapability(csi.VolumeCapability_AccessMode_Mode(99))},
			},
			wantCode: codes.InvalidArgument,
		},
		{
			name: "volume id that does not decode",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           "some-vol-id",
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
			},
			wantCode:       codes.NotFound,
			wantErrMessage: "not found",
		},
		{
			name: "cluster is not configured",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testUnknownClusterVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
			},
			wantCode:       codes.NotFound,
			wantErrMessage: `cluster "missing" is not configured`,
		},
		{
			name: "block is unsupported, not an error",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{blockCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)},
			},
			wantMessage: []string{"block access type is not supported"},
			setup: func(t *testing.T, service *controllerService) {
				createExistingVolume(t, service, "pvc-1")
			},
		},
		{
			name: "ext4 is unsupported, not an error",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapabilityOfType("ext4")},
			},
			wantMessage: []string{`file system type "ext4" is not supported`},
			setup: func(t *testing.T, service *controllerService) {
				createExistingVolume(t, service, "pvc-1")
			},
		},
		{
			name: "an unsupported capability among supported ones",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId: testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{
					mountCapability,
					blockCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
				},
			},
			wantMessage: []string{"capability 1:", "block access type is not supported"},
			setup: func(t *testing.T, service *controllerService) {
				createExistingVolume(t, service, "pvc-1")
			},
		},
		{
			name: "forbidden mount flag",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, "rw")},
			},
			wantMessage: []string{"mount flags are not supported", `"rw" is not allowed`},
			setup: func(t *testing.T, service *controllerService) {
				createExistingVolume(t, service, "pvc-1")
			},
		},
		{
			name: "mount flag outside the allowlist",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, "foo=bar")},
			},
			wantMessage: []string{"mount flags are not supported", `option "foo" is not in the allowlist`},
			setup: func(t *testing.T, service *controllerService) {
				createExistingVolume(t, service, "pvc-1")
			},
		},
		{
			name: "duplicate mount flag",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, "acl,acl")},
			},
			wantMessage: []string{"mount flags are not supported", "set more than once"},
			setup: func(t *testing.T, service *controllerService) {
				createExistingVolume(t, service, "pvc-1")
			},
		},
		{
			name: "valid mount flag",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, "log-level=WARNING")},
			},
			wantConfirmed: true,
			setup: func(t *testing.T, service *controllerService) {
				createExistingVolume(t, service, "pvc-1")
			},
		},
		{
			name: "matching parameters",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
				Parameters:         subdirParameters(),
			},
			wantConfirmed: true,
			setup: func(t *testing.T, service *controllerService) {
				createExistingVolume(t, service, "pvc-1")
			},
		},
		{
			name: "matching parameters with the default values",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
				Parameters: map[string]string{
					paramClusterID:                "c1",
					paramProvisioningMode:         "subdir",
					paramParentVolume:             "vol",
					paramOnDelete:                 "retain",
					paramQuota:                    "false",
					"csi.storage.k8s.io/pvc/name": "pvc-1",
				},
			},
			wantConfirmed: true,
			setup: func(t *testing.T, service *controllerService) {
				createExistingVolume(t, service, "pvc-1")
			},
		},
		{
			name: "unknown parameter",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
				Parameters:         map[string]string{paramClusterID: "c1", paramParentVolume: "vol", "foo": "bar"},
			},
			wantMessage: []string{"parameters are not supported", `unknown parameter "foo"`},
			setup: func(t *testing.T, service *controllerService) {
				createExistingVolume(t, service, "pvc-1")
			},
		},
		{
			name: "quota true in the parameters",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
				Parameters:         map[string]string{paramClusterID: "c1", paramParentVolume: "vol", paramQuota: "true"},
			},
			wantMessage: []string{"parameters are not supported", `"true" is not supported yet`},
			setup: func(t *testing.T, service *controllerService) {
				createExistingVolume(t, service, "pvc-1")
			},
		},
		{
			name: "parameters of another cluster",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
				Parameters:         map[string]string{paramClusterID: "c2", paramParentVolume: "vol"},
			},
			wantMessage: []string{`parameters request cluster "c2"`},
			setup: func(t *testing.T, service *controllerService) {
				createExistingVolume(t, service, "pvc-1")
			},
		},
		{
			name: "parameters of another parent volume",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
				Parameters:         map[string]string{paramClusterID: "c1", paramParentVolume: "other"},
			},
			wantMessage: []string{`parameters request parent volume "other"`},
			setup: func(t *testing.T, service *controllerService) {
				createExistingVolume(t, service, "pvc-1")
			},
		},
		{
			name: "subdir parameters for a static volume",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testStaticVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
				Parameters:         subdirParameters(),
			},
			wantMessage: []string{`parameters request mode "subdir", but the volume is a "static" volume`},
		},
		{
			// The volume mode exists in the id encoding but no M2 StorageClass
			// can describe it, so any parameter set mismatches.
			name: "parameters for a volume mode id",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeModeVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
				Parameters:         subdirParameters(),
			},
			wantMessage: []string{`parameters request mode "subdir", but the volume is a "volume" volume`},
		},
		{
			name: "mutable parameters are not supported",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
				MutableParameters:  map[string]string{paramQuota: "false"},
			},
			wantMessage: []string{"mutable parameters are not supported"},
			setup: func(t *testing.T, service *controllerService) {
				createExistingVolume(t, service, "pvc-1")
			},
		},
		{
			name: "volume context that matches the volume",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
				VolumeContext: map[string]string{
					attrClusterID: "c1",
					attrVolume:    "vol",
					attrSubdir:    "pvc-1",
				},
			},
			wantConfirmed: true,
			setup: func(t *testing.T, service *controllerService) {
				createExistingVolume(t, service, "pvc-1")
			},
		},
		{
			name: "volume context with an unrelated key",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
				VolumeContext:      map[string]string{"something": "else"},
			},
			wantConfirmed: true,
			setup: func(t *testing.T, service *controllerService) {
				createExistingVolume(t, service, "pvc-1")
			},
		},
		{
			name: "volume context with the wrong volume",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
				VolumeContext:      map[string]string{attrVolume: "other"},
			},
			wantMessage: []string{`volume context volume "other" does not match the volume ("vol")`},
			setup: func(t *testing.T, service *controllerService) {
				createExistingVolume(t, service, "pvc-1")
			},
		},
		{
			name: "volume context with the wrong cluster",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
				VolumeContext:      map[string]string{attrClusterID: "c2"},
			},
			wantMessage: []string{"volume context clusterID"},
			setup: func(t *testing.T, service *controllerService) {
				createExistingVolume(t, service, "pvc-1")
			},
		},
		{
			name: "volume context with the wrong subdir",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
				VolumeContext:      map[string]string{attrSubdir: "other"},
			},
			wantMessage: []string{"volume context subdir"},
			setup: func(t *testing.T, service *controllerService) {
				createExistingVolume(t, service, "pvc-1")
			},
		},
		{
			name: "volume context claims a subdir of a volume without one",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testStaticVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
				VolumeContext:      map[string]string{attrSubdir: "pvc-1"},
			},
			wantMessage: []string{"volume context subdir"},
		},
		{
			name: "empty subdir of a volume without one",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testStaticVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
				VolumeContext:      map[string]string{attrSubdir: ""},
			},
			wantConfirmed: true,
		},
		{
			// The response carries the first reason only; the log has all of them
			// (see TestValidateVolumeCapabilitiesLogsAllReasons).
			name: "the first of several reasons is reported",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{blockCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)},
				MutableParameters:  map[string]string{paramQuota: "false"},
				VolumeContext:      map[string]string{attrVolume: "other"},
			},
			wantMessage: []string{"capability 0: block access type is not supported"},
			setup: func(t *testing.T, service *controllerService) {
				createExistingVolume(t, service, "pvc-1")
			},
		},
		{
			name: "cancelled context",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
			},
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, cancel
			},
			wantCode: codes.Canceled,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			service := mustControllerService(t)

			ctx, cancel := context.Background(), context.CancelFunc(func() {})
			if tc.ctx != nil {
				ctx, cancel = tc.ctx()
			}
			defer cancel()
			if tc.setup != nil {
				tc.setup(t, service)
			}

			resp, err := service.ValidateVolumeCapabilities(ctx, tc.req)

			if tc.wantCode != codes.OK {
				if err == nil {
					t.Fatalf("ValidateVolumeCapabilities() = %+v, nil, want %v", resp, tc.wantCode)
				}
				if got := status.Code(err); got != tc.wantCode {
					t.Fatalf("ValidateVolumeCapabilities() = %v (code %v), want %v", err, got, tc.wantCode)
				}
				if tc.wantErrMessage != "" && !strings.Contains(status.Convert(err).Message(), tc.wantErrMessage) {
					t.Fatalf("ValidateVolumeCapabilities() message = %q, want it to contain %q",
						status.Convert(err).Message(), tc.wantErrMessage)
				}
				if resp != nil {
					t.Fatalf("ValidateVolumeCapabilities() = %+v with an error, want nil", resp)
				}
				return
			}

			if err != nil {
				t.Fatalf("ValidateVolumeCapabilities() = %v, want nil", err)
			}
			if tc.wantConfirmed {
				confirmed := resp.GetConfirmed()
				if confirmed == nil {
					t.Fatalf("Confirmed = nil (%q), want the request confirmed", resp.GetMessage())
				}
				if resp.GetMessage() != "" {
					t.Fatalf("Message = %q, want none for a confirmed request", resp.GetMessage())
				}
				wantCapabilitiesConfirmation(t, tc.req, confirmed)
				return
			}

			if resp.GetConfirmed() != nil {
				t.Fatalf("Confirmed = %+v, want nil", resp.GetConfirmed())
			}
			message := resp.GetMessage()
			if message == "" {
				t.Fatal("Message = empty, want the reason for the unconfirmed request")
			}
			for _, want := range tc.wantMessage {
				if !strings.Contains(message, want) {
					t.Fatalf("Message = %q, want it to contain %q", message, want)
				}
			}
		})
	}
}

// TestValidateVolumeCapabilitiesLogsAllReasons checks that the reasons the
// response cannot carry (it holds the first one only) are still visible in the
// log, so that a whole broken StorageClass can be fixed in one go.
func TestValidateVolumeCapabilitiesLogsAllReasons(t *testing.T) {
	buf := captureKlog(t, "4")

	service := mustControllerService(t)
	createExistingVolume(t, service, "pvc-1")
	resp, err := service.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId:           testVolumeID,
		VolumeCapabilities: []*csi.VolumeCapability{blockCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)},
		MutableParameters:  map[string]string{paramQuota: "false"},
		VolumeContext:      map[string]string{attrVolume: "other"},
	})
	if err != nil {
		t.Fatalf("ValidateVolumeCapabilities() = %v, want nil", err)
	}
	if resp.GetConfirmed() != nil {
		t.Fatalf("Confirmed = %+v, want nil", resp.GetConfirmed())
	}

	logged := buf.String()
	for _, want := range []string{
		"volume capabilities are not confirmed",
		"block access type is not supported",
		"mutable parameters are not supported",
		"volume context volume",
	} {
		if !strings.Contains(logged, want) {
			t.Fatalf("log = %q, want it to contain %q", logged, want)
		}
	}
}

// wantCapabilitiesConfirmation checks that the Confirmed part of a response
// echoes the request and shares no memory with it.
func wantCapabilitiesConfirmation(t *testing.T, req *csi.ValidateVolumeCapabilitiesRequest, confirmed *csi.ValidateVolumeCapabilitiesResponse_Confirmed) {
	t.Helper()

	if len(confirmed.GetVolumeCapabilities()) != len(req.GetVolumeCapabilities()) {
		t.Fatalf("Confirmed.VolumeCapabilities = %v, want %v",
			confirmed.GetVolumeCapabilities(), req.GetVolumeCapabilities())
	}
	for i, want := range req.GetVolumeCapabilities() {
		if !proto.Equal(confirmed.GetVolumeCapabilities()[i], want) {
			t.Fatalf("Confirmed.VolumeCapabilities[%d] = %v, want %v", i, confirmed.GetVolumeCapabilities()[i], want)
		}
	}
	if !mapsEqual(confirmed.GetVolumeContext(), req.GetVolumeContext()) {
		t.Fatalf("Confirmed.VolumeContext = %v, want %v", confirmed.GetVolumeContext(), req.GetVolumeContext())
	}
	if !mapsEqual(confirmed.GetParameters(), req.GetParameters()) {
		t.Fatalf("Confirmed.Parameters = %v, want %v", confirmed.GetParameters(), req.GetParameters())
	}
	if !mapsEqual(confirmed.GetMutableParameters(), req.GetMutableParameters()) {
		t.Fatalf("Confirmed.MutableParameters = %v, want %v",
			confirmed.GetMutableParameters(), req.GetMutableParameters())
	}
}

// mapsEqual reports whether two maps have the same content; two empty maps are
// equal even when only one of them is nil.
func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if other, ok := b[key]; !ok || other != value {
			return false
		}
	}
	return true
}

// TestValidateVolumeCapabilitiesDoesNotShareMemory checks that mutating the
// response does not change the request: the CO may compare the confirmation with
// the request it sent, which is only meaningful when they are independent.
func TestValidateVolumeCapabilitiesDoesNotShareMemory(t *testing.T) {
	service := mustControllerService(t)
	createExistingVolume(t, service, "pvc-1")

	req := &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId: testVolumeID,
		VolumeCapabilities: []*csi.VolumeCapability{
			accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, "acl"),
		},
		VolumeContext: map[string]string{attrClusterID: "c1", "extra": "value"},
		Parameters:    subdirParameters(),
	}

	resp, err := service.ValidateVolumeCapabilities(context.Background(), req)
	if err != nil {
		t.Fatalf("ValidateVolumeCapabilities() = %v, want nil", err)
	}
	confirmed := resp.GetConfirmed()
	if confirmed == nil {
		t.Fatal("Confirmed = nil, want the request confirmed")
	}

	// Mutating the response must not reach the request ...
	confirmed.VolumeContext[attrClusterID] = "mutated"
	confirmed.VolumeContext["added"] = "value"
	confirmed.Parameters[paramClusterID] = "mutated"
	confirmed.VolumeCapabilities[0].AccessMode.Mode = csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER
	confirmed.VolumeCapabilities[0].GetMount().MountFlags[0] = "mutated"

	if got := req.GetVolumeContext()[attrClusterID]; got != "c1" {
		t.Fatalf("request VolumeContext[clusterID] = %q, want %q", got, "c1")
	}
	if _, ok := req.GetVolumeContext()["added"]; ok {
		t.Fatal("request VolumeContext gained the key added by the test")
	}
	if got := req.GetParameters()[paramClusterID]; got != "c1" {
		t.Fatalf("request Parameters[clusterID] = %q, want %q", got, "c1")
	}
	if got := req.GetVolumeCapabilities()[0].GetAccessMode().GetMode(); got != csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER {
		t.Fatalf("request access mode = %v, want %v", got, csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)
	}
	if got := req.GetVolumeCapabilities()[0].GetMount().GetMountFlags(); !slices.Equal(got, []string{"acl"}) {
		t.Fatalf("request mount flags = %v, want [acl]", got)
	}

	// ... and mutating the request must not reach the response.
	req.GetVolumeContext()[attrClusterID] = "changed"
	if got := confirmed.GetVolumeContext()[attrClusterID]; got == "changed" {
		t.Fatal("Confirmed.VolumeContext follows the request, want a copy")
	}
}

// TestValidateVolumeCapabilitiesWithoutConfig checks the nil store guard: a
// Driver built by hand may have no configuration, which must report NotFound
// instead of panicking.
func TestValidateVolumeCapabilitiesWithoutConfig(t *testing.T) {
	service := &controllerService{driver: &Driver{}, locks: newInflight()}

	_, err := service.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId:           testVolumeID,
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
	})
	wantCode(t, err, codes.NotFound)
}

// TestValidateVolumeCapabilitiesWithoutDriver checks the same guard for a
// controller service that was never attached to a driver.
func TestValidateVolumeCapabilitiesWithoutDriver(t *testing.T) {
	service := &controllerService{locks: newInflight()}

	_, err := service.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId:           testVolumeID,
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
	})
	wantCode(t, err, codes.NotFound)
}

// TestControllerServicesUnimplemented checks that the RPCs of later tasks answer
// Unimplemented instead of a nil response.
func TestControllerServicesUnimplemented(t *testing.T) {
	client := csi.NewControllerClient(bufconnConn(t, mustControllerDriver(t)))
	ctx := context.Background()

	t.Run("ControllerExpandVolume", func(t *testing.T) {
		_, err := client.ControllerExpandVolume(ctx, &csi.ControllerExpandVolumeRequest{
			VolumeId: testVolumeID,
			CapacityRange: &csi.CapacityRange{
				RequiredBytes: 1 << 30,
			},
		})
		wantCode(t, err, codes.Unimplemented)
	})
	t.Run("GetCapacity", func(t *testing.T) {
		_, err := client.GetCapacity(ctx, &csi.GetCapacityRequest{})
		wantCode(t, err, codes.Unimplemented)
	})
}

// TestControllerServiceNotRegisteredInNodeMode checks that node mode serves no
// Controller service at all, so that the CO cannot call an RPC that would be
// answered by a service the process does not run.
func TestControllerServiceNotRegisteredInNodeMode(t *testing.T) {
	d, err := New(nodeOptions(t))
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}

	client := csi.NewControllerClient(bufconnConn(t, d))
	_, err = client.ControllerGetCapabilities(context.Background(), &csi.ControllerGetCapabilitiesRequest{})
	wantCode(t, err, codes.Unimplemented)

	_, err = client.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId:           testVolumeID,
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
	})
	wantCode(t, err, codes.Unimplemented)
}

// TestControllerServiceInAllMode checks that all mode serves both services on the
// same server.
func TestControllerServiceInAllMode(t *testing.T) {
	opts := nodeOptions(t)
	opts.Mode = ModeAll
	opts.ControllerMountDir = t.TempDir()

	d, err := New(opts)
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}
	// The controller side of all mode mounts parent volumes as well, so it gets a
	// fake mounter like the node side of the other tests.
	installFakeParents(t, d.controller)
	createExistingVolume(t, d.controller, "pvc-1")

	conn := bufconnConn(t, d)
	controllerClient := csi.NewControllerClient(conn)
	nodeClient := csi.NewNodeClient(conn)

	if _, capErr := controllerClient.ControllerGetCapabilities(context.Background(),
		&csi.ControllerGetCapabilitiesRequest{}); capErr != nil {
		t.Fatalf("ControllerGetCapabilities() = %v, want nil in all mode", capErr)
	}
	resp, err := controllerClient.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId:           testVolumeID,
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
	})
	if err != nil {
		t.Fatalf("ValidateVolumeCapabilities() = %v, want nil", err)
	}
	if resp.GetConfirmed() == nil {
		t.Fatalf("Confirmed = nil (%q), want the request confirmed", resp.GetMessage())
	}
	if _, nodeErr := nodeClient.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{}); nodeErr != nil {
		t.Fatalf("NodeGetInfo() = %v, want nil in all mode", nodeErr)
	}
}

// TestControllerGetCapabilitiesDirect checks the response of the RPC without the
// gRPC layer, which is what a csi-sanity style caller inspects.
func TestControllerGetCapabilitiesDirect(t *testing.T) {
	service := mustControllerService(t)

	resp, err := service.ControllerGetCapabilities(context.Background(), &csi.ControllerGetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("ControllerGetCapabilities() = %v, want nil", err)
	}
	if resp.GetCapabilities() == nil {
		t.Fatal("Capabilities = nil, want an empty, non-nil slice")
	}
	if len(resp.GetCapabilities()) != len(controllerRPCapabilities) {
		t.Fatalf("Capabilities = %v, want %d entries", resp.GetCapabilities(), len(controllerRPCapabilities))
	}
	for i, want := range controllerRPCapabilities {
		if got := resp.GetCapabilities()[i].GetRpc().GetType(); got != want {
			t.Fatalf("Capabilities[%d] = %v, want %v", i, got, want)
		}
	}
}

// TestControllerRPCapabilities checks the capability list itself, independently
// of the RPC: the order is part of the response and unused capabilities must not
// appear before they are implemented.
func TestControllerRPCapabilities(t *testing.T) {
	want := []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
	}
	if !slices.Equal(controllerRPCapabilities, want) {
		t.Fatalf("controllerRPCapabilities = %v, want %v", controllerRPCapabilities, want)
	}

	unused := map[string]csi.ControllerServiceCapability_RPC_Type{
		"EXPAND_VOLUME":          csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
		"GET_CAPACITY":           csi.ControllerServiceCapability_RPC_GET_CAPACITY,
		"CREATE_DELETE_SNAPSHOT": csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
		"CLONE_VOLUME":           csi.ControllerServiceCapability_RPC_CLONE_VOLUME,
	}
	for name, capability := range unused {
		if slices.Contains(controllerRPCapabilities, capability) {
			t.Fatalf("controllerRPCapabilities contains %s, want only what is implemented", name)
		}
	}
}

// TestCheckParameters checks the parameter comparison of
// ValidateVolumeCapabilities against every volume mode of the id encoding.
func TestCheckParameters(t *testing.T) {
	subdir, err := volumeid.Decode(testVolumeID)
	if err != nil {
		t.Fatalf("Decode(%q) = %v, want nil", testVolumeID, err)
	}
	static, err := volumeid.Decode(testStaticVolumeID)
	if err != nil {
		t.Fatalf("Decode(%q) = %v, want nil", testStaticVolumeID, err)
	}
	volume, err := volumeid.Decode(testVolumeModeVolumeID)
	if err != nil {
		t.Fatalf("Decode(%q) = %v, want nil", testVolumeModeVolumeID, err)
	}

	service := &controllerService{}

	tests := []struct {
		name    string
		params  map[string]string
		id      volumeid.ID
		want    string
		wantNot bool
	}{
		{
			name:   "no parameters are always acceptable",
			params: nil,
			id:     subdir,
		},
		{
			name:   "matching subdir parameters",
			params: subdirParameters(),
			id:     subdir,
		},
		{
			name:   "static parameters for a static volume",
			params: map[string]string{paramClusterID: "c1", paramProvisioningMode: "static"},
			id:     static,
			want:   "parameters are not supported",
		},
		{
			name:   "subdir parameters for a volume without a subdir",
			params: subdirParameters(),
			id:     static,
			want:   `parameters request mode "subdir"`,
		},
		{
			name:   "subdir parameters for a volume mode id",
			params: subdirParameters(),
			id:     volume,
			want:   `parameters request mode "subdir", but the volume is a "volume" volume`,
		},
		{
			name:   "invalid parameters",
			params: map[string]string{paramClusterID: "a/b", paramParentVolume: "vol"},
			id:     subdir,
			want:   "invalid storage class parameters",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := service.checkParameters(tc.params, tc.id)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("checkParameters() = %q, want no reason", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("checkParameters() = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

// TestCheckMountFlags checks that the mount options are validated against the
// same mounter rules the node service uses.
func TestCheckMountFlags(t *testing.T) {
	id, err := volumeid.Decode(testVolumeID)
	if err != nil {
		t.Fatalf("Decode(%q) = %v, want nil", testVolumeID, err)
	}
	cluster := configClusterForTest(t, "c1")

	tests := []struct {
		name       string
		caps       []*csi.VolumeCapability
		wantReason string
	}{
		{
			name: "no flags",
			caps: []*csi.VolumeCapability{mountCapability},
		},
		{
			name: "allowed flag",
			caps: []*csi.VolumeCapability{
				accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, "log-level=WARNING"),
			},
		},
		{
			name: "allowed comma separated flags",
			caps: []*csi.VolumeCapability{
				accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, "acl,log-level=INFO"),
			},
		},
		{
			name: "forbidden flag",
			caps: []*csi.VolumeCapability{
				accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, "rw"),
			},
			wantReason: "mount flags are not supported",
		},
		{
			name: "unknown flag",
			caps: []*csi.VolumeCapability{
				accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, "foo=bar"),
			},
			wantReason: "mount flags are not supported",
		},
	}

	service := &controllerService{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reasons := service.checkMountFlags(tc.caps, cluster, id)
			if tc.wantReason == "" {
				if len(reasons) != 0 {
					t.Fatalf("checkMountFlags() = %v, want no reason", reasons)
				}
				return
			}
			if len(reasons) != 1 {
				t.Fatalf("checkMountFlags() = %v, want one reason", reasons)
			}
			if !strings.Contains(reasons[0], tc.wantReason) {
				t.Fatalf("checkMountFlags() = %q, want it to contain %q", reasons[0], tc.wantReason)
			}
		})
	}
}

// configClusterForTest returns the configured cluster of a test configuration.
func configClusterForTest(t *testing.T, clusterID string) config.Cluster {
	t.Helper()

	store := testConfigStore(t)
	cluster, ok := store.Current().Get(clusterID)
	if !ok {
		t.Fatalf("cluster %q is not configured", clusterID)
	}
	return cluster
}

// TestConfirmedResponse checks that the confirmation echoes every field of the
// request, including mutable_parameters: the CSI spec tells the CO to compare
// the confirmation with what it sent, so a field the driver validated has to
// come back. (ValidateVolumeCapabilities never confirms a request with mutable
// parameters today, see checkParameters and the MODIFY_VOLUME capability.)
func TestConfirmedResponse(t *testing.T) {
	req := &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId:           testVolumeID,
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
		VolumeContext:      map[string]string{attrVolume: "vol"},
		Parameters:         subdirParameters(),
		MutableParameters:  map[string]string{paramQuota: "false"},
	}

	confirmed := confirmedResponse(req)
	wantCapabilitiesConfirmation(t, req, confirmed)

	empty := confirmedResponse(&csi.ValidateVolumeCapabilitiesRequest{})
	if empty.GetVolumeCapabilities() != nil {
		t.Fatalf("VolumeCapabilities = %v, want nil for a request without any", empty.GetVolumeCapabilities())
	}
	if empty.GetVolumeContext() != nil || empty.GetParameters() != nil || empty.GetMutableParameters() != nil {
		t.Fatalf("Confirmed = %+v, want every map nil for a request without any", empty)
	}
}

// TestCopyStringMap checks the copy helper, including the nil case the response
// relies on to keep unset fields unset.
func TestCopyStringMap(t *testing.T) {
	if got := copyStringMap(nil); got != nil {
		t.Fatalf("copyStringMap(nil) = %v, want nil", got)
	}
	if got := copyStringMap(map[string]string{}); got != nil {
		t.Fatalf("copyStringMap(empty) = %v, want nil", got)
	}

	in := map[string]string{"a": "1", "b": "2"}
	got := copyStringMap(in)
	if !mapsEqual(got, in) {
		t.Fatalf("copyStringMap() = %v, want %v", got, in)
	}
	got["a"] = "changed"
	if in["a"] != "1" {
		t.Fatal("copyStringMap() shares memory with the input")
	}
}

// TestCopyVolumeCapabilities checks the deep copy helper.
func TestCopyVolumeCapabilities(t *testing.T) {
	if got := copyVolumeCapabilities(nil); got != nil {
		t.Fatalf("copyVolumeCapabilities(nil) = %v, want nil", got)
	}

	in := []*csi.VolumeCapability{
		accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, "acl"),
		nil,
	}
	got := copyVolumeCapabilities(in)
	if len(got) != len(in) {
		t.Fatalf("copyVolumeCapabilities() = %v, want %d entries", got, len(in))
	}
	if got[1] != nil {
		t.Fatalf("copyVolumeCapabilities()[1] = %v, want nil", got[1])
	}
	if !proto.Equal(got[0], in[0]) {
		t.Fatalf("copyVolumeCapabilities()[0] = %v, want %v", got[0], in[0])
	}
	got[0].GetMount().MountFlags[0] = "mutated"
	if in[0].GetMount().GetMountFlags()[0] != "acl" {
		t.Fatal("copyVolumeCapabilities() shares memory with the input")
	}
}

// TestValidateVolumeCapabilitiesThroughGRPC checks the RPC over the real server,
// including the interceptors, for the two outcomes a CO cares about.
func TestValidateVolumeCapabilitiesThroughGRPC(t *testing.T) {
	d, _ := mustControllerDriverWithFake(t)
	client := csi.NewControllerClient(bufconnConn(t, d))
	createExistingVolume(t, d.controller, "pvc-1")
	ctx := context.Background()

	ok, err := client.ValidateVolumeCapabilities(ctx, &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId:           testVolumeID,
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
	})
	if err != nil {
		t.Fatalf("ValidateVolumeCapabilities() = %v, want nil", err)
	}
	if ok.GetConfirmed() == nil {
		t.Fatalf("Confirmed = nil (%q), want the request confirmed", ok.GetMessage())
	}
	if len(ok.GetConfirmed().GetVolumeCapabilities()) != 1 {
		t.Fatalf("Confirmed.VolumeCapabilities = %v, want one", ok.GetConfirmed().GetVolumeCapabilities())
	}

	rejected, err := client.ValidateVolumeCapabilities(ctx, &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId:           testVolumeID,
		VolumeCapabilities: []*csi.VolumeCapability{blockCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)},
	})
	if err != nil {
		t.Fatalf("ValidateVolumeCapabilities() = %v, want OK with an unconfirmed response", err)
	}
	if rejected.GetConfirmed() != nil {
		t.Fatalf("Confirmed = %+v, want nil", rejected.GetConfirmed())
	}
	if !strings.Contains(rejected.GetMessage(), "block access type is not supported") {
		t.Fatalf("Message = %q, want the reason", rejected.GetMessage())
	}

	_, err = client.ValidateVolumeCapabilities(ctx, &csi.ValidateVolumeCapabilitiesRequest{})
	wantCode(t, err, codes.InvalidArgument)
}

// TestValidateVolumeCapabilitiesSecretsNotLogged checks that the Secrets map of
// the request never reaches the log, which is a rule of AGENTS.md.
func TestValidateVolumeCapabilitiesSecretsNotLogged(t *testing.T) {
	// V(5) is the only verbosity at which the interceptors log the request.
	buf := captureKlog(t, "5")

	d, _ := mustControllerDriverWithFake(t)
	client := csi.NewControllerClient(bufconnConn(t, d))
	createExistingVolume(t, d.controller, "pvc-1")
	const secret = "sup3r-s3cret"
	req := &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId:           testVolumeID,
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
		Secrets:            map[string]string{"password": secret},
	}

	if _, err := client.ValidateVolumeCapabilities(context.Background(), req); err != nil {
		t.Fatalf("ValidateVolumeCapabilities() = %v, want nil", err)
	}

	logged := buf.String()
	if !strings.Contains(logged, "CSI request succeeded") {
		t.Fatalf("log = %q, want a success line at V(5)", logged)
	}
	if strings.Contains(logged, secret) {
		t.Fatalf("log = %q, must not contain the secret value", logged)
	}
	if !strings.Contains(logged, "***stripped***") {
		t.Fatalf("log = %q, want the secret to be replaced by ***stripped***", logged)
	}
}

// TestValidateVolumeCapabilitiesErrorIsNotAResponse checks that a failure does
// not come with a response body, which is what the CSI spec requires.
func TestValidateVolumeCapabilitiesErrorIsNotAResponse(t *testing.T) {
	service := mustControllerService(t)

	resp, err := service.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId:           "some-vol-id",
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
	})
	wantCode(t, err, codes.NotFound)
	if resp != nil {
		t.Fatalf("ValidateVolumeCapabilities() = %+v with an error, want nil", resp)
	}
}

// newTestControllerMounting returns a controller service whose fake mounter really
// mounts: the mount state is NotMounted and a successful mount creates the mount
// point, so the RPCs see a fresh directory as the parent volume and a test can
// count the mounts.
func newTestControllerMounting(t *testing.T) (*controllerService, *fakeMounter) {
	t.Helper()

	d := mustControllerDriver(t)
	// trackState makes the fake remember that the mount succeeded, which is what a
	// real mount does: the second RPC finds the volume mounted and does not mount it
	// again.
	fake := &fakeMounter{mountState: mounter.MountStateNotMounted, trackState: true}
	fake.onCreate = func(target string) error { return os.MkdirAll(target, 0o750) }
	d.controller.parents = newParentMounts(fake, t.TempDir())
	return d.controller, fake
}

// subdirCreateRequest returns a valid CreateVolumeRequest for a subdir volume
// named name in the parent volume "vol" of the test configuration.
func subdirCreateRequest(name string) *csi.CreateVolumeRequest {
	return &csi.CreateVolumeRequest{
		Name:               name,
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
		Parameters:         subdirParameters(),
	}
}

// withCapacity returns req with the given capacity range.
func withCapacity(req *csi.CreateVolumeRequest, required, limit int64) *csi.CreateVolumeRequest {
	req.CapacityRange = &csi.CapacityRange{RequiredBytes: required, LimitBytes: limit}
	return req
}

// withOnDelete returns req with the given onDelete parameter.
func withOnDelete(req *csi.CreateVolumeRequest, policy deletePolicy) *csi.CreateVolumeRequest {
	req.Parameters = map[string]string{
		paramClusterID:    "c1",
		paramParentVolume: "vol",
		paramOnDelete:     string(policy),
	}
	return req
}

// seedVolume writes the metadata and the directory of a volume directly, i.e. the
// state a successful CreateVolume leaves behind, and returns the volume directory.
// files are written below it.
func seedVolume(t *testing.T, service *controllerService, name string, meta volumeMeta, files map[string]string) string {
	t.Helper()

	root, err := service.parentRoot(context.Background(), configClusterForTest(t, "c1"), "vol")
	if err != nil {
		t.Fatalf("parentRoot() = %v, want nil", err)
	}
	if err := writeVolumeMeta(root, name, meta); err != nil {
		t.Fatalf("writeVolumeMeta() = %v, want nil", err)
	}
	if err := ensureVolumeDir(root, name); err != nil {
		t.Fatalf("ensureVolumeDir() = %v, want nil", err)
	}
	dir := filepath.Join(root.Name(), name)
	for file, content := range files {
		writeTestFile(t, dir, file, content)
	}
	return dir
}

// seedVolumeUnmounted is seedVolume for a service whose parent volume is not
// mounted yet: it writes the files through a root it opens itself, so the next RPC
// still has to mount the parent volume.
func seedVolumeUnmounted(t *testing.T, service *controllerService, name string, meta volumeMeta, files map[string]string) string {
	t.Helper()

	parent := parentVolumePath(t, service, "c1", "vol")
	if err := os.MkdirAll(parent, 0o750); err != nil {
		t.Fatalf("MkdirAll(%q) = %v, want nil", parent, err)
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		t.Fatalf("OpenRoot(%q) = %v, want nil", parent, err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			t.Errorf("Close() = %v, want nil", err)
		}
	}()

	if err := writeVolumeMeta(root, name, meta); err != nil {
		t.Fatalf("writeVolumeMeta() = %v, want nil", err)
	}
	if err := ensureVolumeDir(root, name); err != nil {
		t.Fatalf("ensureVolumeDir() = %v, want nil", err)
	}
	dir := filepath.Join(parent, name)
	for file, content := range files {
		writeTestFile(t, dir, file, content)
	}
	return dir
}

// volumeMetaOf reads the metadata of a volume from the parent volume of a test.
func volumeMetaOf(t *testing.T, service *controllerService, name string) (volumeMeta, bool) {
	t.Helper()

	root, err := service.parentRoot(context.Background(), configClusterForTest(t, "c1"), "vol")
	if err != nil {
		t.Fatalf("parentRoot() = %v, want nil", err)
	}
	meta, found, err := readVolumeMeta(root, name)
	if err != nil {
		t.Fatalf("readVolumeMeta() = %v, want nil", err)
	}
	return meta, found
}

// volumeDirOf returns the path of the directory of a volume in the parent volume of
// a test.
func volumeDirOf(t *testing.T, service *controllerService, name string) string {
	t.Helper()

	return filepath.Join(parentVolumePath(t, service, "c1", "vol"), name)
}

// wantVolumeDirMode checks the permission of a volume directory (D-27).
func wantVolumeDirMode(t *testing.T, dir string) {
	t.Helper()

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat(%q) = %v, want nil", dir, err)
	}
	if got := info.Mode().Perm(); got != volumeDirPerm {
		t.Fatalf("volume directory mode = %o, want %o", got, volumeDirPerm)
	}
}

func TestCreateVolumeValidation(t *testing.T) {
	longName := strings.Repeat("a", volumeid.MaxLength)

	tests := []struct {
		name           string
		req            *csi.CreateVolumeRequest
		wantCode       codes.Code
		wantErrMessage string
	}{
		{
			name:           "empty name",
			req:            subdirCreateRequest(""),
			wantCode:       codes.InvalidArgument,
			wantErrMessage: "volume name is required",
		},
		{
			name:           "separator in the name",
			req:            subdirCreateRequest("a/b"),
			wantCode:       codes.InvalidArgument,
			wantErrMessage: "invalid volume name",
		},
		{
			name:           "relative name",
			req:            subdirCreateRequest(".."),
			wantCode:       codes.InvalidArgument,
			wantErrMessage: "invalid volume name",
		},
		{
			name:           "name starting with a dot is reserved",
			req:            subdirCreateRequest(".hidden"),
			wantCode:       codes.InvalidArgument,
			wantErrMessage: "reserved",
		},
		{
			name:           "name that makes the volume id too long",
			req:            subdirCreateRequest(longName),
			wantCode:       codes.InvalidArgument,
			wantErrMessage: "cannot encode the volume id",
		},
		{
			name: "no capabilities",
			req: &csi.CreateVolumeRequest{
				Name:       "pvc-1",
				Parameters: subdirParameters(),
			},
			wantCode:       codes.InvalidArgument,
			wantErrMessage: "volume capabilities are required",
		},
		{
			name: "malformed capability",
			req: &csi.CreateVolumeRequest{
				Name:               "pvc-1",
				VolumeCapabilities: []*csi.VolumeCapability{accessModeCapability(csi.VolumeCapability_AccessMode_UNKNOWN)},
				Parameters:         subdirParameters(),
			},
			wantCode: codes.InvalidArgument,
		},
		{
			name: "block capability",
			req: &csi.CreateVolumeRequest{
				Name:               "pvc-1",
				VolumeCapabilities: []*csi.VolumeCapability{blockCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)},
				Parameters:         subdirParameters(),
			},
			wantCode:       codes.InvalidArgument,
			wantErrMessage: "capability 0: block access type is not supported",
		},
		{
			name: "unsupported file system type",
			req: &csi.CreateVolumeRequest{
				Name:               "pvc-1",
				VolumeCapabilities: []*csi.VolumeCapability{mountCapabilityOfType("ext4")},
				Parameters:         subdirParameters(),
			},
			wantCode:       codes.InvalidArgument,
			wantErrMessage: `file system type "ext4" is not supported`,
		},
		{
			name:           "negative required capacity",
			req:            withCapacity(subdirCreateRequest("pvc-1"), -1, 0),
			wantCode:       codes.InvalidArgument,
			wantErrMessage: "must not be negative",
		},
		{
			name:           "negative capacity limit",
			req:            withCapacity(subdirCreateRequest("pvc-1"), 0, -1),
			wantCode:       codes.InvalidArgument,
			wantErrMessage: "must not be negative",
		},
		{
			name:           "required capacity above the limit",
			req:            withCapacity(subdirCreateRequest("pvc-1"), 2<<30, 1<<30),
			wantCode:       codes.InvalidArgument,
			wantErrMessage: "exceeds the limit",
		},
		{
			name: "content source",
			req: func() *csi.CreateVolumeRequest {
				req := subdirCreateRequest("pvc-1")
				req.VolumeContentSource = &csi.VolumeContentSource{
					Type: &csi.VolumeContentSource_Snapshot{
						Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: "snap-1"},
					},
				}
				return req
			}(),
			wantCode:       codes.InvalidArgument,
			wantErrMessage: "volume content source is not supported",
		},
		{
			name: "mutable parameters",
			req: func() *csi.CreateVolumeRequest {
				req := subdirCreateRequest("pvc-1")
				req.MutableParameters = map[string]string{paramQuota: "false"}
				return req
			}(),
			wantCode:       codes.InvalidArgument,
			wantErrMessage: "mutable parameters are not supported",
		},
		{
			name: "invalid parameters",
			req: func() *csi.CreateVolumeRequest {
				req := subdirCreateRequest("pvc-1")
				req.Parameters = map[string]string{paramClusterID: "a/b", paramParentVolume: "vol"}
				return req
			}(),
			wantCode:       codes.InvalidArgument,
			wantErrMessage: "invalid storage class parameters",
		},
		{
			name: "missing cluster parameter",
			req: func() *csi.CreateVolumeRequest {
				req := subdirCreateRequest("pvc-1")
				req.Parameters = map[string]string{paramParentVolume: "vol"}
				return req
			}(),
			wantCode:       codes.InvalidArgument,
			wantErrMessage: "clusterID is required",
		},
		{
			name: "cluster is not configured",
			req: func() *csi.CreateVolumeRequest {
				req := subdirCreateRequest("pvc-1")
				req.Parameters = map[string]string{paramClusterID: "missing", paramParentVolume: "vol"}
				return req
			}(),
			wantCode:       codes.InvalidArgument,
			wantErrMessage: `cluster "missing" is not configured`,
		},
		{
			name: "forbidden mount flag",
			req: func() *csi.CreateVolumeRequest {
				req := subdirCreateRequest("pvc-1")
				req.VolumeCapabilities = []*csi.VolumeCapability{
					accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, "rw"),
				}
				return req
			}(),
			wantCode:       codes.InvalidArgument,
			wantErrMessage: "mount flags are not supported",
		},
		{
			name: "quota is not supported yet",
			req: func() *csi.CreateVolumeRequest {
				req := subdirCreateRequest("pvc-1")
				req.Parameters = map[string]string{
					paramClusterID:    "c1",
					paramParentVolume: "vol",
					paramQuota:        "true",
				}
				return req
			}(),
			wantCode:       codes.InvalidArgument,
			wantErrMessage: "not supported yet",
		},
		{
			name: "a volume mode StorageClass is not supported yet",
			req: func() *csi.CreateVolumeRequest {
				req := subdirCreateRequest("pvc-1")
				req.Parameters = map[string]string{paramClusterID: "c1", paramProvisioningMode: "volume"}
				return req
			}(),
			wantCode:       codes.InvalidArgument,
			wantErrMessage: `provisioningMode "volume" is not supported yet`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			service, fake := newTestController(t)

			resp, err := service.CreateVolume(context.Background(), tc.req)

			if tc.wantCode != codes.OK {
				if err == nil {
					t.Fatalf("CreateVolume() = %+v, nil, want %v", resp, tc.wantCode)
				}
				if got := status.Code(err); got != tc.wantCode {
					t.Fatalf("CreateVolume() = %v (code %v), want %v", err, got, tc.wantCode)
				}
				if !strings.Contains(status.Convert(err).Message(), tc.wantErrMessage) {
					t.Fatalf("CreateVolume() message = %q, want it to contain %q",
						status.Convert(err).Message(), tc.wantErrMessage)
				}
				if resp != nil {
					t.Fatalf("CreateVolume() = %+v with an error, want nil", resp)
				}
				// A rejected request must not touch the parent volume at all.
				if got := fake.mountStateCount() + fake.mountCount(); got != 0 {
					t.Fatalf("mounter calls = %d, want none for a rejected request", got)
				}
				if _, statErr := os.Lstat(filepath.Join(parentVolumePath(t, service, "c1", "vol"), "pvc-1")); !errors.Is(statErr, fs.ErrNotExist) {
					t.Fatalf("the volume directory exists (%v), want none", statErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("CreateVolume() = %v, want nil", err)
			}
		})
	}
}

func TestCreateVolumeCreatesDirectoryAndMetadata(t *testing.T) {
	service, fake := newTestControllerMounting(t)
	req := withCapacity(withOnDelete(subdirCreateRequest("pvc-1"), deletePolicyArchive), 1<<30, 2<<30)

	resp, err := service.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolume() = %v, want nil", err)
	}

	want := &csi.Volume{
		VolumeId:      testVolumeID,
		CapacityBytes: 1 << 30,
		VolumeContext: map[string]string{
			attrClusterID: "c1",
			attrVolume:    "vol",
			attrSubdir:    "pvc-1",
		},
	}
	if got := resp.GetVolume(); !proto.Equal(got, want) {
		t.Fatalf("Volume = %v, want %v", got, want)
	}

	// The parent volume was mounted exactly once, with the servers of the cluster.
	if got := fake.mountCount(); got != 1 {
		t.Fatalf("MountGlusterfs calls = %d, want 1", got)
	}
	if got := fake.mountCalls[0].spec; got.Volume != "vol" || !slices.Equal(got.Servers, testServers) {
		t.Fatalf("mount spec = %+v, want volume vol of servers %v", got, testServers)
	}

	// The directory exists with the permission of D-27 ...
	dir := volumeDirOf(t, service, "pvc-1")
	wantVolumeDirMode(t, dir)

	// ... and the metadata holds the capacity and the delete policy of the request.
	meta, found := volumeMetaOf(t, service, "pvc-1")
	if !found {
		t.Fatal("the metadata was not written")
	}
	if wantMeta := (volumeMeta{Version: metaVersion, CapacityBytes: 1 << 30, OnDelete: deletePolicyArchive}); meta != wantMeta {
		t.Fatalf("metadata = %+v, want %+v", meta, wantMeta)
	}
	raw, readErr := os.ReadFile(filepath.Join(parentVolumePath(t, service, "c1", "vol"), filepath.FromSlash(metaFileName("pvc-1"))))
	if readErr != nil {
		t.Fatalf("ReadFile() = %v, want nil", readErr)
	}
	if want := `{"version":1,"capacityBytes":1073741824,"onDelete":"archive"}`; string(raw) != want {
		t.Fatalf("metadata file = %s, want %s", raw, want)
	}
}

func TestCreateVolumeIsIdempotent(t *testing.T) {
	service, fake := newTestControllerMounting(t)
	ctx := context.Background()
	req := withCapacity(subdirCreateRequest("pvc-1"), 1<<30, 2<<30)

	first, err := service.CreateVolume(ctx, req)
	if err != nil {
		t.Fatalf("CreateVolume() = %v, want nil", err)
	}
	second, err := service.CreateVolume(ctx, withCapacity(subdirCreateRequest("pvc-1"), 1<<30, 2<<30))
	if err != nil {
		t.Fatalf("the repeated CreateVolume() = %v, want nil", err)
	}
	if !proto.Equal(first.GetVolume(), second.GetVolume()) {
		t.Fatalf("the repeated response = %v, want %v", second.GetVolume(), first.GetVolume())
	}
	// The parent volume is mounted once and stays mounted, so the second request
	// costs no mount at all.
	if got := fake.mountCount(); got != 1 {
		t.Fatalf("MountGlusterfs calls = %d, want 1", got)
	}
}

func TestCreateVolumeCapacity(t *testing.T) {
	tests := []struct {
		name              string
		required, limit   int64
		wantCapacityBytes int64
	}{
		{name: "no capacity range at all", required: 0, limit: 0, wantCapacityBytes: 0},
		{name: "only a required capacity", required: 1 << 30, limit: 0, wantCapacityBytes: 1 << 30},
		{name: "only a limit", required: 0, limit: 4 << 30, wantCapacityBytes: 4 << 30},
		{name: "a required capacity within the limit", required: 1 << 30, limit: 8 << 30, wantCapacityBytes: 1 << 30},
		{name: "a required capacity at the limit", required: 8 << 30, limit: 8 << 30, wantCapacityBytes: 8 << 30},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			service, _ := newTestController(t)

			resp, err := service.CreateVolume(context.Background(),
				withCapacity(subdirCreateRequest("pvc-1"), tc.required, tc.limit))
			if err != nil {
				t.Fatalf("CreateVolume() = %v, want nil", err)
			}
			if got := resp.GetVolume().GetCapacityBytes(); got != tc.wantCapacityBytes {
				t.Fatalf("CapacityBytes = %d, want %d", got, tc.wantCapacityBytes)
			}
			meta, found := volumeMetaOf(t, service, "pvc-1")
			if !found {
				t.Fatal("the metadata was not written")
			}
			if meta.CapacityBytes != tc.wantCapacityBytes {
				t.Fatalf("recorded capacity = %d, want %d", meta.CapacityBytes, tc.wantCapacityBytes)
			}
		})
	}
}

func TestCreateVolumeCompatibility(t *testing.T) {
	tests := []struct {
		name string
		// recorded is the metadata of the volume that already exists.
		recorded volumeMeta
		// required and limit are the capacity range of the request.
		required, limit int64
		policy          deletePolicy
		wantCode        codes.Code
		// wantCapacity is the capacity of the response. A repeated request reports
		// what is recorded, not what it asked for: that is what idempotency means.
		wantCapacity int64
		// wantReason is a substring of the error message of a refused request.
		wantReason string
	}{
		{
			name:     "the same request",
			recorded: volumeMeta{Version: metaVersion, CapacityBytes: 1 << 30, OnDelete: deletePolicyDelete},
			required: 1 << 30, limit: 2 << 30, policy: deletePolicyDelete,
			wantCode: codes.OK, wantCapacity: 1 << 30,
		},
		{
			name:     "no capacity range at all",
			recorded: volumeMeta{Version: metaVersion, CapacityBytes: 1 << 30, OnDelete: deletePolicyDelete},
			required: 0, limit: 0, policy: deletePolicyDelete,
			wantCode: codes.OK, wantCapacity: 1 << 30,
		},
		{
			name:     "a smaller required capacity",
			recorded: volumeMeta{Version: metaVersion, CapacityBytes: 1 << 30, OnDelete: deletePolicyDelete},
			required: 1 << 20, limit: 0, policy: deletePolicyDelete,
			wantCode: codes.OK, wantCapacity: 1 << 30,
		},
		{
			name:     "a limit at the recorded capacity",
			recorded: volumeMeta{Version: metaVersion, CapacityBytes: 1 << 30, OnDelete: deletePolicyDelete},
			required: 0, limit: 1 << 30, policy: deletePolicyDelete,
			wantCode: codes.OK, wantCapacity: 1 << 30,
		},
		{
			name:     "a larger required capacity",
			recorded: volumeMeta{Version: metaVersion, CapacityBytes: 1 << 30, OnDelete: deletePolicyDelete},
			required: 2 << 30, limit: 4 << 30, policy: deletePolicyDelete,
			wantCode:   codes.AlreadyExists,
			wantReason: "capacity 1073741824 is below the required 2147483648",
		},
		{
			name:     "a limit below the recorded capacity",
			recorded: volumeMeta{Version: metaVersion, CapacityBytes: 1 << 30, OnDelete: deletePolicyDelete},
			required: 0, limit: 1 << 20, policy: deletePolicyDelete,
			wantCode:   codes.AlreadyExists,
			wantReason: "capacity 1073741824 is above the limit 1048576",
		},
		{
			name:     "an unknown recorded capacity with a required one",
			recorded: volumeMeta{Version: metaVersion, CapacityBytes: 0, OnDelete: deletePolicyDelete},
			required: 1 << 30, limit: 0, policy: deletePolicyDelete,
			wantCode:   codes.AlreadyExists,
			wantReason: "capacity 0 is below the required 1073741824",
		},
		{
			name:     "another delete policy",
			recorded: volumeMeta{Version: metaVersion, CapacityBytes: 1 << 30, OnDelete: deletePolicyRetain},
			required: 1 << 30, limit: 2 << 30, policy: deletePolicyDelete,
			wantCode:   codes.AlreadyExists,
			wantReason: `onDelete "delete" was requested, the volume uses "retain"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			service, _ := newTestController(t)
			seedVolume(t, service, "pvc-1", tc.recorded, map[string]string{"keep.txt": "keep"})

			req := withCapacity(withOnDelete(subdirCreateRequest("pvc-1"), tc.policy), tc.required, tc.limit)
			resp, err := service.CreateVolume(context.Background(), req)

			if tc.wantCode != codes.OK {
				wantCode(t, err, tc.wantCode)
				message := status.Convert(err).Message()
				if !strings.Contains(message, "does not satisfy the request") {
					t.Fatalf("CreateVolume() message = %q, want the mismatch", message)
				}
				if !strings.Contains(message, tc.wantReason) {
					t.Fatalf("CreateVolume() message = %q, want it to contain %q", message, tc.wantReason)
				}
				// The existing volume is not modified by a refused request.
				meta, found := volumeMetaOf(t, service, "pvc-1")
				if !found || meta != tc.recorded {
					t.Fatalf("metadata = %+v (%t), want %+v", meta, found, tc.recorded)
				}
				if content, readErr := os.ReadFile(filepath.Join(volumeDirOf(t, service, "pvc-1"), "keep.txt")); readErr != nil || string(content) != "keep" {
					t.Fatalf("the volume content = %q (%v), want it untouched", content, readErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("CreateVolume() = %v, want nil", err)
			}
			if got := resp.GetVolume().GetCapacityBytes(); got != tc.wantCapacity {
				t.Fatalf("CapacityBytes = %d, want %d", got, tc.wantCapacity)
			}
			if got := resp.GetVolume().GetVolumeId(); got != testVolumeID {
				t.Fatalf("VolumeId = %q, want %q", got, testVolumeID)
			}
		})
	}
}

func TestCreateVolumeKeepsForeignDirectory(t *testing.T) {
	service, _ := newTestController(t)
	dir := createVolumeDir(t, parentVolumePath(t, service, "c1", "vol"), "pvc-1", 0o755)
	writeTestFile(t, dir, "data.txt", "somebody else's data")

	_, err := service.CreateVolume(context.Background(), subdirCreateRequest("pvc-1"))
	wantCode(t, err, codes.AlreadyExists)
	if msg := status.Convert(err).Message(); !strings.Contains(msg, "was not created by this driver") {
		t.Fatalf("CreateVolume() message = %q, want it to say the directory is not ours", msg)
	}

	// Neither the directory nor its content may change, and no metadata is written:
	// the driver does not adopt a directory it did not create (D-25).
	content, readErr := os.ReadFile(filepath.Join(dir, "data.txt"))
	if readErr != nil || string(content) != "somebody else's data" {
		t.Fatalf("directory content = %q (%v), want it untouched", content, readErr)
	}
	if entries, readErr := os.ReadDir(dir); readErr != nil || len(entries) != 1 {
		t.Fatalf("directory entries = %v (%v), want only data.txt", namesOf(entries), readErr)
	}
	if _, found := volumeMetaOf(t, service, "pvc-1"); found {
		t.Fatal("metadata was written for a directory this driver did not create")
	}
}

func TestCreateVolumeRepairsMissingDirectory(t *testing.T) {
	service, _ := newTestController(t)
	root, err := service.parentRoot(context.Background(), configClusterForTest(t, "c1"), "vol")
	if err != nil {
		t.Fatalf("parentRoot() = %v, want nil", err)
	}
	// The crash window of CreateVolume: the metadata is there, the directory is not.
	meta := volumeMeta{Version: metaVersion, CapacityBytes: 1 << 30, OnDelete: deletePolicyDelete}
	if writeErr := writeVolumeMeta(root, "pvc-1", meta); writeErr != nil {
		t.Fatalf("writeVolumeMeta() = %v, want nil", writeErr)
	}

	resp, err := service.CreateVolume(context.Background(),
		withCapacity(subdirCreateRequest("pvc-1"), 1<<30, 0))
	if err != nil {
		t.Fatalf("CreateVolume() = %v, want nil", err)
	}
	if got := resp.GetVolume().GetCapacityBytes(); got != 1<<30 {
		t.Fatalf("CapacityBytes = %d, want %d", got, 1<<30)
	}
	wantVolumeDirMode(t, volumeDirOf(t, service, "pvc-1"))
}

func TestCreateVolumeBrokenMetadata(t *testing.T) {
	service, _ := newTestController(t)
	parent := parentVolumePath(t, service, "c1", "vol")
	writeTestFile(t, parent, metaFileName("pvc-1"), `{"version":99}`)

	_, err := service.CreateVolume(context.Background(), subdirCreateRequest("pvc-1"))
	wantCode(t, err, codes.Internal)
	if !strings.Contains(status.Convert(err).Message(), "read the volume metadata") {
		t.Fatalf("CreateVolume() message = %q, want it to name the metadata", status.Convert(err).Message())
	}
	// A broken metadata file is never replaced, and no directory is created.
	if _, statErr := os.Lstat(filepath.Join(parent, "pvc-1")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("the volume directory exists (%v), want none", statErr)
	}
	raw, readErr := os.ReadFile(filepath.Join(parent, filepath.FromSlash(metaFileName("pvc-1"))))
	if readErr != nil || string(raw) != `{"version":99}` {
		t.Fatalf("metadata file = %q (%v), want it untouched", raw, readErr)
	}
}

func TestCreateVolumeMetadataWriteFailureKeepsNoDirectory(t *testing.T) {
	service, _ := newTestController(t)
	parent := parentVolumePath(t, service, "c1", "vol")
	// A directory where the temporary metadata file belongs: reading the metadata
	// still reports "not found", but writing it cannot work.
	createVolumeDir(t, parent, metaTempFileName("pvc-1"), 0o755)

	_, err := service.CreateVolume(context.Background(), subdirCreateRequest("pvc-1"))
	wantCode(t, err, codes.Internal)
	if !strings.Contains(status.Convert(err).Message(), "write the volume metadata") {
		t.Fatalf("CreateVolume() message = %q, want it to name the write", status.Convert(err).Message())
	}

	// The order matters: metadata first, directory second. A failed metadata write
	// must not leave a directory behind, because a directory without metadata is a
	// volume DeleteVolume refuses to remove.
	if _, statErr := os.Lstat(filepath.Join(parent, "pvc-1")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("the volume directory exists (%v), want none after a failed metadata write", statErr)
	}
}

func TestCreateVolumeLocked(t *testing.T) {
	service, _ := newTestController(t)
	if !service.locks.tryAcquire(testVolumeID) {
		t.Fatal("could not hold the in-flight lock")
	}
	defer service.locks.release(testVolumeID)

	_, err := service.CreateVolume(context.Background(), subdirCreateRequest("pvc-1"))
	wantCode(t, err, codes.Aborted)
}

func TestCreateVolumeCancelled(t *testing.T) {
	service, fake := newTestControllerMounting(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := service.CreateVolume(ctx, subdirCreateRequest("pvc-1"))
	wantCode(t, err, codes.Canceled)

	// A cancelled request does not mount anything and leaves the lock free.
	if got := fake.mountCount(); got != 0 {
		t.Fatalf("MountGlusterfs calls = %d, want none", got)
	}
	if !service.locks.tryAcquire(testVolumeID) {
		t.Fatal("the in-flight lock is still held after a cancelled request")
	}
	service.locks.release(testVolumeID)
}

func TestCreateVolumeMountFailure(t *testing.T) {
	service, fake := newTestControllerMounting(t)
	fake.mountErrs = []error{errors.New("no route to host")}

	_, err := service.CreateVolume(context.Background(), subdirCreateRequest("pvc-1"))
	wantCode(t, err, codes.Internal)
	if !strings.Contains(status.Convert(err).Message(), "mount parent volume") {
		t.Fatalf("CreateVolume() message = %q, want it to name the mount", status.Convert(err).Message())
	}
	if _, statErr := os.Lstat(filepath.Join(parentVolumePath(t, service, "c1", "vol"), "pvc-1")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("the volume directory exists (%v), want none", statErr)
	}
}

func TestCreateVolumeWithoutParentMounts(t *testing.T) {
	// A controller service built by hand may have no parent volume cache; the RPC
	// reports that instead of dereferencing a nil pointer.
	service := &controllerService{driver: mustControllerDriver(t), locks: newInflight()}

	_, err := service.CreateVolume(context.Background(), subdirCreateRequest("pvc-1"))
	wantCode(t, err, codes.Internal)
}

func TestDeleteVolumeValidation(t *testing.T) {
	tests := []struct {
		name     string
		req      *csi.DeleteVolumeRequest
		wantCode codes.Code
	}{
		{
			name:     "empty volume id",
			req:      &csi.DeleteVolumeRequest{},
			wantCode: codes.InvalidArgument,
		},
		{
			// An id this driver never issued is a volume that does not exist, and
			// deleting a volume that does not exist is success.
			name:     "undecodable volume id",
			req:      &csi.DeleteVolumeRequest{VolumeId: "some-vol-id"},
			wantCode: codes.OK,
		},
		{
			name:     "volume id with a bad prefix",
			req:      &csi.DeleteVolumeRequest{VolumeId: "abc:v1:subdir:c1:vol:pvc-1"},
			wantCode: codes.OK,
		},
		{
			name:     "static volume id",
			req:      &csi.DeleteVolumeRequest{VolumeId: testStaticVolumeID},
			wantCode: codes.OK,
		},
		{
			name:     "volume mode id",
			req:      &csi.DeleteVolumeRequest{VolumeId: testVolumeModeVolumeID},
			wantCode: codes.OK,
		},
		{
			name:     "cluster is not configured",
			req:      &csi.DeleteVolumeRequest{VolumeId: "gfs:v1:subdir:missing:vol:pvc-1"},
			wantCode: codes.FailedPrecondition,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			service, fake := newTestController(t)

			resp, err := service.DeleteVolume(context.Background(), tc.req)
			wantCode(t, err, tc.wantCode)
			if tc.wantCode == codes.OK {
				if resp == nil {
					t.Fatal("DeleteVolume() = nil, want an empty response")
				}
				if got := fake.callLog(); len(got) != 0 {
					t.Fatalf("mounter calls = %v, want none", got)
				}
			} else if resp != nil {
				t.Fatalf("DeleteVolume() = %+v with an error, want nil", resp)
			}
		})
	}
}

func TestDeleteVolumePolicies(t *testing.T) {
	tests := []struct {
		name   string
		policy deletePolicy
		// wantArchived is the name of the archived directory, empty for the other
		// policies.
		wantArchived string
		wantDirKept  bool
	}{
		{name: "delete removes the directory", policy: deletePolicyDelete},
		{name: "retain keeps the directory", policy: deletePolicyRetain, wantDirKept: true},
		{
			name:         "archive moves the directory",
			policy:       deletePolicyArchive,
			wantArchived: "pvc-1-20261102T030405.000000006Z",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			service, fake := newTestControllerMounting(t)
			service.now = func() time.Time { return testArchiveTime }
			seedVolume(t, service, "pvc-1",
				volumeMeta{Version: metaVersion, CapacityBytes: 1 << 30, OnDelete: tc.policy},
				map[string]string{"data/file.txt": "payload"})
			dir := volumeDirOf(t, service, "pvc-1")

			if _, err := service.DeleteVolume(context.Background(),
				&csi.DeleteVolumeRequest{VolumeId: testVolumeID}); err != nil {
				t.Fatalf("DeleteVolume() = %v, want nil", err)
			}

			// The metadata is gone in every case: the driver does not claim the name
			// any more.
			if _, found := volumeMetaOf(t, service, "pvc-1"); found {
				t.Fatal("the metadata still exists")
			}

			switch {
			case tc.wantDirKept:
				content, readErr := os.ReadFile(filepath.Join(dir, "data", "file.txt"))
				if readErr != nil || string(content) != "payload" {
					t.Fatalf("volume content = %q (%v), want it kept", content, readErr)
				}
			case tc.wantArchived != "":
				if _, statErr := os.Lstat(dir); !errors.Is(statErr, fs.ErrNotExist) {
					t.Fatalf("the volume directory still exists (%v), want it archived", statErr)
				}
				archived := filepath.Join(parentVolumePath(t, service, "c1", "vol"), filepath.FromSlash(metaArchiveDir), tc.wantArchived)
				content, readErr := os.ReadFile(filepath.Join(archived, "data", "file.txt"))
				if readErr != nil || string(content) != "payload" {
					t.Fatalf("archived content = %q (%v), want the volume content", content, readErr)
				}
			default:
				if _, statErr := os.Lstat(dir); !errors.Is(statErr, fs.ErrNotExist) {
					t.Fatalf("the volume directory still exists (%v), want it removed", statErr)
				}
			}

			// The parent volume was mounted once for the whole test.
			if got := fake.mountCount(); got != 1 {
				t.Fatalf("MountGlusterfs calls = %d, want 1", got)
			}

			// A repeated DeleteVolume succeeds and changes nothing.
			if _, err := service.DeleteVolume(context.Background(),
				&csi.DeleteVolumeRequest{VolumeId: testVolumeID}); err != nil {
				t.Fatalf("the repeated DeleteVolume() = %v, want nil", err)
			}

			// A volume that was retained keeps its directory, so creating the same
			// name again is refused: it is not this driver's directory any more.
			if tc.wantDirKept {
				_, err := service.CreateVolume(context.Background(), subdirCreateRequest("pvc-1"))
				wantCode(t, err, codes.AlreadyExists)
			}
		})
	}
}

func TestDeleteVolumeKeepsForeignDirectory(t *testing.T) {
	service, _ := newTestController(t)
	dir := createVolumeDir(t, parentVolumePath(t, service, "c1", "vol"), "pvc-1", 0o755)
	writeTestFile(t, dir, "data.txt", "somebody else's data")

	resp, err := service.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: testVolumeID})
	if err != nil {
		t.Fatalf("DeleteVolume() = %v, want nil", err)
	}
	if resp == nil {
		t.Fatal("DeleteVolume() = nil, want an empty response")
	}

	// A directory without metadata was not created by this driver: it is kept, and
	// the log line says why (D-25).
	content, readErr := os.ReadFile(filepath.Join(dir, "data.txt"))
	if readErr != nil || string(content) != "somebody else's data" {
		t.Fatalf("directory content = %q (%v), want it untouched", content, readErr)
	}
}

func TestDeleteVolumeBrokenMetadata(t *testing.T) {
	service, _ := newTestController(t)
	parent := parentVolumePath(t, service, "c1", "vol")
	dir := createVolumeDir(t, parent, "pvc-1", 0o777)
	writeTestFile(t, dir, "data.txt", "payload")
	writeTestFile(t, parent, metaFileName("pvc-1"), "not json")

	_, err := service.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: testVolumeID})
	wantCode(t, err, codes.Internal)
	if !strings.Contains(status.Convert(err).Message(), "read the volume metadata") {
		t.Fatalf("DeleteVolume() message = %q, want it to name the metadata", status.Convert(err).Message())
	}

	// Nothing may be removed when the metadata cannot be read: it decides what
	// happens to the directory.
	if content, readErr := os.ReadFile(filepath.Join(dir, "data.txt")); readErr != nil || string(content) != "payload" {
		t.Fatalf("volume content = %q (%v), want it untouched", content, readErr)
	}
}

func TestDeleteVolumeDirectoryErrorKeepsMetadata(t *testing.T) {
	service, _ := newTestController(t)
	// A regular file where the volume directory should be: the metadata is there,
	// the directory cannot be handled, and the call must not remove the metadata.
	parent := parentVolumePath(t, service, "c1", "vol")
	writeTestFile(t, parent, "pvc-1", "not a directory")
	root, err := service.parentRoot(context.Background(), configClusterForTest(t, "c1"), "vol")
	if err != nil {
		t.Fatalf("parentRoot() = %v, want nil", err)
	}
	meta := volumeMeta{Version: metaVersion, OnDelete: deletePolicyDelete}
	if err := writeVolumeMeta(root, "pvc-1", meta); err != nil {
		t.Fatalf("writeVolumeMeta() = %v, want nil", err)
	}

	_, delErr := service.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: testVolumeID})
	wantCode(t, delErr, codes.Internal)

	if got, found := volumeMetaOf(t, service, "pvc-1"); !found || got != meta {
		t.Fatalf("metadata = %+v (%t), want it kept", got, found)
	}
}

func TestDeleteVolumeCancelledKeepsMetadata(t *testing.T) {
	service, fake := newTestControllerMounting(t)
	seedVolumeUnmounted(t, service, "pvc-1",
		volumeMeta{Version: metaVersion, OnDelete: deletePolicyDelete},
		map[string]string{"data.txt": "payload"})

	// The mount of the parent volume cancels the request, so the cancellation wins
	// over the removal of the directory: the metadata has to survive.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake.onCreate = func(target string) error {
		cancel()
		return os.MkdirAll(target, 0o750)
	}

	_, err := service.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: testVolumeID})
	wantCode(t, err, codes.Canceled)

	// The metadata is kept, so a retried DeleteVolume continues where this one
	// stopped instead of leaving a directory this driver no longer knows about.
	if _, found := volumeMetaOf(t, service, "pvc-1"); !found {
		t.Fatal("the metadata was removed by a cancelled DeleteVolume")
	}
	content, readErr := os.ReadFile(filepath.Join(volumeDirOf(t, service, "pvc-1"), "data.txt"))
	if readErr != nil || string(content) != "payload" {
		t.Fatalf("volume content = %q (%v), want it kept", content, readErr)
	}
}

func TestDeleteVolumeLocked(t *testing.T) {
	service, _ := newTestController(t)
	seedVolume(t, service, "pvc-1", volumeMeta{Version: metaVersion, OnDelete: deletePolicyDelete}, nil)
	if !service.locks.tryAcquire(testVolumeID) {
		t.Fatal("could not hold the in-flight lock")
	}
	defer service.locks.release(testVolumeID)

	_, err := service.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: testVolumeID})
	wantCode(t, err, codes.Aborted)
}

func TestDeleteVolumeMountFailure(t *testing.T) {
	service, fake := newTestControllerMounting(t)
	fake.mountErrs = []error{errors.New("no route to host")}

	_, err := service.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: testVolumeID})
	wantCode(t, err, codes.Internal)
	if !strings.Contains(status.Convert(err).Message(), "mount parent volume") {
		t.Fatalf("DeleteVolume() message = %q, want it to name the mount", status.Convert(err).Message())
	}
}

func TestValidateVolumeCapabilitiesExistence(t *testing.T) {
	tests := []struct {
		name string
		// prepare sets up the parent volume.
		prepare   func(t *testing.T, service *controllerService)
		wantCode  codes.Code
		wantFound bool
	}{
		{
			name: "created volume is confirmed",
			prepare: func(t *testing.T, service *controllerService) {
				if _, err := service.CreateVolume(context.Background(), subdirCreateRequest("pvc-1")); err != nil {
					t.Fatalf("CreateVolume() = %v, want nil", err)
				}
			},
			wantCode:  codes.OK,
			wantFound: true,
		},
		{
			name: "deleted volume is not found",
			prepare: func(t *testing.T, service *controllerService) {
				if _, err := service.CreateVolume(context.Background(), subdirCreateRequest("pvc-1")); err != nil {
					t.Fatalf("CreateVolume() = %v, want nil", err)
				}
				if _, err := service.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: testVolumeID}); err != nil {
					t.Fatalf("DeleteVolume() = %v, want nil", err)
				}
			},
			wantCode: codes.NotFound,
		},
		{
			name: "metadata without a directory is not found",
			prepare: func(t *testing.T, service *controllerService) {
				root, err := service.parentRoot(context.Background(), configClusterForTest(t, "c1"), "vol")
				if err != nil {
					t.Fatalf("parentRoot() = %v, want nil", err)
				}
				if err := writeVolumeMeta(root, "pvc-1", volumeMeta{Version: metaVersion, OnDelete: deletePolicyDelete}); err != nil {
					t.Fatalf("writeVolumeMeta() = %v, want nil", err)
				}
			},
			wantCode: codes.NotFound,
		},
		{
			name:     "never created volume is not found",
			prepare:  func(*testing.T, *controllerService) {},
			wantCode: codes.NotFound,
		},
		{
			// A directory without metadata is somebody else's directory, not a volume
			// of this driver (D-25).
			name: "a directory without metadata is not found",
			prepare: func(t *testing.T, service *controllerService) {
				createVolumeDir(t, parentVolumePath(t, service, "c1", "vol"), "pvc-1", 0o777)
			},
			wantCode: codes.NotFound,
		},
		{
			name: "a regular file where the directory belongs is Internal",
			prepare: func(t *testing.T, service *controllerService) {
				writeTestFile(t, parentVolumePath(t, service, "c1", "vol"), "pvc-1", "not a directory")
			},
			wantCode: codes.Internal,
		},
		{
			name: "broken metadata is Internal",
			prepare: func(t *testing.T, service *controllerService) {
				writeTestFile(t, parentVolumePath(t, service, "c1", "vol"), metaFileName("pvc-1"), "not json")
			},
			wantCode: codes.Internal,
		},
		{
			name:      "static volume is confirmed without a backend",
			prepare:   func(*testing.T, *controllerService) {},
			wantCode:  codes.OK,
			wantFound: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			service, fake := newTestControllerMounting(t)
			tc.prepare(t, service)

			volumeID := testVolumeID
			if strings.HasPrefix(tc.name, "static") {
				volumeID = testStaticVolumeID
			}
			resp, err := service.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           volumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
			})
			wantCode(t, err, tc.wantCode)
			if tc.wantCode != codes.OK {
				return
			}
			if got := resp.GetConfirmed() != nil; got != tc.wantFound {
				t.Fatalf("Confirmed = %t (%q), want %t", got, resp.GetMessage(), tc.wantFound)
			}
			if strings.HasPrefix(tc.name, "static") && fake.mountCount() != 0 {
				t.Fatalf("a static volume mounted a parent volume: %d mounts", fake.mountCount())
			}
		})
	}
}

func TestControllerCreateDeleteThroughGRPC(t *testing.T) {
	d, fake := mustControllerDriverWithFake(t)
	client := csi.NewControllerClient(bufconnConn(t, d))
	ctx := context.Background()

	created, err := client.CreateVolume(ctx, withCapacity(subdirCreateRequest("pvc-1"), 1<<30, 2<<30))
	if err != nil {
		t.Fatalf("CreateVolume() = %v, want nil", err)
	}
	volumeID := created.GetVolume().GetVolumeId()
	if volumeID != testVolumeID {
		t.Fatalf("VolumeId = %q, want %q", volumeID, testVolumeID)
	}
	if got := created.GetVolume().GetVolumeContext()[attrSubdir]; got != "pvc-1" {
		t.Fatalf("volume context subdir = %q, want %q", got, "pvc-1")
	}

	validated, err := client.ValidateVolumeCapabilities(ctx, &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId:           volumeID,
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
		Parameters:         subdirParameters(),
	})
	if err != nil {
		t.Fatalf("ValidateVolumeCapabilities() = %v, want nil", err)
	}
	if validated.GetConfirmed() == nil {
		t.Fatalf("Confirmed = nil (%q), want the volume confirmed", validated.GetMessage())
	}

	if _, delErr := client.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: volumeID}); delErr != nil {
		t.Fatalf("DeleteVolume() = %v, want nil", delErr)
	}

	// After the delete the volume is gone, and the parent volume was mounted only
	// once for the whole sequence.
	_, err = client.ValidateVolumeCapabilities(ctx, &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId:           volumeID,
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
	})
	wantCode(t, err, codes.NotFound)
	if got := fake.mountCount(); got > 1 {
		t.Fatalf("MountGlusterfs calls = %d, want at most 1", got)
	}
}

func TestCreateVolumeSecretsNotLogged(t *testing.T) {
	// V(5) is the only verbosity at which the interceptors log the request.
	buf := captureKlog(t, "5")

	d, _ := mustControllerDriverWithFake(t)
	client := csi.NewControllerClient(bufconnConn(t, d))
	const secret = "sup3r-s3cret"
	req := subdirCreateRequest("pvc-1")
	req.Secrets = map[string]string{"password": secret}

	if _, err := client.CreateVolume(context.Background(), req); err != nil {
		t.Fatalf("CreateVolume() = %v, want nil", err)
	}

	logged := buf.String()
	if !strings.Contains(logged, "CSI request succeeded") {
		t.Fatalf("log = %q, want a success line at V(5)", logged)
	}
	if strings.Contains(logged, secret) {
		t.Fatalf("log = %q, must not contain the secret value", logged)
	}
}

// TestDeleteVolumeKeepsMetadataOnDirectoryFailure checks the ordering of D-25 on
// every policy: a failure while the directory is handled must leave the metadata in
// place, so that a retried DeleteVolume can finish the job.
func TestDeleteVolumeKeepsMetadataOnDirectoryFailure(t *testing.T) {
	tests := []struct {
		name   string
		policy deletePolicy
		// prepare makes the directory handling of the policy fail.
		prepare func(t *testing.T, service *controllerService, dir string)
		wantOp  string
	}{
		{
			name:   "delete cannot remove the directory",
			policy: deletePolicyDelete,
			prepare: func(t *testing.T, _ *controllerService, dir string) {
				// A read-only volume directory makes the removal of its entries fail.
				if runtime.GOOS == "windows" {
					t.Skip("a read-only directory does not prevent removing its entries on Windows")
				}
				if os.Geteuid() == 0 {
					t.Skip("root ignores the permission of the volume directory")
				}
				if err := os.Chmod(dir, 0o555); err != nil {
					t.Fatalf("Chmod() = %v, want nil", err)
				}
				t.Cleanup(func() { _ = os.Chmod(dir, 0o777) })
			},
			wantOp: "remove the volume directory",
		},
		{
			name:   "archive cannot move the directory",
			policy: deletePolicyArchive,
			prepare: func(t *testing.T, service *controllerService, _ string) {
				// The archive directory cannot be created, so the rename fails.
				writeTestFile(t, parentVolumePath(t, service, "c1", "vol"), metaArchiveDir, "not a directory")
			},
			wantOp: "archive the volume directory",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			service, _ := newTestController(t)
			meta := volumeMeta{Version: metaVersion, OnDelete: tc.policy}
			dir := seedVolume(t, service, "pvc-1", meta, map[string]string{"file.txt": "payload"})
			tc.prepare(t, service, dir)

			_, err := service.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: testVolumeID})
			wantCode(t, err, codes.Internal)
			if !strings.Contains(status.Convert(err).Message(), tc.wantOp) {
				t.Fatalf("DeleteVolume() = %q, want it to name %q", status.Convert(err).Message(), tc.wantOp)
			}

			// The metadata is what a retry works from, so it has to survive.
			if got, found := volumeMetaOf(t, service, "pvc-1"); !found || got != meta {
				t.Fatalf("metadata = %+v (%t), want it kept as %+v", got, found, meta)
			}
		})
	}
}

// TestNowTime checks the clock of the controller service: the production service
// uses the real one, a service built by hand falls back to it, and a test may
// replace it.
func TestNowTime(t *testing.T) {
	if got := (&controllerService{}).nowTime(); got.IsZero() {
		t.Fatal("nowTime() = the zero time, want the current time")
	}

	fixed := time.Date(2026, time.May, 6, 7, 8, 9, 0, time.UTC)
	service := &controllerService{now: func() time.Time { return fixed }}
	if got := service.nowTime(); !got.Equal(fixed) {
		t.Fatalf("nowTime() = %s, want %s", got, fixed)
	}
}

// TestDeleteVolumeMetadataRemovalFailure checks that a failed metadata removal is
// reported instead of being swallowed: the CO retries, and only a reported failure
// tells it that the volume is not fully gone.
//
// The failure is produced by taking the write permission of the metadata directory
// away, so the test needs a process that is not root, where permissions are not
// enforced.
func TestDeleteVolumeMetadataRemovalFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("removing a file from a read-only directory is not prevented on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission of the metadata directory")
	}

	service, _ := newTestController(t)
	parent := parentVolumePath(t, service, "c1", "vol")
	seedVolume(t, service, "pvc-1",
		volumeMeta{Version: metaVersion, OnDelete: deletePolicyRetain}, nil)
	volumesDir := filepath.Join(parent, filepath.FromSlash(metaVolumesDir))

	// The removal has to fail for this test to mean anything: it is checked with a
	// throwaway file before the RPC runs, so that a process the permission does not
	// apply to skips the test instead of passing it vacuously.
	probe := writeTestFile(t, volumesDir, "probe", "x")
	if err := os.Chmod(volumesDir, 0o555); err != nil {
		t.Fatalf("Chmod() = %v, want nil", err)
	}
	t.Cleanup(func() { _ = os.Chmod(volumesDir, 0o755) })
	if removeErr := os.Remove(probe); removeErr == nil {
		t.Skipf("this process can remove a file from a read-only directory (%v)", removeErr)
	}

	_, err := service.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: testVolumeID})
	wantCode(t, err, codes.Internal)
	if !strings.Contains(status.Convert(err).Message(), "remove the volume metadata") {
		t.Fatalf("DeleteVolume() = %q, want it to name the metadata", status.Convert(err).Message())
	}
	if _, statErr := os.Lstat(filepath.Join(parent, filepath.FromSlash(metaFileName("pvc-1")))); statErr != nil {
		t.Fatalf("the metadata is gone (%v), want it kept", statErr)
	}
}
