package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Identity rows (migration v75). Persistence only: this package validates
// what SQLite can state — presence, uniqueness, the CHECK'd value sets —
// and leaves the meaning of a scope, a binding class, or an audit event
// to internal/identity, which is the layer that mints and verifies them.
//
// Every read and write here names its principal explicitly. There is no
// accessor that answers "the user", and the one accessor that resolves a
// user by ROLE (EnsureOwnerUser) exists solely for first boot and says so.
// That is what keeps a hub deployment with many accounts from inheriting a
// single-owner assumption baked into a query nobody re-read
// (docs/specs/remote-access.md §16 phase 2).

// Timestamps that mean "has not happened yet" are stored NULL and read as
// 0. Callers compare against zero; nothing in this package distinguishes
// "never revoked" from "revoked at the epoch", and nothing should.

// UserRoleOwner and UserRoleMember are the two values `users.role` may
// hold. Owner is the first-boot account and at most one row may carry it
// (partial unique index `idx_users_single_owner`); every later account is
// a member. The role is not a permission — scopes are — it only records
// which account bootstrapped this backend.
const (
	UserRoleOwner  = "owner"
	UserRoleMember = "member"
)

// User is one account.
type User struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Role        string `json:"role"`
	CreatedAt   int64  `json:"createdAt"`
	// DisabledAt is 0 for an active account. A disabled account keeps its
	// rows — sessions, audit history, attribution on every thread it
	// started — which deleting it would take with them.
	DisabledAt int64 `json:"disabledAt,omitempty"`
}

// Device is one client instance: this desktop, this browser profile, this
// phone, a peer backend.
type Device struct {
	ID       string `json:"id"`
	UserID   string `json:"userId"`
	Label    string `json:"label"`
	Class    string `json:"class"`
	Platform string `json:"platform"`
	// KeyThumbprint and PasskeyCredentialID are the two proof-of-possession
	// slots. Empty means the device has not registered that factor; both
	// are filled by later phases and both are unique across devices when
	// present, so one key can never name two devices.
	KeyThumbprint       string `json:"keyThumbprint,omitempty"`
	PasskeyCredentialID string `json:"passkeyCredentialId,omitempty"`
	// Channel names a device the backend mints for ITSELF rather than one
	// a person paired: today only the local page channel (v76). Empty for
	// every paired device, and uniquely indexed when present, so a channel
	// resolves to the same row on every boot instead of accumulating one
	// device per launch.
	Channel    string `json:"channel,omitempty"`
	CreatedAt  int64  `json:"createdAt"`
	LastSeenAt int64  `json:"lastSeenAt,omitempty"`
	RevokedAt  int64  `json:"revokedAt,omitempty"`
}

// Session binds a device to a user with a scope set for a bounded window.
// The row is the authoritative half of a credential; the signed claims
// that travel with it are the other half, and a presentation is valid only
// when both agree (§3).
type Session struct {
	ID           string   `json:"id"`
	UserID       string   `json:"userId"`
	DeviceID     string   `json:"deviceId"`
	BindingClass string   `json:"bindingClass"`
	Scopes       []string `json:"scopes"`
	// SigningKeyID names the key whose MAC the claims carry. Sessions
	// cascade with their key: dropping a key retires everything it minted.
	SigningKeyID string `json:"signingKeyId"`
	CreatedAt    int64  `json:"createdAt"`
	ExpiresAt    int64  `json:"expiresAt"`
	RevokedAt    int64  `json:"revokedAt,omitempty"`
	LastSeenAt   int64  `json:"lastSeenAt,omitempty"`
	// ActivatedAt is 0 while a session is still waiting for the owner to
	// confirm the pairing verification number (v76). Every directly minted
	// session carries the moment it was created, because nothing gates it.
	ActivatedAt int64 `json:"activatedAt,omitempty"`
}

// Live reports whether this row would admit a presentation at now (Unix
// millis) — activated, not revoked, not past its expiry. The claims still
// have to verify separately; this answers the row half only.
//
// Activation is checked HERE rather than at the call sites that care,
// because a session awaiting confirmation is exactly as unusable as a
// revoked one and must be refused by the same predicate. A second check
// somewhere else is one a later call path forgets.
func (s Session) Live(now int64) bool {
	return s.ActivatedAt != 0 && s.RevokedAt == 0 && s.ExpiresAt > now
}

// AwaitingConfirmation reports whether this row exists but has not been
// confirmed yet, which is the one refusal `Live` produces that has an
// action attached: confirm the verification number on the device that
// minted the pairing link.
func (s Session) AwaitingConfirmation() bool { return s.ActivatedAt == 0 && s.RevokedAt == 0 }

// SigningKey is one HMAC secret the claims codec signs and verifies with.
// The active key is the newest row; older rows stay so credentials minted
// under them keep verifying until they are deliberately dropped.
type SigningKey struct {
	ID        string
	Secret    []byte
	CreatedAt int64
}

