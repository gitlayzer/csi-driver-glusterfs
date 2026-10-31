package driver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gitlayzer/csi-driver-glusterfs/pkg/mounter"
)

// The paths the recovery tests work with. They are spelled out once so that the
// expected call logs are readable.
const (
	testStaging = "/var/lib/kubelet/plugins/kubernetes.io/csi/staging"
	testTarget1 = "/var/lib/kubelet/pods/uid1/volumes/kubernetes.io~csi/pv/mount"
	testTarget2 = "/var/lib/kubelet/pods/uid2/volumes/kubernetes.io~csi/pv/mount"
)

// recoverySite is a node service wired for the recovery tests: a fake mounter
// that tracks the mount state of every path, and a record store in a temporary
// directory.
type recoverySite struct {
	node  *nodeService
	fake  *fakeMounter
	store *recordStore
	// atDetach is what one case records inside the detach hook.
	atDetach []recordTarget
}

// newRecoverySite builds the site and stores recs.
//
// Every path of the fake starts out Mounted, which is the state a freshly staged
// and published volume has; a case overrides the paths it is about. Tracking is
// on, so that a successful mount or detach of the fake changes the state the next
// pass sees.
func newRecoverySite(t *testing.T, recs ...stageRecord) *recoverySite {
	t.Helper()

	node, fake := newTestNode(t)
	store := newTestRecordStore(t)
	node.records = store

	fake.trackState = true
	fake.mountStateOf = make(map[string]mounter.MountState)

	for _, rec := range recs {
		if err := store.save(rec); err != nil {
			t.Fatalf("save(%+v) = %v, want nil", rec, err)
		}
	}
	return &recoverySite{node: node, fake: fake, store: store}
}

// stageRecordOf returns a record of testVolumeID with the given staging path,
// flags and targets.
func stageRecordOf(staging string, readOnly bool, flags []string, targets ...recordTarget) stageRecord {
	return stageRecord{
		Version:           stageRecordVersion,
		VolumeID:          testVolumeID,
		StagingTargetPath: staging,
		ReadOnly:          readOnly,
		MountFlags:        flags,
		Targets:           targets,
	}
}

// loadRecord returns the record of testVolumeID, failing the test when it cannot
// be read.
func (s *recoverySite) loadRecord(t *testing.T) stageRecord {
	t.Helper()

	rec, err := s.store.load(testVolumeID)
	if err != nil {
		t.Fatalf("load() = %v, want nil", err)
	}
	return rec
}

// wantRecordGone fails the test unless the record of testVolumeID is gone.
func (s *recoverySite) wantRecordGone(t *testing.T) {
	t.Helper()

	if _, err := s.store.load(testVolumeID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("load() = %v, want the record to be gone", err)
	}
}

