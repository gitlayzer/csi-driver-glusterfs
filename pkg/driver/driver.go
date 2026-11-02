// Package driver implements the CSI plugin: the gRPC server, the Identity,
// Controller and Node services and the process lifecycle of the glusterfs-csi
// binary.
//
// The package is deliberately thin. The services register themselves on the
// same [Driver] and are reached through the same gRPC server, so the plugin
// capability list is derived from what is actually registered instead of being
// hard-coded (docs/architecture.md §5.4).
package driver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"k8s.io/klog/v2"

	"github.com/gitlayzer/csi-driver-glusterfs/pkg/config"
)

// Mode is the role the binary runs as (--mode).
type Mode string

const (
	// ModeController runs only the controller service.
	ModeController Mode = "controller"
	// ModeNode runs only the node service.
	ModeNode Mode = "node"
	// ModeAll runs both services in a single process.
	ModeAll Mode = "all"
)

// maxDriverNameLen is the CSI spec limit for GetPluginInfo.name.
const maxDriverNameLen = 63

// unknownVersion replaces an empty Options.Version so that GetPluginInfo never
// returns an empty vendor_version, which csi-sanity rejects.
const unknownVersion = "unknown"

// ParseMode parses the --mode value. The comparison is case sensitive.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case ModeController, ModeNode, ModeAll:
		return Mode(s), nil
	default:
		return "", fmt.Errorf("invalid mode %q: must be one of %q, %q or %q",
			s, ModeController, ModeNode, ModeAll)
	}
}

// runsController reports whether the mode registers the controller service.
func (m Mode) runsController() bool {
	return m == ModeController || m == ModeAll
}

// runsNode reports whether the mode registers the node service.
func (m Mode) runsNode() bool {
	return m == ModeNode || m == ModeAll
}

// ValidateDriverName validates the CSI driver name (GetPluginInfo.name).
//
// The CSI spec allows at most 63 characters: dashes, dots and alphanumerics,
// and the name must start and end with an alphanumeric character
// ("^[a-zA-Z0-9]([a-zA-Z0-9.-]{0,61}[a-zA-Z0-9])?$"). Node ID and driver name end up
// in the CSI object names of Kubernetes, so a name that violates this would be
// rejected later by kubelet with a much less obvious message.
func ValidateDriverName(name string) error {
	if name == "" {
		return errors.New("driver name must not be empty")
	}
	if len(name) > maxDriverNameLen {
		return fmt.Errorf("driver name %q is %d characters long, the maximum is %d",
			name, len(name), maxDriverNameLen)
	}
	for i, r := range name {
		first, last := i == 0, i == len(name)-1
		switch {
		case isAlnum(r):
		case r == '-' || r == '.':
			if first || last {
				return fmt.Errorf("driver name %q must start and end with an alphanumeric character", name)
			}
		default:
			return fmt.Errorf("driver name %q contains %q, only alphanumerics, '-' and '.' are allowed", name, r)
		}
	}
	return nil
}

