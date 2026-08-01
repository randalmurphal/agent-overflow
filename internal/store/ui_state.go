package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ui_state is the persisted per-client UI view state table (migration
// v15). Scope is an opaque namespace string owned by the caller —
// "client:<uuid>" today, "user:<id>" reserved for when identities
// exist — and values are opaque strings (the frontend JSON-encodes
// structured values). This is the durable copy behind the frontend's
// appStorage module; it exists because webview localStorage resets
// every launch (ephemeral transport port = new origin). Transient UI
// state still belongs to frontend $state, not here.

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

var _ = sql.ErrNoRows // keep database/sql imported alongside future single-row reads
