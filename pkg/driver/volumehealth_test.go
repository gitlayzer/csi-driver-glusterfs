package driver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"

	"github.com/gitlayzer/csi-driver-glusterfs/pkg/mounter"
)

// wantHealthEntry is the expected content of one health entry of the response.
type wantHealthEntry struct {
	status csi.VolumeHealthErrorType
	reason string
	// message can be a substring: the entry merges the messages of its paths.
	message string
}

// assertHealth compares the health entries of a response with the expectation,
// including their order.
func assertHealth(t *testing.T, resp *csi.NodeGetVolumeHealthResponse, volumeID string, want []wantHealthEntry) {
	t.Helper()

	health := resp.GetVolumeHealth()
	if health == nil {
		t.Fatal("response has no volume health")
	}
	if health.GetVolumeId() != volumeID {
		t.Fatalf("volume_id = %q, want it to echo %q", health.GetVolumeId(), volumeID)
	}
	statuses := health.GetHealthStatuses()
	if len(statuses) != len(want) {
		t.Fatalf("health statuses = %v, want %d entries: %+v", statuses, len(want), want)
	}
	for i, w := range want {
		if got := statuses[i].GetStatus(); got != w.status {
			t.Errorf("status[%d] = %v, want %v", i, got, w.status)
		}
		if got := statuses[i].GetReason(); got != w.reason {
			t.Errorf("reason[%d] = %q, want %q", i, got, w.reason)
		}
		if !strings.Contains(statuses[i].GetMessage(), w.message) {
			t.Errorf("message[%d] = %q, want it to contain %q", i, statuses[i].GetMessage(), w.message)
		}
	}
}

