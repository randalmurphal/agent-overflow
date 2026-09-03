package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Passkey rows (migration v82). Persistence only, on the same terms as the
// rest of the identity family: this package round-trips what a ceremony
// produced and enforces what SQLite can state about it — presence,
// uniqueness, the CHECK'd 0/1 columns. What a credential PROVES, which RP
// ID it may be presented under, and whether a counter that failed to
// advance matters are internal/identity's answers.

// Passkey is one registered WebAuthn credential belonging to an account.
//
// The fields mirror what a ceremony hands back and nothing more. The raw
// attestation blob is deliberately absent: it is the manufacturer's
// statement about the authenticator at registration time, it is the
// largest thing a ceremony produces, and nothing in this backend verifies
// or re-reads it. Keeping it would be storage with no reader.
type Passkey struct {
	ID     string `json:"id"`
	UserID string `json:"userId"`
	// Label is what the person calls this credential in the list. Advisory;
	// nothing authorizes on it.
	Label string `json:"label"`
	// CredentialID is the authenticator's own opaque id, raw. An assertion
	// arrives naming it, so it is the lookup key and it is uniquely
	// indexed.
	CredentialID []byte `json:"credentialId"`
	// PublicKey is the COSE-encoded public key the assertion's signature is
	// verified against.
	PublicKey         []byte `json:"publicKey"`
	AttestationType   string `json:"attestationType,omitempty"`
	AttestationFormat string `json:"attestationFormat,omitempty"`
	// Transports is what the browser reported about how to reach this
	// authenticator (`internal`, `hybrid`, `usb`, ...). Advisory: it is
	// passed back as a hint on a later ceremony and authorizes nothing.
	Transports []string `json:"transports,omitempty"`
	// AAGUID identifies the authenticator MODEL, not the credential. All
	// zeroes for an authenticator that declines to say, which is common.
	AAGUID []byte `json:"aaguid,omitempty"`
	// Attachment is `platform` or `cross-platform` as the ceremony
	// reported it, or empty.
	Attachment string `json:"attachment,omitempty"`
	// RPID is the domain this credential was registered under. An
	// authenticator will not present it to any other, so a row whose RP ID
	// no longer matches this backend's canonical domain can never assert
	// again — it is still listed, carrying this value, because a list that
	// hid it would leave a person unable to delete what they can see in
	// their own authenticator.
	RPID string `json:"rpId"`
	// SignCount is the authenticator's counter as of the last assertion.
	SignCount uint32 `json:"signCount"`
	// CloneWarning records that a counter failed to advance. Persisted and
	// surfaced, never acted on: authenticators that keep no counter at all
	// report {0, 0} forever, so refusing on it would sign a person out of a
	// working key on evidence that is routinely absent.
	CloneWarning bool `json:"cloneWarning,omitempty"`
	// UserVerified is what registration was performed with. Whether a given
	// ASSERTION verified the person is read off that assertion's own flags;
	// this is the credential's enrollment fact, which is what a list can
	// honestly show.
	UserVerified bool `json:"userVerified,omitempty"`
	// BackupEligible and BackupState are the synced-credential flags.
	// Eligibility LATCHES: it is decided at registration, so an assertion
	// claiming a different one is a different credential.
	BackupEligible bool  `json:"backupEligible,omitempty"`
	BackupState    bool  `json:"backupState,omitempty"`
	CreatedAt      int64 `json:"createdAt"`
	// LastUsedAt is 0 for a credential that has never asserted.
	LastUsedAt int64 `json:"lastUsedAt,omitempty"`
}

const passkeyColumns = `id, user_id, label, credential_id, public_key,
	attestation_type, attestation_format, transports, aaguid, attachment,
	rp_id, sign_count, clone_warning, user_verified, backup_eligible,
	backup_state, created_at, last_used_at`

