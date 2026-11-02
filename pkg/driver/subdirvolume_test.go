package driver

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// testArchiveTime is the clock the archive tests use. It is in UTC and has
// nanoseconds, so that the time layout of D-26 is fully exercised.
var testArchiveTime = time.Date(2026, time.November, 2, 3, 4, 5, 6, time.UTC)

// openTestRoot opens an *os.Root on a fresh temporary directory, which stands in
// for the mount point of a parent volume.
func openTestRoot(t *testing.T) *os.Root {
	t.Helper()

	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot(%q) = %v, want nil", dir, err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Errorf("Close() = %v, want nil", err)
		}
	})
	return root
}

// mustRootPath returns the directory an *os.Root was opened on.
func mustRootPath(t *testing.T, root *os.Root) string {
	t.Helper()

	name := root.Name()
	if name == "" {
		t.Fatal("Root.Name() = empty, want the directory it was opened on")
	}
	return name
}

// writeTestFile writes content to name below dir, creating the parents.
func writeTestFile(t *testing.T, dir, name, content string) string {
	t.Helper()

	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) = %v, want nil", filepath.Dir(p), err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) = %v, want nil", p, err)
	}
	return p
}

// mustSymlink creates a symbolic link and skips the test on a platform where that
// needs privileges (Windows without developer mode).
func mustSymlink(t *testing.T, oldname, newname string) {
	t.Helper()

	if err := os.Symlink(oldname, newname); err != nil {
		t.Skipf("cannot create a symbolic link on %s: %v", runtime.GOOS, err)
	}
}

// metaFileMode returns the mode of a file below dir.
func metaFileMode(t *testing.T, dir, name string) os.FileMode {
	t.Helper()

	info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("Stat(%q) = %v, want nil", name, err)
	}
	return info.Mode().Perm()
}

func TestVolumeMetaRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		meta volumeMeta
	}{
		{
			name: "delete with a capacity",
			meta: volumeMeta{Version: metaVersion, CapacityBytes: 1 << 30, OnDelete: deletePolicyDelete},
		},
		{
			name: "retain without a capacity",
			meta: volumeMeta{Version: metaVersion, OnDelete: deletePolicyRetain},
		},
		{
			name: "archive",
			meta: volumeMeta{Version: metaVersion, CapacityBytes: 42, OnDelete: deletePolicyArchive},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := openTestRoot(t)

			if err := writeVolumeMeta(root, "pvc-1", tc.meta); err != nil {
				t.Fatalf("writeVolumeMeta() = %v, want nil", err)
			}
			got, found, err := readVolumeMeta(root, "pvc-1")
			if err != nil {
				t.Fatalf("readVolumeMeta() = %v, want nil", err)
			}
			if !found {
				t.Fatal("readVolumeMeta() found = false, want true")
			}
			if got != tc.meta {
				t.Fatalf("readVolumeMeta() = %+v, want %+v", got, tc.meta)
			}
		})
	}
}

func TestWriteVolumeMeta(t *testing.T) {
	root := openTestRoot(t)
	dir := mustRootPath(t, root)

	if err := writeVolumeMeta(root, "pvc-1", volumeMeta{Version: metaVersion, OnDelete: deletePolicyDelete}); err != nil {
		t.Fatalf("writeVolumeMeta() = %v, want nil", err)
	}

	// The file is at the documented place, with the documented permission, and no
	// temporary file is left behind.
	if got := metaFileMode(t, dir, metaFileName("pvc-1")); got != metaFilePerm {
		t.Fatalf("metadata mode = %o, want %o", got, metaFilePerm)
	}
	if got := metaFileMode(t, dir, metaVolumesDir); got != metaDirPerm {
		t.Fatalf("metadata directory mode = %o, want %o", got, metaDirPerm)
	}
	checkNoTempFiles(t, filepath.Join(dir, filepath.FromSlash(metaVolumesDir)))

	// A second write replaces the content instead of appending to it.
	second := volumeMeta{Version: metaVersion, CapacityBytes: 7, OnDelete: deletePolicyRetain}
	if err := writeVolumeMeta(root, "pvc-1", second); err != nil {
		t.Fatalf("writeVolumeMeta() = %v, want nil", err)
	}
	got, found, err := readVolumeMeta(root, "pvc-1")
	if err != nil || !found {
		t.Fatalf("readVolumeMeta() = %+v, %t, %v, want the new metadata", got, found, err)
	}
	if got != second {
		t.Fatalf("readVolumeMeta() = %+v, want %+v", got, second)
	}
	checkNoTempFiles(t, filepath.Join(dir, filepath.FromSlash(metaVolumesDir)))
}