// TestRecoverVolume drives one recovery pass per case and asserts the outcome, the
// exact order of the mounter calls and what is left in the record.
func TestRecoverVolume(t *testing.T) {
	targets := []recordTarget{
		{Path: testTarget1},
		{Path: testTarget2, ReadOnly: true},
	}

	tests := []struct {
		name string
		// rec is the record under test; the zero value means "a record of the
		// staged volume without pending targets".
		rec stageRecord
		// useRec is false for the cases that need no record at all.
		noRecord bool
		// state is the mount state of individual paths; every other path is
		// Mounted.
		state map[string]mounter.MountState
		// prep runs before the pass, to queue errors or hold locks.
		prep func(t *testing.T, s *recoverySite)
		// prepRecord runs after the record was stored and after prep.
		prepRecord  func(t *testing.T, s *recoverySite, name string)
		wantOutcome recoveryOutcome
		wantErr     bool
		// wantCalls is the exact ordered log of bindTargets, detach, mount and
		// bind calls.
		wantCalls []string
		// wantTargets is what the record holds after the pass.
		wantTargets []recordTarget
		wantRecord  bool
		// check runs last, after the generic assertions.
		check func(t *testing.T, s *recoverySite)
	}{
		{
			name:        "mounted and nothing pending",
			wantOutcome: recoveryHealthy,
			wantRecord:  true,
		},
		{
			name:        "staging path is mounted from something else",
			state:       map[string]mounter.MountState{testStaging: mounter.MountStateMismatch},
			wantOutcome: recoveryMismatch,
			wantRecord:  true,
		},
		{
			name: "corrupted staging mount with two bind targets",
			rec:  stageRecordOf(testStaging, true, []string{"acl", "log-level=INFO"}, targets...),
			// The FUSE process died, so the staging mount and the bind mounts
			// made from it are all dead.
			state: map[string]mounter.MountState{
				testStaging: mounter.MountStateCorrupted,
				testTarget1: mounter.MountStateCorrupted,
				testTarget2: mounter.MountStateCorrupted,
			},
			wantOutcome: recoveryRecovered,
			wantCalls: []string{
				// BindTargets before the detach: after it the mountinfo entry of
				// the staging mount is gone.
				"bindTargets:" + testStaging,
				"detach:" + testStaging,
				"mount:" + testStaging,
				"detach:" + testTarget1,
				"bind:" + testStaging + "->" + testTarget1 + ":rw",
				"detach:" + testTarget2,
				"bind:" + testStaging + "->" + testTarget2 + ":ro",
			},
			wantRecord: true,
			check: func(t *testing.T, s *recoverySite) {
				t.Helper()
				if got := s.fake.mountCount(); got != 1 {
					t.Fatalf("MountGlusterfs calls = %d, want 1", got)
				}
				spec := s.fake.mountCalls[0].spec
				// The servers come from the live configuration, not the record.
				if !reflect.DeepEqual(spec.Servers, testServers) {
					t.Errorf("Servers = %v, want the configured %v", spec.Servers, testServers)
				}
				if spec.Volume != "vol" || spec.Subdir != "pvc-1" {
					t.Errorf("Volume/Subdir = %q/%q, want vol/pvc-1", spec.Volume, spec.Subdir)
				}
				if !spec.ReadOnly {
					t.Error("ReadOnly = false, want the recorded read-only flag")
				}
				if !reflect.DeepEqual(spec.Options, []string{"acl", "log-level=INFO"}) {
					t.Errorf("Options = %v, want the recorded mount flags", spec.Options)
				}
				if got := s.fake.bindCalls[1].readOnly; !got {
					t.Error("the second bind target was bound read write, want read only")
				}
			},
		},
		{
			name: "bind targets are found by the recovery, not only remembered",
			state: map[string]mounter.MountState{
				testStaging: mounter.MountStateCorrupted,
				testTarget1: mounter.MountStateCorrupted,
				testTarget2: mounter.MountStateCorrupted,
			},
			prep: func(_ *testing.T, s *recoverySite) {
				s.fake.bindTargets = []mounter.BindTarget{
					{Path: testTarget1},
					{Path: testTarget2, ReadOnly: true},
				}
			},
			wantOutcome: recoveryRecovered,
			wantCalls: []string{
				"bindTargets:" + testStaging,
				"detach:" + testStaging,
				"mount:" + testStaging,
				"detach:" + testTarget1,
				"bind:" + testStaging + "->" + testTarget1 + ":rw",
				"detach:" + testTarget2,
				"bind:" + testStaging + "->" + testTarget2 + ":ro",
			},
			wantRecord: true,
		},
		{
			name: "the targets are saved before the staging mount is detached",
			rec:  stageRecordOf(testStaging, false, nil),
			state: map[string]mounter.MountState{
				testStaging: mounter.MountStateCorrupted,
				testTarget1: mounter.MountStateCorrupted,
			},
			prep: func(_ *testing.T, s *recoverySite) {
				s.fake.bindTargets = []mounter.BindTarget{{Path: testTarget1}}
				// The record has to name the targets at the moment of the detach:
				// a crash right after it must not lose them.
				s.atDetach = nil
				s.fake.onDetach = func(target string) {
					if target != testStaging {
						return
					}
					rec, err := s.store.load(testVolumeID)
					if err != nil {
						t.Errorf("load() during the detach = %v, want nil", err)
						return
					}
					s.atDetach = rec.Targets
				}
			},
			wantOutcome: recoveryRecovered,
			wantCalls: []string{
				"bindTargets:" + testStaging,
				"detach:" + testStaging,
				"mount:" + testStaging,
				"detach:" + testTarget1,
				"bind:" + testStaging + "->" + testTarget1 + ":rw",
			},
			wantRecord: true,
			check: func(t *testing.T, s *recoverySite) {
				t.Helper()
				if len(s.atDetach) != 1 || s.atDetach[0].Path != testTarget1 {
					t.Fatalf("targets in the record at the detach = %+v, want the found target", s.atDetach)
				}
			},
		},
		{
			name: "the remount fails and the record keeps the targets",
			rec:  stageRecordOf(testStaging, false, nil, targets...),
			state: map[string]mounter.MountState{
				testStaging: mounter.MountStateCorrupted,
				testTarget1: mounter.MountStateCorrupted,
				testTarget2: mounter.MountStateCorrupted,
			},
			prep:        func(_ *testing.T, s *recoverySite) { s.fake.mountErrs = []error{errors.New("gluster is down")} },
			wantOutcome: recoveryHealthy,
			wantErr:     true,
			wantCalls: []string{
				"bindTargets:" + testStaging,
				"detach:" + testStaging,
				"mount:" + testStaging,
			},
			wantTargets: targets,
			wantRecord:  true,
			check: func(t *testing.T, s *recoverySite) {
				t.Helper()
				for _, call := range s.fake.callLog() {
					if strings.HasPrefix(call, "bind:") || strings.HasPrefix(call, "detach:"+testTarget1) {
						t.Fatalf("a target was touched although the remount failed: %v", s.fake.callLog())
					}
				}
			},
		},
		{
			name:        "not mounted and no corrupted target is a stale record",
			rec:         stageRecordOf(testStaging, false, nil, recordTarget{Path: testTarget1}),
			state:       map[string]mounter.MountState{testStaging: mounter.MountStateNotMounted},
			wantOutcome: recoveryStale,
			wantRecord:  false,
		},
		{
			name:        "not mounted without any target is stale as well",
			state:       map[string]mounter.MountState{testStaging: mounter.MountStateNotMounted},
			wantOutcome: recoveryStale,
			wantRecord:  false,
		},
		{
			name:  "not mounted but a corrupted target keeps the record and remounts",
			rec:   stageRecordOf(testStaging, false, nil, targets...),
			state: map[string]mounter.MountState{testStaging: mounter.MountStateNotMounted},
			prep: func(_ *testing.T, s *recoverySite) {
				// The staging mount is gone, the bind mounts of the Pods are dead.
				s.fake.mountStateOf[testTarget1] = mounter.MountStateCorrupted
				s.fake.mountStateOf[testTarget2] = mounter.MountStateCorrupted
			},
			wantOutcome: recoveryRecovered,
			wantCalls: []string{
				"mount:" + testStaging,
				"detach:" + testTarget1,
				"bind:" + testStaging + "->" + testTarget1 + ":rw",
				"detach:" + testTarget2,
				"bind:" + testStaging + "->" + testTarget2 + ":ro",
			},
			wantRecord: true,
		},
		{
			name: "cluster of the record is not configured",
			rec:  stageRecordOf(testStaging, false, nil, targets...),
			state: map[string]mounter.MountState{
				testStaging: mounter.MountStateCorrupted,
				testTarget1: mounter.MountStateCorrupted,
				testTarget2: mounter.MountStateCorrupted,
			},
			prep:        func(_ *testing.T, s *recoverySite) { s.node.driver.config = nil },
			wantOutcome: recoveryHealthy,
			wantErr:     true,
			wantCalls: []string{
				"bindTargets:" + testStaging,
				"detach:" + testStaging,
			},
			wantTargets: targets,
			wantRecord:  true,
			check: func(t *testing.T, s *recoverySite) {
				t.Helper()
				if got := s.fake.mountCount(); got != 0 {
					t.Fatalf("MountGlusterfs calls = %d, want 0 without a configuration", got)
				}
			},
		},
		{
			name: "stage lock of the volume is held",
			rec:  stageRecordOf(testStaging, false, nil, targets...),
			state: map[string]mounter.MountState{
				testStaging: mounter.MountStateCorrupted,
				testTarget1: mounter.MountStateCorrupted,
				testTarget2: mounter.MountStateCorrupted,
			},
			prep:        holdStageLock,
			wantOutcome: recoveryBusy,
			wantTargets: targets,
			wantRecord:  true,
			check: func(t *testing.T, s *recoverySite) {
				t.Helper()
				if calls := s.fake.callLog(); len(calls) != 0 {
					t.Fatalf("mounter calls = %v, want none while the stage lock is held", calls)
				}
				if got := s.fake.mountStateCount(); got != 0 {
					t.Fatalf("VolumeMountState calls = %d, want 0 while the stage lock is held", got)
				}
			},
		},
		{
			name: "publish lock of a target is held",
			rec:  stageRecordOf(testStaging, false, nil, targets...),
			prep: func(_ *testing.T, s *recoverySite) {
				holdPublishLock(t, s, testTarget1)
			},
			state: map[string]mounter.MountState{
				testStaging: mounter.MountStateCorrupted,
				testTarget1: mounter.MountStateCorrupted,
				testTarget2: mounter.MountStateCorrupted,
			},
			wantOutcome: recoveryPartial,
			wantCalls: []string{
				"bindTargets:" + testStaging,
				"detach:" + testStaging,
				"mount:" + testStaging,
				"detach:" + testTarget2,
				"bind:" + testStaging + "->" + testTarget2 + ":ro",
			},
			// The target of the held lock stays pending, the other one is done.
			wantTargets: []recordTarget{{Path: testTarget1}},
			wantRecord:  true,
		},
		{
			name: "a target cannot be bound again",
			rec:  stageRecordOf(testStaging, false, nil, targets...),
			state: map[string]mounter.MountState{
				testTarget1: mounter.MountStateCorrupted,
				testTarget2: mounter.MountStateCorrupted,
			},
			prep:        func(_ *testing.T, s *recoverySite) { s.fake.bindErrs = []error{errors.New("bind failed")} },
			wantOutcome: recoveryPartial,
			wantCalls: []string{
				"detach:" + testTarget1,
				"bind:" + testStaging + "->" + testTarget1 + ":rw",
				"detach:" + testTarget2,
				"bind:" + testStaging + "->" + testTarget2 + ":ro",
			},
			wantTargets: []recordTarget{{Path: testTarget1}},
			wantRecord:  true,
		},
		{
			name:  "mounted with one corrupted and one mounted target",
			rec:   stageRecordOf(testStaging, false, nil, targets...),
			state: map[string]mounter.MountState{testTarget1: mounter.MountStateCorrupted},
			// The mounted target is dropped without being touched: it is fine.
			wantOutcome: recoveryRecovered,
			wantCalls: []string{
				"detach:" + testTarget1,
				"bind:" + testStaging + "->" + testTarget1 + ":rw",
			},
			wantRecord: true,
		},
		{
			name:  "a target that was unpublished in the meantime is dropped",
			rec:   stageRecordOf(testStaging, false, nil, targets...),
			state: map[string]mounter.MountState{testTarget1: mounter.MountStateNotMounted, testTarget2: mounter.MountStateMismatch},
			// Not mounted means the Pod is gone, mismatch that the path belongs
			// to somebody else; both are none of the recovery's business.
			wantOutcome: recoveryHealthy,
			wantRecord:  true,
		},
		{
			name: "BindTargets fails",
			rec:  stageRecordOf(testStaging, false, nil, targets...),
			state: map[string]mounter.MountState{
				testStaging: mounter.MountStateCorrupted,
				testTarget1: mounter.MountStateCorrupted,
				testTarget2: mounter.MountStateCorrupted,
			},
			prep:        func(_ *testing.T, s *recoverySite) { s.fake.bindTargetsErr = errors.New("mountinfo unreadable") },
			wantOutcome: recoveryHealthy,
			wantErr:     true,
			wantCalls:   []string{"bindTargets:" + testStaging},
			wantTargets: targets,
			wantRecord:  true,
		},
		{
			name: "the staging mount cannot be detached",
			rec:  stageRecordOf(testStaging, false, nil, targets...),
			state: map[string]mounter.MountState{
				testStaging: mounter.MountStateCorrupted,
				testTarget1: mounter.MountStateCorrupted,
				testTarget2: mounter.MountStateCorrupted,
			},
			prep:        func(_ *testing.T, s *recoverySite) { s.fake.detachErrs = []error{errors.New("busy")} },
			wantOutcome: recoveryHealthy,
			wantErr:     true,
			wantCalls: []string{
				"bindTargets:" + testStaging,
				"detach:" + testStaging,
			},
			wantTargets: targets,
			wantRecord:  true,
			check: func(t *testing.T, s *recoverySite) {
				t.Helper()
				if got := s.fake.mountCount(); got != 0 {
					t.Fatalf("MountGlusterfs calls = %d, want 0 after a failed detach", got)
				}
			},
		},
		{
			name: "the state of a bind target cannot be determined",
			rec:  stageRecordOf(testStaging, false, nil, targets...),
			prep: func(_ *testing.T, s *recoverySite) {
				s.fake.mountStateErrOf = map[string]error{testTarget1: errors.New("stat failed")}
				s.fake.mountStateOf[testTarget2] = mounter.MountStateCorrupted
			},
			// The unreadable target stays pending, the other one is refreshed.
			wantOutcome: recoveryPartial,
			wantCalls: []string{
				"detach:" + testTarget2,
				"bind:" + testStaging + "->" + testTarget2 + ":ro",
			},
			wantTargets: []recordTarget{{Path: testTarget1}},
			wantRecord:  true,
		},
		{
			name:  "a bind target of a lost mount cannot be inspected",
			rec:   stageRecordOf(testStaging, false, nil, targets...),
			state: map[string]mounter.MountState{testStaging: mounter.MountStateNotMounted},
			prep: func(_ *testing.T, s *recoverySite) {
				s.fake.mountStateErrOf = map[string]error{testTarget1: errors.New("stat failed")}
			},
			// A target whose state is unknown counts as "not corrupted", so the
			// record is stale and kubelet stages the volume again.
			wantOutcome: recoveryStale,
			wantRecord:  false,
		},
		{
			name:  "a corrupted bind mount cannot be detached",
			rec:   stageRecordOf(testStaging, false, nil, targets...),
			state: map[string]mounter.MountState{testTarget1: mounter.MountStateCorrupted},
			prep:  func(_ *testing.T, s *recoverySite) { s.fake.detachErrs = []error{errors.New("busy")} },
			// A detach failure keeps the target pending, the other one (which is
			// mounted) is dropped.
			wantOutcome: recoveryPartial,
			wantCalls:   []string{"detach:" + testTarget1},
			wantTargets: []recordTarget{{Path: testTarget1}},
			wantRecord:  true,
		},
		{
			name:  "the mount log directory cannot be created",
			rec:   stageRecordOf(testStaging, false, nil),
			state: map[string]mounter.MountState{testStaging: mounter.MountStateCorrupted},
			prep: func(t *testing.T, s *recoverySite) {
				t.Helper()
				file := filepath.Join(t.TempDir(), "file")
				if err := os.WriteFile(file, nil, 0o600); err != nil {
					t.Fatalf("WriteFile() = %v, want nil", err)
				}
				s.node.logDir = filepath.Join(file, "logs")
			},
			wantOutcome: recoveryHealthy,
			wantErr:     true,
			wantCalls: []string{
				"bindTargets:" + testStaging,
				"detach:" + testStaging,
			},
			wantRecord: true,
			check: func(t *testing.T, s *recoverySite) {
				t.Helper()
				if got := s.fake.mountCount(); got != 0 {
					t.Fatalf("MountGlusterfs calls = %d, want 0 when the log directory fails", got)
				}
			},
		},
		{
			name: "the state of the staging mount cannot be determined",
			prep: func(_ *testing.T, s *recoverySite) { s.fake.mountStateErr = errors.New("mount table unreadable") },
			// An injected error is returned for every path, including the
			// staging one, so the pass stops there.
			wantOutcome: recoveryHealthy,
			wantErr:     true,
			wantRecord:  true,
		},
		{
			name:        "the record file is invalid",
			noRecord:    true,
			wantOutcome: recoveryHealthy,
			wantErr:     true,
			prepRecord: func(t *testing.T, s *recoverySite, _ string) {
				t.Helper()
				if err := os.MkdirAll(s.store.dir, recordDirMode); err != nil {
					t.Fatalf("MkdirAll() = %v, want nil", err)
				}
				broken := filepath.Join(s.store.dir, recordFileName(testVolumeID))
				if err := os.WriteFile(broken, []byte("not JSON"), 0o600); err != nil {
					t.Fatalf("WriteFile() = %v, want nil", err)
				}
			},
			wantRecord: false,
			check: func(t *testing.T, s *recoverySite) {
				t.Helper()
				// The invalid record is kept: it is the only trace of the volume
				// and an operator has to look at it.
				if _, err := os.Stat(s.store.path(testVolumeID)); err != nil {
					t.Fatalf("Stat() = %v, want the invalid record to be kept", err)
				}
				if calls := s.fake.callLog(); len(calls) != 0 {
					t.Fatalf("mounter calls = %v, want none for an invalid record", calls)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			site := newRecoverySite(t)
			rec := tc.rec
			if rec.Version == 0 && !tc.noRecord {
				rec = stageRecordOf(testStaging, false, nil)
			}
			if !tc.noRecord {
				if err := site.store.save(rec); err != nil {
					t.Fatalf("save() = %v, want nil", err)
				}
			}
			for path, state := range tc.state {
				site.fake.mountStateOf[path] = state
			}
			if tc.prep != nil {
				tc.prep(t, site)
			}
			name := recordFileName(testVolumeID)
			if tc.prepRecord != nil {
				tc.prepRecord(t, site, name)
			}

			outcome, err := site.node.recoverVolume(context.Background(), name)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("recoverVolume() = %v, nil, want an error", outcome)
				}
			} else if err != nil {
				t.Fatalf("recoverVolume() = %v, want nil", err)
			}
			if outcome != tc.wantOutcome {
				t.Fatalf("recoverVolume() = %v, want %v (err %v)", outcome, tc.wantOutcome, err)
			}

			if got := site.fake.callLog(); !reflect.DeepEqual(got, tc.wantCalls) {
				t.Fatalf("call log = %v, want %v", got, tc.wantCalls)
			}

			if tc.wantRecord {
				if got := site.loadRecord(t).Targets; !reflect.DeepEqual(got, tc.wantTargets) {
					t.Fatalf("record targets = %+v, want %+v", got, tc.wantTargets)
				}
			} else if !tc.noRecord {
				site.wantRecordGone(t)
			}

			if tc.check != nil {
				tc.check(t, site)
			}
		})
	}
}

