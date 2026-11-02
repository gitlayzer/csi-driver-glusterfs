package driver

import (
	"context"
	"slices"
	"strings"
	"testing"

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

	caps := resp.GetCapabilities()
	if len(caps) != 1 {
		t.Fatalf("capabilities = %v, want exactly one", caps)
	}
	if got := caps[0].GetRpc().GetType(); got != csi.ControllerServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER {
		t.Fatalf("capability[0] = %v, want %v", got, csi.ControllerServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER)
	}
	// CREATE_DELETE_VOLUME would make the external-provisioner call a
	// CreateVolume that answers Unimplemented (D-23).
	for i, capability := range caps {
		if got := capability.GetRpc().GetType(); got == csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME {
			t.Fatalf("capability[%d] = %v, want no CREATE_DELETE_VOLUME before T-11", i, got)
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
func mustControllerService(t *testing.T) *controllerService {
	t.Helper()

	d := mustControllerDriver(t)
	if d.controller == nil {
		t.Fatal("controller = nil, want a controller service in controller mode")
	}
	return d.controller
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
	}{
		{
			name: "subdir volume is confirmed",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
			},
			wantConfirmed: true,
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
		},
		{
			name: "ext4 is unsupported, not an error",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapabilityOfType("ext4")},
			},
			wantMessage: []string{`file system type "ext4" is not supported`},
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
		},
		{
			name: "forbidden mount flag",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, "rw")},
			},
			wantMessage: []string{"mount flags are not supported", `"rw" is not allowed`},
		},
		{
			name: "mount flag outside the allowlist",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, "foo=bar")},
			},
			wantMessage: []string{"mount flags are not supported", `option "foo" is not in the allowlist`},
		},
		{
			name: "duplicate mount flag",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, "acl,acl")},
			},
			wantMessage: []string{"mount flags are not supported", "set more than once"},
		},
		{
			name: "valid mount flag",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, "log-level=WARNING")},
			},
			wantConfirmed: true,
		},
		{
			name: "matching parameters",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
				Parameters:         subdirParameters(),
			},
			wantConfirmed: true,
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
		},
		{
			name: "unknown parameter",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
				Parameters:         map[string]string{paramClusterID: "c1", paramParentVolume: "vol", "foo": "bar"},
			},
			wantMessage: []string{"parameters are not supported", `unknown parameter "foo"`},
		},
		{
			name: "quota true in the parameters",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
				Parameters:         map[string]string{paramClusterID: "c1", paramParentVolume: "vol", paramQuota: "true"},
			},
			wantMessage: []string{"parameters are not supported", `"true" is not supported yet`},
		},
		{
			name: "parameters of another cluster",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
				Parameters:         map[string]string{paramClusterID: "c2", paramParentVolume: "vol"},
			},
			wantMessage: []string{`parameters request cluster "c2"`},
		},
		{
			name: "parameters of another parent volume",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
				Parameters:         map[string]string{paramClusterID: "c1", paramParentVolume: "other"},
			},
			wantMessage: []string{`parameters request parent volume "other"`},
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
		},
		{
			name: "volume context with an unrelated key",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
				VolumeContext:      map[string]string{"something": "else"},
			},
			wantConfirmed: true,
		},
		{
			name: "volume context with the wrong volume",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
				VolumeContext:      map[string]string{attrVolume: "other"},
			},
			wantMessage: []string{`volume context volume "other" does not match the volume ("vol")`},
		},
		{
			name: "volume context with the wrong cluster",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
				VolumeContext:      map[string]string{attrClusterID: "c2"},
			},
			wantMessage: []string{"volume context clusterID"},
		},
		{
			name: "volume context with the wrong subdir",
			req: &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId:           testVolumeID,
				VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
				VolumeContext:      map[string]string{attrSubdir: "other"},
			},
			wantMessage: []string{"volume context subdir"},
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

	t.Run("CreateVolume", func(t *testing.T) {
		_, err := client.CreateVolume(ctx, &csi.CreateVolumeRequest{
			Name:               "pvc-1",
			VolumeCapabilities: []*csi.VolumeCapability{mountCapability},
		})
		wantCode(t, err, codes.Unimplemented)
	})
	t.Run("DeleteVolume", func(t *testing.T) {
		_, err := client.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: testVolumeID})
		wantCode(t, err, codes.Unimplemented)
	})
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

	d, err := New(opts)
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}

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
	if len(controllerRPCapabilities) == 0 {
		t.Fatal("controllerRPCapabilities is empty, want SINGLE_NODE_MULTI_WRITER")
	}
	if !slices.Contains(controllerRPCapabilities, csi.ControllerServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER) {
		t.Fatal("controllerRPCapabilities does not contain SINGLE_NODE_MULTI_WRITER")
	}

	unused := map[string]csi.ControllerServiceCapability_RPC_Type{
		"CREATE_DELETE_VOLUME":   csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
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
	client := csi.NewControllerClient(bufconnConn(t, mustControllerDriver(t)))
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

	client := csi.NewControllerClient(bufconnConn(t, mustControllerDriver(t)))
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