// AuthAuditEntry is one credential event. Append-only: a BEFORE UPDATE
// trigger aborts any rewrite, so the history of what was minted, refused,
// and revoked cannot be edited after the fact. Rows are pruned by age
// order (see pruneAuthAudit) rather than rewritten.
//
// UserID / DeviceID / SessionID are attribution, not foreign keys: the
// record of a revocation matters most once the device row is gone.
type AuthAuditEntry struct {
	ID        int64  `json:"id"`
	At        int64  `json:"at"`
	Event     string `json:"event"`
	Outcome   string `json:"outcome"`
	Reason    string `json:"reason,omitempty"`
	UserID    string `json:"userId,omitempty"`
	DeviceID  string `json:"deviceId,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
	Peer      string `json:"peer,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// AuthAuditOutcomeAllowed / AuthAuditOutcomeRefused are the CHECK'd values
// of `auth_audit.outcome`.
const (
	AuthAuditOutcomeAllowed = "allowed"
	AuthAuditOutcomeRefused = "refused"
)

// maxAuthAuditRows bounds the credential log. A refusal writes a row, and
// a peer that keeps presenting a dead credential would otherwise grow the
// table without limit; the rate limiter caps the arrival rate and this
// caps the residue. Ten thousand rows is months of ordinary use (a mint, a
// refresh, a revocation) and a few hours of a wedged client retrying.
const maxAuthAuditRows = 10000

// authAuditPruneEvery is how often an append also prunes. Pruning on every
// insert would put a second statement on every credential event for a
// table that grows one row at a time; every 64th insert bounds the table
// to maxAuthAuditRows + 63 while paying the DELETE once per 64 events.
const authAuditPruneEvery = 64

const userColumns = `id, display_name, role, created_at, disabled_at`

const deviceColumns = `id, user_id, label, class, platform, key_thumbprint,
	passkey_credential_id, channel, created_at, last_seen_at, revoked_at`

const sessionColumns = `id, user_id, device_id, binding_class, scopes,
	signing_key_id, created_at, expires_at, revoked_at, last_seen_at, activated_at`

const authAuditColumns = `id, at, event, outcome, reason, user_id, device_id,
	session_id, peer, detail`

// ErrIdentityFieldRequired is returned when a write is missing a value the
// row cannot be meaningful without. It is a caller bug, not a data state,
// and it is checked in Go because SQLite's NOT NULL accepts the empty
// string.
var ErrIdentityFieldRequired = errors.New("store: identity field required")

// EnsureOwnerUser returns the owner account, creating it on first boot.
//
// This is the ONLY place a user is resolved by role, and it exists so the
// first pairing has something to bind to. Every other read takes an
// explicit user id. Concurrency-safe by construction: the insert races
// against the partial unique index, and a loser re-reads the winner's row
// rather than reporting a conflict.
func (s *Store) EnsureOwnerUser(displayName string) (User, error) {
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		return User{}, fmt.Errorf("%w: owner display name", ErrIdentityFieldRequired)
	}
	if user, err := s.ownerUser(); err == nil {
		return user, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return User{}, err
	}
	_, err := s.db.Exec(
		`INSERT INTO users (id, display_name, role, created_at, disabled_at)
		 VALUES (?, ?, ?, ?, NULL)
		 ON CONFLICT DO NOTHING`,
		uuid.NewString(), displayName, UserRoleOwner, nowMillis(),
	)
	if err != nil {
		return User{}, fmt.Errorf("store: create owner user: %w", err)
	}
	return s.ownerUser()
}

func (s *Store) ownerUser() (User, error) {
	return scanUser(s.reader().QueryRow(
		`SELECT `+userColumns+` FROM users WHERE role = ?`, UserRoleOwner))
}

// CreateUser adds a member account. Team sharing mints these; nothing
// about the schema or these accessors treats them as second-class.
func (s *Store) CreateUser(displayName string) (User, error) {
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		return User{}, fmt.Errorf("%w: user display name", ErrIdentityFieldRequired)
	}
	user := User{
		ID:          uuid.NewString(),
		DisplayName: displayName,
		Role:        UserRoleMember,
		CreatedAt:   nowMillis(),
	}
	if _, err := s.db.Exec(
		`INSERT INTO users (id, display_name, role, created_at, disabled_at)
		 VALUES (?, ?, ?, ?, NULL)`,
		user.ID, user.DisplayName, user.Role, user.CreatedAt,
	); err != nil {
		return User{}, fmt.Errorf("store: create user: %w", err)
	}
	return user, nil
}

// GetUser reads one account by id. sql.ErrNoRows when it does not exist.
func (s *Store) GetUser(id string) (User, error) {
	return scanUser(s.reader().QueryRow(
		`SELECT `+userColumns+` FROM users WHERE id = ?`, id))
}

