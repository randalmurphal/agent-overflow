package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/identity"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"

	"github.com/google/uuid"
)

// UI-state bindings: the wire surface behind the frontend appStorage
// module. A bucket lives in the ui_state table under a scope this
// package builds from the CONNECTION — never from a parameter, which is
// the identity hole docs/specs/remote-access.md §6 opens by naming:
// a caller-supplied client id is a bearer string, and any client could
// spell another's.
//
// Two scope shapes, resolved by uiStateScope below: "device:<id>" for a
// paired device, "client:<id>" for a screen on this backend's own local
// page channel. "user:<id>" stays reserved for the user tier (§6).
//
// Not LocalOnlyMethods entries: per-device UI view state is the whole
// point of this table — remote clients (--connect, LAN browsers) need
// their own buckets. Same reasoning as GetKeybindings: UI preferences,
// not credentials or filesystem access. The ui_state rows are opaque
// strings written and read only by the connection they belong to.

// Wire-input bounds. Generous for real UI state (pane layout JSON is
// the largest value today at well under 4 KB) while keeping a buggy
// client from growing the table without limit.
const (
	maxUIStateBatch    = 128
	maxUIStateKeyLen   = 128
	maxUIStateValueLen = 32 * 1024
)

// validClientID bounds every id that reaches a scope string — the client
// id a screen declares on its upgrade URL and the device id a session
// resolves to alike. It accepts the shapes all three sides mint (Go
// uuid.NewString, frontend crypto.randomUUID) with room for future
// formats: 8..64 chars of [A-Za-z0-9-].
//
// The bound is what keeps a colon, a path separator, or an empty string
// out of the scope namespace, so it stays in front of scope building even
// now that the ids are backend-resolved rather than caller-supplied: an
// id that could carry a colon could name somebody else's namespace.
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

// uiStateScope resolves the bucket this call may touch, from the
// connection and nothing else (docs/specs/remote-access.md §6, "Fix the
// identity hole").
//
// Three outcomes:
//
//  1. The connection presented a session whose device is a PAIRED one →
//     "device:<deviceID>". The screen's declared client id is ignored:
//     a paired device is the unit the spec scopes device-tier state to,
//     and its state is dropped when the device is revoked.
//  2. The connection presented a session on the LOCAL page channel →
//     "client:<declared client id>", exactly today's buckets. See the
//     comment on that branch; the carve-out is deliberate.
//  3. No session at all → the same client scope, which keeps every
//     launch-credential client (the harness CLI, the e2e rig, a
//     `--connect` stub predating the forwarded credential) working
//     unchanged. A connection with neither a session nor a declared
//     client id gets an error: there is no anonymous bucket, because one
//     would be a bucket every anonymous connection shares.
//
// A session the core refuses is an error rather than a fall-through to
// the client scope. Falling through would hand a revoked device a
// working bucket by dropping the credential it presented, which is the
// opposite of what revoking it meant.
func (a *App) uiStateScope(ctx context.Context) (string, error) {
	if sessionID := transport.SessionFromContext(ctx); sessionID != "" {
		state := a.identityState()
		if state == nil {
			return "", fmt.Errorf("ui state: this connection names a session and identity is not wired")
		}
		session, reason := state.sessions.Live(sessionID)
		if reason.Refused() {
			return "", fmt.Errorf("ui state: session refused (%s)", reason.Code())
		}
		device, err := a.store.GetDevice(session.DeviceID)
		if err != nil {
			return "", fmt.Errorf("ui state: read the session's device: %w", err)
		}
		if device.Channel != identity.LocalChannel {
			if !validClientID(device.ID) {
				return "", fmt.Errorf("ui state: device id is not a usable scope")
			}
			return "device:" + device.ID, nil
		}
		// The local page channel names the BACKEND's own channel, not one
		// screen. The embedded webview, the WSL relay and the `--connect`
		// stub all present the single session this backend minted for
		// itself, so scoping by it would collapse several distinct screens
		// — two desktops, a relayed window, a stub — into one bucket and
		// make them fight over pane layout and window geometry.
		//
		// So per-screen buckets stay keyed on the durable client id until
		// native clients pair as their own devices (phase 5). That id is
		// declared by the peer, which is exactly as strong as it was
		// before this change and no weaker: the only connections reaching
		// this branch are ones already holding this backend's own
		// loopback-only credential.
	}
	client := transport.ClientFromContext(ctx).DeviceID
	if !validClientID(client) {
		return "", fmt.Errorf("ui state: this connection names neither a session nor a client")
	}
	return "client:" + client, nil
}

// GetUIState returns the calling connection's full persisted UI-state
// bucket. A fresh client gets an empty map — defaults, not an error.
//
//ao:scope settings:read
func (a *App) GetUIState(ctx context.Context) (map[string]string, error) {
	if a.store == nil {
		return nil, fmt.Errorf("ui state: store unavailable")
	}
	scope, err := a.uiStateScope(ctx)
	if err != nil {
		return nil, err
	}
	return a.store.GetUIState(scope)
}

// SetUIState batch-upserts entries into the calling connection's bucket.
//
//ao:scope settings:write
func (a *App) SetUIState(ctx context.Context, entries map[string]string) error {
	if a.store == nil {
		return fmt.Errorf("ui state: store unavailable")
	}
	scope, err := a.uiStateScope(ctx)
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

// DeleteUIState removes keys from the calling connection's bucket.
// Missing keys are a no-op.
//
//ao:scope settings:write
func (a *App) DeleteUIState(ctx context.Context, keys []string) error {
	if a.store == nil {
		return fmt.Errorf("ui state: store unavailable")
	}
	scope, err := a.uiStateScope(ctx)
	if err != nil {
		return err
	}
	if len(keys) > maxUIStateBatch {
		return fmt.Errorf("ui state: batch of %d keys exceeds limit %d", len(keys), maxUIStateBatch)
	}
	return a.store.DeleteUIState(scope, keys)
}
