package store

import (
	"fmt"
	"os"
	"strings"
)

// SnapshotTo writes a consistent single-file copy of the live database
// to destPath via VACUUM INTO — online-safe under WAL, no lock dance,
// and the output has no sidecar -wal/-shm files. Fails if destPath
// already exists (SQLite refuses to overwrite; callers pick fresh
// names).
//
// Built for the agent test harness (recording bundles, save/restore
// points) but provider-agnostic: it copies whatever schema version the
// live DB is on.
func (s *Store) SnapshotTo(destPath string) error {
	if destPath == "" {
		return fmt.Errorf("store: snapshot destination must be non-empty")
	}
	if _, err := s.db.Exec("VACUUM INTO ?", destPath); err != nil {
		return fmt.Errorf("store: vacuum into %s: %w", destPath, err)
	}
	return nil
}

// RestoreFrom replaces every row in the live database with the
// contents of the snapshot file at srcPath, without closing the live
// connection (the rest of the app keeps its *Store pointer; the result
// is equivalent to having booted against the snapshot).
//
// Version drift is handled by migrating a scratch copy of the snapshot
// first: the file is copied, opened through New (which runs the
// forward-only migration chain), closed, and only then attached as the
// copy source. A snapshot newer than this binary's schema fails inside
// that migration step, which is the correct outcome.
//
// The copy itself runs in one transaction with foreign keys disabled
// (connection-scoped pragma; the store holds exactly one connection):
// delete all rows from every table, then INSERT ... SELECT from the
// attached snapshot per table. sqlite_sequence is reset so AUTOINCREMENT
// counters match the snapshot.
// It returns the post-restore Identity so the caller MUST decide where
// to publish it — the App caches identity for the bootstrap manifest,
// and a restore that skipped re-publishing would keep serving the dead
// generation for the life of the process, leaving every attached
// client's replica invalidation circuit open (the exact failure the
// generation exists to prevent).
func (s *Store) RestoreFrom(srcPath string) (identity Identity, retErr error) {
	if _, err := os.Stat(srcPath); err != nil {
		return Identity{}, fmt.Errorf("store: snapshot source: %w", err)
	}

	// Migrate a scratch copy up to this binary's schema version.
	scratch, err := migratedScratchCopy(srcPath)
	if err != nil {
		return Identity{}, err
	}
	defer os.Remove(scratch)

	tables, err := s.userTables()
	if err != nil {
		return Identity{}, err
	}

	// Read the LIVE backend id before anything touches store_meta.
	// `store_meta` is an ordinary user table, so the copy loop below
	// replaces its row wholesale with the snapshot's — and the snapshot
	// was minted by whatever store produced it, which for a harness
	// recording is not this one. backend_id names THIS database for the
	// life of the file (store_meta.go); adopting a foreign one would
	// orphan every client replica keyed to the real id and, worse, make
	// two stores claim the same identity. The re-mint below writes it
	// back.
	liveBackendID, err := s.backendIDForRestore()
	if err != nil {
		return Identity{}, err
	}

	if _, err := s.db.Exec("PRAGMA foreign_keys=OFF"); err != nil {
		return Identity{}, fmt.Errorf("store: disable foreign keys: %w", err)
	}
	defer func() {
		if _, err := s.db.Exec("PRAGMA foreign_keys=ON"); err != nil && retErr == nil {
			retErr = fmt.Errorf("store: re-enable foreign keys: %w", err)
		}
	}()

	if _, err := s.db.Exec("ATTACH DATABASE ? AS restore_src", scratch); err != nil {
		return Identity{}, fmt.Errorf("store: attach snapshot: %w", err)
	}
	defer func() {
		if _, err := s.db.Exec("DETACH DATABASE restore_src"); err != nil && retErr == nil {
			retErr = fmt.Errorf("store: detach snapshot: %w", err)
		}
	}()

	srcTables, err := attachedTables(s)
	if err != nil {
		return Identity{}, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return Identity{}, fmt.Errorf("store: begin restore: %w", err)
	}
	defer func() {
		if retErr != nil {
			if rbErr := tx.Rollback(); rbErr != nil {
				retErr = fmt.Errorf("%w (rollback: %v)", retErr, rbErr)
			}
		}
	}()

	// A prepared installation and an unsealed snapshot refer to the current
	// history/projects. Rewinding them underneath a host job would invalidate
	// a promise already made to the other computer. Completed tombstones remain
	// preserved below; pending operations must settle before a history restore.
	var transferring bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM thread_transfers WHERE phase NOT IN ('complete','canceled'))`).Scan(&transferring); err != nil {
		return Identity{}, err
	}
	if transferring {
		return Identity{}, fmt.Errorf("Finish or cancel conversation transfers before restoring history.")
	}
	var commandsRunning bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM remote_jobs WHERE state = 'running')`).Scan(&commandsRunning); err != nil {
		return Identity{}, err
	}
	if commandsRunning {
		return Identity{}, fmt.Errorf("Finish or cancel remote commands before restoring history.")
	}
	var missingOwnership bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM main.owned_threads owned
