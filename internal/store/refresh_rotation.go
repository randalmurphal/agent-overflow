package store

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

var (
	ErrRefreshReuse      = errors.New("store: refresh secret reused for a different renewal")
	ErrRefreshSuperseded = errors.New("store: refresh renewal already superseded")
)

// RefreshRotation carries digests only. Recoverable means the client chose and
// persisted NextHash's secret before posting; legacy server-chosen successors
// cannot prove that a repeated predecessor is the same operation.
type RefreshRotation struct {
	SessionID, DeviceID            string
	OldHash, NextHash              []byte
	Now, AccessUntil, RefreshUntil int64
	Recoverable                    bool
}

type RefreshRotationResult struct {
	Session  Session
	Secret   RefreshSecret
	Replayed bool
}

// RotateRefreshSecret commits the entire generation change durably. A crash,
// failed insert or revocation cannot leave a spent predecessor without its
// successor, nor let an acknowledgment rewind after a power loss.
func (s *Store) RotateRefreshSecret(ctx context.Context, in RefreshRotation) (RefreshRotationResult, error) {
	var out RefreshRotationResult
	if in.SessionID == "" || in.DeviceID == "" || len(in.OldHash) != 32 || len(in.NextHash) != 32 ||
		in.Now <= 0 || in.AccessUntil <= in.Now || in.RefreshUntil <= in.Now ||
		subtle.ConstantTimeCompare(in.OldHash, in.NextHash) == 1 {
		return out, errors.New("store: invalid refresh rotation")
	}
	tx, release, err := s.beginDurableTx(ctx)
	if err != nil {
		return out, err
	}
	defer release()
	defer tx.Rollback()
	// The proof was checked by identity. Recheck its session/device inside
	// this same writer transaction so revocation cannot race the extension.
	session, err := scanSession(tx.QueryRowContext(ctx, sessionSelect+`
        WHERE s.id = ? AND s.device_id = ? AND s.revoked_at IS NULL
          AND s.activated_at IS NOT NULL
          AND s.device_id IN (SELECT id FROM devices WHERE revoked_at IS NULL)`, in.SessionID, in.DeviceID))
	if err != nil {
		return out, err
	}
	old, err := scanRefreshSecret(tx.QueryRowContext(ctx, `SELECT `+refreshSecretColumns+` FROM refresh_secrets WHERE secret_hash = ?`, in.OldHash))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	missing := errors.Is(err, sql.ErrNoRows)
	if !missing && old.SessionID != session.ID {
		return out, sql.ErrNoRows
	}
	if missing || old.Spent() || old.ExpiresAt <= in.Now {
		if !in.Recoverable {
			if !missing && old.Spent() {
				return out, ErrRefreshReuse
			}
			return out, sql.ErrNoRows
		}
		if !missing && old.Spent() && subtle.ConstantTimeCompare(old.NextSecretHash, in.NextHash) != 1 {
			return out, ErrRefreshReuse
		}
		next, err := scanRefreshSecret(tx.QueryRowContext(ctx, `SELECT `+refreshSecretColumns+` FROM refresh_secrets WHERE secret_hash = ?`, in.NextHash))
		if err != nil {
			return out, err
		}
		if next.SessionID != session.ID {
			return out, sql.ErrNoRows
		}
		if next.Spent() {
			return out, ErrRefreshSuperseded
		}
		if next.ExpiresAt <= in.Now {
			return out, sql.ErrNoRows
		}
		out.Secret, out.Replayed = next, true
	} else {
		var recorded []byte
		if in.Recoverable {
			recorded = in.NextHash
		}
		if _, err := tx.ExecContext(ctx, `UPDATE refresh_secrets SET consumed_at = ?, consumed_by = ?, next_secret_hash = ? WHERE id = ?`, in.Now, in.DeviceID, recorded, old.ID); err != nil {
			return out, err
		}
		out.Secret = RefreshSecret{ID: uuid.NewString(), SessionID: session.ID, CreatedAt: in.Now, ExpiresAt: in.RefreshUntil}
		if _, err := tx.ExecContext(ctx, `INSERT INTO refresh_secrets
            (id, session_id, secret_hash, created_at, expires_at, consumed_at, consumed_by)
            VALUES (?, ?, ?, ?, ?, NULL, '')`, out.Secret.ID, session.ID, in.NextHash, in.Now, in.RefreshUntil); err != nil {
			return out, fmt.Errorf("store: create refresh successor: %w", err)
		}
	}
	session.ExpiresAt = max(session.ExpiresAt, in.AccessUntil)
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET expires_at = ? WHERE id = ?`, session.ExpiresAt, session.ID); err != nil {
		return out, err
	}
	if err := tx.Commit(); err != nil {
		return out, err
	}
	out.Session = session
	return out, nil
}
