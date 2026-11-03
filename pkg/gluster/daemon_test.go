package gluster

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
)

// countExecutor answers every call with one fixed result and counts the calls.
type countExecutor struct {
	mu     sync.Mutex
	stdout []byte
	err    error
	hosts  []string
}

// Run counts the call and returns the configured answer.
func (e *countExecutor) Run(_ context.Context, host string, _ ...string) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.hosts = append(e.hosts, host)
	return e.stdout, e.err
}

// Calls returns the hosts the executor ran on, in order.
func (e *countExecutor) Calls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.hosts)
}

// TestDaemonCheckRun checks the detection of a glusterd that is not running.
func TestDaemonCheckRun(t *testing.T) {
	t.Parallel()

	dialErr := fmt.Errorf("%w: cannot connect to gluster-1:22", ErrConnect)
	tests := []struct {
		name       string
		host       string
		stdout     []byte
		err        error
		wantStdout string
		wantErr    func(*testing.T, error)
	}{
		{
			name:   "glusterd is not running",
			host:   "gluster-1",
			stdout: []byte(glusterdDownMessage),
			err:    &ExitError{Host: "gluster-1", Program: "gluster", Code: 1, Stdout: []byte(glusterdDownMessage)},
			wantErr: func(t *testing.T, err error) {
				t.Helper()
				if !errors.Is(err, ErrConnect) {
					t.Fatalf("Run = %v, want errors.Is(err, ErrConnect)", err)
				}
				var exitErr *ExitError
				if errors.As(err, &exitErr) {
					t.Fatalf("Run = %v, must not wrap *ExitError: Failover would not try the next host", err)
				}
				if !strings.Contains(err.Error(), "glusterd is not running on gluster-1") {
					t.Errorf("Run = %q, want the host and the reason", err)
				}
			},
		},
		{
			name:   "the message has surrounding white space",
			host:   "gluster-1",
			stdout: []byte("\n  " + glusterdDownMessage + "  \n"),
			err: &ExitError{Host: "gluster-1", Program: "gluster", Code: 1,
				Stdout: []byte("\n  " + glusterdDownMessage + "  \n")},
			wantErr: func(t *testing.T, err error) {
				t.Helper()
				if !errors.Is(err, ErrConnect) {
					t.Fatalf("Run = %v, want ErrConnect", err)
				}
			},
		},
		{
			name:   "exit status 2 is not the documented answer",
			host:   "gluster-1",
			stdout: []byte(glusterdDownMessage),
			err:    &ExitError{Host: "gluster-1", Program: "gluster", Code: 2, Stdout: []byte(glusterdDownMessage)},
			wantErr: func(t *testing.T, err error) {
				t.Helper()
				var exitErr *ExitError
				if !errors.As(err, &exitErr) {
					t.Fatalf("Run = %v, want *ExitError unchanged", err)
				}
				if errors.Is(err, ErrConnect) {
					t.Error("Run wraps ErrConnect, want the original error")
				}
			},
		},
		{
			name:   "another program printed the message",
			host:   "gluster-1",
			stdout: []byte(glusterdDownMessage),
			err:    &ExitError{Host: "gluster-1", Program: "lvm", Code: 1, Stdout: []byte(glusterdDownMessage)},
			wantErr: func(t *testing.T, err error) {
				t.Helper()
				var exitErr *ExitError
				if !errors.As(err, &exitErr) {
					t.Fatalf("Run = %v, want *ExitError unchanged", err)
				}
			},
		},
		{
			name:   "the message is only on stderr",
			host:   "gluster-1",
			stdout: nil,
			err: &ExitError{Host: "gluster-1", Program: "gluster", Code: 1,
				Stderr: []byte(glusterdDownMessage)},
			wantErr: func(t *testing.T, err error) {
				t.Helper()
				var exitErr *ExitError
				if !errors.As(err, &exitErr) {
					t.Fatalf("Run = %v, want *ExitError unchanged", err)
				}
				if errors.Is(err, ErrConnect) {
					t.Error("Run wraps ErrConnect, want the original error: the message has to be on stdout")
				}
			},
		},
		{
			name:   "stdout is something else",
			host:   "gluster-1",
			stdout: []byte("Connection failed."),
			err: &ExitError{Host: "gluster-1", Program: "gluster", Code: 1,
				Stdout: []byte("Connection failed.")},
			wantErr: func(t *testing.T, err error) {
				t.Helper()
				var exitErr *ExitError
				if !errors.As(err, &exitErr) {
					t.Fatalf("Run = %v, want *ExitError unchanged", err)
				}
			},
		},
		{
			name:       "a successful command is returned unchanged",
			host:       "gluster-1",
			stdout:     []byte("<cliOutput><opRet>0</opRet></cliOutput>"),
			wantStdout: "<cliOutput><opRet>0</opRet></cliOutput>",
			wantErr: func(t *testing.T, err error) {
				t.Helper()
				if err != nil {
					t.Fatalf("Run = %v, want nil", err)
				}
			},
		},
		{
			name: "another error is returned unchanged",
			host: "gluster-1",
			err:  dialErr,
			wantErr: func(t *testing.T, err error) {
				t.Helper()
				if !errors.Is(err, ErrConnect) {
					t.Fatalf("Run = %v, want the original ErrConnect", err)
				}
				if !strings.Contains(err.Error(), "cannot connect") {
					t.Errorf("Run = %v, want the original transport message", err)
				}
			},
		},
		{
			name: "an invalid command is returned unchanged",
			host: "gluster-1",
			err:  fmt.Errorf("%w: program sh is not allowlisted", ErrInvalidCommand),
			wantErr: func(t *testing.T, err error) {
				t.Helper()
				if !errors.Is(err, ErrInvalidCommand) {
					t.Fatalf("Run = %v, want ErrInvalidCommand", err)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			executor := &countExecutor{stdout: tc.stdout, err: tc.err}
			check := daemonCheck{Executor: executor}
			stdout, err := check.Run(context.Background(), tc.host, "gluster", "volume", "info")
			tc.wantErr(t, err)
			if err == nil && string(stdout) != tc.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout, tc.wantStdout)
			}
			if got := executor.Calls(); !slices.Equal(got, []string{tc.host}) {
				t.Errorf("the executor was called %v, want %v", got, tc.host)
			}
		})
	}
}