// holdStageLock acquires the stage lock of testVolumeID for the duration of the
// test.
func holdStageLock(t *testing.T, s *recoverySite) {
	t.Helper()
	key := nodeStageLockPrefix + testVolumeID
	if !s.node.locks.tryAcquire(key) {
		t.Fatal("could not pre-acquire the stage lock")
	}
	t.Cleanup(func() { s.node.locks.release(key) })
}

// holdPublishLock acquires the publish lock of a target path for the duration of
// the test.
func holdPublishLock(t *testing.T, s *recoverySite, target string) {
	t.Helper()
	key := nodePublishLockPrefix + target
	if !s.node.locks.tryAcquire(key) {
		t.Fatal("could not pre-acquire the publish lock")
	}
	t.Cleanup(func() { s.node.locks.release(key) })
}

// TestRecoverVolumeRecordGoneUnderTheLock covers the race the second read of the
// record exists for: an unstage that finishes between the first read and the lock
// makes the pass a no-op.
func TestRecoverVolumeRecordGoneUnderTheLock(t *testing.T) {
	site := newRecoverySite(t, stageRecordOf(testStaging, false, nil))
	name := recordFileName(testVolumeID)

	// The record is removed the moment the recovery takes the stage lock, which is
	// exactly the window between the first read and the read under the lock.
	locks := &removingLockTable{lockTable: site.node.locks, store: site.store, volumeID: testVolumeID}
	site.node.locks = locks

	outcome, err := site.node.recoverVolume(context.Background(), name)
	if err != nil {
		t.Fatalf("recoverVolume() = %v, want nil", err)
	}
	if outcome != recoveryHealthy {
		t.Fatalf("recoverVolume() = %v, want healthy", outcome)
	}
	if locks.err != nil {
		t.Fatalf("remove() in the lock = %v, want nil", locks.err)
	}
	if calls := site.fake.callLog(); len(calls) != 0 {
		t.Fatalf("mounter calls = %v, want none", calls)
	}
	if got := site.fake.mountStateCount(); got != 0 {
		t.Fatalf("VolumeMountState calls = %d, want 0", got)
	}
}

