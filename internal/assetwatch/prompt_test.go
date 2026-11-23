package assetwatch

import (
	"os"
	"path/filepath"
	"testing"
)

const testPromptName = "0b7a2c4e-9f1d-4e3a-8c5b-2d6f7a8e9b0c.md"

func TestPromptWatcherStartsAWaitOnlyForPromptFiles(t *testing.T) {
	observed := startObservedWatcher(t, "prompt watcher", t.TempDir(), promptFileRelevant)

	observed.write(t, testPromptName, "one")
	observed.expectQueued(t, 0, 1, "a prompt write")
	armed := observed.clock.Armed()
	observed.write(t, "other.MD", "two")
	observed.expectQueued(t, armed, 1, "a prompt write with an upper-case extension")

	// A backup an editor leaves beside the file, a swap file and a
	// subdirectory hold nothing the settings service reads.
	armed = observed.clock.Armed()
	for _, name := range []string{testPromptName + "~", "." + testPromptName + ".swp", "notes.txt"} {
		observed.write(t, name, "x")
	}
	nested := filepath.Join(observed.core.dir, "drafts")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "draft.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	observed.expectIgnored(t, armed, "non-prompt files")

	// Removing a prompt file empties its entry, so it must reach the app.
	armed = observed.clock.Armed()
	if err := os.Remove(filepath.Join(observed.core.dir, testPromptName)); err != nil {
		t.Fatal(err)
	}
	observed.expectQueued(t, armed, 1, "a prompt removal")
}

func TestPromptWatcherIgnoresAtomicfileTempNames(t *testing.T) {
	watcher, err := NewPromptWatcher(t.TempDir(), func() {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = watcher.Close() })

	name := testPromptName + ".tmp-2148294417"
	if watcher.core.relevant(filepath.Join(watcher.core.dir, name)) {
		t.Fatalf("%s is treated as a prompt file; an atomic write would echo back", name)
	}
	if !watcher.core.relevant(filepath.Join(watcher.core.dir, testPromptName)) {
		t.Fatal("a real prompt file is not relevant")
	}
}

func TestPromptWatcherClosesANilWatcher(t *testing.T) {
	var nilWatcher *PromptWatcher
	if err := nilWatcher.Close(); err != nil {
		t.Fatalf("nil close: %v", err)
	}
}
