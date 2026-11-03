package gluster

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"k8s.io/klog/v2"
)

// The sentinels below classify a gluster command that ran and answered with a
// non-zero opRet. They are the only vocabulary this package offers: mapping them
// to CSI error codes is the job of the RPC layer (T-17, T-18).
var (
	// ErrVolumeNotFound means the volume does not exist.
	ErrVolumeNotFound = errors.New("gluster volume does not exist")
	// ErrVolumeExists means a volume with that name already exists.
	ErrVolumeExists = errors.New("gluster volume already exists")
	// ErrVolumeNotStarted means the volume exists but is not started.
	ErrVolumeNotStarted = errors.New("gluster volume is not started")
	// ErrVolumeStarted means the volume is started, which blocks its deletion.
	ErrVolumeStarted = errors.New("gluster volume is started")
	// ErrBrickInUse means a brick overlaps a brick of an existing volume.
	ErrBrickInUse = errors.New("brick is not available")
	// ErrUnknownOption means the gluster release does not know the volume option.
	ErrUnknownOption = errors.New("unknown gluster volume option")
	// ErrQuotaNotEnabled means quota is disabled on the volume.
	ErrQuotaNotEnabled = errors.New("gluster quota is not enabled")
	// ErrPathNotFound means the directory does not exist in the Gluster volume.
	ErrPathNotFound = errors.New("directory does not exist in the gluster volume")
	// ErrUnexpectedOutput means the answer of the gluster CLI could not be parsed
	// or contradicts itself, so no conclusion about the state of the cluster can
	// be drawn from it.
	ErrUnexpectedOutput = errors.New("unexpected gluster output")
)

// The sentinels below are the "state already reached" cases of D-35. They are not
// exported because no caller outside this package may see them: the method that
// caused them turns them into success. They are still sentinels so that
// [CommandError.Unwrap] says exactly what went wrong for a log line, and so that
// another method receives an ordinary [CommandError] and does not swallow it.
var (
	// errAlreadyStarted is the answer of `volume start` to a started volume.
	errAlreadyStarted = errors.New("gluster volume is already started")
	// errAlreadyStopped is the answer of `volume stop` to a volume that is not in
	// the started state.
	errAlreadyStopped = errors.New("gluster volume is already stopped")
	// errQuotaAlreadyEnabled is the answer of `volume quota enable` to a volume
	// whose quota is already enabled.
	errQuotaAlreadyEnabled = errors.New("gluster quota is already enabled")
	// errNoQuotaConfigured is the answer of `volume quota list` to a volume without
	// any quota limit.
	errNoQuotaConfigured = errors.New("gluster volume has no quota configured")
	// errNoQuotaLimit is the answer of `volume quota remove` to a directory without
	// a quota limit.
	errNoQuotaLimit = errors.New("gluster path has no quota limit")
)

// errstrKind is one row of the classification table: a pattern that is matched
// against the whole opErrstr, and the sentinel it maps to.
type errstrKind struct {
	// pattern must match the entire opErrstr; only firstLinePattern looks at the
	// first line alone.
	pattern *regexp.Regexp
	// kind is the sentinel the row maps to.
	kind error
	// firstLine makes the pattern match against the first line of opErrstr only.
	// The unknown-option answer carries a "Did you mean ..." hint on its second
	// line, which differs between releases and must not affect the verdict.
	firstLine bool
}

// errstrKinds is the classification table of D-35. Every pattern is anchored at
// both ends, so a message that merely contains one of these sentences, such as the
// cluster-wide "Staging failed on h2. Error: Volume foo does not exist", is not
// classified. The texts were verified on 9.2, 10.3 and 11.1 and are identical
// there.
var errstrKinds = []errstrKind{
	{pattern: regexp.MustCompile(`^Volume does not exist$`), kind: ErrVolumeNotFound},
	{pattern: regexp.MustCompile(`^Volume \S+ does not exist$`), kind: ErrVolumeNotFound},
	{pattern: regexp.MustCompile(`^Volume \S+ already exists$`), kind: ErrVolumeExists},
	{pattern: regexp.MustCompile(`^Brick: \S+ not available\. Brick may be containing or be contained by an existing brick\.$`),
		kind: ErrBrickInUse},
	{pattern: regexp.MustCompile(`^Volume \S+ is not started$`), kind: ErrVolumeNotStarted},
	// gluster 9.2, 10.3 and 11.1 print "started.Volume" without a space, so \s*
	// keeps the pattern working if a release adds one.
	{pattern: regexp.MustCompile(`^Volume \S+ has been started\.\s*Volume needs to be stopped before deletion\.$`),
		kind: ErrVolumeStarted},
	{pattern: regexp.MustCompile(`^option : \S+ does not exist$`), kind: ErrUnknownOption, firstLine: true},
	{pattern: regexp.MustCompile(`^Quota is disabled, please enable quota$`), kind: ErrQuotaNotEnabled},
	{pattern: regexp.MustCompile(`^Failed to get trusted\.gfid attribute on path \S+\. Reason : No such file or directory$`),
		kind: ErrPathNotFound},
	{pattern: regexp.MustCompile(`^Volume \S+ already started$`), kind: errAlreadyStarted},
	{pattern: regexp.MustCompile(`^Volume \S+ is not in the started state$`), kind: errAlreadyStopped},
	{pattern: regexp.MustCompile(`^Quota is already enabled$`), kind: errQuotaAlreadyEnabled},
	{pattern: regexp.MustCompile(`^No quota configured on volume \S+$`), kind: errNoQuotaConfigured},
	{pattern: regexp.MustCompile(`^removexattr failed on \S+\. Reason : No data available$`), kind: errNoQuotaLimit},
}

