// Command glusterfs-csi is the single binary of the GlusterFS CSI driver.
//
// It runs as the controller plugin, the node plugin or both ("all"), selected
// with --mode, and serves the CSI gRPC API on --endpoint. The process exits on
// SIGINT or SIGTERM after stopping the gRPC server gracefully.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"k8s.io/klog/v2"

	"github.com/gitlayzer/csi-driver-glusterfs/pkg/config"
	"github.com/gitlayzer/csi-driver-glusterfs/pkg/driver"
	"github.com/gitlayzer/csi-driver-glusterfs/pkg/version"
)

const (
	// DriverName is the default CSI driver name.
	DriverName = "glusterfs.csi.io"
	// DefaultEndpoint is the default gRPC endpoint exposed to the CSI sidecars.
	DefaultEndpoint = "unix:///csi/csi.sock"
	// DefaultConfigPath is the default path of the cluster configuration file.
	DefaultConfigPath = "/etc/glusterfs-csi/config.json"
	// DefaultMountLogDir is the default directory of the per-volume mount logs.
	DefaultMountLogDir = "/var/log/glusterfs/csi"
	// DefaultStateDir is the default directory of the staging records. It lives
	// in the kubelet plugin directory, which the DaemonSet already mounts as a
	// volume and which survives a restart of the plugin container (D-20).
	DefaultStateDir = "/var/lib/kubelet/plugins/glusterfs.csi.io/staged"
	// DefaultRecoveryInterval is how often the mount recovery runs after the pass
	// at startup.
	DefaultRecoveryInterval = 60 * time.Second
)

// usageError wraps an error that the flag package already reported on its own
// output writer, so that main does not print it a second time.
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }

func (e *usageError) Unwrap() error { return e.err }

// options holds the values of the command line flags.
type options struct {
	endpoint         string
	nodeID           string
	mode             string
	driverName       string
	configPath       string
	mountLogDir      string
	stateDir         string
	recoveryInterval time.Duration
	showVer          bool
}

// validateFlags checks the parsed command line flags.
//
// It returns an error describing the first problem found so that the caller can
// exit with a non-zero status and a readable message. The checks delegate to
// pkg/driver, which is the package that enforces them again in driver.New: the
// flag handling only turns them into messages an operator can act on.
func validateFlags(opts options) error {
	mode, err := driver.ParseMode(opts.mode)
	if err != nil {
		return fmt.Errorf("invalid --mode: %w", err)
	}

	// The node plugin must know which Kubernetes node it runs on: the value is
	// reported by NodeGetInfo (T-06) and is used to reject requests that are
	// addressed to a different node.
	if mode == driver.ModeNode || mode == driver.ModeAll {
		if strings.TrimSpace(opts.nodeID) == "" {
			return fmt.Errorf("--nodeid is required when --mode is %q", opts.mode)
		}
	}

	if opts.endpoint == "" {
		return errors.New("--endpoint must not be empty")
	}
	if _, _, err := driver.ParseEndpoint(opts.endpoint); err != nil {
		return fmt.Errorf("invalid --endpoint: %w", err)
	}

	if strings.TrimSpace(opts.driverName) == "" {
		return errors.New("--drivername must not be empty")
	}
	if err := driver.ValidateDriverName(opts.driverName); err != nil {
		return fmt.Errorf("invalid --drivername: %w", err)
	}
	if strings.TrimSpace(opts.configPath) == "" {
		return errors.New("--config must not be empty")
	}

	// An empty value disables the mount log file; anything else ends up in a path.
	if opts.mountLogDir != "" {
		if !filepath.IsAbs(opts.mountLogDir) {
			return fmt.Errorf("invalid --mount-log-dir: %q must be an absolute path", opts.mountLogDir)
		}
		if filepath.Clean(opts.mountLogDir) != opts.mountLogDir {
			return fmt.Errorf("invalid --mount-log-dir: %q must be a clean path", opts.mountLogDir)
		}
	}

	// An empty value disables the staging records and the mount recovery; anything
	// else becomes a directory the driver creates and writes into.
	if opts.stateDir != "" {
		if !filepath.IsAbs(opts.stateDir) {
			return fmt.Errorf("invalid --state-dir: %q must be an absolute path", opts.stateDir)
		}
		if filepath.Clean(opts.stateDir) != opts.stateDir {
			return fmt.Errorf("invalid --state-dir: %q must be a clean path", opts.stateDir)
		}
	}
	// 0 is allowed and means "only at startup"; a negative interval would make
	// time.NewTicker panic.
	if opts.recoveryInterval < 0 {
		return fmt.Errorf("invalid --recovery-interval: %s must not be negative", opts.recoveryInterval)
	}

	return nil
}

