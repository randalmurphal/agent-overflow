package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"

	"agent-overflow/internal/entityid"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Tracker kinds (tracker_connections.kind, thread_tickets.kind). Tempo is a
// connection only: it has no issues of its own, so no ticket names it.
const (
	TrackerKindJira   = "jira"
	TrackerKindTempo  = "tempo"
	TrackerKindLinear = "linear"
)

// ErrTrackerConnectionExists is returned by CreateTrackerConnection for a
// Jira or Linear connection whose (kind, site, account) another connection
// has, or a Tempo connection for a Jira connection that already has one.
var ErrTrackerConnectionExists = errors.New("store: tracker connection already exists")

// ErrInvalidTrackerConnection is returned by CreateTrackerConnection and
// RecordTrackerConnectionVerification for a value the table refuses: an
// unknown kind, an empty site, an account on a Tempo or Linear connection
// or none on a Jira one, a parent on a connection that is not Tempo, or a
// Tempo connection whose parent is missing or not a Jira connection.
var ErrInvalidTrackerConnection = errors.New("store: invalid tracker connection")

// TrackerConnection is one credential for one site (migration v162). Site is
// the Jira site URL, the Tempo regional API URL, or the Linear workspace;
// Account is the Jira account email and empty otherwise. ParentID names the
// Jira connection a Tempo connection belongs to and is empty otherwise.
// VerifiedAccountID and VerifiedName are who the token belongs to as of
// VerifiedAt (unix milliseconds, 0 until verified); VerifiedToken is the
// digest of the token that verification checked, zero when it is not
// known. TokenExpiresAt is when
// the user said the token expires, 0 when they have not; no tracker reports
// it.
type TrackerConnection struct {
	ID                string
	Kind              string
	Site              string
	Account           string
	ParentID          string
	Label             string
	Enabled           bool
	VerifiedAccountID string
	VerifiedName      string
	VerifiedAt        int64
	VerifiedToken     TrackerTokenDigest
	TokenExpiresAt    int64
	CreatedAt         int64
	// TimeSourceGen is a Jira connection's time source generation: its
	// store triggers (migration v168) advance it each time the source of
	// its worklogs changes (its Tempo connection added or removed while
	// on, or turned on or off), and nothing else writes it. A worklog
	// replace names the generation its read began under
	// (ReplaceMirrorWorklogs).
	TimeSourceGen int64
}

// NewTrackerConnection is what CreateTrackerConnection needs; the store
// mints the ID, and a new connection is enabled and unverified.
type NewTrackerConnection struct {
	Kind      string
	Site      string
	Account   string
	ParentID  string
	Label     string
	CreatedAt int64
}

// TrackerVerification is the answer of one identity call to the tracker.
// AccountID and Name may be empty for a tracker whose token check names
// nobody. Token is the digest of the token the call checked, zero when it
// is not known.
type TrackerVerification struct {
	AccountID  string
	Name       string
	VerifiedAt int64
	Token      TrackerTokenDigest
}

// TrackerTokenDigest is the SHA-256 digest of a tracker token
// (trackerapp's tokenDigest), stored as tracker_connections.verified_token:
// 32 bytes, or none for the zero value. It formats as a placeholder, so no
// log or error that prints a row carries it.
type TrackerTokenDigest [32]byte

// Known reports whether d names a token.
func (d TrackerTokenDigest) Known() bool { return d != TrackerTokenDigest{} }

// Format writes a placeholder whatever the verb.
func (d TrackerTokenDigest) Format(f fmt.State, _ rune) {
	if d.Known() {
		_, _ = io.WriteString(f, "TrackerTokenDigest(set)")
		return
	}
	_, _ = io.WriteString(f, "TrackerTokenDigest(none)")
}

// Value stores the digest, or an empty blob for the zero value.
func (d TrackerTokenDigest) Value() (driver.Value, error) {
	if !d.Known() {
		return []byte{}, nil
	}
	return d[:], nil
}

// Scan reads an empty blob as the zero value and a 32-byte one as itself.
func (d *TrackerTokenDigest) Scan(src any) error {
	b, ok := src.([]byte)
	switch {
	case !ok:
		return fmt.Errorf("store: a verified token digest is %T, not a blob", src)
	case len(b) == 0:
		*d = TrackerTokenDigest{}
	case len(b) == len(d):
		copy(d[:], b)
	default:
		return fmt.Errorf("store: a verified token digest is %d bytes, not %d", len(b), len(d))
	}
	return nil
}

const trackerConnectionColumns = `id, kind, site, account, COALESCE(parent_id, ''), label, enabled,
 verified_account_id, verified_name, verified_at, verified_token, token_expires_at, created_at, time_source_gen`

func scanTrackerConnection(scan func(...any) error) (TrackerConnection, error) {
	var c TrackerConnection
	err := scan(&c.ID, &c.Kind, &c.Site, &c.Account, &c.ParentID, &c.Label, &c.Enabled,
		&c.VerifiedAccountID, &c.VerifiedName, &c.VerifiedAt, &c.VerifiedToken, &c.TokenExpiresAt, &c.CreatedAt, &c.TimeSourceGen)
	return c, err
}

