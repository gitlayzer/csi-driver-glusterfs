package driver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	mount "k8s.io/mount-utils"

	"github.com/gitlayzer/csi-driver-glusterfs/pkg/mounter"
	"github.com/gitlayzer/csi-driver-glusterfs/pkg/volumeid"
)

// testVolumeID is the canonical id of a volume of the "c1" cluster that the test
// configuration defines, in the subdir mode so that a subdir is exercised too.
const testVolumeID = "gfs:v1:subdir:c1:vol:pvc-1"

// testServers are the servers of cluster "c1" in testConfigJSON.
var testServers = []string{"g1", "g2", "g3"}

// mountCall records one call of the fake mounter.
type mountCall struct {
	// spec is the mount spec of MountGlusterfs.
	spec mounter.Spec
	// source, target and readOnly describe BindMount.
	source, target string
	readOnly       bool
}

// fakeMounter is a [mounter.Mounter] that records every call and returns queued
// errors, so that a test can drive a single-use recovery path.
//
// Every method takes the next error of its own queue; an empty queue means
// success. The queues are indexed by method name, so mounting twice with one
// queued error fails only the first call.
type fakeMounter struct {
	mu sync.Mutex

	// mountErrs, bindErrs, unmountErrs, cleanupErrs, bindTargetsErrs and
	// detachErrs are the queued errors.
	mountErrs       []error
	bindErrs        []error
	unmountErrs     []error
	cleanupErrs     []error
	bindTargetsErrs []error
	detachErrs      []error

	// bindTargets and bindTargetsErr are returned by BindTargets.
	bindTargets    []mounter.BindTarget
	bindTargetsErr error

	// isMountPointErr is returned by IsMountPoint; isMountPoint is its result.
	isMountPointErr error
	isMountPoint    bool

	// otherBindMounts and otherBindMountsErr are returned by OtherBindMounts.
	otherBindMounts    []string
	otherBindMountsErr error

	// mountState and mountStateErr are returned by VolumeMountState; mountState
	// defaults to MountStateMounted, the state a successful stage and publish
	// leave behind. mountStateOf overrides the state per target path, which the
	// health tests need because they ask about two paths in one call.
	mountState    mounter.MountState
	mountStateOf  map[string]mounter.MountState
	mountStateErr error
	// mountStateErrOf overrides the error per target path, for the cases that
	// need one path to be readable and another one not.
	mountStateErrOf map[string]error

	mountCalls   []mountCall
	bindCalls    []mountCall
	unmountCalls []string
	cleanupCalls []string
	checkCalls   []string
	otherCalls   []mountCall
	// mountStateCalls records the arguments of every VolumeMountState call.
	mountStateCalls []mountStateCall
	// bindTargetCalls records the source of every BindTargets call and
	// detachCalls the target of every Detach call.
	bindTargetCalls []string
	detachCalls     []string
	// calls is the ordered log of the calls that change or inspect the mount
	// state, spelled "bindTargets:/s", "detach:/s", "unmount:/s", "mount:/s" and
	// "bind:/source->/target:ro". The recovery tests assert the exact order with
	// it, which the per-method call lists cannot express.
	calls []string
	// onDetach is called synchronously at the end of a successful Detach, before
	// the lock is released. Tests use it to observe the state of the world at the
	// moment of the detach, which is the only way to assert that something was
	// saved before it.
	onDetach func(target string)
	// onUnmount is the same hook for Unmount, which the corrupted staging path of
	// NodeStageVolume uses instead of Detach.
	onUnmount func(target string)
	// trackState makes VolumeMountState answer from the state the successful
	// mount calls of the fake left behind, so that a test can run two recovery
	// passes over the same fake. It is off by default: the tests that inject one
	// fixed state rely on VolumeMountState answering the same thing every time.
	trackState bool
}

// mountStateCall records one call of VolumeMountState.
type mountStateCall struct {
	target, volume, subdir string
}

// The compiler rejects drift between the interface and the fake.
var _ mounter.Mounter = (*fakeMounter)(nil)

// record appends an entry to the ordered call log. The caller holds the lock.
func (f *fakeMounter) record(entry string) {
	f.calls = append(f.calls, entry)
}

// roSuffix renders a read-only flag as the "ro"/"rw" suffix of a log entry.
func roSuffix(readOnly bool) string {
	if readOnly {
		return "ro"
	}
	return "rw"
}

// setState records the mount state of a path when the fake tracks it. The caller
// holds the lock.
func (f *fakeMounter) setState(target string, state mounter.MountState) {
	if !f.trackState {
		return
	}
	if f.mountStateOf == nil {
		f.mountStateOf = make(map[string]mounter.MountState)
	}
	f.mountStateOf[target] = state
}

// callLog returns a copy of the ordered call log.
func (f *fakeMounter) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// next pops the first queued error of queue and returns the rest.
func next(queue []error) (rest []error, err error) {
	if len(queue) == 0 {
		return nil, nil
	}
	return queue[1:], queue[0]
}

// MountGlusterfs records the call and returns the next queued error.
func (f *fakeMounter) MountGlusterfs(_ context.Context, spec mounter.Spec, target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	var err error
	f.mountErrs, err = next(f.mountErrs)
	f.mountCalls = append(f.mountCalls, mountCall{spec: spec, target: target})
	f.record("mount:" + target)
	if err == nil {
		f.setState(target, mounter.MountStateMounted)
	}
	return err
}

// BindMount records the call and returns the next queued error.
func (f *fakeMounter) BindMount(_ context.Context, source, target string, readOnly bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	var err error
	f.bindErrs, err = next(f.bindErrs)
	f.bindCalls = append(f.bindCalls, mountCall{source: source, target: target, readOnly: readOnly})
	f.record("bind:" + source + "->" + target + ":" + roSuffix(readOnly))
	if err == nil {
		f.setState(target, mounter.MountStateMounted)
	}
	return err
}

// BindTargets records the call and returns the injected result.
func (f *fakeMounter) BindTargets(source string) ([]mounter.BindTarget, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var err error
	f.bindTargetsErrs, err = next(f.bindTargetsErrs)
	f.bindTargetCalls = append(f.bindTargetCalls, source)
	f.record("bindTargets:" + source)
	if err != nil {
		return nil, err
	}
	return f.bindTargets, f.bindTargetsErr
}

// Detach records the call and returns the next queued error. A successful detach
// of a path the fake tracks leaves it not mounted, which is what a lazy unmount
// of a dead mount does.
func (f *fakeMounter) Detach(target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	var err error
	f.detachErrs, err = next(f.detachErrs)
	f.detachCalls = append(f.detachCalls, target)
	f.record("detach:" + target)
	if err == nil {
		f.setState(target, mounter.MountStateNotMounted)
	}
	if hook := f.onDetach; hook != nil {
		hook(target)
	}
	return err
}

// Unmount records the call and returns the next queued error.
func (f *fakeMounter) Unmount(_ context.Context, target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	var err error
	f.unmountErrs, err = next(f.unmountErrs)
	f.unmountCalls = append(f.unmountCalls, target)
	f.record("unmount:" + target)
	if hook := f.onUnmount; hook != nil {
		hook(target)
	}
	return err
}

// Cleanup records the call and returns the next queued error.
func (f *fakeMounter) Cleanup(_ context.Context, target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	var err error
	f.cleanupErrs, err = next(f.cleanupErrs)
	f.cleanupCalls = append(f.cleanupCalls, target)
	return err
}

// IsMountPoint records the call and returns the injected result.
func (f *fakeMounter) IsMountPoint(target string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.checkCalls = append(f.checkCalls, target)
	return f.isMountPoint, f.isMountPointErr
}

// OtherBindMounts records the call and returns the injected result.
func (f *fakeMounter) OtherBindMounts(source, except string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.otherCalls = append(f.otherCalls, mountCall{source: source, target: except})
	return f.otherBindMounts, f.otherBindMountsErr
}

// VolumeMountState records the call and returns the injected result.
func (f *fakeMounter) VolumeMountState(target, volume, subdir string) (mounter.MountState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.mountStateCalls = append(f.mountStateCalls, mountStateCall{target: target, volume: volume, subdir: subdir})
	if err, ok := f.mountStateErrOf[target]; ok {
		return 0, err
	}
	if state, ok := f.mountStateOf[target]; ok {
		return state, f.mountStateErr
	}
	return f.mountState, f.mountStateErr
}

// mountCount, bindCount, unmountCount and cleanupCount report the call counts.
func (f *fakeMounter) mountCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.mountCalls)
}

func (f *fakeMounter) bindCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bindCalls)
}

func (f *fakeMounter) unmountCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.unmountCalls)
}

func (f *fakeMounter) cleanupCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.cleanupCalls)
}

// mountStateCount returns how often VolumeMountState was called.
func (f *fakeMounter) mountStateCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.mountStateCalls)
}

// newTestNode returns a node service in node mode together with its fake mounter.
//
// The log directory is a real temporary directory so that the tests can assert
// that it was created; the paths of individual requests are temporary directories
// of the caller. The fake reports MountStateMounted, and statFS is replaced by
// fixed numbers, so that a test only has to set up the field it is about.
func newTestNode(t *testing.T) (*nodeService, *fakeMounter) {
	t.Helper()

	opts := nodeOptions(t)
	opts.MountLogDir = filepath.Join(t.TempDir(), "logs")

	d, err := New(opts)
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}

	fake := &fakeMounter{mountState: mounter.MountStateMounted}
	d.node.mounter = fake
	d.node.statFS = func(string) (fsStats, error) { return testFSStats, nil }
	return d.node, fake
}

// testFSStats is the statfs result of the fake node service, with values that a
// response assertion can compare one by one.
var testFSStats = fsStats{
	totalBytes:     1000,
	availableBytes: 400,
	usedBytes:      600,
	totalInodes:    100,
	freeInodes:     60,
	usedInodes:     40,
}

// accessModeCapability builds a mount capability with the given access mode.
func accessModeCapability(mode csi.VolumeCapability_AccessMode_Mode, flags ...string) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{
			Mount: &csi.VolumeCapability_MountVolume{FsType: mounter.FSType, MountFlags: flags},
		},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode},
	}
}

// mountCapability is the capability most tests use: a read-write mount of a
// volume by a single node.
var mountCapability = accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)

// tempPathOf returns a clean absolute path below a fresh temporary directory.
// It is not created: a test decides whether the plugin has to create it.
func tempPathOf(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(t.TempDir(), name)
}

