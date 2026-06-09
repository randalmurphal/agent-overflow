package keybindings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func newServiceWithTempDir(t *testing.T) *Service {
	t.Helper()
	svc, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return svc
}

func TestGetReturnsDefaultsWhenNoFile(t *testing.T) {
	svc := newServiceWithTempDir(t)
	kb, err := svc.Get()
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if len(kb) != len(Defaults) {
		t.Fatalf("len(kb) = %d, want %d", len(kb), len(Defaults))
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
		if b.Command == "sidebar.focus-search" && b.Key == "mod+/" {
			wantSidebar = true
		}
	}
	if !wantPalette {
		t.Fatal("expected default palette.open binding (mod+shift+k) in result")
	}
	if !wantSidebar {
		t.Fatal("expected default sidebar.focus-search binding (mod+/) in result")
	}
}

// TestDefaultsIncludeNewHelpSearchAndInterruptBindings pins the
// chord assignments for features that ship visible UI (cheat sheet,
// message search, thread picker, working-indicator interrupt) so a
// refactor can't silently drop them. Users who rebind can override;
// but users who never open Settings should still discover these
// features by muscle memory.
func TestDefaultsIncludeNewHelpSearchAndInterruptBindings(t *testing.T) {
	want := map[string]string{
		"help.keybindings": "mod+shift+/",
		"search.messages":  "mod+shift+f",
		"search.in-thread": "mod+f",
		"thread.search":    "mod+p",
		"thread.interrupt": "esc",
	}
	got := make(map[string]string)
	for _, b := range Defaults {
		got[b.Command] = b.Key
	}
	for cmd, key := range want {
		if got[cmd] != key {
			t.Errorf("Defaults: %s = %q, want %q", cmd, got[cmd], key)
		}
	}
}

// TestModWClosesPaneAndYieldsToTerminalFocus pins the behavior change behind
// "ctrl/cmd+w shouldn't do anything in the terminal": mod+w must be the
// pane.close binding gated on !terminalFocus (so a focused xterm receives
// ctrl-w as werase instead of the pane closing), and the old
// terminalFocus-gated terminal.close twin must stay removed. Without this,
// a regression re-adding {mod+w, terminal.close, terminalFocus} would pass
// every other Defaults test.
func TestModWClosesPaneAndYieldsToTerminalFocus(t *testing.T) {
	var modW []Keybinding
	for _, b := range Defaults {
		if b.Key == "mod+w" {
			modW = append(modW, b)
		}
		if b.Command == "terminal.close" {
			t.Errorf("Defaults still bind terminal.close (%+v); mod+w must fall through to the shell as werase", b)
		}
	}
	if len(modW) != 1 {
		t.Fatalf("want exactly one mod+w binding, got %d: %+v", len(modW), modW)
	}
	if modW[0].Command != "pane.close" || modW[0].When != "!terminalFocus" {
		t.Errorf("mod+w = {Command:%q When:%q}, want {pane.close, !terminalFocus}", modW[0].Command, modW[0].When)
	}
}

// TestPaneNavVimChordsEscapeTerminalFocus pins the asymmetry behind "alt+h/l
// must navigate panes from inside a terminal, but alt+l must not run `ls`": the
// vim pane-nav chords are UN-gated (no !terminalFocus) so they bubble out of a
// focused xterm (TerminalBody's key handler recognises exactly these), while
// their alt+arrow twins KEEP the !terminalFocus gate so the shell still owns
// alt+arrow word-motion. A regression re-adding the gate to a vim chord would
// silently re-break alt+h/l inside the terminal.
func TestPaneNavVimChordsEscapeTerminalFocus(t *testing.T) {
	whenByKey := func(key string) (when string, count int) {
		for _, b := range Defaults {
			if b.Key == key {
				when = b.When
				count++
			}
		}
		return when, count
	}

	// Vim chords: un-gated (escape a focused terminal).
	for _, key := range []string{"alt+h", "alt+l", "alt+shift+h", "alt+shift+l"} {
		when, count := whenByKey(key)
		if count != 1 {
			t.Fatalf("want exactly one %q binding, got %d", key, count)
		}
		if when != "" {
			t.Errorf("%q When = %q, want \"\" (un-gated so it escapes a focused terminal)", key, when)
		}
	}

	// Arrow twins: keep the gate (shell owns alt+arrow word-motion).
	for _, key := range []string{"alt+arrowleft", "alt+arrowright", "alt+shift+arrowleft", "alt+shift+arrowright"} {
		when, count := whenByKey(key)
		if count != 1 {
			t.Fatalf("want exactly one %q binding, got %d", key, count)
		}
		if when != "!terminalFocus" {
			t.Errorf("%q When = %q, want %q (gated so the shell keeps alt+arrow word-motion)", key, when, "!terminalFocus")
		}
	}
}

