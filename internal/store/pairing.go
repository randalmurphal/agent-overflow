package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Pairing links and refresh secrets (migration v76). Persistence only, on
// the same terms as identity.go: this package owns the rows, the atomic
// single-use statements, and nothing about what a pairing means.
//
// Both tables store a HASH of the value a client presents, never the value
// — the backend only ever needs to recognise a presentation, and a table
// that cannot reproduce a credential is one a database read cannot leak.
// Callers pass the digest; this package has no opinion about which one, and
// deliberately no helper that would let a caller hash the wrong form.

// PairingLink is one single-use device-admission ticket.
//
// Its life is a straight line and every step is a stamp: created →
// redeemed (a device presented the token and its key thumbprint) →
// confirmed (the owner matched the verification number) or canceled. A row
// that reaches neither of the last two just expires; the window is minutes
// (docs/specs/remote-access.md §4).
type PairingLink struct {
	ID     string `json:"id"`
	UserID string `json:"userId"`
	// Scopes is the subset this link grants — a viewer link, a peer
	// invitation, or the full set an owner device gets.
	Scopes []string `json:"scopes"`
	// BindingClass and DeviceClass are decided by the MINTING surface, not
	// by the redeeming device. A device that could name its own class
	// could name `loopback-only` and inherit the local channel's posture.
	BindingClass string `json:"bindingClass"`
	DeviceClass  string `json:"deviceClass"`
	// CertFingerprint is the backend TLS certificate a native client pins
	// from the first byte of redemption (§4 step 6, §7). Written by the
	// minting surface, which is the only layer that knows which
	// certificate the listener presents; empty is a link that names none —
	// every browser link, and every link a boot with no certificate mints
	// — which is the trust-on-first-use path the spec already describes
	// for the typed-code case. It lives in the row rather than only in the
	// payload so the value a link was minted with survives the link.
	CertFingerprint string `json:"certFingerprint,omitempty"`
	CreatedAt       int64  `json:"createdAt"`
	ExpiresAt       int64  `json:"expiresAt"`
	RedeemedAt      int64  `json:"redeemedAt,omitempty"`
	// DeviceID, KeyThumbprint, and SessionID are written by redemption.
	// Plain columns, no foreign key: the row is the record of an exchange
	// and is worth most after the device it admitted has been removed —
	// the same reasoning auth_audit's attribution columns carry.
	DeviceID      string `json:"deviceId,omitempty"`
	KeyThumbprint string `json:"keyThumbprint,omitempty"`
	SessionID     string `json:"sessionId,omitempty"`
	ConfirmedAt   int64  `json:"confirmedAt,omitempty"`
	CanceledAt    int64  `json:"canceledAt,omitempty"`
}

// Redeemed reports whether a device has presented this link.
func (p PairingLink) Redeemed() bool { return p.RedeemedAt != 0 }

// Settled reports whether the link's outcome is decided — confirmed or
// canceled. A settled link answers no further call.
func (p PairingLink) Settled() bool { return p.ConfirmedAt != 0 || p.CanceledAt != 0 }

// RefreshSecret is one issued renewal credential. Rotating: each renewal
// writes a new row and spends its predecessor, and a spent row is KEPT
// because recognising a second presentation of it is the whole reuse
// detector (§4 "Sessions").
type RefreshSecret struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	CreatedAt int64  `json:"createdAt"`
	ExpiresAt int64  `json:"expiresAt"`
	// ConsumedAt is 0 for the one secret a session's device currently
	// holds. Every earlier one in the chain carries a stamp.
	ConsumedAt int64 `json:"consumedAt,omitempty"`
	// ConsumedBy names the device key thumbprint that spent it, for the
	// audit trail a reuse investigation reads.
	ConsumedBy string `json:"consumedBy,omitempty"`
}

// Spent reports whether this secret has already been exchanged.
func (r RefreshSecret) Spent() bool { return r.ConsumedAt != 0 }