// wantCode fails the test unless err has the given gRPC code.
func wantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()

	if want == codes.OK {
		if err != nil {
			t.Fatalf("got error %v, want success", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("got success, want code %v", want)
	}
	if got := status.Code(err); got != want {
		t.Fatalf("code = %v (%v), want %v", got, err, want)
	}
}

func TestValidateNodeCapability(t *testing.T) {
	tests := []struct {
		name         string
		capability   *csi.VolumeCapability
		wantCode     codes.Code
		wantReadOnly bool
		// wantSingleTarget is true for the modes that allow one target path.
		wantSingleTarget bool
		wantFlags        []string
	}{
		{
			name:       "nil capability",
			capability: nil,
			wantCode:   codes.InvalidArgument,
		},
		{
			name: "no access mode",
			capability: &csi.VolumeCapability{
				AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			},
			wantCode: codes.InvalidArgument,
		},
		{
			name:       "unknown access mode",
			capability: accessModeCapability(csi.VolumeCapability_AccessMode_UNKNOWN),
			wantCode:   codes.InvalidArgument,
		},
		{
			name:       "access mode not defined by the protocol",
			capability: accessModeCapability(csi.VolumeCapability_AccessMode_Mode(99)),
			wantCode:   codes.InvalidArgument,
		},
		{
			name: "no access type",
			capability: &csi.VolumeCapability{
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
				},
			},
			wantCode: codes.InvalidArgument,
		},
		{
			name: "block access type",
			capability: &csi.VolumeCapability{
				AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
				},
			},
			wantCode: codes.FailedPrecondition,
		},
		{
			name: "unsupported file system type",
			capability: &csi.VolumeCapability{
				AccessType: &csi.VolumeCapability_Mount{
					Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"},
				},
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
				},
			},
			wantCode: codes.FailedPrecondition,
		},
		{
			name: "empty file system type",
			capability: &csi.VolumeCapability{
				AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
				},
			},
			wantCode:         codes.OK,
			wantSingleTarget: true,
		},
		{
			name:       "glusterfs file system type with mount flags",
			capability: accessModeCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER, "acl", "log-level=INFO"),
			wantCode:   codes.OK,
			wantFlags:  []string{"acl", "log-level=INFO"},
		},
		{
			name:             "single node writer",
			capability:       accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
			wantCode:         codes.OK,
			wantReadOnly:     false,
			wantSingleTarget: true,
		},
		{
			name:             "single node single writer",
			capability:       accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER),
			wantCode:         codes.OK,
			wantReadOnly:     false,
			wantSingleTarget: true,
		},
		{
			name:         "single node multi writer",
			capability:   accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER),
			wantCode:     codes.OK,
			wantReadOnly: false,
		},
		{
			name:         "multi node single writer",
			capability:   accessModeCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER),
			wantCode:     codes.OK,
			wantReadOnly: false,
		},
		{
			name:         "multi node multi writer",
			capability:   accessModeCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER),
			wantCode:     codes.OK,
			wantReadOnly: false,
		},
		{
			name:             "single node reader only",
			capability:       accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY),
			wantCode:         codes.OK,
			wantReadOnly:     true,
			wantSingleTarget: true,
		},
		{
			name:         "multi node reader only",
			capability:   accessModeCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY),
			wantCode:     codes.OK,
			wantReadOnly: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateNodeCapability(tc.capability)

			if tc.wantCode != codes.OK {
				wantCode(t, err, tc.wantCode)
				if got.readOnly || got.singleTarget || got.mountFlags != nil {
					t.Fatalf("nodeCapability = %+v, want the zero value on error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateNodeCapability() = %v, want nil", err)
			}
			if got.readOnly != tc.wantReadOnly {
				t.Fatalf("readOnly = %t, want %t", got.readOnly, tc.wantReadOnly)
			}
			if got.singleTarget != tc.wantSingleTarget {
				t.Fatalf("singleTarget = %t, want %t", got.singleTarget, tc.wantSingleTarget)
			}
			if len(got.mountFlags) != len(tc.wantFlags) {
				t.Fatalf("mountFlags = %v, want %v", got.mountFlags, tc.wantFlags)
			}
			for i := range tc.wantFlags {
				if got.mountFlags[i] != tc.wantFlags[i] {
					t.Fatalf("mountFlags[%d] = %q, want %q", i, got.mountFlags[i], tc.wantFlags[i])
				}
			}
		})
	}
}

func TestValidatePath(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		wantCode codes.Code
	}{
		{name: "absolute", path: "/var/lib/kubelet/staging", wantCode: codes.OK},
		{name: "root", path: "/", wantCode: codes.OK},
		{name: "relative", path: "var/lib", wantCode: codes.InvalidArgument},
		{name: "dot", path: ".", wantCode: codes.InvalidArgument},
		{name: "parent element", path: "/a/../b", wantCode: codes.InvalidArgument},
		{name: "trailing slash", path: "/a/", wantCode: codes.InvalidArgument},
		{name: "double slash", path: "/a//b", wantCode: codes.InvalidArgument},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wantCode(t, validatePath("staging target path", tc.path), tc.wantCode)
		})
	}
}

func TestInflight(t *testing.T) {
	t.Run("the same key is rejected until it is released", func(t *testing.T) {
		locks := newInflight()

		if !locks.tryAcquire("stage/v1") {
			t.Fatal("first tryAcquire() = false, want true")
		}
		if locks.tryAcquire("stage/v1") {
			t.Fatal("second tryAcquire() = true, want false for a held key")
		}
		locks.release("stage/v1")
		if !locks.tryAcquire("stage/v1") {
			t.Fatal("tryAcquire() after release = false, want true")
		}
	})

	t.Run("different keys do not block each other", func(t *testing.T) {
		locks := newInflight()

		if !locks.tryAcquire("stage/v1") {
			t.Fatal("tryAcquire(stage/v1) = false, want true")
		}
		if !locks.tryAcquire("stage/v2") {
			t.Fatal("tryAcquire(stage/v2) = false, want true for a different key")
		}
		if !locks.tryAcquire("publish/v1") {
			t.Fatal("tryAcquire(publish/v1) = false, want true for a different prefix")
		}
	})

	t.Run("releasing an unknown key is a no-op", func(t *testing.T) {
		locks := newInflight()
		locks.release("never-acquired")
		if !locks.tryAcquire("never-acquired") {
			t.Fatal("tryAcquire() = false, want true after releasing an unheld key")
		}
	})

	t.Run("exactly one of 50 concurrent acquires succeeds", func(t *testing.T) {
		locks := newInflight()

		const workers = 50
		var (
			wg   sync.WaitGroup
			mu   sync.Mutex
			wins int
		)
		start := make(chan struct{})
		wg.Add(workers)
		for range workers {
			go func() {
				defer wg.Done()
				<-start
				if locks.tryAcquire("stage/v1") {
					mu.Lock()
					wins++
					mu.Unlock()
				}
			}()
		}
		close(start)
		wg.Wait()

		if wins != 1 {
			t.Fatalf("%d goroutines acquired the key, want exactly 1", wins)
		}
	})
}

