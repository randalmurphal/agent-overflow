package keybindings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Keybinding is the on-disk / over-the-wire shape of a single
// keybinding. `When` is an optional VS Code-style context expression
// (e.g. "!terminalFocus" or "terminalOpen && !approvalPending").
// `DefaultID` identifies the shipped default row that a user entry
// overrides, so multiple default chords can target the same command
// under the same context and still be rebound independently.
// `DefaultKey` is retained as a legacy fallback for configs written
// before stable IDs existed. The frontend validates key and when
// syntax; the backend persists the strings verbatim after checking
// required fields are present.
type Keybinding struct {
	Key        string `json:"key"`
	Command    string `json:"command"`
	When       string `json:"when,omitempty"`
	DefaultID  string `json:"defaultId,omitempty"`
	DefaultKey string `json:"defaultKey,omitempty"`
}

// FileName is the basename of the persisted user-override file inside
// the app's config dir.
const FileName = "keybindings.json"

// MaxCount caps the persisted user override list. Entries past the
// cap are dropped from the tail by Update.
const MaxCount = 256

const (
	privateDirPerm    os.FileMode = 0o700
	sensitiveFilePerm os.FileMode = 0o600
)

// Defaults are returned when the config file is missing or malformed.
// User entries with DefaultID override that exact default row;
// entries with only DefaultKey use the legacy (command, when,
// original key) identity, and older entries without either field
// fall back to the first matching (command, when) default.
//
// The set is a curated subset of common editor / chat actions:
// entries that don't apply to agent-overflow (terminal.split,
// script.*.run, most editor.* commands) are omitted, and the
// command-palette / app-level actions this wave introduces are
// layered on top.
var Defaults = []Keybinding{
	// Keybinding-overhaul swap: mod+j / mod+k are reserved for the
	// sidebar visual selector (no DOM focus change). Terminal
	// toggle moves to mod+` (matches VS Code: open+focus when closed,
	// close when open), the sidebar search input moves to mod+/, and
	// the cheat sheet moves to mod+shift+/. The sidebar.focus-search
	// binding fires globally
	// (empty `when`) so it works from the composer, the review pane,
	// the terminal, etc.
	{Key: "mod+shift+k", Command: "palette.open", DefaultID: "palette.open"},
	{Key: "mod+/", Command: "sidebar.focus-search", DefaultID: "sidebar.focus-search"},
	{Key: "mod+`", Command: "terminal.toggle", DefaultID: "terminal.toggle"},
	{Key: "mod+j", Command: "sidebar.cursor.down", DefaultID: "sidebar.cursor.down"},
	{Key: "mod+k", Command: "sidebar.cursor.up", DefaultID: "sidebar.cursor.up"},
	{Key: "mod+enter", Command: "sidebar.cursor.open", When: "sidebarCursorActive && !anyModalOpen", DefaultID: "sidebar.cursor.open"},
	{Key: "mod+shift+enter", Command: "sidebar.cursor.openInNewPane", When: "sidebarCursorActive && !anyModalOpen", DefaultID: "sidebar.cursor.openInNewPane"},
	// mod+b collapses the sidebar to its rail and back — the editor
	// convention (VS Code, Zed) for exactly this. `!terminalFocus` for
	// the same reason mod+w carries it: off macOS `mod` is Ctrl, and
	// ctrl+b is tmux's default prefix, so a terminal-focused ctrl+b
	// must reach the PTY rather than move the app's furniture. The
	// command is editableReachable, so it still fires from the composer
	// and every other text field.
	{Key: "mod+b", Command: "sidebar.toggle", When: "!terminalFocus", DefaultID: "sidebar.toggle"},
	{Key: "mod+/", Command: "picker.toggleInput", When: "anyPickerOpen", DefaultID: "picker.toggleInput"},
	{Key: "mod+n", Command: "terminal.new", When: "terminalFocus", DefaultID: "terminal.new"},
	// terminal.refresh repaints a glitched terminal from inside it (PTY winsize
	// nudge → provider redraw, no visible xterm resize). alt+shift+r, NOT
	// mod+(shift+)r: the webview reserves Ctrl/Cmd+R and Ctrl/Cmd+Shift+R for
	// reload / force-reload (internal/uikeys), so those never reach the SPA.
	// Unlike terminal.new above, this one DOES fire from inside the xterm: it is
	// both editableReachable AND a member of the frontend TERMINAL_ESCAPE_COMMAND_IDS
	// set, so the chord escapes the focused terminal (like alt+shift+h/l) instead
	// of encoding to the PTY. The terminalFocus gate scopes it to that recovery press.
	{Key: "alt+shift+r", Command: "terminal.refresh", When: "terminalFocus", DefaultID: "terminal.refresh"},
	// NOTE: the `!terminalFocus`-gated chords below (mod+w → pane.close, the
	// thread.new family) must NOT fire from inside a focused xterm — ctrl-w in
	// particular has to reach the shell as werase. The gate only holds because
	// the keydown dispatcher reads terminalFocus FRESH per keypress; see
	// App.svelte handleGlobalKeydown for why a memoized read regressed it.
	//
	// mod+w intentionally has no terminalFocus twin (ctrl-w → werase). The twins
	// that do exist (mod+n → terminal.new above) are inert from inside the xterm
	// for a separate reason: terminal.new isn't editableReachable, so it can't
	// reach the editable xterm textarea and falls through to the shell.
	//
	// Pane navigation uses Option/Alt + h/l by default, not Option/Alt + arrow.
	// On macOS, Option+Arrow is word-motion in text fields; stealing it for pane
	// focus makes normal editing feel broken. The vim chords carry NO
	// terminalFocus gate so they drive pane focus/move even from inside a
	// terminal: TerminalBody's xterm key handler recognises un-gated bindings
	// for these pane-nav commands and lets them bubble to the app instead of
	// writing them to the PTY (writing them is what made alt+l run `ls`). Users
	// can still rebind these commands in Settings if they prefer arrow chords.
	{Key: "mod+n", Command: "thread.new", When: "!terminalFocus", DefaultID: "thread.new.primary"},
	{Key: "mod+shift+n", Command: "thread.newPane", When: "!terminalFocus", DefaultID: "thread.newPane"},
	// Uses the SHIFTED glyph `~`, not the bare backtick: on Chromium platforms
	// Ctrl/Cmd+Shift+` yields event.key="~", which the matcher compares
	// literally, so a `mod+shift+\`` binding would never fire there. On macOS
	// WebKit the layout's Cmd table strips Shift (Cmd+Shift+` arrives as
	// key="`"); the frontend matcher reconstructs the shifted glyph from
	// event.code for exactly that case (keybindingParser.ts,
	// macShiftedGlyphFromCode), so the `~` spelling works on both. UN-gated
	// (no !terminalFocus) so it opens a terminal pane from the composer AND from
	// inside a focused terminal: TerminalBody's xterm key handler lets it bubble
	// out via TERMINAL_ESCAPE_COMMAND_IDS, exactly like the alt+h/l vim pane-nav
	// chords. The command carries no `when`, so it also stays palette-runnable.
	{Key: "mod+shift+~", Command: "terminal.newPane", DefaultID: "terminal.newPane"},
	// Terminal tab management. All gated `terminalFocus` (they act on the focused
	// terminal surface) and members of the frontend TERMINAL_ESCAPE_COMMAND_IDS
	// set, so the chord escapes a focused xterm to run the app command instead of
	// being encoded to the PTY (same mechanism as terminal.refresh). New-tab/
	// close-tab use mod+shift (Cmd on macOS); tab-switching uses LITERAL ctrl+tab,
	// not mod+tab — mod+tab is Cmd+Tab on macOS (the OS app switcher), and ctrl+tab
	// is the cross-platform tab-cycle convention, distinct from plain Tab (shell
	// completion). Copy/paste are handled inside TerminalBody, not here (platform
	// split + native-clipboard access).
	{Key: "mod+shift+t", Command: "terminal.newTab", When: "terminalFocus", DefaultID: "terminal.newTab"},
	{Key: "mod+shift+w", Command: "terminal.closeTab", When: "terminalFocus", DefaultID: "terminal.closeTab"},
	{Key: "ctrl+tab", Command: "terminal.nextTab", When: "terminalFocus", DefaultID: "terminal.nextTab"},
	{Key: "ctrl+shift+tab", Command: "terminal.prevTab", When: "terminalFocus", DefaultID: "terminal.prevTab"},
	{Key: "mod+shift+o", Command: "thread.new", When: "!terminalFocus", DefaultID: "thread.new.alternate"},
	{Key: "mod+shift+d", Command: "thread.new.discussion", When: "!terminalFocus", DefaultID: "thread.new.discussion"},
	{Key: "mod+w", Command: "pane.close", When: "!terminalFocus", DefaultID: "pane.close"},
	{Key: "alt+h", Command: "pane.focusLeft", DefaultID: "pane.focusLeft.vim"},
	{Key: "alt+l", Command: "pane.focusRight", DefaultID: "pane.focusRight.vim"},
	{Key: "alt+shift+h", Command: "pane.moveLeft", DefaultID: "pane.moveLeft.vim"},
	{Key: "alt+shift+l", Command: "pane.moveRight", DefaultID: "pane.moveRight.vim"},
	{Key: "mod+shift+g", Command: "diff.panel.toggle", When: "hasActiveThread && !terminalFocus", DefaultID: "diff.panel.toggle"},
	// mod+f is in-thread "find" (search the current thread's messages), matching
	// the editor convention where Ctrl+F finds in the current document and
	// Ctrl+Shift+F (search.messages) finds across everything. The sidebar
	// thread-search it replaced stays on mod+/ (sidebar.focus-search) and the
	// command palette.
	{Key: "mod+f", Command: "search.in-thread", When: "hasActiveThread && !terminalFocus", DefaultID: "search.in-thread"},
	{Key: "mod+,", Command: "settings.open", DefaultID: "settings.open"},
	{Key: "esc", Command: "thread.interrupt", When: "hasActiveThread && (turnActive || sendInFlight || hasPendingPrompt) && !anyModalOpen", DefaultID: "thread.interrupt"},
	{Key: "mod+1", Command: "thread.jump.1", DefaultID: "thread.jump.1"},
	{Key: "mod+2", Command: "thread.jump.2", DefaultID: "thread.jump.2"},
	{Key: "mod+3", Command: "thread.jump.3", DefaultID: "thread.jump.3"},
	{Key: "mod+4", Command: "thread.jump.4", DefaultID: "thread.jump.4"},
	{Key: "mod+5", Command: "thread.jump.5", DefaultID: "thread.jump.5"},
	{Key: "mod+6", Command: "thread.jump.6", DefaultID: "thread.jump.6"},
	{Key: "mod+7", Command: "thread.jump.7", DefaultID: "thread.jump.7"},
	{Key: "mod+8", Command: "thread.jump.8", DefaultID: "thread.jump.8"},
	{Key: "mod+9", Command: "thread.jump.9", DefaultID: "thread.jump.9"},
	// Help + global message search — both open dialogs that trap
	// focus and close with Esc, so they don't need separate
	// `!terminalFocus` guards the way thread.new does.
	{Key: "mod+shift+/", Command: "help.keybindings", DefaultID: "help.keybindings"},
	{Key: "mod+shift+f", Command: "search.messages", DefaultID: "search.messages"},
	// mod+p opens the unified thread picker — fuzzy-jump across
	// every project's threads. Like mod+shift+f this traps focus +
	// closes on Esc so no `!terminalFocus` guard is needed.
	{Key: "mod+p", Command: "thread.search", DefaultID: "thread.search"},
	// Composer toolbar pickers. Each chord opens the menu; pressing
	// the same chord while the menu is open closes it (toggle).
	{Key: "mod+shift+m", Command: "composer.picker.model", When: "hasActiveThread && !anyModalOpen", DefaultID: "composer.picker.model"},
	{Key: "mod+shift+e", Command: "composer.picker.effort", When: "hasActiveThread && !anyModalOpen", DefaultID: "composer.picker.effort"},
	{Key: "mod+shift+a", Command: "composer.picker.access", When: "hasActiveThread && !anyModalOpen", DefaultID: "composer.picker.access"},
	{Key: "mod+shift+b", Command: "composer.picker.branch", When: "hasActiveThread && !anyModalOpen", DefaultID: "composer.picker.branch"},
	// shift+tab cycles the active thread through chat → plan →
	// design. The `when` expression keeps the chord inert while the
	// palette or any modal has focus so Shift+Tab's default "focus
	// prev" behaviour still wins inside those surfaces.
	{Key: "shift+tab", Command: "mode.cycle", When: "hasActiveThread && !paletteOpen && !anyModalOpen", DefaultID: "mode.cycle"},
	// Workflows overlay (workflows-system-ui/UI-SPEC.md §8). Every chord is
	// gated on the overlay being open, so the bare letters and digits below are
	// inert everywhere else; the SPA additionally suppresses them while a text
	// field has focus (App.svelte's editable-target check), which is what lets
	// `a` stay "primary action" on the surface and "the letter a" in the answer
	// box. `esc` sits AFTER thread.interrupt because dispatch walks the resolved
	// list in reverse — the overlay must consume Escape before a background
	// turn does.
	{Key: "mod+shift+w", Command: "workflows.toggle", DefaultID: "workflows.toggle"},
	{Key: "esc", Command: "workflows.escape", When: "workflowsOverlayOpen", DefaultID: "workflows.escape"},
	{Key: "backspace", Command: "workflows.back", When: "workflowsRunDetail", DefaultID: "workflows.back"},
	{Key: "j", Command: "workflows.sweep.next", When: "workflowsRunDetail", DefaultID: "workflows.sweep.next"},
	{Key: "arrowdown", Command: "workflows.sweep.next", When: "workflowsRunDetail", DefaultID: "workflows.sweep.next.down"},
	{Key: "arrowright", Command: "workflows.sweep.next", When: "workflowsRunDetail", DefaultID: "workflows.sweep.next.right"},
	{Key: "k", Command: "workflows.sweep.prev", When: "workflowsRunDetail", DefaultID: "workflows.sweep.prev"},
	{Key: "arrowup", Command: "workflows.sweep.prev", When: "workflowsRunDetail", DefaultID: "workflows.sweep.prev.up"},
	{Key: "arrowleft", Command: "workflows.sweep.prev", When: "workflowsRunDetail", DefaultID: "workflows.sweep.prev.left"},
	{Key: "a", Command: "workflows.action.primary", When: "workflowsRunDetail", DefaultID: "workflows.action.primary"},
	{Key: "r", Command: "workflows.action.reject", When: "workflowsRunDetail", DefaultID: "workflows.action.reject"},
	{Key: "t", Command: "workflows.action.thread", When: "workflowsRunDetail", DefaultID: "workflows.action.thread"},
	{Key: "u", Command: "workflows.action.retry-units", When: "workflowsRunDetail", DefaultID: "workflows.action.retry-units"},
	{Key: "enter", Command: "workflows.action.enter", When: "workflowsRunDetail", DefaultID: "workflows.action.enter"},
	// UI-SPEC §8 also lists 1–9 for "pick + send the nth suggested answer".
	// The phase envelope (internal/workflow/def/envelope.go) carries a single
	// `question` string and forbids outputs on a question status, so there is
	// no suggested-answer list to pick from; binding the digits would ship dead
	// keys. They land the day the envelope grows the field.
}