// checkNoTempFiles fails the test when dir holds a temporary metadata file.
func checkNoTempFiles(t *testing.T, dir string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q) = %v, want nil", dir, err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("%q is a leftover temporary metadata file", entry.Name())
		}
	}
}

func TestWriteVolumeMetaErrors(t *testing.T) {
	tests := []struct {
		name    string
		meta    volumeMeta
		prepare func(t *testing.T, dir string)
		wantErr string
	}{
		{
			name:    "unsupported version",
			meta:    volumeMeta{Version: 2, OnDelete: deletePolicyDelete},
			wantErr: "version 2 is not supported",
		},
		{
			name:    "unknown delete policy",
			meta:    volumeMeta{Version: metaVersion, OnDelete: "squash"},
			wantErr: "onDelete",
		},
		{
			name:    "negative capacity",
			meta:    volumeMeta{Version: metaVersion, CapacityBytes: -1, OnDelete: deletePolicyDelete},
			wantErr: "must not be negative",
		},
		{
			name: "the metadata directory is a regular file",
			meta: volumeMeta{Version: metaVersion, OnDelete: deletePolicyDelete},
			prepare: func(t *testing.T, dir string) {
				writeTestFile(t, dir, metaVolumesDir, "not a directory")
			},
			wantErr: "create the metadata directory",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := openTestRoot(t)
			if tc.prepare != nil {
				tc.prepare(t, mustRootPath(t, root))
			}

			err := writeVolumeMeta(root, "pvc-1", tc.meta)
			if err == nil {
				t.Fatalf("writeVolumeMeta() = nil, want an error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("writeVolumeMeta() = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestReadVolumeMeta(t *testing.T) {
	tests := []struct {
		name string
		// content is written to the metadata file; nil means no file at all.
		content *string
		// prepare runs before the read and may create other files as well.
		prepare func(t *testing.T, dir string)
		found   bool
		wantErr string
	}{
		{
			name:  "no metadata file",
			found: false,
		},
		{
			name:    "valid metadata",
			found:   true,
			content: strPtr(`{"version":1,"capacityBytes":1024,"onDelete":"archive"}`),
		},
		{
			name:    "unknown field",
			found:   false,
			content: strPtr(`{"version":1,"capacityBytes":1,"onDelete":"delete","quota":true}`),
			wantErr: "unknown field",
		},
		{
			name:    "trailing content",
			found:   false,
			content: strPtr(`{"version":1,"capacityBytes":1,"onDelete":"delete"}{"version":1}`),
			wantErr: "trailing content",
		},
		{
			name:    "unsupported version",
			found:   false,
			content: strPtr(`{"version":2,"capacityBytes":1,"onDelete":"delete"}`),
			wantErr: "version 2 is not supported",
		},
		{
			name:    "unknown delete policy",
			found:   false,
			content: strPtr(`{"version":1,"capacityBytes":1,"onDelete":"Delete"}`),
			wantErr: "onDelete",
		},
		{
			name:    "negative capacity",
			found:   false,
			content: strPtr(`{"version":1,"capacityBytes":-5,"onDelete":"delete"}`),
			wantErr: "must not be negative",
		},
		{
			name:    "not JSON",
			found:   false,
			content: strPtr("version: 1\n"),
			wantErr: "decode the metadata",
		},
		{
			name:    "empty file",
			found:   false,
			content: strPtr(""),
			wantErr: "decode the metadata",
		},
		{
			name:    "larger than the limit",
			found:   false,
			content: strPtr(`{"version":1,"capacityBytes":1,"onDelete":"delete"}` + strings.Repeat(" ", maxMetaFileSize)),
			wantErr: "larger than",
		},
		{
			name: "the metadata path is a directory",
			prepare: func(t *testing.T, dir string) {
				if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(metaFileName("pvc-1"))), 0o755); err != nil {
					t.Fatalf("MkdirAll() = %v, want nil", err)
				}
			},
			wantErr: "read the metadata",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := openTestRoot(t)
			dir := mustRootPath(t, root)
			if tc.content != nil {
				writeTestFile(t, dir, metaFileName("pvc-1"), *tc.content)
			}
			if tc.prepare != nil {
				tc.prepare(t, dir)
			}

			meta, found, err := readVolumeMeta(root, "pvc-1")
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("readVolumeMeta() = %+v, %t, nil, want an error", meta, found)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("readVolumeMeta() = %q, want it to contain %q", err, tc.wantErr)
				}
				if found {
					t.Fatal("readVolumeMeta() found = true with an error, want false")
				}
				if meta != (volumeMeta{}) {
					t.Fatalf("readVolumeMeta() = %+v with an error, want the zero value", meta)
				}
				return
			}
			if err != nil {
				t.Fatalf("readVolumeMeta() = %v, want nil", err)
			}
			if found != tc.found {
				t.Fatalf("readVolumeMeta() found = %t, want %t", found, tc.found)
			}
		})
	}
}