func TestNodeStageVolume(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(t *testing.T, req *csi.NodeStageVolumeRequest, fake *fakeMounter)
		held      bool
		wantCode  codes.Code
		wantMount int
		// check runs after the assertions above; it inspects the recorded calls.
		check func(t *testing.T, node *nodeService, fake *fakeMounter, staging string)
	}{
		{
			name:     "missing volume id",
			mutate:   func(_ *testing.T, req *csi.NodeStageVolumeRequest, _ *fakeMounter) { req.VolumeId = "" },
			wantCode: codes.InvalidArgument,
		},
		{
			name: "missing staging target path",
			mutate: func(_ *testing.T, req *csi.NodeStageVolumeRequest, _ *fakeMounter) {
				req.StagingTargetPath = ""
			},
			wantCode: codes.InvalidArgument,
		},
		{
			name: "missing capability",
			mutate: func(_ *testing.T, req *csi.NodeStageVolumeRequest, _ *fakeMounter) {
				req.VolumeCapability = nil
			},
			wantCode: codes.InvalidArgument,
		},
		{
			name: "relative staging target path",
			mutate: func(_ *testing.T, req *csi.NodeStageVolumeRequest, _ *fakeMounter) {
				req.StagingTargetPath = "staging/vol"
			},
			wantCode: codes.InvalidArgument,
		},
		{
			name: "unclean staging target path",
			mutate: func(_ *testing.T, req *csi.NodeStageVolumeRequest, _ *fakeMounter) {
				req.StagingTargetPath = "/a/../b"
			},
			wantCode: codes.InvalidArgument,
		},
		{
			name: "block capability",
			mutate: func(_ *testing.T, req *csi.NodeStageVolumeRequest, _ *fakeMounter) {
				req.VolumeCapability = &csi.VolumeCapability{
					AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
					AccessMode: &csi.VolumeCapability_AccessMode{
						Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
					},
				}
			},
			wantCode: codes.FailedPrecondition,
		},
		{
			name: "garbage volume id",
			mutate: func(_ *testing.T, req *csi.NodeStageVolumeRequest, _ *fakeMounter) {
				req.VolumeId = "garbage"
			},
			wantCode: codes.NotFound,
		},
		{
			name: "unknown cluster",
			mutate: func(_ *testing.T, req *csi.NodeStageVolumeRequest, _ *fakeMounter) {
				req.VolumeId = "gfs:v1:subdir:nope:vol:pvc-1"
			},
			wantCode: codes.NotFound,
		},
		{
			name: "success",
			mutate: func(_ *testing.T, req *csi.NodeStageVolumeRequest, _ *fakeMounter) {
				req.VolumeCapability = accessModeCapability(
					csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER, "acl", "log-level=INFO")
			},
			wantCode:  codes.OK,
			wantMount: 1,
			check: func(t *testing.T, _ *nodeService, fake *fakeMounter, staging string) {
				t.Helper()
				call := fake.mountCalls[0]
				if call.target != staging {
					t.Errorf("MountGlusterfs target = %q, want %q", call.target, staging)
				}
				if got := strings.Join(call.spec.Servers, ","); got != strings.Join(testServers, ",") {
					t.Errorf("Servers = %v, want %v", call.spec.Servers, testServers)
				}
				if call.spec.Volume != "vol" || call.spec.Subdir != "pvc-1" {
					t.Errorf("Volume/Subdir = %q/%q, want %q/%q", call.spec.Volume, call.spec.Subdir, "vol", "pvc-1")
				}
				if call.spec.ReadOnly {
					t.Error("ReadOnly = true, want false for MULTI_NODE_MULTI_WRITER")
				}
				if got := strings.Join(call.spec.Options, ","); got != "acl,log-level=INFO" {
					t.Errorf("Options = %v, want the mount flags unchanged", call.spec.Options)
				}
				if info, err := os.Stat(staging); err != nil || !info.IsDir() {
					t.Errorf("staging directory %q was not created: %v", staging, err)
				}
			},
		},
		{
			name: "read only access mode is staged read only",
			mutate: func(_ *testing.T, req *csi.NodeStageVolumeRequest, _ *fakeMounter) {
				req.VolumeCapability = accessModeCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY)
			},
			wantCode:  codes.OK,
			wantMount: 1,
			check: func(t *testing.T, _ *nodeService, fake *fakeMounter, _ string) {
				t.Helper()
				if !fake.mountCalls[0].spec.ReadOnly {
					t.Error("ReadOnly = false, want true for MULTI_NODE_READER_ONLY")
				}
			},
		},
		{
			name:      "log directory is created",
			wantCode:  codes.OK,
			wantMount: 1,
			check: func(t *testing.T, node *nodeService, _ *fakeMounter, _ string) {
				t.Helper()
				if info, err := os.Stat(node.logDir); err != nil || !info.IsDir() {
					t.Fatalf("log directory %q was not created: %v", node.logDir, err)
				}
			},
		},
		{
			name: "invalid mount option",
			mutate: func(_ *testing.T, _ *csi.NodeStageVolumeRequest, fake *fakeMounter) {
				fake.mountErrs = []error{fmt.Errorf("%w: unknown", mounter.ErrInvalidOption)}
			},
			wantCode:  codes.InvalidArgument,
			wantMount: 1,
		},
		{
			name: "invalid spec",
			mutate: func(_ *testing.T, _ *csi.NodeStageVolumeRequest, fake *fakeMounter) {
				fake.mountErrs = []error{fmt.Errorf("%w: no servers", mounter.ErrInvalidSpec)}
			},
			wantCode:  codes.InvalidArgument,
			wantMount: 1,
		},
		{
			name: "mount conflict",
			mutate: func(_ *testing.T, _ *csi.NodeStageVolumeRequest, fake *fakeMounter) {
				fake.mountErrs = []error{fmt.Errorf("%w: other volume", mounter.ErrMountConflict)}
			},
			wantCode:  codes.AlreadyExists,
			wantMount: 1,
		},
		{
			name: "backend error",
			mutate: func(_ *testing.T, _ *csi.NodeStageVolumeRequest, fake *fakeMounter) {
				fake.mountErrs = []error{errors.New("mount.glusterfs failed")}
			},
			wantCode:  codes.Internal,
			wantMount: 1,
		},
		{
			name: "cancelled context",
			mutate: func(_ *testing.T, _ *csi.NodeStageVolumeRequest, fake *fakeMounter) {
				fake.mountErrs = []error{context.Canceled}
			},
			wantCode:  codes.Canceled,
			wantMount: 1,
		},
		{
			name: "deadline exceeded",
			mutate: func(_ *testing.T, _ *csi.NodeStageVolumeRequest, fake *fakeMounter) {
				fake.mountErrs = []error{context.DeadlineExceeded}
			},
			wantCode:  codes.DeadlineExceeded,
			wantMount: 1,
		},
		{
			name: "corrupted mount is recovered once",
			mutate: func(_ *testing.T, _ *csi.NodeStageVolumeRequest, fake *fakeMounter) {
				fake.mountErrs = []error{fmt.Errorf("%w: ENOTCONN", mounter.ErrCorruptedMount)}
			},
			wantCode:  codes.OK,
			wantMount: 2,
			check: func(t *testing.T, _ *nodeService, fake *fakeMounter, staging string) {
				t.Helper()
				if got := fake.unmountCount(); got != 1 {
					t.Errorf("Unmount calls = %d, want 1", got)
				}
				if fake.unmountCalls[0] != staging {
					t.Errorf("Unmount target = %q, want %q", fake.unmountCalls[0], staging)
				}
			},
		},
		{
			name: "two corrupted mounts stop after the second attempt",
			mutate: func(_ *testing.T, _ *csi.NodeStageVolumeRequest, fake *fakeMounter) {
				fake.mountErrs = []error{
					fmt.Errorf("%w: ENOTCONN", mounter.ErrCorruptedMount),
					fmt.Errorf("%w: ENOTCONN", mounter.ErrCorruptedMount),
				}
			},
			wantCode:  codes.Internal,
			wantMount: 2,
			check: func(t *testing.T, _ *nodeService, fake *fakeMounter, _ string) {
				t.Helper()
				if got := fake.unmountCount(); got != 1 {
					t.Errorf("Unmount calls = %d, want 1", got)
				}
			},
		},
		{
			name: "unmount failure after a corrupted mount",
			mutate: func(_ *testing.T, _ *csi.NodeStageVolumeRequest, fake *fakeMounter) {
				fake.mountErrs = []error{fmt.Errorf("%w: ENOTCONN", mounter.ErrCorruptedMount)}
				fake.unmountErrs = []error{errors.New("device busy")}
			},
			wantCode:  codes.Internal,
			wantMount: 1,
		},
		{
			name:     "concurrent operation on the same volume",
			held:     true,
			wantCode: codes.Aborted,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node, fake := newTestNode(t)
			staging := tempPathOf(t, "staging")

			req := &csi.NodeStageVolumeRequest{
				VolumeId:          testVolumeID,
				StagingTargetPath: staging,
				VolumeCapability:  mountCapability,
			}
			if tc.mutate != nil {
				tc.mutate(t, req, fake)
			}
			if tc.held {
				key := nodeStageLockPrefix + req.GetVolumeId()
				if !node.locks.tryAcquire(key) {
					t.Fatal("could not pre-acquire the lock")
				}
				defer node.locks.release(key)
			}

			_, err := node.NodeStageVolume(context.Background(), req)
			wantCode(t, err, tc.wantCode)

			if got := fake.mountCount(); got != tc.wantMount {
				t.Fatalf("MountGlusterfs calls = %d, want %d", got, tc.wantMount)
			}
			if tc.held && fake.mountCount() != 0 {
				t.Fatal("the mounter was called although the lock was held")
			}
			if tc.check != nil {
				tc.check(t, node, fake, req.GetStagingTargetPath())
			}
		})
	}
}

// TestNodeStageVolumeSecrets checks that a secret of the request never reaches
// the error message of a failing stage call.
func TestNodeStageVolumeSecrets(t *testing.T) {
	const secret = "sup3r-s3cret-p4ssword"

	tests := []struct {
		name     string
		mountErr error
	}{
		{name: "success", mountErr: nil},
		{name: "backend failure", mountErr: errors.New("mount.glusterfs failed: permission denied")},
		{name: "invalid option", mountErr: fmt.Errorf("%w: bad", mounter.ErrInvalidOption)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node, fake := newTestNode(t)
			if tc.mountErr != nil {
				fake.mountErrs = []error{tc.mountErr}
			}

			_, err := node.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
				VolumeId:          testVolumeID,
				StagingTargetPath: tempPathOf(t, "staging"),
				VolumeCapability:  mountCapability,
				Secrets:           map[string]string{"password": secret},
			})
			if err != nil && strings.Contains(err.Error(), secret) {
				t.Fatalf("error %q must not contain the secret", err)
			}
		})
	}
}

func TestNodeUnstageVolume(t *testing.T) {
	tests := []struct {
		name           string
		volumeID       string
		staging        func(t *testing.T) string
		held           bool
		unmountErr     error
		wantCode       codes.Code
		wantUnmount    int
		wantCleanupOff bool
	}{
		{
			name:     "missing volume id",
			volumeID: "",
			wantCode: codes.InvalidArgument,
		},
		{
			name:     "missing staging target path",
			volumeID: testVolumeID,
			staging:  func(*testing.T) string { return "" },
			wantCode: codes.InvalidArgument,
		},
		{
			name:     "relative staging target path",
			volumeID: testVolumeID,
			staging:  func(*testing.T) string { return "staging" },
			wantCode: codes.InvalidArgument,
		},
		{
			name:     "garbage volume id",
			volumeID: "garbage",
			wantCode: codes.NotFound,
		},
		{
			name:        "unmounts the staging path",
			volumeID:    testVolumeID,
			wantCode:    codes.OK,
			wantUnmount: 1,
		},
		{
			name:        "cluster is not consulted",
			volumeID:    "gfs:v1:static:deleted-cluster:vol",
			wantCode:    codes.OK,
			wantUnmount: 1,
		},
		{
			name:        "unmount failure",
			volumeID:    testVolumeID,
			unmountErr:  errors.New("device busy"),
			wantCode:    codes.Internal,
			wantUnmount: 1,
		},
		{
			name:        "cancelled context",
			volumeID:    testVolumeID,
			unmountErr:  context.Canceled,
			wantCode:    codes.Canceled,
			wantUnmount: 1,
		},
		{
			name:     "concurrent operation on the same volume",
			volumeID: testVolumeID,
			held:     true,
			wantCode: codes.Aborted,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node, fake := newTestNode(t)
			fake.unmountErrs = nil
			if tc.unmountErr != nil {
				fake.unmountErrs = []error{tc.unmountErr}
			}

			staging := tc.staging
			path := "/var/lib/kubelet/plugins/staging"
			if staging != nil {
				path = staging(t)
			}
			if tc.held {
				key := nodeStageLockPrefix + tc.volumeID
				if !node.locks.tryAcquire(key) {
					t.Fatal("could not pre-acquire the lock")
				}
				defer node.locks.release(key)
			}

			_, err := node.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
				VolumeId:          tc.volumeID,
				StagingTargetPath: path,
			})
			wantCode(t, err, tc.wantCode)

			if got := fake.unmountCount(); got != tc.wantUnmount {
				t.Fatalf("Unmount calls = %d, want %d", got, tc.wantUnmount)
			}
			// The staging directory belongs to kubelet, so Cleanup must never run.
			if got := fake.cleanupCount(); got != 0 {
				t.Fatalf("Cleanup calls = %d, want 0", got)
			}
		})
	}
}

