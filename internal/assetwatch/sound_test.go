package assetwatch

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/soundlib"
)

func newTestSoundWatcher(t *testing.T) (*SoundWatcher, string, <-chan struct{}) {
	t.Helper()
	dir := t.TempDir()
	emitted := make(chan struct{}, 8)
	watcher, err := newSoundWatcher(dir, 25*time.Millisecond, func() { emitted <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = watcher.Close() })
	return watcher, dir, emitted
}

func waitForSoundChanged(t *testing.T, emitted <-chan struct{}) {
	t.Helper()
	select {
	case <-emitted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a sound-changed event")
	}
}

func expectNoSoundChange(t *testing.T, emitted <-chan struct{}, why string) {
	t.Helper()
	select {
	case <-emitted:
		t.Fatalf("%s emitted a sound-changed event", why)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSoundWatcherDebouncesWritesAndIgnoresNonCueFiles(t *testing.T) {
	_, dir, emitted := newTestSoundWatcher(t)

	cue := filepath.Join(dir, "desk-bell.wav")
	if err := os.WriteFile(cue, []byte("wav"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ping.wav"), []byte("wav"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cue, []byte("wav2"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForSoundChanged(t, emitted)
	expectNoSoundChange(t, emitted, "a write burst")

	// The seeded reference, a source file the user kept beside their cue,
	// and a subdirectory all hold nothing this app plays.
	for _, name := range []string{soundlib.ReferenceFileName, "original.mp3", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	nested := filepath.Join(dir, "sources")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "old.wav"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	expectNoSoundChange(t, emitted, "non-cue files")

	// Removing a cue changes what the pickers offer, so it must reach the
	// UI — an event whose chosen cue just disappeared falls back.
	if err := os.Remove(cue); err != nil {
		t.Fatal(err)
	}
	waitForSoundChanged(t, emitted)
}

// PutSoundFile and DeleteSoundFile write this directory themselves. Their
// caller already knows what it did, so the resulting filesystem events must
// not travel back as an external change and cost a full listing refetch.
func TestSoundWatcherSuppressesItsOwnWrites(t *testing.T) {
	watcher, dir, emitted := newTestSoundWatcher(t)

	cue := filepath.Join(dir, "desk-bell.wav")
	watcher.Suppress(cue)
	if err := os.WriteFile(cue, []byte("wav"), 0o600); err != nil {
		t.Fatal(err)
	}
	expectNoSoundChange(t, emitted, "a suppressed self-write")

	// Suppression is per path and bounded in time, so a different cue
	// written by hand still reaches the UI.
	if err := os.WriteFile(filepath.Join(dir, "ping.wav"), []byte("wav"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForSoundChanged(t, emitted)
}

// internal/atomicfile names its temp file `<base>.tmp-NNNNN`, whose
// EXTENSION is `.tmp-123` rather than `.wav`, so a cue write's temp file is
// outside relevant() before suppression is even consulted.
func TestSoundWatcherIgnoresAtomicfileTempNames(t *testing.T) {
	watcher := &SoundWatcher{core: &watcher{dir: filepath.Clean(t.TempDir())}}

	for _, base := range []string{soundlib.ReferenceFileName, "desk-bell.wav"} {
		name := base + ".tmp-2148294417"
		if watcher.relevant(filepath.Join(watcher.core.dir, name)) {
			t.Fatalf("%s is treated as a cue file", name)
		}
	}
	if watcher.relevant(filepath.Join(watcher.core.dir, soundlib.ReferenceFileName)) {
		t.Fatal("the generated reference is treated as a cue file")
	}
	for _, name := range []string{"desk-bell.wav", "PING.WAV"} {
		if !watcher.relevant(filepath.Join(watcher.core.dir, name)) {
			t.Fatalf("%s is not relevant", name)
		}
	}
}

func TestSoundWatcherRejectsInvalidSetupAndClosesCleanly(t *testing.T) {
	if _, err := newSoundWatcher(filepath.Join(t.TempDir(), "missing"), time.Millisecond, func() {}); err == nil {
		t.Fatal("missing dir unexpectedly accepted")
	}
	notADir := filepath.Join(t.TempDir(), soundlib.DirName)
	if err := os.WriteFile(notADir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newSoundWatcher(notADir, time.Millisecond, func() {}); err == nil {
		t.Fatal("file in place of the sounds dir unexpectedly accepted")
	}
	dir := t.TempDir()
	if _, err := newSoundWatcher(dir, 0, func() {}); err == nil {
		t.Fatal("non-positive debounce unexpectedly accepted")
	}
	if _, err := newSoundWatcher(dir, time.Millisecond, nil); err == nil {
		t.Fatal("nil emit callback unexpectedly accepted")
	}
	watcher, err := newSoundWatcher(dir, time.Millisecond, func() {})
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
