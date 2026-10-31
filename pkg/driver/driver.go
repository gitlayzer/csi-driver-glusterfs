// Package driver implements the CSI plugin: the gRPC server, the Identity
// service and the process lifecycle of the glusterfs-csi binary.
//
// The package is deliberately thin. The Controller and Node services are added
// by later tasks; they register themselves on the same [Driver] and are reached
// through the same gRPC server, so the plugin capability list is derived from
// what is actually registered instead of being hard-coded
// (docs/architecture.md §5.4).
package driver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"

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
	// Config is the live cluster configuration. T-04 only stores it; the
	// Controller service reads it and main keeps it up to date.
	Config *config.Store
}

// Driver is the CSI plugin. It implements the Identity service and, once later
// tasks add them, the Controller and Node services.
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
	// GetPluginCapabilities is derived from it. T-10 sets it.
	controllerRegistered bool

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

	version := opts.Version
	if strings.TrimSpace(version) == "" {
		version = unknownVersion
	}

	d := &Driver{
		driverName: opts.DriverName,
		nodeID:     opts.NodeID,
		mode:       mode,
		version:    version,
		config:     opts.Config,
		endpoint:   opts.Endpoint,
		stat:       os.Stat,
		lookPath:   exec.LookPath,
	}
	d.identityService = identityService{driver: d}
	d.checks = d.readinessChecks()

	klog.V(2).InfoS("glusterfs-csi driver initialised",
		"driverName", d.driverName, "mode", string(d.mode), "version", d.version)

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

// NodeID returns the raw --nodeid value. It is empty in controller only mode
// and is reported by NodeGetInfo (T-06).
func (d *Driver) NodeID() string { return d.nodeID }

// Config returns the cluster configuration store. It may be nil in tests that
// do not need a configuration; the Controller service (T-10) handles that.
func (d *Driver) Config() *config.Store { return d.config }
