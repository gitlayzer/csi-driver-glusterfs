package driver

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testStageRecord is a record of the volume of testVolumeID with everything a
// record can carry set.
func testStageRecord() stageRecord {
	return stageRecord{
		Version:           stageRecordVersion,
		VolumeID:          testVolumeID,
		StagingTargetPath: "/var/lib/kubelet/plugins/kubernetes.io/csi/staging",
		ReadOnly:          true,
		MountFlags:        []string{"acl", "log-level=INFO"},
		Targets: []recordTarget{
			{Path: "/var/lib/kubelet/pods/uid1/volumes/kubernetes.io~csi/pv/mount"},
			{Path: "/var/lib/kubelet/pods/uid2/volumes/kubernetes.io~csi/pv/mount", ReadOnly: true},
		},
	}
}

// newTestRecordStore returns a store in a fresh temporary directory.
func newTestRecordStore(t *testing.T) *recordStore {
	t.Helper()
	return &recordStore{dir: filepath.Join(t.TempDir(), "staged")}
}

// TestStageRecordValidate checks the whole validation table of a record.
func TestStageRecordValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(rec *stageRecord)
		wantErr bool
	}{
		{
			name:   "version 1",
			mutate: func(*stageRecord) {},
		},
		{
			name:   "version 2",
			mutate: func(rec *stageRecord) { rec.Version = 2 },
			// Rejected: the recovery must not guess at a layout it does not know.
			wantErr: true,
		},
		{
			name:    "version 0",
			mutate:  func(rec *stageRecord) { rec.Version = 0 },
			wantErr: true,
		},
		{
			name:    "empty volume id",
			mutate:  func(rec *stageRecord) { rec.VolumeID = "" },
			wantErr: true,
		},
		{
			name:    "malformed volume id",
			mutate:  func(rec *stageRecord) { rec.VolumeID = "garbage" },
			wantErr: true,
		},
		{
			name:    "relative staging path",
			mutate:  func(rec *stageRecord) { rec.StagingTargetPath = "staging" },
			wantErr: true,
		},
		{
			name:    "unclean staging path",
			mutate:  func(rec *stageRecord) { rec.StagingTargetPath = "/a/../b" },
			wantErr: true,
		},
		{
			name: "target with a parent element",
			mutate: func(rec *stageRecord) {
				rec.Targets = []recordTarget{{Path: "/pods/../escape"}}
			},
			wantErr: true,
		},
		{
			name: "relative target",
			mutate: func(rec *stageRecord) {
				rec.Targets = []recordTarget{{Path: "target"}}
			},
			wantErr: true,
		},
		{
			name: "no targets",
			mutate: func(rec *stageRecord) {
				rec.Targets = nil
			},
		},
		{
			name: "empty staging path",
			mutate: func(rec *stageRecord) {
				rec.StagingTargetPath = ""
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := testStageRecord()
			tc.mutate(&rec)

			err := rec.validate()
			if tc.wantErr {
				if !errors.Is(err, errInvalidRecord) {
					t.Fatalf("validate() = %v, want errInvalidRecord", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("validate() = %v, want nil", err)
			}
		})
	}
}

// TestRecordFileName checks that the file name is the hash of the volume_id, so
// that no id ever becomes a path.
func TestRecordFileName(t *testing.T) {
	tests := []struct {
		name     string
		volumeID string
	}{
		{name: "subdir volume", volumeID: testVolumeID},
		{name: "static volume", volumeID: "gfs:v1:static:c1:vol"},
		{name: "id with dots and dashes", volumeID: "gfs:v1:static:c1:vol-a.b"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name := recordFileName(tc.volumeID)

			if !strings.HasSuffix(name, recordSuffix) {
				t.Fatalf("recordFileName(%q) = %q, want the %q suffix", tc.volumeID, name, recordSuffix)
			}
			if strings.Contains(name, tc.volumeID) || strings.Contains(name, "gfs") {
				t.Fatalf("recordFileName(%q) = %q, want the id to be hashed", tc.volumeID, name)
			}
			if filepath.Base(name) != name {
				t.Fatalf("recordFileName(%q) = %q, want a bare file name", tc.volumeID, name)
			}
			if got := len(name); got != 64+len(recordSuffix) {
				t.Fatalf("recordFileName(%q) is %d characters long, want %d",
					tc.volumeID, got, 64+len(recordSuffix))
			}
			// Deterministic, so that a repeated stage overwrites its own record.
			if again := recordFileName(tc.volumeID); again != name {
				t.Fatalf("recordFileName(%q) = %q on the second call, want %q", tc.volumeID, again, name)
			}
		})
	}

	if recordFileName("gfs:v1:static:c1:vol") == recordFileName("gfs:v1:static:c1:vol2") {
		t.Fatal("two different volume ids have the same record file name")
	}
}

