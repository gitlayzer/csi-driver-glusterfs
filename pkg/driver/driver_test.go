package driver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/test/bufconn"

	"github.com/gitlayzer/csi-driver-glusterfs/pkg/mounter"
)

// baseOptions returns a valid option set in controller mode: the mode with the
// fewest requirements, so each test can vary exactly one field. It includes the
// cluster configuration, which every mode requires.
func baseOptions() Options {
	return Options{
		DriverName: "glusterfs.csi.io",
		Endpoint:   "unix:///tmp/csi.sock",
		Mode:       ModeController,
		Version:    "v0.1.0",
	}
}

// controllerOptions returns a valid option set in controller mode, including
// the cluster configuration and the mount directory of the parent volumes that
// controller mode requires (D-24).
func controllerOptions(t *testing.T) Options {
	t.Helper()

	opts := baseOptions()
	opts.Config = testConfigStore(t)
	opts.ControllerMountDir = t.TempDir()
	return opts
}

// nodeOptions returns a valid option set in node mode, including the cluster
// configuration that node mode requires and the default mount log directory.
func nodeOptions(t *testing.T) Options {
	t.Helper()

	opts := baseOptions()
	opts.Mode, opts.NodeID = ModeNode, "node-1"
	opts.Config = testConfigStore(t)
	opts.MountLogDir = "/var/log/glusterfs/csi"
	return opts
}

