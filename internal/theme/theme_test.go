package theme

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newService builds a Service over a throwaway config dir. Every test in
// this package works inside t.TempDir(): the themes directory is a real
// user config surface, so a test that resolved the real one would seed
// and overwrite the developer's own reference artifacts.
func newService(t *testing.T) (*Service, string) {
	t.Helper()
	configDir := t.TempDir()
	service, err := New(configDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if service.Dir() != filepath.Join(configDir, DirName) {
		t.Fatalf("Dir() = %q, want %q", service.Dir(), filepath.Join(configDir, DirName))
	}
	return service, configDir
}

func TestAppearanceRoundTrip(t *testing.T) {
	service, configDir := newService(t)

	want := Appearance{Mode: "dark", UITheme: "tokyo-night", CodeTheme: "monokai", WindowBackground: "#1A1B26"}
	if err := service.SetAppearance(want); err != nil {
		t.Fatalf("SetAppearance: %v", err)
	}

	got := service.Files()
	if got.Appearance != want {
		t.Fatalf("round-tripped appearance = %+v, want %+v", got.Appearance, want)
	}
	if len(got.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", got.Warnings)
	}
	if got := WindowBackground(configDir); got != want.WindowBackground {
		t.Fatalf("WindowBackground = %q, want %q", got, want.WindowBackground)
	}

	// The file on disk is the wire shape, so an agent pointed at it reads
	// the same keys the RPC speaks.
	data, err := os.ReadFile(service.AppearancePath())
	if err != nil {
		t.Fatalf("read appearance.json: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("appearance.json is not JSON: %v", err)
	}
	for _, key := range []string{"mode", "uiTheme", "codeTheme", "windowBackground"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("appearance.json missing %q: %s", key, data)
		}
	}
}

func TestSetAppearanceFillsEmptyFieldsWithDefaults(t *testing.T) {
	service, _ := newService(t)

	if err := service.SetAppearance(Appearance{Mode: "light"}); err != nil {
		t.Fatalf("SetAppearance: %v", err)
	}
	got := service.Files().Appearance
	want := Appearance{Mode: "light", UITheme: DefaultUITheme, CodeTheme: DefaultCodeTheme}
	if got != want {
		t.Fatalf("appearance = %+v, want %+v", got, want)
	}
	// windowBackground is a cache, not semantics: absent stays absent.
	data, err := os.ReadFile(service.AppearancePath())
	if err != nil {
		t.Fatalf("read appearance.json: %v", err)
	}
	if strings.Contains(string(data), "windowBackground") {
		t.Fatalf("empty windowBackground was materialized: %s", data)
	}
}

func TestSetAppearanceRejectsInvalidValues(t *testing.T) {
	cases := map[string]Appearance{
		"mode out of enum":     {Mode: "solarized"},
		"uiTheme uppercase":    {UITheme: "TokyoNight"},
		"uiTheme path escape":  {UITheme: "../evil"},
		"uiTheme leading dash": {UITheme: "-nope"},
		"codeTheme spaces":     {CodeTheme: "tokyo night"},
		"codeTheme too long":   {CodeTheme: strings.Repeat("a", 65)},
		"background no hash":   {WindowBackground: "1a1b26"},
		"background shorthand": {WindowBackground: "#abc"},
		"background non-hex":   {WindowBackground: "#gggggg"},
	}
	for name, appearance := range cases {
		t.Run(name, func(t *testing.T) {
			service, _ := newService(t)
			if err := service.SetAppearance(appearance); err == nil {
				t.Fatalf("SetAppearance(%+v) accepted an invalid selection", appearance)
			}
			if _, err := os.Stat(service.AppearancePath()); !os.IsNotExist(err) {
				t.Fatalf("a rejected selection still wrote appearance.json (stat err = %v)", err)
			}
		})
	}
}

func TestFilesListsThemesAndWarnsWithoutFailing(t *testing.T) {
	service, _ := newService(t)
	dir := service.Dir()
	if err := os.MkdirAll(filepath.Join(dir, "nested.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("zebra.json", `{"name":"Zebra"}`)
	write("alpha.json", `{"name":"Alpha"}`)
	write("README.md", "not a theme")
	write(AppearanceFileName, `{"mode":"dark"}`)
	write(SchemaFileName, `{"title":"schema"}`)
	write("Not An Id.json", `{}`)
	write("huge.json", strings.Repeat("x", MaxThemeFileBytes+1))

	got := service.Files()
	if len(got.Themes) != 2 {
		t.Fatalf("themes = %+v, want exactly alpha + zebra", got.Themes)
	}
	if got.Themes[0].ID != "alpha" || got.Themes[1].ID != "zebra" {
		t.Fatalf("themes are not sorted by id: %+v", got.Themes)
	}
	if got.Themes[0].Raw != `{"name":"Alpha"}` {
		t.Fatalf("raw bytes were not passed through verbatim: %q", got.Themes[0].Raw)
	}
	if got.Appearance.Mode != "dark" {
		t.Fatalf("appearance mode = %q, want dark", got.Appearance.Mode)
	}
	joined := strings.Join(got.Warnings, "\n")
	if !strings.Contains(joined, "Not An Id.json") {
		t.Fatalf("no warning for the invalid id: %v", got.Warnings)
	}
	if !strings.Contains(joined, "huge.json") {
		t.Fatalf("no warning for the oversized file: %v", got.Warnings)
	}
	if strings.Contains(joined, "README.md") || strings.Contains(joined, SchemaFileName) {
		t.Fatalf("non-theme files warned: %v", got.Warnings)
	}
}

// The skip that Files() documents but nothing proved: a symlink named
// like a theme must not be followed. Following it would let a file
// OUTSIDE the themes directory reach the frontend under a themes-directory
// id, which is a read the RPC's own description does not cover. Silent by
// design — a symlink is a deliberate act, not a mistake to report.
func TestFilesSkipsSymlinksSilently(t *testing.T) {
	service, _ := newService(t)
	dir := service.Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(outside, []byte(`{"name":"Outside"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "linked.json")); err != nil {
		t.Skipf("symlinks unavailable on this host: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "real.json"), []byte(`{"name":"Real"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	got := service.Files()
	if len(got.Themes) != 1 || got.Themes[0].ID != "real" {
		t.Fatalf("themes = %+v, want only the regular file", got.Themes)
	}
	if len(got.Warnings) != 0 {
		t.Fatalf("a symlink produced warnings: %v", got.Warnings)
	}
}

// The per-file cap is enforced without ever holding the whole file: the
// read is bounded at cap+1 bytes. Also pins the ONE wording — the message
// used to exist twice with different numbers in it.
func TestFilesRefusesOversizeFileWithOneWording(t *testing.T) {
	service, _ := newService(t)
	dir := service.Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "huge.json"), []byte(strings.Repeat("x", MaxThemeFileBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	got := service.Files()
	if len(got.Themes) != 0 {
		t.Fatalf("an oversize file was listed: %+v", got.Themes)
	}
	if len(got.Warnings) != 1 || got.Warnings[0] != oversizeWarning("huge.json") {
		t.Fatalf("warnings = %v, want exactly %q", got.Warnings, oversizeWarning("huge.json"))
	}
	if !strings.Contains(got.Warnings[0], "1 MiB") {
		t.Fatalf("warning %q does not state the cap readably", got.Warnings[0])
	}
}

// The per-file cap bounds one file; these bound the ANSWER. Without them
// a directory of files just under the per-file cap builds an RPC result
// hundreds of megabytes wide.
func TestFilesEnforcesAggregateCaps(t *testing.T) {
	t.Run("file count", func(t *testing.T) {
		service, _ := newService(t)
		dir := service.Dir()
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for i := range MaxThemeFiles + 5 {
			name := fmt.Sprintf("theme-%03d.json", i)
			if err := os.WriteFile(filepath.Join(dir, name), []byte(`{}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		got := service.Files()
		if len(got.Themes) != MaxThemeFiles {
			t.Fatalf("themes = %d, want the %d cap", len(got.Themes), MaxThemeFiles)
		}
		if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "stopped after") {
			t.Fatalf("warnings = %v, want one explaining the stop", got.Warnings)
		}
	})

	t.Run("total bytes", func(t *testing.T) {
		service, _ := newService(t)
		dir := service.Dir()
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		body := strings.Repeat("x", MaxThemeFileBytes)
		// Five files of exactly the per-file cap: each is individually
		// fine, and together they are past the 4 MiB listing budget.
		for i := range 5 {
			name := fmt.Sprintf("big-%d.json", i)
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		got := service.Files()
		if len(got.Themes) != MaxThemeFilesBytes/MaxThemeFileBytes {
			t.Fatalf("themes = %d, want the listing to stop at the byte budget", len(got.Themes))
		}
		if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "4 MiB") {
			t.Fatalf("warnings = %v, want one naming the total budget", got.Warnings)
		}
	})
}

func TestFilesOnMissingDirectoryIsQuiet(t *testing.T) {
	service, _ := newService(t)
	got := service.Files()
	if len(got.Themes) != 0 || len(got.Warnings) != 0 {
		t.Fatalf("a missing themes dir produced %+v", got)
	}
	if got.Appearance != DefaultAppearance() {
		t.Fatalf("appearance = %+v, want defaults", got.Appearance)
	}
}

func TestFilesWarnsOnMalformedAppearance(t *testing.T) {
	service, _ := newService(t)
	if err := os.MkdirAll(service.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(service.AppearancePath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := service.Files()
	if got.Appearance != DefaultAppearance() {
		t.Fatalf("appearance = %+v, want defaults", got.Appearance)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], AppearanceFileName) {
		t.Fatalf("warnings = %v, want one naming appearance.json", got.Warnings)
	}
}

func TestEnsureBootSeedsDirectoryAndReferenceArtifacts(t *testing.T) {
	service, _ := newService(t)

	if err := service.EnsureBoot(""); err != nil {
		t.Fatalf("EnsureBoot: %v", err)
	}
	for _, name := range []string{SchemaFileName, TokensFileName} {
		onDisk, err := os.ReadFile(filepath.Join(service.Dir(), name))
		if err != nil {
			t.Fatalf("read seeded %s: %v", name, err)
		}
		embedded, err := assets.ReadFile("assets/" + name)
		if err != nil {
			t.Fatalf("read embedded %s: %v", name, err)
		}
		if string(onDisk) != string(embedded) {
			t.Fatalf("%s was not seeded from the embedded copy", name)
		}
	}
	if got := service.Files().Appearance; got != DefaultAppearance() {
		t.Fatalf("seeded appearance = %+v, want defaults", got)
	}
}

func TestEnsureBootRefreshesDriftedArtifactsButKeepsUserSelection(t *testing.T) {
	service, _ := newService(t)
	if err := service.EnsureBoot(""); err != nil {
		t.Fatalf("EnsureBoot: %v", err)
	}
	chosen := Appearance{Mode: "light", UITheme: "mine", CodeTheme: "dracula"}
	if err := service.SetAppearance(chosen); err != nil {
		t.Fatalf("SetAppearance: %v", err)
	}
	// A stale generated artifact documents tokens this build may no
	// longer have, so the embedded copy wins on every boot.
	tokensPath := filepath.Join(service.Dir(), TokensFileName)
	if err := os.WriteFile(tokensPath, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := service.EnsureBoot("dark"); err != nil {
		t.Fatalf("EnsureBoot: %v", err)
	}
	refreshed, err := os.ReadFile(tokensPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(refreshed) == "stale" {
		t.Fatal("a drifted TOKENS.md survived boot")
	}
	// The legacy seed applies only to an ABSENT appearance file; an
	// existing selection is never overwritten.
	if got := service.Files().Appearance; got != chosen {
		t.Fatalf("appearance = %+v, want the user's %+v", got, chosen)
	}
}

func TestEnsureBootSeedsModeFromLegacySetting(t *testing.T) {
	for _, legacy := range []string{"light", "dark", "system"} {
		t.Run(legacy, func(t *testing.T) {
			service, _ := newService(t)
			if err := service.EnsureBoot(legacy); err != nil {
				t.Fatalf("EnsureBoot: %v", err)
			}
			got := service.Files().Appearance
			if got.Mode != legacy {
				t.Fatalf("mode = %q, want the legacy %q", got.Mode, legacy)
			}
			if got.UITheme != DefaultUITheme || got.CodeTheme != DefaultCodeTheme {
				t.Fatalf("legacy seed disturbed the theme axes: %+v", got)
			}
		})
	}
}

func TestEnsureBootIgnoresUnrecognisedLegacyMode(t *testing.T) {
	service, _ := newService(t)
	if err := service.EnsureBoot("solarized"); err != nil {
		t.Fatalf("EnsureBoot: %v", err)
	}
	if got := service.Files().Appearance.Mode; got != DefaultMode {
		t.Fatalf("mode = %q, want %q", got, DefaultMode)
	}
}

func TestSetAppearanceIsAtomicAndLeavesNoTempFiles(t *testing.T) {
	service, _ := newService(t)
	if err := service.SetAppearance(Appearance{Mode: "dark"}); err != nil {
		t.Fatalf("SetAppearance: %v", err)
	}
	// Overwrite the same path repeatedly: the temp+rename write must
	// never leave a partial file or a stray temp behind for the watcher
	// (or the next listing) to trip over.
	for i := range 5 {
		mode := "light"
		if i%2 == 0 {
			mode = "dark"
		}
		if err := service.SetAppearance(Appearance{Mode: mode}); err != nil {
			t.Fatalf("SetAppearance: %v", err)
		}
	}
	entries, err := os.ReadDir(service.Dir())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if len(names) != 1 || names[0] != AppearanceFileName {
		t.Fatalf("themes dir = %v, want only %s", names, AppearanceFileName)
	}
	if got := service.Files().Appearance.Mode; got != "dark" {
		t.Fatalf("final mode = %q, want dark", got)
	}
}

func TestParseHexColor(t *testing.T) {
	red, green, blue, err := ParseHexColor("#1a1B26")
	if err != nil {
		t.Fatalf("ParseHexColor: %v", err)
	}
	if red != 0x1a || green != 0x1b || blue != 0x26 {
		t.Fatalf("channels = %d,%d,%d, want 26,27,38", red, green, blue)
	}
	for _, bad := range []string{"", "1a1b26", "#1a1b2", "#1a1b26f", "#zzzzzz", "rgb(0,0,0)"} {
		if _, _, _, err := ParseHexColor(bad); err == nil {
			t.Fatalf("ParseHexColor(%q) accepted a non-color", bad)
		}
	}
}

func TestWindowBackgroundDegradesToEmpty(t *testing.T) {
	if got := WindowBackground(""); got != "" {
		t.Fatalf("WindowBackground(\"\") = %q", got)
	}
	missing := t.TempDir()
	if got := WindowBackground(missing); got != "" {
		t.Fatalf("WindowBackground with no themes dir = %q", got)
	}
	service, configDir := newService(t)
	if err := os.MkdirAll(service.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(service.AppearancePath(), []byte(`{"windowBackground":"nope"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := WindowBackground(configDir); got != "" {
		t.Fatalf("WindowBackground with a malformed value = %q", got)
	}
}
