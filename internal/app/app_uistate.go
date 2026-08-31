package app

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

// UI-state bindings: the wire surface behind the frontend appStorage
// module. Each client (embedded webview, --connect shell, LAN browser)
// presents an opaque client ID; its bucket lives in the ui_state table
// under scope "client:<id>", built server-side so a caller can only
// ever touch the client namespace ("user:<id>" stays reserved for when
// identities exist).
//
// Not LocalOnlyMethods entries: per-client UI view state is the whole
// point of this table — remote clients (--connect, LAN browsers) need
// their own buckets. Same reasoning as GetKeybindings: UI preferences,
// not credentials or filesystem access. The ui_state rows are opaque
// strings written and read only by the presenting client.

// Wire-input bounds. Generous for real UI state (pane layout JSON is
// the largest value today at well under 4 KB) while keeping a buggy or
// hostile client from growing the table without limit.
const (
	maxUIStateBatch    = 128
	maxUIStateKeyLen   = 128
	maxUIStateValueLen = 32 * 1024
)

// validClientID accepts the IDs both sides mint (Go uuid.NewString,
// frontend crypto.randomUUID) with room for future formats: 8..64
// chars of [A-Za-z0-9-]. Anything else is rejected before it can name
// a scope.
func validClientID(id string) bool {
	if len(id) < 8 || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return false
		}
	}
	return true
}

// EnsureClientIDIn loads or creates the durable client identity shared by the
// pre-Start bootstrap manifest and the post-Start UI-state migration.
func EnsureClientIDIn(dir string) string {
	if dir == "" {
		return ""
	}
	path := filepath.Join(dir, "client-id.json")
	var state struct {
		ClientID string `json:"clientId"`
	}
	found, err := atomicfile.ReadJSON(path, &state)
	if err != nil {
		log.Printf("client-id: read %s: %v", path, err)
	}
	if found && validClientID(state.ClientID) {
		return state.ClientID
	}
	state.ClientID = uuid.NewString()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("client-id: mkdir %s: %v", dir, err)
		return ""
	}
	if err := atomicfile.WriteJSON(path, state); err != nil {
		log.Printf("client-id: write %s: %v", path, err)
		return ""
	}
	return state.ClientID
}

func uiStateScope(clientID string) (string, error) {
	if !validClientID(clientID) {
		return "", fmt.Errorf("ui state: invalid client id")
	}
	return "client:" + clientID, nil
}

// GetUIState returns the calling client's full persisted UI-state
// bucket. A fresh client gets an empty map — defaults, not an error.
func (a *App) GetUIState(clientID string) (map[string]string, error) {
	if a.store == nil {
		return nil, fmt.Errorf("ui state: store unavailable")
	}
	scope, err := uiStateScope(clientID)
	if err != nil {
		return nil, err
	}
	return a.store.GetUIState(scope)
}

// SetUIState batch-upserts entries into the calling client's bucket.
func (a *App) SetUIState(clientID string, entries map[string]string) error {
	if a.store == nil {
		return fmt.Errorf("ui state: store unavailable")
	}
	scope, err := uiStateScope(clientID)
	if err != nil {
		return err
	}
	if len(entries) > maxUIStateBatch {
		return fmt.Errorf("ui state: batch of %d entries exceeds limit %d", len(entries), maxUIStateBatch)
	}
	for key, value := range entries {
		if len(key) == 0 || len(key) > maxUIStateKeyLen {
			return fmt.Errorf("ui state: key length %d outside 1..%d", len(key), maxUIStateKeyLen)
		}
		if len(value) > maxUIStateValueLen {
			return fmt.Errorf("ui state: value for %q is %d bytes, limit %d", key, len(value), maxUIStateValueLen)
		}
	}
	return a.store.SetUIState(scope, entries)
}

// migrateUIStateFromSettings performs the one-shot move of the UI view
// state that used to persist in settings.json — paneLayout and
// collapsedProjects, fields the Settings struct no longer declares —
// into this installation's ui_state bucket (the embedded client's
// identity from client-id.json). It reads the raw file because the
// typed loader silently drops unknown keys. Idempotent: a key already
// present in the bucket is never overwritten, and the stale JSON keys
// vanish from settings.json on its next sparse save. Failures log and
// continue — worst case the user re-collapses a project or re-arranges
// panes once; blocking startup over view state would be backwards.
func migrateUIStateFromSettings(configDir string, st *store.Store) {
	clientID := EnsureClientIDIn(configDir)
	if clientID == "" || st == nil {
		return
	}
	raw, err := os.ReadFile(filepath.Join(configDir, "settings.json"))
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("ui state migration: read settings.json: %v", err)
		}
		return
	}
	var legacy map[string]json.RawMessage
	if err := json.Unmarshal(raw, &legacy); err != nil {
		log.Printf("ui state migration: parse settings.json: %v", err)
		return
	}

	scope := "client:" + clientID
	existing, err := st.GetUIState(scope)
	if err != nil {
		log.Printf("ui state migration: read bucket: %v", err)
		return
	}

	// settings.json key → ui_state key. Values move as their raw JSON;
	// the frontend readers validate shape on load, same as they did
	// against the settings wire value.
	moves := map[string]string{
		"paneLayout":        "paneLayout",
		"collapsedProjects": "sidebar:collapsedProjects",
	}
	entries := make(map[string]string, len(moves))
	for settingsKey, uiStateKey := range moves {
		value, ok := legacy[settingsKey]
		if !ok || string(value) == "null" {
			continue
		}
		if _, taken := existing[uiStateKey]; taken {
			continue
		}
		entries[uiStateKey] = string(value)
	}
	if len(entries) == 0 {
		return
	}
	if err := st.SetUIState(scope, entries); err != nil {
		log.Printf("ui state migration: write bucket: %v", err)
		return
	}
	log.Printf("ui state migration: moved %d settings.json key(s) into %s", len(entries), scope)
}

// DeleteUIState removes keys from the calling client's bucket.
// Missing keys are a no-op.
func (a *App) DeleteUIState(clientID string, keys []string) error {
	if a.store == nil {
		return fmt.Errorf("ui state: store unavailable")
	}
	scope, err := uiStateScope(clientID)
	if err != nil {
		return err
	}
	if len(keys) > maxUIStateBatch {
		return fmt.Errorf("ui state: batch of %d keys exceeds limit %d", len(keys), maxUIStateBatch)
	}
	return a.store.DeleteUIState(scope, keys)
}