func TestNew(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(o *Options)
		wantErr string
	}{
		{
			name:   "controller mode",
			mutate: func(_ *Options) {},
		},
		{
			name:   "node mode with nodeid",
			mutate: func(o *Options) { o.Mode, o.NodeID = ModeNode, "node-1" },
		},
		{
			name:   "all mode with nodeid",
			mutate: func(o *Options) { o.Mode, o.NodeID = ModeAll, "node-1" },
		},
		{
			name:   "controller mode without nodeid",
			mutate: func(o *Options) { o.NodeID = "" },
		},
		{
			name:   "tcp endpoint",
			mutate: func(o *Options) { o.Endpoint = "tcp://127.0.0.1:0" },
		},
		{
			name:   "empty version falls back to unknown",
			mutate: func(o *Options) { o.Version = "" },
		},
		{
			name:   "blank version falls back to unknown",
			mutate: func(o *Options) { o.Version = "   " },
		},

		{
			name:    "node mode without a configuration",
			mutate:  func(o *Options) { o.Mode, o.NodeID, o.Config = ModeNode, "node-1", nil },
			wantErr: "cluster configuration is required",
		},
		{
			name:    "all mode without a configuration",
			mutate:  func(o *Options) { o.Mode, o.NodeID, o.Config = ModeAll, "node-1", nil },
			wantErr: "cluster configuration is required",
		},
		{
			name:    "controller mode without a configuration",
			mutate:  func(o *Options) { o.Config = nil },
			wantErr: "cluster configuration is required",
		},
		{
			name:    "relative mount log directory",
			mutate:  func(o *Options) { o.MountLogDir = "var/log" },
			wantErr: "invalid mount log directory",
		},
		{
			name:    "unclean mount log directory",
			mutate:  func(o *Options) { o.MountLogDir = "/a/../b" },
			wantErr: "invalid mount log directory",
		},
		{
			name:    "mount log directory with a trailing slash",
			mutate:  func(o *Options) { o.MountLogDir = "/var/log/" },
			wantErr: "invalid mount log directory",
		},
		{
			name:   "empty mount log directory disables the log file",
			mutate: func(o *Options) { o.MountLogDir = "" },
		},
		{
			name:    "node mode without nodeid",
			mutate:  func(o *Options) { o.Mode = ModeNode },
			wantErr: "node id is required",
		},
		{
			name:    "all mode with blank nodeid",
			mutate:  func(o *Options) { o.Mode, o.NodeID = ModeAll, "  " },
			wantErr: "node id is required",
		},
		{
			name:    "invalid mode",
			mutate:  func(o *Options) { o.Mode = "bogus" },
			wantErr: "invalid mode",
		},
		{
			name:    "empty mode",
			mutate:  func(o *Options) { o.Mode = "" },
			wantErr: "invalid mode",
		},
		{
			name:    "invalid driver name",
			mutate:  func(o *Options) { o.DriverName = "a_b" },
			wantErr: "invalid driver name",
		},
		{
			name:    "empty driver name",
			mutate:  func(o *Options) { o.DriverName = "" },
			wantErr: "invalid driver name",
		},
		{
			name:    "invalid endpoint",
			mutate:  func(o *Options) { o.Endpoint = "http://x" },
			wantErr: "endpoint",
		},
		{
			name:    "empty endpoint",
			mutate:  func(o *Options) { o.Endpoint = "" },
			wantErr: "endpoint",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Every mode requires a configuration and controller mode a mount
			// directory, so each case starts with both; the cases that check the
			// missing ones clear them.
			opts := controllerOptions(t)
			tc.mutate(&opts)

			d, err := New(opts)

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("New() = %+v, nil, want error containing %q", d, tc.wantErr)
				}
				if d != nil {
					t.Fatalf("New() returned a driver together with the error %v", err)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("New() = %q, want error containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("New() = %v, want nil", err)
			}
			if d == nil {
				t.Fatal("New() = nil, want a driver")
			}
			if d.driverName != opts.DriverName {
				t.Fatalf("driverName = %q, want %q", d.driverName, opts.DriverName)
			}
			if d.mode != opts.Mode {
				t.Fatalf("mode = %q, want %q", d.mode, opts.Mode)
			}
			if want := opts.Mode == ModeController || opts.Mode == ModeAll; d.controllerRegistered != want {
				t.Fatalf("controllerRegistered = %t, want %t in %q mode", d.controllerRegistered, want, opts.Mode)
			}
			if want := opts.Mode == ModeNode || opts.Mode == ModeAll; d.nodeRegistered != want {
				t.Fatalf("nodeRegistered = %t, want %t in %q mode", d.nodeRegistered, want, opts.Mode)
			}
			if opts.Mode == ModeNode || opts.Mode == ModeAll {
				if d.node == nil {
					t.Fatal("node = nil, want a node service in node and all mode")
				}
				if d.node.logDir != opts.MountLogDir {
					t.Fatalf("node.logDir = %q, want %q", d.node.logDir, opts.MountLogDir)
				}
			} else if d.node != nil {
				t.Fatal("node != nil, want no node service in node mode")
			}
			if opts.Mode == ModeController || opts.Mode == ModeAll {
				if d.controller == nil {
					t.Fatal("controller = nil, want a controller service in controller and all mode")
				}
				if d.controller.driver != d {
					t.Fatal("controller.driver = not the driver, want the driver")
				}
				if d.controller.locks == nil {
					t.Fatal("controller.locks = nil, want an in-flight lock table")
				}
				if d.controller.parents == nil {
					t.Fatal("controller.parents = nil, want a parent volume cache")
				}
				if d.controller.parents.dir != opts.ControllerMountDir {
					t.Fatalf("controller.parents.dir = %q, want %q",
						d.controller.parents.dir, opts.ControllerMountDir)
				}
				if d.controller.now == nil {
					t.Fatal("controller.now = nil, want the real clock")
				}
			} else if d.controller != nil {
				t.Fatal("controller != nil, want no controller service in node mode")
			}
		})
	}
}

// TestNewChecks covers the readiness check registration per mode.
func TestNewChecks(t *testing.T) {
	tests := []struct {
		mode      Mode
		wantNames []string
	}{
		{mode: ModeController, wantNames: nil},
		{mode: ModeNode, wantNames: []string{fuseDevice, mountHelper}},
		{mode: ModeAll, wantNames: []string{fuseDevice, mountHelper}},
	}

	for _, tc := range tests {
		t.Run(string(tc.mode), func(t *testing.T) {
			opts := nodeOptions(t)
			opts.Mode = tc.mode
			opts.ControllerMountDir = t.TempDir()

			d, err := New(opts)
			if err != nil {
				t.Fatalf("New() = %v, want nil", err)
			}

			var names []string
			for _, c := range d.checks {
				names = append(names, c.name)
			}
			if len(names) != len(tc.wantNames) {
				t.Fatalf("checks = %v, want %v", names, tc.wantNames)
			}
			for i := range names {
				if names[i] != tc.wantNames[i] {
					t.Fatalf("check[%d] = %q, want %q", i, names[i], tc.wantNames[i])
				}
			}
		})
	}
}