// ListUsers returns every account, oldest first.
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.reader().Query(
		`SELECT ` + userColumns + ` FROM users ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list users: %w", err)
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		user, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, user)
	}
	return out, rows.Err()
}

func scanUser(sc interface{ Scan(...any) error }) (User, error) {
	var user User
	var disabledAt sql.NullInt64
	if err := sc.Scan(&user.ID, &user.DisplayName, &user.Role, &user.CreatedAt, &disabledAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, err
		}
		return User{}, fmt.Errorf("store: scan user: %w", err)
	}
	user.DisabledAt = disabledAt.Int64
	return user, nil
}

// CreateDevice registers a client instance. The caller supplies the label,
// class, and platform; ids and stamps are minted here so two callers
// cannot disagree about them.
//
// The proof-of-possession slots start empty. A device without one is a
// device that has not yet presented a key, which every phase-2 client is.
func (s *Store) CreateDevice(userID, label, class, platform string) (Device, error) {
	if strings.TrimSpace(userID) == "" {
		return Device{}, fmt.Errorf("%w: device user id", ErrIdentityFieldRequired)
	}
	if strings.TrimSpace(label) == "" {
		return Device{}, fmt.Errorf("%w: device label", ErrIdentityFieldRequired)
	}
	device := Device{
		ID:        uuid.NewString(),
		UserID:    userID,
		Label:     strings.TrimSpace(label),
		Class:     class,
		Platform:  platform,
		CreatedAt: nowMillis(),
	}
	if _, err := s.db.Exec(
		`INSERT INTO devices (id, user_id, label, class, platform, key_thumbprint,
			passkey_credential_id, channel, created_at, last_seen_at, revoked_at)
		 VALUES (?, ?, ?, ?, ?, NULL, NULL, '', ?, 0, NULL)`,
		device.ID, device.UserID, device.Label, device.Class, device.Platform, device.CreatedAt,
	); err != nil {
		return Device{}, fmt.Errorf("store: create device: %w", err)
	}
	return device, nil
}

// EnsureChannelDevice resolves the one device row belonging to an implicit
// backend channel (v76 `devices.channel`), creating it on first boot.
// Idempotent, and safe against a concurrent boot of the same store: the
// insert names the channel, the partial unique index refuses the second
// one, and the loser re-reads the winner's row.
//
// Unlike CreateDevice this is a resolve, not a mint — the channel is a
// property of this backend, not of something a person paired, so two calls
// must answer the same device rather than two.
//
// A revoked channel device is returned as it is, revoked. Reviving it here
// would let a boot undo a deliberate revocation, and the caller is the one
// that knows whether re-minting is the right answer.
func (s *Store) EnsureChannelDevice(userID, channel, label, class, platform string) (Device, error) {
	if strings.TrimSpace(channel) == "" {
		return Device{}, fmt.Errorf("%w: device channel", ErrIdentityFieldRequired)
	}
	existing, err := s.deviceByChannel(channel)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Device{}, err
	}
	if strings.TrimSpace(userID) == "" {
		return Device{}, fmt.Errorf("%w: device user id", ErrIdentityFieldRequired)
	}
	if strings.TrimSpace(label) == "" {
		return Device{}, fmt.Errorf("%w: device label", ErrIdentityFieldRequired)
	}
	device := Device{
		ID:        uuid.NewString(),
		UserID:    userID,
		Label:     strings.TrimSpace(label),
		Class:     class,
		Platform:  platform,
		Channel:   channel,
		CreatedAt: nowMillis(),
	}
	if _, err := s.db.Exec(
		`INSERT INTO devices (id, user_id, label, class, platform, key_thumbprint,
			passkey_credential_id, channel, created_at, last_seen_at, revoked_at)
		 VALUES (?, ?, ?, ?, ?, NULL, NULL, ?, ?, 0, NULL)`,
		device.ID, device.UserID, device.Label, device.Class, device.Platform,
		device.Channel, device.CreatedAt,
	); err != nil {
		// A concurrent boot won the unique index. Its row is as good as
		// ours would have been, so read it rather than reporting a
		// conflict nobody can act on.
		if raced, readErr := s.deviceByChannel(channel); readErr == nil {
			return raced, nil
		}
		return Device{}, fmt.Errorf("store: ensure channel device: %w", err)
	}
	return device, nil
}

func (s *Store) deviceByChannel(channel string) (Device, error) {
	return scanDevice(s.reader().QueryRow(
		`SELECT `+deviceColumns+` FROM devices WHERE channel = ? AND channel <> ''`, channel))
}

// CreatePairedDevice registers a device that already holds a key, writing
// the thumbprint in the SAME statement as the row.
//
// Separate from CreateDevice + SetDeviceKeyThumbprint because those two are
// two writes with no transaction around them: a thumbprint that loses the
// unique index to a concurrent redemption would leave a device row nothing
// created it for. One INSERT means the conflict refuses the whole thing.
func (s *Store) CreatePairedDevice(userID, label, class, platform, thumbprint string) (Device, error) {
	if strings.TrimSpace(userID) == "" {
		return Device{}, fmt.Errorf("%w: device user id", ErrIdentityFieldRequired)
	}
	if strings.TrimSpace(label) == "" {
		return Device{}, fmt.Errorf("%w: device label", ErrIdentityFieldRequired)
	}
	if strings.TrimSpace(thumbprint) == "" {
		return Device{}, fmt.Errorf("%w: device key thumbprint", ErrIdentityFieldRequired)
	}
	device := Device{
		ID:            uuid.NewString(),
		UserID:        userID,
		Label:         strings.TrimSpace(label),
		Class:         class,
		Platform:      platform,
		KeyThumbprint: thumbprint,
		CreatedAt:     nowMillis(),
	}
	if _, err := s.db.Exec(
		`INSERT INTO devices (id, user_id, label, class, platform, key_thumbprint,
			passkey_credential_id, channel, created_at, last_seen_at, revoked_at)
		 VALUES (?, ?, ?, ?, ?, ?, NULL, '', ?, 0, NULL)`,
		device.ID, device.UserID, device.Label, device.Class, device.Platform,
		device.KeyThumbprint, device.CreatedAt,
	); err != nil {
		return Device{}, fmt.Errorf("store: create paired device: %w", err)
	}
	return device, nil
}

// DeviceByKeyThumbprint resolves a device from the public key it presented.
// sql.ErrNoRows when no device holds it, which is the ordinary answer for a
// device pairing for the first time.
func (s *Store) DeviceByKeyThumbprint(thumbprint string) (Device, error) {
	if strings.TrimSpace(thumbprint) == "" {
		return Device{}, sql.ErrNoRows
	}
	return scanDevice(s.reader().QueryRow(
		`SELECT `+deviceColumns+` FROM devices
		  WHERE key_thumbprint = ? AND key_thumbprint IS NOT NULL`, thumbprint))
}

// RelabelDevice rewrites the label and platform a device reports for
// itself. Called when a known key re-pairs: the machine may have been
// renamed or moved to another OS since, and the row should say what the
// device says.
func (s *Store) RelabelDevice(deviceID, label, platform string) error {
	if strings.TrimSpace(label) == "" {
		return fmt.Errorf("%w: device label", ErrIdentityFieldRequired)
	}
	result, err := s.db.Exec(
		`UPDATE devices SET label = ?, platform = ? WHERE id = ?`,
		strings.TrimSpace(label), platform, deviceID)
	if err != nil {
		return fmt.Errorf("store: relabel device: %w", err)
	}
	return requireRowsAffected(result, "store: relabel device")
}

// GetDevice reads one device by id. sql.ErrNoRows when it does not exist.
func (s *Store) GetDevice(id string) (Device, error) {
	return scanDevice(s.reader().QueryRow(
		`SELECT `+deviceColumns+` FROM devices WHERE id = ?`, id))
}

// ListDevicesForUser returns one account's devices, oldest first,
// including revoked ones — the device list is also the revocation history
// a person reads to check that a lost laptop really is off.
func (s *Store) ListDevicesForUser(userID string) ([]Device, error) {
	rows, err := s.reader().Query(
		`SELECT `+deviceColumns+` FROM devices WHERE user_id = ? ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: list devices: %w", err)
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		device, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, device)
	}
	return out, rows.Err()
}

