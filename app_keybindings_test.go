package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func newKeybindingsApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	return &App{configDir: dir}
}

func TestGetKeybindingsReturnsDefaultsWhenNoFile(t *testing.T) {
	app := newKeybindingsApp(t)
	kb, err := app.GetKeybindings()
	if err != nil {
		t.Fatalf("GetKeybindings() error = %v", err)
	}
	if len(kb) != len(DefaultKeybindings) {
		t.Fatalf("len(kb) = %d, want %d", len(kb), len(DefaultKeybindings))
	}
	wantPalette := false
	wantSidebar := false
	for _, b := range kb {
		if b.DefaultID == "" {
			t.Fatalf("default %s returned empty DefaultID", b.Command)
		}
		if b.DefaultKey != b.Key {
			t.Fatalf("default %s returned DefaultKey = %q, want %q", b.Command, b.DefaultKey, b.Key)
		}
		if b.Command == "palette.open" && b.Key == "mod+shift+k" {
			wantPalette = true
		}
		if b.Command == "sidebar.focus-search" && b.Key == "mod+k" {
			wantSidebar = true
		}
	}
	if !wantPalette {
		t.Fatal("expected default palette.open binding (mod+shift+k) in result")
	}
	if !wantSidebar {
		t.Fatal("expected default sidebar.focus-search binding (mod+k) in result")
	}
}

// TestDefaultKeybindingsIncludeNewHelpSearchAndInterruptBindings pins
// the chord assignments for features that ship visible UI (cheat sheet,
// message search, thread picker, working-indicator interrupt) so a
// refactor can't silently drop them. Users who rebind can override; but
// users who never open Settings should still discover these features by
// muscle memory.
func TestDefaultKeybindingsIncludeNewHelpSearchAndInterruptBindings(t *testing.T) {
	want := map[string]string{
		"help.keybindings": "mod+/",
		"search.messages":  "mod+shift+f",
		"thread.search":    "mod+p",
		"thread.interrupt": "esc",
	}
	got := make(map[string]string)
	for _, b := range DefaultKeybindings {
		got[b.Command] = b.Key
	}
	for cmd, key := range want {
		if got[cmd] != key {
			t.Errorf("DefaultKeybindings: %s = %q, want %q", cmd, got[cmd], key)
		}
	}
}

// TestDefaultKeybindingsHaveUniqueKeyWhenTuples guarantees no two
// defaults collide on the same (key, when) pair. Multiple chords for one
// command is fine (e.g. mod+n and mod+shift+o both → thread.new), but
// two different commands on the same chord under the same context would
// make one unreachable.
func TestDefaultKeybindingsHaveUniqueKeyWhenTuples(t *testing.T) {
	seen := make(map[string]string)
	for _, b := range DefaultKeybindings {
		tuple := b.Key + "|" + b.When
		if prev, found := seen[tuple]; found && prev != b.Command {
			t.Errorf(
				"chord %q under when=%q is bound to both %q and %q — collision shadows the second binding",
				b.Key, b.When, prev, b.Command,
			)
		}
		seen[tuple] = b.Command
	}
}

func TestDefaultKeybindingsHaveUniqueDefaultIDs(t *testing.T) {
	seen := make(map[string]bool)
	for i, b := range DefaultKeybindings {
		if b.DefaultID == "" {
			t.Fatalf("DefaultKeybindings[%d] has empty DefaultID", i)
		}
		if seen[b.DefaultID] {
			t.Fatalf("DefaultKeybindings[%d] duplicates DefaultID %q", i, b.DefaultID)
		}
		seen[b.DefaultID] = true
	}
}

// TestDefaultKeybindingsUseValidChordSyntax is a smoke test guarding
// against typos like "mod+/ " or empty keys making it into defaults.
// The frontend's tryParseChord is the authoritative validator, but we
// check the basic shape here to catch obvious mistakes at build time.
func TestDefaultKeybindingsUseValidChordSyntax(t *testing.T) {
	for i, b := range DefaultKeybindings {
		if b.Key == "" {
			t.Errorf("DefaultKeybindings[%d] has empty key", i)
		}
		if b.Command == "" {
			t.Errorf("DefaultKeybindings[%d] has empty command", i)
		}
		// Leading/trailing whitespace isn't valid and usually means a
		// typo in the source.
		if b.Key != "" && (b.Key[0] == ' ' || b.Key[len(b.Key)-1] == ' ') {
			t.Errorf("DefaultKeybindings[%d].Key = %q has surrounding whitespace", i, b.Key)
		}
	}
}

