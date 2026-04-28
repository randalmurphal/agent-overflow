package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Keybinding is the on-disk / over-the-wire shape of a single keybinding.
// `When` is an optional VS Code-style context expression (e.g. "!terminalFocus"
// or "terminalOpen && !approvalPending"). `DefaultID` identifies the shipped
// default row that a user entry overrides, so multiple default chords can target
// the same command under the same context and still be rebound independently.
// `DefaultKey` is retained as a legacy fallback for configs written before
// stable IDs existed. The frontend validates key and when syntax; the backend
// persists the strings verbatim after checking required fields are present.
type Keybinding struct {
	Key        string `json:"key"`
	Command    string `json:"command"`
	When       string `json:"when,omitempty"`
	DefaultID  string `json:"defaultId,omitempty"`
	DefaultKey string `json:"defaultKey,omitempty"`
}

// DefaultKeybindings are returned when the config file is missing or malformed.
// User entries with DefaultID override that exact default row. Entries with
// only DefaultKey use the legacy (command, when, original key) identity, and
// older entries without either field fall back to the first matching
// (command, when) default.
//
// Parity note: these mirror forge's DEFAULT_KEYBINDINGS in
// /Users/randy/repos/forge/apps/server/src/keybindings.ts with the entries
// that don't apply to agent-overflow (terminal.split, script.*.run, most
// editor.* commands) removed, and with the command-palette / app-level
// actions this wave introduces layered on top.
var DefaultKeybindings = []Keybinding{
	// Wave 4 swap: ⌘K focuses the sidebar search (the most common
	// wayfinding chord), and the command palette moves to ⌘⇧K. The
	// sidebar.focus-search binding fires globally (empty `when`) so it
	// works from the composer, the diff panel, the terminal, etc.
	{Key: "mod+shift+k", Command: "palette.open", DefaultID: "palette.open"},
	{Key: "mod+k", Command: "sidebar.focus-search", DefaultID: "sidebar.focus-search"},
	// NOTE: mod+j for terminal.toggle is owned by ChatView's existing listener
	// (Wave 2C). The command is still registered and reachable from the
	// palette / user remappings, but the default binding is intentionally
	// omitted here to avoid double-firing until the ChatView handler can be
	// migrated into this keybindings system.
	{Key: "mod+n", Command: "terminal.new", When: "terminalFocus", DefaultID: "terminal.new"},
	{Key: "mod+w", Command: "terminal.close", When: "terminalFocus", DefaultID: "terminal.close"},
	{Key: "mod+n", Command: "thread.new", When: "!terminalFocus", DefaultID: "thread.new.primary"},
	{Key: "mod+shift+o", Command: "thread.new", When: "!terminalFocus", DefaultID: "thread.new.alternate"},
	{Key: "mod+shift+d", Command: "thread.new.discussion", When: "!terminalFocus", DefaultID: "thread.new.discussion"},
	{Key: "mod+shift+g", Command: "diff.panel.toggle", When: "hasActiveThread && !terminalFocus", DefaultID: "diff.panel.toggle"},
	{Key: "mod+f", Command: "search.threads", When: "!terminalFocus", DefaultID: "search.threads"},
	{Key: "mod+,", Command: "settings.open", DefaultID: "settings.open"},
	{Key: "mod+shift+[", Command: "thread.previous", DefaultID: "thread.previous"},
	{Key: "mod+shift+]", Command: "thread.next", DefaultID: "thread.next"},
	{Key: "esc", Command: "thread.interrupt", When: "hasActiveThread && turnActive && !anyModalOpen", DefaultID: "thread.interrupt"},
	{Key: "mod+1", Command: "thread.jump.1", DefaultID: "thread.jump.1"},
	{Key: "mod+2", Command: "thread.jump.2", DefaultID: "thread.jump.2"},
	{Key: "mod+3", Command: "thread.jump.3", DefaultID: "thread.jump.3"},
	{Key: "mod+4", Command: "thread.jump.4", DefaultID: "thread.jump.4"},
	{Key: "mod+5", Command: "thread.jump.5", DefaultID: "thread.jump.5"},
	{Key: "mod+6", Command: "thread.jump.6", DefaultID: "thread.jump.6"},
	{Key: "mod+7", Command: "thread.jump.7", DefaultID: "thread.jump.7"},
	{Key: "mod+8", Command: "thread.jump.8", DefaultID: "thread.jump.8"},
	{Key: "mod+9", Command: "thread.jump.9", DefaultID: "thread.jump.9"},
	// Help + global message search — both open dialogs that trap focus and
	// close with Esc, so they don't need separate `!terminalFocus` guards
	// the way thread.new does.
	{Key: "mod+/", Command: "help.keybindings", DefaultID: "help.keybindings"},
	{Key: "mod+shift+f", Command: "search.messages", DefaultID: "search.messages"},
	// mod+p opens the unified thread picker — fuzzy-jump across every
	// project's threads. Like mod+shift+f this traps focus + closes on Esc
	// so no `!terminalFocus` guard is needed.
	{Key: "mod+p", Command: "thread.search", DefaultID: "thread.search"},
	// shift+tab cycles the active thread through chat → plan → design. The
	// `when` expression keeps the chord inert while the palette or any
	// modal has focus so Shift+Tab's default "focus prev" behaviour still
	// wins inside those surfaces.
	{Key: "shift+tab", Command: "mode.cycle", When: "hasActiveThread && !paletteOpen && !anyModalOpen", DefaultID: "mode.cycle"},
}