// TestNewVersionFallback checks the vendor_version fallback through the public
// Identity API, which is what csi-sanity inspects.
func TestNewVersionFallback(t *testing.T) {
	tests := []struct {
		name        string
		version     string
		wantVersion string
	}{
		{name: "configured version", version: "v1.2.3", wantVersion: "v1.2.3"},
		{name: "empty version", version: "", wantVersion: "unknown"},
		{name: "blank version", version: "  ", wantVersion: "unknown"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := controllerOptions(t)
			opts.Version = tc.version

			d, err := New(opts)
			if err != nil {
				t.Fatalf("New() = %v, want nil", err)
			}

			resp, err := d.GetPluginInfo(context.Background(), &csi.GetPluginInfoRequest{})
			if err != nil {
				t.Fatalf("GetPluginInfo() = %v, want nil", err)
			}
			if resp.GetName() != opts.DriverName {
				t.Fatalf("GetPluginInfo().Name = %q, want %q", resp.GetName(), opts.DriverName)
			}
			if resp.GetVendorVersion() != tc.wantVersion {
				t.Fatalf("GetPluginInfo().VendorVersion = %q, want %q", resp.GetVendorVersion(), tc.wantVersion)
			}
			if resp.GetManifest() != nil {
				t.Fatalf("GetPluginInfo().Manifest = %v, want nil", resp.GetManifest())
			}
		})
	}
}

// TestNewGRPCServerControllerModeMismatch checks the guard that keeps a driver
// from registering a service in a mode that does not run it, and that a
// registered service is always built.
func TestNewGRPCServerControllerModeMismatch(t *testing.T) {
	d, err := New(nodeOptions(t))
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}

	// A flag without a service would register a typed nil that answers every
	// RPC with a nil response instead of an error.
	d.controllerRegistered = true
	if _, srvErr := d.newGRPCServer(); srvErr == nil {
		t.Fatal("newGRPCServer() = nil error, want an error for controller service in node mode")
	}
	d.controller = &controllerService{driver: d, locks: newInflight()}
	if _, srvErr := d.newGRPCServer(); srvErr == nil {
		t.Fatal("newGRPCServer() = nil error, want an error for controller service in node mode")
	}

	d.mode = ModeController
	// Clearing the flags is what a driver built in controller mode looks like;
	// the mode alone does not unregister a service.
	d.nodeRegistered = false
	d.node = nil
	srv, err := d.newGRPCServer()
	if err != nil {
		t.Fatalf("newGRPCServer() = %v, want nil in controller mode", err)
	}
	if srv == nil {
		t.Fatal("newGRPCServer() = nil server, want a server")
	}
}

// TestNewGRPCServerNodeServiceNotBuilt checks the guard for a node registration
// flag without a node service.
func TestNewGRPCServerNodeServiceNotBuilt(t *testing.T) {
	d, err := New(nodeOptions(t))
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}
	d.node = nil

	if _, srvErr := d.newGRPCServer(); srvErr == nil {
		t.Fatal("newGRPCServer() = nil error, want an error for a registered node service that is not built")
	}
}

