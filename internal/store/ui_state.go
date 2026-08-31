package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ui_state is the persisted per-device UI view state table (migration
// v15). Scope is a namespace string owned by the caller —
// "device:<id>" for a paired device, "client:<uuid>" for a screen on the
// local page channel, "user:<id>" reserved for the user tier — and
// values are opaque strings (the frontend JSON-encodes structured
// values). This is the durable copy behind the frontend's appStorage
// module; it exists because webview localStorage resets every launch
// (ephemeral transport port = new origin). Transient UI state still
// belongs to frontend $state, not here.
//
// This package has no opinion about which scope a caller may name. The
// derivation — connection to scope, and the refusal when a connection
// resolves to none — lives in internal/app, which is where the session
// core is reachable.

var ErrEmptyUIStateScope = errors.New("store: ui state scope is empty")

func normalizeUIStateScope(scope string) (string, error) {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return "", ErrEmptyUIStateScope
	}
	return scope, nil
}

// GetUIState returns every key/value pair in the scope. A scope with
// no rows returns an empty, non-nil map — a fresh client simply has
// no persisted state yet, which is not an error.
func (s *Store) GetUIState(scope string) (map[string]string, error) {
	scope, err := normalizeUIStateScope(scope)
	if err != nil {
		return nil, err
	}
	rows, err := s.reader().Query(
		`SELECT key, value FROM ui_state WHERE scope = ?`, scope,
	)
	if err != nil {
		return nil, fmt.Errorf("store: get ui state for %q: %w", scope, err)
	}
	defer rows.Close()

	out := make(map[string]string, 16)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, fmt.Errorf("store: get ui state for %q: scan: %w", scope, err)
		}
		out[key] = value
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: get ui state for %q: iterate: %w", scope, err)
	}
	return out, nil
}

// SetUIState upserts every entry in one transaction. Empty keys are
// rejected; an empty entries map is a no-op.
func (s *Store) SetUIState(scope string, entries map[string]string) error {
	scope, err := normalizeUIStateScope(scope)
	if err != nil {
		return err
	}
	for key := range entries {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("store: set ui state for %q: empty key", scope)
		}
	}
	if len(entries) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: set ui state for %q: begin: %w", scope, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	stmt, err := tx.Prepare(
		`INSERT INTO ui_state (scope, key, value, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(scope, key) DO UPDATE SET
		 	value = excluded.value,
		 	updated_at = excluded.updated_at`,
	)
	if err != nil {
		return fmt.Errorf("store: set ui state for %q: prepare: %w", scope, err)
	}
	defer stmt.Close()

	now := nowMillis()
	for key, value := range entries {
		if _, err := stmt.Exec(scope, key, value, now); err != nil {
			return fmt.Errorf("store: set ui state for %q key %q: %w", scope, key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: set ui state for %q: commit: %w", scope, err)
	}
	committed = true
	return nil
}

// DeleteUIState removes the given keys from the scope. Missing keys
// are a no-op — delete is idempotent.
func (s *Store) DeleteUIState(scope string, keys []string) error {
	scope, err := normalizeUIStateScope(scope)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: delete ui state for %q: begin: %w", scope, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	stmt, err := tx.Prepare(`DELETE FROM ui_state WHERE scope = ? AND key = ?`)
	if err != nil {
		return fmt.Errorf("store: delete ui state for %q: prepare: %w", scope, err)
	}
	defer stmt.Close()

	for _, key := range keys {
		if _, err := stmt.Exec(scope, key); err != nil {
			return fmt.Errorf("store: delete ui state for %q key %q: %w", scope, key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: delete ui state for %q: commit: %w", scope, err)
	}
	committed = true
	return nil
}

// DeleteUIStateScope removes a whole bucket, returning how many rows
// went. One statement, because the scope is the unit: revoking a device
// drops its state (docs/specs/remote-access.md §6), and a caller that had
// to enumerate the keys first could drop a partial bucket if a write
// landed between the read and the delete.
//
// A scope with no rows answers 0, which is a normal answer: a device that
// never persisted anything is as fully cleared as one that did.
func (s *Store) DeleteUIStateScope(scope string) (int64, error) {
	scope, err := normalizeUIStateScope(scope)
	if err != nil {
		return 0, err
	}
	result, err := s.db.Exec(`DELETE FROM ui_state WHERE scope = ?`, scope)
	if err != nil {
		return 0, fmt.Errorf("store: delete ui state scope %q: %w", scope, err)
	}
	dropped, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: delete ui state scope %q: rows affected: %w", scope, err)
	}
	return dropped, nil
}

// ClearUIState removes every scope's persisted UI view state. The one
// production-shaped caller is HarnessReset: view state names entity ids
// (the workflows overlay stack persists work-item ids), so state
// surviving a reset makes the next test's fresh page restore a
// selection onto rows the reset deleted.
func (s *Store) ClearUIState() error {
	if _, err := s.db.Exec(`DELETE FROM ui_state`); err != nil {
		return fmt.Errorf("store: clear ui state: %w", err)
	}
	return nil
}

var _ = sql.ErrNoRows // keep database/sql imported alongside future single-row reads