func TestUpdateKeybindingsPersistsAndMergesOverDefaults(t *testing.T) {
	app := newKeybindingsApp(t)

	user := []Keybinding{
		{Key: "mod+shift+p", Command: "palette.open"},
		{Key: "mod+l", Command: "terminal.toggle"},
	}
	if err := app.UpdateKeybindings(user); err != nil {
		t.Fatalf("UpdateKeybindings() error = %v", err)
	}

	path := filepath.Join(app.configDir, keybindingsFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading persisted file: %v", err)
	}
	var persisted []Keybinding
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("parsing persisted file: %v", err)
	}
	if len(persisted) != 2 {
		t.Fatalf("persisted entries = %d, want 2", len(persisted))
	}

	merged, err := app.GetKeybindings()
	if err != nil {
		t.Fatalf("GetKeybindings() error = %v", err)
	}
	// Palette should be rebound to mod+shift+p (legacy override by command+when).
	var palette Keybinding
	var termToggle Keybinding
	for _, b := range merged {
		if b.Command == "palette.open" && b.When == "" {
			palette = b
		}
		if b.Command == "terminal.toggle" && b.When == "" {
			termToggle = b
		}
	}
	if palette.Key != "mod+shift+p" {
		t.Fatalf("palette.open key = %q, want mod+shift+p", palette.Key)
	}
	if palette.DefaultKey != "mod+shift+k" {
		t.Fatalf("palette.open defaultKey = %q, want mod+shift+k", palette.DefaultKey)
	}
	if palette.DefaultID != "palette.open" {
		t.Fatalf("palette.open defaultID = %q, want palette.open", palette.DefaultID)
	}
	if termToggle.Key != "mod+l" {
		t.Fatalf("terminal.toggle key = %q, want mod+l", termToggle.Key)
	}
	// Unrelated defaults should still be present.
	seenThreadJump := false
	for _, b := range merged {
		if b.Command == "thread.jump.1" {
			seenThreadJump = true
			break
		}
	}
	if !seenThreadJump {
		t.Fatal("default thread.jump.1 binding missing after override")
	}
}

func TestUpdateKeybindingsRejectsEmptyKeyOrCommand(t *testing.T) {
	app := newKeybindingsApp(t)

	if err := app.UpdateKeybindings([]Keybinding{{Key: "", Command: "x"}}); err == nil {
		t.Fatal("expected error for empty key")
	}
	if err := app.UpdateKeybindings([]Keybinding{{Key: "mod+k", Command: "   "}}); err == nil {
		t.Fatal("expected error for empty command")
	}
}

func TestUpdateKeybindingsCapsMaxEntries(t *testing.T) {
	app := newKeybindingsApp(t)
	big := make([]Keybinding, maxKeybindingsCount+5)
	for i := range big {
		big[i] = Keybinding{Key: "mod+k", Command: "palette.open"}
	}
	if err := app.UpdateKeybindings(big); err != nil {
		t.Fatalf("UpdateKeybindings() error = %v", err)
	}
	// After capping, the written file should hold exactly maxKeybindingsCount
	// entries.
	path := filepath.Join(app.configDir, keybindingsFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading persisted file: %v", err)
	}
	var persisted []Keybinding
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("parsing persisted file: %v", err)
	}
	if len(persisted) != maxKeybindingsCount {
		t.Fatalf("persisted entries = %d, want %d", len(persisted), maxKeybindingsCount)
	}
}

func TestResetKeybindingsDeletesUserFile(t *testing.T) {
	app := newKeybindingsApp(t)

	if err := app.UpdateKeybindings([]Keybinding{{Key: "mod+k", Command: "palette.open"}}); err != nil {
		t.Fatalf("UpdateKeybindings() error = %v", err)
	}
	path := filepath.Join(app.configDir, keybindingsFileName)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not written: %v", err)
	}
	if err := app.ResetKeybindings(); err != nil {
		t.Fatalf("ResetKeybindings() error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file still exists after reset: err=%v", err)
	}
	// ResetKeybindings on a missing file is a no-op.
	if err := app.ResetKeybindings(); err != nil {
		t.Fatalf("second ResetKeybindings() error = %v", err)
	}
}

func TestGetKeybindingsFallsBackWhenFileMalformed(t *testing.T) {
	app := newKeybindingsApp(t)
	path := filepath.Join(app.configDir, keybindingsFileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("writing malformed file: %v", err)
	}
	kb, err := app.GetKeybindings()
	if err != nil {
		t.Fatalf("GetKeybindings() returned error: %v", err)
	}
	if len(kb) != len(DefaultKeybindings) {
		t.Fatalf("fallback len(kb) = %d, want %d", len(kb), len(DefaultKeybindings))
	}
}