// strPtr returns a pointer to a copy of s, for a table entry that needs a value.
func strPtr(s string) *string { return &s }

func TestRemoveVolumeMeta(t *testing.T) {
	root := openTestRoot(t)
	dir := mustRootPath(t, root)
	writeTestFile(t, dir, metaFileName("pvc-1"), `{"version":1,"onDelete":"delete"}`)

	if err := removeVolumeMeta(root, "pvc-1"); err != nil {
		t.Fatalf("removeVolumeMeta() = %v, want nil", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(metaFileName("pvc-1")))); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the metadata file still exists (%v), want it removed", err)
	}

	// A volume without metadata is not an error: DeleteVolume has to be repeatable.
	if err := removeVolumeMeta(root, "pvc-1"); err != nil {
		t.Fatalf("removeVolumeMeta() on a missing file = %v, want nil", err)
	}
}

func TestVolumeDirExists(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, dir string)
		want    bool
		wantErr bool
	}{
		{
			name:    "missing",
			prepare: func(*testing.T, string) {},
			want:    false,
		},
		{
			name: "directory",
			prepare: func(t *testing.T, dir string) {
				if err := os.Mkdir(filepath.Join(dir, "pvc-1"), 0o777); err != nil {
					t.Fatalf("Mkdir() = %v, want nil", err)
				}
			},
			want: true,
		},
		{
			name: "regular file",
			prepare: func(t *testing.T, dir string) {
				writeTestFile(t, dir, "pvc-1", "data")
			},
			wantErr: true,
		},
		{
			name: "symbolic link to a directory",
			prepare: func(t *testing.T, dir string) {
				if err := os.Mkdir(filepath.Join(dir, "target"), 0o777); err != nil {
					t.Fatalf("Mkdir() = %v, want nil", err)
				}
				mustSymlink(t, "target", filepath.Join(dir, "pvc-1"))
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := openTestRoot(t)
			tc.prepare(t, mustRootPath(t, root))

			got, err := volumeDirExists(root, "pvc-1")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("volumeDirExists() = %t, nil, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("volumeDirExists() = %v, want nil", err)
			}
			if got != tc.want {
				t.Fatalf("volumeDirExists() = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestEnsureVolumeDir(t *testing.T) {
	tests := []struct {
		name string
		// prepare runs before the call; perm is the mode of the path afterwards.
		prepare func(t *testing.T, dir string)
		wantErr string
	}{
		{
			name:    "creates a missing directory",
			prepare: func(*testing.T, string) {},
		},
		{
			name: "repairs the permission of an existing directory",
			prepare: func(t *testing.T, dir string) {
				if err := os.Mkdir(filepath.Join(dir, "pvc-1"), 0o700); err != nil {
					t.Fatalf("Mkdir() = %v, want nil", err)
				}
			},
		},
		{
			name: "a regular file in the way is an error",
			prepare: func(t *testing.T, dir string) {
				writeTestFile(t, dir, "pvc-1", "data")
			},
			wantErr: "is not a directory",
		},
		{
			name: "a symbolic link in the way is an error",
			prepare: func(t *testing.T, dir string) {
				if err := os.Mkdir(filepath.Join(dir, "target"), 0o777); err != nil {
					t.Fatalf("Mkdir() = %v, want nil", err)
				}
				mustSymlink(t, "target", filepath.Join(dir, "pvc-1"))
			},
			wantErr: "is not a directory",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := openTestRoot(t)
			dir := mustRootPath(t, root)
			tc.prepare(t, dir)

			err := ensureVolumeDir(root, "pvc-1")
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("ensureVolumeDir() = nil, want an error containing %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ensureVolumeDir() = %q, want it to contain %q", err, tc.wantErr)
				}
				// Nothing may be replaced by the failure.
				if _, statErr := os.Lstat(filepath.Join(dir, "pvc-1")); statErr != nil {
					t.Fatalf("the path was removed by the failure: %v", statErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ensureVolumeDir() = %v, want nil", err)
			}
			// D-27: 0777 whatever the umask is, on the first and on every later call.
			for i := 0; i < 2; i++ {
				info, statErr := os.Stat(filepath.Join(dir, "pvc-1"))
				if statErr != nil {
					t.Fatalf("Stat() = %v, want nil", statErr)
				}
				if got := info.Mode().Perm(); got != volumeDirPerm {
					t.Fatalf("volume directory mode = %o, want %o", got, volumeDirPerm)
				}
				if err := ensureVolumeDir(root, "pvc-1"); err != nil {
					t.Fatalf("ensureVolumeDir() on an existing directory = %v, want nil", err)
				}
			}
		})
	}
}

// TestEnsureVolumeDirIgnoresUmask checks the explicit Chmod of D-27: os.Mkdir is
// subject to the umask, so a fresh volume directory would be narrower than 0777
// without it.
//
// The umask is measured instead of set: setting it needs a platform specific call,
// and this test file has to build on Windows as well. A process without a umask
// (very unusual) skips the check rather than making it vacuous.
func TestEnsureVolumeDirIgnoresUmask(t *testing.T) {
	root := openTestRoot(t)
	dir := mustRootPath(t, root)

	umask, err := effectiveUmask(dir)
	if err != nil {
		t.Fatalf("effectiveUmask() = %v, want nil", err)
	}
	if umask == 0 {
		t.Skip("the process has no umask, so a missing Chmod is not observable")
	}

	if err := ensureVolumeDir(root, "pvc-1"); err != nil {
		t.Fatalf("ensureVolumeDir() = %v, want nil", err)
	}
	if got := metaFileMode(t, dir, "pvc-1"); got != volumeDirPerm {
		t.Fatalf("volume directory mode = %o, want %o with umask %o", got, volumeDirPerm, umask)
	}
}

// effectiveUmask measures the umask of the process by creating a file with mode
// 0777 in dir and reporting the bits that were removed.
func effectiveUmask(dir string) (os.FileMode, error) {
	name := filepath.Join(dir, "umask-probe")
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o777)
	if err != nil {
		return 0, err
	}
	if closeErr := f.Close(); closeErr != nil {
		return 0, closeErr
	}
	info, err := os.Stat(name)
	if err != nil {
		return 0, err
	}
	if err := os.Remove(name); err != nil {
		return 0, err
	}
	return volumeDirPerm &^ info.Mode().Perm(), nil
}

func TestArchiveVolumeDir(t *testing.T) {
	root := openTestRoot(t)
	dir := mustRootPath(t, root)
	volumeDir := filepath.Join(dir, "pvc-1")
	writeTestFile(t, volumeDir, "data/file.txt", "payload")

	if err := archiveVolumeDir(root, "pvc-1", testArchiveTime); err != nil {
		t.Fatalf("archiveVolumeDir() = %v, want nil", err)
	}

	// D-26: the name carries the UTC timestamp with nanoseconds, and the archive
	// lives in the hidden directory, not in the root of the parent volume.
	wantName := "pvc-1-20261102T030405.000000006Z"
	want := filepath.Join(dir, filepath.FromSlash(metaArchiveDir), wantName)
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("Stat(%q) = %v, want the archived directory", want, err)
	}
	if _, err := os.Lstat(volumeDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the source directory still exists (%v), want it renamed", err)
	}
	content, err := os.ReadFile(filepath.Join(want, "data", "file.txt"))
	if err != nil {
		t.Fatalf("ReadFile() = %v, want the archived content", err)
	}
	if string(content) != "payload" {
		t.Fatalf("archived content = %q, want %q", content, "payload")
	}
	if entries, readErr := os.ReadDir(dir); readErr != nil || !slices.Equal(namesOf(entries), []string{metaDirName}) {
		t.Fatalf("parent volume root = %v (%v), want only %q", namesOf(entries), readErr, metaDirName)
	}

	// Archiving the same name again does not collide with the first archive.
	if err := os.Mkdir(volumeDir, 0o777); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}
	later := testArchiveTime.Add(time.Second)
	if err := archiveVolumeDir(root, "pvc-1", later); err != nil {
		t.Fatalf("archiveVolumeDir() = %v, want nil", err)
	}
	wantLater := filepath.Join(dir, filepath.FromSlash(metaArchiveDir), "pvc-1-20261102T030406.000000006Z")
	if _, err := os.Stat(wantLater); err != nil {
		t.Fatalf("Stat(%q) = %v, want the second archive", wantLater, err)
	}
}