// classify maps an opErrstr to a sentinel from errstrKinds, or nil when the text
// is not one of the known answers. The errno is deliberately not consulted: it is
// 30800 for almost every failure.
func classify(errstr string) error {
	trimmed := strings.TrimSpace(errstr)
	if trimmed == "" {
		return nil
	}
	firstLine, _, _ := strings.Cut(trimmed, "\n")
	for _, row := range errstrKinds {
		subject := trimmed
		if row.firstLine {
			subject = strings.TrimSpace(firstLine)
		}
		if row.pattern.MatchString(subject) {
			return row.kind
		}
	}
	return nil
}

// CommandError reports a gluster command that ran and answered with a non-zero
// opRet. It is never used for a command that did not run: those failures wrap
// [ErrConnect] or are a context error.
//
// The error unwraps to the sentinel of the classification table, so
// errors.Is(err, ErrVolumeNotFound) works, and to nil when the text was not
// recognised. The errno is carried for the log line only.
type CommandError struct {
	// Op is the subcommand, for example "volume start". It never contains an
	// argument value.
	Op string
	// Volume is the volume the command was about.
	Volume string
	// Host is the server that ran the command.
	Host string
	// Ret is the opRet of the answer.
	Ret int
	// Errno is the opErrno of the answer; it is informational.
	Errno int
	// Errstr is the opErrstr of the answer, verbatim.
	Errstr string
	// kind is the sentinel the opErrstr was classified into, or nil.
	kind error
}

// Error returns "gluster volume start t16vol on gfs-1: Volume t16vol already
// started (opRet -1, opErrno 30800)". The errstr is cleaned and truncated like the
// stderr of an [ExitError], because it may span several lines.
func (e *CommandError) Error() string {
	subject := "gluster " + e.Op
	if e.Volume != "" {
		subject += " " + cleanValue(e.Volume)
	}
	msg := fmt.Sprintf("%s on %s", subject, e.Host)
	if errstr := exitStderrMessage([]byte(e.Errstr)); errstr != "" {
		msg += ": " + errstr
	}
	return fmt.Sprintf("%s (opRet %d, opErrno %d)", msg, e.Ret, e.Errno)
}

// Unwrap returns the sentinel of the classification, or nil when the answer was not
// recognised.
func (e *CommandError) Unwrap() error { return e.kind }

// cleanValue renders a value from the gluster output for an error message: control
// characters become spaces, invalid UTF-8 is dropped and the result is cut to 256
// bytes, exactly like the stderr of an [ExitError]. gluster output is never
// trusted, so no value from it is ever echoed in full.
func cleanValue(value string) string {
	return exitStderrMessage([]byte(value))
}

// Client runs gluster management commands on the servers of one cluster.
//
// Every command is executed as `gluster --mode=script --xml ...` through
// [Failover], so a server that cannot be reached is replaced by the next one and
// the command is never executed twice. The result is judged by the opRet of the
// XML answer and never by the exit status. A Client has no mutable state apart
// from the memo of the Failover and may be used concurrently.
type Client struct {
	failover *Failover
	hosts    []string
}

// NewClient returns a Client that runs the commands through executor on hosts, in
// the failover order of [Failover].
//
// The hosts are validated before anything is sent, so a typo in the configuration
// is reported once instead of on every call; every failure wraps
// [ErrInvalidCommand].
func NewClient(executor Executor, hosts []string) (*Client, error) {
	if executor == nil {
		return nil, fmt.Errorf("%w: an executor is required", ErrInvalidCommand)
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("%w: no glusterfs servers", ErrInvalidCommand)
	}
	for _, host := range hosts {
		if err := validateHost(host); err != nil {
			return nil, err
		}
	}
	return &Client{
		// daemonCheck turns "glusterd is not running" into ErrConnect, so that a
		// server whose glusterd is down is skipped like an unreachable one.
		failover: &Failover{Executor: daemonCheck{executor}},
		hosts:    slices.Clone(hosts),
	}, nil
}