func TestMergeKeybindingsPreservesOverriddenAndRetainsDefaults(t *testing.T) {
	defaults := []Keybinding{
		{Key: "mod+k", Command: "palette.open"},
		{Key: "mod+j", Command: "terminal.toggle"},
		{Key: "mod+n", Command: "terminal.new", When: "terminalFocus"},
	}
	user := []Keybinding{
		{Key: "mod+p", Command: "palette.open"},
		{Key: "mod+t", Command: "custom.action"},
	}
	out := mergeKeybindings(defaults, user)

	// terminal.toggle and terminal.new remain.
	hasTermToggle := false
	hasTermNew := false
	hasCustom := false
	paletteKey := ""
	for _, b := range out {
		switch b.Command {
		case "terminal.toggle":
			hasTermToggle = true
		case "terminal.new":
			hasTermNew = true
		case "custom.action":
			hasCustom = true
		case "palette.open":
			paletteKey = b.Key
		}
	}
	if !hasTermToggle || !hasTermNew {
		t.Fatal("defaults missing from merged output")
	}
	if !hasCustom {
		t.Fatal("user-only entry missing from merged output")
	}
	if paletteKey != "mod+p" {
		t.Fatalf("palette key = %q, want mod+p", paletteKey)
	}
}

func TestMergeKeybindingsUsesDefaultKeyForDuplicateCommandContexts(t *testing.T) {
	defaults := []Keybinding{
		{Key: "mod+n", Command: "thread.new", When: "!terminalFocus", DefaultID: "thread.new.primary"},
		{Key: "mod+shift+o", Command: "thread.new", When: "!terminalFocus", DefaultID: "thread.new.alternate"},
	}
	user := []Keybinding{
		{
			Key:       "mod+x",
			Command:   "thread.new",
			When:      "!terminalFocus",
			DefaultID: "thread.new.alternate",
		},
	}

	out := mergeKeybindings(defaults, user)

	seenOriginal := false
	seenRebound := false
	for _, b := range out {
		if b.Command != "thread.new" || b.When != "!terminalFocus" {
			continue
		}
		switch {
		case b.Key == "mod+n" && b.DefaultID == "thread.new.primary" && b.DefaultKey == "mod+n":
			seenOriginal = true
		case b.Key == "mod+x" && b.DefaultID == "thread.new.alternate" && b.DefaultKey == "mod+shift+o":
			seenRebound = true
		}
	}
	if !seenOriginal {
		t.Fatal("original mod+n thread.new binding missing")
	}
	if !seenRebound {
		t.Fatal("rebound mod+shift+o thread.new binding missing")
	}
}

func TestMergeKeybindingsMigratesLegacyDefaultKeyIdentity(t *testing.T) {
	defaults := []Keybinding{
		{Key: "mod+n", Command: "thread.new", When: "!terminalFocus", DefaultID: "thread.new.primary"},
		{Key: "mod+shift+o", Command: "thread.new", When: "!terminalFocus", DefaultID: "thread.new.alternate"},
	}
	user := []Keybinding{
		{
			Key:        "mod+x",
			Command:    "thread.new",
			When:       "!terminalFocus",
			DefaultKey: "mod+shift+o",
		},
	}

	out := mergeKeybindings(defaults, user)

	for _, b := range out {
		if b.Key == "mod+x" {
			if b.DefaultID != "thread.new.alternate" {
				t.Fatalf("migrated override DefaultID = %q, want thread.new.alternate", b.DefaultID)
			}
			return
		}
	}
	t.Fatal("migrated override missing")
}

func TestMergeKeybindingsAppendsOverridesAfterDefaultsForRuntimePrecedence(t *testing.T) {
	defaults := []Keybinding{
		{Key: "mod+x", Command: "command.a", DefaultID: "command.a"},
		{Key: "mod+y", Command: "command.b", DefaultID: "command.b"},
	}
	user := []Keybinding{
		{Key: "mod+y", Command: "command.a", DefaultID: "command.a"},
	}

	out := mergeKeybindings(defaults, user)

	if len(out) != 2 {
		t.Fatalf("merged len = %d, want 2", len(out))
	}
	if out[0].Command != "command.b" || out[1].Command != "command.a" {
		t.Fatalf("merged order = [%s, %s], want defaults before override", out[0].Command, out[1].Command)
	}
}