// Service owns the persisted user-override file and serializes
// Get/Update/Reset against it under a private mutex.
type Service struct {
	mu   sync.Mutex
	path string
}

// New returns a Service that reads / writes <configDir>/<FileName>.
// When configDir is empty it falls back to ~/.agent-overflow/ so
// early-boot callers can still write keybindings if the OS config
// directory failed to resolve. Returns an error only when neither
// path is available (HOME unset).
func New(configDir string) (*Service, error) {
	if configDir != "" {
		return &Service{path: filepath.Join(configDir, FileName)}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("keybindings: cannot determine home directory: %w", err)
	}
	return &Service{path: filepath.Join(home, ".agent-overflow", FileName)}, nil
}

// Path returns the absolute config-file path. Exposed for tests and
// for the rare callsite that wants to surface the location to the
// user.
func (s *Service) Path() string { return s.path }

// Get returns the effective keybindings: Defaults with any user
// overrides layered on top. Invalid entries in the user file are
// dropped silently (the frontend surfaces parse errors on its own);
// a completely malformed file falls back to Defaults.
func (s *Service) Get() ([]Keybinding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	user, _ := readFile(s.path)
	return Merge(Defaults, user), nil
}

// Update replaces the user keybindings file. The config is capped at
// MaxCount entries; entries past the cap are dropped from the tail.
// Empty key / empty command both return errors.
func (s *Service) Update(bindings []Keybinding) error {
	if len(bindings) > MaxCount {
		bindings = bindings[:MaxCount]
	}
	for i, b := range bindings {
		if strings.TrimSpace(b.Key) == "" {
			return fmt.Errorf("keybinding %d: key is empty", i)
		}
		if strings.TrimSpace(b.Command) == "" {
			return fmt.Errorf("keybinding %d: command is empty", i)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return writeFile(s.path, bindings)
}

// Reset deletes the user file so Get returns Defaults. Reset on a
// missing file is a no-op (returns nil), so callers can treat the
// "revert to defaults" button as idempotent.
func (s *Service) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reset keybindings: %w", err)
	}
	return nil
}