// TestRecordStoreRoundTrip checks that a saved record is read back unchanged, with
// the permissions the store promises.
func TestRecordStoreRoundTrip(t *testing.T) {
	store := newTestRecordStore(t)
	rec := testStageRecord()

	if err := store.save(rec); err != nil {
		t.Fatalf("save() = %v, want nil", err)
	}

	got, err := store.load(rec.VolumeID)
	if err != nil {
		t.Fatalf("load() = %v, want nil", err)
	}
	if got.Version != rec.Version || got.VolumeID != rec.VolumeID {
		t.Fatalf("load() = %+v, want %+v", got, rec)
	}
	if got.StagingTargetPath != rec.StagingTargetPath || got.ReadOnly != rec.ReadOnly {
		t.Fatalf("load() = %+v, want %+v", got, rec)
	}
	if strings.Join(got.MountFlags, ",") != strings.Join(rec.MountFlags, ",") {
		t.Fatalf("MountFlags = %v, want %v", got.MountFlags, rec.MountFlags)
	}
	if len(got.Targets) != len(rec.Targets) {
		t.Fatalf("Targets = %+v, want %+v", got.Targets, rec.Targets)
	}
	for i := range rec.Targets {
		if got.Targets[i] != rec.Targets[i] {
			t.Fatalf("Targets[%d] = %+v, want %+v", i, got.Targets[i], rec.Targets[i])
		}
	}

	t.Run("file and directory permissions", func(t *testing.T) {
		info, statErr := os.Stat(filepath.Join(store.dir, recordFileName(rec.VolumeID)))
		if statErr != nil {
			t.Fatalf("Stat() = %v, want nil", statErr)
		}
		if perm := info.Mode().Perm(); perm != recordFileMode {
			t.Fatalf("record file mode = %o, want %o", perm, recordFileMode)
		}
		dirInfo, statErr := os.Stat(store.dir)
		if statErr != nil {
			t.Fatalf("Stat(%q) = %v, want nil", store.dir, statErr)
		}
		if perm := dirInfo.Mode().Perm(); perm != recordDirMode {
			t.Fatalf("record directory mode = %o, want %o", perm, recordDirMode)
		}
	})

	t.Run("a second save replaces the record", func(t *testing.T) {
		updated := rec
		updated.Targets = nil
		if err := store.save(updated); err != nil {
			t.Fatalf("second save() = %v, want nil", err)
		}
		got, loadErr := store.load(rec.VolumeID)
		if loadErr != nil {
			t.Fatalf("load() after the second save = %v, want nil", loadErr)
		}
		if len(got.Targets) != 0 {
			t.Fatalf("Targets = %+v, want them cleared by the second save", got.Targets)
		}
		names, listErr := store.list()
		if listErr != nil {
			t.Fatalf("list() = %v, want nil", listErr)
		}
		if len(names) != 1 {
			t.Fatalf("list() = %v, want one record after two saves", names)
		}
	})

	t.Run("no temporary file is left behind", func(t *testing.T) {
		entries, readErr := os.ReadDir(store.dir)
		if readErr != nil {
			t.Fatalf("ReadDir() = %v, want nil", readErr)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), recordTempPrefix) {
				t.Fatalf("temporary file %q was left behind", entry.Name())
			}
		}
	})
}

