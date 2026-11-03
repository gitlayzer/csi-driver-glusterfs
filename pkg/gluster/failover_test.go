package gluster

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeExecutor records the order of the hosts it was called with and answers with
// a per host result.
type fakeExecutor struct {
	mu      sync.Mutex
	calls   []string
	results map[string]fakeResult
}

// fakeResult is the answer of a fakeExecutor for one host.
type fakeResult struct {
	stdout []byte
	err    error
}

// Run records the host and returns the configured result, defaulting to success.
func (f *fakeExecutor) Run(_ context.Context, host string, _ ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, host)
	f.mu.Unlock()
	result, ok := f.results[host]
	if !ok {
		return []byte("stdout of " + host), nil
	}
	return result.stdout, result.err
}

// Calls returns the hosts Run was called with, in order.
func (f *fakeExecutor) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func TestFailoverRun(t *testing.T) {
	t.Parallel()

	connectErr := func(host string) error {
		return fmt.Errorf("%w: cannot connect to %s", ErrConnect, host)
	}
	hosts := []string{"gluster-1", "gluster-2", "gluster-3"}

	tests := []struct {
		name       string
		hosts      []string
		results    map[string]fakeResult
		wantCalls  []string
		wantHost   string
		wantStdout string
		wantErr    func(*testing.T, error)
	}{
		{
			name:       "first host succeeds and is the only call",
			hosts:      hosts,
			wantCalls:  []string{"gluster-1"},
			wantHost:   "gluster-1",
			wantStdout: "stdout of gluster-1",
		},
		{
			name:  "the second host is used after ErrConnect",
			hosts: hosts,
			results: map[string]fakeResult{
				"gluster-1": {err: connectErr("gluster-1")},
			},
			wantCalls:  []string{"gluster-1", "gluster-2"},
			wantHost:   "gluster-2",
			wantStdout: "stdout of gluster-2",
		},
		{
			name:  "an ExitError does not move to the next host",
			hosts: hosts,
			results: map[string]fakeResult{
				"gluster-1": {err: &ExitError{Host: "gluster-1", Program: "gluster", Code: 1}},
			},
			wantCalls: []string{"gluster-1"},
			wantHost:  "gluster-1",
			wantErr: func(t *testing.T, err error) {
				t.Helper()
				var exitErr *ExitError
				if !errors.As(err, &exitErr) {
					t.Fatalf("error = %v, want *ExitError", err)
				}
				if exitErr.Code != 1 {
					t.Errorf("exit code = %d, want 1", exitErr.Code)
				}
			},
		},
		{
			name:  "another error does not move to the next host",
			hosts: hosts,
			results: map[string]fakeResult{
				"gluster-1": {err: errors.New("the connection closed")},
			},
			wantCalls: []string{"gluster-1"},
			wantHost:  "gluster-1",
			wantErr: func(t *testing.T, err error) {
				t.Helper()
				if errors.Is(err, ErrConnect) {
					t.Error("error wraps ErrConnect, want a plain failure")
				}
				if !strings.Contains(err.Error(), "connection closed") {
					t.Errorf("error = %v, want the original cause", err)
				}
			},
		},
		{
			name:  "all hosts fail with ErrConnect",
			hosts: hosts,
			results: map[string]fakeResult{
				"gluster-1": {err: connectErr("gluster-1")},
				"gluster-2": {err: connectErr("gluster-2")},
				"gluster-3": {err: connectErr("gluster-3")},
			},
			wantCalls: []string{"gluster-1", "gluster-2", "gluster-3"},
			wantErr: func(t *testing.T, err error) {
				t.Helper()
				var all *AllHostsFailedError
				if !errors.As(err, &all) {
					t.Fatalf("error = %v, want *AllHostsFailedError", err)
				}
				if len(all.Errors) != 3 {
					t.Fatalf("the error holds %d causes, want 3", len(all.Errors))
				}
				if !errors.Is(err, ErrConnect) {
					t.Error("errors.Is(err, ErrConnect) = false, want true")
				}
				for _, host := range hosts {
					if !strings.Contains(err.Error(), host) {
						t.Errorf("error %q does not mention %s", err, host)
					}
				}
				if got := strings.Count(err.Error(), "; "); got != 2 {
					t.Errorf("error %q has %d separators, want 2", err, got)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			executor := &fakeExecutor{results: tc.results}
			failover := &Failover{Executor: executor}
			stdout, host, err := failover.Run(context.Background(), tc.hosts, "gluster", "volume", "list")

			if tc.wantErr != nil {
				if err == nil {
					t.Fatalf("Run = %v, nil, want an error", stdout)
				}
				tc.wantErr(t, err)
			} else {
				if err != nil {
					t.Fatalf("Run = %v, want nil", err)
				}
				if string(stdout) != tc.wantStdout {
					t.Errorf("stdout = %q, want %q", stdout, tc.wantStdout)
				}
			}
			if host != tc.wantHost {
				t.Errorf("host = %q, want %q", host, tc.wantHost)
			}
			if got := executor.Calls(); !slices.Equal(got, tc.wantCalls) {
				t.Errorf("calls = %v, want %v", got, tc.wantCalls)
			}
		})
	}
}

func TestFailoverRemembersTheLastSuccessfulHost(t *testing.T) {
	t.Parallel()

	connectErr := fmt.Errorf("%w: dial tcp: connection refused", ErrConnect)
	executor := &fakeExecutor{results: map[string]fakeResult{
		"gluster-1": {err: connectErr},
	}}
	failover := &Failover{Executor: executor}
	hosts := []string{"gluster-1", "gluster-2", "gluster-3"}

	// First run: gluster-1 is down, gluster-2 answers.
	if _, host, err := failover.Run(context.Background(), hosts, "gluster"); err != nil || host != "gluster-2" {
		t.Fatalf("first Run = %q, %v, want gluster-2, nil", host, err)
	}
	// Remove the failure so that a call to gluster-1 would succeed as well.
	executor.mu.Lock()
	executor.results = map[string]fakeResult{}
	executor.calls = nil
	executor.mu.Unlock()

	// Second run: gluster-2 is tried first, because it worked last time.
	if _, host, err := failover.Run(context.Background(), hosts, "gluster"); err != nil || host != "gluster-2" {
		t.Fatalf("second Run = %q, %v, want gluster-2, nil", host, err)
	}
	if got, want := executor.Calls(), []string{"gluster-2"}; !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

func TestFailoverFallsBackWhenTheRememberedHostIsGone(t *testing.T) {
	t.Parallel()

	executor := &fakeExecutor{}
	failover := &Failover{Executor: executor}

	if _, host, err := failover.Run(context.Background(), []string{"gluster-1", "gluster-2"}, "gluster"); err != nil || host != "gluster-1" {
		t.Fatalf("first Run = %q, %v, want gluster-1, nil", host, err)
	}
	// gluster-1 is no longer part of the list: the original order is used.
	if _, host, err := failover.Run(context.Background(), []string{"gluster-3", "gluster-4"}, "gluster"); err != nil || host != "gluster-3" {
		t.Fatalf("second Run = %q, %v, want gluster-3, nil", host, err)
	}
}

func TestFailoverRemembersTheHostOfAnExitError(t *testing.T) {
	t.Parallel()

	executor := &fakeExecutor{results: map[string]fakeResult{
		"gluster-1": {err: fmt.Errorf("%w: dial failed", ErrConnect)},
		"gluster-2": {err: &ExitError{Host: "gluster-2", Program: "gluster", Code: 3}},
	}}
	failover := &Failover{Executor: executor}
	hosts := []string{"gluster-1", "gluster-2"}

	if _, host, err := failover.Run(context.Background(), hosts, "gluster"); host != "gluster-2" {
		t.Fatalf("first Run = %q, %v, want gluster-2", host, err)
	}
	executor.mu.Lock()
	executor.calls = nil
	executor.mu.Unlock()

	// The command ran on gluster-2, so that host is reachable: it is preferred next
	// time even though the command exited with a non-zero status.
	if _, host, err := failover.Run(context.Background(), hosts, "gluster"); host != "gluster-2" {
		t.Fatalf("second Run = %q, %v, want gluster-2", host, err)
	}
	if got, want := executor.Calls(), []string{"gluster-2"}; !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

func TestFailoverEmptyHostList(t *testing.T) {
	t.Parallel()

	executor := &fakeExecutor{}
	failover := &Failover{Executor: executor}
	_, _, err := failover.Run(context.Background(), nil, "gluster")
	if !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("Run = %v, want ErrInvalidCommand", err)
	}
	if got := executor.Calls(); len(got) != 0 {
		t.Errorf("calls = %v, want none", got)
	}
}

func TestFailoverStopsWhenTheContextIsCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	executor := &fakeExecutor{results: map[string]fakeResult{
		"gluster-1": {err: fmt.Errorf("%w: dial failed", ErrConnect)},
	}}
	failover := &Failover{Executor: executor}
	_, _, err := failover.Run(ctx, []string{"gluster-1", "gluster-2"}, "gluster")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want errors.Is(err, context.Canceled)", err)
	}
	if errors.Is(err, ErrConnect) {
		t.Errorf("Run = %v, must not wrap ErrConnect", err)
	}
	if got := executor.Calls(); len(got) != 0 {
		t.Errorf("calls = %v, want none", got)
	}
}

