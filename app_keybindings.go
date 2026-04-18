package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Keybinding is the on-disk / over-the-wire shape of a single user keybinding.
// `When` is an optional VS Code-style context expression (e.g. "!terminalFocus"
// or "terminalOpen && !approvalPending"). It is validated at parse-time on the
// frontend; the backend persists the string verbatim.
type Keybinding struct {
	Key     string `json:"key"`
	Command string `json:"command"`
	When    string `json:"when,omitempty"`
}

// DefaultKeybindings are seeded onto disk when the config file is missing or
// malformed. User entries override defaults by (key, when) tuple first, then
// by command as a fallback.
//
// Parity note: these mirror forge's DEFAULT_KEYBINDINGS in
// /Users/randy/repos/forge/apps/server/src/keybindings.ts with the entries
// that don't apply to agent-overflow (terminal.split, script.*.run, most
// editor.* commands) removed, and with the command-palette / app-level
// actions this wave introduces layered on top.
var DefaultKeybindings = []Keybinding{
	{Key: "mod+k", Command: "palette.open"},
	// NOTE: mod+j for terminal.toggle is owned by ChatView's existing listener
	// (Wave 2C). The command is still registered and reachable from the
	// palette / user remappings, but the default binding is intentionally
	// omitted here to avoid double-firing until the ChatView handler can be
	// migrated into this keybindings system.
	{Key: "mod+n", Command: "terminal.new", When: "terminalFocus"},
	{Key: "mod+w", Command: "terminal.close", When: "terminalFocus"},
	{Key: "mod+n", Command: "thread.new", When: "!terminalFocus"},
	{Key: "mod+shift+o", Command: "thread.new", When: "!terminalFocus"},
	{Key: "mod+shift+d", Command: "thread.new.discussion", When: "!terminalFocus"},
	{Key: "mod+shift+g", Command: "diff.panel.toggle", When: "hasActiveThread && !terminalFocus"},
	{Key: "mod+f", Command: "search.threads", When: "!terminalFocus"},
	{Key: "mod+,", Command: "settings.open"},
	{Key: "mod+shift+[", Command: "thread.previous"},
	{Key: "mod+shift+]", Command: "thread.next"},
	{Key: "mod+1", Command: "thread.jump.1"},
	{Key: "mod+2", Command: "thread.jump.2"},
	{Key: "mod+3", Command: "thread.jump.3"},
	{Key: "mod+4", Command: "thread.jump.4"},
	{Key: "mod+5", Command: "thread.jump.5"},
	{Key: "mod+6", Command: "thread.jump.6"},
	{Key: "mod+7", Command: "thread.jump.7"},
	{Key: "mod+8", Command: "thread.jump.8"},
	{Key: "mod+9", Command: "thread.jump.9"},
	// Help + global message search — both open dialogs that trap focus and
	// close with Esc, so they don't need separate `!terminalFocus` guards
	// the way thread.new does.
	{Key: "mod+/", Command: "help.keybindings"},
	{Key: "mod+shift+f", Command: "search.messages"},
	// mod+p opens the unified thread picker — fuzzy-jump across every
	// project's threads. Like mod+shift+f this traps focus + closes on Esc
	// so no `!terminalFocus` guard is needed.
	{Key: "mod+p", Command: "thread.search"},
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
// A user entry overrides a default when it shares the same (command, when)
// tuple; that default is removed and replaced by the user entry. All other
// defaults remain. User entries with unique (command, when) tuples are
// appended to the end so they take precedence when the runtime walks matches
// in reverse. The merged list is sorted by command for deterministic output.
func mergeKeybindings(defaults, user []Keybinding) []Keybinding {
	type key struct{ command, when string }
	userByKey := make(map[key]Keybinding, len(user))
	userOrder := make([]key, 0, len(user))
	for _, u := range user {
		k := key{command: u.Command, when: u.When}
		if _, seen := userByKey[k]; !seen {
			userOrder = append(userOrder, k)
		}
		userByKey[k] = u
	}

	out := make([]Keybinding, 0, len(defaults)+len(user))
	for _, d := range defaults {
		k := key{command: d.Command, when: d.When}
		if u, ok := userByKey[k]; ok {
			out = append(out, u)
			delete(userByKey, k)
			continue
		}
		out = append(out, d)
	}
	// Append remaining user entries in their original order.
	for _, k := range userOrder {
		if u, ok := userByKey[k]; ok {
			out = append(out, u)
			delete(userByKey, k)
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Command != out[j].Command {
			return out[i].Command < out[j].Command
		}
		return out[i].When < out[j].When
	})
	return out
}
