package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadTransferJSONReadsANameInsideItsDirectory(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "manifest.json"), []byte(`{"version":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var manifest struct{ Version int }
	if err := readTransferJSON(directory, "manifest.json", 64, &manifest); err != nil {
		t.Fatalf("read: %v", err)
	}
	if manifest.Version != 3 {
		t.Fatalf("version = %d, want 3", manifest.Version)
	}
	if err := readTransferJSON(directory, "manifest.json", 4, &manifest); err == nil {
		t.Fatal("read past the limit succeeded")
	}
}

// Discard treats a transfer that never extracted as nothing to clean up, so
// a missing directory or file must stay a not-exist error.
func TestReadTransferJSONReportsAMissingDirectoryOrFileAsNotExist(t *testing.T) {
	directory := t.TempDir()
	var manifest struct{}
	if err := readTransferJSON(filepath.Join(directory, "extracted"), "manifest.json", 64, &manifest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing directory: err = %v, want not exist", err)
	}
	if err := readTransferJSON(directory, "manifest.json", 64, &manifest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: err = %v, want not exist", err)
	}
}

func TestReadTransferJSONRefusesALinkOutOfItsDirectory(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte(`{"version":9}`), 0o600); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(directory, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	var manifest struct{ Version int }
	err := readTransferJSON(directory, "manifest.json", 64, &manifest)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want a refusal that is not a missing file", err)
	}
	if manifest.Version != 0 {
		t.Fatalf("read the linked file: version = %d", manifest.Version)
	}
}
