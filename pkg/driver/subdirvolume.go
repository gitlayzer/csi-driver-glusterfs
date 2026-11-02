package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"time"

	"k8s.io/klog/v2"
)

// Layout of the parent volume (D-25).
const (
	// metaDirName is the directory of everything the driver keeps in a parent
	// volume. It is hidden so that it does not look like a volume directory, and
	// volume names starting with "." are refused so that a volume can never be
	// created inside it.
	metaDirName = ".glusterfs-csi"
	// volumeDirPerm is the permission of a volume directory (D-27). It is applied
	// with an explicit Chmod, because os.Mkdir is subject to the umask.
	volumeDirPerm = 0o777
	// metaDirPerm is the permission of the metadata directories. They are not
	// user data, so they are not world writable.
	metaDirPerm = 0o755
	// metaFilePerm is the permission of a metadata file. It holds the capacity and
	// the delete policy of a volume, nothing secret.
	metaFilePerm = 0o644

	// maxMetaFileSize is the largest metadata file that is read. A valid file is
	// a few dozen bytes; the limit keeps a corrupt or malicious file from being
	// read into memory.
	maxMetaFileSize = 64 << 10

	// archiveTimeLayout is the timestamp suffix of an archived volume directory
	// (D-26). Nanoseconds are included so that archiving the same name twice
	// never collides.
	archiveTimeLayout = "20060102T150405.000000000Z"

	// metaVersion is the version of the metadata layout. A file of any other
	// version is rejected instead of being guessed at: DeleteVolume must never
	// act on a file it does not fully understand.
	metaVersion = 1

	// removeVolumeDirBatch is how many directory entries removeVolumeDir reads per
	// ReadDir call, i.e. the granularity at which it reacts to a cancelled
	// context.
	removeVolumeDirBatch = 256
)

// Directories inside the hidden metadata directory of a parent volume.
//
// They are joined with [path.Join] and not with filepath.Join or a hard-coded
// separator: a name handed to an *os.Root is slash separated on every platform, so
// a Windows build would form a name a Root may reject if it used filepath.Join.
var (
	// metaVolumesDir is the directory of the metadata files.
	metaVolumesDir = path.Join(metaDirName, "volumes")
	// metaArchiveDir is the directory of the archived volume directories.
	metaArchiveDir = path.Join(metaDirName, "archive")
)

// errInvalidVolumeMeta is the sentinel wrapped by every error about the content of
// a metadata file. Errors of the file system itself (a missing file, a permission
// problem) are returned unchanged, so that a caller can tell "there is no
// metadata" from "the metadata is broken".
var errInvalidVolumeMeta = errors.New("invalid volume metadata")

// invalidMetaf builds an error wrapping [errInvalidVolumeMeta]. It never includes
// the content of a metadata file: only what was expected and what was wrong.
func invalidMetaf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errInvalidVolumeMeta, fmt.Sprintf(format, args...))
}

// volumeMeta is the content of .glusterfs-csi/volumes/<name>.json (D-25).
//
// It exists because DeleteVolume only receives a volume_id: the delete policy and
// the capacity of a volume have to be persisted, and the capacity is what a
// repeated CreateVolume of the same name is compared against.
type volumeMeta struct {
	// Version is the layout version, [metaVersion].
	Version int `json:"version"`
	// CapacityBytes is the capacity of the volume in bytes, 0 for unknown.
	CapacityBytes int64 `json:"capacityBytes"`
	// OnDelete is what DeleteVolume does with the directory.
	OnDelete deletePolicy `json:"onDelete"`
}

// validate checks the metadata after it was read and before it is written, so
// that a file the driver cannot act on is neither used nor produced.
func (m volumeMeta) validate() error {
	switch {
	case m.Version != metaVersion:
		return invalidMetaf("version %d is not supported, want %d", m.Version, metaVersion)
	case m.CapacityBytes < 0:
		return invalidMetaf("capacity %d must not be negative", m.CapacityBytes)
	case !m.OnDelete.valid():
		return invalidMetaf("onDelete %q is not one of %s", string(m.OnDelete), joinDeletePolicies())
	}
	return nil
}

// metaFileName returns the metadata file name of a volume. The name has already
// been validated, so it cannot escape the volumes directory.
func metaFileName(name string) string {
	return path.Join(metaVolumesDir, name+".json")
}

// metaTempFileName returns the name of the temporary file a metadata file is
// written to before it is renamed into place. It starts with a dot, so a crash
// leaves a file that is recognisably not metadata behind.
func metaTempFileName(name string) string {
	return path.Join(metaVolumesDir, "."+name+".json.tmp")
}