// EnsureUserWebAuthnHandle records the account's WebAuthn user handle,
// keeping whatever the row already holds.
//
// The caller draws the bytes, because entropy is not this package's
// business; the guard `webauthn_user_handle IS NULL` inside the UPDATE is
// what makes the write idempotent, so two concurrent first ceremonies
// cannot hand one account two handles. The read-back is the answer either
// way: a caller that lost the race gets the winner's handle rather than
// its own, which is the only value any authenticator will ever return.
func (s *Store) EnsureUserWebAuthnHandle(userID string, handle []byte) ([]byte, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, fmt.Errorf("%w: passkey user id", ErrIdentityFieldRequired)
	}
	if len(handle) == 0 {
		return nil, fmt.Errorf("%w: webauthn user handle", ErrIdentityFieldRequired)
	}
	if _, err := s.db.Exec(
		`UPDATE users SET webauthn_user_handle = ?
		  WHERE id = ? AND webauthn_user_handle IS NULL`, handle, userID); err != nil {
		return nil, fmt.Errorf("store: set webauthn user handle: %w", err)
	}
	var stored []byte
	if err := s.reader().QueryRow(
		`SELECT webauthn_user_handle FROM users WHERE id = ?`, userID).Scan(&stored); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("store: read webauthn user handle: %w", err)
	}
	return stored, nil
}

// UserByWebAuthnHandle resolves the account a discoverable assertion named.
// sql.ErrNoRows when no account carries that handle, which is what an
// assertion minted against another backend answers.
func (s *Store) UserByWebAuthnHandle(handle []byte) (User, error) {
	if len(handle) == 0 {
		return User{}, sql.ErrNoRows
	}
	return scanUser(s.reader().QueryRow(
		`SELECT `+userColumns+` FROM users
		  WHERE webauthn_user_handle = ? AND webauthn_user_handle IS NOT NULL`, handle))
}

// CreatePasskey records one registered credential. The unique index on
// `credential_id` refuses a second row for the same authenticator
// credential rather than letting two accounts claim it.
func (s *Store) CreatePasskey(passkey Passkey) error {
	if strings.TrimSpace(passkey.ID) == "" {
		return fmt.Errorf("%w: passkey id", ErrIdentityFieldRequired)
	}
	if strings.TrimSpace(passkey.UserID) == "" {
		return fmt.Errorf("%w: passkey user id", ErrIdentityFieldRequired)
	}
	if strings.TrimSpace(passkey.Label) == "" {
		return fmt.Errorf("%w: passkey label", ErrIdentityFieldRequired)
	}
	if len(passkey.CredentialID) == 0 {
		return fmt.Errorf("%w: passkey credential id", ErrIdentityFieldRequired)
	}
	if len(passkey.PublicKey) == 0 {
		return fmt.Errorf("%w: passkey public key", ErrIdentityFieldRequired)
	}
	if strings.TrimSpace(passkey.RPID) == "" {
		return fmt.Errorf("%w: passkey relying party id", ErrIdentityFieldRequired)
	}
	transports, err := encodeTransports(passkey.Transports)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(
		`INSERT INTO passkeys (`+passkeyColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		passkey.ID, passkey.UserID, strings.TrimSpace(passkey.Label),
		passkey.CredentialID, passkey.PublicKey,
		passkey.AttestationType, passkey.AttestationFormat, transports,
		passkey.AAGUID, passkey.Attachment, passkey.RPID,
		passkey.SignCount, boolToInt(passkey.CloneWarning),
		boolToInt(passkey.UserVerified), boolToInt(passkey.BackupEligible),
		boolToInt(passkey.BackupState), passkey.CreatedAt, passkey.LastUsedAt,
	); err != nil {
		return fmt.Errorf("store: create passkey: %w", err)
	}
	return nil
}

// PasskeyByCredentialID resolves the credential an assertion named.
// sql.ErrNoRows when this backend holds no such row.
func (s *Store) PasskeyByCredentialID(credentialID []byte) (Passkey, error) {
	if len(credentialID) == 0 {
		return Passkey{}, sql.ErrNoRows
	}
	return scanPasskey(s.reader().QueryRow(
		`SELECT `+passkeyColumns+` FROM passkeys WHERE credential_id = ?`, credentialID))
}

