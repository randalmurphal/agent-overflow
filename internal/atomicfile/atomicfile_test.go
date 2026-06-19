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
