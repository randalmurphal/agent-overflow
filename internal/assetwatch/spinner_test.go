package assetwatch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fsnotify/fsnotify"

	"agent-overflow/internal/spinner"
)

func TestSpinnerWatcherStartsAWaitOnlyForSpriteFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "orb.png"), []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	observed := startObservedWatcher(t, "spinner watcher", dir, spinnerFileRelevant)

	// The generated reference is rewritten by the backend at boot and read
	// by nobody at runtime; a stray file is not a sprite half; a
	// subdirectory holds nothing this app reads. None may wake the
	// frontend.
	names := []string{spinner.ReferenceFileName, "notes.txt", "source.gif"}
	for _, name := range names {
		observed.place(t, name, "x")
	}
	if err := os.Mkdir(observed.path("sources"), 0o700); err != nil {
		t.Fatal(err)
	}
	observed.expectNoChange(t, 0, fsnotify.Create, "non-sprite files",
		observed.path(names[0]), observed.path(names[1]), observed.path(names[2]), observed.path("sources"))

	// Removing a strip changes what the picker offers, so it must reach
	// the UI.
	armed := observed.clock.Armed()
	if err := os.Remove(observed.path("orb.png")); err != nil {
		t.Fatal(err)
	}
	observed.expectChange(t, armed, observed.path("orb.png"), fsnotify.Remove, "a strip removal")

	// A sprite lands as two files, and each half is content.
	for _, name := range []string{"robo-papers.png", "robo-papers.json"} {
		armed = observed.clock.Armed()
		observed.place(t, name, "x")
		observed.expectChange(t, armed, observed.path(name), fsnotify.Create, "a write of "+name)
	}
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