// TestNodeGetVolumeHealth checks the request validation, the state to entry
// mapping and the merging of the two paths.
func TestNodeGetVolumeHealth(t *testing.T) {
	const otherVolumeID = "gfs:v1:static:c1:vol"

	tests := []struct {
		name     string
		volumeID string
		// staging and publish are the request paths; empty omits them.
		staging string
		publish string
		// mounts is the answer of VolumeMountState, indexed by path.
		mounts map[string]mounter.MountState
		// mountErr is returned by every VolumeMountState call.
		mountErr error
		wantCode codes.Code
		// wantMountState is the expected number of VolumeMountState calls.
		wantMountState int
		wantEntries    []wantHealthEntry
		wantMsg        string
	}{
		{
			name:     "missing volume id",
			volumeID: "",
			wantCode: codes.InvalidArgument,
		},
		{
			name:     "garbage volume id",
			volumeID: "garbage",
			wantCode: codes.NotFound,
			wantMsg:  `volume "garbage"`,
		},
		{
			name:     "relative staging target path",
			volumeID: testVolumeID,
			staging:  "staging/vol",
			wantCode: codes.InvalidArgument,
			wantMsg:  "staging target path",
		},
		{
			name:     "relative publish path",
			volumeID: testVolumeID,
			publish:  "pods/uid/mount",
			wantCode: codes.InvalidArgument,
			wantMsg:  "publish path",
		},
		{
			name:     "both untested paths are optional",
			volumeID: testVolumeID,
			wantCode: codes.OK,
			// The plugin keeps no record of where a volume is mounted, so there
			// is nothing to ask the mounter about.
			wantMountState: 0,
			wantEntries:    nil,
		},
		{
			name:     "healthy staging path and publish path",
			volumeID: testVolumeID,
			staging:  "/var/lib/kubelet/plugins/kubernetes.io/csi/staging",
			publish:  "/var/lib/kubelet/pods/uid/volumes/glusterfs/mount",
			mounts: map[string]mounter.MountState{
				"/var/lib/kubelet/plugins/kubernetes.io/csi/staging": mounter.MountStateMounted,
				"/var/lib/kubelet/pods/uid/volumes/glusterfs/mount":  mounter.MountStateMounted,
			},
			wantCode:       codes.OK,
			wantMountState: 2,
			wantEntries:    nil,
		},
		{
			name:     "corrupted staging path",
			volumeID: testVolumeID,
			staging:  "/var/lib/kubelet/plugins/kubernetes.io/csi/staging",
			mounts: map[string]mounter.MountState{
				"/var/lib/kubelet/plugins/kubernetes.io/csi/staging": mounter.MountStateCorrupted,
			},
			wantCode:       codes.OK,
			wantMountState: 1,
			wantEntries: []wantHealthEntry{{
				status:  csi.VolumeHealthErrorType_INACCESSIBLE,
				reason:  "MountCorrupted",
				message: `staging target path "/var/lib/kubelet/plugins/kubernetes.io/csi/staging"`,
			}},
		},
		{
			name:     "not mounted publish path",
			volumeID: testVolumeID,
			publish:  "/var/lib/kubelet/pods/uid/volumes/glusterfs/mount",
			mounts: map[string]mounter.MountState{
				"/var/lib/kubelet/pods/uid/volumes/glusterfs/mount": mounter.MountStateNotMounted,
			},
			wantCode:       codes.OK,
			wantMountState: 1,
			wantEntries: []wantHealthEntry{{
				status:  csi.VolumeHealthErrorType_INACCESSIBLE,
				reason:  "NotMounted",
				message: `publish path "/var/lib/kubelet/pods/uid/volumes/glusterfs/mount" is not a mount point`,
			}},
		},
		{
			name:     "another volume at the publish path",
			volumeID: testVolumeID,
			publish:  "/var/lib/kubelet/pods/uid/volumes/glusterfs/mount",
			mounts: map[string]mounter.MountState{
				"/var/lib/kubelet/pods/uid/volumes/glusterfs/mount": mounter.MountStateMismatch,
			},
			wantCode:       codes.OK,
			wantMountState: 1,
			wantEntries: []wantHealthEntry{{
				status:  csi.VolumeHealthErrorType_INACCESSIBLE,
				reason:  "VolumeMismatch",
				message: "is mounted from something else",
			}},
		},
		{
			name:     "both paths corrupted are merged into one entry",
			volumeID: testVolumeID,
			staging:  "/staging",
			publish:  "/publish",
			mounts: map[string]mounter.MountState{
				"/staging": mounter.MountStateCorrupted,
				"/publish": mounter.MountStateCorrupted,
			},
			wantCode:       codes.OK,
			wantMountState: 2,
			wantEntries: []wantHealthEntry{{
				status: csi.VolumeHealthErrorType_INACCESSIBLE,
				reason: "MountCorrupted",
				message: `staging target path "/staging" is a corrupted mount point: the file system does not respond; ` +
					`publish path "/publish" is a corrupted mount point: the file system does not respond`,
			}},
		},
		{
			name:     "not mounted staging and corrupted publish",
			volumeID: testVolumeID,
			staging:  "/staging",
			publish:  "/publish",
			mounts: map[string]mounter.MountState{
				"/staging": mounter.MountStateNotMounted,
				"/publish": mounter.MountStateCorrupted,
			},
			wantCode:       codes.OK,
			wantMountState: 2,
			wantEntries: []wantHealthEntry{
				{
					status:  csi.VolumeHealthErrorType_INACCESSIBLE,
					reason:  "NotMounted",
					message: `staging target path "/staging" is not a mount point`,
				},
				{
					status:  csi.VolumeHealthErrorType_INACCESSIBLE,
					reason:  "MountCorrupted",
					message: `publish path "/publish" is a corrupted mount point`,
				},
			},
		},
		{
			name:     "state cannot be determined",
			volumeID: testVolumeID,
			staging:  "/staging",
			publish:  "/publish",
			mountErr: errors.New("mount table unavailable"),
			wantCode: codes.Internal,
			// The RPC stops at the first failing path.
			wantMountState: 1,
			wantMsg:        "check volume",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node, fake := newTestNode(t)
			fake.mountStateErr = tc.mountErr
			fake.mountStateOf = tc.mounts

			req := &csi.NodeGetVolumeHealthRequest{
				VolumeId:          tc.volumeID,
				StagingTargetPath: tc.staging,
				VolumePublishPath: tc.publish,
			}
			id := tc.volumeID
			if id == "" {
				id = testVolumeID
			}

			resp, err := node.NodeGetVolumeHealth(context.Background(), req)
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
			assertHealth(t, resp, id, tc.wantEntries)

			// The volume fields come from the volume id, not from the request.
			for _, call := range fake.mountStateCalls {
				if call.volume != "vol" || call.subdir != "pvc-1" {
					t.Fatalf("VolumeMountState(%q) asked for %q/%q, want %q/%q",
						call.target, call.volume, call.subdir, "vol", "pvc-1")
				}
			}
		})
	}

	t.Run("staging path is checked before the publish path", func(t *testing.T) {
		node, fake := newTestNode(t)

		if _, err := node.NodeGetVolumeHealth(context.Background(), &csi.NodeGetVolumeHealthRequest{
			VolumeId:          testVolumeID,
			StagingTargetPath: "/staging",
			VolumePublishPath: "/publish",
		}); err != nil {
			t.Fatalf("NodeGetVolumeHealth() = %v, want nil", err)
		}
		if len(fake.mountStateCalls) != 2 {
			t.Fatalf("VolumeMountState calls = %d, want 2", len(fake.mountStateCalls))
		}
		if got := fake.mountStateCalls[0].target; got != "/staging" {
			t.Errorf("first checked path = %q, want %q", got, "/staging")
		}
		if got := fake.mountStateCalls[1].target; got != "/publish" {
			t.Errorf("second checked path = %q, want %q", got, "/publish")
		}
	})

	t.Run("static volume id without a subdir", func(t *testing.T) {
		node, fake := newTestNode(t)

		if _, err := node.NodeGetVolumeHealth(context.Background(), &csi.NodeGetVolumeHealthRequest{
			VolumeId:          otherVolumeID,
			StagingTargetPath: "/staging",
		}); err != nil {
			t.Fatalf("NodeGetVolumeHealth() = %v, want nil", err)
		}
		if got := fake.mountStateCalls[0].subdir; got != "" {
			t.Errorf("subdir = %q, want empty for a static volume without one", got)
		}
	})

	t.Run("no in-flight lock is taken", func(t *testing.T) {
		node, _ := newTestNode(t)
		for _, key := range []string{nodeStageLockPrefix + testVolumeID, nodePublishVolumeLockPrefix + testVolumeID} {
			if !node.locks.tryAcquire(key) {
				t.Fatalf("could not pre-acquire %q", key)
			}
			defer node.locks.release(key)
		}

		if _, err := node.NodeGetVolumeHealth(context.Background(), &csi.NodeGetVolumeHealthRequest{
			VolumeId:          testVolumeID,
			StagingTargetPath: "/staging",
		}); err != nil {
			t.Fatalf("NodeGetVolumeHealth() = %v, want nil although the volume locks are held", err)
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		node, fake := newTestNode(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := node.NodeGetVolumeHealth(ctx, &csi.NodeGetVolumeHealthRequest{
			VolumeId:          testVolumeID,
			StagingTargetPath: "/staging",
		})
		wantCode(t, err, codes.Canceled)
		if got := fake.mountStateCount(); got != 0 {
			t.Fatalf("VolumeMountState calls = %d, want 0 with a cancelled context", got)
		}
	})
}

// TestNodeGetVolumeHealthOverGRPC checks the RPC through the real gRPC server:
// the alpha messages must survive the wire, and the volume id has to be echoed
// for csi-sanity.
func TestNodeGetVolumeHealthOverGRPC(t *testing.T) {
	d, err := New(nodeOptions(t))
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}
	fake := &fakeMounter{
		mountState: mounter.MountStateMounted,
		mountStateOf: map[string]mounter.MountState{
			"/staging": mounter.MountStateCorrupted,
		},
	}
	d.node.mounter = fake

	client := csi.NewNodeClient(bufconnConn(t, d))
	resp, err := client.NodeGetVolumeHealth(context.Background(), &csi.NodeGetVolumeHealthRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: "/staging",
	})
	if err != nil {
		t.Fatalf("NodeGetVolumeHealth() = %v, want nil", err)
	}
	assertHealth(t, resp, testVolumeID, []wantHealthEntry{{
		status:  csi.VolumeHealthErrorType_INACCESSIBLE,
		reason:  healthReasonMountCorrupted,
		message: `staging target path "/staging"`,
	}})
}