const keybindingsFileName = "keybindings.json"
const maxKeybindingsCount = 256

var keybindingsMu sync.Mutex

// keybindingsConfigPath returns the absolute path to the keybindings JSON file
// inside the app's config dir. Falls back to ~/.agent-overflow/ when the OS
// config dir is unavailable.
func (a *App) keybindingsConfigPath() (string, error) {
	if a.configDir != "" {
		return filepath.Join(a.configDir, keybindingsFileName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	dir := filepath.Join(home, ".agent-overflow")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("cannot create keybindings directory: %w", err)
	}
	return filepath.Join(dir, keybindingsFileName), nil
}

// GetKeybindings returns the effective keybindings: defaults with any
// user overrides layered on top. Invalid entries in the user file are
// dropped silently (the frontend surfaces parse errors on its own); a
// completely malformed file falls back to defaults.
func (a *App) GetKeybindings() ([]Keybinding, error) {
	path, err := a.keybindingsConfigPath()
	if err != nil {
		return nil, err
	}
	keybindingsMu.Lock()
	defer keybindingsMu.Unlock()

	user, _ := readKeybindingsFile(path)
	return mergeKeybindings(DefaultKeybindings, user), nil
}

// UpdateKeybindings replaces the user keybindings file with the caller's
// config. The config is capped at maxKeybindingsCount entries; entries past
// the cap are dropped from the tail.
func (a *App) UpdateKeybindings(bindings []Keybinding) error {
	if len(bindings) > maxKeybindingsCount {
		bindings = bindings[:maxKeybindingsCount]
	}
	for i, b := range bindings {
		if strings.TrimSpace(b.Key) == "" {
			return fmt.Errorf("keybinding %d: key is empty", i)
		}
		if strings.TrimSpace(b.Command) == "" {
			return fmt.Errorf("keybinding %d: command is empty", i)
		}
	}

	path, err := a.keybindingsConfigPath()
	if err != nil {
		return err
	}
	keybindingsMu.Lock()
	defer keybindingsMu.Unlock()
	return writeKeybindingsFile(path, bindings)
}

// ResetKeybindings deletes the user file so GetKeybindings returns defaults.
func (a *App) ResetKeybindings() error {
	path, err := a.keybindingsConfigPath()
	if err != nil {
		return err
	}
	keybindingsMu.Lock()
	defer keybindingsMu.Unlock()

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reset keybindings: %w", err)
	}
	return nil
}

// readKeybindingsFile loads the user config. Missing file returns nil, nil.
// A malformed JSON file returns an empty slice + error so callers can decide
// whether to surface it.
func readKeybindingsFile(path string) ([]Keybinding, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read keybindings: %w", err)
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

// writeKeybindingsFile writes a pretty-printed JSON array atomically.
func writeKeybindingsFile(path string, bindings []Keybinding) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
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
	return nil
}

// mergeKeybindings applies user overrides over defaults.
//
// User entries with DefaultID replace that shipped row and are emitted after
// defaults so runtime reverse-dispatch gives user overrides precedence. Entries
// with only DefaultKey use the legacy command/context/original-key identity.
// Older entries without either field replace the first matching
// command/context default.
func mergeKeybindings(defaults, user []Keybinding) []Keybinding {
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
