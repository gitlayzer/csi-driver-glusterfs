package driver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"k8s.io/klog/v2"

	"github.com/gitlayzer/csi-driver-glusterfs/pkg/volumeid"
)

const (
	// stageRecordVersion is the version of the record layout. A record of any
	// other version is rejected instead of being guessed at: the recovery must
	// never act on a file it does not fully understand.
	stageRecordVersion = 1

	// recordSuffix is the file name suffix of a staging record.
	recordSuffix = ".json"
	// recordTempPrefix is the prefix of the temporary file a record is written
	// to before it is renamed into place.
	recordTempPrefix = ".tmp-"
	// recordMaxSize is the maximum size of a record file. A valid record is a
	// few hundred bytes; the limit keeps a corrupt or malicious file from being
	// read into memory.
	recordMaxSize = 64 << 10
	// recordDirMode and recordFileMode are the permissions of the record
	// directory and of a single record. The records name mount points of the
	// node and volumes, which no other user has to read.
	recordDirMode  = 0o700
	recordFileMode = 0o600
)

// errInvalidRecord is the sentinel wrapped by every error about the content of a
// staging record. The errors of the file system itself (a missing file, a
// permission problem) are returned unchanged, so that a caller can tell "the
// record is gone" from "the record is broken".
var errInvalidRecord = errors.New("invalid staging record")

// invalidRecordf builds an error wrapping [errInvalidRecord].
func invalidRecordf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errInvalidRecord, fmt.Sprintf(format, args...))
}

// stageRecord is what NodeStageVolume leaves behind for the recovery. It holds
// no secret and no server: the servers are looked up in the live
// configuration at recovery time, so that a changed server list is used.
type stageRecord struct {
	// Version is the layout version, [stageRecordVersion].
	Version int `json:"version"`
	// VolumeID is the CSI volume_id of the staged volume.
	VolumeID string `json:"volumeID"`
	// StagingTargetPath is the path of the FUSE mount.
	StagingTargetPath string `json:"stagingTargetPath"`
	// ReadOnly is the read-only flag the volume was staged with.
	ReadOnly bool `json:"readOnly"`
	// MountFlags are the mount options of the StorageClass capability. They are
	// validated again by the mounter when the volume is mounted again.
	MountFlags []string `json:"mountFlags,omitempty"`
	// Targets are the bind targets of an interrupted recovery: found before
	// the staging mount was detached and not yet bound again.
	Targets []recordTarget `json:"targets,omitempty"`
}

// recordTarget is one bind target of a staging mount in a [stageRecord].
type recordTarget struct {
	// Path is the bind mount point.
	Path string `json:"path"`
	// ReadOnly is the read-only flag of that bind mount.
	ReadOnly bool `json:"readOnly"`
}

// recordStore keeps one file per staged volume in dir.
//
// A record is the only state the driver keeps on the node. It is written after a
// successful stage and removed after a successful unstage, which is what lets the
// recovery find the volumes that need attention after a restart of the plugin
// container (D-20).
type recordStore struct {
	// dir is the directory of the record files.
	dir string
}

// recordFileName returns the file name of the record of a volume.
//
// The volume_id is hashed instead of being used as a file name: it is bounded
// (128 bytes) but contains characters that are legal in a CSI id and unusual in
// a file name, and hashing keeps the mapping fixed-length and case sensitive on
// any file system.
func recordFileName(volumeID string) string {
	sum := sha256.Sum256([]byte(volumeID))
	return hex.EncodeToString(sum[:]) + recordSuffix
}

// validate checks the record. It is called before a record is written and after
// it is read, so that a record the recovery cannot act on is never used and never
// produced.
func (r stageRecord) validate() error {
	if r.Version != stageRecordVersion {
		return invalidRecordf("version %d is not supported, want %d", r.Version, stageRecordVersion)
	}
	if _, err := volumeid.Decode(r.VolumeID); err != nil {
		return invalidRecordf("volumeID: %v", err)
	}
	if err := validatePath("stagingTargetPath", r.StagingTargetPath); err != nil {
		return invalidRecordf("%v", err)
	}
	for i, target := range r.Targets {
		if err := validatePath(fmt.Sprintf("targets[%d].path", i), target.Path); err != nil {
			return invalidRecordf("%v", err)
		}
	}
	// MountFlags is not validated here: BuildMountArgs validates the options
	// against its allowlist when the volume is mounted again, and the record is
	// only a memory of what the CO asked for.
	return nil
}

