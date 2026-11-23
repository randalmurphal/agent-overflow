package assetwatch

import (
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/soundlib"
)

func TestSoundWatcherStartsAWaitOnlyForCueFiles(t *testing.T) {
	observed := startObservedWatcher(t, "sound watcher", t.TempDir(), soundFileRelevant)

	observed.write(t, "desk-bell.wav", "wav")
	observed.expectQueued(t, 0, 1, "a cue write")

	// The seeded reference, a source file the user kept beside their cue,
	// and a subdirectory all hold nothing this app plays.
	armed := observed.clock.Armed()
	for _, name := range []string{soundlib.ReferenceFileName, "original.mp3", "notes.txt"} {
		observed.write(t, name, "x")
	}
	nested := filepath.Join(observed.core.dir, "sources")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "old.wav"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	observed.expectIgnored(t, armed, "non-cue files")

	// Removing a cue changes what the pickers offer, so it must reach the
	// UI: an event whose chosen cue just disappeared falls back.
	armed = observed.clock.Armed()
	if err := os.Remove(filepath.Join(observed.core.dir, "desk-bell.wav")); err != nil {
		t.Fatal(err)
	}
	observed.expectQueued(t, armed, 1, "a cue removal")
}

// PutSoundFile and DeleteSoundFile write this directory themselves. Their
// caller already knows what it did, so the resulting filesystem events must
// not travel back as an external change and cost a full listing refetch.
func TestSoundWatcherSuppressesItsOwnWrites(t *testing.T) {
	observed := startObservedWatcher(t, "sound watcher", t.TempDir(), soundFileRelevant)
	watcher := &SoundWatcher{core: observed.core}

	cue := filepath.Join(observed.core.dir, "desk-bell.wav")
	watcher.Suppress(cue)
	if err := os.WriteFile(cue, []byte("wav"), 0o600); err != nil {
		t.Fatal(err)
	}
	observed.expectIgnored(t, 0, "a suppressed self-write")

	// Suppression is per path, so a different cue written by hand still
	// reaches the UI.
	observed.write(t, "ping.wav", "wav")
	observed.expectQueued(t, 0, 1, "a cue written by hand")
}

// internal/atomicfile names its temp file `<base>.tmp-NNNNN`, whose
// EXTENSION is `.tmp-123` rather than `.wav`, so a cue write's temp file is
// outside relevant() before suppression is even consulted.
func TestSoundWatcherIgnoresAtomicfileTempNames(t *testing.T) {
	watcher, err := NewSoundWatcher(t.TempDir(), func() {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = watcher.Close() })

	for _, base := range []string{soundlib.ReferenceFileName, "desk-bell.wav"} {
		name := base + ".tmp-2148294417"
		if watcher.core.relevant(filepath.Join(watcher.core.dir, name)) {
			t.Fatalf("%s is treated as a cue file", name)
		}
	}
	if watcher.core.relevant(filepath.Join(watcher.core.dir, soundlib.ReferenceFileName)) {
		t.Fatal("the generated reference is treated as a cue file")
	}
	for _, name := range []string{"desk-bell.wav", "PING.WAV"} {
		if !watcher.core.relevant(filepath.Join(watcher.core.dir, name)) {
			t.Fatalf("%s is not relevant", name)
		}
	}
}
