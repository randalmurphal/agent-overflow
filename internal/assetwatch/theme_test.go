package assetwatch

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"agent-overflow/internal/theme"
)

func TestThemeWatcherStartsAWaitOnlyForThemeFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "old.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	observed := startObservedWatcher(t, "theme watcher", dir, themeFileRelevant)

	// The generated reference artifacts are rewritten by the backend at
	// boot and read by nobody at runtime; a subdirectory holds nothing
	// this app reads. None may wake the frontend.
	names := []string{theme.SchemaFileName, theme.TokensFileName, "notes.txt"}
	for _, name := range names {
		observed.place(t, name, "x")
	}
	if err := os.Mkdir(observed.path("backup"), 0o700); err != nil {
		t.Fatal(err)
	}
	observed.expectNoChange(t, 0, fsnotify.Create, "non-theme files",
		observed.path(names[0]), observed.path(names[1]), observed.path(names[2]), observed.path("backup"))

	// Removing a theme changes what resolves, so it must reach the UI.
	armed := observed.clock.Armed()
	if err := os.Remove(observed.path("old.json")); err != nil {
		t.Fatal(err)
	}
	observed.expectChange(t, armed, observed.path("old.json"), fsnotify.Remove, "a theme removal")

	armed = observed.clock.Armed()
	observed.place(t, "tokyo-night.json", "{}")
	observed.expectChange(t, armed, observed.path("tokyo-night.json"), fsnotify.Create, "a theme write")
}

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
	appearancePath := service.AppearancePath()

	// The atomic write renames its temporary file onto appearance.json,
	// which arrives as a create of that name.
	watcher.Suppress(appearancePath)
	if err := service.SetAppearance(theme.Appearance{Mode: "dark", WindowBackground: "#1a1b26"}); err != nil {
		t.Fatal(err)
	}
	observed.expectNoChange(t, 0, fsnotify.Create, "the app's own appearance write", appearancePath)

	// The suppression is a window, not a latch: an external edit after it
	// lapses must still reach the frontend.
	watcher.core.suppressMu.Lock()
	watcher.core.suppressed[filepath.Clean(appearancePath)] = time.Now().Add(-time.Second)
	watcher.core.suppressMu.Unlock()
	armed := observed.clock.Armed()
	if err := os.WriteFile(appearancePath, []byte(`{"mode":"light"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	observed.expectChange(t, armed, appearancePath, fsnotify.Write, "an external edit after the window")
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
