// Command glusterfs-csi is the single binary of the GlusterFS CSI driver.
//
// It can run as the controller plugin, the node plugin or both ("all"), selected
// with --mode. The gRPC server itself is implemented in a later task (T-04);
// for now the binary validates its flags, reports the build version and exits.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"k8s.io/klog/v2"

	"github.com/gitlayzer/csi-driver-glusterfs/pkg/version"
)

const (
	// DriverName is the default CSI driver name.
	DriverName = "glusterfs.csi.io"
	// DefaultEndpoint is the default gRPC endpoint exposed to the CSI sidecars.
	DefaultEndpoint = "unix:///csi/csi.sock"
	// DefaultConfigPath is the default path of the cluster configuration file.
	DefaultConfigPath = "/etc/glusterfs-csi/config.json"

	modeController = "controller"
	modeNode       = "node"
	modeAll        = "all"

	schemeUnix = "unix://"
	schemeTCP  = "tcp://"
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
// exit with a non-zero status and a readable message.
func validateFlags(opts options) error {
	switch opts.mode {
	case modeController, modeNode, modeAll:
	default:
		return fmt.Errorf("invalid --mode %q: must be one of %q, %q or %q", opts.mode, modeController, modeNode, modeAll)
	}

	// The node plugin must know which Kubernetes node it runs on: the value is
	// reported by NodeGetInfo (T-06) and is used to reject requests that are
	// addressed to a different node.
	if opts.mode == modeNode || opts.mode == modeAll {
		if strings.TrimSpace(opts.nodeID) == "" {
			return fmt.Errorf("--nodeid is required when --mode is %q", opts.mode)
		}
	}

	if opts.endpoint == "" {
		return errors.New("--endpoint must not be empty")
	}
	if !strings.HasPrefix(opts.endpoint, schemeUnix) && !strings.HasPrefix(opts.endpoint, schemeTCP) {
		return fmt.Errorf("invalid --endpoint %q: must start with %q or %q", opts.endpoint, schemeUnix, schemeTCP)
	}
	// Reject "unix://" / "tcp://" without an address part.
	if strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(opts.endpoint, schemeUnix), schemeTCP)) == "" {
		return fmt.Errorf("invalid --endpoint %q: address must not be empty", opts.endpoint)
	}

	if strings.TrimSpace(opts.driverName) == "" {
		return errors.New("--drivername must not be empty")
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
	fs.StringVar(&opts.mode, "mode", modeAll, "Run mode: controller, node or all")
	fs.StringVar(&opts.driverName, "drivername", DriverName, "Name of the CSI driver")
	fs.StringVar(&opts.configPath, "config", DefaultConfigPath, "Path of the cluster configuration file (not used yet)")
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

func run(args []string, stdout, stderr io.Writer) error {
	opts, err := parseFlags(args, stderr)
	if err != nil {
		return err
	}

	if opts.showVer {
		if _, err := fmt.Fprintln(stdout, version.Get().String()); err != nil {
			return fmt.Errorf("write version: %w", err)
		}
		return nil
	}

	if err := validateFlags(opts); err != nil {
		return err
	}

	// The gRPC server (Identity + Controller + Node services) arrives in T-04.
	klog.V(2).InfoS("glusterfs-csi starting", "mode", opts.mode, "nodeID", opts.nodeID, "driverName", opts.driverName, "endpoint", opts.endpoint, "config", opts.configPath)
	if _, err := fmt.Fprintln(stdout, "not implemented yet"); err != nil {
		return fmt.Errorf("write startup message: %w", err)
	}
	return nil
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		// Flag parse errors are already printed by the flag package itself.
		var usageErr *usageError
		if !errors.As(err, &usageErr) {
			fmt.Fprintf(os.Stderr, "glusterfs-csi: %v\n", err)
		}
		os.Exit(1)
	}
}