// TestNodeStageVolumeRecord checks the staging record NodeStageVolume writes for
// the recovery: its fields, the cases where it must not be written, and the
// failure of the write itself.
func TestNodeStageVolumeRecord(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(t *testing.T, req *csi.NodeStageVolumeRequest, fake *fakeMounter, node *nodeService)
		wantCode  codes.Code
		wantMount int
		// wantRecord is true when a record of the request must exist afterwards.
		wantRecord bool
		// check runs after the generic assertions.
		check func(t *testing.T, rec stageRecord, fake *fakeMounter, staging string)
	}{
		{
			name:       "a successful stage writes the record",
			wantCode:   codes.OK,
			wantMount:  1,
			wantRecord: true,
		},
		{
			name: "mount flags and the read-only flag are recorded",
			mutate: func(_ *testing.T, req *csi.NodeStageVolumeRequest, _ *fakeMounter, _ *nodeService) {
				req.VolumeCapability = accessModeCapability(
					csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY, "acl", "log-level=INFO")
			},
			wantCode:   codes.OK,
			wantMount:  1,
			wantRecord: true,
			check: func(t *testing.T, rec stageRecord, _ *fakeMounter, staging string) {
				t.Helper()
				if !rec.ReadOnly {
					t.Error("ReadOnly = false, want true for MULTI_NODE_READER_ONLY")
				}
				if got := strings.Join(rec.MountFlags, ","); got != "acl,log-level=INFO" {
					t.Errorf("MountFlags = %v, want the request mount flags", rec.MountFlags)
				}
				if rec.StagingTargetPath != staging {
					t.Errorf("StagingTargetPath = %q, want %q", rec.StagingTargetPath, staging)
				}
				if len(rec.Targets) != 0 {
					t.Errorf("Targets = %+v, want none after a successful stage", rec.Targets)
				}
			},
		},
		{
			name: "a repeated stage rewrites the record and keeps one file",
			mutate: func(_ *testing.T, _ *csi.NodeStageVolumeRequest, _ *fakeMounter, node *nodeService) {
				// A left over target of an interrupted recovery that is no longer
				// corrupted (the fake reports every path mounted) is dropped by the
				// successful stage.
				if err := node.records.save(stageRecord{
					Version:           stageRecordVersion,
					VolumeID:          testVolumeID,
					StagingTargetPath: "/somewhere/else",
					Targets:           []recordTarget{{Path: "/stale/target"}},
				}); err != nil {
					t.Fatalf("save() = %v, want nil", err)
				}
			},
			wantCode:   codes.OK,
			wantMount:  1,
			wantRecord: true,
			check: func(t *testing.T, rec stageRecord, _ *fakeMounter, _ string) {
				t.Helper()
				if len(rec.Targets) != 0 {
					t.Errorf("Targets = %+v, want them cleared by the stage", rec.Targets)
				}
			},
		},
		{
			name: "a corrupted staging mount keeps its bind targets and refreshes them",
			mutate: func(t *testing.T, _ *csi.NodeStageVolumeRequest, fake *fakeMounter, node *nodeService) {
				fake.mountErrs = []error{fmt.Errorf("%w: ENOTCONN", mounter.ErrCorruptedMount)}
				fake.trackState = true
				fake.mountStateOf = map[string]mounter.MountState{
					testTarget1: mounter.MountStateCorrupted,
					testTarget2: mounter.MountStateCorrupted,
				}
				fake.bindTargets = []mounter.BindTarget{{Path: testTarget1, ReadOnly: true}, {Path: testTarget2}}
				// The targets must be on disk before the unmount removes the
				// mountinfo entry that identifies them.
				fake.onUnmount = func(string) {
					rec, err := node.records.load(testVolumeID)
					if err != nil {
						t.Errorf("load() at the unmount = %v, want the saved record", err)
						return
					}
					want := []recordTarget{{Path: testTarget1, ReadOnly: true}, {Path: testTarget2}}
					if !slices.Equal(rec.Targets, want) {
						t.Errorf("Targets at the unmount = %+v, want %+v", rec.Targets, want)
					}
				}
			},
			wantCode:   codes.OK,
			wantMount:  2,
			wantRecord: true,
			check: func(t *testing.T, rec stageRecord, fake *fakeMounter, staging string) {
				t.Helper()
				want := []string{
					"mount:" + staging,
					"bindTargets:" + staging,
					"unmount:" + staging,
					"mount:" + staging,
					"detach:" + testTarget1,
					"bind:" + staging + "->" + testTarget1 + ":ro",
					"detach:" + testTarget2,
					"bind:" + staging + "->" + testTarget2 + ":rw",
				}
				if got := fake.callLog(); !slices.Equal(got, want) {
					t.Errorf("calls = %q, want %q", got, want)
				}
				if len(rec.Targets) != 0 {
					t.Errorf("Targets = %+v, want none after the refresh", rec.Targets)
				}
			},
		},
		{
			name: "a target that cannot be bound again stays in the record",
			mutate: func(_ *testing.T, _ *csi.NodeStageVolumeRequest, fake *fakeMounter, _ *nodeService) {
				fake.mountErrs = []error{fmt.Errorf("%w: ENOTCONN", mounter.ErrCorruptedMount)}
				fake.bindErrs = []error{errors.New("device busy")}
				fake.trackState = true
				fake.mountStateOf = map[string]mounter.MountState{
					testTarget1: mounter.MountStateCorrupted,
					testTarget2: mounter.MountStateCorrupted,
				}
				fake.bindTargets = []mounter.BindTarget{{Path: testTarget1, ReadOnly: true}, {Path: testTarget2}}
			},
			wantCode:   codes.OK,
			wantMount:  2,
			wantRecord: true,
			check: func(t *testing.T, rec stageRecord, _ *fakeMounter, _ string) {
				t.Helper()
				want := []recordTarget{{Path: testTarget1, ReadOnly: true}}
				if !slices.Equal(rec.Targets, want) {
					t.Errorf("Targets = %+v, want %+v for the next recovery pass", rec.Targets, want)
				}
			},
		},
		{
			name: "the targets of an interrupted recovery are refreshed by the stage",
			mutate: func(t *testing.T, _ *csi.NodeStageVolumeRequest, fake *fakeMounter, node *nodeService) {
				t.Helper()
				fake.trackState = true
				fake.mountStateOf = map[string]mounter.MountState{testTarget1: mounter.MountStateCorrupted}
				if err := node.records.save(stageRecord{
					Version:           stageRecordVersion,
					VolumeID:          testVolumeID,
					StagingTargetPath: testStaging,
					Targets:           []recordTarget{{Path: testTarget1}},
				}); err != nil {
					t.Fatalf("save() = %v, want nil", err)
				}
			},
			wantCode:   codes.OK,
			wantMount:  1,
			wantRecord: true,
			check: func(t *testing.T, rec stageRecord, fake *fakeMounter, staging string) {
				t.Helper()
				want := []string{
					"mount:" + staging,
					"detach:" + testTarget1,
					"bind:" + staging + "->" + testTarget1 + ":rw",
				}
				if got := fake.callLog(); !slices.Equal(got, want) {
					t.Errorf("calls = %q, want %q", got, want)
				}
				if len(rec.Targets) != 0 {
					t.Errorf("Targets = %+v, want none after the refresh", rec.Targets)
				}
			},
		},
		{
			name: "bind targets that cannot be read do not fail the stage",
			mutate: func(_ *testing.T, _ *csi.NodeStageVolumeRequest, fake *fakeMounter, _ *nodeService) {
				fake.mountErrs = []error{fmt.Errorf("%w: ENOTCONN", mounter.ErrCorruptedMount)}
				fake.bindTargetsErrs = []error{errors.New("no mountinfo")}
			},
			wantCode:   codes.OK,
			wantMount:  2,
			wantRecord: true,
			check: func(t *testing.T, rec stageRecord, fake *fakeMounter, _ string) {
				t.Helper()
				if got := fake.unmountCount(); got != 1 {
					t.Errorf("Unmount calls = %d, want 1", got)
				}
				if len(rec.Targets) != 0 {
					t.Errorf("Targets = %+v, want none", rec.Targets)
				}
			},
		},
		{
			name: "bind targets that cannot be recorded stop before the unmount",
			mutate: func(t *testing.T, _ *csi.NodeStageVolumeRequest, fake *fakeMounter, node *nodeService) {
				t.Helper()
				fake.mountErrs = []error{fmt.Errorf("%w: ENOTCONN", mounter.ErrCorruptedMount)}
				fake.bindTargets = []mounter.BindTarget{{Path: testTarget1}}
				file := filepath.Join(t.TempDir(), "file")
				if err := os.WriteFile(file, nil, 0o600); err != nil {
					t.Fatalf("WriteFile() = %v, want nil", err)
				}
				node.records = &recordStore{dir: filepath.Join(file, "staged")}
			},
			wantCode:  codes.Internal,
			wantMount: 1,
			check: func(t *testing.T, _ stageRecord, fake *fakeMounter, _ string) {
				t.Helper()
				if got := fake.unmountCount(); got != 0 {
					t.Errorf("Unmount calls = %d, want 0: the targets would be lost", got)
				}
			},
		},
		{
			name: "a failed mount writes no record",
			mutate: func(_ *testing.T, _ *csi.NodeStageVolumeRequest, fake *fakeMounter, _ *nodeService) {
				fake.mountErrs = []error{errors.New("gluster is down")}
			},
			wantCode:  codes.Internal,
			wantMount: 1,
		},
		{
			name: "an unwritable state directory is Internal",
			mutate: func(t *testing.T, _ *csi.NodeStageVolumeRequest, _ *fakeMounter, node *nodeService) {
				t.Helper()
				file := filepath.Join(t.TempDir(), "file")
				if err := os.WriteFile(file, nil, 0o600); err != nil {
					t.Fatalf("WriteFile() = %v, want nil", err)
				}
				node.records = &recordStore{dir: filepath.Join(file, "staged")}
			},
			wantCode:  codes.Internal,
			wantMount: 1,
			// The mount is kept: repeating the request only rewrites the record.
			check: func(t *testing.T, _ stageRecord, fake *fakeMounter, _ string) {
				t.Helper()
				if got := fake.unmountCount(); got != 0 {
					t.Errorf("Unmount calls = %d, want 0: the successful mount must be kept", got)
				}
			},
		},
		{
			name: "no state directory writes no record",
			mutate: func(_ *testing.T, _ *csi.NodeStageVolumeRequest, _ *fakeMounter, node *nodeService) {
				node.records = nil
			},
			wantCode:  codes.OK,
			wantMount: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node, fake := newTestNode(t)
			store := newTestRecordStore(t)
			node.records = store
			staging := tempPathOf(t, "staging")

			req := &csi.NodeStageVolumeRequest{
				VolumeId:          testVolumeID,
				StagingTargetPath: staging,
				VolumeCapability:  mountCapability,
			}
			if tc.mutate != nil {
				tc.mutate(t, req, fake, node)
			}

			_, err := node.NodeStageVolume(context.Background(), req)
			wantCode(t, err, tc.wantCode)
			if got := fake.mountCount(); got != tc.wantMount {
				t.Fatalf("MountGlusterfs calls = %d, want %d", got, tc.wantMount)
			}

			names, listErr := store.list()
			if listErr != nil {
				t.Fatalf("list() = %v, want nil", listErr)
			}
			if !tc.wantRecord {
				if len(names) != 0 {
					t.Fatalf("records = %v, want none", names)
				}
				return
			}
			if len(names) != 1 {
				t.Fatalf("records = %v, want exactly one", names)
			}
			rec, loadErr := store.load(req.GetVolumeId())
			if loadErr != nil {
				t.Fatalf("load() = %v, want nil", loadErr)
			}
			if rec.Version != stageRecordVersion || rec.VolumeID != testVolumeID {
				t.Fatalf("record = %+v, want version %d of volume %s", rec, stageRecordVersion, testVolumeID)
			}
			if tc.check != nil {
				tc.check(t, rec, fake, staging)
			}
		})
	}
}