// isAlnum reports whether r is an ASCII letter or digit. The spec talks about
// alphanumerics, so non-ASCII letters are rejected even though unicode.IsLetter
// would accept them.
func isAlnum(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

// Options configures a [Driver]. It mirrors the command line flags.
type Options struct {
	// DriverName is reported by GetPluginInfo.
	DriverName string
	// NodeID identifies the Kubernetes node; required in node and all mode and
	// reported by NodeGetInfo (T-06).
	NodeID string
	// Endpoint is the --endpoint value, parsed by [ParseEndpoint].
	Endpoint string
	// Mode selects which services are registered.
	Mode Mode
	// Version is the build version, normally version.Get().Version.
	Version string
	// Config is the live cluster configuration. The Node and Controller
	// services read it and main keeps it up to date. It is required in
	// controller, node and all mode: NodeStageVolume looks the servers of a
	// cluster up by clusterID, and the controller RPCs decide whether a volume
	// exists by asking whether its cluster is configured (D-23).
	Config *config.Store
	// MountLogDir is the directory of the per-volume glusterfs mount logs; empty
	// disables log-file.
	MountLogDir string
	// StateDir is the directory of the staging records the mount recovery works
	// from. Empty disables the records and with them the recovery (D-20).
	StateDir string
	// ControllerMountDir is the directory the controller mounts the parent volumes
	// below, one mount point per cluster and parent volume at
	// <dir>/<clusterID>/<volume> (D-24). It is required in controller and all mode
	// and ignored in node mode, which never mounts a parent volume.
	ControllerMountDir string
	// RecoveryInterval is how often the recovery pass is repeated after the one at
	// startup. 0 means "only at startup"; a negative value is an error.
	RecoveryInterval time.Duration
}

// Driver is the CSI plugin. It implements the Identity service; the Controller
// service is registered through the controller field and the Node service
// through the node field, each only in the modes that run it.
//
// The controller service mounts the parent volumes of the subdir volumes it
// provisions and keeps the mounts until the process stops (D-24), so a Driver in
// controller or all mode owns the mounts those volumes need.
//
// A Driver is safe for concurrent use: the registration flags are only written
// before the gRPC server starts, and everything else is read-only afterwards.
type Driver struct {
	// identityService provides the csi.IdentityServer methods. Embedding it
	// keeps the generated UnimplementedIdentityServer in one place.
	identityService

	driverName string
	nodeID     string
	mode       Mode
	version    string
	config     *config.Store
	endpoint   string

	// controllerRegistered is true when the Controller service is registered.
	// GetPluginCapabilities is derived from it.
	controllerRegistered bool

	// controller is the Controller service, nil unless the mode runs it. It is
	// not embedded in Driver for the same reason as node: the generated
	// Unimplemented*Server types all define testEmbeddedByValue, so embedding
	// more than one of them would make every selector on them ambiguous.
	controller *controllerService

	// node is the Node service, nil unless the mode runs it. It is not embedded
	// in Driver: UnimplementedIdentityServer and UnimplementedNodeServer both
	// define testEmbeddedByValue, so embedding both would be ambiguous.
	node *nodeService

	// nodeRegistered is true when the Node service is registered.
	nodeRegistered bool

	// recoveryInterval is how often the node service repeats its recovery pass
	// once the gRPC server runs. It is only used in node and all mode.
	recoveryInterval time.Duration

	// checks are the Probe readiness checks of the current mode.
	checks []check

	// stat and lookPath are the dependencies of the readiness checks. They are
	// fields so that tests can replace them: a unit test must not depend on the
	// machine it runs on having /dev/fuse or mount.glusterfs.
	stat     func(name string) (os.FileInfo, error)
	lookPath func(file string) (string, error)
}

// New validates opts and returns a Driver.
//
// The validation is done here rather than only in the flag handling of main,
// because New is the entry point of the tests too and an invalid Driver must
// never reach the gRPC server.
func New(opts Options) (*Driver, error) {
	if err := ValidateDriverName(opts.DriverName); err != nil {
		return nil, fmt.Errorf("invalid driver name: %w", err)
	}

	mode, err := ParseMode(string(opts.Mode))
	if err != nil {
		return nil, err
	}

	if _, _, err := ParseEndpoint(opts.Endpoint); err != nil {
		return nil, err
	}

	if mode.runsNode() && strings.TrimSpace(opts.NodeID) == "" {
		return nil, fmt.Errorf("node id is required in %q mode", mode)
	}

	// Every mode needs the cluster configuration, for two reasons a step apart:
	// NodeStageVolume looks the servers of a volume up by clusterID, and the
	// controller RPCs decide whether a volume exists by asking whether its
	// cluster is configured (D-23). A plugin without a configuration could never
	// stage anything and would report every volume as NotFound.
	if opts.Config == nil {
		return nil, fmt.Errorf("cluster configuration is required in %q mode", mode)
	}

	if err := validateMountLogDir(opts.MountLogDir); err != nil {
		return nil, err
	}
	if err := validateStateDir(opts.StateDir); err != nil {
		return nil, err
	}
	// The controller writes and deletes below its mount points, so a relative or
	// unclean path would address a different directory than the one the operator
	// configured. Node mode never uses the value and accepts it empty.
	if mode.runsController() || opts.ControllerMountDir != "" {
		if err := validateControllerMountDir(opts.ControllerMountDir); err != nil {
			return nil, err
		}
	}
	if opts.RecoveryInterval < 0 {
		return nil, fmt.Errorf("invalid recovery interval %s: must not be negative", opts.RecoveryInterval)
	}

	version := opts.Version
	if strings.TrimSpace(version) == "" {
		version = unknownVersion
	}

	d := &Driver{
		driverName:       opts.DriverName,
		nodeID:           opts.NodeID,
		mode:             mode,
		version:          version,
		config:           opts.Config,
		endpoint:         opts.Endpoint,
		recoveryInterval: opts.RecoveryInterval,
		stat:             os.Stat,
		lookPath:         exec.LookPath,
	}
	d.identityService = identityService{driver: d}
	d.checks = d.readinessChecks()

	if mode.runsController() {
		d.controller = &controllerService{
			driver:  d,
			locks:   newInflight(),
			parents: newParentMounts(newMounter(opts.MountLogDir), opts.ControllerMountDir),
			now:     time.Now,
		}
		d.controllerRegistered = true
	}

	if mode.runsNode() {
		d.node = &nodeService{
			driver:  d,
			mounter: newMounter(opts.MountLogDir),
			logDir:  opts.MountLogDir,
			locks:   newInflight(),
			stat:    os.Stat,
			statFS:  statFS,
		}
		if opts.StateDir != "" {
			d.node.records = &recordStore{dir: opts.StateDir}
		}
		d.nodeRegistered = true
	}

	klog.V(2).InfoS("glusterfs-csi driver initialised",
		"driverName", d.driverName, "mode", string(d.mode), "version", d.version,
		"stateDir", opts.StateDir, "recoveryInterval", opts.RecoveryInterval.String(),
		"controllerMountDir", opts.ControllerMountDir)

	return d, nil
}

// Run listens on the configured endpoint and serves until ctx is cancelled.
//
// A unix socket that is left over from a previous container is removed before
// listening, the parent directory is not created (kubelet owns it) and the
// socket file is removed again on return, so that a restart finds a clean
// state.
func (d *Driver) Run(ctx context.Context) error {
	network, address, err := ParseEndpoint(d.endpoint)
	if err != nil {
		return err
	}

	if network == networkUnix {
		if prepErr := prepareUnixSocket(address); prepErr != nil {
			return prepErr
		}
		defer removeUnixSocket(address)
	}

	lis, err := net.Listen(network, address)
	if err != nil {
		return fmt.Errorf("listen on %s %q: %w", network, address, err)
	}

	klog.InfoS("glusterfs-csi listening", "network", network, "address", lis.Addr().String(),
		"mode", string(d.mode), "driverName", d.driverName)

	return d.Serve(ctx, lis)
}

// Serve serves the CSI gRPC API on lis until ctx is cancelled or the server
// stops. It is exported so that tests can drive the driver over a bufconn
// listener without touching the filesystem.
//
// On cancellation the server is stopped gracefully; if that takes longer than
// gracefulStopTimeout the remaining RPCs are aborted with Stop. grpc's
// ErrServerStopped is not an error here: it is how the gRPC server reports the
// Stop call that cancelling ctx triggered.
func (d *Driver) Serve(ctx context.Context, lis net.Listener) error {
	srv, err := d.newGRPCServer()
	if err != nil {
		return err
	}

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(lis)
	}()

	// The recovery runs next to the gRPC server, not before it: it must not delay
	// the first RPC of kubelet. Its context is derived from ctx and cancelled on
	// every return path below, and the wait group makes the return wait for the
	// pass that is still running.
	stopRecovery := d.startRecovery(ctx)
	defer stopRecovery()

	// The parent volume mounts of the controller are closed after the gRPC server
	// stopped, on both return paths: closing a root while an RPC still uses it would
	// fail that RPC with os.ErrClosed for no reason. The deferred call is the one
	// registered last, so it runs first; the recovery pass and the unmount do not
	// depend on each other.
	defer d.closeParentMounts()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			return fmt.Errorf("serve gRPC: %w", err)
		}
		return nil
	case <-ctx.Done():
	}

	klog.InfoS("glusterfs-csi shutting down", "reason", context.Cause(ctx))
	stopGRPC(srv)

	if err := <-serveErr; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return fmt.Errorf("serve gRPC: %w", err)
	}
	klog.InfoS("glusterfs-csi stopped")
	return nil
}