// validate checks what the table's CHECKs check, so a refusal names the
// rule; the parent's kind is checked against its row inside the insert's
// transaction.
func (c NewTrackerConnection) validate() error {
	switch c.Kind {
	case TrackerKindJira, TrackerKindTempo, TrackerKindLinear:
	default:
		return fmt.Errorf("%w: unknown kind %q", ErrInvalidTrackerConnection, c.Kind)
	}
	switch {
	case c.Site == "":
		return fmt.Errorf("%w: a %s connection needs a site", ErrInvalidTrackerConnection, c.Kind)
	case (c.Kind == TrackerKindJira) != (c.Account != ""):
		return fmt.Errorf("%w: a Jira connection names its account and no other kind does", ErrInvalidTrackerConnection)
	case (c.Kind == TrackerKindTempo) != (c.ParentID != ""):
		return fmt.Errorf("%w: a Tempo connection names its Jira connection and no other kind does", ErrInvalidTrackerConnection)
	case c.CreatedAt <= 0:
		return fmt.Errorf("%w: creation time is required", ErrInvalidTrackerConnection)
	}
	return nil
}

// CreateTrackerConnection inserts a connection with a new ID and returns the
// row as stored. A duplicate fails with ErrTrackerConnectionExists and an
// invalid value with ErrInvalidTrackerConnection; the parent of a Tempo
// connection is checked to be a Jira connection in the insert's
// transaction.
func (s *Store) CreateTrackerConnection(c NewTrackerConnection) (created TrackerConnection, err error) {
	if err := c.validate(); err != nil {
		return TrackerConnection{}, err
	}
	const action = "store: create tracker connection"
	tx, err := s.db.Begin()
	if err != nil {
		return TrackerConnection{}, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	if c.ParentID != "" {
		var parentKind string
		err := tx.QueryRow(`SELECT kind FROM tracker_connections WHERE id = ?`, c.ParentID).Scan(&parentKind)
		if errors.Is(err, sql.ErrNoRows) {
			return TrackerConnection{}, fmt.Errorf("%w: parent connection %s does not exist", ErrInvalidTrackerConnection, c.ParentID)
		}
		if err != nil {
			return TrackerConnection{}, fmt.Errorf("%s: read parent %s: %w", action, c.ParentID, err)
		}
		if parentKind != TrackerKindJira {
			return TrackerConnection{}, fmt.Errorf("%w: parent connection %s is %s, not jira", ErrInvalidTrackerConnection, c.ParentID, parentKind)
		}
	}
	created, err = scanTrackerConnection(tx.QueryRow(`INSERT INTO tracker_connections
		(id, kind, site, account, parent_id, label, created_at) VALUES (?, ?, ?, ?, NULLIF(?, ''), ?, ?)
		RETURNING `+trackerConnectionColumns,
		entityid.New(), c.Kind, c.Site, c.Account, c.ParentID, c.Label, c.CreatedAt).Scan)
	var engine *sqlite.Error
	if errors.As(err, &engine) && engine.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE {
		// The table's only UNIQUE constraints are its two identity indexes.
		return TrackerConnection{}, fmt.Errorf("%w: %s %s %s", ErrTrackerConnectionExists, c.Kind, c.Site, c.Account)
	}
	if err != nil {
		return TrackerConnection{}, fmt.Errorf("%s: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return TrackerConnection{}, fmt.Errorf("%s: commit: %w", action, err)
	}
	return created, nil
}

// GetTrackerConnection reads one connection; a missing one fails with
// sql.ErrNoRows.
func (s *Store) GetTrackerConnection(id string) (TrackerConnection, error) {
	c, err := scanTrackerConnection(s.reader().QueryRow(`SELECT `+trackerConnectionColumns+`
		FROM tracker_connections WHERE id = ?`, id).Scan)
	if err != nil {
		return TrackerConnection{}, fmt.Errorf("store: get tracker connection %s: %w", id, err)
	}
	return c, nil
}

// ListTrackerConnections lists every connection in the order they were
// created.
func (s *Store) ListTrackerConnections() ([]TrackerConnection, error) {
	rows, err := s.reader().QueryContext(context.Background(), `SELECT `+trackerConnectionColumns+`
		FROM tracker_connections ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list tracker connections: %w", err)
	}
	var out []TrackerConnection
	for rows.Next() {
		c, err := scanTrackerConnection(rows.Scan)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("store: scan tracker connection: %w", err), rows.Close())
		}
		out = append(out, c)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("store: list tracker connections: %w", err)
	}
	return out, nil
}

// SetTrackerConnectionLabel renames a connection. It returns the row and
// whether it changed; a missing connection fails with sql.ErrNoRows.
func (s *Store) SetTrackerConnectionLabel(id, label string) (TrackerConnection, bool, error) {
	return commitRowWrite(s, rowWrite{
		Action:     fmt.Sprintf("store: set tracker connection %s label", id),
		ID:         id,
		Set:        "label = ?",
		SetArgs:    []any{label},
		Change:     "label IS NOT ?",
		ChangeArgs: []any{label},
	}, applyTrackerConnectionRowWriteTx)
}

// SetTrackerConnectionEnabled turns a connection on or off. It returns the
// row and whether it changed; a missing connection fails with
// sql.ErrNoRows.
func (s *Store) SetTrackerConnectionEnabled(id string, enabled bool) (TrackerConnection, bool, error) {
	value := boolToInt(enabled)
	return commitRowWrite(s, rowWrite{
		Action:     fmt.Sprintf("store: set tracker connection %s enabled", id),
		ID:         id,
		Set:        "enabled = ?",
		SetArgs:    []any{value},
		Change:     "enabled IS NOT ?",
		ChangeArgs: []any{value},
	}, applyTrackerConnectionRowWriteTx)
}

// RecordTrackerConnectionVerification records who the connection's token
// belongs to and when that was checked; the token's expiry is the user's
// (SetTrackerConnectionTokenExpiry). It returns the row and whether it
// changed; a missing connection fails with sql.ErrNoRows.
func (s *Store) RecordTrackerConnectionVerification(id string, v TrackerVerification) (TrackerConnection, bool, error) {
	if v.VerifiedAt <= 0 {
		return TrackerConnection{}, false, fmt.Errorf("%w: a verification needs its time", ErrInvalidTrackerConnection)
	}
	values := []any{v.AccountID, v.Name, v.VerifiedAt, v.Token}
	return commitRowWrite(s, rowWrite{
		Action:     fmt.Sprintf("store: record tracker connection %s verification", id),
		ID:         id,
		Set:        "verified_account_id = ?, verified_name = ?, verified_at = ?, verified_token = ?",
		SetArgs:    values,
		Change:     "(verified_account_id IS NOT ? OR verified_name IS NOT ? OR verified_at IS NOT ? OR verified_token IS NOT ?)",
		ChangeArgs: values,
	}, applyTrackerConnectionRowWriteTx)
}

// SetTrackerConnectionTokenExpiry records when the user said the
// connection's token expires (unix milliseconds); 0 clears it. It returns
// the row and whether it changed; a negative time fails with
// ErrInvalidTrackerConnection and a missing connection with sql.ErrNoRows.
func (s *Store) SetTrackerConnectionTokenExpiry(id string, expiresAt int64) (TrackerConnection, bool, error) {
	if expiresAt < 0 {
		return TrackerConnection{}, false, fmt.Errorf("%w: a token expiry cannot be negative", ErrInvalidTrackerConnection)
	}
	return commitRowWrite(s, rowWrite{
		Action:     fmt.Sprintf("store: set tracker connection %s token expiry", id),
		ID:         id,
		Set:        "token_expires_at = ?",
		SetArgs:    []any{expiresAt},
		Change:     "token_expires_at IS NOT ?",
		ChangeArgs: []any{expiresAt},
	}, applyTrackerConnectionRowWriteTx)
}

// DeleteTrackerConnection deletes a connection with the Tempo connection
// that belongs to it, and with them the boards projects list through them.
// It returns the IDs it deleted, the connection's first, so the caller can
// delete their key files; none when the connection did not exist. The
// connection's mirror root is detached in the same statement (ON DELETE
// SET NULL), and its read clock removed; FinishMirrorCleanup deletes its
// rows.
func (s *Store) DeleteTrackerConnection(id string) (deleted []string, err error) {
	action := fmt.Sprintf("store: delete tracker connection %s", id)
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	children, err := queryIDs(tx, `SELECT id FROM tracker_connections WHERE parent_id = ? ORDER BY id`, id)
	if err != nil {
		return nil, fmt.Errorf("%s: list the connections it owns: %w", action, err)
	}
	var root sql.NullInt64
	if err := tx.QueryRow(`SELECT id FROM tracker_cache_roots WHERE connection_id = ?`, id).Scan(&root); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%s: read its mirror root: %w", action, err)
	}
	gone, err := queryIDs(tx, `DELETE FROM tracker_connections WHERE id = ? RETURNING id`, id)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	if len(gone) == 0 {
		return nil, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("%s: commit: %w", action, err)
	}
	if root.Valid {
		s.forgetMirrorRead(root.Int64)
	}
	return append(gone, children...), nil
}

// applyTrackerConnectionRowWriteTx is the tracker_connections entry point of
// applyRowWriteTx: it names the table and its read-back projection. A write
// that changed nothing returns the row as stored, read in the write's
// transaction, so every setter returns the current row with its flag.
func applyTrackerConnectionRowWriteTx(tx *sql.Tx, write rowWrite) (TrackerConnection, bool, error) {
	write.table = "tracker_connections"
	readBack := func(tx *sql.Tx, id string) (TrackerConnection, error) {
		return scanTrackerConnection(tx.QueryRow(`SELECT `+trackerConnectionColumns+` FROM tracker_connections WHERE id = ?`, id).Scan)
	}
	row, changed, err := applyRowWriteTx(tx, write, readBack)
	if err != nil || changed {
		return row, changed, err
	}
	if row, err = readBack(tx, write.ID); err != nil {
		return TrackerConnection{}, false, fmt.Errorf("%s: read unchanged row: %w", write.Action, err)
	}
	return row, false, nil
}
