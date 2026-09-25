package store

import "strings"

// historyRevTriggersSQL is the latest DDL for the three AFTER triggers on
// `items` that maintain the contract (docs/architecture/thread-replica-sync.md
// §3.1). It is shared rather than inline migration text because it has
// two installers: migration v128 replays drop+create of all three to leave
// parked siblings out of the sibling legs, and RestoreFrom recreates them
// after dropping them for the row copy. Earlier points in the chain
// install the generation that matches the schema they run against:
// historyRevTriggersLegacySQL (v55, v58, before threads.history_bulk_load
// exists), historyRevTriggersBulkLoadSQL (v59, v72, before items.rev
// exists), historyRevTriggersV100SQL (v100, v101), historyRevTriggersV118SQL
// (v118), historyRevInsertTriggerV119SQL (v119, before the subagent
// aggregate stamps) and historyRevTriggersV121SQL (v121).
// Two hand-kept latest copies would be free to drift, and the drifted
// half would be the one running on a restored database: the state
// nobody re-reads a migration to check.
//
// Each trigger does two things: advance the THREAD stamp, then stamp the
// ROWS whose read result the write changed with the thread's new
// history_rev. The row stamp is `items.rev`, and it is what lets a client
// describe a held window as (id, rev) pairs (§5). Rows are stamped only
// here and by the subagent_aggregates stamp triggers; Go never writes the
// column.
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
// The UPDATE trigger fires on every column but `rev`
// (itemRevUpdateColumns), so a stamping UPDATE, which writes `rev` alone,
// does not fire it: no second thread bump for one write, and no trigger
// program per stamped row. SQLite's `recursive_triggers` is OFF (pinned in
// writerConnPragmas), which stops a trigger re-entering ITSELF but NOT
// another trigger on the same table; no stamp writes a listed column. Go's
// touch (bumpHistoryRevForItemTx, touchPayloadOwnerRowsSQL) writes
// `updated_at` to itself, a listed column left equal, so it DOES fire and
// the trigger does the stamping.
//
// Under `history_bulk_load = 1` the thread stamp is frozen and the row
// stamps take the thread's current history_rev. That is sound only while
// no bulk-load writer UPDATES the content of an existing local row: such a
// write would leave a stamp a client may already hold on different bytes.
//
// An INSERT under the flag stamps only the inserted row. Every bulk-load
// insert either moves a row that was already visible into `items`, from
// imported history (localizeImportedItemTx, UnsealThreadHistory) or from a
// pointer fork's ancestor (snapshotRowsTx), or rebuilds a thread whose
// rows the same transaction deleted (a returning transfer), where every row
// it could stamp was inserted at the same frozen revision. A moved row
// changes no other row's read: the subagent cards read every arm. The
// movers recompute the stamps a move changes, which only local rows hold
// (recomputeLocalizedCardsTx). The anchor legs would rewrite each anchor
// once per inserted child. A bulk-load writer that inserts a row a read
// did not already show must not hold the flag. Deletes under the flag
// (thread deletion chunks) do change their anchors' reads, so the update
// and delete triggers stamp every changed row. ApplyImportBatch writes
// shared import history and adds its row count to history_rev before
// commit.

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
// A parked sibling (agent_stops.go) records one run and borrows no card
// (aggBorrowsCardSQL), so no write to its launch or under it changes its
// read: the sibling legs pass over it.
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
// (forEachSubagentAggregateRow), so a write under a nested launch changes
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
	return stampedRowIDsWithCarrierKey(threadExpr, idExpr, parentExpr, "+ancestors.id", stampedSiblingTerm)
}

// stampedSiblingTerm keeps parked siblings out of the sibling legs.
const stampedSiblingTerm = " AND status <> '" + ItemStatusParked + "'"

// stampedRowIDsV100SQL is the row set of the v100 trigger generation, whose
// carrier leg compares against `ancestors.id`. It is frozen because
// migrations v100 and v101 install that generation; v118 replaces it.
func stampedRowIDsV100SQL(ref string) string {
	return stampedRowIDsWithCarrierKey(ref+".thread_id", ref+".id", ref+".parent_id", "ancestors.id", "")
}

// stampedRowIDsV118SQL is the row set of the v118 to v121 trigger
// generations, whose sibling legs stamp parked siblings too. It is frozen
// with their recorded SQL; v128 replaces it.
func stampedRowIDsV118SQL(ref string) string {
	return stampedRowIDsWithCarrierKey(ref+".thread_id", ref+".id", ref+".parent_id", "+ancestors.id", "")
}

