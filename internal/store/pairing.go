package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// Pairing links and refresh secrets (migration v80). Persistence only, on
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
	Purpose          string `json:"purpose,omitempty"`
	ExpectedKey      string `json:"expectedKey,omitempty"`
	MemberGeneration int64  `json:"memberGeneration,omitempty"`
	SponsorKey       string `json:"sponsorKey,omitempty"`
	ID               string `json:"id"`
	UserID           string `json:"userId"`
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

// Ready reports whether a redeemed link names the session its redemption
// created, which is the one state an owner's confirmation can complete.
func (p PairingLink) Ready() bool { return p.RedeemedAt != 0 && p.SessionID != "" }

// Settled reports whether the link's outcome is decided — confirmed or
// canceled. A settled link answers no further call.
func (p PairingLink) Settled() bool { return p.ConfirmedAt != 0 || p.CanceledAt != 0 }

// RefreshSecret is one issued renewal credential. Rotating: each renewal
// writes a new row and spends its predecessor, and a spent row is KEPT
// because recognising a second presentation of it is the whole reuse
// detector (§4 "Sessions").
type RefreshSecret struct {
	// NextSecretHash recognizes the same client-selected renewal after a lost reply.
	NextSecretHash []byte `json:"-"`
	ID             string `json:"id"`
	SessionID      string `json:"sessionId"`
	CreatedAt      int64  `json:"createdAt"`
	ExpiresAt      int64  `json:"expiresAt"`
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
	key_thumbprint, session_id, confirmed_at, canceled_at, purpose, expected_key, member_generation, sponsor_key`

const refreshSecretColumns = `id, session_id, created_at, expires_at,
	consumed_at, consumed_by, next_secret_hash`

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
			device_id, key_thumbprint, session_id, confirmed_at, canceled_at, purpose, expected_key, member_generation, sponsor_key)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, '', '', '', NULL, NULL, ?, ?, ?, ?)`,
		link.ID, link.UserID, tokenHash, scopes, link.BindingClass, link.DeviceClass,
		link.CertFingerprint, link.CreatedAt, link.ExpiresAt, link.Purpose, link.ExpectedKey, link.MemberGeneration, link.SponsorKey,
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
// CompletePairingRedemption afterwards, because those two rows do not exist
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

// DeviceSession is a session together with the device row it binds to,
// written as one: a failed write adds or renames no device and leaves no
// session or refresh secret.
type DeviceSession struct {
	// Device is created when NewDevice is set (a row built by
	// NewPairedDevice); otherwise it is an existing row, relabeled to the
	// Label and Platform it now reports.
	Device    Device
	NewDevice bool
	// Session is on Device.
	Session Session
	// Refresh is the session's first renewal credential; a nil Hash issues
	// none, for a device class that does not renew.
	Refresh RefreshGrant
}

// writeDeviceSession writes d's device row or relabel, its session, and its
// refresh secret inside the caller's transaction.
func writeDeviceSession(tx sqlExecutor, d DeviceSession) error {
	if d.Session.DeviceID != d.Device.ID {
		return fmt.Errorf("session %s is not on device %s", d.Session.ID, d.Device.ID)
	}
	var err error
	if d.NewDevice {
		err = insertPairedDevice(tx, d.Device)
	} else {
		err = relabelDevice(tx, d.Device.ID, d.Device.Label, d.Device.Platform)
	}
	if err != nil {
		return err
	}
	if err := createSession(tx, d.Session); err != nil {
		return err
	}
	if d.Refresh.Hash != nil {
		if _, err := insertRefreshSecret(tx, d.Session.ID, d.Refresh.Hash, d.Refresh.CreatedAt, d.Refresh.ExpiresAt); err != nil {
			return err
		}
	}
	return nil
}

// CreateDeviceSession writes a live session with its device row or relabel
// and its refresh secret, in one transaction. A pending session is refused:
// it is written only with the link that names it (CompletePairingRedemption).
func (s *Store) CreateDeviceSession(d DeviceSession) error {
	switch {
	case d.Session.ActivatedAt == 0:
		return fmt.Errorf("store: create device session: session %s is not activated", d.Session.ID)
	case d.Session.BindingClass == "loopback-only":
		return fmt.Errorf("store: local sessions require EnsureLocalSession")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: create device session: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := writeDeviceSession(tx, d); err != nil {
		return fmt.Errorf("store: create device session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: create device session: commit: %w", err)
	}
	return nil
}

// PairingRedemption is what a redemption produced for its spent link: the
// redeeming device's row and the unactivated session with its refresh
// secret.
type PairingRedemption struct {
	LinkID string
	DeviceSession
	// ConfirmedAt, when set, confirms an own-device introduction, which no
	// owner confirms, in the same transaction: the session is activated
	// until AccessExpiresAt and the membership admitted.
	ConfirmedAt     int64
	AccessExpiresAt int64
}

// RefreshGrant is a refresh secret to write: the hash of the secret only the
// caller saw, and its window.
type RefreshGrant struct {
	Hash      []byte
	CreatedAt int64
	ExpiresAt int64
}

// CompletePairingRedemption writes the device row a redemption produced or
// its relabel, the session, its refresh secret, and the link naming both, in
// one transaction. A link that is no longer redeemed and unsettled, or that
// already names a redemption, writes nothing: a pending session never exists
// without the link that names it, a failed redemption adds or renames no
// device, and a cancellation that reached the spent link first leaves no
// session behind. sql.ErrNoRows reports that refusal.
//
// With ConfirmedAt set, the same transaction confirms an own-device
// introduction as ConfirmRedeemedPairing would; a confirmation that is
// refused writes nothing and answers ErrPairingIncomplete.
func (s *Store) CompletePairingRedemption(r PairingRedemption) error {
	switch {
	case r.Session.ActivatedAt != 0:
		return fmt.Errorf("store: complete pairing redemption: session %s is already activated", r.Session.ID)
	case r.ConfirmedAt != 0 && r.AccessExpiresAt <= r.ConfirmedAt:
		return fmt.Errorf("store: complete pairing redemption: access expiry %d is not after confirmation %d",
			r.AccessExpiresAt, r.ConfirmedAt)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: complete pairing redemption: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := writeDeviceSession(tx, r.DeviceSession); err != nil {
		return fmt.Errorf("store: complete pairing redemption: %w", err)
	}
	if err := attachPairingRedemption(tx, r.LinkID, r.Session.DeviceID, r.Session.ID); err != nil {
		return err
	}
	if r.ConfirmedAt != 0 {
		link, err := readConfirmableLink(tx, r.LinkID)
		if err != nil {
			return err
		}
		if link.Purpose != "own-introduction" {
			return fmt.Errorf("store: complete pairing redemption: a %q link is confirmed by its owner, not with its redemption", link.Purpose)
		}
		_, refusal, err := admitPairingTx(tx, link, r.ConfirmedAt, r.AccessExpiresAt)
		if err != nil {
			return err
		}
		if refusal != nil {
			return fmt.Errorf("%w: %v", ErrPairingIncomplete, refusal)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: complete pairing redemption: commit: %w", err)
	}
	return nil
}

// attachPairingRedemption records the device and session a redemption
// produced. Scoped to a redeemed, unsettled link that does not already name
// one, so a second call cannot re-point a link at different rows.
func attachPairingRedemption(db sqlExecutor, linkID, deviceID, sessionID string) error {
	switch {
	case strings.TrimSpace(deviceID) == "":
		return fmt.Errorf("%w: pairing redemption device id", ErrIdentityFieldRequired)
	case strings.TrimSpace(sessionID) == "":
		return fmt.Errorf("%w: pairing redemption session id", ErrIdentityFieldRequired)
	}
	result, err := db.Exec(
		`UPDATE pairing_links SET device_id = ?, session_id = ?
		  WHERE id = ? AND redeemed_at IS NOT NULL AND device_id = '' AND session_id = ''
		    AND confirmed_at IS NULL AND canceled_at IS NULL`,
		deviceID, sessionID, linkID)
	if err != nil {
		return fmt.Errorf("store: attach pairing redemption: %w", err)
	}
	return requireRowsAffected(result, "store: attach pairing redemption")
}

// ErrPairingIncomplete reports a confirmation that could not admit its
// device. ConfirmRedeemedPairing cancelled the link and revoked its session
// instead; CompletePairingRedemption wrote nothing.
var ErrPairingIncomplete = errors.New("store: the pairing could not be completed")

// PairingConfirmation is what one ConfirmRedeemedPairing settled.
type PairingConfirmation struct {
	// Link is the confirmed link.
	Link PairingLink
	// Canceled is what an incomplete confirmation cancelled instead.
	Canceled PairingCancellation
}

// ConfirmRedeemedPairing records the owner matching the verification number
// and admits the device, in one transaction: the link is confirmed, its
// session activated with accessExpiresAt, and a personal pairing's
// membership admitted, or none of it is. Scoped to a redeemed link that
// names its session and is unsettled; sql.ErrNoRows otherwise.
//
// A session that can no longer be activated (revoked, lapsed, or its device
// revoked) or a membership that is no longer admissible cannot be completed
// by a retry, so the same transaction cancels the link and revokes the
// session instead, and the call answers ErrPairingIncomplete with what that
// ended. Any other failure rolls back, leaving the link as it was.
func (s *Store) ConfirmRedeemedPairing(id string, at, accessExpiresAt int64) (PairingConfirmation, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return PairingConfirmation{}, fmt.Errorf("store: confirm pairing: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	link, err := readConfirmableLink(tx, id)
	if err != nil {
		return PairingConfirmation{}, err
	}
	if _, err := tx.Exec(`SAVEPOINT confirm_pairing`); err != nil {
		return PairingConfirmation{}, fmt.Errorf("store: confirm pairing: %w", err)
	}
	confirmed, refusal, err := admitPairingTx(tx, link, at, accessExpiresAt)
	if err != nil {
		return PairingConfirmation{}, err
	}
	if refusal != nil {
		if _, err := tx.Exec(`ROLLBACK TO confirm_pairing`); err != nil {
			return PairingConfirmation{}, fmt.Errorf("store: confirm pairing: %w", err)
		}
		canceled, err := cancelPairingLinkTx(tx, id, at)
		if err != nil {
			return PairingConfirmation{}, err
		}
		if err := tx.Commit(); err != nil {
			return PairingConfirmation{}, fmt.Errorf("store: confirm pairing: commit the cancellation: %w", err)
		}
		return PairingConfirmation{Canceled: canceled}, fmt.Errorf("%w: %v", ErrPairingIncomplete, refusal)
	}
	if err := tx.Commit(); err != nil {
		return PairingConfirmation{}, fmt.Errorf("store: confirm pairing: commit: %w", err)
	}
	return PairingConfirmation{Link: confirmed}, nil
}

// readConfirmableLink settles a personal pairing's member generation and
// reads the link if it is redeemed, names its session and is unsettled;
// sql.ErrNoRows otherwise.
func readConfirmableLink(tx *sql.Tx, id string) (PairingLink, error) {
	if err := prepareOwnPairing(tx, id); err != nil {
		return PairingLink{}, err
	}
	link, err := scanPairingLink(tx.QueryRow(
		`SELECT `+pairingLinkColumns+` FROM pairing_links
		  WHERE id = ? AND redeemed_at IS NOT NULL AND session_id <> ''
		    AND confirmed_at IS NULL AND canceled_at IS NULL`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return PairingLink{}, err
	}
	if err != nil {
		return PairingLink{}, fmt.Errorf("store: confirm pairing: %w", err)
	}
	return link, nil
}

// admitPairingTx activates link's session, confirms link and admits its
// membership. refusal says why the device cannot be admitted; err is a
// failure to write.
func admitPairingTx(tx *sql.Tx, link PairingLink, at, accessExpiresAt int64) (confirmed PairingLink, refusal, err error) {
	moved, err := activateSession(tx, link.SessionID, at, accessExpiresAt)
	if err != nil {
		return PairingLink{}, nil, err
	}
	if !moved {
		return PairingLink{}, errors.New("its session or device was revoked or expired"), nil
	}
	confirmed, err = scanPairingLink(tx.QueryRow(
		`UPDATE pairing_links SET confirmed_at = ? WHERE id = ?
		 RETURNING `+pairingLinkColumns, at, link.ID))
	if err != nil {
		return PairingLink{}, nil, fmt.Errorf("store: confirm pairing link: %w", err)
	}
	if err := admitOwnPairingTx(tx, confirmed); err != nil {
		if errors.Is(err, ErrOwnAdmissionRefused) {
			return PairingLink{}, err, nil
		}
		return PairingLink{}, nil, err
	}
	return confirmed, nil, nil
}

// CancelPairingLink records the owner refusing a link — the verification
// number did not match, or the link was minted by mistake — and returns the
// row so the caller can revoke whatever the redemption already created.
//
// Cancellable before OR after redemption, and that is the point: the
// refusal case the verification number exists for is a link that has
// already been redeemed by a device the owner does not recognise.
//
// The link and the unconfirmed session its redemption created end in one
// transaction, so a cancellation either did both or neither: a canceled
// link never leaves its pending session behind, and a retry after a
// failure finds the link still open. sql.ErrNoRows when the link is
// unknown or already settled.
func (s *Store) CancelPairingLink(id string, at int64) (PairingCancellation, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return PairingCancellation{}, fmt.Errorf("store: cancel pairing link: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	out, err := cancelPairingLinkTx(tx, id, at)
	if err != nil {
		return PairingCancellation{}, err
	}
	if err := tx.Commit(); err != nil {
		return PairingCancellation{}, fmt.Errorf("store: cancel pairing link: commit: %w", err)
	}
	return out, nil
}

// cancelPairingLinkTx is CancelPairingLink's statements, in tx.
func cancelPairingLinkTx(tx *sql.Tx, id string, at int64) (PairingCancellation, error) {
	link, err := scanPairingLink(tx.QueryRow(
		`UPDATE pairing_links SET canceled_at = ?
		  WHERE id = ? AND confirmed_at IS NULL AND canceled_at IS NULL
		 RETURNING `+pairingLinkColumns, at, id))
	if errors.Is(err, sql.ErrNoRows) {
		return PairingCancellation{}, err
	}
	if err != nil {
		return PairingCancellation{}, fmt.Errorf("store: cancel pairing link: %w", err)
	}
	out := PairingCancellation{Link: link}
	if link.SessionID != "" {
		ids, released, err := endSessionsTx(tx, revokeSessions, at, nil, `id = ?`, link.SessionID)
		if err != nil {
			return PairingCancellation{}, fmt.Errorf("store: cancel pairing link: revoke its pending session: %w", err)
		}
		out.SessionRevoked, out.Released = len(ids) > 0, released
	}
	return out, nil
}

// PairingCancellation is what one CancelPairingLink ended.
type PairingCancellation struct {
	Link PairingLink
	// SessionRevoked reports that the link's unconfirmed session was
	// revoked with it; Released names the threads whose spawn links that
	// revocation released, as RevokeSession reports them.
	SessionRevoked bool
	Released       []string
}

// RetirePendingPairings runs before a restarted host serves credentials. An
// unfinished screen comparison cannot survive the process that owned it; an
// already-confirmed device is deliberately outside both update predicates.
func (s *Store) RetirePendingPairings(at int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: retire pending pairings: %w", err)
	}
	defer tx.Rollback()
	if _, _, err = endSessionsTx(tx, revokeSessions, at, nil, `activated_at IS NULL
		AND id IN (SELECT session_id FROM pairing_links WHERE confirmed_at IS NULL)`); err != nil {
		return fmt.Errorf("store: retire pending pairing sessions: %w", err)
	}
	if _, err = tx.Exec(`UPDATE pairing_links SET canceled_at = ?
		WHERE confirmed_at IS NULL AND canceled_at IS NULL`, at); err != nil {
		return fmt.Errorf("store: retire pending pairing links: %w", err)
	}
	return tx.Commit()
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
// A non-positive limit returns no links without reading.
func (s *Store) ListPairingLinksForUser(userID string, limit int) ([]PairingLink, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.reader().Query(
		`SELECT `+pairingLinkColumns+` FROM pairing_links
		  WHERE user_id = ? ORDER BY created_at DESC, id LIMIT `+strconv.Itoa(limit), userID)
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
		&link.Purpose, &link.ExpectedKey, &link.MemberGeneration, &link.SponsorKey,
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

// insertRefreshSecret writes a session's first renewal credential inside the
// transaction that writes the session (writeDeviceSession). The id is minted
// here; the hash is the caller's, because only the caller ever sees the
// secret.
func insertRefreshSecret(db sqlExecutor, sessionID string, secretHash []byte, createdAt, expiresAt int64) (RefreshSecret, error) {
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
	if _, err := db.Exec(
		`INSERT INTO refresh_secrets (id, session_id, secret_hash, created_at,
			expires_at, consumed_at, consumed_by)
		 VALUES (?, ?, ?, ?, ?, NULL, '')`,
		secret.ID, secret.SessionID, secretHash, secret.CreatedAt, secret.ExpiresAt,
	); err != nil {
		return RefreshSecret{}, fmt.Errorf("store: create refresh secret: %w", err)
	}
	return secret, nil
}

// GetRefreshSecretByHash reads a secret whatever its state. A renewal reads
// it before it rotates, to answer one question: was this a secret that never
// existed, or one the real device already spent? The two have different
// consequences and the same symptom.
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
// dead credential would not already refuse — with one exception. A spent
// secret whose recorded successor is still unspent is the receipt a
// recoverable renewal is recovered against (RotateRefreshSecret), and it is
// the ONLY thing that distinguishes "the device that chose that successor,
// retrying a lost reply" from "somebody holding a copy of the live head".
// It stays until the successor is spent or gone; then it goes.
func (s *Store) DeleteRefreshSecretsExpiredBefore(before int64) (int64, error) {
	result, err := s.db.Exec(`DELETE FROM refresh_secrets
        WHERE expires_at < ?
          AND NOT EXISTS (SELECT 1 FROM refresh_secrets n
                          WHERE n.secret_hash = refresh_secrets.next_secret_hash
                            AND n.consumed_at IS NULL)`, before)
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
		&consumedAt, &secret.ConsumedBy, &secret.NextSecretHash,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RefreshSecret{}, err
		}
		return RefreshSecret{}, fmt.Errorf("store: scan refresh secret: %w", err)
	}
	secret.ConsumedAt = consumedAt.Int64
	return secret, nil
}