// glusterArgs builds the argv of one gluster command. --mode=script keeps stop and
// delete from asking a question, --xml selects the machine readable answer.
func glusterArgs(args []string) []string {
	argv := make([]string, 0, len(args)+3)
	argv = append(argv, "gluster", "--mode=script", "--xml")
	return append(argv, args...)
}

// run executes one gluster command and returns its parsed answer together with the
// host that ran it.
//
// Success and failure are decided by the opRet of the XML answer only. A non-zero
// exit status is not an error by itself: gluster answers with a complete XML
// document and a non-zero opRet in that case, and `volume quota list` even exits
// with status 2 when quota is disabled. Only an exit whose stdout is not a
// gluster answer is reported as the error it came with, so that the caller keeps
// the [ExitError], [ErrConnect] or context error.
//
// op is the subcommand without argument values; it only appears in error messages.
func (c *Client) run(ctx context.Context, op string, args ...string) (*cliOutput, string, error) {
	stdout, host, err := c.failover.Run(ctx, c.hosts, glusterArgs(args)...)
	// exitCode is the status of a command that ran and exited non-zero. It is never
	// used to decide success: only opRet is. It is kept so that a non-zero exit with
	// an answer that claims opRet 0 can be reported as the contradiction it is.
	exitCode := 0
	if err != nil {
		var exitErr *ExitError
		if !errors.As(err, &exitErr) {
			// Nothing ran, or the transport failed: ErrConnect, *AllHostsFailedError,
			// a context error and ErrInvalidCommand stay reachable through errors.Is.
			return nil, host, fmt.Errorf("gluster %s: %w", op, err)
		}
		// The command ran and exited non-zero. That alone says nothing: gluster
		// reports almost every failure with exit status 0 and a non-zero opRet, and
		// `volume quota <vol> list` exits with status 2 while printing a complete
		// answer. So the answer is judged like any other; only when stdout is not a
		// gluster answer at all is the exit status the cause.
		if !isCLIOutputDocument(exitErr.Stdout) {
			return nil, host, fmt.Errorf("gluster %s: %w", op, err)
		}
		stdout = exitErr.Stdout
		exitCode = exitErr.Code
	}

	output, parseErr := parseCLIOutput(op, host, stdout)
	if parseErr != nil {
		return nil, host, parseErr
	}
	ret, err := output.ret(op, host)
	if err != nil {
		return nil, host, err
	}
	if ret != 0 {
		return nil, host, newCommandError(op, host, args, output, ret)
	}
	if exitCode != 0 {
		return nil, host, unexpectedf(op, host, "the command exited with status %d although opRet is 0", exitCode)
	}
	return output, host, nil
}

// isCLIOutputDocument reports whether stdout is a complete --xml answer of the
// gluster CLI, that is an XML document whose root element is <cliOutput>. It is the
// predicate that decides whether the stdout of an exit with a non-zero status is
// judged by its opRet (see [Client.run]); it does not require an opRet, because an
// answer without one is still an answer that [parseCLIOutput] rejects.
func isCLIOutputDocument(stdout []byte) bool {
	if len(strings.TrimSpace(string(stdout))) == 0 {
		return false
	}
	var output cliOutput
	return xml.Unmarshal(stdout, &output) == nil
}

// newCommandError builds the typed error of a command that answered with a
// non-zero opRet.
func newCommandError(op, host string, args []string, output *cliOutput, ret int) *CommandError {
	return &CommandError{
		Op:     op,
		Volume: volumeOfArgs(args),
		Host:   host,
		Ret:    ret,
		Errno:  output.errno(),
		Errstr: output.OpErrstr,
		kind:   classify(output.OpErrstr),
	}
}

// volumeOfArgs returns the volume name of a gluster argv, or "".
//
// Every command this package runs names the volume as its third argument (after
// "volume" and the subcommand, or after "volume" and "quota"); the only one
// without a volume is `volume info`, which lists all of them. The value is only
// used in the message of a [CommandError].
func volumeOfArgs(args []string) string {
	const volumeIndex = 2
	if len(args) <= volumeIndex {
		return ""
	}
	return args[volumeIndex]
}

// logSuccess records a successful change at -v=2. Read-only commands are not
// logged here: the RPC layer logs the request.
func logSuccess(op, volume, host string) {
	klog.V(2).InfoS("ran a gluster command", "op", op, "volume", volume, "host", host)
}