// TestRecordStoreSaveRejects checks that the store never writes a record the
// recovery could not use.
func TestRecordStoreSaveRejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(rec *stageRecord)
	}{
		{name: "relative staging path", mutate: func(rec *stageRecord) { rec.StagingTargetPath = "staging" }},
		{name: "target with a parent element", mutate: func(rec *stageRecord) {
			rec.Targets = []recordTarget{{Path: "/a/../b"}}
		}},
		{name: "unsupported version", mutate: func(rec *stageRecord) { rec.Version = 2 }},
		{name: "malformed volume id", mutate: func(rec *stageRecord) { rec.VolumeID = "x" }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestRecordStore(t)
			rec := testStageRecord()
			tc.mutate(&rec)

			if err := store.save(rec); !errors.Is(err, errInvalidRecord) {
				t.Fatalf("save() = %v, want errInvalidRecord", err)
			}
			names, err := store.list()
			if err != nil {
				t.Fatalf("list() = %v, want nil", err)
			}
			if len(names) != 0 {
				t.Fatalf("list() = %v, want no record after a rejected save", names)
			}
		})
	}

	t.Run("a write failure removes the temporary file", func(t *testing.T) {
		store := newTestRecordStore(t)
		if err := os.MkdirAll(store.dir, recordDirMode); err != nil {
			t.Fatalf("MkdirAll() = %v, want nil", err)
		}
		// A non-empty directory where the record belongs makes the rename fail
		// after the temporary file was written.
		blocker := filepath.Join(store.dir, recordFileName(testVolumeID))
		if err := os.MkdirAll(filepath.Join(blocker, "keep"), recordDirMode); err != nil {
			t.Fatalf("MkdirAll(%q) = %v, want nil", blocker, err)
		}

		err := store.save(testStageRecord())
		if err == nil {
			t.Fatal("save() = nil, want an error for an unusable record path")
		}
		entries, readErr := os.ReadDir(store.dir)
		if readErr != nil {
			t.Fatalf("ReadDir() = %v, want nil", readErr)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), recordTempPrefix) {
				t.Fatalf("temporary file %q was left behind after a failed save", entry.Name())
			}
		}
	})

	t.Run("an unwritable directory", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatalf("WriteFile() = %v, want nil", err)
		}
		// A regular file where the directory should be makes MkdirAll fail with
		// ENOTDIR.
		store := &recordStore{dir: filepath.Join(file, "staged")}

		err := store.save(testStageRecord())
		if err == nil {
			t.Fatal("save() = nil, want an error for an uncreatable directory")
		}
		if errors.Is(err, errInvalidRecord) {
			t.Fatalf("save() = %v, want a file system error, not errInvalidRecord", err)
		}
	})
}