// TestTerminalRefreshDefaultBinding pins the in-terminal repaint chord. The
// chord is behaviour-critical and NOT free to change silently: alt+shift+r is
// deliberate because the webview reserves Ctrl/Cmd+R and Ctrl/Cmd+Shift+R for
// reload (internal/uikeys), so a regression moving terminal.refresh to a mod+r
// chord would be swallowed by the webview and never reach the SPA. The
// terminalFocus gate scopes it to the recovery press from inside the terminal.
// Both properties pass every generic Defaults test, so pin them explicitly.
func TestTerminalRefreshDefaultBinding(t *testing.T) {
	var refresh []Keybinding
	for _, b := range Defaults {
		if b.Command == "terminal.refresh" {
			refresh = append(refresh, b)
		}
	}
	if len(refresh) != 1 {
		t.Fatalf("want exactly one terminal.refresh binding, got %d: %+v", len(refresh), refresh)
	}
	b := refresh[0]
	if b.Key != "alt+shift+r" {
		t.Errorf("terminal.refresh Key = %q, want %q (mod+r chords are reserved by the webview reload)", b.Key, "alt+shift+r")
	}
	if b.When != "terminalFocus" {
		t.Errorf("terminal.refresh When = %q, want %q (scopes the chord to the in-terminal recovery press)", b.When, "terminalFocus")
	}
	if b.DefaultID != "terminal.refresh" {
		t.Errorf("terminal.refresh DefaultID = %q, want %q", b.DefaultID, "terminal.refresh")
	}
}

// TestTerminalTabAndPaneDefaultBindings pins the terminal tab/pane management
// chords. terminal.newPane is UN-gated (no !terminalFocus) so Ctrl+Shift+~ opens
// a terminal pane from inside a focused terminal too — TerminalBody's key handler
// lets it escape via TERMINAL_ESCAPE_COMMAND_IDS, like the alt+h/l vim chords; a
// regression re-adding the gate would re-break it inside the terminal. The tab
// commands are gated terminalFocus (they act on the focused surface) and use
// LITERAL ctrl+tab for switching — mod+tab is Cmd+Tab on macOS (the OS app
// switcher), so it must NOT be written as mod.
func TestTerminalTabAndPaneDefaultBindings(t *testing.T) {
	only := func(command string) Keybinding {
		t.Helper()
		var matches []Keybinding
		for _, b := range Defaults {
			if b.Command == command {
				matches = append(matches, b)
			}
		}
		if len(matches) != 1 {
			t.Fatalf("want exactly one %q binding, got %d: %+v", command, len(matches), matches)
		}
		return matches[0]
	}

	// New pane: un-gated so it fires from the composer AND inside a focused xterm.
	newPane := only("terminal.newPane")
	if newPane.Key != "mod+shift+~" {
		t.Errorf("terminal.newPane Key = %q, want %q", newPane.Key, "mod+shift+~")
	}
	if newPane.When != "" {
		t.Errorf("terminal.newPane When = %q, want \"\" (un-gated so Ctrl+Shift+~ opens a pane from inside a focused terminal)", newPane.When)
	}

	// Tab management: gated terminalFocus, exact chords.
	cases := []struct {
		command string
		key     string
	}{
		{"terminal.newTab", "mod+shift+t"},
		{"terminal.closeTab", "mod+shift+w"},
		{"terminal.nextTab", "ctrl+tab"},
		{"terminal.prevTab", "ctrl+shift+tab"},
	}
	for _, tc := range cases {
		b := only(tc.command)
		if b.Key != tc.key {
			t.Errorf("%s Key = %q, want %q", tc.command, b.Key, tc.key)
		}
		if b.When != "terminalFocus" {
			t.Errorf("%s When = %q, want %q (acts on the focused terminal surface)", tc.command, b.When, "terminalFocus")
		}
		if b.DefaultID != tc.command {
			t.Errorf("%s DefaultID = %q, want %q", tc.command, b.DefaultID, tc.command)
		}
	}
}

