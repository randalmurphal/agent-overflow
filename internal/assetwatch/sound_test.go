package assetwatch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fsnotify/fsnotify"

	"agent-overflow/internal/soundlib"
)

func TestSoundWatcherStartsAWaitOnlyForCueFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "old.wav"), []byte("wav"), 0o600); err != nil {
		t.Fatal(err)
	}
	observed := startObservedWatcher(t, "sound watcher", dir, soundFileRelevant)

	// The seeded reference, a source file the user kept beside their cue,
	// and a subdirectory all hold nothing this app plays.
	names := []string{soundlib.ReferenceFileName, "original.mp3", "notes.txt"}
	for _, name := range names {
		observed.place(t, name, "x")
	}
	if err := os.Mkdir(observed.path("sources"), 0o700); err != nil {
		t.Fatal(err)
	}
	observed.expectNoChange(t, 0, fsnotify.Create, "non-cue files",
		observed.path(names[0]), observed.path(names[1]), observed.path(names[2]), observed.path("sources"))

	// Removing a cue changes what the pickers offer, so it must reach the
	// UI: an event whose chosen cue just disappeared falls back.
	armed := observed.clock.Armed()
	if err := os.Remove(observed.path("old.wav")); err != nil {
		t.Fatal(err)
	}
	observed.expectChange(t, armed, observed.path("old.wav"), fsnotify.Remove, "a cue removal")

	armed = observed.clock.Armed()
	observed.place(t, "desk-bell.wav", "wav")
	observed.expectChange(t, armed, observed.path("desk-bell.wav"), fsnotify.Create, "a cue write")
}

func TestSoundWatcherSuppressesItsOwnWrites(t *testing.T) {
	observed := startObservedWatcher(t, "sound watcher", t.TempDir(), soundFileRelevant)
	watcher := &SoundWatcher{core: observed.core}

	cue := observed.path("desk-bell.wav")
	watcher.Suppress(cue)
	observed.place(t, "desk-bell.wav", "wav")
	observed.expectNoChange(t, 0, fsnotify.Create, "a suppressed self-write", cue)

	// Suppression is per path, so a different cue written by hand still
	// reaches the UI.
	armed := observed.clock.Armed()
	observed.place(t, "ping.wav", "wav")
	observed.expectChange(t, armed, observed.path("ping.wav"), fsnotify.Create, "a cue written by hand")
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
