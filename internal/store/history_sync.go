package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// HistoryStamp is a thread's history invalidation contract
// (docs/architecture/thread-replica-sync.md §3): the pair a client compares
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
	Scope      *TimelineScopeContext
}

// historyRevTriggersSQL is the latest DDL for the three AFTER triggers on
// `items` that maintain the contract (docs/architecture/thread-replica-sync.md
// §3.1). It is shared rather than inline migration text because it has
// two installers: migration v121 replays drop+create of all three to add
// the subagent aggregate stamps, and RestoreFrom recreates them after
// dropping them for the row copy. Earlier points in the chain install the
// generation that matches the schema they run against:
// historyRevTriggersLegacySQL (v55, v58, before threads.history_bulk_load
// exists), historyRevTriggersBulkLoadSQL (v59, v72, before items.rev
// exists), historyRevTriggersV100SQL (v100, v101), historyRevTriggersV118SQL
// (v118) and historyRevInsertTriggerV119SQL (v119, before the subagent
// aggregate stamps).
// Two hand-kept latest copies would be free to drift, and the drifted
// half would be the one running on a restored database — the state
// nobody re-reads a migration to check.
//
// Each trigger does two things: advance the THREAD stamp, then stamp the
// ROWS whose read result the write changed with the thread's new
// history_rev. The row stamp is `items.rev`, and it is what lets a client
// describe a held window as (id, rev) pairs (§5). Rows are stamped only
// here; Go never assigns the column a value.
//
// Which rows a write changes is stampedRowIDsSQL: the row, the rows a
// page decorates FROM it, and the anchors a page decorates from its
// parent's subtree (decorateSubagentAnchors). An UPDATE that re-parents a
// row, or moves it across threads, stamps from both the old and the new
// side.
//
// The UPDATE trigger's epoch term is a boolean addition, so a
// repositioning UPDATE bumps epoch and an in-place content UPDATE does
// not. `IS NOT` rather than `<>` because a null-valued comparison would
// add NULL and blank the column. The `thread_id` disjunct plus the
// two-row `WHERE id IN (OLD.thread_id, NEW.thread_id)` scope cover a
// cross-thread move: it is a delete from one ordering and an insert into
// another, so both threads take rev AND epoch.
//
// `WHEN OLD.rev IS NEW.rev` on the UPDATE trigger is what keeps the
// stamping UPDATEs from being mistaken for content changes. SQLite's
// `recursive_triggers` is OFF (pinned in writerConnPragmas), which stops
// a trigger re-entering ITSELF but NOT another trigger on the same table:
// without the guard, the INSERT trigger's stamping UPDATE would fire the
// UPDATE trigger and bump the thread a second time for one insert. A
// stamping UPDATE always writes a value the row did not have (the stamp
// is read after a bump), so the guard excludes exactly those and nothing
// else. Go's own `UPDATE items SET rev = rev` touch (bumpHistoryRevForItemTx)
// leaves the column equal and therefore DOES fire, which is the point of
// spelling it that way.
//
// Under `history_bulk_load = 1` the thread stamp is frozen and the row
// stamps take the thread's current history_rev. That is sound only while
// no bulk-load writer UPDATES the content of an existing local row: such a
// write would leave a stamp a client may already hold on different bytes.
//
// An INSERT under the flag stamps only the inserted row. Every bulk-load
// insert either moves a row that was already visible from imported history
// into `items` (localizeImportedItemTx, UnsealThreadHistory), which changes
// no other row's read, or rebuilds a thread whose rows the same transaction
// deleted (a returning transfer), where every row it could stamp was
// inserted at the same frozen revision. The anchor legs would rewrite each
// anchor once per inserted child for no change in any read. A bulk-load
// writer that inserts a row a read did not already show must not hold the
// flag. Deletes under the flag (thread deletion chunks) do change their
// anchors' reads, so the update and delete triggers stamp every changed
// row. ApplyImportBatch writes shared import history and adds its row
// count to history_rev before commit.

// transcriptRootExpr is the SQL expression that reads a resume carrier's
// `transcript_root_id` stamp. It is one string because the trigger
// predicate below and idx_items_transcript_root (v100) must be textually
// identical for the planner to serve the predicate from the index.
const transcriptRootExpr = "json_extract(meta, '$." + metaKeyTranscriptRootID + "')"