// TestDefaultsHaveUniqueKeyWhenTuples guarantees no two defaults
// collide on the same (key, when) pair. Multiple chords for one
// command is fine (e.g. mod+n and mod+shift+o both → thread.new),
// but two different commands on the same chord under the same
// context would make one unreachable.
func TestDefaultsHaveUniqueKeyWhenTuples(t *testing.T) {
	seen := make(map[string]string)
	for _, b := range Defaults {
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

func TestDefaultsHaveUniqueDefaultIDs(t *testing.T) {
	seen := make(map[string]bool)
	for i, b := range Defaults {
		if b.DefaultID == "" {
			t.Fatalf("Defaults[%d] has empty DefaultID", i)
		}
		if seen[b.DefaultID] {
			t.Fatalf("Defaults[%d] duplicates DefaultID %q", i, b.DefaultID)
		}
		seen[b.DefaultID] = true
	}
}

// TestDefaultsUseValidChordSyntax is a smoke test guarding against
// typos like "mod+/ " or empty keys making it into defaults. The
// frontend's tryParseChord is the authoritative validator, but we
// check the basic shape here to catch obvious mistakes at build
// time.
func TestDefaultsUseValidChordSyntax(t *testing.T) {
	for i, b := range Defaults {
		if b.Key == "" {
			t.Errorf("Defaults[%d] has empty key", i)
		}
		if b.Command == "" {
			t.Errorf("Defaults[%d] has empty command", i)
		}
		// Leading/trailing whitespace isn't valid and usually means
		// a typo in the source.
		if b.Key != "" && (b.Key[0] == ' ' || b.Key[len(b.Key)-1] == ' ') {
			t.Errorf("Defaults[%d].Key = %q has surrounding whitespace", i, b.Key)
		}
	}
}

func TestUpdatePersistsAndMergesOverDefaults(t *testing.T) {
	svc := newServiceWithTempDir(t)

	user := []Keybinding{
		{Key: "mod+shift+p", Command: "palette.open"},
		{Key: "mod+l", Command: "terminal.toggle"},
	}
	if err := svc.Update(user); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	raw, err := os.ReadFile(svc.Path())
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

	merged, err := svc.Get()
	if err != nil {
		t.Fatalf("Get() error = %v", err)
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

func TestUpdateUsesPrivatePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not stable on Windows")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	svc, err := New(dir)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if err := svc.Update([]Keybinding{{Key: "mod+k", Command: "palette.open"}}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	assertMode(t, dir, 0o700)
	assertMode(t, svc.Path(), 0o600)
}

func TestGetRepairsExistingFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not stable on Windows")
	}
	svc := newServiceWithTempDir(t)
	if err := os.WriteFile(svc.Path(), []byte(`[{"key":"mod+k","command":"palette.open"}]`), 0o644); err != nil {
		t.Fatalf("writing keybindings file: %v", err)
	}

	if _, err := svc.Get(); err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	assertMode(t, svc.Path(), 0o600)
}

func TestUpdateRejectsEmptyKeyOrCommand(t *testing.T) {
	svc := newServiceWithTempDir(t)

	if err := svc.Update([]Keybinding{{Key: "", Command: "x"}}); err == nil {
		t.Fatal("expected error for empty key")
	}
	if err := svc.Update([]Keybinding{{Key: "mod+k", Command: "   "}}); err == nil {
		t.Fatal("expected error for empty command")
	}
}

func TestUpdateCapsMaxEntries(t *testing.T) {
	svc := newServiceWithTempDir(t)
	big := make([]Keybinding, MaxCount+5)
	for i := range big {
		big[i] = Keybinding{Key: "mod+k", Command: "palette.open"}
	}
	if err := svc.Update(big); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	// After capping, the written file should hold exactly MaxCount entries.
	raw, err := os.ReadFile(svc.Path())
	if err != nil {
		t.Fatalf("reading persisted file: %v", err)
	}
	var persisted []Keybinding
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("parsing persisted file: %v", err)
	}
	if len(persisted) != MaxCount {
		t.Fatalf("persisted entries = %d, want %d", len(persisted), MaxCount)
	}
}

