package gluster

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"k8s.io/klog/v2"
)

const (
	// maxFailoverEntries bounds the memo of last successful hosts. The map is
	// emptied as a whole when it would grow beyond this, which keeps a driver that
	// serves many clusters from retaining memory forever.
	maxFailoverEntries = 64

	// keySeparator joins the host list into the key of the memo. A comma cannot
	// occur in a validated host name, so distinct lists cannot collide.
	keySeparator = ","
)

// Failover runs a command on the first reachable server of a cluster.
//
// Any glusterd can run a cluster wide management command, so when a server is
// unreachable the next one is used. Only a failure that proves the remote command
// did not start ([ErrConnect]) moves on to the next host, which means a command
// that changes something is never executed twice. The host that worked last is
// tried first, so a cluster with a dead first server does not pay its dial timeout
// on every call.
//
// The zero value is not usable, but a value with only [Failover.Executor] set is:
// both the memo and the mutex have useful zero values.
type Failover struct {
	// Executor is the transport that actually runs the command.
	Executor Executor

	// mu guards last.
	mu sync.Mutex
	// last maps the host list (joined with commas) to the host that last ran a
	// command successfully.
	last map[string]string
}

// AllHostsFailedError reports that no server of the list was reachable.
//
// It unwraps to every attempt error, so errors.Is(err, ErrConnect) still holds and
// a caller can tell "nothing was executed anywhere" from a single host failure.
type AllHostsFailedError struct {
	// Errors holds one error per host, in the order the hosts were tried.
	Errors []error
}

// Error returns "no glusterfs server is reachable: <reason>; <reason>".
func (e *AllHostsFailedError) Error() string {
	reasons := make([]string, len(e.Errors))
	for i, err := range e.Errors {
		reasons[i] = err.Error()
	}
	return fmt.Sprintf("no glusterfs server is reachable: %s", strings.Join(reasons, "; "))
}

// Unwrap returns the attempt errors, so that errors.Is finds [ErrConnect].
func (e *AllHostsFailedError) Unwrap() []error { return e.Errors }

// Run runs args on the first reachable host of hosts and returns its stdout
// together with the host that ran it.
//
// The order is the last host that succeeded for this list first, then hosts in
// the given order. A nil error or an [ExitError] means the command ran on that
// host; every other error ends the loop.
func (f *Failover) Run(ctx context.Context, hosts []string, args ...string) (stdout []byte, host string, err error) {
	if len(hosts) == 0 {
		return nil, "", fmt.Errorf("%w: no glusterfs servers to run the command on", ErrInvalidCommand)
	}
	key := strings.Join(hosts, keySeparator)
	order := f.order(key, hosts)

	attemptErrors := make([]error, 0, len(order))
	for _, candidate := range order {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, "", interruptedError(ctxErr)
		}
		stdout, err := f.Executor.Run(ctx, candidate, args...)
		if err == nil {
			f.remember(key, candidate)
			return stdout, candidate, nil
		}
		var exitErr *ExitError
		if errors.As(err, &exitErr) {
			// The command ran on this host, so it is reachable. Recording it also
			// keeps the next call from probing a server that is actually down.
			f.remember(key, candidate)
			return nil, candidate, err
		}
		if !errors.Is(err, ErrConnect) {
			return nil, candidate, err
		}
		klog.V(2).InfoS("a glusterfs server is not reachable, trying the next one",
			"host", candidate, "error", err)
		attemptErrors = append(attemptErrors, err)
	}
	return nil, "", &AllHostsFailedError{Errors: attemptErrors}
}

// order returns the hosts to try: the last successful one first if it is still in
// the list, then the remaining hosts in their original order.
func (f *Failover) order(key string, hosts []string) []string {
	preferred := f.lastHost(key)
	order := make([]string, 0, len(hosts))
	if preferred != "" && slices.Contains(hosts, preferred) {
		order = append(order, preferred)
	}
	for _, host := range hosts {
		if host == preferred {
			continue
		}
		order = append(order, host)
	}
	return order
}

// lastHost returns the remembered host for key, or "".
func (f *Failover) lastHost(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last[key]
}

// remember stores host as the last successful host for key.
func (f *Failover) remember(key, host string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.last == nil {
		f.last = make(map[string]string)
	}
	if len(f.last) >= maxFailoverEntries {
		if _, ok := f.last[key]; !ok {
			f.last = make(map[string]string)
		}
	}
	f.last[key] = host
}
