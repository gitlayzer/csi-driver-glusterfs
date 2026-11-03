package gluster

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// MaxCommandLength is the longest quoted command line that is sent to a
	// server. It bounds the size of the SSH exec request payload and keeps a
	// corrupted argument list from turning into a multi-megabyte request.
	MaxCommandLength = 64 << 10

	// maxHostLength is the longest host name that is accepted. The DNS limit is
	// 253 bytes; dotted IPv4 and IPv6 literals fit as well.
	maxHostLength = 255

	// maxExitStderr is the number of stderr bytes kept in an [ExitError].
	maxExitStderr = 64 << 10

	// maxExitStdout is the number of stdout bytes kept in an [ExitError]. A
	// gluster command that exits non-zero may still have written a complete
	// --xml answer (volume quota list exits with status 2 when quota is
	// disabled), so the output has to survive the failure.
	maxExitStdout = 64 << 10

	// maxExitStderrMessage is the number of stderr bytes shown in the message of
	// an [ExitError]. Anything longer is truncated and marked with "...".
	maxExitStderrMessage = 256
)

// allowedPrograms is the allowlist of programs that may be executed on a
// GlusterFS server. Anything else is rejected before a connection is made, so a
// validated command can only ever run one of these binaries. M4 adds the lvm
// commands of the thin brick mode here.
var allowedPrograms = map[string]bool{"gluster": true}

var (
	// ErrConnect marks every failure that happens before the remote command
	// starts: the TCP connection or the SSH handshake fails, the host key is not
	// in known_hosts or does not match it, the authentication fails, the session
	// cannot be opened or the exec request is rejected. It is therefore a
	// guarantee that nothing ran on the server, which is the only situation in
	// which [Failover] moves on to the next host. It is never wrapped around the
	// failure of a command that did start, and never around the cancellation of
	// the caller's context.
	ErrConnect = errors.New("cannot reach the glusterfs server")

	// ErrInvalidCommand marks a command that is rejected before any network
	// operation: an empty or malformed host, an empty argument list, a program
	// outside the allowlist, an argument with a control character or invalid
	// UTF-8, or a command line that is too long.
	ErrInvalidCommand = errors.New("invalid executor command")

	// errOutputLimit is returned by [limitedWriter] when the remote command wrote
	// more than the configured maximum of stdout. It is internal because the
	// caller facing error mentions the host and the limit.
	errOutputLimit = errors.New("stdout limit exceeded")
)

// Executor runs a command on one GlusterFS server.
type Executor interface {
	// Run runs args (args[0] is the program) on host and returns its stdout.
	Run(ctx context.Context, host string, args ...string) ([]byte, error)
}

// ExitError reports a command that ran on the server and exited with a non-zero
// status. The command did run, so the host is reachable and a retry elsewhere
// would run the command a second time; [Failover] therefore returns the error
// unchanged instead of trying the next host.
type ExitError struct {
	// Host is the server the command ran on.
	Host string
	// Program is args[0], the name of the command that failed.
	Program string
	// Code is the exit status the server reported.
	Code int
	// Stdout is what the command wrote to standard output, truncated to 64 KiB.
	// It is kept because a non-zero exit status does not mean the command failed:
	// gluster answers with a complete --xml document and a non-zero opRet in that
	// case, and `volume quota list` even exits with status 2 while printing the
	// answer. It is never part of [ExitError.Error]: the caller parses it.
	Stdout []byte
	// Stderr is what the command wrote to standard error, truncated to 64 KiB.
	Stderr []byte
}

// Error returns "gluster on gluster-1 exited with status 1: <stderr>". The
// stderr part is trimmed, has its control characters replaced by spaces and is
// cut to 256 bytes, so that a broken command cannot flood a log line.
func (e *ExitError) Error() string {
	msg := fmt.Sprintf("%s on %s exited with status %d", e.Program, e.Host, e.Code)
	if stderr := exitStderrMessage(e.Stderr); stderr != "" {
		msg += ": " + stderr
	}
	return msg
}

// exitStderrMessage renders stderr for the message of an [ExitError]:
// surrounding white space is dropped, control characters become spaces and the
// result is cut to [maxExitStderrMessage] bytes followed by "...".
func exitStderrMessage(stderr []byte) string {
	truncated := len(stderr) > maxExitStderrMessage
	if truncated {
		stderr = stderr[:maxExitStderrMessage]
	}
	sanitized := make([]byte, 0, len(stderr))
	for _, b := range stderr {
		if isControlByte(b) {
			sanitized = append(sanitized, ' ')
			continue
		}
		sanitized = append(sanitized, b)
	}
	// The cut may have split a multi-byte character; drop the partial rune and
	// any other invalid UTF-8 so that the message stays printable.
	msg := strings.TrimSpace(strings.ToValidUTF8(string(sanitized), ""))
	if truncated {
		msg += "..."
	}
	return msg
}