// parentMountCloseTimeout bounds how long closing the parent volume mounts may
// take after the gRPC server stopped. An unmount of a dead FUSE mount can block for
// a long time, and a plugin that has stopped serving must still exit.
const parentMountCloseTimeout = 30 * time.Second

// closeParentMounts closes the parent volume mounts of the controller service.
//
// It is a no-op in node mode, where there is no controller service. The context is
// fresh instead of the one Serve was given, because that one is already cancelled at
// this point and the unmount would refuse to run.
func (d *Driver) closeParentMounts() {
	if d.controller == nil || d.controller.parents == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), parentMountCloseTimeout)
	defer cancel()
	d.controller.parents.close(ctx)
}

// startRecovery starts the mount recovery of the node service in the background
// and returns the function that stops it and waits for it to return.
//
// The returned stop function is idempotent, so a caller can defer it and still
// call it explicitly on the shutdown path. Without a node service or without a
// state directory (--state-dir is empty) no goroutine is started at all (D-20).
func (d *Driver) startRecovery(ctx context.Context) (stop func()) {
	if d.node == nil || d.node.records == nil {
		return func() {}
	}

	recoveryCtx, cancel := context.WithCancel(ctx)

	var (
		once sync.Once
		wg   sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		d.node.runRecovery(recoveryCtx, d.recoveryInterval)
	}()

	return func() {
		once.Do(func() {
			cancel()
			wg.Wait()
		})
	}
}

