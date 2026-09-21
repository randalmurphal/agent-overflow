package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
)

// EnsureLocalSession resolves the newest local session, including revoked rows.
// The writer transaction serializes concurrent initialization. Revocation never
// becomes an absent row that could authorize minting a replacement.
func (s *Store) EnsureLocalSession(candidate Session) (Session, error) {
	if !candidate.ProcessBound() || candidate.ActivatedAt == 0 {
		return Session{}, fmt.Errorf("store: local session must be activated and process-bound")
	}
	scopes, err := encodeScopes(candidate.Scopes)
	if err != nil {
		return Session{}, err
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return Session{}, fmt.Errorf("store: begin local session: %w", err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			log.Printf("store: rollback local session: %v", err)
		}
	}()
	var channel string
	if err := tx.QueryRow(`SELECT channel FROM devices WHERE id=? AND user_id=?`, candidate.DeviceID, candidate.UserID).Scan(&channel); err != nil {
		return Session{}, fmt.Errorf("store: local device: %w", err)
	}
	if channel != "local" {
		return Session{}, fmt.Errorf("store: local session requires the local channel device")
	}
	row, err := scanSession(tx.QueryRow(sessionSelect+` WHERE s.device_id=? AND s.binding_class='loopback-only' ORDER BY s.created_at DESC,s.id DESC LIMIT 1`, candidate.DeviceID))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if err := createSession(tx, candidate); err != nil {
			return Session{}, err
		}
		row = candidate
	case err != nil:
		return Session{}, err
	}
	// The local channel always receives the current host policy, including
	// scopes introduced by an upgrade. Keep its attribution row unchanged.
	if _, err := tx.Exec(`UPDATE sessions SET scopes=? WHERE id=? AND scopes<>? AND revoked_at IS NULL
		AND device_id IN (SELECT id FROM devices WHERE revoked_at IS NULL)`, scopes, row.ID, scopes); err != nil {
		return Session{}, fmt.Errorf("store: update local session scopes: %w", err)
	}
	row, err = scanSession(tx.QueryRow(sessionSelect+` WHERE s.id=?`, row.ID))
	if err != nil {
		return Session{}, err
	}
	if err := tx.Commit(); err != nil {
		return Session{}, fmt.Errorf("store: commit local session: %w", err)
	}
	return row, nil
}