// TestDaemonCheckWithFailover checks that the conversion actually makes Failover try
// the next host: that is the only reason the conversion exists.
func TestDaemonCheckWithFailover(t *testing.T) {
	t.Parallel()

	executor := &orderedExecutor{results: map[string]countResult{
		"gluster-1": {
			stdout: nil,
			err: &ExitError{Host: "gluster-1", Program: "gluster", Code: 1,
				Stdout: []byte(glusterdDownMessage)},
		},
		"gluster-2": {stdout: []byte("<cliOutput><opRet>0</opRet></cliOutput>")},
	}}
	failover := &Failover{Executor: daemonCheck{Executor: executor}}
	stdout, host, err := failover.Run(context.Background(), []string{"gluster-1", "gluster-2"},
		"gluster", "--mode=script", "--xml", "volume", "info")
	if err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if host != "gluster-2" {
		t.Errorf("host = %q, want gluster-2", host)
	}
	if !strings.Contains(string(stdout), "<opRet>0</opRet>") {
		t.Errorf("stdout = %q, want the answer of gluster-2", stdout)
	}
	if got, want := executor.Calls(), []string{"gluster-1", "gluster-2"}; !slices.Equal(got, want) {
		t.Errorf("the executor was called %v, want %v", got, want)
	}
}

// TestDaemonCheckDoesNotRetryARealFailure checks the other direction: an ExitError
// that is not the "glusterd is not running" answer has to stop the Failover, because
// the command did run on that host.
func TestDaemonCheckDoesNotRetryARealFailure(t *testing.T) {
	t.Parallel()

	executor := &orderedExecutor{results: map[string]countResult{
		"gluster-1": {
			err: &ExitError{Host: "gluster-1", Program: "gluster", Code: 1,
				Stdout: []byte("<cliOutput><opRet>-1</opRet></cliOutput>")},
		},
		"gluster-2": {stdout: []byte("<cliOutput><opRet>0</opRet></cliOutput>")},
	}}
	failover := &Failover{Executor: daemonCheck{Executor: executor}}
	_, host, err := failover.Run(context.Background(), []string{"gluster-1", "gluster-2"},
		"gluster", "--mode=script", "--xml", "volume", "info")
	if err == nil {
		t.Fatal("Run = nil, want the ExitError of gluster-1")
	}
	if host != "gluster-1" {
		t.Errorf("host = %q, want gluster-1", host)
	}
	if got, want := executor.Calls(), []string{"gluster-1"}; !slices.Equal(got, want) {
		t.Errorf("the executor was called %v, want %v", got, want)
	}
}

// countResult is the answer of an orderedExecutor for one host.
type countResult struct {
	stdout []byte
	err    error
}

// orderedExecutor answers per host and records the order of the calls.
type orderedExecutor struct {
	mu      sync.Mutex
	results map[string]countResult
	calls   []string
}

// Run records the call and returns the answer configured for host.
func (e *orderedExecutor) Run(_ context.Context, host string, _ ...string) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, host)
	result := e.results[host]
	return result.stdout, result.err
}

// Calls returns the hosts the executor ran on, in order.
func (e *orderedExecutor) Calls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.calls)
}