// removingLockTable is a [lockTable] that deletes a staging record when the stage
// lock of its volume is acquired, which is what an unstage finishing between the
// two reads of [nodeService.recoverVolume] looks like.
type removingLockTable struct {
	lockTable
	store    *recordStore
	volumeID string
	// err is the failure of the removal, checked by the test.
	err error
}

// tryAcquire removes the record and then acquires the key.
func (l *removingLockTable) tryAcquire(key string) bool {
	ok := l.lockTable.tryAcquire(key)
	if ok && key == nodeStageLockPrefix+l.volumeID {
		l.err = l.store.remove(l.volumeID)
	}
	return ok
}

// TestRecoverVolumeInterruptedRecovery covers the two-pass recovery of D-20: the
// first pass detaches the corrupt staging mount and cannot mount again, the
// second one finishes the job although the staging path is not mounted any more.
func TestRecoverVolumeInterruptedRecovery(t *testing.T) {
	targets := []recordTarget{
		{Path: testTarget1},
		{Path: testTarget2, ReadOnly: true},
	}
	site := newRecoverySite(t, stageRecordOf(testStaging, false, nil, targets...))
	site.fake.mountStateOf[testStaging] = mounter.MountStateCorrupted
	site.fake.mountStateOf[testTarget1] = mounter.MountStateCorrupted
	site.fake.mountStateOf[testTarget2] = mounter.MountStateCorrupted
	site.fake.mountErrs = []error{errors.New("gluster is down")}

	// First pass: the targets are saved and the staging mount is detached, but
	// the mount fails, so nothing is bound again.
	if _, err := site.node.recoverVolume(context.Background(), recordFileName(testVolumeID)); err == nil {
		t.Fatal("first recoverVolume() = nil, want the injected mount failure")
	}
	if rec := site.loadRecord(t); !reflect.DeepEqual(rec.Targets, targets) {
		t.Fatalf("Targets after the first pass = %+v, want %+v", rec.Targets, targets)
	}
	if got := site.fake.detachCalls; len(got) != 1 || got[0] != testStaging {
		t.Fatalf("Detach calls = %v, want only the staging mount", got)
	}

	// Second pass: the staging path is not mounted, but the bind mounts of the
	// Pods are still dead, so the volume is mounted again and the targets are
	// refreshed.
	site.fake.calls = nil
	outcome, err := site.node.recoverVolume(context.Background(), recordFileName(testVolumeID))
	if err != nil {
		t.Fatalf("second recoverVolume() = %v, want nil", err)
	}
	if outcome != recoveryRecovered {
		t.Fatalf("second recoverVolume() = %v, want recovered", outcome)
	}
	want := []string{
		"mount:" + testStaging,
		"detach:" + testTarget1,
		"bind:" + testStaging + "->" + testTarget1 + ":rw",
		"detach:" + testTarget2,
		"bind:" + testStaging + "->" + testTarget2 + ":ro",
	}
	if got := site.fake.callLog(); !reflect.DeepEqual(got, want) {
		t.Fatalf("second pass call log = %v, want %v", got, want)
	}
	if rec := site.loadRecord(t); len(rec.Targets) != 0 {
		t.Fatalf("Targets after the second pass = %+v, want none", rec.Targets)
	}
	if got := site.fake.mountCount(); got != 2 {
		t.Fatalf("MountGlusterfs calls = %d, want 2 (one failed, one succeeded)", got)
	}
}