// namesOf returns the names of directory entries.
func namesOf(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestRemoveVolumeDir(t *testing.T) {
	tests := []struct {
		name string
		// prepare creates the state below the mount point.
		prepare func(t *testing.T, dir string)
		// ctx is the context of the call, nil for a live one.
		ctx context.Context
		// wantErr is a substring of the expected error.
		wantErr string
		// wantContext is the context error the call must return.
		wantContext error
		// wantKept is true when the volume path must still exist afterwards.
		wantKept bool
		// wantGone are paths relative to the mount point that must be gone.
		wantGone []string
		// wantThere are paths relative to the mount point that must still exist.
		wantThere []string
	}{
		{
			name: "removes a tree",
			prepare: func(t *testing.T, dir string) {
				writeTestFile(t, dir, "pvc-1/a/b/c.txt", "deep")
				writeTestFile(t, dir, "pvc-1/d.txt", "flat")
				if err := os.MkdirAll(filepath.Join(dir, "pvc-1", "empty"), 0o777); err != nil {
					t.Fatalf("MkdirAll() = %v, want nil", err)
				}
			},
			wantGone: []string{"pvc-1"},
		},
		{
			name: "removes more entries than one batch",
			prepare: func(t *testing.T, dir string) {
				for i := 0; i < removeVolumeDirBatch+5; i++ {
					writeTestFile(t, dir, "pvc-1/file-"+itoa(i), "x")
				}
			},
			wantGone: []string{"pvc-1"},
		},
		{
			name:    "a missing directory is not an error",
			prepare: func(*testing.T, string) {},
		},
		{
			name: "a symbolic link is removed without its target",
			prepare: func(t *testing.T, dir string) {
				if err := os.MkdirAll(filepath.Join(dir, "pvc-1"), 0o777); err != nil {
					t.Fatalf("MkdirAll() = %v, want nil", err)
				}
				writeTestFile(t, dir, "other/keep.txt", "keep")
				mustSymlink(t, "../other", filepath.Join(dir, "pvc-1", "link"))
			},
			wantGone:  []string{"pvc-1"},
			wantThere: []string{"other/keep.txt"},
		},
		{
			name: "a cancelled context keeps the directory",
			prepare: func(t *testing.T, dir string) {
				writeTestFile(t, dir, "pvc-1/file.txt", "x")
			},
			ctx:         cancelledContext(),
			wantContext: context.Canceled,
			wantKept:    true,
			wantThere:   []string{"pvc-1/file.txt"},
		},
		{
			name: "a regular file with the volume name is an error",
			prepare: func(t *testing.T, dir string) {
				writeTestFile(t, dir, "pvc-1", "not a directory")
			},
			wantErr:   "is not a directory",
			wantThere: []string{"pvc-1"},
		},
		{
			name: "a symbolic link with the volume name is an error",
			prepare: func(t *testing.T, dir string) {
				if err := os.MkdirAll(filepath.Join(dir, "other"), 0o777); err != nil {
					t.Fatalf("MkdirAll() = %v, want nil", err)
				}
				writeTestFile(t, dir, "other/keep.txt", "keep")
				mustSymlink(t, "other", filepath.Join(dir, "pvc-1"))
			},
			wantErr:   "is not a directory",
			wantThere: []string{"pvc-1", "other/keep.txt"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := openTestRoot(t)
			dir := mustRootPath(t, root)
			tc.prepare(t, dir)

			ctx := tc.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			err := removeVolumeDir(ctx, root, "pvc-1")

			switch {
			case tc.wantContext != nil:
				if !errors.Is(err, tc.wantContext) {
					t.Fatalf("removeVolumeDir() = %v, want %v", err, tc.wantContext)
				}
			case tc.wantErr != "":
				if err == nil {
					t.Fatalf("removeVolumeDir() = nil, want an error containing %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("removeVolumeDir() = %q, want it to contain %q", err, tc.wantErr)
				}
			default:
				if err != nil {
					t.Fatalf("removeVolumeDir() = %v, want nil", err)
				}
			}

			if tc.wantKept {
				if _, statErr := os.Lstat(filepath.Join(dir, "pvc-1")); statErr != nil {
					t.Fatalf("the volume directory is gone (%v), want it kept", statErr)
				}
			}
			for _, gone := range tc.wantGone {
				if _, statErr := os.Lstat(filepath.Join(dir, filepath.FromSlash(gone))); !errors.Is(statErr, fs.ErrNotExist) {
					t.Fatalf("%q still exists (%v), want it removed", gone, statErr)
				}
			}
			for _, there := range tc.wantThere {
				if _, statErr := os.Lstat(filepath.Join(dir, filepath.FromSlash(there))); statErr != nil {
					t.Fatalf("%q is gone (%v), want it kept", there, statErr)
				}
			}
		})
	}
}

// cancelAfterContext is a context whose Err() reports nil a fixed number of times
// before it reports [context.Canceled].
//
// It exists to cancel a removal in the middle of its loop: the checks removeVolumeDir
// does are the only thing that looks at the context, so counting the calls is how a
// test reaches the check inside the loop instead of the one before it.
type cancelAfterContext struct {
	// allowed is the number of Err calls that still report nil.
	allowed int
	// calls is the number of Err calls so far.
	calls int
}

// Deadline reports no deadline, like the background context.
func (c *cancelAfterContext) Deadline() (time.Time, bool) { return time.Time{}, false }

// Done returns a channel that is closed once the context reports its cancellation.
func (c *cancelAfterContext) Done() <-chan struct{} {
	if c.calls > c.allowed {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return nil
}

// Err reports nil until the allowance is used up and [context.Canceled] afterwards.
func (c *cancelAfterContext) Err() error {
	c.calls++
	if c.calls > c.allowed {
		return context.Canceled
	}
	return nil
}

// Value returns nil: the context carries no values.
func (c *cancelAfterContext) Value(any) any { return nil }

// TestRemoveVolumeDirStopsBetweenEntries checks the cancellation granularity of the
// removal: a context that ends while the entries of the volume directory are being
// removed stops the loop, and the next DeleteVolume continues with the rest.
func TestRemoveVolumeDirStopsBetweenEntries(t *testing.T) {
	root := openTestRoot(t)
	dir := mustRootPath(t, root)
	entries := []string{"a", "b", "c"}
	for _, entry := range entries {
		writeTestFile(t, dir, "pvc-1/"+entry, "x")
	}

	// One call for the check before the loop, one for the check of the batch and one
	// for the check of the first entry: after that the context is gone.
	ctx := &cancelAfterContext{allowed: 3}
	err := removeVolumeDir(ctx, root, "pvc-1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("removeVolumeDir() = %v, want context.Canceled", err)
	}

	// The removal stopped in the middle: the directory is still there, with at least
	// one entry left for the next call.
	if _, statErr := os.Lstat(filepath.Join(dir, "pvc-1")); statErr != nil {
		t.Fatalf("the volume directory is gone (%v), want it kept for the next call", statErr)
	}
	left, readErr := os.ReadDir(filepath.Join(dir, "pvc-1"))
	if readErr != nil {
		t.Fatalf("ReadDir() = %v, want nil", readErr)
	}
	if len(left) == 0 || len(left) == len(entries) {
		t.Fatalf("%d entries are left, want a partial removal", len(left))
	}

	// A retried DeleteVolume finishes the job.
	if err := removeVolumeDir(context.Background(), root, "pvc-1"); err != nil {
		t.Fatalf("the repeated removeVolumeDir() = %v, want nil", err)
	}
	if _, statErr := os.Lstat(filepath.Join(dir, "pvc-1")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("the volume directory still exists (%v), want it removed", statErr)
	}
}

// cancelledContext returns a context that is already cancelled.
func cancelledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = ctx.Err()
	return ctx
}

// itoa renders a small non-negative number without importing strconv, which the
// test file otherwise does not need.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// TestMetaPathsAreBelowTheHiddenDirectory checks the layout of D-25: everything the
// driver keeps lives in .glusterfs-csi, so the root of a parent volume only holds
// volume directories.
func TestMetaPathsAreBelowTheHiddenDirectory(t *testing.T) {
	for name, p := range map[string]string{
		"volumes":  metaVolumesDir,
		"archive":  metaArchiveDir,
		"metadata": metaFileName("pvc-1"),
		"temp":     metaTempFileName("pvc-1"),
	} {
		if path.Dir(p) != metaDirName && !strings.HasPrefix(p, metaDirName+"/") {
			t.Fatalf("%s path %q is not below %q", name, p, metaDirName)
		}
		if strings.Contains(p, `\`) {
			t.Fatalf("%s path %q uses a platform separator", name, p)
		}
	}
	if got := path.Base(metaFileName("pvc-1")); got != "pvc-1.json" {
		t.Fatalf("metadata file name = %q, want pvc-1.json", got)
	}
	if got := path.Base(metaTempFileName("pvc-1")); got != ".pvc-1.json.tmp" {
		t.Fatalf("temporary metadata file name = %q, want .pvc-1.json.tmp", got)
	}
}

// TestArchiveVolumeDirErrors checks that a failure is reported and nothing is
// moved, which is what DeleteVolume relies on: the metadata must survive a failed
// archive.
func TestArchiveVolumeDirErrors(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, dir string)
		wantErr string
	}{
		{
			name: "the archive directory cannot be created",
			prepare: func(t *testing.T, dir string) {
				writeTestFile(t, dir, metaArchiveDir, "not a directory")
				if err := os.Mkdir(filepath.Join(dir, "pvc-1"), 0o777); err != nil {
					t.Fatalf("Mkdir() = %v, want nil", err)
				}
			},
			wantErr: "create the archive directory",
		},
		{
			name:    "the volume directory does not exist",
			prepare: func(*testing.T, string) {},
			wantErr: "archive the directory",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := openTestRoot(t)
			tc.prepare(t, mustRootPath(t, root))

			err := archiveVolumeDir(root, "pvc-1", testArchiveTime)
			if err == nil {
				t.Fatalf("archiveVolumeDir() = nil, want an error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("archiveVolumeDir() = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestRemoveVolumeDirRemovalFailure checks the last step of the removal: the empty
// directory itself has to go, and a failure there is reported.
func TestRemoveVolumeDirRemovalFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("removing a directory that is in use does not fail the same way on Windows")
	}
	root := openTestRoot(t)
	dir := mustRootPath(t, root)
	volumeDir := filepath.Join(dir, "pvc-1")
	if err := os.Mkdir(volumeDir, 0o555); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}

	// The directory is empty, so every entry of it was removed; the parent is not
	// writable, so the final Remove cannot succeed.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("Chmod() = %v, want nil", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	err := removeVolumeDir(context.Background(), root, "pvc-1")
	if err == nil {
		t.Skip("the test process can remove a directory from a read-only parent, nothing to assert")
	}
	if !strings.Contains(err.Error(), "remove the directory of volume") {
		t.Fatalf("removeVolumeDir() = %q, want it to name the removal", err)
	}
}

// TestRemoveVolumeDirFirstEntryOnly checks the cancellation granularity: a cancelled
// context stops the removal of the entries below the volume directory, and the
// volume directory itself is kept for the next DeleteVolume.
func TestRemoveVolumeDirFirstEntryOnly(t *testing.T) {
	root := openTestRoot(t)
	dir := mustRootPath(t, root)
	if err := os.Mkdir(filepath.Join(dir, "pvc-1"), 0o777); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}
	// A directory with an entry: the loop has to look at it, and the cancelled
	// context is seen before anything is removed.
	writeTestFile(t, dir, "pvc-1/file.txt", "payload")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := removeVolumeDir(ctx, root, "pvc-1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("removeVolumeDir() = %v, want context.Canceled", err)
	}
	if _, statErr := os.Lstat(filepath.Join(dir, "pvc-1", "file.txt")); statErr != nil {
		t.Fatalf("the entry was removed (%v), want it kept", statErr)
	}
}

// TestRemoveVolumeMetaFailure checks the error path: a metadata path that is not a
// file cannot be removed, and that is an error rather than success.
func TestRemoveVolumeMetaFailure(t *testing.T) {
	root := openTestRoot(t)
	dir := mustRootPath(t, root)
	if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(metaFileName("pvc-1"))), 0o755); err != nil {
		t.Fatalf("MkdirAll() = %v, want nil", err)
	}
	writeTestFile(t, filepath.Join(dir, filepath.FromSlash(metaFileName("pvc-1"))), "keep.txt", "x")

	err := removeVolumeMeta(root, "pvc-1")
	if err == nil {
		t.Fatal("removeVolumeMeta() = nil, want an error for a non-empty directory")
	}
	if !strings.Contains(err.Error(), "remove the metadata") {
		t.Fatalf("removeVolumeMeta() = %q, want it to name the metadata", err)
	}
}

// TestReadVolumeMetaCloseErrorIsIgnored checks that the file handle of a metadata
// file is closed on every path: the read helper is called on every CreateVolume and
// DeleteVolume, so a leaked descriptor would accumulate.
func TestReadVolumeMetaClosesTheFile(t *testing.T) {
	root := openTestRoot(t)
	writeTestFile(t, mustRootPath(t, root), metaFileName("pvc-1"),
		`{"version":1,"capacityBytes":1,"onDelete":"delete"}`)

	for i := 0; i < 64; i++ {
		if _, found, err := readVolumeMeta(root, "pvc-1"); err != nil || !found {
			t.Fatalf("readVolumeMeta() = %t, %v, want the metadata", found, err)
		}
	}
}

// TestWriteVolumeMetaRemovesTempFile checks the cleanup of the temporary file when
// the rename into place fails: a leftover temporary file would pile up on every
// retry of a broken CreateVolume.
func TestWriteVolumeMetaRemovesTempFile(t *testing.T) {
	root := openTestRoot(t)
	dir := mustRootPath(t, root)
	// The destination of the rename is a non-empty directory, which no file can
	// replace.
	target := filepath.Join(dir, filepath.FromSlash(metaFileName("pvc-1")))
	writeTestFile(t, target, "keep.txt", "x")

	err := writeVolumeMeta(root, "pvc-1", volumeMeta{Version: metaVersion, OnDelete: deletePolicyDelete})
	if err == nil {
		t.Fatal("writeVolumeMeta() = nil, want an error for a destination it cannot replace")
	}
	if !strings.Contains(err.Error(), "replace the metadata") {
		t.Fatalf("writeVolumeMeta() = %q, want it to name the rename", err)
	}
	checkNoTempFiles(t, filepath.Join(dir, filepath.FromSlash(metaVolumesDir)))

	// The existing path is untouched: the driver never removes something it did not
	// write.
	if content, readErr := os.ReadFile(filepath.Join(target, "keep.txt")); readErr != nil || string(content) != "x" {
		t.Fatalf("destination content = %q (%v), want it untouched", content, readErr)
	}
}