// SetDeviceKeyThumbprint records the device's public-key thumbprint. The
// column is uniquely indexed, so a thumbprint already naming another
// device is refused by SQLite rather than silently moved.
func (s *Store) SetDeviceKeyThumbprint(deviceID, thumbprint string) error {
	if strings.TrimSpace(thumbprint) == "" {
		return fmt.Errorf("%w: device key thumbprint", ErrIdentityFieldRequired)
	}
	result, err := s.db.Exec(
		`UPDATE devices SET key_thumbprint = ? WHERE id = ?`, thumbprint, deviceID)
	if err != nil {
		return fmt.Errorf("store: set device key thumbprint: %w", err)
	}
	return requireRowsAffected(result, "store: set device key thumbprint")
}

// SetDevicePasskeyCredential records the device's passkey credential id,
// under the same uniqueness rule as the key thumbprint.
func (s *Store) SetDevicePasskeyCredential(deviceID, credentialID string) error {
	if strings.TrimSpace(credentialID) == "" {
		return fmt.Errorf("%w: device passkey credential id", ErrIdentityFieldRequired)
	}
	result, err := s.db.Exec(
		`UPDATE devices SET passkey_credential_id = ? WHERE id = ?`, credentialID, deviceID)
	if err != nil {
		return fmt.Errorf("store: set device passkey credential: %w", err)
	}
	return requireRowsAffected(result, "store: set device passkey credential")
}