const pairingLinkColumns = `id, user_id, scopes, binding_class, device_class,
	cert_fingerprint, created_at, expires_at, redeemed_at, device_id,
	key_thumbprint, session_id, confirmed_at, canceled_at`

const refreshSecretColumns = `id, session_id, created_at, expires_at,
	consumed_at, consumed_by`

// CreatePairingLink writes a minted link. The caller owns the id and the
// token hash, because the token itself is returned to the minting surface
// and never stored.
func (s *Store) CreatePairingLink(link PairingLink, tokenHash []byte) error {
	switch {
	case strings.TrimSpace(link.ID) == "":
		return fmt.Errorf("%w: pairing link id", ErrIdentityFieldRequired)
	case strings.TrimSpace(link.UserID) == "":
		return fmt.Errorf("%w: pairing link user id", ErrIdentityFieldRequired)
	case strings.TrimSpace(link.BindingClass) == "":
		return fmt.Errorf("%w: pairing link binding class", ErrIdentityFieldRequired)
	case strings.TrimSpace(link.DeviceClass) == "":
		return fmt.Errorf("%w: pairing link device class", ErrIdentityFieldRequired)
	case len(tokenHash) == 0:
		return fmt.Errorf("%w: pairing link token hash", ErrIdentityFieldRequired)
	case link.ExpiresAt <= link.CreatedAt:
		return fmt.Errorf("store: create pairing link: expiry %d is not after creation %d",
			link.ExpiresAt, link.CreatedAt)
	}
	scopes, err := encodeScopes(link.Scopes)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(
		`INSERT INTO pairing_links (id, user_id, token_hash, scopes, binding_class,
			device_class, cert_fingerprint, created_at, expires_at, redeemed_at,
			device_id, key_thumbprint, session_id, confirmed_at, canceled_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, '', '', '', NULL, NULL)`,
		link.ID, link.UserID, tokenHash, scopes, link.BindingClass, link.DeviceClass,
		link.CertFingerprint, link.CreatedAt, link.ExpiresAt,
	); err != nil {
		return fmt.Errorf("store: create pairing link: %w", err)
	}
	return nil
}

// RedeemPairingLink consumes a link and records the key that presented it,
// returning the row as it now stands.
//
// ONE statement, so consumption is atomic against every other connection:
// `redeemed_at IS NULL` is the single-use rule and `expires_at > ?` is the
// window, and SQLite decides the winner of a race between two redemptions.
// A caller-side read-then-write would leave exactly the gap this shape
// closes.
//
// The thumbprint is written here and the device and session by
// AttachPairingRedemption afterwards, because those two rows do not exist
// yet: what the link's user id and device class admit is only readable from
// the row this statement returns. Spending the link FIRST is the deliberate
// order — a second presentation must be refused even while the first is
// still assembling what it won.
//
// A token that matches no live, unredeemed row answers sql.ErrNoRows —
// unknown, expired, canceled, and already-redeemed all land there together,
// because the difference is not something a redeeming device may be told.
func (s *Store) RedeemPairingLink(tokenHash []byte, at int64, keyThumbprint string) (PairingLink, error) {
	if len(tokenHash) == 0 {
		return PairingLink{}, fmt.Errorf("%w: pairing link token hash", ErrIdentityFieldRequired)
	}
	link, err := scanPairingLink(s.db.QueryRow(
		`UPDATE pairing_links
		    SET redeemed_at = ?, key_thumbprint = ?
		  WHERE token_hash = ? AND redeemed_at IS NULL AND canceled_at IS NULL
		    AND expires_at > ?
		 RETURNING `+pairingLinkColumns,
		at, keyThumbprint, tokenHash, at))
	if errors.Is(err, sql.ErrNoRows) {
		return PairingLink{}, err
	}
	if err != nil {
		return PairingLink{}, fmt.Errorf("store: redeem pairing link: %w", err)
	}
	return link, nil
}

