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
	"strings"
	"sync"
	"syscall"

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
)

// usageError wraps an error that the flag package already reported on its own
// output writer, so that main does not print it a second time.
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }

func (e *usageError) Unwrap() error { return e.err }

// options holds the values of the command line flags.
type options struct {
	endpoint   string
	nodeID     string
	mode       string
	driverName string
	configPath string
	showVer    bool
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
		DriverName: opts.driverName,
		NodeID:     opts.nodeID,
		Endpoint:   opts.endpoint,
		Mode:       mode,
		Version:    version.Get().Version,
		Config:     store,
	})
	if err != nil {
		return err
	}

	klog.InfoS("glusterfs-csi starting", "mode", opts.mode, "nodeID", opts.nodeID,
		"driverName", opts.driverName, "endpoint", opts.endpoint, "version", version.Get().Version)

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
