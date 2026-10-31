package config

import (
	"context"
	"errors"
	"flag"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/klog/v2"
)

// TestMain silences klog so that the reload tests do not spam the output with
// the expected "reloaded" and "reload failed" lines.
func TestMain(m *testing.M) {
	// klog writes to stderr for anything at or above stderrthreshold, so both
	// the destination and the thresholds have to be changed.
	flags := flag.NewFlagSet("klog", flag.ContinueOnError)
	klog.InitFlags(flags)
	for name, value := range map[string]string{
		"logtostderr":     "false",
		"alsologtostderr": "false",
		"stderrthreshold": "FATAL",
	} {
		if err := flags.Set(name, value); err != nil {
			panic(err)
		}
	}
	klog.SetOutput(io.Discard)
	os.Exit(m.Run())
}

// storePaths returns the directory and file a Store test works on.
func storePaths(t *testing.T) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	return dir, filepath.Join(dir, "config.json")
}

// servers returns the servers of clusterID in the given snapshot.
func servers(t *testing.T, cfg *Config, clusterID string) []string {
	t.Helper()
	cl, ok := cfg.Get(clusterID)
	if !ok {
		t.Fatalf("Get(%q) not found in %v", clusterID, cfg.ClusterIDs())
	}
	return cl.Servers
}

