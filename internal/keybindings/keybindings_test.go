package keybindings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	res := svc.Get()
	if res.LoadError != "" {
		// A missing file is a fresh install, not a failure to read one.
		t.Fatalf("Get() LoadError = %q, want empty", res.LoadError)
	}
	kb := res.Bindings
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
// message search, thread picker, account switcher, working-indicator
// interrupt) so a refactor can't silently drop them. Users who rebind
// can override; but users who never open Settings should still
// discover these features by muscle memory.
func TestDefaultsIncludeNewHelpSearchAndInterruptBindings(t *testing.T) {
	want := map[string]string{
		"help.keybindings":       "mod+shift+/",
		"search.messages":        "mod+shift+f",
		"search.in-thread":       "mod+f",
		"thread.search":          "mod+p",
		"provider.switchAccount": "mod+shift+u",
		"thread.interrupt":       "esc",
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

// TestAccountSwitcherDefaultIsContextFree pins that the account-switcher
// chord ships with no `when` gate: switching provider accounts is reachable
// from anywhere, including with no thread open. Only the frontend's
// editable-target rules and the command's own view-only gate narrow it.
func TestAccountSwitcherDefaultIsContextFree(t *testing.T) {
	var matches []Keybinding
	for _, b := range Defaults {
		if b.Command == "provider.switchAccount" {
			matches = append(matches, b)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("want exactly one provider.switchAccount binding, got %d: %+v", len(matches), matches)
	}
	if matches[0].When != "" {
		t.Errorf("provider.switchAccount When = %q, want empty", matches[0].When)
	}
}

// TestSettingsDefaultBindings pins the settings pair. The Esc row's `when`
// must stay `settingsOpen` and must stay mutually exclusive with
// thread.interrupt's `!anyModalOpen` arm — if either drifts, one Esc press
// could match both rows and the winner would depend on Defaults ordering
// (the frontend dispatches the resolved list in reverse).
func TestSettingsDefaultBindings(t *testing.T) {
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

	open := only("settings.open")
	if open.Key != "mod+," || open.When != "" {
		t.Errorf("settings.open = {Key:%q When:%q}, want {mod+, \"\"}", open.Key, open.When)
	}

	closeBinding := only("settings.close")
	if closeBinding.Key != "esc" {
		t.Errorf("settings.close Key = %q, want %q", closeBinding.Key, "esc")
	}
	if closeBinding.When != "settingsOpen" {
		t.Errorf("settings.close When = %q, want %q", closeBinding.When, "settingsOpen")
	}
	if closeBinding.DefaultID != "settings.close" {
		t.Errorf("settings.close DefaultID = %q, want %q", closeBinding.DefaultID, "settings.close")
	}

	interrupt := only("thread.interrupt")
	if !strings.Contains(interrupt.When, "!anyModalOpen") {
		t.Errorf(
			"thread.interrupt When = %q, want it to exclude modal surfaces so esc cannot match it and settings.close at once",
			interrupt.When,
		)
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

// TestPaneNavDefaultsUseVimChordsOnly pins the macOS-friendly default: alt+h/l
// navigate panes, while alt+arrow is left to native text-editing word motion.
// The vim pane-nav chords are UN-gated (no !terminalFocus) so they bubble out of
// a focused xterm; a regression re-adding the gate would silently re-break
// alt+h/l inside the terminal.
func TestPaneNavDefaultsUseVimChordsOnly(t *testing.T) {
	bindingsByKey := func(key string) []Keybinding {
		var matches []Keybinding
		for _, b := range Defaults {
			if b.Key == key {
				matches = append(matches, b)
			}
		}
		return matches
	}

	expected := map[string]string{
		"alt+h":       "pane.focusLeft",
		"alt+l":       "pane.focusRight",
		"alt+shift+h": "pane.moveLeft",
		"alt+shift+l": "pane.moveRight",
	}
	for key, command := range expected {
		matches := bindingsByKey(key)
		if len(matches) != 1 {
			t.Fatalf("want exactly one %q binding, got %d", key, len(matches))
		}
		if matches[0].Command != command {
			t.Errorf("%q Command = %q, want %q", key, matches[0].Command, command)
		}
		if matches[0].When != "" {
			t.Errorf("%q When = %q, want \"\" (un-gated so it escapes a focused terminal)", key, matches[0].When)
		}
	}

	for _, key := range []string{"alt+arrowleft", "alt+arrowright", "alt+shift+arrowleft", "alt+shift+arrowright"} {
		matches := bindingsByKey(key)
		if len(matches) != 0 {
			t.Errorf("want no default %q binding, got %d: %+v", key, len(matches), matches)
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

	loaded := svc.Get()
	if loaded.LoadError != "" {
		t.Fatalf("Get() LoadError = %q, want empty", loaded.LoadError)
	}
	merged := loaded.Bindings
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

	if got := svc.Get().LoadError; got != "" {
		t.Fatalf("Get() LoadError = %q, want empty", got)
	}

	assertMode(t, svc.Path(), 0o600)
}

func TestUpdateRejectsEmptyKeyOrCommand(t *testing.T) {
	svc := newServiceWithTempDir(t)

	// An empty key with no default identity is a dropped chord, not a
	// deliberate clear — see Service.Update.
	if err := svc.Update([]Keybinding{{Key: "", Command: "x"}}); err == nil {
		t.Fatal("expected error for empty key with no default identity")
	}
	if err := svc.Update([]Keybinding{{Key: "   ", Command: "x"}}); err == nil {
		t.Fatal("expected error for whitespace-only key with no default identity")
	}
	if err := svc.Update([]Keybinding{{Key: "mod+k", Command: "   "}}); err == nil {
		t.Fatal("expected error for empty command")
	}
	if err := svc.Update([]Keybinding{{Key: Unbound, Command: "   ", DefaultID: "palette.open"}}); err == nil {
		t.Fatal("expected error for empty command on an unbound entry")
	}
}

// --- unbound sentinel ---

// findMerged returns the merged row for a command in a `when` context.
func findMerged(t *testing.T, merged []Keybinding, command, when string) (Keybinding, bool) {
	t.Helper()
	for _, b := range merged {
		if b.Command == command && b.When == when {
			return b, true
		}
	}
	return Keybinding{}, false
}

func TestUpdateAcceptsUnboundWithDefaultIdentity(t *testing.T) {
	svc := newServiceWithTempDir(t)

	// Both identity shapes are accepted: the stable DefaultID and the
	// legacy DefaultKey a pre-DefaultID config carries.
	for _, entry := range []Keybinding{
		{Key: Unbound, Command: "palette.open", DefaultID: "palette.open"},
		{Key: Unbound, Command: "palette.open", DefaultKey: "mod+shift+k"},
	} {
		if err := svc.Update([]Keybinding{entry}); err != nil {
			t.Fatalf("Update(%+v) error = %v", entry, err)
		}
	}
}

func TestUnboundOverrideSuppressesTheDefaultChord(t *testing.T) {
	svc := newServiceWithTempDir(t)

	if err := svc.Update([]Keybinding{
		{Key: Unbound, Command: "palette.open", DefaultID: "palette.open", DefaultKey: "mod+shift+k"},
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	merged := svc.Get().Bindings
	row, ok := findMerged(t, merged, "palette.open", "")
	if !ok {
		// The row must SURVIVE with no chord: settings renders it so the
		// user can restore the default. Dropping it would strand them.
		t.Fatal("palette.open row missing from merged output after unbind")
	}
	if !IsUnbound(row) {
		t.Fatalf("palette.open key = %q, want unbound", row.Key)
	}
	if row.DefaultKey != "mod+shift+k" {
		t.Fatalf("palette.open defaultKey = %q, want mod+shift+k (needed to restore)", row.DefaultKey)
	}
	// No other row may still carry the shipped chord for that command.
	for _, b := range merged {
		if b.Command == "palette.open" && b.Key == "mod+shift+k" {
			t.Fatal("shipped palette.open chord still present after unbind")
		}
	}
}

// The transition sequence, not just the states: bind → unbind → rebind
// → reset, each step observed through a fresh Get so persistence is in
// the loop.
func TestUnboundSurvivesBindUnbindRebindResetSequence(t *testing.T) {
	svc := newServiceWithTempDir(t)
	identity := Keybinding{
		Command:    "palette.open",
		DefaultID:  "palette.open",
		DefaultKey: "mod+shift+k",
	}
	keyAfter := func(t *testing.T) string {
		t.Helper()
		row, ok := findMerged(t, svc.Get().Bindings, "palette.open", "")
		if !ok {
			t.Fatal("palette.open row missing from merged output")
		}
		return row.Key
	}

	if got := keyAfter(t); got != "mod+shift+k" {
		t.Fatalf("initial key = %q, want the shipped default", got)
	}

	// bind
	bound := identity
	bound.Key = "mod+o"
	if err := svc.Update([]Keybinding{bound}); err != nil {
		t.Fatalf("Update(bind) error = %v", err)
	}
	if got := keyAfter(t); got != "mod+o" {
		t.Fatalf("after bind key = %q, want mod+o", got)
	}

	// unbind — must not fall back to the default
	cleared := identity
	cleared.Key = Unbound
	if err := svc.Update([]Keybinding{cleared}); err != nil {
		t.Fatalf("Update(unbind) error = %v", err)
	}
	if got := keyAfter(t); got != Unbound {
		t.Fatalf("after unbind key = %q, want unbound", got)
	}

	// rebind out of unbound
	rebound := identity
	rebound.Key = "mod+shift+z"
	if err := svc.Update([]Keybinding{rebound}); err != nil {
		t.Fatalf("Update(rebind) error = %v", err)
	}
	if got := keyAfter(t); got != "mod+shift+z" {
		t.Fatalf("after rebind key = %q, want mod+shift+z", got)
	}

	// unbind again, then reset back to defaults
	if err := svc.Update([]Keybinding{cleared}); err != nil {
		t.Fatalf("Update(unbind again) error = %v", err)
	}
	if got := keyAfter(t); got != Unbound {
		t.Fatalf("after second unbind key = %q, want unbound", got)
	}
	if err := svc.Reset(); err != nil {
		t.Fatalf("Reset() error = %v", err)
	}
	if got := keyAfter(t); got != "mod+shift+k" {
		t.Fatalf("after reset key = %q, want the shipped default", got)
	}
}

func TestUnbindingOneRowLeavesItsSiblingBound(t *testing.T) {
	svc := newServiceWithTempDir(t)
	// thread.new ships two default rows; clearing one must not disturb
	// the other.
	if err := svc.Update([]Keybinding{{
		Key:        Unbound,
		Command:    "thread.new",
		When:       "!terminalFocus",
		DefaultID:  "thread.new.alternate",
		DefaultKey: "mod+shift+o",
	}}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	byID := map[string]Keybinding{}
	merged := svc.Get().Bindings
	for _, b := range merged {
		if b.Command == "thread.new" {
			byID[b.DefaultID] = b
		}
	}
	if got := byID["thread.new.primary"].Key; got != "mod+n" {
		t.Fatalf("thread.new.primary key = %q, want mod+n", got)
	}
	if got := byID["thread.new.alternate"].Key; got != Unbound {
		t.Fatalf("thread.new.alternate key = %q, want unbound", got)
	}
}

func TestGetNormalizesWhitespaceOnlyKeyToUnbound(t *testing.T) {
	svc := newServiceWithTempDir(t)
	// Hand-edited file: a whitespace key is the same intent as an empty
	// one, and must not reach the frontend as an unparseable chord.
	raw := `[{"key":"   ","command":"palette.open","defaultId":"palette.open"}]`
	if err := os.WriteFile(svc.Path(), []byte(raw), 0o600); err != nil {
		t.Fatalf("writing keybindings file: %v", err)
	}

	row, ok := findMerged(t, svc.Get().Bindings, "palette.open", "")
	if !ok {
		t.Fatal("palette.open row missing from merged output")
	}
	if row.Key != Unbound {
		t.Fatalf("palette.open key = %q, want the canonical empty sentinel", row.Key)
	}
}

func TestUpdatePersistsTheTrimmedKey(t *testing.T) {
	svc := newServiceWithTempDir(t)
	if err := svc.Update([]Keybinding{
		{Key: " mod+o ", Command: "palette.open", DefaultID: "palette.open"},
	}); err != nil {
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
	if persisted[0].Key != "mod+o" {
		t.Fatalf("persisted key = %q, want mod+o", persisted[0].Key)
	}
}

func TestMergeDropsUnboundEntryThatMatchesNoDefault(t *testing.T) {
	defaults := []Keybinding{{Key: "mod+k", Command: "palette.open", DefaultID: "palette.open"}}
	user := []Keybinding{
		// Names a default this release no longer ships.
		{Key: Unbound, Command: "gone.command", DefaultID: "gone.command"},
		// A REBIND that matches nothing is an extra binding the user
		// wanted — it must still survive.
		{Key: "mod+t", Command: "custom.action"},
	}
	out := Merge(defaults, user)

	for _, b := range out {
		if b.Command == "gone.command" {
			t.Fatal("orphan unbound entry survived the merge")
		}
	}
	hasCustom := false
	for _, b := range out {
		if b.Command == "custom.action" {
			hasCustom = true
		}
	}
	if !hasCustom {
		t.Fatal("orphan REBIND was dropped; only unbound orphans should be")
	}
}

func TestMergeDoesNotMutateCallerSlice(t *testing.T) {
	defaults := []Keybinding{{Key: "mod+k", Command: "palette.open", DefaultID: "palette.open"}}
	user := []Keybinding{{Key: "  mod+o  ", Command: "palette.open", DefaultID: "palette.open"}}
	Merge(defaults, user)
	if user[0].Key != "  mod+o  " {
		t.Fatalf("Merge mutated its input: key = %q", user[0].Key)
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

// --- the load-error contract ---

// A malformed file must not read as a fresh install. The bindings still
// come back (Defaults), and the reason is reported so the frontend can
// warn before the next Update overwrites the file.
func TestGetReportsLoadErrorForMalformedFile(t *testing.T) {
	svc := newServiceWithTempDir(t)
	if err := os.WriteFile(svc.Path(), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("writing malformed file: %v", err)
	}

	res := svc.Get()
	if len(res.Bindings) != len(Defaults) {
		t.Fatalf("fallback len(Bindings) = %d, want %d", len(res.Bindings), len(Defaults))
	}
	if res.LoadError == "" {
		t.Fatal("LoadError is empty for a malformed file — the overwrite would be silent")
	}
	if !strings.Contains(res.LoadError, "parse keybindings") {
		t.Fatalf("LoadError = %q, want it to name the parse failure", res.LoadError)
	}
}

func TestGetReportsNoLoadErrorForAReadableFile(t *testing.T) {
	svc := newServiceWithTempDir(t)
	if err := svc.Update([]Keybinding{
		{Key: "mod+o", Command: "palette.open", DefaultID: "palette.open"},
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	res := svc.Get()
	if res.LoadError != "" {
		t.Fatalf("LoadError = %q, want empty for a readable file", res.LoadError)
	}
	row, ok := findMerged(t, res.Bindings, "palette.open", "")
	if !ok {
		t.Fatal("palette.open row missing from merged output")
	}
	if row.Key != "mod+o" {
		t.Fatalf("palette.open key = %q, want mod+o", row.Key)
	}
}

// The TRANSITION, not just the two states: a broken file reports, and
// repairing it clears the report AND applies the repaired overrides.
// A LoadError that outlives the file it described would warn about an
// overwrite that is no longer destructive.
func TestGetLoadErrorClearsWhenTheFileIsRepaired(t *testing.T) {
	svc := newServiceWithTempDir(t)
	if err := os.WriteFile(svc.Path(), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("writing malformed file: %v", err)
	}
	if svc.Get().LoadError == "" {
		t.Fatal("LoadError is empty for a malformed file")
	}

	repaired := `[{"key":"mod+o","command":"palette.open","defaultId":"palette.open"}]`
	if err := os.WriteFile(svc.Path(), []byte(repaired), 0o600); err != nil {
		t.Fatalf("writing repaired file: %v", err)
	}

	res := svc.Get()
	if res.LoadError != "" {
		t.Fatalf("LoadError = %q after repair, want empty", res.LoadError)
	}
	row, ok := findMerged(t, res.Bindings, "palette.open", "")
	if !ok {
		t.Fatal("palette.open row missing from merged output")
	}
	if row.Key != "mod+o" {
		t.Fatalf("palette.open key = %q after repair, want the repaired override", row.Key)
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

// TestSidebarToggleBindsModBAndYieldsToTerminalFocus pins the sidebar
// collapse chord. mod+b is the editor convention for this (VS Code,
// Zed), and the !terminalFocus gate is not cosmetic: off macOS `mod`
// resolves to Ctrl, and ctrl+b is tmux's default prefix, so a
// terminal-focused press has to reach the PTY instead of moving the
// app's furniture.
func TestSidebarToggleBindsModBAndYieldsToTerminalFocus(t *testing.T) {
	var found int
	for _, b := range Defaults {
		if b.Command != "sidebar.toggle" {
			continue
		}
		found++
		if b.Key != "mod+b" {
			t.Errorf("sidebar.toggle key = %q, want %q", b.Key, "mod+b")
		}
		if b.When != "!terminalFocus" {
			t.Errorf("sidebar.toggle when = %q, want %q", b.When, "!terminalFocus")
		}
	}
	if found != 1 {
		t.Fatalf("Defaults bind sidebar.toggle %d times, want exactly 1", found)
	}
	// mod+b must not be shared with another command in an ungated or
	// non-terminal context — the collapse would shadow it everywhere
	// outside a focused terminal.
	for _, b := range Defaults {
		if b.Key == "mod+b" && b.Command != "sidebar.toggle" {
			t.Errorf("mod+b is also bound to %q (when %q)", b.Command, b.When)
		}
	}
}
