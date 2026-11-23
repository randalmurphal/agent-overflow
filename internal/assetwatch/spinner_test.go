package assetwatch

import (
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/spinner"
)

func TestSpinnerWatcherStartsAWaitOnlyForSpriteFiles(t *testing.T) {
	observed := startObservedWatcher(t, "spinner watcher", t.TempDir(), spinnerFileRelevant)

	// A sprite lands as two files, and each half is content.
	observed.write(t, "robo-papers.png", "png")
	observed.expectQueued(t, 0, 1, "a strip write")
	armed := observed.clock.Armed()
	observed.write(t, "robo-papers.json", "{}")
	observed.expectQueued(t, armed, 1, "a sprite descriptor write")

	// The generated reference is rewritten by the backend at boot and read
	// by nobody at runtime; a stray file is not a sprite half; a
	// subdirectory holds nothing this app reads. None may wake the
	// frontend.
	armed = observed.clock.Armed()
	for _, name := range []string{spinner.ReferenceFileName, "notes.txt", "source.gif"} {
		observed.write(t, name, "x")
	}
	nested := filepath.Join(observed.core.dir, "sources")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "old.png"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	observed.expectIgnored(t, armed, "non-sprite files")

	// Removing a strip changes what the picker offers, so it must reach
	// the UI.
	armed = observed.clock.Armed()
	if err := os.Remove(filepath.Join(observed.core.dir, "robo-papers.png")); err != nil {
		t.Fatal(err)
	}
	observed.expectQueued(t, armed, 1, "a strip removal")
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
	// The generated reference is never a sprite, under any casing.
	if watcher.core.relevant(filepath.Join(watcher.core.dir, spinner.ReferenceFileName)) {
		t.Fatal("the generated reference is treated as a sprite file")
	}
	// Both halves of a real pair still are.
	for _, name := range []string{"robo-papers.png", "robo-papers.json", "ORB.PNG"} {
		if !watcher.core.relevant(filepath.Join(watcher.core.dir, name)) {
			t.Fatalf("%s is not relevant", name)
		}
	}
}