// TestHealthEntryOf checks the mapping table of a mount state to a health entry
// as a whole, including the state this package does not define.
func TestHealthEntryOf(t *testing.T) {
	check := healthCheck{field: "staging target path", path: "/staging"}

	tests := []struct {
		name       string
		state      mounter.MountState
		wantOK     bool
		wantStatus csi.VolumeHealthErrorType
		wantReason string
	}{
		{
			name:   "mounted is not reported",
			state:  mounter.MountStateMounted,
			wantOK: false,
		},
		{
			name:       "corrupted",
			state:      mounter.MountStateCorrupted,
			wantOK:     true,
			wantStatus: csi.VolumeHealthErrorType_INACCESSIBLE,
			wantReason: healthReasonMountCorrupted,
		},
		{
			name:       "not mounted",
			state:      mounter.MountStateNotMounted,
			wantOK:     true,
			wantStatus: csi.VolumeHealthErrorType_INACCESSIBLE,
			wantReason: healthReasonNotMounted,
		},
		{
			name:       "mismatch",
			state:      mounter.MountStateMismatch,
			wantOK:     true,
			wantStatus: csi.VolumeHealthErrorType_INACCESSIBLE,
			wantReason: healthReasonVolumeMismatch,
		},
		{
			name:       "undefined state",
			state:      mounter.MountState(42),
			wantOK:     true,
			wantStatus: csi.VolumeHealthErrorType_INACCESSIBLE,
			wantReason: healthReasonVolumeMismatch,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entryStatus, reason, message, ok := healthEntryOf(check, tc.state)
			if ok != tc.wantOK {
				t.Fatalf("ok = %t, want %t", ok, tc.wantOK)
			}
			if !ok {
				if entryStatus != csi.VolumeHealthErrorType_UNKNOWN_VOLUME_HEALTH_TYPE || reason != "" || message != "" {
					t.Fatalf("healthy entry = %v/%q/%q, want the zero value", entryStatus, reason, message)
				}
				return
			}
			if entryStatus != tc.wantStatus {
				t.Errorf("status = %v, want %v", entryStatus, tc.wantStatus)
			}
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
			if !strings.Contains(message, check.path) || !strings.Contains(message, check.field) {
				t.Errorf("message = %q, want it to name %q and %q", message, check.field, check.path)
			}
		})
	}
}