// TestNewStateDirAndRecoveryInterval checks the two options of the mount recovery
// (D-20): the state directory and the interval.
func TestNewStateDirAndRecoveryInterval(t *testing.T) {
	tests := []struct {
		name     string
		mode     Mode
		stateDir string
		interval time.Duration
		wantErr  string
		// wantRecords is true when the node service must have a record store.
		wantRecords bool
	}{
		{
			name: "state directory in node mode",
			mode: ModeNode, stateDir: "/var/lib/kubelet/plugins/glusterfs.csi.io/staged",
			interval: 60 * time.Second, wantRecords: true,
		},
		{
			name: "empty state directory disables the records",
			mode: ModeNode, stateDir: "", interval: 60 * time.Second, wantRecords: false,
		},
		{
			name: "zero interval is allowed",
			mode: ModeNode, stateDir: "/staged", interval: 0, wantRecords: true,
		},
		{
			name:     "relative state directory",
			mode:     ModeNode,
			stateDir: "staged", interval: time.Second,
			wantErr: "invalid state directory",
		},
		{
			name:     "unclean state directory",
			mode:     ModeNode,
			stateDir: "/a/../b", interval: time.Second,
			wantErr: "invalid state directory",
		},
		{
			name:     "state directory with a trailing slash",
			mode:     ModeNode,
			stateDir: "/staged/", interval: time.Second,
			wantErr: "invalid state directory",
		},
		{
			name:     "negative recovery interval",
			mode:     ModeNode,
			stateDir: "/staged", interval: -time.Second,
			wantErr: "invalid recovery interval",
		},
		{
			name: "state directory in controller mode keeps no records",
			mode: ModeController, stateDir: "/staged", interval: time.Second,
			wantRecords: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := nodeOptions(t)
			opts.Mode = tc.mode
			opts.ControllerMountDir = t.TempDir()
			opts.StateDir = tc.stateDir
			opts.RecoveryInterval = tc.interval

			d, err := New(opts)

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("New() = nil, want an error containing %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("New() = %q, want an error containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("New() = %v, want nil", err)
			}
			if d.recoveryInterval != tc.interval {
				t.Fatalf("recoveryInterval = %s, want %s", d.recoveryInterval, tc.interval)
			}
			if d.node == nil {
				if tc.wantRecords {
					t.Fatal("node = nil, want a node service")
				}
				return
			}
			switch {
			case tc.wantRecords && d.node.records == nil:
				t.Fatal("records = nil, want a record store")
			case !tc.wantRecords && d.node.records != nil:
				t.Fatalf("records = %+v, want none", d.node.records)
			case tc.wantRecords && d.node.records.dir != tc.stateDir:
				t.Fatalf("records.dir = %q, want %q", d.node.records.dir, tc.stateDir)
			}
		})
	}
}

// recoveryDriver returns a node driver whose record store holds one record and
// whose fake mounter reports the staging mount as corrupted, so that every pass
// does a visible amount of work.
func recoveryDriver(t *testing.T) (*Driver, *fakeMounter, *recordStore) {
	t.Helper()

	opts := nodeOptions(t)
	opts.MountLogDir = filepath.Join(t.TempDir(), "logs")
	opts.StateDir = filepath.Join(t.TempDir(), "staged")
	opts.RecoveryInterval = 5 * time.Millisecond

	d, err := New(opts)
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}
	fake := &fakeMounter{mountState: mounter.MountStateMounted, trackState: true}
	fake.mountStateOf = map[string]mounter.MountState{testStaging: mounter.MountStateCorrupted}
	d.node.mounter = fake

	rec := stageRecord{
		Version:           stageRecordVersion,
		VolumeID:          testVolumeID,
		StagingTargetPath: testStaging,
	}
	if err := d.node.records.save(rec); err != nil {
		t.Fatalf("save() = %v, want nil", err)
	}
	return d, fake, d.node.records
}

// TestStartRecovery checks the lifecycle of the recovery goroutine: it is started
// only for a node service with records, the interval is honoured, and the stop
// function waits for it and may be called twice.
func TestStartRecovery(t *testing.T) {
	t.Run("controller mode starts no goroutine", func(t *testing.T) {
		d, err := New(controllerOptions(t))
		if err != nil {
			t.Fatalf("New() = %v, want nil", err)
		}

		done := make(chan struct{})
		go func() {
			defer close(done)
			d.startRecovery(context.Background())()
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("startRecovery() stop function did not return")
		}
	})

	t.Run("an empty state directory starts no goroutine", func(t *testing.T) {
		d, _, _ := recoveryDriver(t)
		d.node.records = nil

		done := make(chan struct{})
		go func() {
			defer close(done)
			d.startRecovery(context.Background())()
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("startRecovery() stop function did not return")
		}
	})

	t.Run("stop waits for the running pass and is idempotent", func(t *testing.T) {
		d, fake, _ := recoveryDriver(t)
		stop := d.startRecovery(context.Background())

		if !waitFor(t, time.Second, func() bool { return fake.mountStateCount() >= 2 }) {
			t.Fatal("the recovery did not run repeatedly")
		}
		stop()

		after := fake.mountStateCount()
		time.Sleep(30 * time.Millisecond)
		if got := fake.mountStateCount(); got != after {
			t.Fatalf("the recovery ran again after stop(): %d -> %d calls", after, got)
		}
		// The second call must not block or panic.
		stop()
	})

	t.Run("a cancelled context stops the loop", func(t *testing.T) {
		d, _, _ := recoveryDriver(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		stop := d.startRecovery(ctx)

		done := make(chan struct{})
		go func() {
			defer close(done)
			stop()
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("stop() did not return for a cancelled context")
		}
	})
}

// TestServeStopsRecovery checks that Serve cancels the recovery and waits for it
// on the shutdown path: after Serve returned, no pass may run any more.
func TestServeStopsRecovery(t *testing.T) {
	d, fake, _ := recoveryDriver(t)

	lis := bufconn.Listen(bufSize)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	served := make(chan error, 1)
	go func() { served <- d.Serve(ctx, lis) }()

	if !waitFor(t, time.Second, func() bool { return fake.mountCount() >= 1 }) {
		t.Fatal("the recovery did not run while the server was serving")
	}
	cancel()

	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve() = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve() did not return within 5s of cancellation")
	}

	after := fake.mountCount()
	time.Sleep(30 * time.Millisecond)
	if got := fake.mountCount(); got != after {
		t.Fatalf("the recovery ran after Serve returned: %d -> %d mounts", after, got)
	}
	if err := lis.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
}