func TestNewStore(t *testing.T) {
	_, valid := storePaths(t)
	writeFile(t, valid, `[{"clusterID":"prod","servers":["10.0.0.1"]}]`)

	_, invalid := storePaths(t)
	writeFile(t, invalid, `[{"clusterID":"prod","servers":[]}]`)

	t.Run("valid file", func(t *testing.T) {
		s, err := NewStore(valid)
		if err != nil {
			t.Fatalf("NewStore() = %v, want nil", err)
		}
		if s.Current() == nil {
			t.Fatal("Current() = nil, want a snapshot")
		}
		if got := servers(t, s.Current(), "prod"); !reflect.DeepEqual(got, []string{"10.0.0.1"}) {
			t.Fatalf("servers = %v, want [10.0.0.1]", got)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		dir, _ := storePaths(t)
		s, err := NewStore(filepath.Join(dir, "nope.json"))
		if err == nil {
			t.Fatalf("NewStore() = %+v, want error", s)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("NewStore() error = %v, want errors.Is(err, fs.ErrNotExist)", err)
		}
		if errors.Is(err, ErrInvalid) {
			t.Fatalf("NewStore() error = %v, must not wrap ErrInvalid", err)
		}
	})

	t.Run("invalid file", func(t *testing.T) {
		if _, err := NewStore(invalid); !errors.Is(err, ErrInvalid) {
			t.Fatalf("NewStore() error = %v, want errors.Is(err, ErrInvalid)", err)
		}
	})

	t.Run("directory instead of file", func(t *testing.T) {
		dir, _ := storePaths(t)
		if _, err := NewStore(dir); err == nil {
			t.Fatal("NewStore(directory) = nil, want error")
		}
	})

	t.Run("empty path", func(t *testing.T) {
		if _, err := NewStore(""); err == nil {
			t.Fatal("NewStore(\"\") = nil, want error")
		}
	})

	t.Run("oversized file", func(t *testing.T) {
		_, path := storePaths(t)
		writeFile(t, path, strings.Repeat(" ", MaxFileSize+1))
		if _, err := NewStore(path); !errors.Is(err, ErrInvalid) {
			t.Fatalf("NewStore() error = %v, want errors.Is(err, ErrInvalid)", err)
		}
	})
}

// TestStoreReload walks through the lifecycle of a configuration file and
// checks the changed flag and the served snapshot at every step.
func TestStoreReload(t *testing.T) {
	dir, path := storePaths(t)
	writeFile(t, path, `[{"clusterID":"prod","servers":["10.0.0.1"]}]`)

	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore() = %v, want nil", err)
	}

	type step struct {
		name        string
		mutate      func(t *testing.T)
		wantChanged bool
		wantInvalid bool
		wantErr     bool
		wantServers []string
	}

	steps := []step{
		{
			name:        "unchanged",
			mutate:      func(*testing.T) {},
			wantServers: []string{"10.0.0.1"},
		},
		{
			name:        "rewritten with identical content",
			mutate:      func(t *testing.T) { writeFile(t, path, `[{"clusterID":"prod","servers":["10.0.0.1"]}]`) },
			wantServers: []string{"10.0.0.1"},
		},
		{
			name:        "new valid content",
			mutate:      func(t *testing.T) { writeFile(t, path, `[{"clusterID":"prod","servers":["10.0.0.2","10.0.0.3"]}]`) },
			wantChanged: true,
			wantServers: []string{"10.0.0.2", "10.0.0.3"},
		},
		{
			name:        "invalid content keeps the previous snapshot",
			mutate:      func(t *testing.T) { writeFile(t, path, `[{"clusterID":"prod","servers":[]}]`) },
			wantInvalid: true,
			wantErr:     true,
			wantServers: []string{"10.0.0.2", "10.0.0.3"},
		},
		{
			name:        "the same invalid content is not reported again",
			mutate:      func(*testing.T) {},
			wantServers: []string{"10.0.0.2", "10.0.0.3"},
		},
		{
			name:        "another invalid content is reported",
			mutate:      func(t *testing.T) { writeFile(t, path, `not json`) },
			wantInvalid: true,
			wantErr:     true,
			wantServers: []string{"10.0.0.2", "10.0.0.3"},
		},
		{
			name:        "back to valid",
			mutate:      func(t *testing.T) { writeFile(t, path, `[{"clusterID":"prod","servers":["10.0.0.9"]}]`) },
			wantChanged: true,
			wantServers: []string{"10.0.0.9"},
		},
		{
			name: "oversized content is rejected",
			mutate: func(t *testing.T) {
				writeFile(t, path, strings.Repeat(" ", MaxFileSize+1))
			},
			wantInvalid: true,
			wantErr:     true,
			wantServers: []string{"10.0.0.9"},
		},
		{
			// The oversized content stays broken, so it is not reported twice.
			name:        "the same oversized content is not reported again",
			mutate:      func(*testing.T) {},
			wantServers: []string{"10.0.0.9"},
		},
		{
			name:        "deleted file keeps the previous snapshot",
			mutate:      func(t *testing.T) { mustRemove(t, path) },
			wantErr:     true,
			wantServers: []string{"10.0.0.9"},
		},
		{
			name:        "still deleted",
			mutate:      func(*testing.T) {},
			wantErr:     true,
			wantServers: []string{"10.0.0.9"},
		},
		{
			name:        "file comes back",
			mutate:      func(t *testing.T) { writeFile(t, path, `[{"clusterID":"prod","servers":["10.0.0.10"]}]`) },
			wantChanged: true,
			wantServers: []string{"10.0.0.10"},
		},
		{
			name:        "a broken file that is restored to the last good content reloads",
			mutate:      func(t *testing.T) { writeFile(t, path, `[{"clusterID":"prod"}]`) },
			wantInvalid: true,
			wantErr:     true,
			wantServers: []string{"10.0.0.10"},
		},
		{
			// The content is identical to the last good one, so there is
			// nothing to parse and nothing to log.
			name:        "restored to the last good content",
			mutate:      func(t *testing.T) { writeFile(t, path, `[{"clusterID":"prod","servers":["10.0.0.10"]}]`) },
			wantServers: []string{"10.0.0.10"},
		},
		{
			// Returning to valid content clears the failure memory, so the
			// same broken document must be reported again instead of being
			// suppressed as a repetition of an already logged failure.
			name:        "the same broken content after a restore is reported again",
			mutate:      func(t *testing.T) { writeFile(t, path, `[{"clusterID":"prod"}]`) },
			wantInvalid: true,
			wantErr:     true,
			wantServers: []string{"10.0.0.10"},
		},
		{
			name:        "back to valid after the repeated broken content",
			mutate:      func(t *testing.T) { writeFile(t, path, `[{"clusterID":"prod","servers":["10.0.0.10"]}]`) },
			wantServers: []string{"10.0.0.10"},
		},
		{
			name:        "the same broken content is still reported after a second restore",
			mutate:      func(t *testing.T) { writeFile(t, path, `[{"clusterID":"prod"}]`) },
			wantInvalid: true,
			wantErr:     true,
			wantServers: []string{"10.0.0.10"},
		},
		{
			name:        "a different valid content after the restore",
			mutate:      func(t *testing.T) { writeFile(t, path, `[{"clusterID":"prod","servers":["10.0.0.11"]}]`) },
			wantChanged: true,
			wantServers: []string{"10.0.0.11"},
		},
	}

	for _, tc := range steps {
		t.Run(tc.name, func(t *testing.T) {
			tc.mutate(t)
			changed, err := s.reload()
			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("reload() = %v, nil, want error", changed)
			case !tc.wantErr && err != nil:
				t.Fatalf("reload() = %v, %v, want nil", changed, err)
			}
			if tc.wantInvalid && !errors.Is(err, ErrInvalid) {
				t.Fatalf("reload() error = %v, want errors.Is(err, ErrInvalid)", err)
			}
			if changed != tc.wantChanged {
				t.Fatalf("reload() changed = %v, want %v", changed, tc.wantChanged)
			}
			if got := servers(t, s.Current(), "prod"); !reflect.DeepEqual(got, tc.wantServers) {
				t.Fatalf("Current() servers = %v, want %v", got, tc.wantServers)
			}
		})
	}

	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		t.Fatalf("stat config file: %v", err)
	}
}