func TestFailoverStopsWhenTheContextIsCancelledBetweenHosts(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	executor := &cancellingExecutor{cancel: cancel}
	failover := &Failover{Executor: executor}
	_, _, err := failover.Run(ctx, []string{"gluster-1", "gluster-2"}, "gluster")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want errors.Is(err, context.Canceled)", err)
	}
	if got, want := executor.calls, []string{"gluster-1"}; !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

// cancellingExecutor cancels the context of the test after the first call and
// always fails with ErrConnect.
type cancellingExecutor struct {
	cancel context.CancelFunc
	calls  []string
}

func (e *cancellingExecutor) Run(_ context.Context, host string, _ ...string) ([]byte, error) {
	e.calls = append(e.calls, host)
	e.cancel()
	return nil, fmt.Errorf("%w: dial failed", ErrConnect)
}

func TestFailoverForgetsEverythingAboveTheLimit(t *testing.T) {
	t.Parallel()

	executor := &fakeExecutor{}
	failover := &Failover{Executor: executor}

	for i := 0; i < maxFailoverEntries+1; i++ {
		hosts := []string{fmt.Sprintf("gluster-%d", i), "fallback"}
		if _, host, err := failover.Run(context.Background(), hosts, "gluster"); err != nil || host != hosts[0] {
			t.Fatalf("Run %d = %q, %v, want %q, nil", i, host, err, hosts[0])
		}
	}
	failover.mu.Lock()
	size := len(failover.last)
	failover.mu.Unlock()
	if size > maxFailoverEntries {
		t.Errorf("the memo holds %d entries, want at most %d", size, maxFailoverEntries)
	}
}