// AttachPairingRedemption records the device and session a redemption
// produced. Scoped to a redeemed, unsettled link that does not already name
// one, so a second call cannot re-point a link at different rows.
func (s *Store) AttachPairingRedemption(linkID, deviceID, sessionID string) error {
	switch {
	case strings.TrimSpace(deviceID) == "":
		return fmt.Errorf("%w: pairing redemption device id", ErrIdentityFieldRequired)
	case strings.TrimSpace(sessionID) == "":
		return fmt.Errorf("%w: pairing redemption session id", ErrIdentityFieldRequired)
	}
	result, err := s.db.Exec(
		`UPDATE pairing_links SET device_id = ?, session_id = ?
		  WHERE id = ? AND redeemed_at IS NOT NULL AND device_id = '' AND session_id = ''
		    AND confirmed_at IS NULL AND canceled_at IS NULL`,
		deviceID, sessionID, linkID)
	if err != nil {
		return fmt.Errorf("store: attach pairing redemption: %w", err)
	}
	return requireRowsAffected(result, "store: attach pairing redemption")
}

// ConfirmPairingLink records the owner matching the verification number,
// returning the row. Scoped to a redeemed, unsettled link: confirming one
// nobody has redeemed would activate a session that does not exist, and
// re-confirming a settled one would let a second decision overwrite the
// first.
func (s *Store) ConfirmPairingLink(id string, at int64) (PairingLink, error) {
	link, err := scanPairingLink(s.db.QueryRow(
		`UPDATE pairing_links SET confirmed_at = ?
		  WHERE id = ? AND redeemed_at IS NOT NULL
		    AND confirmed_at IS NULL AND canceled_at IS NULL
		 RETURNING `+pairingLinkColumns, at, id))
	if errors.Is(err, sql.ErrNoRows) {
		return PairingLink{}, err
	}
	if err != nil {
		return PairingLink{}, fmt.Errorf("store: confirm pairing link: %w", err)
	}
	return link, nil
}

// CancelPairingLink records the owner refusing a link — the verification
// number did not match, or the link was minted by mistake — and returns the
// row so the caller can revoke whatever the redemption already created.
//
// Cancellable before OR after redemption, and that is the point: the
// refusal case the verification number exists for is a link that has
// already been redeemed by a device the owner does not recognise.
func (s *Store) CancelPairingLink(id string, at int64) (PairingLink, error) {
	link, err := scanPairingLink(s.db.QueryRow(
		`UPDATE pairing_links SET canceled_at = ?
		  WHERE id = ? AND confirmed_at IS NULL AND canceled_at IS NULL
		 RETURNING `+pairingLinkColumns, at, id))
	if errors.Is(err, sql.ErrNoRows) {
		return PairingLink{}, err
	}
	if err != nil {
		return PairingLink{}, fmt.Errorf("store: cancel pairing link: %w", err)
	}
	return link, nil
}

// GetPairingLink reads one link by id. sql.ErrNoRows when it does not
// exist. The minting surface reads it to learn which device redeemed and
// what verification number to display.
func (s *Store) GetPairingLink(id string) (PairingLink, error) {
	return scanPairingLink(s.reader().QueryRow(
		`SELECT `+pairingLinkColumns+` FROM pairing_links WHERE id = ?`, id))
}

// ListPairingLinksForUser returns one account's links, newest first,
// settled ones included — the list is also the record of who joined when.
func (s *Store) ListPairingLinksForUser(userID string, limit int) ([]PairingLink, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.reader().Query(
		`SELECT `+pairingLinkColumns+` FROM pairing_links
		  WHERE user_id = ? ORDER BY created_at DESC, id LIMIT ?`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list pairing links: %w", err)
	}
	defer rows.Close()
	out := make([]PairingLink, 0, limit)
	for rows.Next() {
		link, err := scanPairingLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, link)
	}
	return out, rows.Err()
}