// TouchDevice advances last_seen_at, reporting whether it moved. The
// change predicate keeps a repeat presentation within the same
// millisecond from writing a page.
func (s *Store) TouchDevice(deviceID string, at int64) (bool, error) {
	result, err := s.db.Exec(
		`UPDATE devices SET last_seen_at = ? WHERE id = ? AND last_seen_at IS NOT ?`,
		at, deviceID, at)
	if err != nil {
		return false, fmt.Errorf("store: touch device: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: touch device: rows affected: %w", err)
	}
	return rows > 0, nil
}

// RevokeDevice marks a device revoked and revokes every session it still
// holds, in ONE transaction, returning the session ids that moved.
//
// The two halves cannot be separate calls. A device marked revoked whose
// sessions are still live is a credential that keeps working, and a caller
// that has to remember the second write is one forgotten call site away
// from exactly that. The returned ids are what the live-session registry
// force-closes, so a partial write cannot leave a connection open under a
// credential the database says is dead.
//
// Returns nil ids when the device was already revoked.
func (s *Store) RevokeDevice(deviceID string, at int64) ([]string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: revoke device: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.Exec(
		`UPDATE devices SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, at, deviceID)
	if err != nil {
		return nil, fmt.Errorf("store: revoke device: %w", err)
	}
	moved, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("store: revoke device: rows affected: %w", err)
	}
	if moved == 0 {
		// Already revoked, or no such device. Either way nothing to close:
		// a prior revocation already closed what it found.
		return nil, nil
	}

	rows, err := tx.Query(
		`UPDATE sessions SET revoked_at = ? WHERE device_id = ? AND revoked_at IS NULL
		 RETURNING id`, at, deviceID)
	if err != nil {
		return nil, fmt.Errorf("store: revoke device sessions: %w", err)
	}
	var sessionIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: revoke device sessions: %w", err)
		}
		sessionIDs = append(sessionIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: revoke device sessions: %w", err)
	}
	rows.Close()

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: revoke device: commit: %w", err)
	}
	return sessionIDs, nil
}

// RestoreDevice clears a device's revocation, reporting whether a row
// moved. Only the device row: its sessions stay revoked, because a
// restore re-admits the KEY to pairing, never a credential that was
// withdrawn — the device still has to redeem a fresh owner-minted link
// and pass the verification number to hold one again.
func (s *Store) RestoreDevice(deviceID string) (bool, error) {
	result, err := s.db.Exec(
		`UPDATE devices SET revoked_at = NULL WHERE id = ? AND revoked_at IS NOT NULL`, deviceID)
	if err != nil {
		return false, fmt.Errorf("store: restore device: %w", err)
	}
	moved, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: restore device: rows affected: %w", err)
	}
	return moved > 0, nil
}

func scanDevice(sc interface{ Scan(...any) error }) (Device, error) {
	var device Device
	var thumbprint, passkey sql.NullString
	var revokedAt sql.NullInt64
	if err := sc.Scan(
		&device.ID, &device.UserID, &device.Label, &device.Class, &device.Platform,
		&thumbprint, &passkey, &device.Channel, &device.CreatedAt, &device.LastSeenAt,
		&revokedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Device{}, err
		}
		return Device{}, fmt.Errorf("store: scan device: %w", err)
	}
	device.KeyThumbprint = thumbprint.String
	device.PasskeyCredentialID = passkey.String
	device.RevokedAt = revokedAt.Int64
	return device, nil
}

// CreateSession writes a session row. The caller owns the id, because the
// same id is signed into the claims that travel with it and the two must
// be minted together.
//
// Refused rather than defaulted: an empty id, user, device, or binding
// class, and an expiry at or before creation. A session that is already
// expired is not a session; accepting one would put a row in the table
// that nothing could ever present.
func (s *Store) CreateSession(session Session) error {
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
	case session.ExpiresAt <= session.CreatedAt:
		return fmt.Errorf("store: create session: expiry %d is not after creation %d",
			session.ExpiresAt, session.CreatedAt)
	}
	scopes, err := encodeScopes(session.Scopes)
	if err != nil {
		return err
	}
	activatedAt := sql.NullInt64{Int64: session.ActivatedAt, Valid: session.ActivatedAt != 0}
	if _, err := s.db.Exec(
		`INSERT INTO sessions (id, user_id, device_id, binding_class, scopes,
			signing_key_id, created_at, expires_at, revoked_at, last_seen_at, activated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, 0, ?)`,
		session.ID, session.UserID, session.DeviceID, session.BindingClass, scopes,
		session.SigningKeyID, session.CreatedAt, session.ExpiresAt, activatedAt,
	); err != nil {
		return fmt.Errorf("store: create session: %w", err)
	}
	return nil
}

// ActivateSession stamps the moment a session became presentable, which is
// the moment the owner confirmed the pairing verification number. Reports
// whether it moved: a second confirmation keeps the first stamp, so the
// log records when access actually began.
//
// Scoped to unactivated rows that are still inside their window and not
// revoked. A revoked session must not be activatable — that would be a
// revocation a confirmation undoes — and neither must a lapsed one: the
// pending window IS the deadline on the confirmation, so accepting one
// after it would make the deadline decorative.
func (s *Store) ActivateSession(sessionID string, at, expiresAt int64) (bool, error) {
	if at == 0 {
		return false, fmt.Errorf("%w: session activation stamp", ErrIdentityFieldRequired)
	}
	result, err := s.db.Exec(
		`UPDATE sessions SET activated_at = ?, expires_at = ?
		 WHERE id = ? AND activated_at IS NULL AND revoked_at IS NULL AND expires_at > ?`,
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

// ExtendSession moves a live session's expiry forward, reporting whether
// it moved. This is what a refresh rotation writes: the access window is
// the row's expiry, so renewing one means moving the other.
//
// Never shortens and never resurrects: the predicate requires the session
// to be live and the new expiry to be later than the one it holds, so a
// replayed or reordered renewal cannot cut a window short.
func (s *Store) ExtendSession(sessionID string, expiresAt, now int64) (bool, error) {
	result, err := s.db.Exec(
		`UPDATE sessions SET expires_at = ?
		 WHERE id = ? AND revoked_at IS NULL AND activated_at IS NOT NULL
		   AND expires_at > ? AND expires_at < ?`,
		expiresAt, sessionID, now, expiresAt)
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
// it safe: a session past its expiry can never admit a presentation again,
// so removing it takes nothing away from anybody. The caller keeps a
// generous margin so the device list can still show recent history.
// Revoked-but-unexpired rows are deliberately NOT covered — they are the
// evidence that a revocation happened.
func (s *Store) DeleteSessionsExpiredBefore(before int64) (int64, error) {
	result, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, before)
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
	return scanSession(s.reader().QueryRow(
		`SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, id))
}

