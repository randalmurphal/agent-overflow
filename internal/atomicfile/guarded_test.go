package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
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
	expectRefusedWith(t, systemOps(), path, want, before)
}

// expectRefusedWith is expectRefused through ops.
func expectRefusedWith(t *testing.T, ops fileOps, path, want string, before os.FileInfo) {
	t.Helper()
	committed, err := writeIfUnchanged(ops, path, []byte("mine"), before)
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
	assertNoStagedFiles(t, filepath.Dir(path))
}

// assertNoStagedFiles asserts that no staged temporary file is left in dir.
func assertNoStagedFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Fatalf("a write left %s behind", entry.Name())
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
// committed: the new contents are in place.
func TestWriteIfUnchangedReportsACommittedWriteWhoseDirectorySyncFailed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	writeRaw(t, path, "v1")
	before := readVersion(t, path)
	failure := errors.New("injected sync failure")
	ops := systemOps()
	ops.syncDir = func(string) error { return failure }

	committed, err := writeIfUnchanged(ops, path, []byte("mine"), before)
	if !committed || !errors.Is(err, failure) {
		t.Fatalf("WriteIfUnchanged = %v, %v; want committed with the directory sync's failure", committed, err)
	}
	if got, _ := os.ReadFile(path); string(got) != "mine" {
		t.Fatalf("file = %q, want the committed write", got)
	}
}

// A change already visible when the write starts is refused before the
// candidate is staged; a file that changes after the early check is still
// refused by the check before the rename.
func TestWriteIfUnchangedRefusesAVisibleChangeBeforeStaging(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	writeRaw(t, path, "v1")
	before := readVersion(t, path)
	staged := 0
	ops := systemOps()
	ops.stage = func(path string, data []byte) (string, error) {
		staged++
		return stage(path, data)
	}

	writeRaw(t, path, "theirs, longer")
	expectRefusedWith(t, ops, path, "theirs, longer", before)
	if staged != 0 {
		t.Fatalf("a refused write staged %d candidates, want none", staged)
	}

	// Changed after the early check, while the candidate is staged.
	before = readVersion(t, path)
	ops.stage = func(target string, data []byte) (string, error) {
		staged++
		writeRaw(t, target, "theirs again, longer still")
		return stage(target, data)
	}
	expectRefusedWith(t, ops, path, "theirs again, longer still", before)
	if staged != 1 {
		t.Fatalf("staged %d candidates, want the one the late check refused", staged)
	}
}

// The bytes and the info come from the file that was opened: a new
// version renamed onto the path during the read does not pair its info
// with the old bytes, and the write against what was read is refused.
func TestReadWithStatPairsTheBytesWithTheFileItOpened(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	writeRaw(t, path, "old")
	ops := systemOps()
	ops.open = func(name string) (*os.File, error) {
		file, err := os.Open(name)
		replacement := path + ".new"
		writeRaw(t, replacement, "new version")
		if err := os.Rename(replacement, path); err != nil {
			t.Fatal(err)
		}
		return file, err
	}

	data, info, err := readWithStat(ops, path)
	if err != nil || string(data) != "old" || info.Size() != int64(len("old")) {
		t.Fatalf("ReadWithStat = %q, %v, %v; want the opened file's bytes and info", data, info, err)
	}
	expectRefused(t, path, "new version", info)
}

// A config that is a symbolic link to a missing file reads as an error
// naming the link and what to do, not as an absent file the write could
// never create through the link.
func TestReadWithStatNamesALinkToAMissingFile(t *testing.T) {
	dir := t.TempDir()
	path, target := filepath.Join(dir, "config"), filepath.Join(dir, "dotfiles", "config")
	if err := os.Symlink(target, path); err != nil {
		t.Skip(err)
	}
	_, _, err := ReadWithStat(path)
	if err == nil || !strings.Contains(err.Error(), path+" is a symbolic link to "+target+", which does not exist: create "+target+" or remove the link") {
		t.Fatalf("ReadWithStat = %v, want the dangling link named", err)
	}
}

func TestReadWithStatAnswersNothingForAMissingFile(t *testing.T) {
	data, info, err := ReadWithStat(filepath.Join(t.TempDir(), "absent"))
	if data != nil || info != nil || err != nil {
		t.Fatalf("ReadWithStat of a missing file = %q, %v, %v", data, info, err)
	}
}