// DeletePairingLinksExpiredBefore drops links whose window closed before
// `before` and that nobody redeemed, returning how many went. A redeemed
// link is kept whatever its expiry: it names a device that exists.
func (s *Store) DeletePairingLinksExpiredBefore(before int64) (int64, error) {
	result, err := s.db.Exec(
		`DELETE FROM pairing_links WHERE expires_at < ? AND redeemed_at IS NULL`, before)
	if err != nil {
		return 0, fmt.Errorf("store: delete expired pairing links: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: delete expired pairing links: rows affected: %w", err)
	}
	return rows, nil
}

func scanPairingLink(sc interface{ Scan(...any) error }) (PairingLink, error) {
	var link PairingLink
	var scopes string
	var redeemedAt, confirmedAt, canceledAt sql.NullInt64
	if err := sc.Scan(
		&link.ID, &link.UserID, &scopes, &link.BindingClass, &link.DeviceClass,
		&link.CertFingerprint, &link.CreatedAt, &link.ExpiresAt, &redeemedAt,
		&link.DeviceID, &link.KeyThumbprint, &link.SessionID, &confirmedAt, &canceledAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PairingLink{}, err
		}
		return PairingLink{}, fmt.Errorf("store: scan pairing link: %w", err)
	}
	link.RedeemedAt = redeemedAt.Int64
	link.ConfirmedAt = confirmedAt.Int64
	link.CanceledAt = canceledAt.Int64
	decoded, err := decodeScopes(scopes)
	if err != nil {
		return PairingLink{}, err
	}
	link.Scopes = decoded
	return link, nil
}

// CreateRefreshSecret writes one issued renewal credential. The id is
// minted here; the hash is the caller's, because only the caller ever sees
// the secret.
func (s *Store) CreateRefreshSecret(sessionID string, secretHash []byte, createdAt, expiresAt int64) (RefreshSecret, error) {
	switch {
	case strings.TrimSpace(sessionID) == "":
		return RefreshSecret{}, fmt.Errorf("%w: refresh secret session id", ErrIdentityFieldRequired)
	case len(secretHash) == 0:
		return RefreshSecret{}, fmt.Errorf("%w: refresh secret hash", ErrIdentityFieldRequired)
	case expiresAt <= createdAt:
		return RefreshSecret{}, fmt.Errorf("store: create refresh secret: expiry %d is not after creation %d",
			expiresAt, createdAt)
	}
	secret := RefreshSecret{
		ID:        uuid.NewString(),
		SessionID: sessionID,
		CreatedAt: createdAt,
		ExpiresAt: expiresAt,
	}
	if _, err := s.db.Exec(
		`INSERT INTO refresh_secrets (id, session_id, secret_hash, created_at,
			expires_at, consumed_at, consumed_by)
		 VALUES (?, ?, ?, ?, ?, NULL, '')`,
		secret.ID, secret.SessionID, secretHash, secret.CreatedAt, secret.ExpiresAt,
	); err != nil {
		return RefreshSecret{}, fmt.Errorf("store: create refresh secret: %w", err)
	}
	return secret, nil
}

// ConsumeRefreshSecret spends one unspent, unexpired secret and returns it.
//
// ONE statement again: the two predicates ARE the rotation rule, and
// SQLite decides which of two concurrent presentations wins. The loser
// gets sql.ErrNoRows and the caller then asks GetRefreshSecretByHash what
// happened — which is how a spent secret becomes a detected reuse rather
// than a generic refusal.
func (s *Store) ConsumeRefreshSecret(secretHash []byte, at int64, byThumbprint string) (RefreshSecret, error) {
	if len(secretHash) == 0 {
		return RefreshSecret{}, fmt.Errorf("%w: refresh secret hash", ErrIdentityFieldRequired)
	}
	secret, err := scanRefreshSecret(s.db.QueryRow(
		`UPDATE refresh_secrets SET consumed_at = ?, consumed_by = ?
		  WHERE secret_hash = ? AND consumed_at IS NULL AND expires_at > ?
		 RETURNING `+refreshSecretColumns,
		at, byThumbprint, secretHash, at))
	if errors.Is(err, sql.ErrNoRows) {
		return RefreshSecret{}, err
	}
	if err != nil {
		return RefreshSecret{}, fmt.Errorf("store: consume refresh secret: %w", err)
	}
	return secret, nil
}