// stampedRowIDsSQL selects, within `ref`'s thread, the id of every row
// whose read result a write to `ref` (NEW or OLD) changed:
//
//   - the row itself;
//   - its completion sibling, whose card is walked from the launch row
//     (decorateSubagentAnchors resolves `completion_of` to the launch and
//     reads the launch's kind, tool and transcript-root stamp);
//   - when the row has a parent: every anchor decorated from the parent's
//     subtree, which is the parent, each resume carrier whose
//     `transcript_root_id` names the parent (a carrier's round is counted
//     from the root's children, claude-wire.md §E6), and the completion
//     siblings of all of those.
//
// Every leg is an index probe: the primary key for ids,
// idx_items_completion_of for siblings and idx_items_transcript_root for
// carriers, so a child write costs a fixed handful of probes regardless
// of thread size (TestItemRevisionStampProbesIndexes,
// TestItemRevisionTriggersProbeCarriersByValue). The outer
// `id IN (...)` stamps each row once even when legs overlap, which
// matters because a stamp that leaves `rev` unchanged would fire the
// update trigger's thread bump a second time.
func stampedRowIDsSQL(ref string) string {
	return stampedRowIDsFor(ref+".thread_id", ref+".id", ref+".parent_id")
}

// stampedRowIDsFor is stampedRowIDsSQL over caller-supplied SQL
// expressions for the written row's thread, id and parent, so the same
// set can be selected from Go with placeholders (ListWireItemsBehind)
// as the triggers select with NEW/OLD references.
//
// The anchors are every row on the written row's parent chain, not only
// its parent: a page's descendant aggregate is transitive
// (subagentAggregatesByRoot), so a write under a nested launch changes
// the outer launch's read too. The chain is walked by primary key; the
// depth guard only bounds a corrupt cycle, real chains are a few levels.
//
// The carrier leg compares transcriptRootExpr with `+ancestors.id`.
// `ancestors.id` is a CTE column with TEXT affinity, and SQLite applies it
// to the other operand, which has none; an index on the expression can then
// serve only its thread_id prefix, so every write under a parent would read
// and parse the meta of each of the thread's carriers. The unary plus drops
// the affinity and the probe keys on the expression.
func stampedRowIDsFor(threadExpr, idExpr, parentExpr string) string {
	return stampedRowIDsWithCarrierKey(threadExpr, idExpr, parentExpr, "+ancestors.id")
}

// stampedRowIDsV100SQL is the row set of the v100 trigger generation, whose
// carrier leg compares against `ancestors.id`. It is frozen because
// migrations v100 and v101 install that generation; v118 replaces it.
func stampedRowIDsV100SQL(ref string) string {
	return stampedRowIDsWithCarrierKey(ref+".thread_id", ref+".id", ref+".parent_id", "ancestors.id")
}

func stampedRowIDsWithCarrierKey(threadExpr, idExpr, parentExpr, carrierKey string) string {
	ancestors := `WITH RECURSIVE ancestors(id, parent_id, depth) AS (
              SELECT id, parent_id, 1 FROM items WHERE thread_id = ` + threadExpr + ` AND id = ` + parentExpr + `
              UNION ALL
              SELECT items.id, items.parent_id, ancestors.depth + 1
                FROM ancestors CROSS JOIN items
               WHERE ancestors.parent_id <> '' AND ancestors.depth < 64
                 AND items.thread_id = ` + threadExpr + ` AND items.id = ancestors.parent_id
          )`
	anchors := `SELECT id FROM ancestors
              UNION ALL
              SELECT items.id FROM ancestors CROSS JOIN items
               WHERE items.thread_id = ` + threadExpr + ` AND ` + transcriptRootExpr + ` = ` + carrierKey
	return ancestors + `
          SELECT ` + idExpr + `
          UNION ALL
          SELECT id FROM items WHERE thread_id = ` + threadExpr + ` AND completion_of <> '' AND completion_of = ` + idExpr + `
          UNION ALL
          ` + anchors + `
          UNION ALL
          SELECT id FROM items WHERE thread_id = ` + threadExpr + ` AND completion_of <> '' AND completion_of IN (
          ` + anchors + `)`
}

