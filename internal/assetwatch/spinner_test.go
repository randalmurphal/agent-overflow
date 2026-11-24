package assetwatch

import (
	"path/filepath"
	"testing"

	"github.com/fsnotify/fsnotify"

	"agent-overflow/internal/spinner"
)

func TestSpinnerWatcherStartsAWaitOnlyForSpriteFiles(t *testing.T) {
	observed := startObservedWatcher(t, "spinner watcher", t.TempDir(), spinnerFileRelevant)

	// The generated reference is rewritten by the backend at boot and read
	// by nobody at runtime; a stray file is not a sprite half; a
	// subdirectory holds nothing this app reads. None may wake the
	// frontend.
	observed.expectNoChanges(t, "non-sprite files",
		observed.event(spinner.ReferenceFileName, fsnotify.Create), observed.event("notes.txt", fsnotify.Create),
		observed.event("source.gif", fsnotify.Write), observed.event("sources", fsnotify.Create))

	// Removing a strip changes what the picker offers, so it must reach
	// the UI, and a sprite lands as two files, each of them content.
	observed.expectChanges(t, "sprite files",
		observed.event("orb.png", fsnotify.Remove), observed.event("robo-papers.png", fsnotify.Create),
		observed.event("robo-papers.json", fsnotify.Create))
}

// internal/atomicfile names its temp file `<base>.tmp-NNNNN`, so the boot
// refresh of SPINNERS.md lands as `SPINNERS.md.tmp-123` — whose EXTENSION
// is `.tmp-123`, neither `.png` nor `.json`. That is what keeps our own
// writes outside relevant() without a suppression call, and the property
// belongs to atomicfile's naming, which this package does not own.
func TestSpinnerWatcherIgnoresAtomicfileTempNames(t *testing.T) {
	watcher, err := NewSpinnerWatcher(t.TempDir(), func() {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = watcher.Close() })

	for _, base := range []string{spinner.ReferenceFileName, "robo-papers.png", "robo-papers.json"} {
		name := base + ".tmp-2148294417"
		if watcher.core.relevant(filepath.Join(watcher.core.dir, name)) {
			t.Fatalf("%s is treated as a sprite file; an atomic write would echo back as an external change", name)
		}
	}
	// Both halves of a real pair still are.
	for _, name := range []string{"robo-papers.png", "robo-papers.json", "ORB.PNG"} {
		if !watcher.core.relevant(filepath.Join(watcher.core.dir, name)) {
			t.Fatalf("%s is not relevant", name)
		}
	}
}
