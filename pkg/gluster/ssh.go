package gluster

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"k8s.io/klog/v2"
)

const (
	// DefaultDialTimeout is the time limit for connecting, handshaking and
	// authenticating to a server. Management commands are rare, so a dead host is
	// skipped after this timeout at worst.
	DefaultDialTimeout = 10 * time.Second

	// DefaultMaxOutput is the maximum amount of stdout that is collected. A
	// gluster command answers with a few kilobytes of XML; the limit only keeps a
	// run away command from exhausting the controller's memory.
	DefaultMaxOutput = 16 << 20
)

// SSHOptions tunes an [SSHExecutor]. The zero value is usable and means
// [DefaultDialTimeout] and [DefaultMaxOutput] with a plain TCP dialer.
type SSHOptions struct {
	// DialTimeout bounds connecting, handshaking and authenticating together.
	DialTimeout time.Duration
	// MaxOutput is the maximum number of stdout bytes that are collected.
	MaxOutput int
	// Dial opens the TCP connection. It defaults to a net.Dialer's DialContext
	// and exists so that tests can map host names to a local listener.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// SSHExecutor runs commands on GlusterFS servers over SSH.
//
// Every call opens and closes its own connection, authenticates with the single
// key of the [Credentials] and verifies the host key against their known_hosts.
// There is no connection pool and no background goroutine, so a credential
// rotation takes effect with the next call and nothing has to be shut down. The
// executor has no mutable state and may be used concurrently.
type SSHExecutor struct {
	creds       *Credentials
	dialTimeout time.Duration
	maxOutput   int
	dial        func(ctx context.Context, network, addr string) (net.Conn, error)
}

// NewSSHExecutor returns an executor that connects with creds. It fails if creds
// is nil, because an executor without credentials could not verify the host key
// or authenticate.
func NewSSHExecutor(creds *Credentials, opts SSHOptions) (*SSHExecutor, error) {
	if creds == nil {
		return nil, errors.New("gluster: ssh credentials are required")
	}
	if creds.signer == nil || creds.hostKeyCallback == nil {
		return nil, errors.New("gluster: ssh credentials are incomplete")
	}
	executor := &SSHExecutor{
		creds:       creds,
		dialTimeout: opts.DialTimeout,
		maxOutput:   opts.MaxOutput,
		dial:        opts.Dial,
	}
	if executor.dialTimeout <= 0 {
		executor.dialTimeout = DefaultDialTimeout
	}
	if executor.maxOutput <= 0 {
		executor.maxOutput = DefaultMaxOutput
	}
	if executor.dial == nil {
		dialer := &net.Dialer{}
		executor.dial = dialer.DialContext
	}
	return executor, nil
}

// Run runs args (args[0] is the program) on host and returns its stdout.
//
// The command line is built by shellQuote, so every argument reaches the remote
// side as one uninterpreted shell word. Failures before the remote command
// starts wrap [ErrConnect]; a command that ran and exited non-zero returns an
// [ExitError]; a cancelled context returns an error that satisfies
// errors.Is(err, ctx.Err()).
func (e *SSHExecutor) Run(ctx context.Context, host string, args ...string) ([]byte, error) {
	if err := validateCommand(host, args); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, interruptedError(err)
	}
	started := time.Now()

	client, err := e.connect(ctx, host)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()

	session, err := client.NewSession()
	if err != nil {
		return nil, e.connectError(ctx, host, err, "cannot open an ssh session on %s", host)
	}
	defer func() { _ = session.Close() }()

	stdout := &limitedWriter{limit: e.maxOutput, onExceed: func() { _ = client.Close() }}
	stderr := &boundedWriter{limit: maxExitStderr}
	session.Stdout = stdout
	session.Stderr = stderr

	command := shellQuote(args)
	if err := session.Start(command); err != nil {
		return nil, e.connectError(ctx, host, err, "the server %s rejected the exec request", host)
	}
	klog.V(4).InfoS("started a command on a glusterfs server",
		"host", host, "argv", args, "commandLength", len(command))

	waitErr := e.wait(ctx, client, session)
	sinceStart := time.Since(started)

	// A cancellation that raced with the end of the command is reported as the
	// interruption the caller asked for, unless the server delivered a real exit
	// status: then the command did run to completion and the status is reported.
	code, hasStatus := exitStatusCode(waitErr)
	if ctxErr := ctx.Err(); ctxErr != nil && !hasStatus {
		klog.V(4).InfoS("interrupted a command on a glusterfs server",
			"host", host, "argv", args, "duration", sinceStart)
		return nil, interruptedError(ctxErr)
	}

	// The connection was closed because of the output limit, so waitErr is
	// whatever the closed channel produced; the limit is the real cause.
	if stdout.Exceeded() {
		return nil, fmt.Errorf("output of %s on %s exceeds %d bytes: %w",
			args[0], host, e.maxOutput, errOutputLimit)
	}
	if waitErr == nil {
		klog.V(4).InfoS("finished a command on a glusterfs server",
			"host", host, "argv", args, "exitCode", 0, "duration", sinceStart)
		return stdout.Bytes(), nil
	}

	if hasStatus {
		klog.V(4).InfoS("a command on a glusterfs server failed",
			"host", host, "argv", args, "exitCode", code, "duration", sinceStart)
		return nil, &ExitError{
			Host:    host,
			Program: args[0],
			Code:    code,
			Stderr:  truncateOutput(stderr.Bytes(), maxExitStderr),
		}
	}
	var missingErr *ssh.ExitMissingError
	if errors.As(waitErr, &missingErr) {
		return nil, fmt.Errorf("the connection to %s closed before %s reported an exit status",
			host, args[0])
	}
	return nil, fmt.Errorf("%s on %s failed after the command started: %w", args[0], host, waitErr)
}