// TestRecoverAll checks the pass over all records: every record is processed, a
// failing one does not stop the others, and the summary counts the outcomes.
func TestRecoverAll(t *testing.T) {
	tests := []struct {
		name string
		// records is the number of healthy records to create.
		records int
		// corrupt is the number of records whose staging mount is corrupted.
		corrupt int
		// broken adds a record file with invalid content.
		broken bool
		// cancel cancels the context before the pass.
		cancel bool
		want   recoverySummary
		// wantCalls is the number of mounter calls the pass makes.
		wantCalls int
	}{
		{
			name:    "no records",
			records: 0,
		},
		{
			name:    "one healthy record",
			records: 1,
			want:    recoverySummary{processed: 1, counts: map[recoveryOutcome]int{recoveryHealthy: 1}},
		},
		{
			name:    "a corrupted and a healthy record",
			records: 1,
			corrupt: 1,
			want:    recoverySummary{processed: 2, counts: map[recoveryOutcome]int{recoveryHealthy: 1, recoveryRecovered: 1}},
			// BindTargets, the detach and the mount; the record has no target.
			wantCalls: 3,
		},
		{
			name:    "an invalid record counts as a failure",
			records: 1,
			broken:  true,
			want:    recoverySummary{processed: 2, failures: 1, counts: map[recoveryOutcome]int{recoveryHealthy: 1}},
		},
		{
			name:    "a cancelled context processes nothing",
			records: 2,
			cancel:  true,
			want:    recoverySummary{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			site := newRecoverySite(t)
			for i := range tc.records + tc.corrupt {
				id := "gfs:v1:static:c1:vol" + string(rune('a'+i))
				staging := filepath.Join("/staging", string(rune('a'+i)))
				rec := stageRecord{
					Version:           stageRecordVersion,
					VolumeID:          id,
					StagingTargetPath: staging,
				}
				if err := site.store.save(rec); err != nil {
					t.Fatalf("save(%q) = %v, want nil", id, err)
				}
				if i < tc.corrupt {
					site.fake.mountStateOf[staging] = mounter.MountStateCorrupted
				}
			}
			if tc.broken {
				if err := os.MkdirAll(site.store.dir, recordDirMode); err != nil {
					t.Fatalf("MkdirAll() = %v, want nil", err)
				}
				name := filepath.Join(site.store.dir, recordFileName("gfs:v1:static:c1:broken"))
				if err := os.WriteFile(name, []byte("{"), 0o600); err != nil {
					t.Fatalf("WriteFile() = %v, want nil", err)
				}
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}

			got := site.node.recoverAll(ctx)

			if got.processed != tc.want.processed {
				t.Errorf("processed = %d, want %d", got.processed, tc.want.processed)
			}
			if got.failures != tc.want.failures {
				t.Errorf("failures = %d, want %d", got.failures, tc.want.failures)
			}
			if !reflect.DeepEqual(got.counts, tc.want.counts) {
				t.Errorf("counts = %v, want %v", got.counts, tc.want.counts)
			}
			if got := len(site.fake.callLog()); got != tc.wantCalls {
				t.Errorf("mounter calls = %d, want %d: %v", got, tc.wantCalls, site.fake.callLog())
			}
			if tc.cancel && got.processed != 0 {
				t.Errorf("processed = %d, want 0 for a cancelled context", got.processed)
			}
		})
	}

	t.Run("no record store", func(t *testing.T) {
		node, _ := newTestNode(t)
		node.records = nil

		if got := node.recoverAll(context.Background()); got.processed != 0 {
			t.Fatalf("processed = %d, want 0 without a record store", got.processed)
		}
	})

	t.Run("unreadable record directory", func(t *testing.T) {
		node, _ := newTestNode(t)
		file := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatalf("WriteFile() = %v, want nil", err)
		}
		node.records = &recordStore{dir: file}

		if got := node.recoverAll(context.Background()); got.processed != 0 {
			t.Fatalf("processed = %d, want 0 for an unreadable record directory", got.processed)
		}
	})
}