func stampedRowIDsWithCarrierKey(threadExpr, idExpr, parentExpr, carrierKey, siblingTerm string) string {
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
          SELECT id FROM items WHERE thread_id = ` + threadExpr + ` AND completion_of <> '' AND completion_of = ` + idExpr + siblingTerm + `
          UNION ALL
          ` + anchors + `
          UNION ALL
          SELECT id FROM items WHERE thread_id = ` + threadExpr + ` AND completion_of <> ''` + siblingTerm + ` AND completion_of IN (
          ` + anchors + `)`
}

const stampRowsSQL = `UPDATE items SET rev = (SELECT history_rev FROM threads WHERE id = items.thread_id)`

// itemRevUpdateColumns are the columns whose write fires
// trg_items_rev_update: every items column but rev, in table order.
// Migration v121 installs the trigger with this list; a migration that
// adds a column to items must reinstall the trigger with it
// (TestItemRevUpdateTriggerListsEveryColumnButRev).
var itemRevUpdateColumns = []string{
	"id", "thread_id", "turn_index", "item_index", "kind", "role", "status", "summary",
	"payload_id", "parent_id", "is_background", "completion_of", "tool_name", "decision", "meta",
	"created_at", "updated_at", "input_payload_id",
}

// threadRevSQL is the history_rev of the thread a trigger's
// NEW.thread_id or OLD.thread_id names. It is a constant of the statement,
// read once, where a subquery on items.thread_id would read the threads
// row again for every row the statement stamps.
func threadRevSQL(threadExpr string) string {
	return `(SELECT history_rev FROM threads WHERE id = ` + threadExpr + `)`
}

// stampRowsInSQL is a trigger's row stamp to revision rev.
// stampPendingInSQL keeps it off rows already at that revision, which it
// would rewrite for nothing.
func stampRowsInSQL(rev string) string { return `UPDATE items SET rev = ` + rev }

func stampPendingInSQL(rev string) string { return `rev IS NOT ` + rev }

// updatedRowRevSQL is the revision of a row the update trigger stamps: a
// write that moves a row between threads stamps rows in both.
var updatedRowRevSQL = `(CASE WHEN thread_id = NEW.thread_id THEN ` + threadRevSQL("NEW.thread_id") +
	` ELSE ` + threadRevSQL("OLD.thread_id") + ` END)`

// The triggers do no subagent card work: the store keeps the cards in Go
// (subagent_card.go). The insert trigger's two stamping statements are
// gated on the thread's bulk-load flag, a constant for the statement, so
// only one of them reads anything.
var historyRevTriggersSQL = historyRevTriggersOver(stampedRowIDsSQL)

// historyRevTriggersV121SQL is the generation migration v121 installs,
// frozen with its recorded SQL; v128 replaces it.
var historyRevTriggersV121SQL = historyRevTriggersOver(stampedRowIDsV118SQL)

// historyRevTriggersOver builds the three row-stamping triggers of the
// v121 and later generations over one definition of the rows a write
// changed.
func historyRevTriggersOver(stampedRowIDsSQL func(ref string) string) string {
	return `CREATE TRIGGER trg_items_rev_insert AFTER INSERT ON items BEGIN
  UPDATE threads SET history_rev = history_rev + 1
   WHERE id = NEW.thread_id AND history_bulk_load = 0;
  ` + stampRowsInSQL(threadRevSQL("NEW.thread_id")) + `
   WHERE thread_id = NEW.thread_id AND id = NEW.id
     AND (SELECT history_bulk_load FROM threads WHERE id = NEW.thread_id) = 1
     AND ` + stampPendingInSQL(threadRevSQL("NEW.thread_id")) + `;
  ` + stampRowsInSQL(threadRevSQL("NEW.thread_id")) + `
   WHERE thread_id = NEW.thread_id AND id IN (` + stampedRowIDsSQL("NEW") + `)
     AND (SELECT history_bulk_load FROM threads WHERE id = NEW.thread_id) = 0
     AND ` + stampPendingInSQL(threadRevSQL("NEW.thread_id")) + `;
END;

CREATE TRIGGER trg_items_rev_update AFTER UPDATE OF ` + strings.Join(itemRevUpdateColumns, ", ") + ` ON items
BEGIN
  UPDATE threads SET
    history_rev   = history_rev + 1,
    history_epoch = history_epoch
      + (OLD.turn_index IS NOT NEW.turn_index OR
         OLD.item_index IS NOT NEW.item_index OR
         OLD.thread_id  IS NOT NEW.thread_id)
  WHERE id IN (OLD.thread_id, NEW.thread_id) AND history_bulk_load = 0;
  ` + stampRowsInSQL(updatedRowRevSQL) + `
   WHERE ((thread_id = NEW.thread_id AND id IN (` + stampedRowIDsSQL("NEW") + `))
      OR (thread_id = OLD.thread_id AND id IN (` + stampedRowIDsSQL("OLD") + `)))
     AND ` + stampPendingInSQL(updatedRowRevSQL) + `;
END;

CREATE TRIGGER trg_items_rev_delete AFTER DELETE ON items BEGIN
  UPDATE threads SET
    history_rev   = history_rev + 1,
    history_epoch = history_epoch + 1
  WHERE id = OLD.thread_id AND history_bulk_load = 0;
  ` + stampRowsInSQL(threadRevSQL("OLD.thread_id")) + `
   WHERE thread_id = OLD.thread_id AND id IN (` + stampedRowIDsSQL("OLD") + `)
     AND ` + stampPendingInSQL(threadRevSQL("OLD.thread_id")) + `;
END;`
}

// historyRevTriggersV100SQL is the generation migrations v100 and v101
// install. It is frozen with their recorded SQL; v118 replaces it.
var historyRevTriggersV100SQL = historyRevTriggersFrom(stampedRowIDsV100SQL)

// historyRevTriggersV118SQL is the generation migration v118 installs,
// frozen with its recorded SQL; v119 replaces its insert trigger.
var historyRevTriggersV118SQL = historyRevTriggersFrom(stampedRowIDsV118SQL)

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
   WHERE thread_id = NEW.thread_id AND id IN (` + stampedRowIDsV118SQL("NEW") + `)
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
