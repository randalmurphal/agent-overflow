package atomicfile

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

type sample struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func TestWriteThenReadRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	want := sample{Name: "geometry", Count: 3}
	if err := WriteJSON(path, want); err != nil {
		t.Fatalf("WriteJSON() error = %v", err)
	}

	var got sample
	found, err := ReadJSON(path, &got)
	if err != nil {
		t.Fatalf("ReadJSON() error = %v", err)
	}
	if !found {
		t.Fatal("ReadJSON() found = false, want true")
	}
	if got != want {
		t.Fatalf("ReadJSON() = %+v, want %+v", got, want)
	}
}

func TestWriteBytesRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "credential.bin")
	want := []byte{0, 1, 2, 255, '\n'}
	if err := Write(path, want); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("ReadFile() = %v, want %v", got, want)
	}
}

func TestReadMissingFileIsNotFoundNotError(t *testing.T) {
	var got sample
	found, err := ReadJSON(filepath.Join(t.TempDir(), "absent.json"), &got)
	if err != nil {
		t.Fatalf("ReadJSON(absent) error = %v, want nil", err)
	}
	if found {
		t.Fatal("ReadJSON(absent) found = true, want false")
	}
}

func TestWriteLeavesNoTempFileBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := WriteJSON(path, sample{Name: "x"}); err != nil {
		t.Fatalf("WriteJSON() error = %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("dir entries = %v, want exactly [state.json] (rename contract, no temp survives)", names)
	}
}

func TestWritePermsAreOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes not meaningful on Windows")
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := WriteJSON(path, sample{Name: "x"}); err != nil {
		t.Fatalf("WriteJSON() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if info.Mode().Perm() != fileMode {
		t.Fatalf("file mode = %o, want %o", info.Mode().Perm(), fileMode)
	}
}

// SyncDir is the second half of durability, and it is exported because
// internal/supervise needs it on directories this package did not write into:
// a snapshot directory it filled file by file, and the runtime root after a
// restore marker is removed. Without a directory fsync a rename is only in the
// page cache, so a machine that loses power after a "successful" write comes
// back to the old name, or to no name at all.
func TestSyncDir(t *testing.T) {
	dir := t.TempDir()
	if err := SyncDir(dir); err != nil {
		t.Fatalf("SyncDir: %v", err)
	}
	if err := SyncDir(""); err == nil {
		t.Error("SyncDir accepted an empty directory")
	}
	if err := SyncDir(filepath.Join(dir, "not-there")); err == nil {
		t.Error("SyncDir accepted a directory that does not exist")
	}
}

// Write does both halves itself, so a caller writing one file needs no second
// call. The observable half here is that the file is present and readable with
// private permissions after Write returns.
func TestWriteSyncsTheDirectoryItRenamedInto(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "state.json")
	if err := Write(path, []byte("durable")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "durable" {
		t.Fatalf("contents = %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != fileMode {
		t.Errorf("mode = %v, want %v", info.Mode().Perm(), fileMode)
	}
	// No temp file survives a successful write.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want just the written file", len(entries))
	}
}