// TestRunRecovery checks the periodic loop: the interval, the repeat and the stop
// path.
func TestRunRecovery(t *testing.T) {
	t.Run("interval 0 runs exactly one pass", func(t *testing.T) {
		site := newRecoverySite(t, stageRecordOf(testStaging, false, nil))
		// A failing record: one pass, one failure.
		site.fake.mountStateErr = errors.New("mount table unreadable")

		done := make(chan struct{})
		go func() {
			defer close(done)
			site.node.runRecovery(context.Background(), 0)
		}()

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("runRecovery(0) did not return")
		}
		if got := site.fake.mountStateCount(); got != 1 {
			t.Fatalf("VolumeMountState calls = %d, want exactly 1 pass", got)
		}
	})

	t.Run("a repeated interval runs more than one pass", func(t *testing.T) {
		site := newRecoverySite(t, stageRecordOf(testStaging, false, nil))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan struct{})
		go func() {
			defer close(done)
			site.node.runRecovery(ctx, 10*time.Millisecond)
		}()

		// Two passes list the records twice; waiting for the second one keeps the
		// assertion independent of the timing of the first.
		if !waitFor(t, time.Second, func() bool { return site.fake.mountStateCount() >= 2 }) {
			t.Fatal("runRecovery did not run a second pass within 1s")
		}
		cancel()

		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("runRecovery did not return within 1s of cancellation")
		}

		// The state of the fake is what the passes saw; the count of the calls
		// must have stopped growing.
		after := site.fake.mountStateCount()
		time.Sleep(30 * time.Millisecond)
		if got := site.fake.mountStateCount(); got != after {
			t.Fatalf("VolumeMountState calls grew from %d to %d after the cancellation", after, got)
		}
	})

	t.Run("a cancelled context processes nothing", func(t *testing.T) {
		site := newRecoverySite(t, stageRecordOf(testStaging, false, nil))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		site.node.runRecovery(ctx, 10*time.Millisecond)

		if got := site.fake.mountStateCount(); got != 0 {
			t.Fatalf("VolumeMountState calls = %d, want 0 with a cancelled context", got)
		}
	})
}