func TestStoreReloadAddsCluster(t *testing.T) {
	_, path := storePaths(t)
	writeFile(t, path, `[{"clusterID":"prod","servers":["10.0.0.1"]}]`)

	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore() = %v, want nil", err)
	}

	writeFile(t, path, `[{"clusterID":"prod","servers":["10.0.0.1"]},{"clusterID":"dev","servers":["10.0.0.2"]}]`)
	changed, err := s.reload()
	if err != nil || !changed {
		t.Fatalf("reload() = %v, %v, want true, nil", changed, err)
	}
	if ids := s.Current().ClusterIDs(); !reflect.DeepEqual(ids, []string{"dev", "prod"}) {
		t.Fatalf("ClusterIDs() = %v, want [dev prod]", ids)
	}
	if got := servers(t, s.Current(), "dev"); !reflect.DeepEqual(got, []string{"10.0.0.2"}) {
		t.Fatalf("dev servers = %v, want [10.0.0.2]", got)
	}
}

// TestStoreReloadKubernetesSymlinkSwap reproduces the layout kubelet creates for
// a ConfigMap volume and swaps the ..data symlink the way kubelet does, to prove
// that polling the path picks up the new content.
func TestStoreReloadKubernetesSymlinkSwap(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "config.json")

	first := filepath.Join(dir, "..2026_01_01_10_00_00.000000000")
	mustMkdir(t, first)
	writeFile(t, filepath.Join(first, "config.json"), `[{"clusterID":"prod","servers":["10.0.0.1"]}]`)
	mustSymlink(t, filepath.Base(first), filepath.Join(dir, "..data"))
	mustSymlink(t, filepath.Join("..data", "config.json"), link)

	s, err := NewStore(link)
	if err != nil {
		t.Fatalf("NewStore() = %v, want nil", err)
	}
	if got := servers(t, s.Current(), "prod"); !reflect.DeepEqual(got, []string{"10.0.0.1"}) {
		t.Fatalf("servers = %v, want [10.0.0.1]", got)
	}

	if changed, reloadErr := s.reload(); reloadErr != nil || changed {
		t.Fatalf("reload() without a change = %v, %v, want false, nil", changed, reloadErr)
	}

	// kubelet writes the new content into a fresh timestamped directory and
	// then atomically replaces the ..data symlink.
	second := filepath.Join(dir, "..2026_01_02_10_00_00.000000000")
	mustMkdir(t, second)
	writeFile(t, filepath.Join(second, "config.json"), `[{"clusterID":"prod","servers":["10.0.0.2","10.0.0.3"]}]`)

	tmpLink := filepath.Join(dir, "..data_tmp")
	mustSymlink(t, filepath.Base(second), tmpLink)
	if renameErr := os.Rename(tmpLink, filepath.Join(dir, "..data")); renameErr != nil {
		t.Fatalf("swap ..data: %v", renameErr)
	}

	changed, err := s.reload()
	if err != nil {
		t.Fatalf("reload() = %v, want nil", err)
	}
	if !changed {
		t.Fatal("reload() after the symlink swap = false, want true")
	}
	if got := servers(t, s.Current(), "prod"); !reflect.DeepEqual(got, []string{"10.0.0.2", "10.0.0.3"}) {
		t.Fatalf("servers after the swap = %v, want [10.0.0.2 10.0.0.3]", got)
	}

	// The old directory disappearing must not matter, and a broken new
	// timestamped directory must keep the good snapshot.
	mustRemoveAll(t, first)
	if changed, reloadErr := s.reload(); reloadErr != nil || changed {
		t.Fatalf("reload() after removing the old directory = %v, %v, want false, nil", changed, reloadErr)
	}
}

