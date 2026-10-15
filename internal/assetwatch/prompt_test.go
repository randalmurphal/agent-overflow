package assetwatch

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestPromptWatcher(t *testing.T) (*PromptWatcher, string, <-chan struct{}) {
	t.Helper()
	dir := t.TempDir()
	emitted := make(chan struct{}, 8)
	watcher, err := newPromptWatcher(dir, 25*time.Millisecond, func() { emitted <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = watcher.Close() })
	return watcher, dir, emitted
}

func waitForPromptChanged(t *testing.T, emitted <-chan struct{}) {
	t.Helper()
	select {
	case <-emitted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a prompt-changed event")
	}
}

const testPromptName = "0b7a2c4e-9f1d-4e3a-8c5b-2d6f7a8e9b0c.md"

func TestPromptWatcherDebouncesWritesAndIgnoresNonPromptFiles(t *testing.T) {
	observed := startObservedWatcher(t, "prompt watcher", promptFileRelevant)

	observed.write(t, testPromptName, "one")
	observed.write(t, testPromptName, "two")
	observed.write(t, "other.MD", "three")
	observed.expectQueued(t, 0, 2, "a write burst")
	observed.releaseOneEmit(t, "a write burst")

	// A backup an editor leaves beside the file, a swap file and a
	// subdirectory hold nothing the settings service reads.
	armed := observed.clock.armedCount()
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
	armed = observed.clock.armedCount()
	if err := os.Remove(filepath.Join(observed.core.dir, testPromptName)); err != nil {
		t.Fatal(err)
	}
	observed.expectQueued(t, armed, 1, "a prompt removal")
	observed.releaseOneEmit(t, "a prompt removal")
}

func TestPromptWatcherIgnoresAtomicfileTempNames(t *testing.T) {
	watcher := &PromptWatcher{core: &watcher{dir: filepath.Clean(t.TempDir())}}

	name := testPromptName + ".tmp-2148294417"
	if watcher.relevant(filepath.Join(watcher.core.dir, name)) {
		t.Fatalf("%s is treated as a prompt file; an atomic write would echo back", name)
	}
	if !watcher.relevant(filepath.Join(watcher.core.dir, testPromptName)) {
		t.Fatal("a real prompt file is not relevant")
	}
}

// Removing the prompts directory must not end live reload for the rest of
// the process.
func TestPromptWatcherRearmsAfterTheDirectoryIsRemoved(t *testing.T) {
	_, dir, emitted := newTestPromptWatcher(t)

	if err := os.WriteFile(filepath.Join(dir, testPromptName), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForPromptChanged(t, emitted)

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	waitForPromptChanged(t, emitted)

	deadline := time.Now().Add(3 * time.Second)
	for {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the prompts directory was never re-created")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-emitted:
	case <-time.After(200 * time.Millisecond):
	}
	if err := os.WriteFile(filepath.Join(dir, testPromptName), []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForPromptChanged(t, emitted)
}

func TestPromptWatcherRejectsInvalidSetupAndClosesCleanly(t *testing.T) {
	if _, err := newPromptWatcher(filepath.Join(t.TempDir(), "missing"), time.Millisecond, func() {}); err == nil {
		t.Fatal("missing dir unexpectedly accepted")
	}
	watcher, err := newPromptWatcher(t.TempDir(), time.Millisecond, func() {})
	if err != nil {
		t.Fatal(err)
	}
	if err := watcher.Close(); err != nil {
		t.Fatal(err)
	}
	if err := watcher.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	var nilWatcher *PromptWatcher
	if err := nilWatcher.Close(); err != nil {
		t.Fatalf("nil close: %v", err)
	}
}