// readFile loads the user config. Missing file returns (nil, nil).
// A malformed JSON file returns (nil, error) so callers can decide
// whether to surface it.
func readFile(path string) ([]Keybinding, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read keybindings: %w", err)
	}
	if err := chmodSensitiveFile(path); err != nil {
		return nil, fmt.Errorf("repair keybindings permissions: %w", err)
	}
	if len(data) == 0 {
		return nil, nil
	}
	var parsed []Keybinding
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("parse keybindings: %w", err)
	}
	return parsed, nil
}

// writeFile writes a pretty-printed JSON array atomically (temp file
// + sync + rename) so a crash mid-write can't corrupt the user's
// config.
func writeFile(path string, bindings []Keybinding) error {
	dir := filepath.Dir(path)
	if err := ensurePrivateDir(dir); err != nil {
		return fmt.Errorf("create keybindings dir: %w", err)
	}
	data, err := json.MarshalIndent(bindings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal keybindings: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, "keybindings-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	if err := os.Chmod(tmpPath, sensitiveFilePerm); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("repair temp file permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename temp file: %w", err)
	}
	if err := chmodSensitiveFile(path); err != nil {
		return fmt.Errorf("repair keybindings permissions: %w", err)
	}
	return nil
}

func ensurePrivateDir(path string) error {
	if err := os.MkdirAll(path, privateDirPerm); err != nil {
		return err
	}
	return os.Chmod(path, privateDirPerm)
}

func chmodSensitiveFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil
	}
	return os.Chmod(path, sensitiveFilePerm)
}

// Merge applies user overrides over defaults.
//
// User entries with DefaultID replace that shipped row and are
// emitted after defaults so runtime reverse-dispatch gives user
// overrides precedence. Entries with only DefaultKey use the legacy
// command/context/original-key identity. Older entries without
// either field replace the first matching command/context default.
func Merge(defaults, user []Keybinding) []Keybinding {
	type defaultID string
	type defaultKey struct{ command, when, originalKey string }
	type legacyKey struct{ command, when string }
	type userOrderEntry struct {
		kind       string
		defaultID  defaultID
		defaultKey defaultKey
		legacyKey  legacyKey
	}

	userByID := make(map[defaultID]Keybinding, len(user))
	userByDefault := make(map[defaultKey]Keybinding, len(user))
	userByLegacy := make(map[legacyKey]Keybinding, len(user))
	userOrder := make([]userOrderEntry, 0, len(user))

	for _, u := range user {
		if u.DefaultID != "" {
			k := defaultID(u.DefaultID)
			if _, seen := userByID[k]; !seen {
				userOrder = append(userOrder, userOrderEntry{kind: "id", defaultID: k})
			}
			userByID[k] = u
			continue
		}

		if u.DefaultKey != "" {
			k := defaultKey{command: u.Command, when: u.When, originalKey: u.DefaultKey}
			if _, seen := userByDefault[k]; !seen {
				userOrder = append(userOrder, userOrderEntry{kind: "key", defaultKey: k})
			}
			userByDefault[k] = u
			continue
		}

		k := legacyKey{command: u.Command, when: u.When}
		if _, seen := userByLegacy[k]; !seen {
			userOrder = append(userOrder, userOrderEntry{kind: "legacy", legacyKey: k})
		}
		userByLegacy[k] = u
	}

	out := make([]Keybinding, 0, len(defaults)+len(user))
	overrides := make([]Keybinding, 0, len(user))
	for _, d := range defaults {
		if d.DefaultID != "" {
			id := defaultID(d.DefaultID)
			if u, ok := userByID[id]; ok {
				overrides = append(overrides, withDefaultIdentity(u, d))
				delete(userByID, id)
				continue
			}
		}

		dk := defaultKey{command: d.Command, when: d.When, originalKey: d.Key}
		if u, ok := userByDefault[dk]; ok {
			overrides = append(overrides, withDefaultIdentity(u, d))
			delete(userByDefault, dk)
			continue
		}

		lk := legacyKey{command: d.Command, when: d.When}
		if u, ok := userByLegacy[lk]; ok {
			overrides = append(overrides, withDefaultIdentity(u, d))
			delete(userByLegacy, lk)
			continue
		}

		out = append(out, withDefaultIdentity(d, d))
	}
	out = append(out, overrides...)

	// Append remaining user entries in their original order.
	for _, entry := range userOrder {
		switch entry.kind {
		case "id":
			if u, ok := userByID[entry.defaultID]; ok {
				out = append(out, u)
				delete(userByID, entry.defaultID)
			}
		case "key":
			if u, ok := userByDefault[entry.defaultKey]; ok {
				out = append(out, u)
				delete(userByDefault, entry.defaultKey)
			}
		case "legacy":
			if u, ok := userByLegacy[entry.legacyKey]; ok {
				out = append(out, u)
				delete(userByLegacy, entry.legacyKey)
			}
		}
	}

	return out
}

func withDefaultIdentity(binding, defaultBinding Keybinding) Keybinding {
	binding.DefaultID = defaultBinding.DefaultID
	binding.DefaultKey = defaultBinding.Key
	return binding
}
