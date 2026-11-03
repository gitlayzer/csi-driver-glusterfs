package gluster

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// glusterdDownMessage is what the gluster CLI prints on stdout, with exit status 1,
// when it cannot reach the local glusterd. Verified on 9.2, 10.3 and 11.1
// (pkg/gluster/testdata/gluster-*/glusterd-down.xml).
//
// The text is on stdout, not stderr, and there is no XML: the CLI did not get as
// far as asking a glusterd anything, so the remote command never reached a cluster.
const glusterdDownMessage = "Connection failed. Please check if gluster daemon is operational."

// daemonCheck wraps an Executor and turns "glusterd is not running" into an error
// that wraps [ErrConnect], so that [Failover] tries the next server.
//
// The conversion is what keeps the promise of D-34 that a command is never
// executed twice: when the CLI cannot reach the local glusterd it has not sent the
// command anywhere, so another server may run it.
type daemonCheck struct{ Executor }

// Run runs the command on host and rewrites the "glusterd is not running" answer of
// the CLI.
func (d daemonCheck) Run(ctx context.Context, host string, args ...string) ([]byte, error) {
	stdout, err := d.Executor.Run(ctx, host, args...)
	if !isGlusterdDown(err) {
		return stdout, err
	}
	return nil, glusterdDownError(host)
}

// isGlusterdDown reports whether err is the "glusterd is not running" answer of the
// gluster CLI. All of these have to hold, so that no other failure is mistaken for an
// unreachable server:
//
//   - the error is an [ExitError],
//   - it is the gluster program (the allowlist may have more entries in M4),
//   - the exit status is 1,
//   - stdout is exactly the documented sentence.
//
// stderr is not consulted: the message is printed to stdout and glusterd may log
// anything to stderr, including nothing at all.
func isGlusterdDown(err error) bool {
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	return exitErr.Program == "gluster" &&
		exitErr.Code == 1 &&
		strings.TrimSpace(string(exitErr.Stdout)) == glusterdDownMessage
}

// glusterdDownError builds the replacement error. It wraps [ErrConnect] and
// deliberately does not wrap the [ExitError]: [Failover] looks for an ExitError
// first and would stop at this host, because it treats it as proof that the command
// ran. Here nothing ran, so the next server has to be tried.
func glusterdDownError(host string) error {
	return fmt.Errorf("%w: glusterd is not running on %s", ErrConnect, host)
}