func TestResetDeletesUserFile(t *testing.T) {
	svc := newServiceWithTempDir(t)

	if err := svc.Update([]Keybinding{{Key: "mod+k", Command: "palette.open"}}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if _, err := os.Stat(svc.Path()); err != nil {
		t.Fatalf("file not written: %v", err)
	}
	if err := svc.Reset(); err != nil {
		t.Fatalf("Reset() error = %v", err)
	}
	if _, err := os.Stat(svc.Path()); !os.IsNotExist(err) {
		t.Fatalf("file still exists after reset: err=%v", err)
	}
	// Reset on a missing file is a no-op.
	if err := svc.Reset(); err != nil {
		t.Fatalf("second Reset() error = %v", err)
	}
}

func TestGetFallsBackWhenFileMalformed(t *testing.T) {
	svc := newServiceWithTempDir(t)
	if err := os.WriteFile(svc.Path(), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("writing malformed file: %v", err)
	}
	kb, err := svc.Get()
	if err != nil {
		t.Fatalf("Get() returned error: %v", err)
	}
	if len(kb) != len(Defaults) {
		t.Fatalf("fallback len(kb) = %d, want %d", len(kb), len(Defaults))
	}
}

func TestMergePreservesOverriddenAndRetainsDefaults(t *testing.T) {
	defaults := []Keybinding{
		{Key: "mod+k", Command: "palette.open"},
		{Key: "mod+j", Command: "terminal.toggle"},
		{Key: "mod+n", Command: "terminal.new", When: "terminalFocus"},
	}
	user := []Keybinding{
		{Key: "mod+p", Command: "palette.open"},
		{Key: "mod+t", Command: "custom.action"},
	}
	out := Merge(defaults, user)

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

func TestMergeUsesDefaultIDForDuplicateCommandContexts(t *testing.T) {
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

	out := Merge(defaults, user)

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

func TestMergeMigratesLegacyDefaultKeyIdentity(t *testing.T) {
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

	out := Merge(defaults, user)

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

func TestMergeAppendsOverridesAfterDefaultsForRuntimePrecedence(t *testing.T) {
	defaults := []Keybinding{
		{Key: "mod+x", Command: "command.a", DefaultID: "command.a"},
		{Key: "mod+y", Command: "command.b", DefaultID: "command.b"},
	}
	user := []Keybinding{
		{Key: "mod+y", Command: "command.a", DefaultID: "command.a"},
	}

	out := Merge(defaults, user)

	if len(out) != 2 {
		t.Fatalf("merged len = %d, want 2", len(out))
	}
	if out[0].Command != "command.b" || out[1].Command != "command.a" {
		t.Fatalf("merged order = [%s, %s], want defaults before override", out[0].Command, out[1].Command)
	}
}

func TestNewFallsBackToHomeWhenConfigDirEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	svc, err := New("")
	if err != nil {
		t.Fatalf("New(\"\") error = %v", err)
	}
	want := filepath.Join(os.Getenv("HOME"), ".agent-overflow", FileName)
	if svc.Path() != want {
		t.Fatalf("Path() = %q, want %q", svc.Path(), want)
	}
}

func TestPathLayout(t *testing.T) {
	dir := t.TempDir()
	svc, err := New(dir)
	if err != nil {
		t.Fatalf("New error: %v", err)
	}
	want := filepath.Join(dir, FileName)
	if svc.Path() != want {
		t.Fatalf("Path() = %q, want %q", svc.Path(), want)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode %s = %o, want %o", path, got, want)
	}
}