// TestNodeUnstageVolumeRecord checks that NodeUnstageVolume removes the staging
// record only after a successful unmount.
func TestNodeUnstageVolumeRecord(t *testing.T) {
	tests := []struct {
		name string
		// noRecord starts without a record.
		noRecord  bool
		unmountEr error
		wantCode  codes.Code
		// wantRecord is true when the record must still be there afterwards.
		wantRecord bool
	}{
		{name: "a successful unstage removes the record", wantCode: codes.OK},
		{name: "a failed unmount keeps the record", unmountEr: errors.New("device busy"), wantCode: codes.Internal, wantRecord: true},
		{name: "an unstage without a record is idempotent", noRecord: true, wantCode: codes.OK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node, fake := newTestNode(t)
			store := newTestRecordStore(t)
			node.records = store
			staging := tempPathOf(t, "staging")

			req := &csi.NodeUnstageVolumeRequest{VolumeId: testVolumeID, StagingTargetPath: staging}
			if !tc.noRecord {
				rec := stageRecord{
					Version:           stageRecordVersion,
					VolumeID:          testVolumeID,
					StagingTargetPath: staging,
				}
				if err := store.save(rec); err != nil {
					t.Fatalf("save() = %v, want nil", err)
				}
			}
			if tc.unmountEr != nil {
				fake.unmountErrs = []error{tc.unmountEr}
			}

			_, err := node.NodeUnstageVolume(context.Background(), req)
			wantCode(t, err, tc.wantCode)

			_, loadErr := store.load(testVolumeID)
			if tc.wantRecord {
				if loadErr != nil {
					t.Fatalf("load() = %v, want the record to be kept", loadErr)
				}
			} else if !errors.Is(loadErr, os.ErrNotExist) {
				t.Fatalf("load() = %v, want the record to be gone", loadErr)
			}

			// The repetition of an idempotent unstage succeeds in both cases.
			if !tc.wantRecord {
				_, err := node.NodeUnstageVolume(context.Background(), req)
				wantCode(t, err, codes.OK)
			}
		})
	}

	t.Run("no state directory removes nothing", func(t *testing.T) {
		node, _ := newTestNode(t)
		node.records = nil

		_, err := node.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
			VolumeId:          testVolumeID,
			StagingTargetPath: tempPathOf(t, "staging"),
		})
		wantCode(t, err, codes.OK)
	})

	t.Run("a removal failure is Internal", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root can remove a file from a read-only directory")
		}
		node, _ := newTestNode(t)
		store := newTestRecordStore(t)
		node.records = store
		staging := tempPathOf(t, "staging")
		rec := stageRecord{
			Version:           stageRecordVersion,
			VolumeID:          testVolumeID,
			StagingTargetPath: staging,
		}
		if err := store.save(rec); err != nil {
			t.Fatalf("save() = %v, want nil", err)
		}
		if err := os.Chmod(store.dir, 0o500); err != nil {
			t.Fatalf("Chmod() = %v, want nil", err)
		}
		t.Cleanup(func() { _ = os.Chmod(store.dir, recordDirMode) })

		_, err := node.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
			VolumeId:          testVolumeID,
			StagingTargetPath: staging,
		})
		wantCode(t, err, codes.Internal)
	})
}

