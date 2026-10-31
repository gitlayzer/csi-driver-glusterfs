package config

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync/atomic"
	"time"

	"k8s.io/klog/v2"
)

// defaultInterval is the polling interval [Store.Run] uses when it is called
// with a non-positive interval.
const defaultInterval = 10 * time.Second

// Store holds the current configuration snapshot and polls the configuration
// file for changes.
//
// Kubernetes replaces a ConfigMap volume by writing a new timestamped directory
// and atomically swapping the ..data symlink, so the inode of config.json
// changes on every update and watching the file itself is not reliable.
// Polling the content and comparing its hash is, and it needs no extra
// dependency (docs/tasks/T-03.md).
//
// A Store is safe for concurrent use: readers go through the atomic pointer,
// and Run (or reload) is called from a single goroutine.
type Store struct {
	path string

	// current holds the last successfully loaded snapshot. It is never nil
	// after NewStore returned successfully.
	current atomic.Pointer[Config]

	// okHash identifies the last successfully loaded content; failedHash
	// identifies the content that failed last, so that the same broken file is
	// reported once instead of on every tick.
	okHash     [sha256.Size]byte
	failedHash [sha256.Size]byte
	haveOK     bool
	haveFailed bool
}

// NewStore loads the configuration file at path synchronously and returns a
// Store holding the result.
//
// A missing or invalid file is an error: the driver fails at startup instead of
// running without any configuration. Failures of later reloads are not fatal;
// the store keeps serving the previous snapshot.
func NewStore(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("config path must not be empty")
	}
	cfg, hash, err := loadHashed(path)
	if err != nil {
		return nil, err
	}
	s := &Store{path: path, okHash: hash, haveOK: true}
	s.current.Store(cfg)
	return s, nil
}

// Current returns the current snapshot. It never returns nil, so callers can
// use the result without a nil check.
func (s *Store) Current() *Config {
	if cfg := s.current.Load(); cfg != nil {
		return cfg
	}
	// Only reachable for a zero Store, which NewStore never returns.
	return &Config{}
}

// Run polls the configuration file every interval until ctx is cancelled, then
// returns. A non-positive interval means 10s.
//
// Run starts no goroutine: the caller decides whether to run it in the
// background, and cancelling ctx is the stop path.
func (s *Store) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.reload(); err != nil {
				// reload suppresses repeated reports of the same broken
				// content, so this logs each distinct problem once.
				klog.ErrorS(err, "glusterfs-csi config reload failed, keeping the previous configuration", "path", s.path)
			}
		}
	}
}

// reload checks the configuration file once.
//
// It reports changed=true only when a new valid document replaced the snapshot.
// Everything else keeps the previous snapshot: unchanged content, an unreadable
// file and an invalid document all leave Current untouched. No log entry is
// written here; Run logs the returned error, except for a repetition of the
// same invalid content, which is not an error again.
func (s *Store) reload() (changed bool, err error) {
	data, err := readFile(s.path)
	if err != nil {
		// ConfigMap updates and deletions are transient; keep serving the last
		// good configuration and let the caller log the cause.
		return false, err
	}

	hash := sha256.Sum256(data)
	if s.haveOK && hash == s.okHash {
		// Unchanged content: no parsing, no logging. The file is valid again
		// (or never stopped being valid), so a later breakage is a new event
		// and has to be reported again.
		s.haveFailed = false
		return false, nil
	}
	if s.haveFailed && hash == s.failedHash {
		// Same broken content as on the previous tick: already reported.
		return false, nil
	}

	if len(data) > MaxFileSize {
		return s.reject(hash, invalidf("file %q is larger than %d bytes", s.path, MaxFileSize))
	}
	cfg, err := Parse(data)
	if err != nil {
		return s.reject(hash, err)
	}

	s.current.Store(cfg)
	s.okHash = hash
	s.haveOK = true
	// A later failure is a new event again now that the content is valid.
	s.haveFailed = false

	klog.InfoS("glusterfs-csi config reloaded", "path", s.path, "clusters", cfg.ClusterIDs())
	return true, nil
}

// reject records content that failed to load and returns the cause. The
// snapshot and the hash of the last good content are left untouched.
func (s *Store) reject(hash [sha256.Size]byte, err error) (changed bool, out error) {
	s.failedHash = hash
	s.haveFailed = true
	return false, err
}
