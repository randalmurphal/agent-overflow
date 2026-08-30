package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Identity is the one-row `store_meta` table (migration v55): who this
// backend is, and which generation of its history counters a client may
// trust. See docs/architecture/thread-replica-sync.md §3.3.
//
// BackendID is minted once per store and never rewritten. It keys the
// client's replica database, so two backends on one machine never share
// cached windows.
//
// ReplicaGeneration is re-minted whenever `history_rev` / `history_epoch`
// continuity breaks for a reason the counters themselves cannot express —
// today only RestoreFrom, which replaces every row and therefore rewinds
// the counters to a snapshot's values while clients may hold stamps from
// the divergent future that snapshot was taken before. A client whose
// generation no longer matches drops its whole replica for this backend;
// it is never migrated.
type Identity struct {
	BackendID         string `json:"backendId"`
	ReplicaGeneration string `json:"replicaGeneration"`
}

// mintStoreIdentity seeds `store_meta`'s single row. Runs as migration
// v55's Fix hook (same transaction as the DDL) because SQLite has no uuid
// generator, and every other id in this app is a google/uuid string.
func mintStoreIdentity(tx *sql.Tx) error {
	if _, err := tx.Exec(
		`INSERT INTO store_meta (id, backend_id, replica_generation) VALUES (1, ?, ?)`,
		uuid.NewString(), uuid.NewString(),
	); err != nil {
		return fmt.Errorf("store: mint store identity: %w", err)
	}
	return nil
}

// Identity returns the store's backend id and current replica generation.
func (s *Store) Identity() (Identity, error) {
	return identityFrom(s.reader())
}

func identityFrom(q sqlQueryer) (Identity, error) {
	var id Identity
	if err := q.QueryRow(
		`SELECT backend_id, replica_generation FROM store_meta WHERE id = 1`,
	).Scan(&id.BackendID, &id.ReplicaGeneration); err != nil {
		return Identity{}, fmt.Errorf("store: read store identity: %w", err)
	}
	return id, nil
}

// remintStoreIdentityTx writes a fresh replica generation under the
// caller-supplied backend id, inside a caller-owned transaction. The
// caller is whoever just made the history counters discontinuous.
//
// backendID is a REQUIRED parameter rather than something read here
// because the only caller (RestoreFrom) has to capture it before the
// operation that would destroy it — the whole-database row copy, which
// overwrites store_meta with the snapshot's row. Taking it as an
// argument is what makes "which store is this?" a question the caller
// has already answered by the time the identity is rewritten, instead of
// one this function could answer wrongly from post-copy state.
//
// It writes BOTH columns: an UPDATE that touched only the generation
// would leave whatever backend id the copy restored, which is the bug
// this signature exists to prevent. An UPSERT rather than an UPDATE
// because a restore source that predates v55 carries no store_meta row,
// and the restore must not fail over the one table whose whole job is
// describing discontinuity.
func remintStoreIdentityTx(exec sqlExecutor, backendID, label string) error {
	if backendID == "" {
		return fmt.Errorf("%s: backend id is required to re-mint store identity", label)
	}
	if _, err := exec.Exec(
		`INSERT INTO store_meta (id, backend_id, replica_generation) VALUES (1, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		     backend_id         = excluded.backend_id,
		     replica_generation = excluded.replica_generation`,
		backendID, uuid.NewString(),
	); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	return nil
}

// backendIDForRestore reads this store's own backend id ahead of a
// whole-database replace. A store opened through New always has the row
// (ensureStoreIdentity runs at open), so the missing-row branch covers
// only hand-surgery cases; it mints a fresh id rather than failing,
// which is the same "a missing row IS discontinuity" posture the rest of
// this file takes — and still never adopts the snapshot's.
func (s *Store) backendIDForRestore() (string, error) {
	var backendID string
	err := s.db.QueryRow(`SELECT backend_id FROM store_meta WHERE id = 1`).Scan(&backendID)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.NewString(), nil
	}
	if err != nil {
		return "", fmt.Errorf("store: read backend id before restore: %w", err)
	}
	if backendID == "" {
		return uuid.NewString(), nil
	}
	return backendID, nil
}

// ensureStoreIdentity self-heals a missing store_meta row at open time
// (INSERT OR IGNORE — a present row wins). Every v55 store has the row;
// this exists so the exotic ways of losing it (a restore from an ancient
// snapshot, hand surgery on the DB file) degrade to "all replicas drop"
// instead of an unreadable identity for the life of the store.
func ensureStoreIdentity(db *sql.DB) error {
	if _, err := db.Exec(
		`INSERT OR IGNORE INTO store_meta (id, backend_id, replica_generation) VALUES (1, ?, ?)`,
		uuid.NewString(), uuid.NewString(),
	); err != nil {
		return fmt.Errorf("store: ensure store identity: %w", err)
	}
	return nil
}
