package gluster

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
)

// staticExecutor answers every call with the same stdout and error and records the
// calls it received. It is the synthetic counterpart of the golden replayExecutor:
// it can produce answers that no gluster release ever printed.
type staticExecutor struct {
	mu     sync.Mutex
	stdout []byte
	err    error
	hosts  []string
	argvs  [][]string
}

// Run records the call and returns the configured answer.
func (e *staticExecutor) Run(_ context.Context, host string, args ...string) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.hosts = append(e.hosts, host)
	e.argvs = append(e.argvs, slices.Clone(args))
	return e.stdout, e.err
}

// Calls returns the recorded hosts and argvs.
func (e *staticExecutor) Calls() ([]string, [][]string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.hosts), cloneArgvs(e.argvs)
}

// newStaticClient returns a Client whose single host answers with stdout and err.
func newStaticClient(t *testing.T, stdout string, err error) (*Client, *staticExecutor) {
	t.Helper()
	executor := &staticExecutor{stdout: []byte(stdout), err: err}
	client, clientErr := NewClient(executor, []string{"gluster-1"})
	if clientErr != nil {
		t.Fatalf("NewClient: %v", clientErr)
	}
	return client, executor
}

// cliAnswer builds a minimal gluster answer with the given opRet, opErrno and
// opErrstr and the given payload (which may be empty).
func cliAnswer(opRet, opErrno, opErrstr, payload string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<cliOutput><opRet>%s</opRet><opErrno>%s</opErrno><opErrstr>%s</opErrstr>%s</cliOutput>`,
		opRet, opErrno, opErrstr, payload)
}

// okAnswer is a successful answer without a payload.
func okAnswer() string { return cliAnswer("0", "0", "", "") }

// TestClassify checks the opErrstr classification table row by row.
func TestClassify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		errstr  string
		want    error
		wantNil bool
	}{
		{name: "volume does not exist", errstr: "Volume does not exist", want: ErrVolumeNotFound},
		{name: "named volume does not exist", errstr: "Volume nosuch does not exist", want: ErrVolumeNotFound},
		{name: "volume already exists", errstr: "Volume t16vol already exists", want: ErrVolumeExists},
		{
			name:   "brick is not available",
			errstr: "Brick: gfs-golden:/bricks/b1/t16vol not available. Brick may be containing or be contained by an existing brick.",
			want:   ErrBrickInUse,
		},
		{name: "volume is not started", errstr: "Volume t16vol is not started", want: ErrVolumeNotStarted},
		{
			name:   "volume has been started",
			errstr: "Volume t16one has been started.Volume needs to be stopped before deletion.",
			want:   ErrVolumeStarted,
		},
		{
			name:   "volume has been started with a space",
			errstr: "Volume t16one has been started. Volume needs to be stopped before deletion.",
			want:   ErrVolumeStarted,
		},
		{name: "unknown option", errstr: "option : no.such.option does not exist", want: ErrUnknownOption},
		{
			name:   "unknown option with a hint on the second line",
			errstr: "option : no.such.option does not exist\nDid you mean nfs.drc-size or ...mem-factor?",
			want:   ErrUnknownOption,
		},
		{
			name:   "unknown option with any second line",
			errstr: "option : no.such.option does not exist\nsomething else entirely",
			want:   ErrUnknownOption,
		},
		{
			name:   "quota is disabled",
			errstr: "Quota is disabled, please enable quota",
			want:   ErrQuotaNotEnabled,
		},
		{
			name:   "quota path does not exist",
			errstr: "Failed to get trusted.gfid attribute on path /nosuch. Reason : No such file or directory",
			want:   ErrPathNotFound,
		},
		{name: "volume already started", errstr: "Volume t16vol already started", want: errAlreadyStarted},
		{name: "volume is not started (stop)", errstr: "Volume t16vol is not in the started state", want: errAlreadyStopped},
		{name: "quota already enabled", errstr: "Quota is already enabled", want: errQuotaAlreadyEnabled},
		{name: "no quota configured", errstr: "No quota configured on volume t16vol", want: errNoQuotaConfigured},
		{
			name:   "no quota limit",
			errstr: "removexattr failed on /var/run/gluster/t16vol_quota_limit/d2. Reason : No data available",
			want:   errNoQuotaLimit,
		},

		// Positive matches must survive surrounding white space, because gluster
		// writes the text into an XML element that may be indented.
		{name: "leading and trailing white space", errstr: "  Volume nosuch does not exist\n", want: ErrVolumeNotFound},

		// The patterns are anchored, so a longer message is not classified. A
		// cluster wide inconsistency that quotes the sentence must not be read as
		// "this volume does not exist".
		{
			name:    "sentence embedded in another message",
			errstr:  "Staging failed on h2. Error: Volume foo does not exist",
			wantNil: true,
		},
		{name: "trailing text", errstr: "Volume foo does not exist on h2", wantNil: true},
		{name: "leading text on its own line", errstr: "Error:\nVolume foo does not exist", wantNil: true},
		{name: "unknown text", errstr: "something went wrong", wantNil: true},
		{name: "empty text", errstr: "", wantNil: true},
		{name: "only white space", errstr: " \n\t ", wantNil: true},

		// Only the first line is compared for the unknown option rule, and only
		// there: a second line may not make another rule match either.
		{name: "unknown option embedded later", errstr: "Failed.\noption : a does not exist", wantNil: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classify(tc.errstr)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("classify(%q) = %v, want nil", tc.errstr, got)
				}
				return
			}
			if !errors.Is(got, tc.want) {
				t.Fatalf("classify(%q) = %v, want %v", tc.errstr, got, tc.want)
			}
		})
	}
}

// TestClassifyReturnsExactlyOneSentinel makes sure a classification is a single
// sentinel and not a chain that would make an unrelated errors.Is succeed.
func TestClassifyReturnsExactlyOneSentinel(t *testing.T) {
	t.Parallel()

	kinds := []error{
		ErrVolumeNotFound, ErrVolumeExists, ErrVolumeNotStarted, ErrVolumeStarted,
		ErrBrickInUse, ErrUnknownOption, ErrQuotaNotEnabled, ErrPathNotFound,
		errAlreadyStarted, errAlreadyStopped, errQuotaAlreadyEnabled,
		errNoQuotaConfigured, errNoQuotaLimit,
	}
	for _, tc := range []struct {
		errstr string
		want   error
	}{
		{"Volume nosuch does not exist", ErrVolumeNotFound},
		{"Quota is already enabled", errQuotaAlreadyEnabled},
	} {
		got := classify(tc.errstr)
		for _, kind := range kinds {
			want := errors.Is(kind, tc.want)
			if have := errors.Is(got, kind); have != want {
				t.Errorf("errors.Is(classify(%q), %v) = %v, want %v", tc.errstr, kind, have, want)
			}
		}
	}
}

// TestCommandErrorMessage checks the text of a CommandError.
func TestCommandErrorMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  CommandError
		want []string
		not  []string
	}{
		{
			name: "typed failure",
			err: CommandError{
				Op: "volume start", Volume: "t16vol", Host: "dxt-gluster-node-1",
				Ret: -1, Errno: 30800, Errstr: "Volume t16vol already started",
			},
			want: []string{
				"gluster volume start t16vol on dxt-gluster-node-1",
				"Volume t16vol already started",
				"(opRet -1, opErrno 30800)",
			},
		},
		{
			name: "opRet -2",
			err: CommandError{
				Op: "volume quota limit-usage", Volume: "t16vol", Host: "gluster-1",
				Ret: -2, Errno: 30800,
				Errstr: "Failed to get trusted.gfid attribute on path /nosuch. Reason : No such file or directory",
			},
			want: []string{"opRet -2", "opErrno 30800", "trusted.gfid"},
		},
		{
			name: "without a volume",
			err:  CommandError{Op: "volume info", Host: "gluster-1", Ret: -1, Errno: 30806},
			want: []string{"gluster volume info on gluster-1", "(opRet -1, opErrno 30806)"},
		},
		{
			name: "multi line errstr is flattened",
			err: CommandError{
				Op: "volume set", Volume: "t16vol", Host: "gluster-1", Ret: -1, Errno: 30800,
				Errstr: "option : no.such.option does not exist\nDid you mean dht.force-readdirp?",
			},
			want: []string{"option : no.such.option does not exist Did you mean dht.force-readdirp?"},
			not:  []string{"\n"},
		},
		{
			name: "control characters are replaced",
			err: CommandError{
				Op: "volume start", Volume: "t16vol", Host: "gluster-1", Ret: -1,
				Errstr: "bad \x1b[31mvalue\x07 here",
			},
			want: []string{"bad  [31mvalue  here"},
			not:  []string{"\x1b", "\x07"},
		},
		{
			name: "a long errstr is truncated",
			err: CommandError{
				Op: "volume start", Volume: "t16vol", Host: "gluster-1", Ret: -1,
				Errstr: strings.Repeat("x", 4096),
			},
			want: []string{strings.Repeat("x", maxExitStderrMessage) + "..."},
			not:  []string{strings.Repeat("x", maxExitStderrMessage+1)},
		},
		{
			name: "invalid utf-8 is dropped",
			err: CommandError{
				Op: "volume start", Volume: "t16vol", Host: "gluster-1", Ret: -1,
				Errstr: "bad \xff\xfe byte",
			},
			want: []string{"bad  byte"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			msg := tc.err.Error()
			for _, want := range tc.want {
				if !strings.Contains(msg, want) {
					t.Errorf("Error() = %q, want it to contain %q", msg, want)
				}
			}
			for _, not := range tc.not {
				if strings.Contains(msg, not) {
					t.Errorf("Error() = %q, want it not to contain %q", msg, not)
				}
			}
			if len(msg) > 1024 {
				t.Errorf("Error() is %d bytes long, want at most 1024", len(msg))
			}
		})
	}
}

// TestCommandErrorUnwrap checks that Unwrap returns the classification and nothing
// else.
func TestCommandErrorUnwrap(t *testing.T) {
	t.Parallel()

	typed := &CommandError{kind: ErrVolumeNotFound}
	if !errors.Is(typed, ErrVolumeNotFound) {
		t.Error("errors.Is(CommandError{kind: ErrVolumeNotFound}, ErrVolumeNotFound) = false, want true")
	}
	if errors.Is(typed, ErrVolumeExists) {
		t.Error("errors.Is(CommandError{kind: ErrVolumeNotFound}, ErrVolumeExists) = true, want false")
	}

	untyped := &CommandError{}
	if got := untyped.Unwrap(); got != nil {
		t.Errorf("Unwrap() = %v, want nil for an unclassified answer", got)
	}
	for _, kind := range []error{ErrVolumeNotFound, ErrVolumeExists, ErrUnexpectedOutput, ErrConnect} {
		if errors.Is(untyped, kind) {
			t.Errorf("an unclassified CommandError matches %v, want no sentinel", kind)
		}
	}
}

// TestClientResultVerdict checks how the verdict is taken from the answer.
func TestClientResultVerdict(t *testing.T) {
	t.Parallel()

	volInfoEmpty := `<volInfo><volumes><count>0</count></volumes></volInfo>`
	tests := []struct {
		name    string
		stdout  string
		err     error
		want    func(*testing.T, error)
		noCalls int
	}{
		{
			name:   "opRet 0 is success",
			stdout: cliAnswer("0", "0", "", volInfoEmpty),
			want: func(t *testing.T, err error) {
				t.Helper()
				if err != nil {
					t.Fatalf("call = %v, want nil", err)
				}
			},
		},
		{
			name:   "opRet -1 is a failure",
			stdout: cliAnswer("-1", "30800", "Volume does not exist", volInfoEmpty),
			want: func(t *testing.T, err error) {
				t.Helper()
				if !errors.Is(err, ErrVolumeNotFound) {
					t.Fatalf("call = %v, want ErrVolumeNotFound", err)
				}
			},
		},
		{
			name:   "opRet -2 is a failure",
			stdout: cliAnswer("-2", "30800", "Failed to get trusted.gfid attribute on path /nosuch. Reason : No such file or directory", ""),
			want: func(t *testing.T, err error) {
				t.Helper()
				var commandErr *CommandError
				if !errors.As(err, &commandErr) {
					t.Fatalf("call = %v, want *CommandError", err)
				}
				if commandErr.Ret != -2 {
					t.Errorf("Ret = %d, want -2", commandErr.Ret)
				}
				if !errors.Is(err, ErrPathNotFound) {
					t.Errorf("call = %v, want ErrPathNotFound", err)
				}
			},
		},
		{
			name:   "opRet 1 is a failure",
			stdout: cliAnswer("1", "0", "", volInfoEmpty),
			want: func(t *testing.T, err error) {
				t.Helper()
				var commandErr *CommandError
				if !errors.As(err, &commandErr) {
					t.Fatalf("call = %v, want *CommandError", err)
				}
				if commandErr.kind != nil {
					t.Errorf("kind = %v, want nil for an unrecognised opErrstr", commandErr.kind)
				}
			},
		},
		{
			name:   "unrecognised opErrstr keeps the message",
			stdout: cliAnswer("-1", "30800", "Staging failed on h2. Error: Volume foo does not exist", ""),
			want: func(t *testing.T, err error) {
				t.Helper()
				var commandErr *CommandError
				if !errors.As(err, &commandErr) {
					t.Fatalf("call = %v, want *CommandError", err)
				}
				if commandErr.kind != nil {
					t.Errorf("kind = %v, want nil: the sentence is only part of the message", commandErr.kind)
				}
				if !strings.Contains(err.Error(), "Staging failed on h2") {
					t.Errorf("call = %v, want the original opErrstr", err)
				}
			},
		},
		{
			name:   "missing opRet",
			stdout: "<cliOutput><opErrno>0</opErrno></cliOutput>",
			want: func(t *testing.T, err error) {
				t.Helper()
				if !errors.Is(err, ErrUnexpectedOutput) {
					t.Fatalf("call = %v, want ErrUnexpectedOutput", err)
				}
			},
		},
		{
			name:   "opRet is not a number",
			stdout: cliAnswer("zero", "0", "", ""),
			want: func(t *testing.T, err error) {
				t.Helper()
				if !errors.Is(err, ErrUnexpectedOutput) {
					t.Fatalf("call = %v, want ErrUnexpectedOutput", err)
				}
			},
		},
		{
			name:   "wrong root element",
			stdout: "<somethingElse><opRet>0</opRet></somethingElse>",
			want: func(t *testing.T, err error) {
				t.Helper()
				if !errors.Is(err, ErrUnexpectedOutput) {
					t.Fatalf("call = %v, want ErrUnexpectedOutput", err)
				}
			},
		},
		{
			name:   "not xml at all",
			stdout: "volume info: no volumes",
			want: func(t *testing.T, err error) {
				t.Helper()
				if !errors.Is(err, ErrUnexpectedOutput) {
					t.Fatalf("call = %v, want ErrUnexpectedOutput", err)
				}
			},
		},
		{
			name:   "empty stdout",
			stdout: "",
			want: func(t *testing.T, err error) {
				t.Helper()
				if !errors.Is(err, ErrUnexpectedOutput) {
					t.Fatalf("call = %v, want ErrUnexpectedOutput", err)
				}
			},
		},
		{
			name:   "only white space on stdout",
			stdout: " \n\t ",
			want: func(t *testing.T, err error) {
				t.Helper()
				if !errors.Is(err, ErrUnexpectedOutput) {
					t.Fatalf("call = %v, want ErrUnexpectedOutput", err)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client, _ := newStaticClient(t, tc.stdout, tc.err)
			_, err := client.Volumes(context.Background())
			tc.want(t, err)
		})
	}
}

// TestClientUnexpectedOutputDoesNotEchoTheOutput checks that the message of an
// unparsable answer is short and does not contain the answer itself: gluster output
// is untrusted and may be megabytes long.
func TestClientUnexpectedOutputDoesNotEchoTheOutput(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		stdout string
	}{
		{name: "garbage", stdout: strings.Repeat("garbage ", 4096)},
		{name: "xml without opRet", stdout: "<cliOutput>" + strings.Repeat("<x>y</x>", 4096) + "</cliOutput>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client, _ := newStaticClient(t, tc.stdout, nil)
			_, err := client.Volumes(context.Background())
			if !errors.Is(err, ErrUnexpectedOutput) {
				t.Fatalf("call = %v, want ErrUnexpectedOutput", err)
			}
			if len(err.Error()) > 1024 {
				t.Errorf("the message is %d bytes long, want at most 1024", len(err.Error()))
			}
			if strings.Contains(err.Error(), "garbage garbage") {
				t.Errorf("the message contains the output: %q", err.Error())
			}
		})
	}
}

// TestClientExitErrorVerdict checks how the stdout of a non-zero exit is used.
func TestClientExitErrorVerdict(t *testing.T) {
	t.Parallel()

	exitErr := func(code int, stdout, stderr string) *ExitError {
		return &ExitError{Host: "gluster-1", Program: "gluster", Code: code, Stdout: []byte(stdout), Stderr: []byte(stderr)}
	}
	tests := []struct {
		name string
		err  *ExitError
		want func(*testing.T, error)
	}{
		{
			name: "exit 2 with a complete answer is judged by the opRet",
			err:  exitErr(2, cliAnswer("-1", "30800", "Quota is disabled, please enable quota", ""), ""),
			want: func(t *testing.T, err error) {
				t.Helper()
				if !errors.Is(err, ErrQuotaNotEnabled) {
					t.Fatalf("call = %v, want ErrQuotaNotEnabled", err)
				}
			},
		},
		{
			name: "exit 1 with a complete answer is judged by the opRet",
			err:  exitErr(1, cliAnswer("-1", "30800", "Volume nosuch does not exist", ""), ""),
			want: func(t *testing.T, err error) {
				t.Helper()
				if !errors.Is(err, ErrVolumeNotFound) {
					t.Fatalf("call = %v, want ErrVolumeNotFound", err)
				}
			},
		},
		{
			name: "exit 1 with opRet 0 is unexpected output",
			err:  exitErr(1, okAnswer(), ""),
			want: func(t *testing.T, err error) {
				t.Helper()
				if !errors.Is(err, ErrUnexpectedOutput) {
					t.Fatalf("call = %v, want ErrUnexpectedOutput", err)
				}
				if !strings.Contains(err.Error(), "exited with status 1") {
					t.Errorf("call = %v, want the exit status in the message", err)
				}
			},
		},
		{
			name: "exit 1 without xml keeps the exit error",
			err:  exitErr(1, "", "Please enter an integer value in the range of (1 - 9223372036854775807)"),
			want: func(t *testing.T, err error) {
				t.Helper()
				var got *ExitError
				if !errors.As(err, &got) {
					t.Fatalf("call = %v, want *ExitError", err)
				}
				if got.Code != 1 {
					t.Errorf("Code = %d, want 1", got.Code)
				}
				if errors.Is(err, ErrUnexpectedOutput) {
					t.Error("call wraps ErrUnexpectedOutput, want the ExitError unchanged")
				}
				if !strings.Contains(err.Error(), "Please enter an integer value") {
					t.Errorf("call = %v, want the stderr in the message", err)
				}
			},
		},
		{
			name: "exit 1 with garbage on stdout keeps the exit error",
			err:  exitErr(1, "Usage: volume quota <VOLNAME> ...", ""),
			want: func(t *testing.T, err error) {
				t.Helper()
				var got *ExitError
				if !errors.As(err, &got) {
					t.Fatalf("call = %v, want *ExitError", err)
				}
				if errors.Is(err, ErrUnexpectedOutput) {
					t.Error("call wraps ErrUnexpectedOutput, want the ExitError unchanged")
				}
			},
		},
		{
			name: "exit 1 with an answer without opRet is unexpected output",
			err:  exitErr(1, "<cliOutput><opErrno>0</opErrno></cliOutput>", ""),
			// The root element makes it an answer, so it is judged as one; an answer
			// without opRet cannot be judged and is reported as such.
			want: func(t *testing.T, err error) {
				t.Helper()
				if !errors.Is(err, ErrUnexpectedOutput) {
					t.Fatalf("call = %v, want ErrUnexpectedOutput", err)
				}
				if !strings.Contains(err.Error(), "no opRet") {
					t.Errorf("call = %q, want the missing field", err)
				}
			},
		},
		{
			name: "exit 1 with an XML document of another root element keeps the exit error",
			err:  exitErr(1, "<other><opRet>0</opRet></other>", ""),
			want: func(t *testing.T, err error) {
				t.Helper()
				var got *ExitError
				if !errors.As(err, &got) {
					t.Fatalf("call = %v, want *ExitError", err)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client, _ := newStaticClient(t, "", tc.err)
			_, err := client.Volumes(context.Background())
			tc.want(t, err)
		})
	}
}

// TestClientKeepsTheTransportError checks that the errors of the transport survive a
// Client call, so that the caller can still tell "nothing ran anywhere" from "the
// command failed".
func TestClientKeepsTheTransportError(t *testing.T) {
	t.Parallel()

	connectErr := fmt.Errorf("%w: cannot connect to 10.0.0.13:22", ErrConnect)
	allFailed := &AllHostsFailedError{Errors: []error{connectErr}}
	tests := []struct {
		name string
		err  error
		want func(*testing.T, error)
	}{
		{
			name: "ErrConnect",
			err:  connectErr,
			want: func(t *testing.T, err error) {
				t.Helper()
				if !errors.Is(err, ErrConnect) {
					t.Fatalf("call = %v, want ErrConnect", err)
				}
				var exitErr *ExitError
				if errors.As(err, &exitErr) {
					t.Fatalf("call = %v, must not wrap *ExitError", err)
				}
			},
		},
		{
			name: "AllHostsFailedError",
			err:  allFailed,
			want: func(t *testing.T, err error) {
				t.Helper()
				var got *AllHostsFailedError
				if !errors.As(err, &got) {
					t.Fatalf("call = %v, want *AllHostsFailedError", err)
				}
				if !errors.Is(err, ErrConnect) {
					t.Errorf("call = %v, want ErrConnect as well", err)
				}
			},
		},
		{
			name: "ErrInvalidCommand",
			err:  fmt.Errorf("%w: program sh is not allowlisted", ErrInvalidCommand),
			want: func(t *testing.T, err error) {
				t.Helper()
				if !errors.Is(err, ErrInvalidCommand) {
					t.Fatalf("call = %v, want ErrInvalidCommand", err)
				}
			},
		},
		{
			name: "context cancellation",
			err:  fmt.Errorf("running the command on the glusterfs server was interrupted: %w", context.Canceled),
			want: func(t *testing.T, err error) {
				t.Helper()
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("call = %v, want errors.Is(err, context.Canceled)", err)
				}
				if errors.Is(err, ErrConnect) {
					t.Errorf("call = %v, must not wrap ErrConnect", err)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client, _ := newStaticClient(t, "", tc.err)
			_, err := client.VolumeInfo(context.Background(), "t16vol")
			tc.want(t, err)
		})
	}
}

// TestClientStopsWhenTheContextIsCancelled checks that a cancelled context prevents
// any command from being sent.
func TestClientStopsWhenTheContextIsCancelled(t *testing.T) {
	t.Parallel()

	client, executor := newStaticClient(t, okAnswer(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := client.VolumeStart(ctx, "t16vol"); !errors.Is(err, context.Canceled) {
		t.Fatalf("VolumeStart = %v, want context.Canceled", err)
	}
	if hosts, _ := executor.Calls(); len(hosts) != 0 {
		t.Errorf("the executor was called %v, want no call", hosts)
	}
}

// TestUnexportedSentinelsAreOnlySwallowedInTheirOwnMethod makes sure that the four
// "state already reached" answers only become success in the method that owns them.
func TestUnexportedSentinelsAreOnlySwallowedInTheirOwnMethod(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		errstr string
		call   func(*Client) error
		want   error
	}{
		{
			name:   "already started is not success for delete",
			errstr: "Volume t16vol already started",
			call:   func(c *Client) error { return c.VolumeDelete(context.Background(), "t16vol") },
			want:   errAlreadyStarted,
		},
		{
			name:   "already started is not success for stop",
			errstr: "Volume t16vol already started",
			call:   func(c *Client) error { return c.VolumeStop(context.Background(), "t16vol") },
			want:   errAlreadyStarted,
		},
		{
			name:   "not in the started state is not success for start",
			errstr: "Volume t16vol is not in the started state",
			call:   func(c *Client) error { return c.VolumeStart(context.Background(), "t16vol") },
			want:   errAlreadyStopped,
		},
		{
			name:   "not in the started state is not success for delete",
			errstr: "Volume t16vol is not in the started state",
			call:   func(c *Client) error { return c.VolumeDelete(context.Background(), "t16vol") },
			want:   errAlreadyStopped,
		},
		{
			name:   "quota already enabled is not success for limit-usage",
			errstr: "Quota is already enabled",
			call: func(c *Client) error {
				return c.QuotaLimitUsage(context.Background(), "t16vol", "/d1", 1)
			},
			want: errQuotaAlreadyEnabled,
		},
		{
			name:   "no quota limit is not success for limit-usage",
			errstr: "removexattr failed on /var/run/gluster/t16vol_quota_limit/d2. Reason : No data available",
			call: func(c *Client) error {
				return c.QuotaLimitUsage(context.Background(), "t16vol", "/d1", 1)
			},
			want: errNoQuotaLimit,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client, _ := newStaticClient(t, cliAnswer("-1", "30800", tc.errstr, ""), nil)
			err := tc.call(client)
			if err == nil {
				t.Fatalf("call = nil, want an error wrapping %v", tc.want)
			}
			var commandErr *CommandError
			if !errors.As(err, &commandErr) {
				t.Fatalf("call = %v, want *CommandError", err)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("call = %v, want errors.Is(err, %v)", err, tc.want)
			}
		})
	}
}

// volumeWith builds a complete `volume info` answer for one volume called name with
// extra appended to the volume element. A field that is repeated overrides the
// default value, because encoding/xml keeps the last occurrence.
func volumeWith(name, extra string) string {
	return cliAnswer("0", "0", "", `<volInfo><volumes><count>1</count><volume><name>`+name+`</name>`+
		`<brickCount>1</brickCount><distCount>1</distCount><replicaCount>1</replicaCount>`+
		`<arbiterCount>0</arbiterCount><disperseCount>0</disperseCount><redundancyCount>0</redundancyCount>`+
		extra+`</volume></volumes></volInfo>`)
}

// TestVolumesRejectsBrokenAnswers checks the checks around `volume info`.
func TestVolumesRejectsBrokenAnswers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		stdout  string
		method  func(*Client) error
		wantMsg string
	}{
		{
			name:    "count does not match the number of volumes",
			stdout:  cliAnswer("0", "0", "", `<volInfo><volumes><count>2</count><volume><name>a</name></volume></volumes></volInfo>`),
			method:  func(c *Client) error { _, err := c.Volumes(context.Background()); return err },
			wantMsg: "count is 2 but the output holds 1 volumes",
		},
		{
			name:    "count is missing",
			stdout:  cliAnswer("0", "0", "", `<volInfo><volumes><volume><name>a</name></volume></volumes></volInfo>`),
			method:  func(c *Client) error { _, err := c.Volumes(context.Background()); return err },
			wantMsg: "the field count is empty",
		},
		{
			name:    "count is not a number",
			stdout:  cliAnswer("0", "0", "", `<volInfo><volumes><count>many</count></volumes></volInfo>`),
			method:  func(c *Client) error { _, err := c.Volumes(context.Background()); return err },
			wantMsg: "not an integer",
		},
		{
			name:    "no volInfo element",
			stdout:  okAnswer(),
			method:  func(c *Client) error { _, err := c.Volumes(context.Background()); return err },
			wantMsg: "no volInfo element",
		},
		{
			name:    "no volumes element",
			stdout:  cliAnswer("0", "0", "", `<volInfo/>`),
			method:  func(c *Client) error { _, err := c.Volumes(context.Background()); return err },
			wantMsg: "no volumes element",
		},
		{
			name: "VolumeInfo returns two volumes",
			stdout: cliAnswer("0", "0", "", `<volInfo><volumes><count>2</count>`+
				`<volume><name>a</name></volume><volume><name>b</name></volume></volumes></volInfo>`),
			method:  func(c *Client) error { _, err := c.VolumeInfo(context.Background(), "a"); return err },
			wantMsg: "expected exactly one volume, got 2",
		},
		{
			name:    "VolumeInfo returns the wrong volume",
			stdout:  volumeWith("other", ""),
			method:  func(c *Client) error { _, err := c.VolumeInfo(context.Background(), "a"); return err },
			wantMsg: "asked for volume a but got volume other",
		},
		{
			name: "VolumeInfo with a broken answer",
			stdout: cliAnswer("0", "0", "", `<volInfo><volumes><count>1</count><volume><name>a</name>`+
				`<brickCount>1</brickCount><distCount>1</distCount><replicaCount>x</replicaCount>`+
				`</volume></volumes></volInfo>`),
			method:  func(c *Client) error { _, err := c.VolumeInfo(context.Background(), "a"); return err },
			wantMsg: "the field replicaCount is x",
		},
		{
			name:    "a count field of the volume is not a number",
			stdout:  volumeWith("a", `<replicaCount>three</replicaCount>`),
			method:  func(c *Client) error { _, err := c.Volumes(context.Background()); return err },
			wantMsg: "the field replicaCount is three",
		},
		{
			name:    "a count field of the volume is empty",
			stdout:  volumeWith("a", `<disperseCount></disperseCount>`),
			method:  func(c *Client) error { _, err := c.Volumes(context.Background()); return err },
			wantMsg: "the field disperseCount is empty",
		},
		{
			name:    "a brick name has no separator",
			stdout:  volumeWith("a", `<bricks><brick><name>nopath</name><isArbiter>0</isArbiter></brick></bricks>`),
			method:  func(c *Client) error { _, err := c.Volumes(context.Background()); return err },
			wantMsg: "has no",
		},
		{
			name:    "isArbiter is not a number",
			stdout:  volumeWith("a", `<bricks><brick><name>h:/b</name><isArbiter>x</isArbiter></brick></bricks>`),
			method:  func(c *Client) error { _, err := c.Volumes(context.Background()); return err },
			wantMsg: "the field isArbiter is x",
		},
		{
			name:    "the volume name contains a newline",
			stdout:  volumeWith("a\nb", ""),
			method:  func(c *Client) error { _, err := c.Volumes(context.Background()); return err },
			wantMsg: "contains a control character",
		},
		{
			name: "an option name is too long",
			stdout: volumeWith("a", `<options><option><name>`+strings.Repeat("o", maxTextBytes+1)+
				`</name><value>on</value></option></options>`),
			method:  func(c *Client) error { _, err := c.Volumes(context.Background()); return err },
			wantMsg: "limit is 4096",
		},
		{
			name: "an option value contains a newline",
			stdout: volumeWith("a", `<options><option><name>nfs.disable</name>`+
				`<value>on`+"\n"+`off</value></option></options>`),
			method:  func(c *Client) error { _, err := c.Volumes(context.Background()); return err },
			wantMsg: "contains a control character",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client, _ := newStaticClient(t, tc.stdout, nil)
			err := tc.method(client)
			if !errors.Is(err, ErrUnexpectedOutput) {
				t.Fatalf("call = %v, want ErrUnexpectedOutput", err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("call = %q, want it to contain %q", err, tc.wantMsg)
			}
		})
	}
}

// TestVolumeStatusRejectsAnExtraVolume checks the "exactly one volume" rule of
// `volume status`, which no release prints but a corrupted answer could.
func TestVolumeStatusRejectsAnExtraVolume(t *testing.T) {
	t.Parallel()

	stdout := cliAnswer("0", "0", "", `<volStatus><volumes><volume><volName>t16vol</volName></volume>`+
		`<volume><volName>t16vol</volName></volume></volumes></volStatus>`)
	client, _ := newStaticClient(t, stdout, nil)
	_, err := client.VolumeStatus(context.Background(), "t16vol")
	if !errors.Is(err, ErrUnexpectedOutput) {
		t.Fatalf("VolumeStatus = %v, want ErrUnexpectedOutput", err)
	}
	if !strings.Contains(err.Error(), "expected exactly one volume, got 2") {
		t.Errorf("VolumeStatus = %q, want the number of volumes", err)
	}
}

// TestBrickNameWithANewline checks that the brick name, which ends up in a volume id
// and in a log line, is validated like every other text field of the answer.
func TestBrickNameWithANewline(t *testing.T) {
	t.Parallel()

	client, _ := newStaticClient(t, volumeWith("a",
		"<bricks><brick><name>h:/b\nx</name><isArbiter>0</isArbiter></brick></bricks>"), nil)
	_, err := client.Volumes(context.Background())
	if !errors.Is(err, ErrUnexpectedOutput) {
		t.Fatalf("Volumes = %v, want ErrUnexpectedOutput", err)
	}
	if !strings.Contains(err.Error(), "the field brick name contains a control character") {
		t.Errorf("Volumes = %q, want the field name", err)
	}
}

// TestBrickNameWithAnEmptyPart checks that a brick name without a host or without a
// path is rejected: both are needed to address the brick.
func TestBrickNameWithAnEmptyPart(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"h:/", ":/b"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client, _ := newStaticClient(t, volumeWith("a",
				"<bricks><brick><name>"+name+"</name><isArbiter>0</isArbiter></brick></bricks>"), nil)
			_, err := client.Volumes(context.Background())
			if !errors.Is(err, ErrUnexpectedOutput) {
				t.Fatalf("Volumes = %v, want ErrUnexpectedOutput", err)
			}
			if !strings.Contains(err.Error(), "has an empty host or path") {
				t.Errorf("Volumes = %q, want the reason", err)
			}
		})
	}
}

// TestVolumeStatusRejectsBrokenAnswers checks the checks around `volume status`.
func TestVolumeStatusRejectsBrokenAnswers(t *testing.T) {
	t.Parallel()

	status := func(nodes string) string {
		return cliAnswer("0", "0", "", `<volStatus><volumes><volume><volName>t16vol</volName>`+nodes+
			`</volume></volumes></volStatus>`)
	}
	tests := []struct {
		name    string
		stdout  string
		wantMsg string
	}{
		{
			name:    "no volStatus element",
			stdout:  okAnswer(),
			wantMsg: "no volumes element",
		},
		{
			name:    "the volume name differs",
			stdout:  cliAnswer("0", "0", "", `<volStatus><volumes><volume><volName>other</volName></volume></volumes></volStatus>`),
			wantMsg: "asked for volume t16vol but got volume other",
		},
		{
			name: "status is not a number",
			stdout: status(`<node><hostname>h</hostname><path>/b</path><status>up</status>` +
				`<port>1</port><pid>2</pid></node>`),
			wantMsg: "the field status is up",
		},
		{
			name:    "port is not a number and not N/A",
			stdout:  status(`<node><hostname>h</hostname><path>/b</path><status>1</status><port>many</port></node>`),
			wantMsg: "the field port is many",
		},
		{
			name:    "pid is not a number and not N/A",
			stdout:  status(`<node><hostname>h</hostname><path>/b</path><status>1</status><pid>no</pid></node>`),
			wantMsg: "the field pid is no",
		},
		{
			name: "sizeTotal is not a number",
			stdout: status(`<node><hostname>h</hostname><path>/b</path><status>1</status>` +
				`<sizeTotal>big</sizeTotal></node>`),
			wantMsg: "the field sizeTotal is big",
		},
		{
			name: "sizeFree is negative",
			stdout: status(`<node><hostname>h</hostname><path>/b</path><status>1</status>` +
				`<sizeFree>-1</sizeFree></node>`),
			wantMsg: "the field sizeFree is -1",
		},
		{
			name:    "hostname contains a newline",
			stdout:  status("<node><hostname>h\nx</hostname><path>/b</path><status>1</status></node>"),
			wantMsg: "contains a control character",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client, _ := newStaticClient(t, tc.stdout, nil)
			_, err := client.VolumeStatus(context.Background(), "t16vol")
			if !errors.Is(err, ErrUnexpectedOutput) {
				t.Fatalf("call = %v, want ErrUnexpectedOutput", err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("call = %q, want it to contain %q", err, tc.wantMsg)
			}
		})
	}
}

// TestVolumeStatusParsesABrick checks the positive case with the minimal fields.
func TestVolumeStatusParsesABrick(t *testing.T) {
	t.Parallel()

	stdout := cliAnswer("0", "0", "", `<volStatus><volumes><volume><volName>t16vol</volName>`+
		`<node><hostname>h</hostname><path>/bricks/b1</path><peerid>p</peerid><status>1</status>`+
		`<port>N/A</port><pid>-1</pid><sizeTotal>N/A</sizeTotal></node>`+
		`<node><hostname>Quota Daemon</hostname><path>localhost</path><status>1</status></node>`+
		`</volume></volumes></volStatus>`)
	client, _ := newStaticClient(t, stdout, nil)
	bricks, err := client.VolumeStatus(context.Background(), "t16vol")
	if err != nil {
		t.Fatalf("VolumeStatus = %v, want nil", err)
	}
	if len(bricks) != 1 {
		t.Fatalf("VolumeStatus = %d bricks, want 1 (the daemon is skipped)", len(bricks))
	}
	brick := bricks[0]
	if brick.Port != 0 || brick.PID != 0 || brick.SizeTotal != 0 || brick.SizeFree != 0 {
		t.Errorf("Port, PID, SizeTotal, SizeFree = %d, %d, %d, %d, want 0, 0, 0, 0",
			brick.Port, brick.PID, brick.SizeTotal, brick.SizeFree)
	}
	if !brick.Online || brick.Host != "h" || brick.Path != "/bricks/b1" {
		t.Errorf("brick = %+v, want the brick of the sample", brick)
	}
}

// TestVolumeInfoWithZeroVolumes checks that an answer without a volume is reported
// as a broken answer, not as "the volume does not exist".
func TestVolumeInfoWithZeroVolumes(t *testing.T) {
	t.Parallel()

	client, _ := newStaticClient(t, cliAnswer("0", "0", "", `<volInfo><volumes><count>0</count></volumes></volInfo>`), nil)
	_, err := client.VolumeInfo(context.Background(), "t16vol")
	if !errors.Is(err, ErrUnexpectedOutput) {
		t.Fatalf("VolumeInfo = %v, want ErrUnexpectedOutput", err)
	}
	if errors.Is(err, ErrVolumeNotFound) {
		t.Error("VolumeInfo wraps ErrVolumeNotFound, want a broken answer")
	}
}

// TestQuotaListRejectsBrokenAnswers checks the checks around `volume quota list`.
func TestQuotaListRejectsBrokenAnswers(t *testing.T) {
	t.Parallel()

	limit := func(path, hard, percent, sl string) string {
		return `<limit><path>` + path + `</path><hard_limit>` + hard + `</hard_limit>` +
			`<soft_limit_percent>` + percent + `</soft_limit_percent><soft_limit_value>1</soft_limit_value>` +
			`<used_space>1</used_space><avail_space>1</avail_space><sl_exceeded>` + sl + `</sl_exceeded>` +
			`<hl_exceeded>No</hl_exceeded></limit>`
	}
	quota := func(limits string) string {
		return cliAnswer("0", "0", "", `<volQuota>`+limits+`</volQuota>`)
	}
	tests := []struct {
		name    string
		stdout  string
		dirs    []string
		wantMsg string
	}{
		{
			name:    "a limit without a path",
			stdout:  quota(`<limit><hard_limit>1</hard_limit></limit>`),
			wantMsg: "a limit has no path",
		},
		{
			name:    "a relative path",
			stdout:  quota(limit("d1", "1", "80%", "No")),
			wantMsg: "is not absolute",
		},
		{
			name:    "hard_limit is not a number",
			stdout:  quota(limit("/d1", "lots", "80%", "No")),
			wantMsg: "the field hard_limit is lots",
		},
		{
			name:    "soft_limit_percent has no percent sign",
			stdout:  quota(limit("/d1", "1", "80", "No")),
			wantMsg: "which is not a percentage",
		},
		{
			name:    "soft_limit_percent is above 100",
			stdout:  quota(limit("/d1", "1", "101%", "No")),
			wantMsg: "between 0 and 100",
		},
		{
			name:    "a path contains a newline",
			stdout:  quota(limit("/d1\nx", "1", "80%", "No")),
			wantMsg: "the field path contains a control character",
		},
		{
			name:    "used_space is not a number",
			stdout:  quota(`<limit><path>/d1</path><hard_limit>1</hard_limit><used_space>none</used_space></limit>`),
			wantMsg: "the field used_space is none",
		},
		{
			name:    "used_space is missing",
			stdout:  quota(`<limit><path>/d1</path><hard_limit>1</hard_limit></limit>`),
			wantMsg: "the field used_space is empty",
		},
		{
			name: "hl_exceeded has an impossible value",
			stdout: quota(`<limit><path>/d1</path><hard_limit>1</hard_limit><soft_limit_percent>80%</soft_limit_percent>` +
				`<used_space>1</used_space><sl_exceeded>No</sl_exceeded><hl_exceeded>Sometimes</hl_exceeded></limit>`),
			wantMsg: "the field hl_exceeded is Sometimes",
		},
		{
			name:    "sl_exceeded has an impossible value",
			stdout:  quota(limit("/d1", "1", "80%", "Maybe")),
			wantMsg: "which is not Yes, No or N/A",
		},
		{
			name: "soft_limit_value is not a number",
			stdout: quota(`<limit><path>/d1</path><hard_limit>1</hard_limit><soft_limit_value>x</soft_limit_value>` +
				`<used_space>1</used_space></limit>`),
			wantMsg: "the field soft_limit_value is x",
		},
		{
			name: "avail_space is not a number",
			stdout: quota(`<limit><path>/d1</path><hard_limit>1</hard_limit><avail_space>x</avail_space>` +
				`<used_space>1</used_space></limit>`),
			wantMsg: "the field avail_space is x",
		},
		{
			name:    "a directory that was not requested",
			stdout:  quota(limit("/d9", "1", "80%", "No")),
			dirs:    []string{"/d1"},
			wantMsg: "the directory /d1 has no quota entry",
		},
		{
			name:    "an extra directory on top of the requested one",
			stdout:  quota(limit("/d1", "1", "80%", "No") + limit("/d9", "1", "80%", "No")),
			dirs:    []string{"/d1"},
			wantMsg: "which was not requested",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client, _ := newStaticClient(t, tc.stdout, nil)
			_, err := client.QuotaList(context.Background(), "t16vol", tc.dirs...)
			if err == nil {
				t.Fatal("QuotaList = nil, want an error")
			}
			if !errors.Is(err, ErrUnexpectedOutput) && !errors.Is(err, ErrPathNotFound) {
				t.Fatalf("QuotaList = %v, want ErrUnexpectedOutput or ErrPathNotFound", err)
			}
			if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("QuotaList = %q, want it to contain %q", err, tc.wantMsg)
			}
		})
	}
}

// TestQuotaListMissingDirectory checks that a requested directory without an entry
// is reported as missing and that the message names the first one.
func TestQuotaListMissingDirectory(t *testing.T) {
	t.Parallel()

	stdout := cliAnswer("0", "0", "", `<volQuota><limit><path>/d1</path><hard_limit>1</hard_limit>`+
		`<soft_limit_percent>80%</soft_limit_percent><soft_limit_value>1</soft_limit_value>`+
		`<used_space>1</used_space><avail_space>1</avail_space><sl_exceeded>No</sl_exceeded>`+
		`<hl_exceeded>No</hl_exceeded></limit></volQuota>`)
	client, executor := newStaticClient(t, stdout, nil)
	_, err := client.QuotaList(context.Background(), "t16vol", "/d1", "/nosuch1", "/nosuch2")
	if !errors.Is(err, ErrPathNotFound) {
		t.Fatalf("QuotaList = %v, want ErrPathNotFound", err)
	}
	if !strings.Contains(err.Error(), "/nosuch1") {
		t.Errorf("QuotaList = %q, want the first missing directory", err)
	}
	_, argvs := executor.Calls()
	want := wantArgv("volume", "quota", "t16vol", "list", "/d1", "/nosuch1", "/nosuch2")
	if !slices.Equal(argvs[0], want) {
		t.Errorf("argv = %v, want %v", argvs[0], want)
	}
}

// TestQuotaListEmptyAnswer checks that an answer without a volQuota element is an
// empty result, which is what a request for a directory without a limit looks like.
func TestQuotaListEmptyAnswer(t *testing.T) {
	t.Parallel()

	client, _ := newStaticClient(t, okAnswer(), nil)
	limits, err := client.QuotaList(context.Background(), "t16vol")
	if err != nil {
		t.Fatalf("QuotaList = %v, want nil", err)
	}
	if len(limits) != 0 {
		t.Fatalf("QuotaList = %v, want an empty result", quotaPaths(limits))
	}
}

// TestQuotaLimitFieldsWithNAValues checks that the optional fields a hard limit does
// not always have -- "N/A" for the soft limit, the available space and the exceeded
// flags -- are read as 0 / false, not as an error.
func TestQuotaLimitFieldsWithNAValues(t *testing.T) {
	t.Parallel()

	stdout := cliAnswer("0", "0", "", `<volQuota><limit><path>/d1</path><hard_limit>100</hard_limit>`+
		`<soft_limit_percent>N/A</soft_limit_percent><soft_limit_value>N/A</soft_limit_value>`+
		`<used_space>10</used_space><avail_space>N/A</avail_space><sl_exceeded>N/A</sl_exceeded>`+
		`<hl_exceeded>N/A</hl_exceeded></limit></volQuota>`)
	client, _ := newStaticClient(t, stdout, nil)
	limits, err := client.QuotaList(context.Background(), "t16vol", "/d1")
	if err != nil {
		t.Fatalf("QuotaList = %v, want nil", err)
	}
	want := QuotaLimit{Path: "/d1", HasLimit: true, HardLimit: 100, Used: 10}
	if len(limits) != 1 || limits[0] != want {
		t.Fatalf("QuotaList = %+v, want %+v", limits, want)
	}
}

// TestQuotaLimitFields checks how the numeric fields of a limit are read.
func TestQuotaLimitFields(t *testing.T) {
	t.Parallel()

	stdout := cliAnswer("0", "0", "", `<volQuota><limit><path>/d1</path><hard_limit>100</hard_limit>`+
		`<soft_limit_percent>0%</soft_limit_percent><soft_limit_value>0</soft_limit_value>`+
		`<used_space>10</used_space><avail_space>90</avail_space><sl_exceeded>Yes</sl_exceeded>`+
		`<hl_exceeded>No</hl_exceeded></limit>`+
		`<limit><path>/d2</path><hard_limit>N/A</hard_limit><soft_limit_percent>N/A</soft_limit_percent>`+
		`<soft_limit_value>N/A</soft_limit_value><used_space>0</used_space><avail_space>N/A</avail_space>`+
		`<sl_exceeded>N/A</sl_exceeded><hl_exceeded>N/A</hl_exceeded></limit></volQuota>`)
	client, _ := newStaticClient(t, stdout, nil)
	limits, err := client.QuotaList(context.Background(), "t16vol")
	if err != nil {
		t.Fatalf("QuotaList = %v, want nil", err)
	}
	if len(limits) != 2 {
		t.Fatalf("QuotaList = %d limits, want 2", len(limits))
	}

	withLimit := limits[0]
	want := QuotaLimit{
		Path: "/d1", HasLimit: true, HardLimit: 100, SoftLimitPercent: 0,
		SoftLimit: 0, Used: 10, Available: 90, SoftLimitExceeded: true, HardLimitExceeded: false,
	}
	if withLimit != want {
		t.Errorf("limits[0] = %+v, want %+v", withLimit, want)
	}

	withoutLimit := limits[1]
	if withoutLimit.HasLimit {
		t.Error("limits[1].HasLimit = true, want false")
	}
	if withoutLimit.HardLimit != 0 || withoutLimit.SoftLimit != 0 || withoutLimit.Available != 0 ||
		withoutLimit.SoftLimitPercent != 0 {
		t.Errorf("limits[1] = %+v, want every limit field zero", withoutLimit)
	}
	if withoutLimit.SoftLimitExceeded || withoutLimit.HardLimitExceeded {
		t.Error("limits[1] reports an exceeded limit, want none")
	}
}

// TestQuotaEnableAlreadyEnabled checks that the two "already done" answers of the
// quota methods become success.
func TestQuotaEnableAlreadyEnabled(t *testing.T) {
	t.Parallel()

	client, _ := newStaticClient(t, cliAnswer("-1", "30800", "Quota is already enabled", ""), nil)
	if err := client.QuotaEnable(context.Background(), "t16vol"); err != nil {
		t.Fatalf("QuotaEnable = %v, want nil", err)
	}

	client, _ = newStaticClient(t, cliAnswer("-1", "30800",
		"removexattr failed on /var/run/gluster/t16vol_quota_limit/d2. Reason : No data available", ""), nil)
	if err := client.QuotaRemove(context.Background(), "t16vol", "/d2"); err != nil {
		t.Fatalf("QuotaRemove = %v, want nil", err)
	}
}

// TestVolumeCreateArgvOrder checks the argv of `volume create` for every
// combination of replica and force.
func TestVolumeCreateArgvOrder(t *testing.T) {
	t.Parallel()

	bricks := []Brick{
		{Host: "h1", Path: "/b1"},
		{Host: "h2", Path: "/b2"},
		{Host: "h3", Path: "/b3"},
	}
	tests := []struct {
		name string
		opts CreateOptions
		want []string
	}{
		{
			name: "distribute without force",
			opts: CreateOptions{Name: "v", Bricks: bricks[:1]},
			want: wantArgv("volume", "create", "v", "h1:/b1"),
		},
		{
			name: "replica 3 with force",
			opts: CreateOptions{Name: "v", Replica: 3, Bricks: bricks, Force: true},
			want: wantArgv("volume", "create", "v", "replica", "3", "h1:/b1", "h2:/b2", "h3:/b3", "force"),
		},
		{
			name: "replica 2 with six bricks",
			opts: CreateOptions{Name: "v", Replica: 2, Bricks: append(slices.Clone(bricks), bricks...)},
			want: wantArgv("volume", "create", "v", "replica", "2",
				"h1:/b1", "h2:/b2", "h3:/b3", "h1:/b1", "h2:/b2", "h3:/b3"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Duplicate bricks are rejected by the validator, so the six brick case
			// is checked through the argv builder only.
			if err := validateCreateOptions(tc.opts); err != nil {
				if tc.name != "replica 2 with six bricks" {
					t.Fatalf("validateCreateOptions = %v, want nil", err)
				}
			}
			client, executor := newStaticClient(t, okAnswer(), nil)
			err := client.VolumeCreate(context.Background(), tc.opts)
			if tc.name == "replica 2 with six bricks" {
				if !errors.Is(err, ErrInvalidCommand) {
					t.Fatalf("VolumeCreate with duplicate bricks = %v, want ErrInvalidCommand", err)
				}
				if hosts, _ := executor.Calls(); len(hosts) != 0 {
					t.Fatalf("the executor was called %v, want no call", hosts)
				}
				return
			}
			if err != nil {
				t.Fatalf("VolumeCreate = %v, want nil", err)
			}
			_, argvs := executor.Calls()
			if !slices.Equal(argvs[0], tc.want) {
				t.Errorf("argv = %v, want %v", argvs[0], tc.want)
			}
		})
	}
}

// TestVolumeCreateWithoutForce checks that "force" is not appended by default.
func TestVolumeCreateWithoutForce(t *testing.T) {
	t.Parallel()

	client, executor := newStaticClient(t, okAnswer(), nil)
	if err := client.VolumeCreate(context.Background(), CreateOptions{
		Name: "v", Bricks: []Brick{{Host: "h1", Path: "/b1"}},
	}); err != nil {
		t.Fatalf("VolumeCreate = %v, want nil", err)
	}
	_, argvs := executor.Calls()
	if slices.Contains(argvs[0], "force") {
		t.Errorf("argv = %v, want no force", argvs[0])
	}
}

// TestNewClientErrors checks the checks of the constructor.
func TestNewClientErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		executor Executor
		hosts    []string
	}{
		{name: "no executor", executor: nil, hosts: []string{"gluster-1"}},
		{name: "no host", executor: &staticExecutor{}, hosts: nil},
		{name: "empty host", executor: &staticExecutor{}, hosts: []string{""}},
		{name: "host with a space", executor: &staticExecutor{}, hosts: []string{"gluster-1", "gluster 2"}},
		{name: "host with a slash", executor: &staticExecutor{}, hosts: []string{"../gluster-1"}},
		{name: "host that is too long", executor: &staticExecutor{}, hosts: []string{strings.Repeat("a", maxHostLength+1)}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client, err := NewClient(tc.executor, tc.hosts)
			if err == nil {
				t.Fatalf("NewClient = %v, nil, want an error", client)
			}
			if !errors.Is(err, ErrInvalidCommand) {
				t.Fatalf("NewClient = %v, want ErrInvalidCommand", err)
			}
			if client != nil {
				t.Error("NewClient returned a client together with an error")
			}
		})
	}
}

// TestNewClientCopiesTheHosts checks that the caller cannot change the host list
// behind the client's back.
func TestNewClientCopiesTheHosts(t *testing.T) {
	t.Parallel()

	hosts := []string{"gluster-1", "gluster-2"}
	client, err := NewClient(&staticExecutor{}, hosts)
	if err != nil {
		t.Fatalf("NewClient = %v, want nil", err)
	}
	hosts[0] = "changed"
	if client.hosts[0] != "gluster-1" {
		t.Errorf("the client kept %q, want a copy of the original list", client.hosts[0])
	}
}

// TestClientIsConcurrent runs several commands through one client at the same time:
// the Failover memo is the only shared state and it has to be safe.
func TestClientIsConcurrent(t *testing.T) {
	t.Parallel()

	executor := &staticExecutor{stdout: []byte(okAnswer())}
	client, err := NewClient(executor, []string{"gluster-1", "gluster-2"})
	if err != nil {
		t.Fatalf("NewClient = %v, want nil", err)
	}

	const runs = 20
	var wg sync.WaitGroup
	errs := make(chan error, runs)
	for i := 0; i < runs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := client.VolumeStart(context.Background(), "t16vol"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if hosts, _ := executor.Calls(); len(hosts) != runs {
		t.Errorf("the executor was called %d times, want %d", len(hosts), runs)
	}
}

// TestXMLTypesDecodeTheDocumentShape makes sure the structs really describe the
// document, independent of the client methods.
func TestXMLTypesDecodeTheDocumentShape(t *testing.T) {
	t.Parallel()

	const document = `<cliOutput>
  <opRet>0</opRet><opErrno>0</opErrno><opErrstr/>
  <volInfo><volumes><count>1</count><volume>
    <name>v</name><id>id</id><statusStr>Started</statusStr>
    <brickCount>1</brickCount><distCount>1</distCount><replicaCount>1</replicaCount>
    <arbiterCount>0</arbiterCount><disperseCount>0</disperseCount><redundancyCount>0</redundancyCount>
    <typeStr>Distribute</typeStr>
    <bricks><brick uuid="u">h:/b<name>h:/b</name><hostUuid>u</hostUuid><isArbiter>0</isArbiter></brick></bricks>
    <options><option><name>nfs.disable</name><value>on</value></option></options>
  </volume></volumes></volInfo>
</cliOutput>`
	var output cliOutput
	if err := xml.Unmarshal([]byte(document), &output); err != nil {
		t.Fatalf("Unmarshal = %v, want nil", err)
	}
	if output.VolInfo == nil || output.VolInfo.Volumes == nil {
		t.Fatal("the volInfo element was not decoded")
	}
	volume := output.VolInfo.Volumes.Volumes[0]
	if volume.Name != "v" || volume.TypeStr != "Distribute" {
		t.Errorf("volume = %+v, want the volume of the document", volume)
	}
	if len(volume.Bricks) != 1 || volume.Bricks[0].Name != "h:/b" || volume.Bricks[0].HostUUID != "u" {
		t.Errorf("bricks = %+v, want one brick named h:/b", volume.Bricks)
	}
	if len(volume.Options) != 1 || volume.Options[0].Name != "nfs.disable" {
		t.Errorf("options = %+v, want one option", volume.Options)
	}
}

// TestParseErrno checks that a broken opErrno does not fail a call: the errno is
// only carried in the message.
func TestParseErrno(t *testing.T) {
	t.Parallel()

	output := &cliOutput{OpRet: "0", OpErrno: "not-a-number"}
	if got := output.errno(); got != 0 {
		t.Errorf("errno() = %d, want 0", got)
	}
	output.OpErrno = "30800"
	if got := output.errno(); got != 30800 {
		t.Errorf("errno() = %d, want 30800", got)
	}
}
