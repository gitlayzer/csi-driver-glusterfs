package driver

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"time"

	"google.golang.org/grpc"
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