// readVolumeMeta reads and validates the metadata of a volume.
//
// A volume without a metadata file is reported as found=false and no error: that
// is the state of a directory this driver did not create (D-25). Every other
// failure - an unreadable file, a file that is not metadata, an unsupported
// version - is an error, and the caller must not touch the volume then.
func readVolumeMeta(root *os.Root, name string) (meta volumeMeta, found bool, err error) {
	f, err := root.Open(metaFileName(name))
	if errors.Is(err, fs.ErrNotExist) {
		return volumeMeta{}, false, nil
	}
	if err != nil {
		return volumeMeta{}, false, fmt.Errorf("open the metadata of volume %q: %w", name, err)
	}
	defer func() { _ = f.Close() }()

	// One byte more than the limit, so that "too large" is distinguishable from
	// "exactly at the limit".
	data, err := io.ReadAll(io.LimitReader(f, maxMetaFileSize+1))
	if err != nil {
		return volumeMeta{}, false, fmt.Errorf("read the metadata of volume %q: %w", name, err)
	}
	if len(data) > maxMetaFileSize {
		return volumeMeta{}, false, invalidMetaf("the metadata of volume %q is larger than %d bytes",
			name, maxMetaFileSize)
	}

	var m volumeMeta
	dec := json.NewDecoder(bytes.NewReader(data))
	// Unknown fields are an error: a key this driver would ignore is a key that
	// somebody expected to have an effect.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		// The decoder error names the problem, not the content of the file.
		return volumeMeta{}, false, invalidMetaf("decode the metadata of volume %q: %v", name, err)
	}
	// A second JSON value after the first one means the file was appended to or
	// written by something else.
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return volumeMeta{}, false, invalidMetaf("the metadata of volume %q has trailing content", name)
	}
	if err := m.validate(); err != nil {
		return volumeMeta{}, false, fmt.Errorf("the metadata of volume %q: %w", name, err)
	}
	return m, true, nil
}

// writeVolumeMeta writes the metadata of a volume.
//
// The file is written to a temporary file, flushed and renamed into place, so a
// crash leaves either the previous file or the new one behind and never a
// truncated one. That matters because CreateVolume writes the metadata before it
// creates the directory: an interrupted write must not look like a volume that
// exists with a broken metadata file.
func writeVolumeMeta(root *os.Root, name string, meta volumeMeta) error {
	if err := meta.validate(); err != nil {
		return fmt.Errorf("the metadata of volume %q: %w", name, err)
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("encode the metadata of volume %q: %w", name, err)
	}

	if mkErr := root.MkdirAll(metaVolumesDir, metaDirPerm); mkErr != nil {
		return fmt.Errorf("create the metadata directory of volume %q: %w", name, mkErr)
	}

	tmpName := metaTempFileName(name)
	f, err := root.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, metaFilePerm)
	if err != nil {
		return fmt.Errorf("create the metadata file of volume %q: %w", name, err)
	}
	if err := writeMetaContent(f, data); err != nil {
		removeMetaFile(root, tmpName)
		return fmt.Errorf("write the metadata of volume %q: %w", name, err)
	}
	if err := root.Rename(tmpName, metaFileName(name)); err != nil {
		removeMetaFile(root, tmpName)
		return fmt.Errorf("replace the metadata of volume %q: %w", name, err)
	}
	return nil
}

// writeMetaContent writes data to f, flushes it and closes f. Every error path
// closes f once.
func writeMetaContent(f *os.File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	// Sync before the rename: the parent volume is a network file system, and only
	// a flushed file is guaranteed to survive a crash of the client.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// removeMetaFile deletes a metadata file, ignoring the errors: the caller is
// already returning a more interesting one.
func removeMetaFile(root *os.Root, name string) {
	if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		klog.ErrorS(err, "failed to remove a temporary metadata file", "name", name)
	}
}

// removeVolumeMeta removes the metadata of a volume. A volume without metadata is
// not an error, so that a repeated DeleteVolume succeeds.
func removeVolumeMeta(root *os.Root, name string) error {
	err := root.Remove(metaFileName(name))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the metadata of volume %q: %w", name, err)
	}
	return nil
}

// volumeDirExists reports whether the directory of a volume exists.
//
// A symbolic link, a regular file or anything else that is not a directory is an
// error: the caller has to refuse to act on it rather than treat it as a volume
// this driver created.
func volumeDirExists(root *os.Root, name string) (bool, error) {
	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look for the directory of volume %q: %w", name, err)
	}
	if !info.IsDir() {
		return false, fmt.Errorf("%q in the parent volume is not a directory (mode %s)", name, info.Mode())
	}
	return true, nil
}

