package assetwatch

import (
	"path/filepath"
	"testing"

	"github.com/fsnotify/fsnotify"
)

const testPromptName = "0b7a2c4e-9f1d-4e3a-8c5b-2d6f7a8e9b0c.md"

func TestPromptWatcherStartsAWaitOnlyForPromptFiles(t *testing.T) {
	observed := startObservedWatcher(t, "prompt watcher", t.TempDir(), promptFileRelevant)

	// A backup an editor leaves beside the file, a swap file and a
	// subdirectory hold nothing the settings service reads.
	observed.expectNoChanges(t, "non-prompt files",
		observed.event(testPromptName+"~", fsnotify.Create), observed.event("."+testPromptName+".swp", fsnotify.Write),
		observed.event("notes.txt", fsnotify.Create), observed.event("drafts", fsnotify.Create))

	// Removing a prompt file empties its entry, so it must reach the app.
	observed.expectChanges(t, "prompt files",
		observed.event(testPromptName, fsnotify.Remove), observed.event("other.MD", fsnotify.Create),
		observed.event("new.md", fsnotify.Create))
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