const stampRowsSQL = `UPDATE items SET rev = (SELECT history_rev FROM threads WHERE id = items.thread_id)`

// stampPendingSQL keeps a trigger's row stamp off rows the same trigger
// already stamped: the aggregate statement writes `rev` on every row it
// changes, and a second write of the same value would fire the update
// trigger and bump the thread again.
const stampPendingSQL = `rev IS NOT (SELECT history_rev FROM threads WHERE id = items.thread_id)`

// Each trigger runs its subagent aggregate statement
// (subagent_aggregate_stamps.go) between the thread bump and the row
// stamp. The insert trigger's two stamping statements are gated on the
// thread's bulk-load flag, a constant for the statement, so only one of
// them reads anything.
var historyRevTriggersSQL = `CREATE TRIGGER trg_items_rev_insert AFTER INSERT ON items BEGIN
  UPDATE threads SET history_rev = history_rev + 1
   WHERE id = NEW.thread_id AND history_bulk_load = 0;
  ` + subagentAggregateInsertStmt + `
  ` + stampRowsSQL + `
   WHERE thread_id = NEW.thread_id AND id = NEW.id
     AND (SELECT history_bulk_load FROM threads WHERE id = NEW.thread_id) = 1
     AND ` + stampPendingSQL + `;
  ` + stampRowsSQL + `
   WHERE thread_id = NEW.thread_id AND id IN (` + stampedRowIDsSQL("NEW") + `)
     AND (SELECT history_bulk_load FROM threads WHERE id = NEW.thread_id) = 0
     AND ` + stampPendingSQL + `;
END;

CREATE TRIGGER trg_items_rev_update AFTER UPDATE ON items
WHEN OLD.rev IS NEW.rev
BEGIN
  UPDATE threads SET
    history_rev   = history_rev + 1,
    history_epoch = history_epoch
      + (OLD.turn_index IS NOT NEW.turn_index OR
         OLD.item_index IS NOT NEW.item_index OR
         OLD.thread_id  IS NOT NEW.thread_id)
  WHERE id IN (OLD.thread_id, NEW.thread_id) AND history_bulk_load = 0;
  ` + subagentAggregateUpdateStmt + `
  ` + stampRowsSQL + `
   WHERE ((thread_id = NEW.thread_id AND id IN (` + stampedRowIDsSQL("NEW") + `))
      OR (thread_id = OLD.thread_id AND id IN (` + stampedRowIDsSQL("OLD") + `)))
     AND ` + stampPendingSQL + `;
END;

CREATE TRIGGER trg_items_rev_delete AFTER DELETE ON items BEGIN
  UPDATE threads SET
    history_rev   = history_rev + 1,
    history_epoch = history_epoch + 1
  WHERE id = OLD.thread_id AND history_bulk_load = 0;
  ` + subagentAggregateDeleteStmt + `
  ` + stampRowsSQL + `
   WHERE thread_id = OLD.thread_id AND id IN (` + stampedRowIDsSQL("OLD") + `)
     AND ` + stampPendingSQL + `;
END;`

// historyRevTriggersV100SQL is the generation migrations v100 and v101
// install. It is frozen with their recorded SQL; v118 replaces it.
var historyRevTriggersV100SQL = historyRevTriggersFrom(stampedRowIDsV100SQL)

// historyRevTriggersV118SQL is the generation migration v118 installs,
// frozen with its recorded SQL; v119 replaces its insert trigger.
var historyRevTriggersV118SQL = historyRevTriggersFrom(stampedRowIDsSQL)

// historyRevInsertTriggerV119SQL is the insert trigger migration v119
// installs, frozen with its recorded SQL; v121 replaces it. Its two
// stamping statements are gated on the thread's bulk-load flag.
var historyRevInsertTriggerV119SQL = `CREATE TRIGGER trg_items_rev_insert AFTER INSERT ON items BEGIN
  UPDATE threads SET history_rev = history_rev + 1
   WHERE id = NEW.thread_id AND history_bulk_load = 0;
  ` + stampRowsSQL + `
   WHERE thread_id = NEW.thread_id AND id = NEW.id
     AND (SELECT history_bulk_load FROM threads WHERE id = NEW.thread_id) = 1;
  ` + stampRowsSQL + `
   WHERE thread_id = NEW.thread_id AND id IN (` + stampedRowIDsSQL("NEW") + `)
     AND (SELECT history_bulk_load FROM threads WHERE id = NEW.thread_id) = 0;
END;`