func TestNodePublishVolume(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, req *csi.NodePublishVolumeRequest, fake *fakeMounter)
		held   bool
		// heldVolume pre-acquires the single target lock of the volume.
		heldVolume bool
		wantCode   codes.Code
		wantBind   int
		wantCheck  int
		// wantOther is the number of OtherBindMounts calls.
		wantOther int
		// check runs after the generic assertions.
		check func(t *testing.T, fake *fakeMounter, staging, target string)
	}{
		{
			name:     "missing volume id",
			mutate:   func(_ *testing.T, req *csi.NodePublishVolumeRequest, _ *fakeMounter) { req.VolumeId = "" },
			wantCode: codes.InvalidArgument,
		},
		{
			name: "missing target path",
			mutate: func(_ *testing.T, req *csi.NodePublishVolumeRequest, _ *fakeMounter) {
				req.TargetPath = ""
			},
			wantCode: codes.InvalidArgument,
		},
		{
			name: "missing capability",
			mutate: func(_ *testing.T, req *csi.NodePublishVolumeRequest, _ *fakeMounter) {
				req.VolumeCapability = nil
			},
			wantCode: codes.InvalidArgument,
		},
		{
			name: "missing staging target path",
			mutate: func(_ *testing.T, req *csi.NodePublishVolumeRequest, _ *fakeMounter) {
				req.StagingTargetPath = ""
			},
			wantCode: codes.FailedPrecondition,
		},
		{
			name: "relative target path",
			mutate: func(_ *testing.T, req *csi.NodePublishVolumeRequest, _ *fakeMounter) {
				req.TargetPath = "target"
			},
			wantCode: codes.InvalidArgument,
		},
		{
			name: "unclean staging target path",
			mutate: func(_ *testing.T, req *csi.NodePublishVolumeRequest, _ *fakeMounter) {
				req.StagingTargetPath = "/a/../b"
			},
			wantCode: codes.InvalidArgument,
		},
		{
			name: "block capability",
			mutate: func(_ *testing.T, req *csi.NodePublishVolumeRequest, _ *fakeMounter) {
				req.VolumeCapability = &csi.VolumeCapability{
					AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
					AccessMode: &csi.VolumeCapability_AccessMode{
						Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
					},
				}
			},
			wantCode: codes.FailedPrecondition,
		},
		{
			name: "garbage volume id",
			mutate: func(_ *testing.T, req *csi.NodePublishVolumeRequest, _ *fakeMounter) {
				req.VolumeId = "garbage"
			},
			wantCode: codes.NotFound,
		},
		{
			name: "staging is not a mount point",
			mutate: func(_ *testing.T, _ *csi.NodePublishVolumeRequest, fake *fakeMounter) {
				fake.isMountPoint = false
			},
			wantCode:  codes.FailedPrecondition,
			wantCheck: 1,
		},
		{
			name: "staging does not exist",
			mutate: func(_ *testing.T, _ *csi.NodePublishVolumeRequest, fake *fakeMounter) {
				fake.isMountPointErr = fmt.Errorf("stat: %w", os.ErrNotExist)
			},
			wantCode:  codes.FailedPrecondition,
			wantCheck: 1,
		},
		{
			name: "staging mount is corrupted",
			mutate: func(_ *testing.T, _ *csi.NodePublishVolumeRequest, fake *fakeMounter) {
				fake.isMountPointErr = fmt.Errorf("%w: ENOTCONN", mounter.ErrCorruptedMount)
			},
			wantCode:  codes.Internal,
			wantCheck: 1,
		},
		{
			name: "staging check fails",
			mutate: func(_ *testing.T, _ *csi.NodePublishVolumeRequest, fake *fakeMounter) {
				fake.isMountPointErr = errors.New("mount table unavailable")
			},
			wantCode:  codes.Internal,
			wantCheck: 1,
		},
		{
			name: "success creates the target directory",
			mutate: func(_ *testing.T, _ *csi.NodePublishVolumeRequest, fake *fakeMounter) {
				fake.isMountPoint = true
			},
			wantCode:  codes.OK,
			wantBind:  1,
			wantCheck: 1,
			wantOther: 1,
			check: func(t *testing.T, fake *fakeMounter, staging, target string) {
				t.Helper()
				call := fake.bindCalls[0]
				if call.source != staging || call.target != target {
					t.Errorf("BindMount(%q, %q), want (%q, %q)", call.source, call.target, staging, target)
				}
				if call.readOnly {
					t.Error("readOnly = true, want false for a writable capability")
				}
				if info, err := os.Stat(target); err != nil || !info.IsDir() {
					t.Errorf("target directory %q was not created: %v", target, err)
				}
			},
		},
		{
			name: "readonly request flag is passed through",
			mutate: func(_ *testing.T, req *csi.NodePublishVolumeRequest, fake *fakeMounter) {
				fake.isMountPoint = true
				req.Readonly = true
			},
			wantCode:  codes.OK,
			wantBind:  1,
			wantCheck: 1,
			wantOther: 1,
			check: func(t *testing.T, fake *fakeMounter, _, _ string) {
				t.Helper()
				if !fake.bindCalls[0].readOnly {
					t.Error("readOnly = false, want true for a readonly request")
				}
			},
		},
		{
			name: "read only access mode makes the bind mount read only",
			mutate: func(_ *testing.T, req *csi.NodePublishVolumeRequest, fake *fakeMounter) {
				fake.isMountPoint = true
				req.VolumeCapability = accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY)
			},
			wantCode:  codes.OK,
			wantBind:  1,
			wantCheck: 1,
			wantOther: 1,
			check: func(t *testing.T, fake *fakeMounter, _, _ string) {
				t.Helper()
				if !fake.bindCalls[0].readOnly {
					t.Error("readOnly = false, want true for SINGLE_NODE_READER_ONLY")
				}
			},
		},
		{
			name: "mount conflict",
			mutate: func(_ *testing.T, _ *csi.NodePublishVolumeRequest, fake *fakeMounter) {
				fake.isMountPoint = true
				fake.bindErrs = []error{fmt.Errorf("%w: ro mismatch", mounter.ErrMountConflict)}
			},
			wantCode:  codes.AlreadyExists,
			wantBind:  1,
			wantCheck: 1,
			wantOther: 1,
		},
		{
			name: "backend failure",
			mutate: func(_ *testing.T, _ *csi.NodePublishVolumeRequest, fake *fakeMounter) {
				fake.isMountPoint = true
				fake.bindErrs = []error{errors.New("bind failed")}
			},
			wantCode:  codes.Internal,
			wantBind:  1,
			wantCheck: 1,
			wantOther: 1,
		},
		{
			name: "corrupted target is recovered once",
			mutate: func(_ *testing.T, _ *csi.NodePublishVolumeRequest, fake *fakeMounter) {
				fake.isMountPoint = true
				fake.bindErrs = []error{fmt.Errorf("%w: ENOTCONN", mounter.ErrCorruptedMount)}
			},
			wantCode:  codes.OK,
			wantBind:  2,
			wantCheck: 1,
			wantOther: 1,
			check: func(t *testing.T, fake *fakeMounter, _, target string) {
				t.Helper()
				if got := fake.unmountCount(); got != 1 {
					t.Errorf("Unmount calls = %d, want 1", got)
				}
				if fake.unmountCalls[0] != target {
					t.Errorf("Unmount target = %q, want %q", fake.unmountCalls[0], target)
				}
			},
		},
		{
			name: "two corrupted binds stop after the second attempt",
			mutate: func(_ *testing.T, _ *csi.NodePublishVolumeRequest, fake *fakeMounter) {
				fake.isMountPoint = true
				fake.bindErrs = []error{
					fmt.Errorf("%w: ENOTCONN", mounter.ErrCorruptedMount),
					fmt.Errorf("%w: ENOTCONN", mounter.ErrCorruptedMount),
				}
			},
			wantCode:  codes.Internal,
			wantBind:  2,
			wantCheck: 1,
			wantOther: 1,
		},
		{
			name: "SINGLE_NODE_WRITER already published at another target",
			mutate: func(_ *testing.T, req *csi.NodePublishVolumeRequest, fake *fakeMounter) {
				fake.isMountPoint = true
				fake.otherBindMounts = []string{"/var/lib/kubelet/pods/other/mount"}
				req.VolumeCapability = accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)
			},
			wantCode:  codes.FailedPrecondition,
			wantBind:  0,
			wantCheck: 1,
			wantOther: 1,
		},
		{
			name: "SINGLE_NODE_READER_ONLY already published at another target",
			mutate: func(_ *testing.T, req *csi.NodePublishVolumeRequest, fake *fakeMounter) {
				fake.isMountPoint = true
				fake.otherBindMounts = []string{"/var/lib/kubelet/pods/other/mount"}
				req.VolumeCapability = accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY)
			},
			wantCode:  codes.FailedPrecondition,
			wantBind:  0,
			wantCheck: 1,
			wantOther: 1,
		},
		{
			name: "SINGLE_NODE_SINGLE_WRITER already published at another target",
			mutate: func(_ *testing.T, req *csi.NodePublishVolumeRequest, fake *fakeMounter) {
				fake.isMountPoint = true
				fake.otherBindMounts = []string{"/var/lib/kubelet/pods/other/mount"}
				req.VolumeCapability = accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER)
			},
			wantCode:  codes.FailedPrecondition,
			wantBind:  0,
			wantCheck: 1,
			wantOther: 1,
		},
		{
			name: "SINGLE_NODE_MULTI_WRITER already published at another target",
			mutate: func(_ *testing.T, req *csi.NodePublishVolumeRequest, fake *fakeMounter) {
				fake.isMountPoint = true
				fake.otherBindMounts = []string{"/var/lib/kubelet/pods/other/mount"}
				req.VolumeCapability = accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER)
			},
			wantCode:  codes.OK,
			wantBind:  1,
			wantCheck: 1,
			wantOther: 0,
		},
		{
			name: "MULTI_NODE_SINGLE_WRITER already published at another target",
			mutate: func(_ *testing.T, req *csi.NodePublishVolumeRequest, fake *fakeMounter) {
				fake.isMountPoint = true
				fake.otherBindMounts = []string{"/var/lib/kubelet/pods/other/mount"}
				req.VolumeCapability = accessModeCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER)
			},
			wantCode:  codes.OK,
			wantBind:  1,
			wantCheck: 1,
			wantOther: 0,
		},
		{
			name: "MULTI_NODE_READER_ONLY already published at another target",
			mutate: func(_ *testing.T, req *csi.NodePublishVolumeRequest, fake *fakeMounter) {
				fake.isMountPoint = true
				fake.otherBindMounts = []string{"/var/lib/kubelet/pods/other/mount"}
				req.VolumeCapability = accessModeCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY)
			},
			wantCode:  codes.OK,
			wantBind:  1,
			wantCheck: 1,
			wantOther: 0,
		},
		{
			name: "MULTI_NODE_MULTI_WRITER already published at another target",
			mutate: func(_ *testing.T, req *csi.NodePublishVolumeRequest, fake *fakeMounter) {
				fake.isMountPoint = true
				fake.otherBindMounts = []string{"/var/lib/kubelet/pods/other/mount"}
				req.VolumeCapability = accessModeCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)
			},
			wantCode:  codes.OK,
			wantBind:  1,
			wantCheck: 1,
			wantOther: 0,
		},
		{
			name: "single target check asks about the request paths",
			mutate: func(_ *testing.T, _ *csi.NodePublishVolumeRequest, fake *fakeMounter) {
				fake.isMountPoint = true
			},
			wantCode:  codes.OK,
			wantBind:  1,
			wantCheck: 1,
			wantOther: 1,
			check: func(t *testing.T, fake *fakeMounter, staging, target string) {
				t.Helper()
				if call := fake.otherCalls[0]; call.source != staging || call.target != target {
					t.Errorf("OtherBindMounts(%q, %q), want (%q, %q)", call.source, call.target, staging, target)
				}
			},
		},
		{
			name: "single target check fails",
			mutate: func(_ *testing.T, _ *csi.NodePublishVolumeRequest, fake *fakeMounter) {
				fake.isMountPoint = true
				fake.otherBindMountsErr = errors.New("mountinfo unreadable")
			},
			wantCode:  codes.Internal,
			wantCheck: 1,
			wantOther: 1,
		},
		{
			name: "concurrent publish of a single target volume",
			mutate: func(_ *testing.T, _ *csi.NodePublishVolumeRequest, fake *fakeMounter) {
				fake.isMountPoint = true
			},
			heldVolume: true,
			wantCode:   codes.Aborted,
			wantCheck:  1,
		},
		{
			name: "multi writer publish ignores the single target lock",
			mutate: func(_ *testing.T, req *csi.NodePublishVolumeRequest, fake *fakeMounter) {
				fake.isMountPoint = true
				req.VolumeCapability = accessModeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER)
			},
			heldVolume: true,
			wantCode:   codes.OK,
			wantBind:   1,
			wantCheck:  1,
		},
		{
			name:     "concurrent operation on the same target",
			held:     true,
			wantCode: codes.Aborted,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node, fake := newTestNode(t)
			staging := tempPathOf(t, "staging")
			target := tempPathOf(t, "target")

			req := &csi.NodePublishVolumeRequest{
				VolumeId:          testVolumeID,
				StagingTargetPath: staging,
				TargetPath:        target,
				VolumeCapability:  mountCapability,
			}
			if tc.mutate != nil {
				tc.mutate(t, req, fake)
			}
			if tc.held {
				key := nodePublishLockPrefix + req.GetTargetPath()
				if !node.locks.tryAcquire(key) {
					t.Fatal("could not pre-acquire the lock")
				}
				defer node.locks.release(key)
			}
			if tc.heldVolume {
				key := nodePublishVolumeLockPrefix + req.GetVolumeId()
				if !node.locks.tryAcquire(key) {
					t.Fatal("could not pre-acquire the volume lock")
				}
				defer node.locks.release(key)
			}

			_, err := node.NodePublishVolume(context.Background(), req)
			wantCode(t, err, tc.wantCode)

			if got := fake.bindCount(); got != tc.wantBind {
				t.Fatalf("BindMount calls = %d, want %d", got, tc.wantBind)
			}
			if got := len(fake.checkCalls); got != tc.wantCheck {
				t.Fatalf("IsMountPoint calls = %d, want %d", got, tc.wantCheck)
			}
			if got := len(fake.otherCalls); got != tc.wantOther {
				t.Fatalf("OtherBindMounts calls = %d, want %d", got, tc.wantOther)
			}
			if tc.check != nil {
				tc.check(t, fake, req.GetStagingTargetPath(), req.GetTargetPath())
			}
		})
	}
}

func TestNodeUnpublishVolume(t *testing.T) {
	tests := []struct {
		name        string
		volumeID    string
		target      func(t *testing.T) string
		cleanupErr  error
		held        bool
		wantCode    codes.Code
		wantCleanup int
	}{
		{
			name:     "missing volume id",
			volumeID: "",
			wantCode: codes.InvalidArgument,
		},
		{
			name:     "missing target path",
			volumeID: testVolumeID,
			target:   func(*testing.T) string { return "" },
			wantCode: codes.InvalidArgument,
		},
		{
			name:     "unclean target path",
			volumeID: testVolumeID,
			target:   func(*testing.T) string { return "/a/../b" },
			wantCode: codes.InvalidArgument,
		},
		{
			name:     "garbage volume id",
			volumeID: "garbage",
			wantCode: codes.NotFound,
		},
		{
			name:        "cleans up the target",
			volumeID:    testVolumeID,
			wantCode:    codes.OK,
			wantCleanup: 1,
		},
		{
			name:        "cleanup failure",
			volumeID:    testVolumeID,
			cleanupErr:  errors.New("device busy"),
			wantCode:    codes.Internal,
			wantCleanup: 1,
		},
		{
			name:        "invalid spec is reported as InvalidArgument",
			volumeID:    testVolumeID,
			cleanupErr:  fmt.Errorf("%w: bad target", mounter.ErrInvalidSpec),
			wantCode:    codes.InvalidArgument,
			wantCleanup: 1,
		},
		{
			name:     "concurrent operation on the same target",
			volumeID: testVolumeID,
			held:     true,
			wantCode: codes.Aborted,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node, fake := newTestNode(t)
			if tc.cleanupErr != nil {
				fake.cleanupErrs = []error{tc.cleanupErr}
			}

			target := "/var/lib/kubelet/pods/uid/volumes/target"
			if tc.target != nil {
				target = tc.target(t)
			}
			if tc.held {
				key := nodePublishLockPrefix + target
				if !node.locks.tryAcquire(key) {
					t.Fatal("could not pre-acquire the lock")
				}
				defer node.locks.release(key)
			}

			_, err := node.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
				VolumeId:   tc.volumeID,
				TargetPath: target,
			})
			wantCode(t, err, tc.wantCode)

			if got := fake.cleanupCount(); got != tc.wantCleanup {
				t.Fatalf("Cleanup calls = %d, want %d", got, tc.wantCleanup)
			}
			if got := fake.unmountCount(); got != 0 {
				t.Fatalf("Unmount calls = %d, want 0: NodeUnpublishVolume must use Cleanup", got)
			}
		})
	}
}

// fakeActionCount returns how many log entries of the given action the fake
// mounter recorded.
func fakeActionCount(fake *mount.FakeMounter, action string) int {
	count := 0
	for _, entry := range fake.GetLog() {
		if entry.Action == action {
			count++
		}
	}
	return count
}

