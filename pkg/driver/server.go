package driver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-lib-utils/protosanitizer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

const (
	// gracefulStopTimeout bounds how long a cancelled server may take to drain
	// its in-flight RPCs before it is stopped hard.
	gracefulStopTimeout = 10 * time.Second

	// identityMethodPrefix is the path prefix of the Identity service. Its
	// methods are called by the livenessprobe sidecar every few seconds, so
	// their success is logged at a lower level than the rest.
	identityMethodPrefix = "/csi.v1.Identity/"

	// logLevelIdentity and logLevelDefault are the verbosity levels of
	// successful RPC log lines. Errors are always logged at ERROR.
	logLevelIdentity = 5
	logLevelDefault  = 2

	// internalErrorMessage is the message returned to the caller when a handler
	// panics. It deliberately contains nothing else: the panic value and the
	// method name are logged, never sent over the wire.
	internalErrorMessage = "internal error"
)

// newGRPCServer builds the gRPC server and registers every service the mode
// provides. It is the single registration point later tasks hook into: adding
// the Controller service here and setting controllerRegistered is all T-10
// needs.
func (d *Driver) newGRPCServer() (*grpc.Server, error) {
	srv := grpc.NewServer(grpc.ChainUnaryInterceptor(recoverInterceptor, logInterceptor))

	csi.RegisterIdentityServer(srv, d)

	// T-10 registers the Controller service here and sets
	// d.controllerRegistered = true; T-06 does the same for the Node service.
	if d.controllerRegistered && !d.mode.runsController() {
		return nil, fmt.Errorf("mode %q cannot register the controller service", d.mode)
	}
	if d.nodeRegistered && !d.mode.runsNode() {
		return nil, fmt.Errorf("mode %q cannot register the node service", d.mode)
	}
	if d.nodeRegistered {
		csi.RegisterNodeServer(srv, d.node)
	}

	klog.V(2).InfoS("glusterfs-csi gRPC server created", "mode", string(d.mode),
		"controllerRegistered", d.controllerRegistered, "nodeRegistered", d.nodeRegistered)
	return srv, nil
}

// stopGRPC stops srv gracefully and falls back to a hard stop when the graceful
// stop does not finish within gracefulStopTimeout.
//
// GracefulStop would deadlock without the fallback at startup and shutdown
// ordering bugs; the fallback runs in its own goroutine that is guaranteed to
// end because GracefulStop returns once Stop has been called.
func stopGRPC(srv *grpc.Server) {
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		srv.GracefulStop()
	}()

	timer := time.NewTimer(gracefulStopTimeout)
	defer timer.Stop()

	select {
	case <-stopped:
	case <-timer.C:
		klog.InfoS("graceful stop timed out, stopping gRPC server hard",
			"timeout", gracefulStopTimeout.String())
		srv.Stop()
		<-stopped
	}
}

// recoverInterceptor turns a panicking handler into an Internal error instead of
// crashing the process. A panic in a CSI plugin would take down the node or
// controller pod and could leave half finished mounts behind.
func recoverInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	defer func() {
		if r := recover(); r != nil {
			klog.ErrorS(nil, "panic in CSI handler", "method", info.FullMethod, "panic", r)
			resp = nil
			err = status.Error(codes.Internal, internalErrorMessage)
		}
	}()
	return handler(ctx, req)
}

// logInterceptor logs every RPC with its method, the gRPC code and how long it
// took (docs/architecture.md §5.7).
//
// Neither the request nor the response is logged by default: they can contain
// the CSI Secrets map. At V(5) both are logged, but only after
// protosanitizer.StripSecrets replaced every secret value.
// It returns the handler error unchanged, so the caller's status code survives.
func logInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	start := time.Now()
	resp, err := handler(ctx, req)
	elapsed := time.Since(start)

	code := status.Code(err)
	kvs := []any{
		"method", info.FullMethod,
		"code", code.String(),
		"duration", elapsed.String(),
	}

	if err != nil {
		klog.ErrorS(err, "CSI request failed", kvs...)
		return resp, err
	}

	verbose := klog.V(logLevelDefault)
	if strings.HasPrefix(info.FullMethod, identityMethodPrefix) {
		verbose = klog.V(logLevelIdentity)
	}
	if verbose.Enabled() {
		if klog.V(logLevelIdentity).Enabled() {
			kvs = append(kvs, "request", protosanitizer.StripSecrets(req),
				"response", protosanitizer.StripSecrets(resp))
		}
		verbose.InfoS("CSI request succeeded", kvs...)
	}

	return resp, err
}

// prepareUnixSocket makes sure path can be used as a unix socket.
//
// A socket left over by a killed container is removed, because binding to an
// existing socket path fails with EADDRINUSE and the driver could never
// restart. Anything else lying at that path (a regular file, a directory, a
// symlink) is not ours to delete: removing it could destroy data, so it is
// reported as an error.
//
// The parent directory has to exist already. kubelet creates the plugin
// directory; creating it here would hide a wrong --endpoint behind a driver
// that "works" but writes its socket somewhere unexpected.
func prepareUnixSocket(path string) error {
	dir := filepath.Dir(path)
	info, err := os.Stat(dir)
	switch {
	case err != nil:
		return fmt.Errorf("socket directory %q: %w", dir, err)
	case !info.IsDir():
		return fmt.Errorf("socket directory %q is not a directory", dir)
	}

	info, err = os.Lstat(path)
	switch {
	case err == nil:
		// os.ModeSocket is set for a unix domain socket, so a regular file is
		// never deleted here.
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("socket path %q exists and is not a socket (mode %s)", path, info.Mode())
		}
		if rmErr := os.Remove(path); rmErr != nil {
			return fmt.Errorf("remove stale socket %q: %w", path, rmErr)
		}
		klog.V(2).InfoS("removed stale unix socket", "path", path)
		return nil
	case errors.Is(err, os.ErrNotExist):
		return nil
	default:
		return fmt.Errorf("stat socket path %q: %w", path, err)
	}
}

// removeUnixSocket deletes the socket file the driver created. A failure is
// only logged: the process is shutting down and there is nothing left to do
// about it.
func removeUnixSocket(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		klog.ErrorS(err, "failed to remove unix socket", "path", path)
	}
}
