package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrDeviceRevoked is returned by a write that would have given a revoked
// device a credential. A state, not a caller bug: the device row can move
// underneath a mint that already read it as live, which is precisely the
// interleaving this refusal closes.
var ErrDeviceRevoked = errors.New("store: the device is revoked")

// CreateSession writes a session row. The caller owns the id, because the
// same id is signed into the claims that travel with it and the two must
// be minted together.
//
// Refused rather than defaulted: an empty id, user, device, or binding
// class, and an expiry at or before creation. A session that is already
// expired is not a session; accepting one would put a row in the table
// that nothing could ever present.
//
// The device's liveness is part of the INSERT rather than a read before
// it, and that is the mechanism (docs/specs/remote-access.md §2). SQLite
// serializes writers, and RevokeDevice marks the device and sweeps its
// sessions inside ONE transaction, so there is no interleaving in which
// this row lands between those two statements: either it commits first and
// the sweep revokes it, or the device reads revoked here and no row is
// written. A caller-side check could only ever be a read that the
// revocation then invalidates — which is exactly how a paired browser
// outlived its revocation (incident 2026-08-31).
func (s *Store) CreateSession(session Session) error {
	if session.BindingClass == "loopback-only" {
		return fmt.Errorf("store: local sessions require EnsureLocalSession")
	}
	err := createSession(s.db, session)
	if errors.Is(err, ErrDeviceRevoked) {
		return s.diagnoseDeviceRefusal(session.DeviceID)
	}
	return err
}

func createSession(db sqlExecutor, session Session) error {
	switch {
	case strings.TrimSpace(session.ID) == "":
		return fmt.Errorf("%w: session id", ErrIdentityFieldRequired)
	case strings.TrimSpace(session.UserID) == "":
		return fmt.Errorf("%w: session user id", ErrIdentityFieldRequired)
	case strings.TrimSpace(session.DeviceID) == "":
		return fmt.Errorf("%w: session device id", ErrIdentityFieldRequired)
	case strings.TrimSpace(session.BindingClass) == "":
		return fmt.Errorf("%w: session binding class", ErrIdentityFieldRequired)
	case strings.TrimSpace(session.SigningKeyID) == "":
		return fmt.Errorf("%w: session signing key id", ErrIdentityFieldRequired)
	case session.BindingClass == "loopback-only" && (!session.ProcessBound() || session.ActivatedAt == 0):
		return fmt.Errorf("store: local session must be activated and process-bound")
	case !session.ProcessBound() && session.ExpiresAt <= session.CreatedAt:
		return fmt.Errorf("store: create session: expiry %d is not after creation %d",
			session.ExpiresAt, session.CreatedAt)
	}
	scopes, err := encodeScopes(session.Scopes)
	if err != nil {
		return err
	}
	activatedAt := sql.NullInt64{Int64: session.ActivatedAt, Valid: session.ActivatedAt != 0}
	result, err := db.Exec(
		`INSERT INTO sessions (id, user_id, device_id, binding_class, scopes,
			signing_key_id, created_at, expires_at, revoked_at, last_seen_at, activated_at)
		 SELECT ?, ?, ?, ?, ?, ?, ?, ?, NULL, 0, ?
		 WHERE EXISTS (SELECT 1 FROM devices WHERE id = ? AND revoked_at IS NULL)`,
		session.ID, session.UserID, session.DeviceID, session.BindingClass, scopes,
		session.SigningKeyID, session.CreatedAt, sql.NullInt64{Int64: session.ExpiresAt, Valid: !session.ProcessBound()}, activatedAt,
		session.DeviceID,
	)
	if err != nil {
		return fmt.Errorf("store: create session: %w", err)
	}
	written, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: create session: rows affected: %w", err)
	}
	if written == 0 {
		// Diagnosed only on the refusal path, so the ordinary insert stays
		// one statement. Both answers refuse; naming which one lets a
		// caller tell "that device is gone" from "the owner revoked it".
		return fmt.Errorf("store: create session for device %s: %w",
			session.DeviceID, ErrDeviceRevoked)
	}
	return nil
}

// diagnoseDeviceRefusal names why a device-gated write matched no device.
func (s *Store) diagnoseDeviceRefusal(deviceID string) error {
	if _, err := s.GetDevice(deviceID); errors.Is(err, sql.ErrNoRows) {
		return sql.ErrNoRows
	}
	return ErrDeviceRevoked
}

// ActivateSession stamps the moment a session became presentable, which is
// the moment the owner confirmed the pairing verification number. Reports
// whether it moved: a second confirmation keeps the first stamp, so the
// log records when access actually began.
//
// Scoped to unactivated rows that are still inside their window, whose
// session and whose DEVICE are both unrevoked. A revoked session must not
// be activatable — that would be a revocation a confirmation undoes — and
// neither must a lapsed one: the pending window IS the deadline on the
// confirmation, so accepting one after it would make the deadline
// decorative. The device clause is the same rule one row up: confirming a
// pairing whose device the owner revoked in the meantime would turn an
// inert row into a live credential, and it is in the statement rather than
// in front of it so a revocation cannot land between the check and the
// write.
func (s *Store) ActivateSession(sessionID string, at, expiresAt int64) (bool, error) {
	if at == 0 {
		return false, fmt.Errorf("%w: session activation stamp", ErrIdentityFieldRequired)
	}
	result, err := s.db.Exec(
		`UPDATE sessions SET activated_at = ?, expires_at = ?
		 WHERE id = ? AND activated_at IS NULL AND revoked_at IS NULL AND expires_at > ?
		   AND device_id IN (SELECT id FROM devices WHERE revoked_at IS NULL)`,
		at, expiresAt, sessionID, at)
	if err != nil {
		return false, fmt.Errorf("store: activate session: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: activate session: rows affected: %w", err)
	}
	return rows > 0, nil
}