// parseFlags registers the flags and parses args. It is separate from main so
// that tests can exercise it without touching the process environment.
func parseFlags(args []string, output io.Writer) (options, error) {
	opts := options{}

	fs := flag.NewFlagSet("glusterfs-csi", flag.ContinueOnError)
	fs.SetOutput(output)

	fs.StringVar(&opts.endpoint, "endpoint", DefaultEndpoint, "CSI endpoint exposed to the sidecars, e.g. unix:///csi/csi.sock or tcp://127.0.0.1:10000")
	fs.StringVar(&opts.nodeID, "nodeid", "", "Kubernetes node name; required when --mode is node or all")
	fs.StringVar(&opts.mode, "mode", string(driver.ModeAll), "Run mode: controller, node or all")
	fs.StringVar(&opts.driverName, "drivername", DriverName, "Name of the CSI driver")
	fs.StringVar(&opts.configPath, "config", DefaultConfigPath, "Path of the cluster configuration file")
	fs.StringVar(&opts.mountLogDir, "mount-log-dir", DefaultMountLogDir,
		"Directory of the per-volume glusterfs mount logs; an empty value disables the log-file mount option")
	fs.StringVar(&opts.stateDir, "state-dir", DefaultStateDir,
		"Directory of the staging records the mount recovery works from; an empty value disables the records and the recovery")
	fs.DurationVar(&opts.recoveryInterval, "recovery-interval", DefaultRecoveryInterval,
		"How often the mount recovery scans the staging records; 0 scans at startup only")
	fs.BoolVar(&opts.showVer, "version", false, "Print the version and exit")

	klog.InitFlags(fs)

	if err := fs.Parse(args); err != nil {
		return options{}, &usageError{err: err}
	}
	if fs.NArg() > 0 {
		return options{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}

	return opts, nil
}

// run is the whole lifecycle of the process except signal handling: parse and
// validate the flags, load the cluster configuration, start the configuration
// poller, serve gRPC until ctx is cancelled and wait for the poller to return.
//
// Every step that can fail does so before the gRPC server starts, so a
// misconfiguration never leaves a half running plugin behind.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	opts, err := parseFlags(args, stderr)
	if err != nil {
		return err
	}

	if opts.showVer {
		if _, writeErr := fmt.Fprintln(stdout, version.Get().String()); writeErr != nil {
			return fmt.Errorf("write version: %w", writeErr)
		}
		return nil
	}

	if validErr := validateFlags(opts); validErr != nil {
		return validErr
	}

	mode, err := driver.ParseMode(opts.mode)
	if err != nil {
		// Unreachable: validateFlags just checked it.
		return err
	}

	// A missing or invalid configuration file has to fail the start: serving
	// without any cluster would only turn the problem into a failing
	// CreateVolume later.
	store, err := config.NewStore(opts.configPath)
	if err != nil {
		return fmt.Errorf("load config %q: %w", opts.configPath, err)
	}

	// The poller gets a derived context so that run can stop it on every return
	// path, not only when the caller cancels ctx: a failed start (for example a
	// missing config directory) returns before ctx is done, and waiting for a
	// poller that is still running would hang the process.
	cfgCtx, cancelCfg := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// A non-positive interval means the store's default (10s).
		store.Run(cfgCtx, 0)
	}()
	// Deferred so that the order is cancel, then wait, on every return path.
	defer func() {
		cancelCfg()
		wg.Wait()
	}()

	d, err := driver.New(driver.Options{
		DriverName:       opts.driverName,
		NodeID:           opts.nodeID,
		Endpoint:         opts.endpoint,
		Mode:             mode,
		Version:          version.Get().Version,
		Config:           store,
		MountLogDir:      opts.mountLogDir,
		StateDir:         opts.stateDir,
		RecoveryInterval: opts.recoveryInterval,
	})
	if err != nil {
		return err
	}

	klog.InfoS("glusterfs-csi starting", "mode", opts.mode, "nodeID", opts.nodeID,
		"driverName", opts.driverName, "endpoint", opts.endpoint, "mountLogDir", opts.mountLogDir,
		"stateDir", opts.stateDir, "recoveryInterval", opts.recoveryInterval.String(),
		"version", version.Get().Version)

	return d.Run(ctx)
}

func main() {
	// SIGTERM is what the kubelet sends on eviction and rolling updates;
	// cancelling the context is what makes Run stop the gRPC server gracefully
	// and remove the unix socket.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		// Flag parse errors are already printed by the flag package itself.
		var usageErr *usageError
		if !errors.As(err, &usageErr) {
			fmt.Fprintf(os.Stderr, "glusterfs-csi: %v\n", err)
		}
		stop()
		os.Exit(1)
	}
}