// isControlByte reports whether b is an ASCII control character.
func isControlByte(b byte) bool { return b < 0x20 || b == 0x7f }

// validateCommand checks that host and args may be sent to a server. It runs
// before any network operation and wraps [ErrInvalidCommand] on every failure.
func validateCommand(host string, args []string) error {
	if err := validateHost(host); err != nil {
		return err
	}
	if len(args) == 0 {
		return fmt.Errorf("%w: empty argument list", ErrInvalidCommand)
	}
	if !allowedPrograms[args[0]] {
		return fmt.Errorf("%w: program %s is not allowlisted", ErrInvalidCommand, quoteValue(args[0]))
	}
	for i, arg := range args {
		if err := validateArgument(arg); err != nil {
			return fmt.Errorf("%w: argument %d: %s", ErrInvalidCommand, i, err)
		}
	}
	if n := len(shellQuote(args)); n > MaxCommandLength {
		return fmt.Errorf("%w: quoted command line is %d bytes, limit is %d",
			ErrInvalidCommand, n, MaxCommandLength)
	}
	return nil
}

// quoteValue renders a value for an error message. Very long values are replaced
// by their length, so that a huge argument never ends up in a log line.
func quoteValue(v string) string {
	const maxEcho = 64
	if len(v) > maxEcho {
		return fmt.Sprintf("value of length %d", len(v))
	}
	return fmt.Sprintf("%q", v)
}

// validateHost checks the host name. The syntax is not validated any further
// because pkg/config already validated the servers of the ConfigMap; what is
// checked here is that the value cannot change the meaning of the SSH address
// or of an error message.
func validateHost(host string) error {
	switch {
	case host == "":
		return fmt.Errorf("%w: empty host", ErrInvalidCommand)
	case len(host) > maxHostLength:
		return fmt.Errorf("%w: host name is %d bytes, limit is %d",
			ErrInvalidCommand, len(host), maxHostLength)
	case !utf8.ValidString(host):
		return fmt.Errorf("%w: host is not valid UTF-8", ErrInvalidCommand)
	}
	for _, r := range host {
		switch r {
		case '@':
			return fmt.Errorf("%w: host must not contain %q", ErrInvalidCommand, "@")
		case '/':
			return fmt.Errorf("%w: host must not contain %q", ErrInvalidCommand, "/")
		case '\\':
			return fmt.Errorf("%w: host must not contain %q", ErrInvalidCommand, `\`)
		}
		if r <= ' ' || r == 0x7f {
			return fmt.Errorf("%w: host must not contain white space or control characters",
				ErrInvalidCommand)
		}
	}
	return nil
}

// validateArgument checks one argument of a command. Every argument is
// shell-quoted on the remote side, so the check is about hygiene and not about
// shell metacharacters: a control character could inject a line into a terminal
// or a log, and an invalid UTF-8 sequence would reach the shell as garbage. An
// empty argument is valid.
func validateArgument(arg string) error {
	if !utf8.ValidString(arg) {
		return errors.New("argument is not valid UTF-8")
	}
	for i := 0; i < len(arg); i++ {
		if b := arg[i]; isControlByte(b) {
			return fmt.Errorf("argument must not contain control characters (byte 0x%02x at offset %d)", b, i)
		}
	}
	return nil
}

// limitedWriter collects at most limit bytes of stdout and fails once a write
// would exceed the limit. A failed copy alone does not stop the remote command:
// the ssh package stops reading the channel, the flow control window fills up
// and the command blocks in its next write, so Wait would never return. The
// first write above the limit therefore calls onExceed, which closes the
// connection.
type limitedWriter struct {
	buf      bytes.Buffer
	limit    int
	exceeded bool
	onExceed func()
}

// Write appends p to the buffer, or fails without writing anything if the result
// would exceed the limit.
func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.buf.Len()+len(p) > w.limit {
		if !w.exceeded && w.onExceed != nil {
			w.onExceed()
		}
		w.exceeded = true
		return 0, fmt.Errorf("%w (limit %d bytes)", errOutputLimit, w.limit)
	}
	return w.buf.Write(p)
}

// Exceeded reports whether a write above the limit was attempted. It may only be
// called after the writing goroutine finished (after session.Wait returned).
func (w *limitedWriter) Exceeded() bool { return w.exceeded }

// Bytes returns the collected bytes.
func (w *limitedWriter) Bytes() []byte { return w.buf.Bytes() }

// truncateOutput caps b at limit bytes. It is used for the stderr of an
// [ExitError], which is kept but not displayed in full.
func truncateOutput(b []byte, limit int) []byte {
	if len(b) <= limit {
		return b
	}
	truncated := make([]byte, limit)
	copy(truncated, b)
	return truncated
}
