package assetwatch

import (
	"path/filepath"
	"testing"

	"github.com/fsnotify/fsnotify"

	"agent-overflow/internal/soundlib"
)

func TestSoundWatcherStartsAWaitOnlyForCueFiles(t *testing.T) {
	observed := startObservedWatcher(t, "sound watcher", t.TempDir(), soundFileRelevant)

	// The seeded reference, a source file the user kept beside their cue,
	// and a subdirectory all hold nothing this app plays.
	observed.expectNoChanges(t, "non-cue files",
		observed.event(soundlib.ReferenceFileName, fsnotify.Create), observed.event("original.mp3", fsnotify.Create),
		observed.event("notes.txt", fsnotify.Write), observed.event("sources", fsnotify.Create))

	// Removing a cue changes what the pickers offer, so it must reach the
	// UI: an event whose chosen cue just disappeared falls back.
	observed.expectChanges(t, "cue files",
		observed.event("old.wav", fsnotify.Remove), observed.event("desk-bell.wav", fsnotify.Create))
}

func TestSoundWatcherSuppressesItsOwnWrites(t *testing.T) {
	observed := startObservedWatcher(t, "sound watcher", t.TempDir(), soundFileRelevant)
	watcher := &SoundWatcher{core: observed.core}

	watcher.Suppress(observed.path("desk-bell.wav"))
	observed.expectNoChanges(t, "a suppressed self-write",
		observed.event("desk-bell.wav", fsnotify.Create), observed.event("desk-bell.wav", fsnotify.Write))

	// Suppression is per path, so a different cue written by hand still
	// reaches the UI.
	observed.expectChanges(t, "a cue written by hand", observed.event("ping.wav", fsnotify.Create))
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