// ListSessionsForDevice returns a device's sessions, newest first,
// revoked and expired ones included.
func (s *Store) ListSessionsForDevice(deviceID string) ([]Session, error) {
	rows, err := s.reader().Query(
		`SELECT `+sessionColumns+` FROM sessions WHERE device_id = ? ORDER BY created_at DESC, id`,
		deviceID)
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
		`SELECT `+sessionColumns+` FROM sessions
		 WHERE revoked_at IS NULL AND activated_at IS NOT NULL AND expires_at > ?
		 ORDER BY created_at DESC, id`, now)
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
// it moved. Deliberately scoped to live rows: a revoked session that keeps
// being presented must not look freshly used in the device list.
func (s *Store) TouchSession(sessionID string, at int64) (bool, error) {
	result, err := s.db.Exec(
		`UPDATE sessions SET last_seen_at = ?
		 WHERE id = ? AND revoked_at IS NULL AND last_seen_at IS NOT ?`,
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
	var revokedAt, activatedAt sql.NullInt64
	if err := sc.Scan(
		&session.ID, &session.UserID, &session.DeviceID, &session.BindingClass, &scopes,
		&session.SigningKeyID, &session.CreatedAt, &session.ExpiresAt, &revokedAt,
		&session.LastSeenAt, &activatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Session{}, err
		}
		return Session{}, fmt.Errorf("store: scan session: %w", err)
	}
	session.RevokedAt = revokedAt.Int64
	session.ActivatedAt = activatedAt.Int64
	decoded, err := decodeScopes(scopes)
	if err != nil {
		return Session{}, err
	}
	session.Scopes = decoded
	return session, nil
}

// encodeScopes renders a scope set as the JSON array the column holds. A
// nil or empty set encodes as `[]`, never as `null` or the empty string,
// so "this session was granted nothing" has exactly one spelling and a
// reader never has to branch on which.
func encodeScopes(scopes []string) (string, error) {
	if len(scopes) == 0 {
		return "[]", nil
	}
	buf, err := json.Marshal(scopes)
	if err != nil {
		return "", fmt.Errorf("store: encode session scopes: %w", err)
	}
	return string(buf), nil
}

// decodeScopes is strict. A blob that does not decode is an error, not an
// empty grant: silently reading a corrupt scope set as "no scopes" would
// turn a storage fault into a permissions answer, and a caller cannot tell
// the difference.
func decodeScopes(raw string) ([]string, error) {
	if raw == "" {
		return nil, fmt.Errorf("store: decode session scopes: empty blob")
	}
	var scopes []string
	if err := json.Unmarshal([]byte(raw), &scopes); err != nil {
		return nil, fmt.Errorf("store: decode session scopes: %w", err)
	}
	return scopes, nil
}

