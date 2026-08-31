package assetwatch

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/theme"
)

func newTestThemeWatcher(t *testing.T) (*ThemeWatcher, string, <-chan struct{}) {
	t.Helper()
	dir := t.TempDir()
	emitted := make(chan struct{}, 8)
	watcher, err := newThemeWatcher(dir, 25*time.Millisecond, func() { emitted <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = watcher.Close() })
	return watcher, dir, emitted
}

func waitForThemeChanged(t *testing.T, emitted <-chan struct{}) {
	t.Helper()
	select {
	case <-emitted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a theme-changed event")
	}
}

func expectNoThemeChange(t *testing.T, emitted <-chan struct{}, why string) {
	t.Helper()
	select {
	case <-emitted:
		t.Fatalf("%s emitted a theme-changed event", why)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestThemeWatcherDebouncesWritesAndIgnoresNonThemeFiles(t *testing.T) {
	_, dir, emitted := newTestThemeWatcher(t)

	themeFile := filepath.Join(dir, "tokyo-night.json")
	for index := range 3 {
		if err := os.WriteFile(themeFile, []byte{byte('a' + index)}, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	waitForThemeChanged(t, emitted)
	expectNoThemeChange(t, emitted, "a write burst")

	// The generated reference artifacts are rewritten by the backend at
	// boot and read by nobody at runtime; a subdirectory holds nothing
	// this app reads. Neither may wake the frontend.
	for _, name := range []string{theme.SchemaFileName, theme.TokensFileName, "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	nested := filepath.Join(dir, "backup")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "old.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	expectNoThemeChange(t, emitted, "non-theme files")

	// Removing a theme changes what resolves, so it must reach the UI.
	if err := os.Remove(themeFile); err != nil {
		t.Fatal(err)
	}
	waitForThemeChanged(t, emitted)
}

func TestThemeWatcherIgnoresItsOwnAtomicAppearanceWrite(t *testing.T) {
	service, err := theme.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := service.EnsureBoot(""); err != nil {
		t.Fatal(err)
	}
	emitted := make(chan struct{}, 8)
	watcher, err := newThemeWatcher(service.Dir(), 25*time.Millisecond, func() { emitted <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = watcher.Close() })
	appearancePath := service.AppearancePath()

	watcher.Suppress(appearancePath)
	if err := service.SetAppearance(theme.Appearance{Mode: "dark", WindowBackground: "#1a1b26"}); err != nil {
		t.Fatal(err)
	}
	watcher.Suppress(appearancePath)
	expectNoThemeChange(t, emitted, "the app's own appearance write")

	// The suppression is a window, not a latch: an external edit after it
	// lapses must still reach the frontend.
	watcher.core.suppressMu.Lock()
	watcher.core.suppressed[filepath.Clean(appearancePath)] = time.Now().Add(-time.Second)
	watcher.core.suppressMu.Unlock()
	if err := os.WriteFile(appearancePath, []byte(`{"mode":"light"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForThemeChanged(t, emitted)
}

// Removing the watched directory kills the inotify watch silently: no
// error on the Errors channel, no further events, live reload dead for
// the rest of the process. The watcher must put the directory back and
// re-register, or `rm -rf themes/` would be a one-way door.
func TestThemeWatcherRearmsAfterTheDirectoryIsRemoved(t *testing.T) {
	_, dir, emitted := newTestThemeWatcher(t)

	if err := os.WriteFile(filepath.Join(dir, "tokyo-night.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForThemeChanged(t, emitted)

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	// The removal is itself a change the frontend has to see.
	waitForThemeChanged(t, emitted)

	// Wait for the re-create rather than assuming it already happened —
	// it runs on the watch goroutine, off this one's clock.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the themes directory was never re-created")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Drain any emit the re-create itself produced, then prove the new
	// watch is live by writing a theme into the fresh directory.
	select {
	case <-emitted:
	case <-time.After(200 * time.Millisecond):
	}
	if err := os.WriteFile(filepath.Join(dir, "after.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForThemeChanged(t, emitted)
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
	watcher := &ThemeWatcher{core: &watcher{dir: filepath.Clean(t.TempDir())}}

	for _, base := range []string{theme.AppearanceFileName, theme.SchemaFileName, theme.TokensFileName, "tokyo-night.json"} {
		name := base + ".tmp-2148294417"
		if watcher.relevant(filepath.Join(watcher.core.dir, name)) {
			t.Fatalf("%s is treated as a theme file; an atomic write would echo back as an external change", name)
		}
	}
	// The destination of that same write still is one.
	if !watcher.relevant(filepath.Join(watcher.core.dir, "tokyo-night.json")) {
		t.Fatal("a real theme file is not relevant")
	}
}

func TestThemeWatcherRejectsInvalidSetupAndClosesCleanly(t *testing.T) {
	if _, err := newThemeWatcher(filepath.Join(t.TempDir(), "missing"), time.Millisecond, func() {}); err == nil {
		t.Fatal("missing dir unexpectedly accepted")
	}
	notADir := filepath.Join(t.TempDir(), "themes")
	if err := os.WriteFile(notADir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newThemeWatcher(notADir, time.Millisecond, func() {}); err == nil {
		t.Fatal("file in place of the themes dir unexpectedly accepted")
	}
	dir := t.TempDir()
	if _, err := newThemeWatcher(dir, 0, func() {}); err == nil {
		t.Fatal("non-positive debounce unexpectedly accepted")
	}
	if _, err := newThemeWatcher(dir, time.Millisecond, nil); err == nil {
		t.Fatal("nil emit callback unexpectedly accepted")
	}
	watcher, err := newThemeWatcher(dir, time.Millisecond, func() {})
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