func TestStoreRunStopsOnCancel(t *testing.T) {
	_, path := storePaths(t)
	writeFile(t, path, `[{"clusterID":"prod","servers":["10.0.0.1"]}]`)

	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore() = %v, want nil", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx, 5*time.Millisecond)
	}()

	writeFile(t, path, `[{"clusterID":"prod","servers":["10.0.0.2"]}]`)
	waitForServers(t, s, "prod", []string{"10.0.0.2"})

	cancel()
	waitForDone(t, done, "Run did not return within 2s after the context was cancelled")
}

func TestStoreRunStopsWithoutChanges(t *testing.T) {
	_, path := storePaths(t)
	writeFile(t, path, `[{"clusterID":"prod","servers":["10.0.0.1"]}]`)

	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore() = %v, want nil", err)
	}

	// A non-positive interval must fall back to the default without panicking,
	// and a cancelled context must stop Run immediately.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx, 0)
	}()
	waitForDone(t, done, "Run did not return for an already cancelled context")
}

func TestStoreRunKeepsWorkingAfterInvalidContent(t *testing.T) {
	_, path := storePaths(t)
	writeFile(t, path, `[{"clusterID":"prod","servers":["10.0.0.1"]}]`)

	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore() = %v, want nil", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx, time.Millisecond)
	}()

	writeFile(t, path, `[{"clusterID":"prod","servers":[]}]`)
	// Give the loop a chance to observe the broken file: the snapshot must not
	// change, and Run must keep running.
	time.Sleep(20 * time.Millisecond)
	if got := servers(t, s.Current(), "prod"); !reflect.DeepEqual(got, []string{"10.0.0.1"}) {
		t.Fatalf("servers with a broken file = %v, want [10.0.0.1]", got)
	}

	writeFile(t, path, `[{"clusterID":"prod","servers":["10.0.0.5"]}]`)
	waitForServers(t, s, "prod", []string{"10.0.0.5"})

	cancel()
	waitForDone(t, done, "Run did not return after cancel")
}

// TestStoreConcurrentAccess checks that readers never observe a partially
// updated snapshot. Run with -race.
func TestStoreConcurrentAccess(t *testing.T) {
	_, path := storePaths(t)
	writeFile(t, path, `[{"clusterID":"prod","servers":["10.0.0.1"]}]`)

	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore() = %v, want nil", err)
	}

	const readers = 8
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				cl, ok := s.Current().Get("prod")
				if !ok || len(cl.Servers) != 1 {
					t.Errorf("inconsistent snapshot: %+v, %v", cl, ok)
					return
				}
				_ = s.Current().ClusterIDs()
			}
		}()
	}

	for i := 0; i < 50; i++ {
		content := `[{"clusterID":"prod","servers":["10.0.0.1"]}]`
		if i%2 == 1 {
			content = `[{"clusterID":"prod","servers":["10.0.0.2"]}]`
		}
		writeFile(t, path, content)
		if _, err := s.reload(); err != nil {
			t.Fatalf("reload() = %v, want nil", err)
		}
	}

	close(stop)
	wg.Wait()

	// The last write wins: iteration 49 wrote the second content.
	if got := servers(t, s.Current(), "prod"); !reflect.DeepEqual(got, []string{"10.0.0.2"}) {
		t.Fatalf("servers = %v, want [10.0.0.2]", got)
	}
}

// waitForServers polls the snapshot until clusterID reports want. Only the Run
// tests poll; everywhere else reload is called directly.
func waitForServers(t *testing.T, s *Store, clusterID string, want []string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if reflect.DeepEqual(servers(t, s.Current(), clusterID), want) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s servers did not reach %v within 2s, current: %v",
		clusterID, want, servers(t, s.Current(), clusterID))
}

// waitForDone fails the test unless done is closed within 2s.
func waitForDone(t *testing.T, done <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal(msg)
	}
}

// mustMkdir creates dir, failing the test on error.
func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

// mustRemove removes path, failing the test on error.
func mustRemove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove %s: %v", path, err)
	}
}

// mustRemoveAll removes path recursively, failing the test on error.
func mustRemoveAll(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("remove all %s: %v", path, err)
	}
}

// mustSymlink creates oldname -> newname in dir, failing the test on error.
func mustSymlink(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		t.Fatalf("symlink %s -> %s: %v", newname, oldname, err)
	}
}
