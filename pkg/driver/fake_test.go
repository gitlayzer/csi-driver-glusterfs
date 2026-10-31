package driver

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/gitlayzer/csi-driver-glusterfs/pkg/config"
)

// modeCharDevice is os.ModeCharDevice, re-exported so that the fake file modes
// below read like the real ones without importing os everywhere.
const modeCharDevice = os.ModeCharDevice

// errFakeStat and errFakeLookPath are the injected failures of the readiness
// checks. They are distinct so that the tests can tell which check failed.
var (
	errFakeStat     = &fs.PathError{Op: "stat", Path: "/dev/fuse", Err: fs.ErrNotExist}
	errFakeLookPath = fmt.Errorf("look up mount.glusterfs: %w", exec.ErrNotFound)
)

// fakeFileInfo is a minimal os.FileInfo for the fake stat results.
type fakeFileInfo struct {
	name string
	mode fs.FileMode
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeFileInfo) Sys() any           { return nil }

// fakeStatFunc adapts a function returning the concrete fakeFileInfo to the
// os.Stat signature of the Driver field.
func fakeStatFunc(fn func(string) (fakeFileInfo, error)) func(string) (os.FileInfo, error) {
	return func(name string) (os.FileInfo, error) {
		info, err := fn(name)
		if err != nil {
			return nil, err
		}
		return info, nil
	}
}

// isServerStopped reports whether err is what grpc.Serve returns after Stop.
func isServerStopped(err error) bool { return errors.Is(err, grpc.ErrServerStopped) }

// testConfigJSON is the configuration document testConfigStore writes: a single
// cluster "c1" with three servers, which is what the node tests look up through
// the clusterID of their volume ids.
const testConfigJSON = `[{"clusterID":"c1","servers":["g1","g2","g3"]}]`

// testConfigStore writes testConfigJSON to a temporary file and returns a store
// for it. A node driver without a configuration is rejected by [New], so every
// test that builds one in node or all mode needs this.
func testConfigStore(t *testing.T) *config.Store {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(testConfigJSON), 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	store, err := config.NewStore(path)
	if err != nil {
		t.Fatalf("NewStore(%q) = %v, want nil", path, err)
	}
	return store
}