// ensureVolumeDir creates the directory of a volume with the permissions of D-27.
//
// The Chmod runs on every call, existing directory or not: os.Mkdir applies the
// umask, so a fresh directory is usually narrower than 0777, and a repeated
// CreateVolume is the cheapest opportunity to repair that. A path that exists but
// is not a real directory (a file, a symbolic link) is an error, never replaced.
func ensureVolumeDir(root *os.Root, name string) error {
	err := root.Mkdir(name, volumeDirPerm)
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("create the directory of volume %q: %w", name, err)
	}
	if err != nil {
		// Exists: only a real directory may be used, and it has to be the one the
		// caller asked for, not a link to somewhere else.
		exists, checkErr := volumeDirExists(root, name)
		if checkErr != nil {
			return checkErr
		}
		if !exists {
			// Unreachable: Lstat either finds the path or reports an error.
			return fmt.Errorf("the directory of volume %q disappeared", name)
		}
	}
	if err := root.Chmod(name, volumeDirPerm); err != nil {
		return fmt.Errorf("set the permission of the directory of volume %q: %w", name, err)
	}
	return nil
}

// archiveVolumeDir moves the directory of a volume into the archive (D-26).
//
// The directory keeps its content; only its name changes, so archiving a large
// volume costs a rename instead of a copy. The timestamp makes the target unique,
// which is why the archive can hold the same volume name more than once.
func archiveVolumeDir(root *os.Root, name string, now time.Time) error {
	if err := root.MkdirAll(metaArchiveDir, metaDirPerm); err != nil {
		return fmt.Errorf("create the archive directory for volume %q: %w", name, err)
	}
	target := path.Join(metaArchiveDir, name+"-"+now.UTC().Format(archiveTimeLayout))
	if err := root.Rename(name, target); err != nil {
		return fmt.Errorf("archive the directory of volume %q as %q: %w", name, target, err)
	}
	return nil
}

// removeVolumeDir deletes the directory of a volume and everything below it.
//
// The removal is batched so that a cancelled context is honoured: the driver stops
// between two top-level entries of the volume directory and leaves the rest for
// the next DeleteVolume. The granularity is one top-level entry rather than one
// file, because a volume may hold a whole tree and an RPC that cannot be finished
// should still make progress.
//
// Nothing below the volume directory is ever followed: [os.Root.RemoveAll] removes
// a symbolic link itself instead of its target and cannot leave the root the
// subdirectory was opened on.
func removeVolumeDir(ctx context.Context, root *os.Root, name string) error {
	exists, err := volumeDirExists(root, name)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}

	sub, err := root.OpenRoot(name)
	if err != nil {
		return fmt.Errorf("open the directory of volume %q: %w", name, err)
	}
	// The sub root must be closed on every path, including the error paths below.
	closed := false
	defer func() {
		if !closed {
			_ = sub.Close()
		}
	}()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, readErr := readDirNames(sub, ".")
		if readErr != nil {
			return fmt.Errorf("read the directory of volume %q: %w", name, readErr)
		}
		if len(entries) == 0 {
			break
		}
		for _, entry := range entries {
			// Checked per entry: a volume with many thousands of files must not
			// ignore a cancellation until the whole batch is gone.
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := sub.RemoveAll(entry); err != nil {
				return fmt.Errorf("remove %q from the directory of volume %q: %w", entry, name, err)
			}
		}
	}

	// Closed before the directory itself is removed: on most platforms the root
	// holds a file descriptor, and a directory that still has one is not empty from
	// the point of view of a caller that wants to remove it on some systems. The
	// error only matters if the removal below fails, so it is logged.
	closeErr := sub.Close()
	closed = true
	if closeErr != nil {
		klog.ErrorS(closeErr, "failed to close the directory of a volume", "name", name)
	}

	if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the directory of volume %q: %w", name, err)
	}
	return nil
}

// readDirNames returns up to removeVolumeDirBatch entry names of the directory
// name below dir. It opens and closes its own handle so that a cancelled context
// does not leave a descriptor open.
func readDirNames(dir *os.Root, name string) ([]string, error) {
	f, err := dir.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	entries, err := f.ReadDir(removeVolumeDirBatch)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		// The name is re-joined with the directory by RemoveAll, which rejects a
		// name that is not a single element; entry.Name never is.
		names = append(names, entry.Name())
	}
	return names, nil
}

// valid reports whether p is one of the accepted onDelete values.
func (p deletePolicy) valid() bool {
	for _, policy := range deletePolicies {
		if p == policy {
			return true
		}
	}
	return false
}