// TestRecordStoreLoadInvalid checks every way a record file can be unusable.
func TestRecordStoreLoadInvalid(t *testing.T) {
	valid := testStageRecord()

	tests := []struct {
		name string
		// file is the file name to write; defaults to the name of the record.
		file string
		// content is the raw file content; when empty the marshalled record is
		// written instead.
		content string
		rec     stageRecord
		// wantNotExist expects an error wrapping fs.ErrNotExist.
		wantNotExist bool
		// wantInvalid expects an error wrapping errInvalidRecord.
		wantInvalid bool
		// wantErr is a substring the error must contain.
		wantErr string
	}{
		{
			name: "valid",
			rec:  valid,
		},
		{
			name:         "renamed file",
			file:         recordFileName("gfs:v1:static:c1:other"),
			rec:          valid,
			wantInvalid:  true,
			wantNotExist: false,
		}, {
			name:        "version 2",
			rec:         func() stageRecord { r := valid; r.Version = 2; return r }(),
			wantInvalid: true,
		},
		{
			name:        "relative staging path",
			rec:         func() stageRecord { r := valid; r.StagingTargetPath = "staging"; return r }(),
			wantInvalid: true,
		},
		{
			name:        "target with a parent element",
			rec:         func() stageRecord { r := valid; r.Targets = []recordTarget{{Path: "/a/../b"}}; return r }(),
			wantInvalid: true,
		},
		{
			name:        "broken JSON",
			content:     `{"version": 1,`,
			wantInvalid: true,
		},
		{
			name:        "not an object",
			content:     `[1, 2, 3]`,
			wantInvalid: true,
		},
		{
			name:        "oversized file",
			content:     `{"version": 1, "volumeID": "` + strings.Repeat("x", recordMaxSize) + `"}`,
			wantInvalid: true,
			wantErr:     "larger than",
		},
		{
			name:         "missing file",
			file:         recordFileName(valid.VolumeID),
			wantNotExist: true,
		},
		{
			// A directory where a record is expected: opening it works, reading
			// it fails, and the failure has to be reported as an unusable record.
			name:        "a directory instead of a file",
			file:        "not-a-record",
			wantInvalid: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestRecordStore(t)
			if err := os.MkdirAll(store.dir, recordDirMode); err != nil {
				t.Fatalf("MkdirAll() = %v, want nil", err)
			}

			name := tc.file
			if name == "" {
				name = recordFileName(tc.rec.VolumeID)
			}
			switch {
			case tc.name == "missing file":
				// Nothing to write.
			case tc.name == "a directory instead of a file":
				if err := os.Mkdir(filepath.Join(store.dir, name), recordDirMode); err != nil {
					t.Fatalf("Mkdir() = %v, want nil", err)
				}
			case tc.content != "":
				if err := os.WriteFile(filepath.Join(store.dir, name), []byte(tc.content), 0o600); err != nil {
					t.Fatalf("WriteFile() = %v, want nil", err)
				}
			default:
				// Written with json.Marshal instead of save: save refuses to write a
				// record this test wants to read back.
				data, mErr := json.Marshal(tc.rec)
				if mErr != nil {
					t.Fatalf("Marshal() = %v, want nil", mErr)
				}
				if err := os.WriteFile(filepath.Join(store.dir, name), data, 0o600); err != nil {
					t.Fatalf("WriteFile() = %v, want nil", err)
				}
			}

			_, err := store.loadFile(name)

			switch {
			case tc.wantNotExist:
				if !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("loadFile(%q) = %v, want an error wrapping fs.ErrNotExist", name, err)
				}
			case tc.wantInvalid:
				if !errors.Is(err, errInvalidRecord) {
					t.Fatalf("loadFile(%q) = %v, want errInvalidRecord", name, err)
				}
				if errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("loadFile(%q) = %v, want no fs.ErrNotExist for an invalid record", name, err)
				}
			default:
				if err != nil {
					t.Fatalf("loadFile(%q) = %v, want nil", name, err)
				}
			}
			if tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("loadFile(%q) = %v, want an error containing %q", name, err, tc.wantErr)
			}
		})
	}

	t.Run("load of a volume whose file is gone", func(t *testing.T) {
		store := newTestRecordStore(t)
		_, err := store.load(valid.VolumeID)
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("load() = %v, want an error wrapping fs.ErrNotExist", err)
		}
	})
}

// TestRecordStoreRemove checks that removing a record is idempotent.
func TestRecordStoreRemove(t *testing.T) {
	store := newTestRecordStore(t)
	rec := testStageRecord()

	if err := store.remove(rec.VolumeID); err != nil {
		t.Fatalf("remove() of a missing record = %v, want nil", err)
	}

	if err := store.save(rec); err != nil {
		t.Fatalf("save() = %v, want nil", err)
	}
	if err := store.remove(rec.VolumeID); err != nil {
		t.Fatalf("remove() = %v, want nil", err)
	}
	if _, err := store.load(rec.VolumeID); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("load() after remove = %v, want fs.ErrNotExist", err)
	}
	// The second remove is the repetition of an idempotent call.
	if err := store.remove(rec.VolumeID); err != nil {
		t.Fatalf("second remove() = %v, want nil", err)
	}
}

