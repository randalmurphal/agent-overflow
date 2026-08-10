package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// HistoryStamp is a thread's history invalidation contract
// (docs/specs/thread-replica-sync.md §3): the pair a client compares
// against its cached window to learn whether the window is still what a
// fresh read would return.
//
// Rev advances on EVERY persisted mutation that can change what a
// windowed item read returns (item insert/update/delete, payload
// content/meta/span writes). Equal revs mean byte-identical window reads;
// a differing rev says nothing about WHAT changed.
//
// Epoch advances on the subset a client holding a cached ORDER cannot
// survive by re-fetching a range: item deletion and item repositioning.
// Every epoch bump also bumps rev, so a rev match alone implies fully
// fresh — epoch exists to grade how stale a mismatch is.
//
// UnknownStamp (-1) is the client's "I hold no replica / my stamp is not
// trustworthy" value. It can never compare equal to a real stamp, which
// is what makes an understated client stamp cost one redundant fetch
// instead of showing stale content as fresh (§3.4).
type HistoryStamp struct {
	Rev   int64 `json:"historyRev"`
	Epoch int64 `json:"historyEpoch"`
}

// UnknownStamp is the sentinel for "no replica / stamp unknown".
const UnknownStamp int64 = -1

// UnknownHistoryStamp is the HistoryStamp a caller with no replica sends.
func UnknownHistoryStamp() HistoryStamp {
	return HistoryStamp{Rev: UnknownStamp, Epoch: UnknownStamp}
}

// SyncStatus grades a client window against the store's current stamps.
type SyncStatus string

const (
	// SyncFresh — the caller's stamps match. No page is returned; the
	// caller's cached window is byte-identical to what a read would give.
	SyncFresh SyncStatus = "fresh"
	// SyncStale — epoch matches, rev doesn't: only additive or in-place
	// changes can have happened. A page is returned and is
	// range-authoritative within its cursor bounds.
	SyncStale SyncStatus = "stale"
	// SyncRewritten — epoch differs: rows may have been deleted or moved,
	// so cached scrollback outside the returned page is unusable.
	SyncRewritten SyncStatus = "rewritten"
	// SyncGone — no thread row. The caller drops its replica entry.
	SyncGone SyncStatus = "gone"
)

// ThreadWindowSync is one SyncThreadWindow answer. Page is nil for
// SyncFresh (nothing changed) and SyncGone (nothing to send).
type ThreadWindowSync struct {
	Status SyncStatus
	Stamp  HistoryStamp
	// Generation is the store's replica generation at read time. A
	// mismatch against the client's tells it to drop the whole replica
	// for this backend rather than reason about the counters at all.
	Generation string
	Page       *PagedItems
}

// historyRevTriggersSQL is the DDL for the three AFTER triggers on
// `items` that maintain the contract (docs/specs/thread-replica-sync.md
// §3.1). It is a const rather than inline migration text because it has
// two installers: migration v55 concatenates it into its SQL, and
// RestoreFrom recreates the triggers after dropping them for the row
// copy. Two hand-kept copies would be free to drift, and the drifted
// half would be the one running on a restored database — the state
// nobody re-reads a migration to check.
//
// The UPDATE trigger's epoch term is a boolean addition, so a
// repositioning UPDATE bumps epoch and an in-place content UPDATE does
// not. `IS NOT` rather than `<>` because a null-valued comparison would
// add NULL and blank the column. The `thread_id` disjunct plus the
// two-row `WHERE id IN (OLD.thread_id, NEW.thread_id)` scope cover a
// cross-thread move: it is a delete from one ordering and an insert into
// another, so both threads take rev AND epoch.
const historyRevTriggersSQL = `CREATE TRIGGER trg_items_rev_insert AFTER INSERT ON items BEGIN
  UPDATE threads SET history_rev = history_rev + 1 WHERE id = NEW.thread_id;
END;

CREATE TRIGGER trg_items_rev_update AFTER UPDATE ON items BEGIN
  UPDATE threads SET
    history_rev   = history_rev + 1,
    history_epoch = history_epoch
      + (OLD.turn_index IS NOT NEW.turn_index OR
         OLD.item_index IS NOT NEW.item_index OR
         OLD.thread_id  IS NOT NEW.thread_id)
  WHERE id IN (OLD.thread_id, NEW.thread_id);
END;

CREATE TRIGGER trg_items_rev_delete AFTER DELETE ON items BEGIN
  UPDATE threads SET
    history_rev   = history_rev + 1,
    history_epoch = history_epoch + 1
  WHERE id = OLD.thread_id;
END;`

// dropHistoryRevTriggersSQL removes what historyRevTriggersSQL installs.
// Only RestoreFrom uses it, and only to bracket the whole-database row
// copy: every copied item row would otherwise fire a per-row
// `UPDATE threads`, and every deleted one an entirely useless
// counter bump on a row that is about to be replaced. Dropping the
// triggers makes the copy's counter values exactly the snapshot's,
// by construction rather than by table ordering.
const dropHistoryRevTriggersSQL = `DROP TRIGGER IF EXISTS trg_items_rev_insert;
DROP TRIGGER IF EXISTS trg_items_rev_update;
DROP TRIGGER IF EXISTS trg_items_rev_delete;`