// GetRefreshSecretByHash reads a secret whatever its state. Only the
// failure path of ConsumeRefreshSecret calls it, and only to answer one
// question: was this a secret that never existed, or one the real device
// already spent? The two have different consequences and the same
// symptom.
func (s *Store) GetRefreshSecretByHash(secretHash []byte) (RefreshSecret, error) {
	if len(secretHash) == 0 {
		return RefreshSecret{}, fmt.Errorf("%w: refresh secret hash", ErrIdentityFieldRequired)
	}
	return scanRefreshSecret(s.reader().QueryRow(
		`SELECT `+refreshSecretColumns+` FROM refresh_secrets WHERE secret_hash = ?`, secretHash))
}

// SpendRefreshSecretsForSession marks every unspent secret of one session
// consumed, returning how many moved. This is what a detected reuse
// writes: the session is revoked separately, and this makes sure no
// outstanding secret of that family can be exchanged in the window between
// the two writes.
func (s *Store) SpendRefreshSecretsForSession(sessionID string, at int64, reason string) (int64, error) {
	result, err := s.db.Exec(
		`UPDATE refresh_secrets SET consumed_at = ?, consumed_by = ?
		  WHERE session_id = ? AND consumed_at IS NULL`,
		at, reason, sessionID)
	if err != nil {
		return 0, fmt.Errorf("store: spend refresh secrets: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: spend refresh secrets: rows affected: %w", err)
	}
	return rows, nil
}

// ListRefreshSecretsForSession returns one session's chain, newest first.
// For tests and for the audit view that shows how often a device renewed.
func (s *Store) ListRefreshSecretsForSession(sessionID string) ([]RefreshSecret, error) {
	rows, err := s.reader().Query(
		`SELECT `+refreshSecretColumns+` FROM refresh_secrets
		  WHERE session_id = ? ORDER BY created_at DESC, id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: list refresh secrets: %w", err)
	}
	defer rows.Close()
	var out []RefreshSecret
	for rows.Next() {
		secret, err := scanRefreshSecret(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, secret)
	}
	return out, rows.Err()
}

// DeleteRefreshSecretsExpiredBefore drops secrets whose window closed
// before `before`, returning how many went.
//
// Keyed on expiry, never on "consumed": a spent secret inside its window is
// the reuse detector's only evidence, and deleting it would turn a detected
// reuse into an unknown credential. Past its expiry it detects nothing a
// dead credential would not already refuse.
func (s *Store) DeleteRefreshSecretsExpiredBefore(before int64) (int64, error) {
	result, err := s.db.Exec(`DELETE FROM refresh_secrets WHERE expires_at < ?`, before)
	if err != nil {
		return 0, fmt.Errorf("store: delete expired refresh secrets: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: delete expired refresh secrets: rows affected: %w", err)
	}
	return rows, nil
}

func scanRefreshSecret(sc interface{ Scan(...any) error }) (RefreshSecret, error) {
	var secret RefreshSecret
	var consumedAt sql.NullInt64
	if err := sc.Scan(
		&secret.ID, &secret.SessionID, &secret.CreatedAt, &secret.ExpiresAt,
		&consumedAt, &secret.ConsumedBy,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RefreshSecret{}, err
		}
		return RefreshSecret{}, fmt.Errorf("store: scan refresh secret: %w", err)
	}
	secret.ConsumedAt = consumedAt.Int64
	return secret, nil
}
