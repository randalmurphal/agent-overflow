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
	found := false
	for _, b := range kb {
		if b.Command == "palette.open" && b.Key == "mod+k" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected default palette.open binding (mod+k) in result")
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
	// Palette should be rebound to mod+shift+p (override by command+when).
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