// bumpHistoryRevTx advances one thread's history_rev inside a
// caller-owned transaction. It exists for every write that changes what a
// windowed read returns WITHOUT touching an `items` row, so the v55
// triggers cannot see it:
//
//   - the payload mutators — `payloads` carries no thread_id, and routing
//     a trigger through a subquery over items.payload_id would put an
//     unindexed scan on the streaming append path;
//   - the proposed-plan state and comment mutators — their rows are
//     projected onto `Item.Meta` at read time by
//     decorateProposedPlanItems, so a window read genuinely changes when
//     they do.
//
// Each of those mutators names its thread in its own signature, and that
// IS the enforcement: there is no way to reach the write without saying
// whose history it belongs to.
//
// A bump that matches no thread row is an error, not a no-op: it means
// either a caller naming the wrong thread (a bug that would otherwise
// silently under-report history changes forever) or a thread deleted
// underneath the write, which is the same benign-drop shape the payload
// mutators already report as wrapped sql.ErrNoRows.
func bumpHistoryRevTx(exec sqlExecutor, threadID, label string) error {
	if threadID == "" {
		return fmt.Errorf("%s: thread id is required to advance history_rev", label)
	}
	result, err := exec.Exec(
		`UPDATE threads SET history_rev = history_rev + 1 WHERE id = ?`,
		threadID,
	)
	if err != nil {
		return fmt.Errorf("%s: bump history rev: %w", label, err)
	}
	return requireRowsAffected(result, label+": bump history rev")
}

// readHistoryStampTx reads a thread's stamps. found=false means no thread
// row — a deleted thread, which SyncThreadWindow reports as `gone`.
func readHistoryStampTx(q sqlQueryer, threadID string) (HistoryStamp, bool, error) {
	var stamp HistoryStamp
	err := q.QueryRow(
		`SELECT history_rev, history_epoch FROM threads WHERE id = ?`,
		threadID,
	).Scan(&stamp.Rev, &stamp.Epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return HistoryStamp{}, false, nil
	}
	if err != nil {
		return HistoryStamp{}, false, fmt.Errorf("store: read history stamp for %s: %w", threadID, err)
	}
	return stamp, true, nil
}

// ThreadHistoryStamp reads one thread's current stamps. Used by event
// emitters that attach a stamp to content the client already holds; a
// missing thread reports found=false rather than an error, since a
// deleted thread is an ordinary outcome for a late event.
func (s *Store) ThreadHistoryStamp(threadID string) (HistoryStamp, bool, error) {
	return readHistoryStampTx(s.reader(), threadID)
}

// SyncThreadWindow answers "is my cached window for this thread still
// current, and if not, here is the window" in one read-pool transaction
// (docs/specs/thread-replica-sync.md §5).
//
// The single transaction is the load-bearing part: under WAL the whole
// call sees one snapshot, so the stamps returned attest EXACTLY the rows
// returned. Reading the stamps and the page separately would admit the
// one answer the contract must never give — newer stamps over older rows,
// which a client would record as fresh and never correct.
//
// It is read-only. On a WAL database it runs on the read pool, so it
// neither takes nor waits on the single writer connection and stays fast
// mid-turn. On the writer-fallback configurations that have no read pool
// (`:memory:`, non-WAL) `reader()` IS the writer, so the transaction
// serializes with flush writes like any other read there.
func (s *Store) SyncThreadWindow(ctx context.Context, threadID, anchorItemID string, itemBudget int, have HistoryStamp) (ThreadWindowSync, error) {
	tx, err := s.reader().BeginTx(ctx, nil)
	if err != nil {
		return ThreadWindowSync{}, fmt.Errorf("store: begin sync thread window for %s: %w", threadID, err)
	}
	// Read-only: the read pool's connections carry query_only(1), and
	// nothing here writes. Rollback is the whole cleanup.
	defer tx.Rollback()

	stamp, found, err := readHistoryStampTx(tx, threadID)
	if err != nil {
		return ThreadWindowSync{}, err
	}
	identity, err := identityFrom(tx)
	if err != nil {
		return ThreadWindowSync{}, err
	}
	if !found {
		return ThreadWindowSync{Status: SyncGone, Generation: identity.ReplicaGeneration}, nil
	}

	status := SyncRewritten
	switch {
	case have.Epoch == stamp.Epoch && have.Rev == stamp.Rev:
		status = SyncFresh
	case have.Epoch == stamp.Epoch:
		status = SyncStale
	}
	out := ThreadWindowSync{
		Status:     status,
		Stamp:      stamp,
		Generation: identity.ReplicaGeneration,
	}
	if status == SyncFresh {
		return out, nil
	}

	page, err := s.listThreadSliceAround(tx, threadID, anchorItemID, itemBudget)
	if err != nil {
		return ThreadWindowSync{}, err
	}
	out.Page = &page
	return out, nil
}