// InsertSigningKey persists one HMAC secret.
func (s *Store) InsertSigningKey(key SigningKey) error {
	if strings.TrimSpace(key.ID) == "" {
		return fmt.Errorf("%w: signing key id", ErrIdentityFieldRequired)
	}
	if len(key.Secret) == 0 {
		return fmt.Errorf("%w: signing key secret", ErrIdentityFieldRequired)
	}
	if _, err := s.db.Exec(
		`INSERT INTO signing_keys (id, secret, created_at) VALUES (?, ?, ?)
		 ON CONFLICT(id) DO NOTHING`,
		key.ID, key.Secret, key.CreatedAt,
	); err != nil {
		return fmt.Errorf("store: insert signing key: %w", err)
	}
	return nil
}

// ActiveSigningKey returns the newest key, which is the one new claims are
// signed with. sql.ErrNoRows before first boot has minted one.
//
// Ordering breaks ties on id so two keys minted in the same millisecond
// still have one deterministic answer — otherwise two processes could
// disagree about which key is active and each would refuse the other's
// credentials.
func (s *Store) ActiveSigningKey() (SigningKey, error) {
	return scanSigningKey(s.reader().QueryRow(
		`SELECT id, secret, created_at FROM signing_keys ORDER BY created_at DESC, id DESC LIMIT 1`))
}

// SigningKeyByID reads the key a presentation names. sql.ErrNoRows means
// this backend does not hold that key, which is a refusal reason of its
// own — distinct from a signature that failed to verify.
func (s *Store) SigningKeyByID(id string) (SigningKey, error) {
	return scanSigningKey(s.reader().QueryRow(
		`SELECT id, secret, created_at FROM signing_keys WHERE id = ?`, id))
}

func scanSigningKey(sc interface{ Scan(...any) error }) (SigningKey, error) {
	var key SigningKey
	if err := sc.Scan(&key.ID, &key.Secret, &key.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SigningKey{}, err
		}
		return SigningKey{}, fmt.Errorf("store: scan signing key: %w", err)
	}
	return key, nil
}