// ExtendSession moves a confirmed, unrevoked session's expiry forward, reporting whether
// it moved. This is what a refresh rotation writes: the access window is
// the row's expiry, so renewing one means moving the other.
//
// Access may already have expired: renewal is authorized by the longer-lived
// refresh secret in identity. Never shorten a window, extend into the past,
// or extend an unconfirmed/revoked session or a revoked device.
func (s *Store) ExtendSession(sessionID string, expiresAt, now int64) (bool, error) {
	result, err := s.db.Exec(
		`UPDATE sessions SET expires_at = ?
		 WHERE id = ? AND revoked_at IS NULL AND activated_at IS NOT NULL
		   AND expires_at < ? AND ? > ?
		   AND device_id IN (SELECT id FROM devices WHERE revoked_at IS NULL)`,
		expiresAt, sessionID, expiresAt, expiresAt, now)
	if err != nil {
		return false, fmt.Errorf("store: extend session: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: extend session: rows affected: %w", err)
	}
	return rows > 0, nil
}

// DeleteSessionsExpiredBefore drops sessions whose window closed before
// `before`, returning how many went.
//
// The only identity rows this package deletes, and the bound is what makes
// it safe: access expiry alone does not end a renewable session. Keep rows
// with refresh secrets still inside the retention window, including spent
// secrets needed to detect reuse. The caller keeps a
// generous margin so the device list can still show recent history.
// Revoked-but-unexpired rows are deliberately NOT covered — they are the
// evidence that a revocation happened.
func (s *Store) DeleteSessionsExpiredBefore(before int64) (int64, error) {
	result, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at < ?
        AND NOT EXISTS (SELECT 1 FROM refresh_secrets r
                        WHERE r.session_id = sessions.id AND r.expires_at >= ?)`, before, before)
	if err != nil {
		return 0, fmt.Errorf("store: delete expired sessions: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: delete expired sessions: rows affected: %w", err)
	}
	return rows, nil
}

// GetSession reads one session by id. sql.ErrNoRows when it does not
// exist, which callers distinguish from a revoked or expired row: an
// unknown session and a dead one are different facts even though both
// refuse.
func (s *Store) GetSession(id string) (Session, error) {
	return scanSession(s.reader().QueryRow(sessionSelect+` WHERE s.id = ?`, id))
}

// ListSessionsForDevice returns a device's sessions, newest first,
// revoked and expired ones included.
func (s *Store) ListSessionsForDevice(deviceID string) ([]Session, error) {
	rows, err := s.reader().Query(
		sessionSelect+` WHERE s.device_id = ? ORDER BY s.created_at DESC, s.id`, deviceID)
	if err != nil {
		return nil, fmt.Errorf("store: list sessions: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		session, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, session)
	}
	return out, rows.Err()
}

// ListLiveSessions returns every session that would still admit a
// presentation at now, newest first. Used to warm the in-memory session
// table at boot and to render the device-management list.
func (s *Store) ListLiveSessions(now int64) ([]Session, error) {
	rows, err := s.reader().Query(
		sessionSelect+` WHERE s.revoked_at IS NULL AND d.revoked_at IS NULL
		 AND s.activated_at IS NOT NULL AND (s.expires_at IS NULL OR s.expires_at > ?)
		 ORDER BY s.created_at DESC, s.id`, now)
	if err != nil {
		return nil, fmt.Errorf("store: list live sessions: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		session, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, session)
	}
	return out, rows.Err()
}

// RevokeSession marks one session revoked, reporting whether it moved. A
// second revocation of the same session reports false and keeps the first
// stamp, so the log records when access actually ended.
func (s *Store) RevokeSession(sessionID string, at int64) (bool, error) {
	result, err := s.db.Exec(
		`UPDATE sessions SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, at, sessionID)
	if err != nil {
		return false, fmt.Errorf("store: revoke session: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: revoke session: rows affected: %w", err)
	}
	return rows > 0, nil
}

// TouchSession advances last_seen_at on a live session, reporting whether
// it moved. Deliberately scoped to live rows, and "live" is both of them:
// neither a revoked session nor a session whose DEVICE was revoked may
// look freshly used in the device list, however often it keeps being
// presented.
func (s *Store) TouchSession(sessionID string, at int64) (bool, error) {
	result, err := s.db.Exec(
		`UPDATE sessions SET last_seen_at = ?
		 WHERE id = ? AND revoked_at IS NULL AND last_seen_at IS NOT ?
		   AND device_id IN (SELECT id FROM devices WHERE revoked_at IS NULL)`,
		at, sessionID, at)
	if err != nil {
		return false, fmt.Errorf("store: touch session: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: touch session: rows affected: %w", err)
	}
	return rows > 0, nil
}

func scanSession(sc interface{ Scan(...any) error }) (Session, error) {
	var session Session
	var scopes string
	var revokedAt, activatedAt, deviceRevokedAt, expiresAt sql.NullInt64
	if err := sc.Scan(
		&session.ID, &session.UserID, &session.DeviceID, &session.BindingClass, &scopes,
		&session.SigningKeyID, &session.CreatedAt, &expiresAt, &revokedAt,
		&session.LastSeenAt, &activatedAt, &deviceRevokedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Session{}, err
		}
		return Session{}, fmt.Errorf("store: scan session: %w", err)
	}
	session.ExpiresAt = expiresAt.Int64
	session.RevokedAt = revokedAt.Int64
	session.DeviceRevokedAt = deviceRevokedAt.Int64
	session.ActivatedAt = activatedAt.Int64
	decoded, err := decodeScopes(scopes)
	if err != nil {
		return Session{}, err
	}
	session.Scopes = decoded
	return session, nil
}