func TestFailoverConcurrentRuns(t *testing.T) {
	t.Parallel()

	executor := &fakeExecutor{}
	failover := &Failover{Executor: executor}
	hosts := []string{"gluster-1", "gluster-2", "gluster-3"}

	const runs = 20
	var wg sync.WaitGroup
	errs := make(chan error, runs)
	for i := 0; i < runs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := failover.Run(context.Background(), hosts, "gluster"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := len(executor.Calls()); got != runs {
		t.Errorf("the executor was called %d times, want %d", got, runs)
	}
}

// TestFailoverWithSSHExecutor runs a command through a real executor whose first
// host is a closed port and whose second host is a test server.
func TestFailoverWithSSHExecutor(t *testing.T) {
	t.Parallel()

	hostKey := newTestSigner(t)
	clientKey := newTestSigner(t)
	server := newTestSSHServer(t, hostKey, clientKey.PublicKey(), modeHandler)
	server.handler = func(string) (string, string, int) {
		return "<cliOutput><opRet>0</opRet></cliOutput>", "", 0
	}

	// A port that is closed: the listener is created and closed again, so the
	// kernel refuses the connection to it.
	closedListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	closedAddr := closedListener.Addr().String()
	if closeErr := closedListener.Close(); closeErr != nil {
		t.Fatalf("close the listener: %v", closeErr)
	}

	// Both hosts are dialled on the port of the credentials, so known_hosts holds
	// one record per host name for that port.
	port := server.Port()
	data := map[string]string{
		KeyUsername:   "gluster",
		KeyPrivateKey: clientKey.pem,
		KeyPort:       strconv.Itoa(port),
		KeyKnownHosts: strings.Join([]string{
			knownhostsLineForPort("gluster-1", port, hostKey.PublicKey()),
			knownhostsLineForPort("gluster-2", port, hostKey.PublicKey()),
		}, "\n"),
	}
	creds, err := ParseCredentials(data)
	if err != nil {
		t.Fatalf("ParseCredentials = %v", err)
	}

	executor := newTestExecutor(t, creds, SSHOptions{
		DialTimeout: 500 * time.Millisecond,
	}, map[string]string{
		net.JoinHostPort("gluster-1", strconv.Itoa(port)): closedAddr,
		net.JoinHostPort("gluster-2", strconv.Itoa(port)): server.Addr(),
	})

	failover := &Failover{Executor: executor}
	stdout, host, err := failover.Run(context.Background(), []string{"gluster-1", "gluster-2"},
		"gluster", "--mode=script", "--xml", "volume", "info")
	if err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if host != "gluster-2" {
		t.Errorf("host = %q, want gluster-2", host)
	}
	if !strings.Contains(string(stdout), "<opRet>0</opRet>") {
		t.Errorf("stdout = %q, want the output of the second host", stdout)
	}
	if got, want := server.Commands(), []string{"'gluster' '--mode=script' '--xml' 'volume' 'info'"}; !slices.Equal(got, want) {
		t.Errorf("the server received %q, want %q", got, want)
	}
}