// validateMountLogDir validates Options.MountLogDir. An empty value disables the
// mount log file and is always accepted; anything else is used in a path, so it
// has to be absolute and clean instead of being normalized silently.
func validateMountLogDir(dir string) error {
	switch {
	case dir == "":
		return nil
	case !filepath.IsAbs(dir):
		return fmt.Errorf("invalid mount log directory %q: must be an absolute path", dir)
	case filepath.Clean(dir) != dir:
		return fmt.Errorf("invalid mount log directory %q: must be a clean path", dir)
	}
	return nil
}

// validateStateDir validates Options.StateDir. An empty value disables the
// staging records and the recovery and is always accepted; anything else becomes
// a directory the driver creates and writes into, so it has to be absolute and
// clean instead of being normalized silently.
func validateStateDir(dir string) error {
	return validateAbsoluteDir("state directory", dir, true)
}

// validateControllerMountDir validates Options.ControllerMountDir, the directory
// of the parent volume mount points of the controller (D-24). Unlike a state
// directory it is required: it is empty only in node mode, which New checks
// before calling this.
func validateControllerMountDir(dir string) error {
	return validateAbsoluteDir("controller mount directory", dir, false)
}

// validateAbsoluteDir reports whether dir is a usable absolute, clean directory
// path. An empty value is accepted when optional is set.
func validateAbsoluteDir(what, dir string, optional bool) error {
	switch {
	case dir == "":
		if optional {
			return nil
		}
		return fmt.Errorf("the %s is required", what)
	case !filepath.IsAbs(dir):
		return fmt.Errorf("invalid %s %q: must be an absolute path", what, dir)
	case filepath.Clean(dir) != dir:
		return fmt.Errorf("invalid %s %q: must be a clean path", what, dir)
	}
	return nil
}

// NodeID returns the raw --nodeid value. It is empty in controller only mode
// and is reported by NodeGetInfo.
func (d *Driver) NodeID() string { return d.nodeID }

// Config returns the cluster configuration store. It is required by [New] in
// every mode, so it is never nil for a Driver that New returned; the Controller
// and Node services still guard against a nil store, because a Driver built by
// hand in a test may not have one.
func (d *Driver) Config() *config.Store { return d.config }