// TestNodeLifecycle drives the whole stage/publish/unstage/unpublish sequence
// over gRPC against a real mount-utils FakeMounter and asserts idempotency and
// the directory effects of every step.
func TestNodeLifecycle(t *testing.T) {
	opts := nodeOptions(t)
	opts.MountLogDir = filepath.Join(t.TempDir(), "logs")

	d, err := New(opts)
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}
	fake := mount.NewFakeMounter(nil)
	d.node.mounter = mounter.New(fake, opts.MountLogDir)

	client := csi.NewNodeClient(bufconnConn(t, d))
	ctx := context.Background()

	staging := filepath.Join(t.TempDir(), "staging")
	target := filepath.Join(t.TempDir(), "target")

	stageReq := &csi.NodeStageVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: staging,
		VolumeCapability:  mountCapability,
	}
	if _, stageErr := client.NodeStageVolume(ctx, stageReq); stageErr != nil {
		t.Fatalf("first NodeStageVolume() = %v, want nil", stageErr)
	}
	if _, stageErr := client.NodeStageVolume(ctx, stageReq); stageErr != nil {
		t.Fatalf("repeated NodeStageVolume() = %v, want nil", stageErr)
	}
	if got := fakeActionCount(fake, mount.FakeActionMount); got != 1 {
		t.Fatalf("glusterfs mounts = %d, want 1 for an idempotent stage", got)
	}

	publishReq := &csi.NodePublishVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: staging,
		TargetPath:        target,
		VolumeCapability:  mountCapability,
	}
	if _, publishErr := client.NodePublishVolume(ctx, publishReq); publishErr != nil {
		t.Fatalf("first NodePublishVolume() = %v, want nil", publishErr)
	}
	if _, publishErr := client.NodePublishVolume(ctx, publishReq); publishErr != nil {
		t.Fatalf("repeated NodePublishVolume() = %v, want nil", publishErr)
	}
	if got := fakeActionCount(fake, mount.FakeActionMount); got != 2 {
		t.Fatalf("mounts = %d, want 2 (one FUSE mount, one bind mount)", got)
	}

	// A second publish with the opposite read-only setting conflicts.
	// A new request, not a copy of publishReq: the generated structs contain a
	// mutex that must not be copied.
	conflicting := &csi.NodePublishVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: staging,
		TargetPath:        target,
		VolumeCapability:  mountCapability,
		Readonly:          true,
	}
	_, err = client.NodePublishVolume(ctx, conflicting)
	wantCode(t, err, codes.AlreadyExists)

	// Unpublish removes the target path but keeps the staging path.
	if _, err := client.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
		VolumeId:   testVolumeID,
		TargetPath: target,
	}); err != nil {
		t.Fatalf("first NodeUnpublishVolume() = %v, want nil", err)
	}
	if _, statErr := os.Stat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Stat(target) = %v, want the target directory to be deleted", statErr)
	}
	if _, err := client.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
		VolumeId:   testVolumeID,
		TargetPath: target,
	}); err != nil {
		t.Fatalf("repeated NodeUnpublishVolume() = %v, want nil", err)
	}

	// Unstage unmounts the staging path and keeps the directory.
	if _, err := client.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: staging,
	}); err != nil {
		t.Fatalf("first NodeUnstageVolume() = %v, want nil", err)
	}
	if info, statErr := os.Stat(staging); statErr != nil || !info.IsDir() {
		t.Fatalf("Stat(staging) = %v, want the staging directory to be kept", statErr)
	}
	if _, err := client.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: staging,
	}); err != nil {
		t.Fatalf("repeated NodeUnstageVolume() = %v, want nil", err)
	}

	log := fake.GetLog()
	if got, want := fakeActionCount(fake, mount.FakeActionUnmount), 2; got != want {
		t.Fatalf("unmounts = %d, want %d (one bind mount, one FUSE mount); log = %+v", got, want, log)
	}
	if len(fake.MountPoints) != 0 {
		t.Fatalf("MountPoints = %+v, want no mount left", fake.MountPoints)
	}
}

// TestNodePublishSecondTarget publishes a staged volume at a second target path
// of the same node against a real mount-utils FakeMounter and checks the CSI
// spec table of NodePublishVolume for a plugin that advertises
// SINGLE_NODE_MULTI_WRITER: the single node modes are refused with
// FailedPrecondition, the multi writer and multi node modes are not.
func TestNodePublishSecondTarget(t *testing.T) {
	tests := []struct {
		mode     csi.VolumeCapability_AccessMode_Mode
		wantCode codes.Code
	}{
		{mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, wantCode: codes.FailedPrecondition},
		{mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY, wantCode: codes.FailedPrecondition},
		{mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER, wantCode: codes.FailedPrecondition},
		{mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER, wantCode: codes.OK},
		{mode: csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY, wantCode: codes.OK},
		{mode: csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER, wantCode: codes.OK},
		{mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER, wantCode: codes.OK},
	}
	for _, tc := range tests {
		t.Run(tc.mode.String(), func(t *testing.T) {
			node, _ := newTestNode(t)
			fake := mount.NewFakeMounter(nil)
			node.mounter = mounter.New(fake, "")
			ctx := context.Background()
			capability := accessModeCapability(tc.mode)

			// Resolved paths: FakeMounter records a bind mount of an unresolved
			// source (macOS /var -> /private/var) under the wrong device and would
			// hide the reference; the Linux mounter matches by major:minor.
			resolvedTemp := func(name string) string {
				dir, err := filepath.EvalSymlinks(t.TempDir())
				if err != nil {
					t.Fatalf("EvalSymlinks: %v", err)
				}
				return filepath.Join(dir, name)
			}
			staging := resolvedTemp("staging")
			first, second := resolvedTemp("first"), resolvedTemp("second")

			if _, err := node.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
				VolumeId:          testVolumeID,
				StagingTargetPath: staging,
				VolumeCapability:  capability,
			}); err != nil {
				t.Fatalf("NodeStageVolume() = %v, want nil", err)
			}
			publish := func(target string) error {
				_, err := node.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
					VolumeId:          testVolumeID,
					StagingTargetPath: staging,
					TargetPath:        target,
					VolumeCapability:  capability,
				})
				return err
			}
			if err := publish(first); err != nil {
				t.Fatalf("NodePublishVolume(first) = %v, want nil", err)
			}

			wantCode(t, publish(second), tc.wantCode)
			wantBinds := 1
			if tc.wantCode == codes.OK {
				wantBinds = 2
			}
			// One FUSE mount plus the bind mounts.
			if got := fakeActionCount(fake, mount.FakeActionMount); got != 1+wantBinds {
				t.Fatalf("mounts = %d, want %d", got, 1+wantBinds)
			}

			// Repeating the first publish stays idempotent in every mode.
			if err := publish(first); err != nil {
				t.Fatalf("repeated NodePublishVolume(first) = %v, want nil", err)
			}

			// Once every other target is unpublished, the volume may be published
			// at a new one. A third target rather than the second: FakeMounter's
			// Unmount drops the options of the remaining entries, which would turn
			// a read only bind mount at the second target into a conflict.
			for _, target := range []string{first, second} {
				if _, err := node.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
					VolumeId:   testVolumeID,
					TargetPath: target,
				}); err != nil {
					t.Fatalf("NodeUnpublishVolume(%q) = %v, want nil", target, err)
				}
			}
			if err := publish(resolvedTemp("third")); err != nil {
				t.Fatalf("NodePublishVolume(third) after unpublishing the others = %v, want nil", err)
			}
		})
	}
}

// TestNodeRPCContextCancellation checks that a cancelled ctx is reported as
// Canceled by every Node RPC instead of being mistaken for a backend failure.
func TestNodeRPCContextCancellation(t *testing.T) {
	tests := []struct {
		name string
		call func(ctx context.Context, node *nodeService, staging, target string) error
	}{
		{
			name: "stage",
			call: func(ctx context.Context, node *nodeService, staging, _ string) error {
				_, err := node.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
					VolumeId: testVolumeID, StagingTargetPath: staging, VolumeCapability: mountCapability,
				})
				return err
			},
		},
		{
			name: "unstage",
			call: func(ctx context.Context, node *nodeService, staging, _ string) error {
				_, err := node.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
					VolumeId: testVolumeID, StagingTargetPath: staging,
				})
				return err
			},
		},
		{
			name: "publish",
			call: func(ctx context.Context, node *nodeService, staging, target string) error {
				_, err := node.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
					VolumeId: testVolumeID, StagingTargetPath: staging, TargetPath: target,
					VolumeCapability: mountCapability,
				})
				return err
			},
		},
		{
			name: "unpublish",
			call: func(ctx context.Context, node *nodeService, _, target string) error {
				_, err := node.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
					VolumeId: testVolumeID, TargetPath: target,
				})
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node, fake := newTestNode(t)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			err := tc.call(ctx, node, tempPathOf(t, "staging"), tempPathOf(t, "target"))
			wantCode(t, err, codes.Canceled)

			if fake.mountCount()+fake.bindCount()+fake.unmountCount()+fake.cleanupCount() != 0 {
				t.Fatal("the mounter was called with a cancelled context")
			}
		})
	}
}

// TestNodeVolumeStageDirectoryFailure checks that an uncreatable staging or log
// directory is an Internal error, not a panic and not a NotFound.
func TestNodeDirectoryFailures(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, node *nodeService, staging *string)
	}{
		{
			name: "staging directory cannot be created",
			mutate: func(t *testing.T, _ *nodeService, staging *string) {
				t.Helper()
				// A regular file where a parent directory is expected makes
				// MkdirAll fail with ENOTDIR.
				file := filepath.Join(t.TempDir(), "file")
				if err := os.WriteFile(file, nil, 0o600); err != nil {
					t.Fatalf("WriteFile() = %v, want nil", err)
				}
				*staging = filepath.Join(file, "staging")
			},
		},
		{
			name: "mount log directory cannot be created",
			mutate: func(t *testing.T, node *nodeService, _ *string) {
				t.Helper()
				file := filepath.Join(t.TempDir(), "file")
				if err := os.WriteFile(file, nil, 0o600); err != nil {
					t.Fatalf("WriteFile() = %v, want nil", err)
				}
				node.logDir = filepath.Join(file, "logs")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node, fake := newTestNode(t)
			staging := tempPathOf(t, "staging")
			tc.mutate(t, node, &staging)

			_, err := node.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
				VolumeId:          testVolumeID,
				StagingTargetPath: staging,
				VolumeCapability:  mountCapability,
			})
			wantCode(t, err, codes.Internal)
			if got := fake.mountCount(); got != 0 {
				t.Fatalf("MountGlusterfs calls = %d, want 0", got)
			}
		})
	}
}

