package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Phone-push persistence: which devices can be woken, and what this backend
// sends with (docs/specs/remote-access.md §9, "Push"; migration v80).

// PushToken is one device's registration, joined to the two facts the
// fan-out needs about the device itself.
//
// Class rides along because the fan-out reads that device's OWN notification
// preferences, and a device-tier read needs the bucket AND the class of
// screen behind it (internal/settings/classdefaults.go: a paired phone read
// with the desktop class would silently answer desktop defaults). Two halves
// of one answer, so they come out of one query.
type PushToken struct {
	DeviceID    string
	DeviceClass string
	Platform    string
	Token       string
	UpdatedAt   int64
}

// PushSenderCredential is the singleton service-account key this backend
// sends with. CredentialJSON is backend-local secret material of the same
// class as `signing_keys.secret`: it is written here, read by the sender,
// and never travels on a wire shape a client reads.
type PushSenderCredential struct {
	ProjectID      string
	ClientEmail    string
	CredentialJSON string
	UpdatedAt      int64
}

// UpsertPushToken records the registration for one device, replacing
// whatever that device last registered.
//
// One row per DEVICE, not per token: the platform replaces a device's
// registration rather than issuing a second, and a table that accumulated
// rows would send one phone the same notification once per stale row.
func (s *Store) UpsertPushToken(deviceID, platform, token string, at int64) error {
	if strings.TrimSpace(deviceID) == "" {
		return fmt.Errorf("%w: push token device id", ErrIdentityFieldRequired)
	}
	if strings.TrimSpace(platform) == "" {
		return fmt.Errorf("%w: push token platform", ErrIdentityFieldRequired)
	}
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("%w: push token", ErrIdentityFieldRequired)
	}
	if _, err := s.db.Exec(
		`INSERT INTO push_tokens (device_id, platform, token, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(device_id) DO UPDATE SET
		     platform = excluded.platform,
		     token = excluded.token,
		     updated_at = excluded.updated_at`,
		deviceID, platform, token, at,
	); err != nil {
		return fmt.Errorf("store: upsert push token: %w", err)
	}
	return nil
}

// DeletePushToken forgets one device's registration. Reports whether a row
// was there, so a caller can tell "unregistered" from "was not registered".
func (s *Store) DeletePushToken(deviceID string) (bool, error) {
	result, err := s.db.Exec(`DELETE FROM push_tokens WHERE device_id = ?`, deviceID)
	if err != nil {
		return false, fmt.Errorf("store: delete push token: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: delete push token: %w", err)
	}
	return affected > 0, nil
}

// LivePushTokens returns every registration whose device is not revoked.
//
// THE ONE QUERY THE FAN-OUT USES, and the join is the point rather than a
// convenience. "Which phones may be woken" and "which devices are still
// allowed in" are one question: reading the tokens and filtering revoked
// devices in Go would be two reads that can disagree, and the way they
// disagree is that a revoked phone keeps receiving notifications from a
// backend the owner believes it has been cut off from. The cascade on
// `devices` covers deletion; this covers revocation, which leaves the row.
//
// Ordered by device id so a fan-out is deterministic and a test can assert
// on the sequence.
func (s *Store) LivePushTokens() ([]PushToken, error) {
	rows, err := s.reader().Query(
		`SELECT t.device_id, d.class, t.platform, t.token, t.updated_at
		 FROM push_tokens t
		 JOIN devices d ON d.id = t.device_id
		 WHERE d.revoked_at IS NULL
		 ORDER BY t.device_id`)
	if err != nil {
		return nil, fmt.Errorf("store: list push tokens: %w", err)
	}
	defer rows.Close()
	var out []PushToken
	for rows.Next() {
		var token PushToken
		if err := rows.Scan(
			&token.DeviceID, &token.DeviceClass, &token.Platform, &token.Token, &token.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("store: scan push token: %w", err)
		}
		out = append(out, token)
	}
	return out, rows.Err()
}

// SetPushSender writes the singleton sender credential, replacing any
// previous one. The row id is pinned to 1 by the schema, so this can only
// ever be an upsert of the same row.
func (s *Store) SetPushSender(cred PushSenderCredential) error {
	if strings.TrimSpace(cred.ProjectID) == "" {
		return fmt.Errorf("%w: push sender project id", ErrIdentityFieldRequired)
	}
	if strings.TrimSpace(cred.CredentialJSON) == "" {
		return fmt.Errorf("%w: push sender credential", ErrIdentityFieldRequired)
	}
	if _, err := s.db.Exec(
		`INSERT INTO push_sender (id, project_id, client_email, credential_json, updated_at)
		 VALUES (1, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		     project_id = excluded.project_id,
		     client_email = excluded.client_email,
		     credential_json = excluded.credential_json,
		     updated_at = excluded.updated_at`,
		cred.ProjectID, cred.ClientEmail, cred.CredentialJSON, cred.UpdatedAt,
	); err != nil {
		return fmt.Errorf("store: set push sender: %w", err)
	}
	return nil
}

// ClearPushSender drops the credential. Reports whether one was there.
func (s *Store) ClearPushSender() (bool, error) {
	result, err := s.db.Exec(`DELETE FROM push_sender WHERE id = 1`)
	if err != nil {
		return false, fmt.Errorf("store: clear push sender: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: clear push sender: %w", err)
	}
	return affected > 0, nil
}

// GetPushSender reads the credential. sql.ErrNoRows means this backend
// sends nothing — which is the resting state of every backend but the
// owner's own (§18 item 1, ruled owner-only for this wave).
func (s *Store) GetPushSender() (PushSenderCredential, error) {
	var cred PushSenderCredential
	err := s.reader().QueryRow(
		`SELECT project_id, client_email, credential_json, updated_at FROM push_sender WHERE id = 1`,
	).Scan(&cred.ProjectID, &cred.ClientEmail, &cred.CredentialJSON, &cred.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PushSenderCredential{}, err
		}
		return PushSenderCredential{}, fmt.Errorf("store: read push sender: %w", err)
	}
	return cred, nil
}
