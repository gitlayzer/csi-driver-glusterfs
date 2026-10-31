package driver

import "sync"

// inflight is a set of keys of operations in progress. tryAcquire never blocks:
// a concurrent operation on the same key is rejected with Aborted, as the CSI
// spec requires (docs/architecture.md §5.5).
//
// The zero value is not usable, use [newInflight].
type inflight struct {
	mu   sync.Mutex
	keys map[string]struct{}
}

// newInflight returns an empty set of in-flight keys.
func newInflight() *inflight {
	return &inflight{keys: make(map[string]struct{})}
}

// tryAcquire marks key as in progress and reports whether it was free.
//
// It never blocks: the caller answers Aborted when the key is taken, because a
// CSI plugin must not queue RPCs that the CO is going to retry anyway.
func (l *inflight) tryAcquire(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if _, busy := l.keys[key]; busy {
		return false
	}
	l.keys[key] = struct{}{}
	return true
}

// release removes key from the set. Releasing a key that is not held is a
// no-op, so a deferred release is always safe.
func (l *inflight) release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	delete(l.keys, key)
}