// TestNodePublishTargetDirectoryFailure checks that a target path that cannot be
// created is reported as Internal, because the spec makes the SP responsible for
// creating it.
func TestNodePublishTargetDirectoryFailure(t *testing.T) {
	node, fake := newTestNode(t)
	fake.isMountPoint = true

	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}

	_, err := node.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: tempPathOf(t, "staging"),
		TargetPath:        filepath.Join(file, "target"),
		VolumeCapability:  mountCapability,
	})
	wantCode(t, err, codes.Internal)
	if got := fake.bindCount(); got != 0 {
		t.Fatalf("BindMount calls = %d, want 0", got)
	}
}

// TestNodeServiceClusterWithoutConfig checks the nil store guard of the cluster
// lookup. [New] rejects node mode without a configuration, so the state is built
// by hand here; the guard keeps a future caller from dereferencing nil.
func TestNodeServiceClusterWithoutConfig(t *testing.T) {
	node := &nodeService{driver: &Driver{}, locks: newInflight()}
	if _, ok := node.cluster("c1"); ok {
		t.Fatal("cluster() = true, want false without a configuration store")
	}
}

func TestNodeGetCapabilities(t *testing.T) {
	opts := nodeOptions(t)
	d, err := New(opts)
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}

	client := csi.NewNodeClient(bufconnConn(t, d))
	resp, err := client.NodeGetCapabilities(context.Background(), &csi.NodeGetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("NodeGetCapabilities() = %v, want nil", err)
	}

	// The order is part of the response: it is the order of the enum values.
	want := []csi.NodeServiceCapability_RPC_Type{
		csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
		csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
		csi.NodeServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
		csi.NodeServiceCapability_RPC_GET_VOLUME_HEALTH,
	}
	caps := resp.GetCapabilities()
	if len(caps) != len(want) {
		t.Fatalf("capabilities = %v, want %d entries", caps, len(want))
	}
	for i, w := range want {
		if got := caps[i].GetRpc().GetType(); got != w {
			t.Fatalf("capability[%d] = %v, want %v", i, got, w)
		}
	}

	// VOLUME_CONDITION was removed in CSI v1.13 and must never be advertised by
	// its old enum value; the health reporting uses GET_VOLUME_HEALTH instead
	// (D-18). The value 4 is not in the enum of this protocol version any more.
	if got := int32(csi.NodeServiceCapability_RPC_GET_VOLUME_HEALTH); got != 7 {
		t.Fatalf("GET_VOLUME_HEALTH = %d, want the post fix enum value 7", got)
	}
	for _, c := range caps {
		if int32(c.GetRpc().GetType()) == 4 {
			t.Fatal("capability 4 (the removed VOLUME_CONDITION) must not be advertised")
		}
	}

	// NodeGetVolumeStats is advertised, so it must be served, not Unimplemented.
	_, err = client.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   testVolumeID,
		VolumePath: "some/path",
	})
	if got := status.Code(err); got == codes.Unimplemented {
		t.Fatalf("NodeGetVolumeStats() = %v, want it to be implemented after advertising the capability", err)
	}

	// NodeExpandVolume is not advertised, so it must answer Unimplemented.
	_, err = client.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId:   testVolumeID,
		VolumePath: "/var/lib/kubelet/pods/uid/volumes/target",
	})
	wantCode(t, err, codes.Unimplemented)
}

func TestNodeGetInfo(t *testing.T) {
	opts := nodeOptions(t)
	opts.NodeID = "node-7"

	d, err := New(opts)
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}

	client := csi.NewNodeClient(bufconnConn(t, d))
	resp, err := client.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	if err != nil {
		t.Fatalf("NodeGetInfo() = %v, want nil", err)
	}
	if resp.GetNodeId() != "node-7" {
		t.Fatalf("NodeId = %q, want %q", resp.GetNodeId(), "node-7")
	}
	if resp.GetMaxVolumesPerNode() != 0 {
		t.Fatalf("MaxVolumesPerNode = %d, want 0 (unlimited)", resp.GetMaxVolumesPerNode())
	}
	if resp.GetAccessibleTopology() != nil {
		t.Fatalf("AccessibleTopology = %v, want nil", resp.GetAccessibleTopology())
	}
}

// TestNodeServiceNotRegisteredInControllerMode checks that controller mode serves
// no Node service at all.
func TestNodeServiceNotRegisteredInControllerMode(t *testing.T) {
	d, err := New(baseOptions())
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}

	client := csi.NewNodeClient(bufconnConn(t, d))
	_, err = client.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	wantCode(t, err, codes.Unimplemented)
}

// TestNewGRPCServerNodeModeMismatch checks the guard that keeps a driver from
// registering the node service in a mode that does not run it.
func TestNewGRPCServerNodeModeMismatch(t *testing.T) {
	d, err := New(baseOptions())
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}
	d.nodeRegistered = true
	d.node = &nodeService{driver: d, locks: newInflight()}

	if _, srvErr := d.newGRPCServer(); srvErr == nil {
		t.Fatal("newGRPCServer() = nil error, want an error for the node service in controller mode")
	}

	d.mode = ModeNode
	srv, err := d.newGRPCServer()
	if err != nil {
		t.Fatalf("newGRPCServer() = %v, want nil in node mode", err)
	}
	if srv == nil {
		t.Fatal("newGRPCServer() = nil server, want a server")
	}
}

// TestDecodeVolumeID checks the error mapping and the length handling of
// oversized ids.
func TestDecodeVolumeID(t *testing.T) {
	// An id longer than volumeid.MaxLength must not be echoed at all.
	oversizedID := "gfs:v1:static:c1:" + strings.Repeat("x", volumeid.MaxLength)

	tests := []struct {
		name     string
		volumeID string
		wantCode codes.Code
		wantMsg  string
	}{
		{name: "valid", volumeID: testVolumeID, wantCode: codes.OK},
		{name: "garbage", volumeID: "garbage", wantCode: codes.NotFound, wantMsg: `volume "garbage"`},
		{
			name:     "oversized id is reported by length only",
			volumeID: oversizedID,
			wantCode: codes.NotFound,
			wantMsg:  fmt.Sprintf("volume of length %d", len(oversizedID)),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id, err := decodeVolumeID(tc.volumeID)
			wantCode(t, err, tc.wantCode)
			if tc.wantCode != codes.OK {
				if !strings.Contains(status.Convert(err).Message(), tc.wantMsg) {
					t.Fatalf("message = %q, want it to contain %q", status.Convert(err).Message(), tc.wantMsg)
				}
				return
			}
			want, decErr := volumeid.Decode(tc.volumeID)
			if decErr != nil {
				t.Fatalf("Decode(%q) = %v, want nil", tc.volumeID, decErr)
			}
			if id != want {
				t.Fatalf("decodeVolumeID() = %+v, want %+v", id, want)
			}
		})
	}
}

// TestMountErrorToStatus checks the whole error mapping table.
func TestMountErrorToStatus(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode codes.Code
	}{
		{name: "nil", err: nil, wantCode: codes.OK},
		{name: "cancelled", err: context.Canceled, wantCode: codes.Canceled},
		{name: "deadline", err: context.DeadlineExceeded, wantCode: codes.DeadlineExceeded},
		{name: "invalid spec", err: fmt.Errorf("%w: x", mounter.ErrInvalidSpec), wantCode: codes.InvalidArgument},
		{name: "invalid option", err: fmt.Errorf("%w: x", mounter.ErrInvalidOption), wantCode: codes.InvalidArgument},
		{name: "mount conflict", err: fmt.Errorf("%w: x", mounter.ErrMountConflict), wantCode: codes.AlreadyExists},
		{name: "corrupted mount", err: fmt.Errorf("%w: x", mounter.ErrCorruptedMount), wantCode: codes.Internal},
		{name: "other", err: errors.New("boom"), wantCode: codes.Internal},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := mountErrorToStatus("stage volume", tc.err)
			wantCode(t, err, tc.wantCode)
			if tc.wantCode == codes.Internal && tc.err != nil {
				if !strings.Contains(err.Error(), "stage volume") {
					t.Fatalf("message = %q, want it to name the operation", err)
				}
			}
		})
	}
}

// TestNodeCorruptedMountPointReachesMounter checks that a corrupted mount point
// at the staging or target path is handed to the mounter, which recovers it,
// instead of failing in the directory creation in front of it. On a real node
// stat of such a path fails with ENOTCONN; os.MkdirAll would report that as an
// error and the recovery would never run.
func TestNodeCorruptedMountPointReachesMounter(t *testing.T) {
	corrupted := func(name string) (os.FileInfo, error) {
		return nil, &os.PathError{Op: "stat", Path: name, Err: syscall.ENOTCONN}
	}
	corruptedErr := fmt.Errorf("%w: ENOTCONN", mounter.ErrCorruptedMount)

	tests := []struct {
		name string
		call func(t *testing.T, node *nodeService, path string) error
		prep func(fake *fakeMounter)
		// calls returns the number of mount attempts of the RPC under test.
		calls func(fake *fakeMounter) int
	}{
		{
			name: "stage",
			prep: func(fake *fakeMounter) { fake.mountErrs = []error{corruptedErr} },
			call: func(_ *testing.T, node *nodeService, path string) error {
				_, err := node.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
					VolumeId:          testVolumeID,
					StagingTargetPath: path,
					VolumeCapability:  mountCapability,
				})
				return err
			},
			calls: (*fakeMounter).mountCount,
		},
		{
			name: "publish",
			prep: func(fake *fakeMounter) {
				fake.isMountPoint = true
				fake.bindErrs = []error{corruptedErr}
			},
			call: func(t *testing.T, node *nodeService, path string) error {
				_, err := node.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
					VolumeId:          testVolumeID,
					StagingTargetPath: tempPathOf(t, "staging"),
					TargetPath:        path,
					VolumeCapability:  mountCapability,
				})
				return err
			},
			calls: (*fakeMounter).bindCount,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node, fake := newTestNode(t)
			// A path that does not exist on disk: if the node service still tried
			// to create it, the directory would appear.
			path := filepath.Join(t.TempDir(), "corrupted")
			node.stat = func(name string) (os.FileInfo, error) {
				if name == path {
					return corrupted(name)
				}
				return os.Stat(name)
			}
			tc.prep(fake)

			wantCode(t, tc.call(t, node, path), codes.OK)
			if got := tc.calls(fake); got != 2 {
				t.Errorf("mount attempts = %d, want 2 (corrupted, then recovered)", got)
			}
			if got := fake.unmountCount(); got != 1 {
				t.Errorf("Unmount calls = %d, want 1", got)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("os.Stat(%q) = %v, want the directory not to be created", path, err)
			}
		})
	}
}