// exitStatusCode returns the exit status the server reported, if err carries one.
// An exit status means the command ran to completion on the server, which is why
// it is reported even when the caller's context was cancelled at the same moment.
func exitStatusCode(err error) (int, bool) {
	var exitErr *ssh.ExitError
	if !errors.As(err, &exitErr) {
		return 0, false
	}
	return exitErr.ExitStatus(), true
}

// connect dials host, performs the handshake and returns the authenticated
// client. Every failure is reported through connectError, so it either wraps
// [ErrConnect] or, when the context is done, the context error.
func (e *SSHExecutor) connect(ctx context.Context, host string) (*ssh.Client, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(e.creds.Port))
	deadline := time.Now().Add(e.dialTimeout)

	dialCtx, cancel := context.WithDeadline(ctx, deadline)
	conn, err := e.dial(dialCtx, "tcp", addr)
	cancel()
	if err != nil {
		return nil, e.connectError(ctx, host, err, "cannot connect to %s", addr)
	}

	// The deadline covers the handshake as well, so a peer that accepts the
	// connection and then stalls cannot hold the caller for longer than
	// DialTimeout. It is removed once the connection is authenticated: a command
	// may legitimately take longer than the dial timeout.
	if deadlineErr := conn.SetDeadline(deadline); deadlineErr != nil {
		_ = conn.Close()
		return nil, e.connectError(ctx, host, deadlineErr, "cannot set a deadline on the connection to %s", addr)
	}

	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		// Closing the connection is the only way to break out of a handshake
		// that is blocked in a read.
		_ = conn.Close()
		close(closed)
	})
	clientConn, chans, reqs, err := ssh.NewClientConn(conn, addr, e.clientConfig())
	if !stop() {
		<-closed
	}
	if err != nil {
		_ = conn.Close()
		return nil, e.handshakeError(ctx, host, addr, err)
	}
	if clearErr := conn.SetDeadline(time.Time{}); clearErr != nil {
		_ = conn.Close()
		return nil, e.connectError(ctx, host, clearErr, "cannot clear the deadline of the connection to %s", addr)
	}
	return ssh.NewClient(clientConn, chans, reqs), nil
}

