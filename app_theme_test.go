package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/settings"
	"agent-overflow/internal/theme"
)

// newThemeTestApp builds the narrowest App that can answer the theme
// bindings: a config dir and nothing else. The themes surface touches no
// store, no provider, and no process, so it needs none of the
// session-capable fixtures — and it must never resolve the developer's
// real config dir, which is what configDir being set to t.TempDir()
// guarantees.
func newThemeTestApp(t *testing.T) (*App, string) {
	t.Helper()
	configDir := t.TempDir()
	return &App{configDir: configDir}, configDir
}

func TestGetThemeFilesAndSetAppearanceRoundTrip(t *testing.T) {
	app, configDir := newThemeTestApp(t)

	files, err := app.GetThemeFiles()
	if err != nil {
		t.Fatalf("GetThemeFiles: %v", err)
	}
	if files.Dir != filepath.Join(configDir, theme.DirName) {
		t.Fatalf("dir = %q, want the themes subdirectory of %q", files.Dir, configDir)
	}
	if files.Appearance != theme.DefaultAppearance() {
		t.Fatalf("appearance = %+v, want defaults", files.Appearance)
	}

	if err := os.MkdirAll(files.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := `{"name":"Mine","dark":{"colors":{"accent":"#ff0000"}}}`
	if err := os.WriteFile(filepath.Join(files.Dir, "mine.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.SetAppearance(theme.Appearance{Mode: "dark", UITheme: "mine", WindowBackground: "#0a0a0a"}); err != nil {
		t.Fatalf("SetAppearance: %v", err)
	}

	files, err = app.GetThemeFiles()
	if err != nil {
		t.Fatalf("GetThemeFiles: %v", err)
	}
	if len(files.Themes) != 1 || files.Themes[0].ID != "mine" || files.Themes[0].Raw != raw {
		t.Fatalf("themes = %+v, want the file's bytes verbatim under id \"mine\"", files.Themes)
	}
	if files.Appearance.UITheme != "mine" || files.Appearance.Mode != "dark" {
		t.Fatalf("appearance = %+v", files.Appearance)
	}
	if files.Appearance.CodeTheme != theme.DefaultCodeTheme {
		t.Fatalf("unset code axis = %q, want the default", files.Appearance.CodeTheme)
	}
	if len(files.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", files.Warnings)
	}
}

func TestSetAppearanceRejectsInvalidSelection(t *testing.T) {
	app, _ := newThemeTestApp(t)
	err := app.SetAppearance(theme.Appearance{Mode: "midnight"})
	if err == nil {
		t.Fatal("an out-of-enum mode was accepted")
	}
	if !strings.Contains(err.Error(), "midnight") {
		t.Fatalf("error %q does not name the rejected value", err)
	}
}

func TestSetWindowBackgroundColorValidatesThenApplies(t *testing.T) {
	app, _ := newThemeTestApp(t)

	// No native window (the headless WSL backend shape): a valid color
	// is satisfied as far as this process can satisfy it.
	if err := app.SetWindowBackgroundColor("#1a1b26"); err != nil {
		t.Fatalf("SetWindowBackgroundColor with no window: %v", err)
	}

	var applied [3]uint8
	calls := 0
	app.setWindowBackground = func(red, green, blue uint8) {
		applied = [3]uint8{red, green, blue}
		calls++
	}
	if err := app.SetWindowBackgroundColor("#1A1B26"); err != nil {
		t.Fatalf("SetWindowBackgroundColor: %v", err)
	}
	if calls != 1 || applied != [3]uint8{0x1a, 0x1b, 0x26} {
		t.Fatalf("applied %v after %d calls", applied, calls)
	}

	for _, bad := range []string{"", "1a1b26", "#abc", "rgb(0,0,0)", "oklch(0.2 0 0)"} {
		if err := app.SetWindowBackgroundColor(bad); err == nil {
			t.Fatalf("SetWindowBackgroundColor(%q) accepted a non-color", bad)
		}
	}
	if calls != 1 {
		t.Fatalf("a rejected color still reached the window (%d calls)", calls)
	}
}

// The retirement migration, end to end: an existing settings.json carries
// `theme` as a plain JSON key that the Settings struct no longer has, and
// initThemeDirectory must still find it and seed appearance.json's mode with
// it. This is the whole user-visible promise of retiring the field — someone
// who chose Light two releases ago does not get a dark app back.
func TestInitThemeDirectorySeedsModeFromRetiredSettingsField(t *testing.T) {
	configDir := t.TempDir()
	settingsPath := filepath.Join(configDir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{"theme":"light","timestampFormat":"24-hour"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	svc := settings.NewService(configDir)
	// The typed read cannot see it any more, which is exactly why the
	// migration goes through the raw accessor.
	if svc.Get().TimestampFormat != "24-hour" {
		t.Fatalf("sanity: settings file did not load")
	}

	app := &App{configDir: configDir, settings: svc}
	app.initThemeDirectory()
	t.Cleanup(func() {
		if app.themeWatcher != nil {
			_ = app.themeWatcher.Close()
		}
	})

	files, err := app.GetThemeFiles()
	if err != nil {
		t.Fatalf("GetThemeFiles: %v", err)
	}
	if files.Appearance.Mode != "light" {
		t.Fatalf("seeded mode = %q, want the retired settings.theme value", files.Appearance.Mode)
	}

	// A retired key is NOT republished: the next settings write drops it,
	// which is the whole point of retiring rather than preserving. That is
	// safe precisely because of the ordering above — initThemeDirectory runs
	// on the boot path, before any Update can reach the file, so the value is
	// consumed before it is dropped. Pinned here because the reverse order
	// would silently lose an upgrading user's choice.
	if _, err := svc.Update(map[string]any{"timestampFormat": "12-hour"}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := svc.RetiredString("theme"); got != "" {
		t.Fatalf("retired theme survived a write = %q, want it dropped", got)
	}
	// The seed is untouched by that, because it lives in its own file now.
	files, err = app.GetThemeFiles()
	if err != nil {
		t.Fatalf("GetThemeFiles: %v", err)
	}
	if files.Appearance.Mode != "light" {
		t.Fatalf("mode after the legacy key was dropped = %q, want light", files.Appearance.Mode)
	}
}

// The migration's one hole, closed: boot 1 cannot create the themes
// directory (here a FILE sits on the path — a read-only parent or a full
// disk does the same thing), so nothing seeds appearance.json. The next
// settings write then drops the retired `theme` key, and by boot 2 the
// user's choice does not exist anywhere on disk. The pending legacy mode
// therefore lives in the process for as long as the process does, and the
// seed heals the instant the blocker is gone.
func TestThemeSeedSurvivesABlockedBootAndHealsOnTheNextRead(t *testing.T) {
	configDir := t.TempDir()
	settingsPath := filepath.Join(configDir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{"theme":"light"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A regular file where the themes directory belongs: MkdirAll fails,
	// so EnsureBoot cannot seed.
	blocker := filepath.Join(configDir, theme.DirName)
	if err := os.WriteFile(blocker, []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}

	svc := settings.NewService(configDir)
	app := &App{configDir: configDir, settings: svc}
	app.initThemeDirectory()
	t.Cleanup(func() {
		if app.themeWatcher != nil {
			_ = app.themeWatcher.Close()
		}
	})

	// A blocked themes directory still answers — degraded, not broken.
	files, err := app.GetThemeFiles()
	if err != nil {
		t.Fatalf("GetThemeFiles while blocked: %v", err)
	}
	if files.Appearance.Mode != theme.DefaultMode {
		t.Fatalf("blocked read mode = %q, want the default", files.Appearance.Mode)
	}

	// The value is now gone from disk: this is the step that used to lose
	// it permanently.
	if _, err := svc.Update(map[string]any{"timestampFormat": "12-hour"}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := svc.RetiredString("theme"); got != "" {
		t.Fatalf("sanity: the retired key survived the write = %q", got)
	}

	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	files, err = app.GetThemeFiles()
	if err != nil {
		t.Fatalf("GetThemeFiles after unblocking: %v", err)
	}
	if files.Appearance.Mode != "light" {
		t.Fatalf("healed mode = %q, want the ORIGINAL legacy value the blocked boot was carrying", files.Appearance.Mode)
	}
	if len(files.Warnings) != 0 {
		t.Fatalf("a healed listing still warns: %v", files.Warnings)
	}
	// The seed is real, not just in memory: the next process finds it.
	if _, err := os.Stat(filepath.Join(files.Dir, theme.AppearanceFileName)); err != nil {
		t.Fatalf("appearance.json was not written: %v", err)
	}
}