// save writes rec to the record directory.
//
// The record is written to a temporary file and renamed into place, so that a
// crash in the middle leaves either the old or the new record behind and never a
// truncated one. A record of a volume always replaces the previous one of the
// same volume.
func (s *recordStore) save(rec stageRecord) error {
	if err := rec.validate(); err != nil {
		return err
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode staging record of volume %s: %w", rec.VolumeID, err)
	}

	if mkErr := os.MkdirAll(s.dir, recordDirMode); mkErr != nil {
		return fmt.Errorf("create staging record directory %q: %w", s.dir, mkErr)
	}

	// CreateTemp creates the file with 0600 already; the Chmod is there because
	// the mode is part of the contract and not of the temporary file handling.
	tmp, err := os.CreateTemp(s.dir, recordTempPrefix+"*")
	if err != nil {
		return fmt.Errorf("create staging record file in %q: %w", s.dir, err)
	}
	name := tmp.Name()

	if err := writeRecordFile(tmp, data); err != nil {
		removeRecordFile(name)
		return err
	}
	if err := os.Rename(name, s.path(rec.VolumeID)); err != nil {
		removeRecordFile(name)
		return fmt.Errorf("replace staging record of volume %s: %w", rec.VolumeID, err)
	}

	return nil
}

// writeRecordFile writes data to f, flushes it and closes f.
func writeRecordFile(f *os.File, data []byte) error {
	name := f.Name()

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write staging record %q: %w", name, err)
	}
	// Sync before the rename: without it the rename can survive a crash while
	// the content does not, and the recovery would read a truncated record.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("flush staging record %q: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close staging record %q: %w", name, err)
	}
	if err := os.Chmod(name, recordFileMode); err != nil {
		return fmt.Errorf("change mode of staging record %q: %w", name, err)
	}
	return nil
}

// removeRecordFile deletes a temporary record file, ignoring the errors: the
// caller is already returning a more interesting one.
func removeRecordFile(name string) {
	if err := os.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		klog.ErrorS(err, "failed to remove a temporary staging record", "path", name)
	}
}

// path returns the file name of the record of a volume.
func (s *recordStore) path(volumeID string) string {
	return filepath.Join(s.dir, recordFileName(volumeID))
}

// load reads the record of a volume.
//
// A missing record is reported as an error wrapping fs.ErrNotExist: the caller
// decides whether that is a problem (the recovery treats it as "nothing to do").
func (s *recordStore) load(volumeID string) (stageRecord, error) {
	// loadFile rejects a file whose content does not hash to its own name, so the
	// record it returns always belongs to volumeID.
	return s.loadFile(recordFileName(volumeID))
}

// loadFile reads and validates the record file name inside the record directory.
//
// The name is checked against the hash of the volume_id inside the file, so that
// a renamed or swapped file is rejected instead of being applied to the wrong
// volume. Invalid content wraps [errInvalidRecord]; a missing file keeps its
// fs.ErrNotExist.
func (s *recordStore) loadFile(name string) (stageRecord, error) {
	path := filepath.Join(s.dir, name)
	f, err := os.Open(path)
	if err != nil {
		// Returned unchanged: the caller uses errors.Is(err, fs.ErrNotExist).
		return stageRecord{}, err
	}
	defer func() { _ = f.Close() }()

	// One byte more than the limit, so that "too large" is distinguishable from
	// "exactly at the limit".
	data, err := io.ReadAll(io.LimitReader(f, recordMaxSize+1))
	if err != nil {
		// A read failure is reported as an unusable record as well, but without
		// losing the cause: the caller treats every non-existing file the same
		// way, and an operator needs the underlying error in the log.
		return stageRecord{}, fmt.Errorf("staging record %q: %w: %w", path, errInvalidRecord, err)
	}
	if len(data) > recordMaxSize {
		return stageRecord{}, invalidRecordf("staging record %q is larger than %d bytes", path, recordMaxSize)
	}

	var rec stageRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return stageRecord{}, invalidRecordf("decode staging record %q: %v", path, err)
	}
	if err := rec.validate(); err != nil {
		return stageRecord{}, fmt.Errorf("staging record %q: %w", path, err)
	}
	if want := recordFileName(rec.VolumeID); want != name {
		return stageRecord{}, invalidRecordf("staging record %q belongs to volume %s (file name %q)",
			path, rec.VolumeID, want)
	}
	return rec, nil
}

// remove deletes the record of a volume. A record that does not exist is not an
// error: NodeUnstageVolume must be repeatable.
func (s *recordStore) remove(volumeID string) error {
	err := os.Remove(s.path(volumeID))
	switch {
	case err == nil, errors.Is(err, fs.ErrNotExist):
		return nil
	default:
		return fmt.Errorf("remove staging record of volume %s: %w", volumeID, err)
	}
}

// list returns the file names of all records, sorted.
//
// A missing record directory means "nothing was ever staged", which is the state
// of a freshly started node plugin, so it is not an error.
func (s *recordStore) list() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read staging record directory %q: %w", s.dir, err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, recordSuffix) {
			// The temporary files of an interrupted save and anything an
			// operator left behind are not records.
			continue
		}
		if !entry.Type().IsRegular() {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}
