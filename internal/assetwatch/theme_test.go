package assetwatch

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"agent-overflow/internal/theme"
)

func TestThemeWatcherStartsAWaitOnlyForThemeFiles(t *testing.T) {
	observed := startObservedWatcher(t, "theme watcher", t.TempDir(), themeFileRelevant)

	// The generated reference artifacts are rewritten by the backend at
	// boot and read by nobody at runtime; a subdirectory holds nothing
	// this app reads. None may wake the frontend.
	observed.expectNoChanges(t, "non-theme files",
		observed.event(theme.SchemaFileName, fsnotify.Create), observed.event(theme.TokensFileName, fsnotify.Write),
		observed.event("notes.txt", fsnotify.Create), observed.event("backup", fsnotify.Create))

	// Removing a theme changes what resolves, so it must reach the UI, and
	// so must every write or rename of one.
	observed.expectChanges(t, "theme files",
		observed.event("old.json", fsnotify.Remove), observed.event("tokyo-night.json", fsnotify.Create),
		observed.event("tokyo-night.json", fsnotify.Write), observed.event("renamed.json", fsnotify.Rename))
}

// The app's own write of appearance.json is not an external change while
// its suppression window lasts, whatever events it arrives as. The window
// is not a latch: an external edit after it lapses still reaches the
// frontend.
func TestThemeWatcherIgnoresItsOwnAtomicAppearanceWrite(t *testing.T) {
	service, err := theme.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := service.EnsureBoot(""); err != nil {
		t.Fatal(err)
	}
	observed := startObservedWatcher(t, "theme watcher", service.Dir(), themeFileRelevant)
	watcher := &ThemeWatcher{core: observed.core}
	appearance := filepath.Base(service.AppearancePath())

	watcher.Suppress(service.AppearancePath())
	observed.expectNoChanges(t, "the app's own appearance write",
		observed.event(appearance, fsnotify.Create), observed.event(appearance, fsnotify.Write), observed.event(appearance, fsnotify.Rename))

	watcher.core.suppressMu.Lock()
	watcher.core.suppressed[filepath.Clean(service.AppearancePath())] = time.Now().Add(-time.Second)
	watcher.core.suppressMu.Unlock()
	observed.expectChanges(t, "an external edit after the window", observed.event(appearance, fsnotify.Write))
}

func TestThemeWatcherSuppressionExpiresAndPrunes(t *testing.T) {
	// Bare value, no fsnotify loop: the suppression ledger is pure
	// bookkeeping, and driving its clock from the test goroutine while a
	// watch loop read it would be a race in the test, not in production.
	now := time.Now()
	watcher := &ThemeWatcher{core: &watcher{suppressed: map[string]time.Time{}, now: func() time.Time { return now }}}

	stale := filepath.Join(t.TempDir(), "stale.json")
	fresh := filepath.Join(t.TempDir(), "fresh.json")
	watcher.Suppress(stale)
	if !watcher.core.isSuppressed(stale) {
		t.Fatal("a just-marked path is not suppressed")
	}
	if !watcher.core.isSuppressed(stale) {
		t.Fatal("the first filesystem event consumed the suppression mark")
	}

	now = now.Add(themeSelfWriteWindow + time.Second)
	if watcher.core.isSuppressed(stale) {
		t.Fatal("suppression outlived its window")
	}
	watcher.Suppress(fresh)
	watcher.core.suppressMu.Lock()
	remaining := len(watcher.core.suppressed)
	watcher.core.suppressMu.Unlock()
	if remaining != 1 {
		t.Fatalf("suppressed entries = %d, want only the fresh one", remaining)
	}
}

// internal/atomicfile names its temp file `<base>.tmp-NNNNN`, so every
// write this package makes lands as `appearance.json.tmp-123` — whose
// EXTENSION is `.tmp-123`, not `.json`. That is what keeps the temp files
// outside relevant(), and it is the reason suppression is a backstop for
// the destination name rather than the only defence. Pinned because the
// property belongs to atomicfile's naming, which this package does not
// own.
func TestThemeWatcherIgnoresAtomicfileTempNames(t *testing.T) {
	watcher, err := NewThemeWatcher(t.TempDir(), func() {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = watcher.Close() })

	for _, base := range []string{theme.AppearanceFileName, theme.SchemaFileName, theme.TokensFileName, "tokyo-night.json"} {
		name := base + ".tmp-2148294417"
		if watcher.core.relevant(filepath.Join(watcher.core.dir, name)) {
			t.Fatalf("%s is treated as a theme file; an atomic write would echo back as an external change", name)
		}
	}
	// The destination of that same write still is one.
	if !watcher.core.relevant(filepath.Join(watcher.core.dir, "tokyo-night.json")) {
		t.Fatal("a real theme file is not relevant")
	}
}