// ReplaceRecoveryCodes writes a fresh set of hashed codes for one user and
// drops that user's unconsumed codes in the same transaction.
//
// One transaction because the two halves are one intent: re-minting must
// not leave the previous set usable, and a crash between the delete and
// the insert must not leave an account with no way back in. Consumed rows
// survive — they are the record that a code was spent, which is what makes
// a replay visibly a replay rather than an unknown code.
func (s *Store) ReplaceRecoveryCodes(userID string, hashes [][]byte, at int64) error {
	if strings.TrimSpace(userID) == "" {
		return fmt.Errorf("%w: recovery code user id", ErrIdentityFieldRequired)
	}
	if len(hashes) == 0 {
		return fmt.Errorf("%w: recovery code hashes", ErrIdentityFieldRequired)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: replace recovery codes: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		`DELETE FROM recovery_codes WHERE user_id = ? AND consumed_at IS NULL`, userID,
	); err != nil {
		return fmt.Errorf("store: replace recovery codes: %w", err)
	}
	for _, hash := range hashes {
		if len(hash) == 0 {
			return fmt.Errorf("%w: recovery code hash", ErrIdentityFieldRequired)
		}
		if _, err := tx.Exec(
			`INSERT INTO recovery_codes (id, user_id, code_hash, created_at, consumed_at, consumed_by)
			 VALUES (?, ?, ?, ?, NULL, '')`,
			uuid.NewString(), userID, hash, at,
		); err != nil {
			return fmt.Errorf("store: replace recovery codes: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: replace recovery codes: commit: %w", err)
	}
	return nil
}

// ConsumeRecoveryCode spends one code and returns the account it admits.
//
// ONE statement, so consumption is atomic against every other connection:
// the `consumed_at IS NULL` predicate is the single-use rule, and SQLite
// decides the winner. A replay of a spent code matches no row and comes
// back sql.ErrNoRows, indistinguishable from a code that never existed —
// which is the correct answer to both.
//
// The caller passes the hash, never the code: this package never sees the
// value a person typed.
func (s *Store) ConsumeRecoveryCode(hash []byte, at int64, byDevice string) (string, error) {
	if len(hash) == 0 {
		return "", fmt.Errorf("%w: recovery code hash", ErrIdentityFieldRequired)
	}
	var userID string
	err := s.db.QueryRow(
		`UPDATE recovery_codes SET consumed_at = ?, consumed_by = ?
		 WHERE code_hash = ? AND consumed_at IS NULL
		 RETURNING user_id`,
		at, byDevice, hash,
	).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if err != nil {
		return "", fmt.Errorf("store: consume recovery code: %w", err)
	}
	return userID, nil
}

// CountRecoveryCodes reports how many codes an account has EVER been
// issued, spent ones included. It is the question first boot asks: an
// account with zero rows has never been given a set, while an account
// whose codes are all spent has, and must not be silently re-minted a set
// nobody was shown.
func (s *Store) CountRecoveryCodes(userID string) (int, error) {
	var count int
	if err := s.reader().QueryRow(
		`SELECT COUNT(*) FROM recovery_codes WHERE user_id = ?`, userID,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("store: count recovery codes: %w", err)
	}
	return count, nil
}

// CountUnspentRecoveryCodes reports how many codes an account has left, so
// a surface can tell someone to re-mint before they run out.
func (s *Store) CountUnspentRecoveryCodes(userID string) (int, error) {
	var count int
	if err := s.reader().QueryRow(
		`SELECT COUNT(*) FROM recovery_codes WHERE user_id = ? AND consumed_at IS NULL`,
		userID,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("store: count unspent recovery codes: %w", err)
	}
	return count, nil
}

// AppendAuthAudit records one credential event and returns its row id.
//
// Every append is a fact about something that already happened, so this
// never refuses on the strength of the payload — an event naming a device
// that no longer exists is exactly what the log is for. It refuses only an
// empty event kind or an outcome outside the CHECK'd pair, both of which
// would make the row unreadable rather than merely sparse.
func (s *Store) AppendAuthAudit(entry AuthAuditEntry) (int64, error) {
	if strings.TrimSpace(entry.Event) == "" {
		return 0, fmt.Errorf("%w: auth audit event", ErrIdentityFieldRequired)
	}
	if entry.Outcome != AuthAuditOutcomeAllowed && entry.Outcome != AuthAuditOutcomeRefused {
		return 0, fmt.Errorf("store: append auth audit: outcome %q is not allowed/refused", entry.Outcome)
	}
	if entry.At == 0 {
		entry.At = nowMillis()
	}
	result, err := s.db.Exec(
		`INSERT INTO auth_audit (at, event, outcome, reason, user_id, device_id,
			session_id, peer, detail)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.At, entry.Event, entry.Outcome, entry.Reason, entry.UserID, entry.DeviceID,
		entry.SessionID, entry.Peer, entry.Detail,
	)
	if err != nil {
		return 0, fmt.Errorf("store: append auth audit: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: append auth audit: last insert id: %w", err)
	}
	if id%authAuditPruneEvery == 0 {
		if err := s.pruneAuthAudit(); err != nil {
			return id, err
		}
	}
	return id, nil
}

// pruneAuthAudit drops the oldest rows past maxAuthAuditRows. Keyed on the
// AUTOINCREMENT id rather than a timestamp because id order is insert
// order regardless of what any clock did, and a backwards clock jump must
// not decide which history survives.
func (s *Store) pruneAuthAudit() error {
	if _, err := s.db.Exec(
		`DELETE FROM auth_audit
		 WHERE id <= (SELECT MAX(id) FROM auth_audit) - ?`, maxAuthAuditRows,
	); err != nil {
		return fmt.Errorf("store: prune auth audit: %w", err)
	}
	return nil
}

// ListRecentAuthAudit returns the newest entries first, capped at limit.
func (s *Store) ListRecentAuthAudit(limit int) ([]AuthAuditEntry, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.reader().Query(
		`SELECT `+authAuditColumns+` FROM auth_audit ORDER BY at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list auth audit: %w", err)
	}
	return collectAuthAudit(rows, limit)
}

// ListAuthAuditForDevice returns one device's credential events, newest
// first. The partial index on (device_id, at) serves it, which is why the
// `device_id <> ”` term is repeated here: SQLite uses a partial index
// only when the query's predicates textually imply its WHERE clause.
func (s *Store) ListAuthAuditForDevice(deviceID string, limit int) ([]AuthAuditEntry, error) {
	if limit <= 0 || deviceID == "" {
		return nil, nil
	}
	rows, err := s.reader().Query(
		`SELECT `+authAuditColumns+` FROM auth_audit
		 WHERE device_id = ? AND device_id <> ''
		 ORDER BY at DESC, id DESC LIMIT ?`, deviceID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list auth audit for device: %w", err)
	}
	return collectAuthAudit(rows, limit)
}

// collectAuthAudit drains one audit query. Both list paths share it so
// the column order in authAuditColumns has exactly one Scan to agree with.
func collectAuthAudit(rows *sql.Rows, limit int) ([]AuthAuditEntry, error) {
	defer rows.Close()
	out := make([]AuthAuditEntry, 0, limit)
	for rows.Next() {
		var entry AuthAuditEntry
		if err := rows.Scan(
			&entry.ID, &entry.At, &entry.Event, &entry.Outcome, &entry.Reason,
			&entry.UserID, &entry.DeviceID, &entry.SessionID, &entry.Peer, &entry.Detail,
		); err != nil {
			return nil, fmt.Errorf("store: scan auth audit: %w", err)
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}