// ListPasskeysForUser returns one account's credentials, oldest first.
//
// Every row, including one registered under an RP ID this backend no
// longer answers to. Such a credential can never assert again, and hiding
// it would leave a person looking at an entry in their own authenticator
// with nothing here to remove.
func (s *Store) ListPasskeysForUser(userID string) ([]Passkey, error) {
	rows, err := s.reader().Query(
		`SELECT `+passkeyColumns+` FROM passkeys WHERE user_id = ? ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: list passkeys: %w", err)
	}
	defer rows.Close()
	var out []Passkey
	for rows.Next() {
		passkey, err := scanPasskey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, passkey)
	}
	return out, rows.Err()
}

// RecordPasskeyAssertion writes back what one successful assertion
// reported: the authenticator's counter, the verdict on it, the current
// backup state, and when it was used.
//
// One statement, because they are one event. Splitting the counter from
// the clone verdict would let a row record an advanced counter with a
// stale warning, which reads as an anomaly that already cleared.
//
// `backup_eligible` is deliberately NOT written here: eligibility is
// decided at registration and latches, so an assertion is not allowed to
// change it. `user_verified` is not written either — it records what
// enrollment was performed with, and a later assertion's verification is a
// fact about that assertion, judged where the decision is made.
func (s *Store) RecordPasskeyAssertion(id string, signCount uint32, cloneWarning, backupState bool, at int64) error {
	result, err := s.db.Exec(
		`UPDATE passkeys SET sign_count = ?, clone_warning = ?, backup_state = ?, last_used_at = ?
		  WHERE id = ?`,
		signCount, boolToInt(cloneWarning), boolToInt(backupState), at, id)
	if err != nil {
		return fmt.Errorf("store: record passkey assertion: %w", err)
	}
	return requireRowsAffected(result, "store: record passkey assertion")
}

// DeletePasskey removes one credential belonging to an account, reporting
// whether a row was there to remove. Deleting one that is already gone is
// a no-op rather than an error, so a second click on the surface answers
// the same thing twice.
//
// Scoped by USER as well as by id: the id comes off the wire, and a
// delete that named only it would let a caller reach a row belonging to
// another account on a hub deployment.
func (s *Store) DeletePasskey(userID, id string) (bool, error) {
	result, err := s.db.Exec(`DELETE FROM passkeys WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return false, fmt.Errorf("store: delete passkey: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: delete passkey: rows affected: %w", err)
	}
	return rows > 0, nil
}

// encodeTransports renders the advisory transport hints. An empty set is
// the empty string rather than `[]`, so "nothing reported" has one
// spelling on disk.
func encodeTransports(transports []string) (string, error) {
	if len(transports) == 0 {
		return "", nil
	}
	buf, err := json.Marshal(transports)
	if err != nil {
		return "", fmt.Errorf("store: encode passkey transports: %w", err)
	}
	return string(buf), nil
}

func scanPasskey(sc interface{ Scan(...any) error }) (Passkey, error) {
	var passkey Passkey
	var transports string
	var cloneWarning, userVerified, backupEligible, backupState int
	if err := sc.Scan(
		&passkey.ID, &passkey.UserID, &passkey.Label, &passkey.CredentialID,
		&passkey.PublicKey, &passkey.AttestationType, &passkey.AttestationFormat,
		&transports, &passkey.AAGUID, &passkey.Attachment, &passkey.RPID,
		&passkey.SignCount, &cloneWarning, &userVerified, &backupEligible,
		&backupState, &passkey.CreatedAt, &passkey.LastUsedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Passkey{}, err
		}
		return Passkey{}, fmt.Errorf("store: scan passkey: %w", err)
	}
	if transports != "" {
		// A blob that does not decode is an error, never an empty hint set:
		// the same rule the scope blob follows. Reading corruption as "no
		// transports" would turn a storage fault into a ceremony that
		// silently loses its hints.
		if err := json.Unmarshal([]byte(transports), &passkey.Transports); err != nil {
			return Passkey{}, fmt.Errorf("store: decode passkey transports for %s: %w", passkey.ID, err)
		}
	}
	passkey.CloneWarning = cloneWarning != 0
	passkey.UserVerified = userVerified != 0
	passkey.BackupEligible = backupEligible != 0
	passkey.BackupState = backupState != 0
	return passkey, nil
}