// historyRevTriggersFrom builds the three row-stamping triggers of the
// v100 and v118 generations over one definition of the rows a write
// changed.
func historyRevTriggersFrom(stampedRowIDsSQL func(ref string) string) string {
	return `CREATE TRIGGER trg_items_rev_insert AFTER INSERT ON items BEGIN
  UPDATE threads SET history_rev = history_rev + 1
   WHERE id = NEW.thread_id AND history_bulk_load = 0;
  ` + stampRowsSQL + `
   WHERE thread_id = NEW.thread_id AND id IN (` + stampedRowIDsSQL("NEW") + `);
END;

` + historyRevUpdateDeleteTriggersFrom(stampedRowIDsSQL)
}

// historyRevUpdateDeleteTriggersFrom builds the update and delete
// triggers of the v100 and v118 generations over one definition of the
// rows a write changed.
func historyRevUpdateDeleteTriggersFrom(stampedRowIDsSQL func(ref string) string) string {
	return `CREATE TRIGGER trg_items_rev_update AFTER UPDATE ON items
WHEN OLD.rev IS NEW.rev
BEGIN
  UPDATE threads SET
    history_rev   = history_rev + 1,
    history_epoch = history_epoch
      + (OLD.turn_index IS NOT NEW.turn_index OR
         OLD.item_index IS NOT NEW.item_index OR
         OLD.thread_id  IS NOT NEW.thread_id)
  WHERE id IN (OLD.thread_id, NEW.thread_id) AND history_bulk_load = 0;
  ` + stampRowsSQL + `
   WHERE (thread_id = NEW.thread_id AND id IN (` + stampedRowIDsSQL("NEW") + `))
      OR (thread_id = OLD.thread_id AND id IN (` + stampedRowIDsSQL("OLD") + `));
END;

CREATE TRIGGER trg_items_rev_delete AFTER DELETE ON items BEGIN
  UPDATE threads SET
    history_rev   = history_rev + 1,
    history_epoch = history_epoch + 1
  WHERE id = OLD.thread_id AND history_bulk_load = 0;
  ` + stampRowsSQL + `
   WHERE thread_id = OLD.thread_id AND id IN (` + stampedRowIDsSQL("OLD") + `);
END;`
}

// historyRevTriggersBulkLoadSQL is the v59 trigger body: bulk-load aware,
// thread stamps only. It is frozen because two points in the migration chain
// install it (v59, and the v72 items rebuild) and neither can reference
// `items.rev`, which v100 adds. V100 replaces all three with
// historyRevTriggersSQL before the store opens to callers.
const historyRevTriggersBulkLoadSQL = `CREATE TRIGGER trg_items_rev_insert AFTER INSERT ON items BEGIN
  UPDATE threads SET history_rev = history_rev + 1
   WHERE id = NEW.thread_id AND history_bulk_load = 0;
END;

CREATE TRIGGER trg_items_rev_update AFTER UPDATE ON items BEGIN
  UPDATE threads SET
    history_rev   = history_rev + 1,
    history_epoch = history_epoch
      + (OLD.turn_index IS NOT NEW.turn_index OR
         OLD.item_index IS NOT NEW.item_index OR
         OLD.thread_id  IS NOT NEW.thread_id)
  WHERE id IN (OLD.thread_id, NEW.thread_id) AND history_bulk_load = 0;
END;

CREATE TRIGGER trg_items_rev_delete AFTER DELETE ON items BEGIN
  UPDATE threads SET
    history_rev   = history_rev + 1,
    history_epoch = history_epoch + 1
  WHERE id = OLD.thread_id AND history_bulk_load = 0;
END;`