// clientConfig builds the SSH client configuration. The host key callback and
// the algorithm list always come from the credentials: there is no setting that
// turns verification off.
func (e *SSHExecutor) clientConfig() *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User:              e.creds.User,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(e.creds.signer)},
		HostKeyCallback:   e.creds.hostKeyCallback,
		HostKeyAlgorithms: e.creds.hostKeyAlgorithms,
	}
}

// wait waits for the remote command to finish and closes the connection when the
// context is done first: closing it makes the server hang up on the remote
// command (SIGHUP). The wait goroutine is always joined, so no goroutine outlives
// the call, and the caller decides on the context from ctx.Err() afterwards, so
// the outcome does not depend on which of the two events the select saw first.
func (e *SSHExecutor) wait(ctx context.Context, client *ssh.Client, session *ssh.Session) error {
	done := make(chan error, 1)
	go func() { done <- session.Wait() }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = client.Close()
		return <-done
	}
}

// handshakeError turns a failed handshake into an error message that says what
// went wrong: an unknown host, a changed host key or a rejected key.
func (e *SSHExecutor) handshakeError(ctx context.Context, host, addr string, err error) error {
	var keyErr *knownhosts.KeyError
	if errors.As(err, &keyErr) {
		if len(keyErr.Want) == 0 {
			return e.connectError(ctx, host, err, "%s is not in known_hosts", addr)
		}
		return e.connectError(ctx, host, err,
			"the host key of %s does not match known_hosts (possible man-in-the-middle attack)", addr)
	}
	if isAuthFailure(err) {
		return e.connectError(ctx, host, err, "%s rejected the ssh key of %q", addr, e.creds.User)
	}
	return e.connectError(ctx, host, err, "the ssh handshake with %s failed", addr)
}

// authFailureMarker is part of the error message the ssh package builds when the
// server rejects every offered authentication method. The package has no typed
// error for that case, so the message is the only way to tell it apart; the
// marker is stable and does not contain any key material.
const authFailureMarker = "unable to authenticate"

// isAuthFailure reports whether err says that the authentication was rejected.
func isAuthFailure(err error) bool {
	return err != nil && strings.Contains(err.Error(), authFailureMarker)
}

// connectError reports a failure that happened before the remote command
// started. A cancelled or expired context wins over [ErrConnect]: it is not a
// host failure, so Failover must not try the next host because of it.
func (e *SSHExecutor) connectError(ctx context.Context, host string, err error, format string, args ...any) error {
	message := fmt.Sprintf(format, args...)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%s: %w", message, ctxErr)
	}
	klog.V(4).InfoS("cannot reach a glusterfs server", "host", host, "reason", message)
	return fmt.Errorf("%w: %s: %w", ErrConnect, message, err)
}

// interruptedError wraps a context error so that errors.Is(err, ctx.Err())
// succeeds while the message says what was interrupted.
func interruptedError(err error) error {
	return fmt.Errorf("running the command on the glusterfs server was interrupted: %w", err)
}

// boundedWriter collects at most limit bytes and silently drops the rest. It is
// used for stderr, which is only needed for the message of an [ExitError]: a
// command that spams stderr must not fail because of it.
type boundedWriter struct {
	buf   bytes.Buffer
	limit int
}

// Write keeps as much of p as fits into the limit and reports every byte as
// written, so that the ssh copy loop never fails because stderr was long.
func (w *boundedWriter) Write(p []byte) (int, error) {
	n := len(p)
	if room := w.limit - w.buf.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		_, _ = w.buf.Write(p)
	}
	return n, nil
}

// Bytes returns the collected bytes.
func (w *boundedWriter) Bytes() []byte { return w.buf.Bytes() }