// TestRecordStoreRemoveFailure checks that a directory that cannot be changed
// reports the failure instead of pretending the record is gone.
func TestRecordStoreRemoveFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can remove a file from a read-only directory")
	}

	store := newTestRecordStore(t)
	rec := testStageRecord()
	if err := store.save(rec); err != nil {
		t.Fatalf("save() = %v, want nil", err)
	}
	if err := os.Chmod(store.dir, 0o500); err != nil {
		t.Fatalf("Chmod() = %v, want nil", err)
	}
	t.Cleanup(func() { _ = os.Chmod(store.dir, recordDirMode) })

	if err := store.remove(rec.VolumeID); err == nil {
		t.Fatal("remove() = nil, want a permission error")
	}
}

// TestRecordStoreList checks the filtering and the ordering of the list.
func TestRecordStoreList(t *testing.T) {
	tests := []struct {
		name string
		// files are created in the record directory before list is called.
		files []string
		// dirs are created as directories.
		dirs []string
		// missing skips creating the directory at all.
		missing bool
		want    []string
	}{
		{name: "missing directory", missing: true, want: nil},
		{name: "empty directory", want: []string{}},
		{
			name:  "every record, sorted",
			files: []string{recordFileName("gfs:v1:static:c1:b"), recordFileName("gfs:v1:static:c1:a")},
			want:  nil, // filled in below, the names are hashes
		},
		{
			name:  "temporary and foreign files are ignored",
			files: []string{recordTempPrefix + "123", "notes.txt", "record.json.bak"},
			want:  []string{},
		},
		{
			name: "a directory is not a record",
			dirs: []string{recordFileName("gfs:v1:static:c1:a")},
			want: []string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestRecordStore(t)
			if !tc.missing {
				if err := os.MkdirAll(store.dir, recordDirMode); err != nil {
					t.Fatalf("MkdirAll() = %v, want nil", err)
				}
			}
			for _, name := range tc.files {
				if err := os.WriteFile(filepath.Join(store.dir, name), []byte("{}"), 0o600); err != nil {
					t.Fatalf("WriteFile(%q) = %v, want nil", name, err)
				}
			}
			for _, name := range tc.dirs {
				if err := os.Mkdir(filepath.Join(store.dir, name), recordDirMode); err != nil {
					t.Fatalf("Mkdir(%q) = %v, want nil", name, err)
				}
			}

			got, err := store.list()
			if err != nil {
				t.Fatalf("list() = %v, want nil", err)
			}

			if tc.name == "every record, sorted" {
				first, second := recordFileName("gfs:v1:static:c1:a"), recordFileName("gfs:v1:static:c1:b")
				want := []string{first, second}
				if first > second {
					want = []string{second, first}
				}
				if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
					t.Fatalf("list() = %v, want %v", got, want)
				}
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("list() = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("list() = %v, want %v", got, tc.want)
				}
			}
		})
	}

	t.Run("a directory that is a file", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatalf("WriteFile() = %v, want nil", err)
		}
		store := &recordStore{dir: file}

		if _, err := store.list(); err == nil {
			t.Fatal("list() = nil, want an error for a directory that is a file")
		}
	})
}

// TestRecordStoreNoSecrets checks that a record never contains a secret or a
// server name: the servers are looked up in the live configuration instead
// (D-20).
func TestRecordStoreNoSecrets(t *testing.T) {
	store := newTestRecordStore(t)
	rec := testStageRecord()
	if err := store.save(rec); err != nil {
		t.Fatalf("save() = %v, want nil", err)
	}

	data, err := os.ReadFile(store.path(rec.VolumeID))
	if err != nil {
		t.Fatalf("ReadFile() = %v, want nil", err)
	}
	content := string(data)
	for _, forbidden := range []string{"secret", "password", "g1", "servers", "volfile"} {
		if strings.Contains(strings.ToLower(content), forbidden) {
			t.Fatalf("record content %q contains %q", content, forbidden)
		}
	}
}

// TestWriteRecordFile checks the failure path of the file writer: a file that
// cannot be written is an error, not a silently truncated record.
func TestWriteRecordFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create() = %v, want nil", err)
	}
	// A closed file fails on every further operation, which is how a disk that
	// filled up behaves halfway through a write.
	if err := f.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}

	if err := writeRecordFile(f, []byte(`{"version":1}`)); err == nil {
		t.Fatal("writeRecordFile() = nil, want an error for a closed file")
	}
}