// waitFor polls cond until it is true or the timeout expires.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return cond()
}

// TestMergeTargets checks the merge of the remembered and the found bind targets.
func TestMergeTargets(t *testing.T) {
	tests := []struct {
		name   string
		stored []recordTarget
		found  []mounter.BindTarget
		want   []recordTarget
	}{
		{
			name:   "nothing stored, nothing found",
			stored: nil,
			found:  nil,
			want:   []recordTarget{},
		},
		{
			name:   "only found",
			stored: nil,
			found:  []mounter.BindTarget{{Path: "/t1"}, {Path: "/t2", ReadOnly: true}},
			want:   []recordTarget{{Path: "/t1"}, {Path: "/t2", ReadOnly: true}},
		},
		{
			name:   "only stored",
			stored: []recordTarget{{Path: "/t1", ReadOnly: true}},
			found:  nil,
			want:   []recordTarget{{Path: "/t1", ReadOnly: true}},
		},
		{
			name:   "found keeps the position and replaces the flag",
			stored: []recordTarget{{Path: "/t1"}, {Path: "/t2", ReadOnly: true}},
			found:  []mounter.BindTarget{{Path: "/t2"}, {Path: "/t3"}},
			want:   []recordTarget{{Path: "/t1"}, {Path: "/t2"}, {Path: "/t3"}},
		},
		{
			name:   "a repeated stored target is deduplicated",
			stored: []recordTarget{{Path: "/t1"}, {Path: "/t1", ReadOnly: true}},
			found:  nil,
			want:   []recordTarget{{Path: "/t1", ReadOnly: true}},
		},
		{
			name:   "a repeated found target keeps its first position",
			stored: nil,
			found:  []mounter.BindTarget{{Path: "/t1"}, {Path: "/t2"}, {Path: "/t1", ReadOnly: true}},
			want:   []recordTarget{{Path: "/t1", ReadOnly: true}, {Path: "/t2"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeTargets(tc.stored, tc.found)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("mergeTargets(%+v, %+v) = %+v, want %+v", tc.stored, tc.found, got, tc.want)
			}
		})
	}
}

