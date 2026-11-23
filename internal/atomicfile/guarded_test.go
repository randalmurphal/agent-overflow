package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// readVersion reads path as a read-modify-write would and fails the test
// when it cannot.
func readVersion(t *testing.T, path string) os.FileInfo {
	t.Helper()
	_, info, err := ReadWithStat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func writeRaw(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// expectRefused asserts that a write against a stale read wrote nothing:
// the file still reads want (or is absent when want is ""), and no staged
// temporary file is left behind.
func expectRefused(t *testing.T, path, want string, before os.FileInfo) {
	t.Helper()
	committed, err := WriteIfUnchanged(path, []byte("mine"), before)
	if err != nil || committed {
		t.Fatalf("WriteIfUnchanged = %v, %v; want refused", committed, err)
	}
	got, err := os.ReadFile(path)
	switch {
	case want == "" && !errors.Is(err, os.ErrNotExist):
		t.Fatalf("the file is %q (%v); want it still absent", got, err)
	case want != "" && string(got) != want:
		t.Fatalf("the other writer's file became %q (%v)", got, err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Fatalf("a refused write left %s behind", entry.Name())
		}
	}
}

func TestWriteIfUnchangedRefusesAFileChangedSinceTheRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	writeRaw(t, path, "v1")
	before := readVersion(t, path)
	// Another writer rewrites the file; the mtime moves even on a coarse clock.
	writeRaw(t, path, "v2")
	later := before.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	expectRefused(t, path, "v2", before)
}

// A file another writer renamed onto the path, with the size and the
// modification time of the version read, is still another file.
func TestWriteIfUnchangedRefusesAFileReplacedWithTheSameSizeAndTime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	writeRaw(t, path, "v1")
	before := readVersion(t, path)
	replacement := filepath.Join(dir, "replacement")
	writeRaw(t, replacement, "v2")
	if err := os.Chtimes(replacement, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	expectRefused(t, path, "v2", before)
}

func TestWriteIfUnchangedRefusesAFileThatAppearedSinceTheRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	before := readVersion(t, path)
	if before != nil {
		t.Fatalf("a missing file read as %v", before)
	}
	writeRaw(t, path, "theirs")
	expectRefused(t, path, "theirs", before)
}

func TestWriteIfUnchangedRefusesAFileThatDisappearedSinceTheRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	writeRaw(t, path, "v1")
	before := readVersion(t, path)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	expectRefused(t, path, "", before)
}

func TestWriteIfUnchangedWritesTheVersionItRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	writeRaw(t, path, "v1")
	if committed, err := WriteIfUnchanged(path, []byte("mine"), readVersion(t, path)); err != nil || !committed {
		t.Fatalf("WriteIfUnchanged over an unchanged file = %v, %v", committed, err)
	}
	if got, _ := os.ReadFile(path); string(got) != "mine" {
		t.Fatalf("file = %q, want mine", got)
	}

	absent := filepath.Join(t.TempDir(), "sub", "config")
	if committed, err := WriteIfUnchanged(absent, []byte("new"), readVersion(t, absent)); err != nil || !committed {
		t.Fatalf("WriteIfUnchanged of a still absent file = %v, %v", committed, err)
	}
	if got, _ := os.ReadFile(absent); string(got) != "new" {
		t.Fatalf("file = %q, want new", got)
	}
}

// A directory sync that fails after the rename is reported with the write
// committed: the new contents are in place. A directory that allows the
// rename and not the open for the sync (write and search, no read) shows it.
func TestWriteIfUnchangedReportsACommittedWriteWhoseDirectorySyncFailed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SyncDir is a no-op on Windows")
	}
	dir := filepath.Join(t.TempDir(), "d")
	path := filepath.Join(dir, "config")
	if err := Write(path, []byte("v1")); err != nil {
		t.Fatal(err)
	}
	before := readVersion(t, path)
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, dirMode) })
	committed, err := WriteIfUnchanged(path, []byte("mine"), before)
	if !committed || err == nil || !strings.Contains(err.Error(), "sync dir") {
		t.Fatalf("WriteIfUnchanged = %v, %v; want committed with the directory sync's failure", committed, err)
	}
	if err := os.Chmod(dir, dirMode); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "mine" {
		t.Fatalf("file = %q, want the committed write", got)
	}
}

func TestReadWithStatAnswersNothingForAMissingFile(t *testing.T) {
	data, info, err := ReadWithStat(filepath.Join(t.TempDir(), "absent"))
	if data != nil || info != nil || err != nil {
		t.Fatalf("ReadWithStat of a missing file = %q, %v, %v", data, info, err)
	}
}
