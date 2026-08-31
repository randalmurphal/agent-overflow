package assetwatch

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/spinner"
)

func newTestSpinnerWatcher(t *testing.T) (*SpinnerWatcher, string, <-chan struct{}) {
	t.Helper()
	dir := t.TempDir()
	emitted := make(chan struct{}, 8)
	watcher, err := newSpinnerWatcher(dir, 25*time.Millisecond, func() { emitted <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = watcher.Close() })
	return watcher, dir, emitted
}

func waitForSpinnerChanged(t *testing.T, emitted <-chan struct{}) {
	t.Helper()
	select {
	case <-emitted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a spinner-changed event")
	}
}

func expectNoSpinnerChange(t *testing.T, emitted <-chan struct{}, why string) {
	t.Helper()
	select {
	case <-emitted:
		t.Fatalf("%s emitted a spinner-changed event", why)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSpinnerWatcherDebouncesWritesAndIgnoresNonSpriteFiles(t *testing.T) {
	_, dir, emitted := newTestSpinnerWatcher(t)

	// A sprite lands as two files; the pair plus a rewrite must collapse
	// into one refetch, not three.
	strip := filepath.Join(dir, "robo-papers.png")
	if err := os.WriteFile(strip, []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "robo-papers.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(strip, []byte("png2"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForSpinnerChanged(t, emitted)
	expectNoSpinnerChange(t, emitted, "a write burst")

	// The generated reference is rewritten by the backend at boot and read
	// by nobody at runtime; a stray file is not a sprite half; a
	// subdirectory holds nothing this app reads. None may wake the
	// frontend.
	for _, name := range []string{spinner.ReferenceFileName, "notes.txt", "source.gif"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	nested := filepath.Join(dir, "sources")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "old.png"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	expectNoSpinnerChange(t, emitted, "non-sprite files")

	// Removing a strip changes what the picker offers, so it must reach
	// the UI.
	if err := os.Remove(strip); err != nil {
		t.Fatal(err)
	}
	waitForSpinnerChanged(t, emitted)
}

// Removing the watched directory kills the inotify watch silently: no
// error on the Errors channel, no further events, live reload dead for
// the rest of the process. The watcher must put the directory back and
// re-register, or `rm -rf spinners/` would be a one-way door.
func TestSpinnerWatcherRearmsAfterTheDirectoryIsRemoved(t *testing.T) {
	_, dir, emitted := newTestSpinnerWatcher(t)

	if err := os.WriteFile(filepath.Join(dir, "orb.png"), []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForSpinnerChanged(t, emitted)

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	// The removal is itself a change the frontend has to see.
	waitForSpinnerChanged(t, emitted)

	// Wait for the re-create rather than assuming it already happened —
	// it runs on the watch goroutine, off this one's clock.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the spinners directory was never re-created")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Drain any emit the re-create itself produced, then prove the new
	// watch is live by writing a sprite into the fresh directory.
	select {
	case <-emitted:
	case <-time.After(200 * time.Millisecond):
	}
	if err := os.WriteFile(filepath.Join(dir, "after.png"), []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForSpinnerChanged(t, emitted)
}

// internal/atomicfile names its temp file `<base>.tmp-NNNNN`, so the boot
// refresh of SPINNERS.md lands as `SPINNERS.md.tmp-123` — whose EXTENSION
// is `.tmp-123`, neither `.png` nor `.json`. That is what keeps our own
// writes outside relevant() without a suppression call, and the property
// belongs to atomicfile's naming, which this package does not own.
func TestSpinnerWatcherIgnoresAtomicfileTempNames(t *testing.T) {
	watcher := &SpinnerWatcher{core: &watcher{dir: filepath.Clean(t.TempDir())}}

	for _, base := range []string{spinner.ReferenceFileName, "robo-papers.png", "robo-papers.json"} {
		name := base + ".tmp-2148294417"
		if watcher.relevant(filepath.Join(watcher.core.dir, name)) {
			t.Fatalf("%s is treated as a sprite file; an atomic write would echo back as an external change", name)
		}
	}
	// The generated reference is never a sprite, under any casing.
	if watcher.relevant(filepath.Join(watcher.core.dir, spinner.ReferenceFileName)) {
		t.Fatal("the generated reference is treated as a sprite file")
	}
	// Both halves of a real pair still are.
	for _, name := range []string{"robo-papers.png", "robo-papers.json", "ORB.PNG"} {
		if !watcher.relevant(filepath.Join(watcher.core.dir, name)) {
			t.Fatalf("%s is not relevant", name)
		}
	}
}

func TestSpinnerWatcherRejectsInvalidSetupAndClosesCleanly(t *testing.T) {
	if _, err := newSpinnerWatcher(filepath.Join(t.TempDir(), "missing"), time.Millisecond, func() {}); err == nil {
		t.Fatal("missing dir unexpectedly accepted")
	}
	notADir := filepath.Join(t.TempDir(), "spinners")
	if err := os.WriteFile(notADir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newSpinnerWatcher(notADir, time.Millisecond, func() {}); err == nil {
		t.Fatal("file in place of the spinners dir unexpectedly accepted")
	}
	dir := t.TempDir()
	if _, err := newSpinnerWatcher(dir, 0, func() {}); err == nil {
		t.Fatal("non-positive debounce unexpectedly accepted")
	}
	if _, err := newSpinnerWatcher(dir, time.Millisecond, nil); err == nil {
		t.Fatal("nil emit callback unexpectedly accepted")
	}
	watcher, err := newSpinnerWatcher(dir, time.Millisecond, func() {})
	if err != nil {
		t.Fatal(err)
	}
	if err := watcher.Close(); err != nil {
		t.Fatal(err)
	}
	if err := watcher.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}