// TestRecoveryOutcomeString checks the names of the outcomes and the fallback.
func TestRecoveryOutcomeString(t *testing.T) {
	tests := []struct {
		outcome recoveryOutcome
		want    string
	}{
		{outcome: recoveryHealthy, want: "healthy"},
		{outcome: recoveryBusy, want: "busy"},
		{outcome: recoveryStale, want: "stale"},
		{outcome: recoveryMismatch, want: "mismatch"},
		{outcome: recoveryRecovered, want: "recovered"},
		{outcome: recoveryPartial, want: "partial"},
		{outcome: recoveryOutcome(99), want: "recoveryOutcome(99)"},
	}

	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.outcome.String(); got != tc.want {
				t.Fatalf("recoveryOutcome(%d).String() = %q, want %q", int(tc.outcome), got, tc.want)
			}
		})
	}
}

// TestRecoverySummary checks the rendering of a summary: the fields of the log
// line and the "nothing to report" decision.
func TestRecoverySummary(t *testing.T) {
	tests := []struct {
		name      string
		summary   recoverySummary
		wantIdle  bool
		wantKVs   []any
		wantCount int
	}{
		{
			name:     "empty",
			summary:  recoverySummary{},
			wantIdle: true,
			wantKVs:  []any{"records", 0},
		},
		{
			name:     "only healthy",
			summary:  recoverySummary{processed: 2, counts: map[recoveryOutcome]int{recoveryHealthy: 2}},
			wantIdle: true,
			wantKVs:  []any{"records", 2, "healthy", 2},
		},
		{
			name: "everything",
			summary: recoverySummary{
				processed: 6,
				failures:  1,
				counts: map[recoveryOutcome]int{
					recoveryHealthy:   1,
					recoveryRecovered: 2,
					recoveryPartial:   1,
					recoveryMismatch:  1,
					recoveryStale:     1,
					recoveryBusy:      0,
				},
			},
			wantIdle: false,
			// The order is fixed, the zero entries are skipped.
			wantKVs: []any{"records", 6, "failures", 1, "recovered", 2, "partial", 1, "mismatch", 1, "stale", 1, "healthy", 1},
		},
		{
			name:     "a failure is never idle",
			summary:  recoverySummary{processed: 1, failures: 1},
			wantIdle: false,
			wantKVs:  []any{"records", 1, "failures", 1},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.summary.idle(); got != tc.wantIdle {
				t.Fatalf("idle() = %t, want %t", got, tc.wantIdle)
			}
			if got := tc.summary.fields(); !reflect.DeepEqual(got, tc.wantKVs) {
				t.Fatalf("fields() = %v, want %v", got, tc.wantKVs)
			}
		})
	}
}

// TestRecoverVolumeMountFlagsFromRecord checks that the mount flags of the record
// reach the mount, and that a flag outside the allowlist is reported instead of
// being passed to mount(8): the record is not trusted more than a request.
func TestRecoverVolumeMountFlagsFromRecord(t *testing.T) {
	site := newRecoverySite(t, stageRecordOf(testStaging, false, []string{"log-level=DEBUG"}))
	site.fake.mountStateOf[testStaging] = mounter.MountStateCorrupted

	outcome, err := site.node.recoverVolume(context.Background(), recordFileName(testVolumeID))
	if err != nil {
		t.Fatalf("recoverVolume() = %v, want nil", err)
	}
	if outcome != recoveryRecovered {
		t.Fatalf("recoverVolume() = %v, want recovered", outcome)
	}
	if got := site.fake.mountCalls[0].spec.Options; !reflect.DeepEqual(got, []string{"log-level=DEBUG"}) {
		t.Fatalf("Options = %v, want the recorded flags", got)
	}
}

// TestRecoverVolumeRecordIsRewritten checks that the recovery does not rewrite a
// record it does not have to change, so that a healthy node writes nothing.
func TestRecoverVolumeRecordIsRewritten(t *testing.T) {
	rec := stageRecordOf(testStaging, false, nil)
	site := newRecoverySite(t, rec)

	before, err := os.ReadFile(site.store.path(rec.VolumeID))
	if err != nil {
		t.Fatalf("ReadFile() = %v, want nil", err)
	}
	info, err := os.Stat(site.store.path(rec.VolumeID))
	if err != nil {
		t.Fatalf("Stat() = %v, want nil", err)
	}
	time.Sleep(10 * time.Millisecond)

	if _, recErr := site.node.recoverVolume(context.Background(), recordFileName(rec.VolumeID)); recErr != nil {
		t.Fatalf("recoverVolume() = %v, want nil", recErr)
	}
	after, err := os.Stat(site.store.path(rec.VolumeID))
	if err != nil {
		t.Fatalf("Stat() = %v, want nil", err)
	}
	if !after.ModTime().Equal(info.ModTime()) {
		t.Fatal("a healthy record was rewritten, want it left alone")
	}
	content, err := os.ReadFile(site.store.path(rec.VolumeID))
	if err != nil {
		t.Fatalf("ReadFile() = %v, want nil", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(content, &decoded); err != nil {
		t.Fatalf("Unmarshal() = %v, want nil", err)
	}
	if string(content) != string(before) {
		t.Fatalf("record content changed from %q to %q", before, content)
	}
}