WHERE EXISTS(SELECT 1 FROM main.thread_transfers live WHERE live.thread_id = owned.id AND live.direction = 'incoming' AND live.phase = 'complete')
AND NOT EXISTS(SELECT 1 FROM restore_src.thread_transfers saved JOIN restore_src.threads history ON history.id = saved.thread_id
WHERE saved.thread_id = owned.id AND saved.direction = 'incoming' AND saved.phase = 'complete' AND saved.ownership_epoch = owned.ownership_epoch))`).Scan(&missingOwnership); err != nil {
		return Identity{}, err
	}
	if missingOwnership {
		return Identity{}, fmt.Errorf("This snapshot predates a conversation received from another computer. Choose a snapshot made after that transfer completed.")
	}

	// Bracket the row copy with the history triggers dropped. They exist
	// to catch writes nobody remembered to account for; a whole-database
	// replace is not one of those — the counters it must land on are the
	// snapshot's own, which the copy carries verbatim. Left installed,
	// the DELETE leg would fire one `UPDATE threads` per cleared item row
	// against rows the next statement replaces anyway, and the INSERT leg
	// would be a no-op only for as long as `items` keeps sorting before
	// `threads` in userTables' ORDER BY — a correctness argument resting
	// on a collation accident. Dropping them makes it structural.
	if _, err := tx.Exec(dropHistoryRevTriggersSQL); err != nil {
		return Identity{}, fmt.Errorf("store: restore: drop history triggers: %w", err)
	}
	// The background-launch settlement triggers come off for the same
	// reason, plus one of their own: the copy replays `items` in table
	// order, so the launch and its completion sibling arrive in whatever
	// order rowids give — the triggers would re-derive, row by row, a
	// flag the snapshot already carries settled. Dropping them makes the
	// restored flag the snapshot's by construction.
	if _, err := tx.Exec(dropBackgroundSettleTriggersSQL); err != nil {
		return Identity{}, fmt.Errorf("store: restore: drop background settle triggers: %w", err)
	}

	for _, table := range tables {
		// A history restore cannot revoke a transfer commit. Keep local
		// ownership/recovery records, including tombstones for deleted rows.
		if table == "thread_transfers" || table == "thread_transfer_sessions" || table == "remote_jobs" {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM main."` + table + `"`); err != nil {
			return Identity{}, fmt.Errorf("store: restore: clear %s: %w", table, err)
		}
	}
	for _, table := range tables {
		if table == "thread_transfers" || table == "thread_transfer_sessions" || table == "remote_jobs" {
			continue
		}
		if !srcTables[table] {
			// The migrated snapshot is on the same schema version, so a
			// missing table means real corruption, not drift.
			return Identity{}, fmt.Errorf("store: restore: snapshot missing table %s", table)
		}
		if _, err := tx.Exec(fmt.Sprintf(`INSERT INTO main.%q SELECT * FROM restore_src.%q`, table, table)); err != nil {
			return Identity{}, fmt.Errorf("store: restore: copy %s: %w", table, err)
		}
	}
	// Align AUTOINCREMENT counters (usage_ledger uses one). The table
	// only exists once an AUTOINCREMENT table has been written to, on
	// either side.
	if _, err := tx.Exec(`DELETE FROM main.sqlite_sequence`); err != nil && !isNoSuchTable(err) {
		return Identity{}, fmt.Errorf("store: restore: clear sqlite_sequence: %w", err)
	}
	if srcTables["sqlite_sequence"] {
		if _, err := tx.Exec(`INSERT INTO main.sqlite_sequence SELECT * FROM restore_src.sqlite_sequence`); err != nil && !isNoSuchTable(err) {
			return Identity{}, fmt.Errorf("store: restore: copy sqlite_sequence: %w", err)
		}
	}

	// Re-install what the copy ran without, from the same const migration
	// v55 installs, so the two can never describe different contracts.
	if _, err := tx.Exec(historyRevTriggersSQL); err != nil {
		return Identity{}, fmt.Errorf("store: restore: recreate history triggers: %w", err)
	}
	if _, err := tx.Exec(backgroundSettleTriggersSQL); err != nil {
		return Identity{}, fmt.Errorf("store: restore: recreate background settle triggers: %w", err)
	}

	// A restore rewinds every thread's history_rev / history_epoch to the
	// snapshot's values while attached clients may hold stamps from the
	// future that snapshot was taken before — a divergent future the
	// counters cannot express, because they only ever count up. Re-minting
	// the replica generation is what invalidates those clients wholesale
	// (docs/architecture/thread-replica-sync.md §3.3); it happens regardless of
	// what generation the snapshot carried, since the snapshot's own value
	// is just another stamp from another timeline. The live backend_id
	// read before the copy is written back in the same statement: it names
	// the store, not its continuity, and the snapshot's copy of it belongs
	// to whichever store minted the snapshot.
	if err := remintStoreIdentityTx(tx, liveBackendID, "store: restore: re-mint replica generation"); err != nil {
		return Identity{}, err
	}
	identity, err = identityFrom(tx)
	if err != nil {
		return Identity{}, err
	}

	if err := tx.Commit(); err != nil {
		return Identity{}, fmt.Errorf("store: commit restore: %w", err)
	}
	return identity, nil
}

