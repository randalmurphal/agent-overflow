package assetwatch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fsnotify/fsnotify"
)

const testPromptName = "0b7a2c4e-9f1d-4e3a-8c5b-2d6f7a8e9b0c.md"

func TestPromptWatcherStartsAWaitOnlyForPromptFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, testPromptName), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	observed := startObservedWatcher(t, "prompt watcher", dir, promptFileRelevant)

	// A backup an editor leaves beside the file, a swap file and a
	// subdirectory hold nothing the settings service reads.
	names := []string{testPromptName + "~", "." + testPromptName + ".swp", "notes.txt"}
	for _, name := range names {
		observed.place(t, name, "x")
	}
	if err := os.Mkdir(observed.path("drafts"), 0o700); err != nil {
		t.Fatal(err)
	}
	observed.expectNoChange(t, 0, fsnotify.Create, "non-prompt files",
		observed.path(names[0]), observed.path(names[1]), observed.path(names[2]), observed.path("drafts"))

	// Removing a prompt file empties its entry, so it must reach the app.
	armed := observed.clock.Armed()
	if err := os.Remove(observed.path(testPromptName)); err != nil {
		t.Fatal(err)
	}
	observed.expectChange(t, armed, observed.path(testPromptName), fsnotify.Remove, "a prompt removal")

	for _, name := range []string{"other.MD", "new.md"} {
		armed = observed.clock.Armed()
		observed.place(t, name, "x")
		observed.expectChange(t, armed, observed.path(name), fsnotify.Create, "a write of "+name)
	}
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