// historyRevTriggersLegacySQL is the v55 trigger body used while replaying the
// migration chain before v59 adds threads.history_bulk_load. V59 replaces all
// three with historyRevTriggersBulkLoadSQL before the store opens to callers.
const historyRevTriggersLegacySQL = `CREATE TRIGGER trg_items_rev_insert AFTER INSERT ON items BEGIN
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

// dropHistoryRevTriggersSQL removes either generation of the item-history
// triggers. Migration v59 uses it before installing the bulk-load-aware
// generation; RestoreFrom uses it to bracket the whole-database row copy,
// where every copied item row would otherwise fire a per-row
// `UPDATE threads`, and every deleted one an entirely useless
// counter bump on a row that is about to be replaced. Dropping the
// triggers makes the copy's counter values exactly the snapshot's,
// by construction rather than by table ordering.
const dropHistoryRevTriggersSQL = `DROP TRIGGER IF EXISTS trg_items_rev_insert;
DROP TRIGGER IF EXISTS trg_items_rev_update;
DROP TRIGGER IF EXISTS trg_items_rev_delete;`

// bumpHistoryRevTx advances one thread's history_rev inside a
// caller-owned transaction. It exists for every write that changes what a
// windowed read returns WITHOUT touching an `items` row, so the item
// triggers cannot see it: the payload span backfill, the whole-thread
// transfer import, and the fallback inside touchItemRowsTx for a row that
// is still imported history.
//
// A write that knows WHICH rows it changes uses bumpHistoryRevForItemTx or
// bumpHistoryRevForPayloadTx instead. Those advance this same counter
// through the trigger and additionally stamp the rows, which is what the
// per-row window digest reads (§5). Bumping the thread alone would leave a
// digest verifying a window whose content had changed.
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

// bumpHistoryRevForItemTx is bumpHistoryRevTx for a write that changes what
// a read of ONE KNOWN item row returns without touching the row: the
// proposed-plan state and comment mutators, whose rows decorateProposedPlanItems
// projects onto `Item.Meta` at read time.
//
// It touches the row instead of bumping the thread directly, so the item
// UPDATE trigger does the whole job: thread stamp AND the row's own `rev`.
// A window whose plan row gained an Accepted badge must not verify as
// unchanged, and only a re-stamped row says that. `SET rev = rev` is the
// touch: it asserts the column is unchanged, which is exactly the trigger's
// `WHEN OLD.rev IS NEW.rev` guard, and it keeps `rev` a value no Go
// statement ever chooses.
//
// A thread whose row is still IMPORTED history has nothing local to touch.
// That row cannot carry a per-row stamp at all (importedItemRevExpr), so the
// fallback is the plain thread bump — the same answer as before this column
// existed. Window verification refuses any window containing an imported row,
// so the thread stamp is the only signal such a client can use, and it moves.
func bumpHistoryRevForItemTx(exec sqlExecutor, threadID, itemID, label string) error {
	if itemID == "" {
		return fmt.Errorf("%s: item id is required to stamp an item revision", label)
	}
	return touchItemRowsTx(
		exec, threadID, label,
		`UPDATE items SET rev = rev WHERE thread_id = ? AND id = ?`,
		threadID, itemID,
	)
}

// bumpHistoryRevForPayloadTx is bumpHistoryRevForItemTx for the payload
// mutators: payload content and meta ride the item rows that reference the
// payload, so those rows are the ones whose read result changed. A payload
// can be referenced as a result (`payload_id`) or as a tool-call input
// (`input_payload_id`), and both projections are on the wire.
//
// UpdatePayloadSpans is deliberately NOT a caller. Preview spans are a
// derived highlight cache with a documented "empty means not computed, use
// the highlight RPC" fallback, and the client version-checks them against
// the payload content it already holds (frontend utils/payloadVersion.ts),
// so a window whose spans are behind is still a CORRECT window. It keeps the
// plain thread bump: a span backfill still grades the stamp stale, it just
// does not invalidate a per-row digest.
func bumpHistoryRevForPayloadTx(exec sqlExecutor, threadID, payloadID, label string) error {
	if payloadID == "" {
		return fmt.Errorf("%s: payload id is required to stamp an item revision", label)
	}
	return touchItemRowsTx(
		exec, threadID, label,
		touchPayloadOwnerRowsSQL,
		threadID, payloadID, threadID, payloadID, threadID,
	)
}

// touchPayloadOwnerRowsSQL is bumpHistoryRevForPayloadTx's touch, written
// as the UNION ALL of two single-column probes for the same reason
// GetThreadItemByPayloadID is: each branch states one column against one
// partial index (idx_items_payload_id, idx_items_input_payload_id), while
// a single `payload_id = ? OR input_payload_id = ?` clause puts SQLite on
// the broad idx_items_thread and scans every row of the thread. This runs
// on every streaming payload append, so the scan would be per delta batch.
//
// The bind order is thread id, payload id, thread id, payload id, thread
// id. It is a const so TestPayloadTouchProbesPayloadIndexes pins the
// production statement rather than a copy of it.
const touchPayloadOwnerRowsSQL = `UPDATE items SET rev = rev
		  WHERE thread_id = ? AND id IN (
		        SELECT id FROM items WHERE payload_id = ? AND thread_id = ?
		         UNION ALL
		        SELECT id FROM items WHERE input_payload_id = ? AND thread_id = ?
		  )`

// touchItemRowsTx runs a row touch and guarantees the thread stamp moved.
// Matching no row is not an error and not a no-op: the owner is imported
// history, and the thread stamp still has to advance or a client holding it
// would be told nothing changed.
func touchItemRowsTx(exec sqlExecutor, threadID, label, touchSQL string, args ...any) error {
	if threadID == "" {
		return fmt.Errorf("%s: thread id is required to advance history_rev", label)
	}
	result, err := exec.Exec(touchSQL, args...)
	if err != nil {
		return fmt.Errorf("%s: stamp item revision: %w", label, err)
	}
	touched, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: count stamped item revisions: %w", label, err)
	}
	if touched > 0 {
		return nil
	}
	return bumpHistoryRevTx(exec, threadID, label)
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
// (docs/architecture/thread-replica-sync.md §5).
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
// `runWindowRows` sizes the page exactly as it sizes ListThreadSliceAround's:
// the two return the same window and must therefore compose it identically.
//
// `held` describes the rows the caller already has, and is nil when it has
// none. It is the second way to earn `fresh`: when the stamps do not match
// but the held rows still ARE the read (verifyHeldWindowTx), the answer is
// page-less and the returned stamp attests the caller's own rows. That is
// what keeps a reopen after a turn on the same thread free, where a stamp
// the turn invalidated cannot.
func (s *Store) SyncThreadWindow(ctx context.Context, threadID, anchorItemID string, itemBudget, runWindowRows int, have HistoryStamp, held *HeldWindow, selection TimelineSelection) (ThreadWindowSync, error) {
	return readSnapshotContext(ctx, s.reader(), "sync thread window", func(q sqlQueryer) (ThreadWindowSync, error) {
		return s.syncThreadWindow(q, threadID, anchorItemID, itemBudget, runWindowRows, have, held, selection)
	})
}

func (s *Store) syncThreadWindow(q sqlQueryer, threadID, anchorItemID string, itemBudget, runWindowRows int, have HistoryStamp, held *HeldWindow, selection TimelineSelection) (ThreadWindowSync, error) {
	stamp, found, err := readHistoryStampTx(q, threadID)
	if err != nil {
		return ThreadWindowSync{}, err
	}
	identity, err := identityFrom(q)
	if err != nil {
		return ThreadWindowSync{}, err
	}
	if !found {
		return ThreadWindowSync{Status: SyncGone, Generation: identity.ReplicaGeneration}, nil
	}

	scope, err := s.resolveTimelineScope(q, threadID, selection)
	if errors.Is(err, ErrTimelineScopeGone) {
		return ThreadWindowSync{Status: SyncGone, Generation: identity.ReplicaGeneration}, nil
	}
	if err != nil {
		return ThreadWindowSync{}, err
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
		Scope:      scope.context,
	}
	if status == SyncFresh {
		return out, nil
	}
	if held != nil {
		verified, err := verifyHeldWindowTx(q, threadID, *held, scope)
		if err != nil {
			return ThreadWindowSync{}, err
		}
		if verified {
			// The rows the caller holds are the rows a read would return,
			// so the current stamp attests them exactly as it would attest
			// a page. The status is the same `fresh`, and the client may
			// adopt the stamp for the window it already painted.
			out.Status = SyncFresh
			return out, nil
		}
	}

	page, err := s.listThreadSliceAround(q, threadID, anchorItemID, itemBudget, runWindowRows, scope)
	if err != nil {
		return ThreadWindowSync{}, err
	}
	out.Page = &page
	return out, nil
}