// migratedScratchCopy copies the snapshot beside itself and runs the
// migration chain on the copy, returning the scratch path. The caller
// removes it.
func migratedScratchCopy(srcPath string) (string, error) {
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return "", fmt.Errorf("store: read snapshot: %w", err)
	}
	scratchFile, err := os.CreateTemp("", "ao-restore-*.db")
	if err != nil {
		return "", fmt.Errorf("store: scratch copy: %w", err)
	}
	scratch := scratchFile.Name()
	if _, err := scratchFile.Write(data); err != nil {
		scratchFile.Close()
		os.Remove(scratch)
		return "", fmt.Errorf("store: write scratch copy: %w", err)
	}
	if err := scratchFile.Close(); err != nil {
		os.Remove(scratch)
		return "", fmt.Errorf("store: close scratch copy: %w", err)
	}
	migrated, err := New(scratch)
	if err != nil {
		os.Remove(scratch)
		return "", fmt.Errorf("store: migrate snapshot copy: %w", err)
	}
	if err := migrated.Close(); err != nil {
		os.Remove(scratch)
		return "", fmt.Errorf("store: close migrated snapshot copy: %w", err)
	}
	return scratch, nil
}

// userTables lists main-schema tables in a stable order, excluding
// SQLite internals. Order doesn't matter for the restore copy (FKs are
// off) but stability keeps errors reproducible.
func (s *Store) userTables() ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM main.sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: list tables: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("store: scan table name: %w", err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// attachedTables reports the restore_src schema's table set (including
// sqlite_sequence when present).
func attachedTables(s *Store) (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT name FROM restore_src.sqlite_master WHERE type='table'`)
	if err != nil {
		return nil, fmt.Errorf("store: list snapshot tables: %w", err)
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("store: scan snapshot table name: %w", err)
		}
		out[name] = true
	}
	return out, rows.Err()
}

func isNoSuchTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}