// blockingMounter is a fakeMounter whose MountGlusterfs blocks until release is
// closed, so that a test can hold a recovery pass in the middle of a mount.
type blockingMounter struct {
	*fakeMounter
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

// MountGlusterfs signals entered once and waits for release.
func (b *blockingMounter) MountGlusterfs(ctx context.Context, spec mounter.Spec, target string) error {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return b.fakeMounter.MountGlusterfs(ctx, spec, target)
}

// TestServeWaitsForRecovery checks that Serve does not return while a recovery
// pass is still running: a goroutine that outlives Serve would keep mounting
// after the plugin reported that it stopped.
func TestServeWaitsForRecovery(t *testing.T) {
	d, fake, _ := recoveryDriver(t)
	blocking := &blockingMounter{fakeMounter: fake, entered: make(chan struct{}), release: make(chan struct{})}
	d.node.mounter = blocking

	lis := bufconn.Listen(bufSize)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	served := make(chan error, 1)
	go func() { served <- d.Serve(ctx, lis) }()

	select {
	case <-blocking.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the recovery did not start mounting")
	}
	cancel()

	select {
	case err := <-served:
		t.Fatalf("Serve() = %v while a recovery pass was still mounting, want it to wait", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(blocking.release)
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve() = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve() did not return after the recovery pass finished")
	}
}

// TestServeWithoutRecords checks that Serve works for a driver without a
// recovery, i.e. that the shutdown path does not depend on one.
func TestServeWithoutRecords(t *testing.T) {
	opts := nodeOptions(t)
	d, err := New(opts)
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}
	fake := &fakeMounter{mountState: mounter.MountStateMounted}
	d.node.mounter = fake

	lis := bufconn.Listen(bufSize)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	served := make(chan error, 1)
	go func() { served <- d.Serve(ctx, lis) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case serveErr := <-served:
		if serveErr != nil {
			t.Fatalf("Serve() = %v, want nil", serveErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve() did not return within 5s of cancellation")
	}
	if got := fake.callLog(); len(got) != 0 {
		t.Fatalf("mounter calls = %v, want none without a record store", got)
	}
	if err := lis.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
}

// TestNewNodeServiceWiring checks that New builds a node service with the
// production mounter and the record store of the options.
func TestNewNodeServiceWiring(t *testing.T) {
	opts := nodeOptions(t)
	opts.StateDir = filepath.Join(t.TempDir(), "staged")

	d, err := New(opts)
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}
	if d.node.mounter == nil {
		t.Fatal("node.mounter = nil, want the production mounter")
	}
	if d.node.logDir != opts.MountLogDir {
		t.Fatalf("logDir = %q, want %q", d.node.logDir, opts.MountLogDir)
	}
	if d.node.records == nil || d.node.records.dir != opts.StateDir {
		t.Fatalf("records = %+v, want a store in %q", d.node.records, opts.StateDir)
	}
	// The production mounter accepts the real mount table path, which is what the
	// recovery of a real node needs.
	if _, ok := d.node.mounter.(interface{ Detach(string) error }); !ok {
		t.Fatal("the mounter has no Detach method")
	}
}

// TestNewControllerMountDir checks the option the controller mounts the parent
// volumes below (D-24): required in controller and all mode, absolute and clean,
// and ignored in node mode.
func TestNewControllerMountDir(t *testing.T) {
	tests := []struct {
		name    string
		mode    Mode
		dir     string
		wantErr string
	}{
		{name: "controller mode with a clean absolute path", mode: ModeController, dir: "/var/lib/glusterfs-csi/controller"},
		{name: "all mode with a clean absolute path", mode: ModeAll, dir: "/var/lib/glusterfs-csi/controller"},
		{
			name: "controller mode without a mount directory", mode: ModeController, dir: "",
			wantErr: "controller mount directory is required",
		},
		{
			name: "all mode without a mount directory", mode: ModeAll, dir: "",
			wantErr: "controller mount directory is required",
		},
		{
			name: "relative mount directory", mode: ModeController, dir: "controller",
			wantErr: "must be an absolute path",
		},
		{
			name: "unclean mount directory", mode: ModeController, dir: "/a/../b",
			wantErr: "must be a clean path",
		},
		{
			name: "mount directory with a trailing slash", mode: ModeController, dir: "/controller/",
			wantErr: "must be a clean path",
		},
		{
			// Node mode never mounts a parent volume, so an empty value is the
			// normal case there.
			name: "node mode without a mount directory", mode: ModeNode, dir: "",
		},
		{
			// A value that is set is still checked, so that a typo in the
			// Deployment does not stay hidden until the mode changes.
			name: "node mode with an unclean mount directory", mode: ModeNode, dir: "/a/../b",
			wantErr: "must be a clean path",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := nodeOptions(t)
			opts.Mode = tc.mode
			opts.ControllerMountDir = tc.dir

			d, err := New(opts)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("New() = %+v, nil, want an error containing %q", d, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("New() = %q, want an error containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("New() = %v, want nil", err)
			}
			if tc.mode.runsController() {
				if got := d.controller.parents.dir; got != tc.dir {
					t.Fatalf("parents.dir = %q, want %q", got, tc.dir)
				}
				return
			}
			if d.controller != nil {
				t.Fatal("controller != nil in node mode, want no parent volume mounts")
			}
		})
	}
}

// TestServeClosesParentMounts checks that Serve unmounts the parent volumes the
// controller mounted once the gRPC server stopped: the plugin must not leave a
// FUSE mount behind on every restart of the Deployment.
func TestServeClosesParentMounts(t *testing.T) {
	d := mustControllerDriver(t)
	fake := &fakeMounter{mountState: mounter.MountStateNotMounted, trackState: true}
	fake.onCreate = func(target string) error { return os.MkdirAll(target, 0o750) }
	d.controller.parents = newParentMounts(fake, t.TempDir())

	lis := bufconn.Listen(bufSize)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	served := make(chan error, 1)
	go func() { served <- d.Serve(ctx, lis) }()

	if !waitFor(t, 5*time.Second, func() bool {
		_, err := d.controller.parents.root(context.Background(), testCluster, "vol")
		return err == nil
	}) {
		t.Fatal("could not mount a parent volume before the server was stopped")
	}
	cancel()

	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve() = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve() did not return within 5s of cancellation")
	}

	if got := fake.unmountCount(); got != 1 {
		t.Fatalf("Unmount calls = %d (%v), want 1", got, fake.unmountCalls)
	}
	if want := d.controller.parents.mountPath("c1", "vol"); fake.unmountCalls[0] != want {
		t.Fatalf("Unmount target = %q, want %q", fake.unmountCalls[0], want)
	}
	if err := lis.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
}

// TestCloseParentMountsWithoutController checks the shutdown helper of the modes
// that run no controller service: it must be a no-op instead of dereferencing a
// missing service.
func TestCloseParentMountsWithoutController(t *testing.T) {
	d, err := New(nodeOptions(t))
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}
	d.closeParentMounts()

	if (&Driver{}).controller != nil {
		t.Fatal("a hand-built Driver has a controller service")
	}
	(&Driver{}).closeParentMounts()
}
