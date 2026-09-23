package store

import "fmt"

// Pointer forks (docs/architecture/sqlite-store.md#pointer-forks).
//
// A fork stores no copy of the history it inherits. threads carries the
// pointer (immediate source and cut); thread_fork_lineage flattens the chain
// so a view arm can reach every ancestor with one index probe per level; and
// thread_fork_hidden lists inherited ids a fork replaced or removed. The four
// timeline views gain lineage arms, so every logical read of a fork resolves
// inherited rows, payloads and turns from the thread that owns them.
//
// The migration also retires payload snapshots. Forks no longer borrow
// payloads, so every borrowed graph is copied back into the payload rows that
// referenced it before the copy-on-write triggers and views drop.
var pointerForksV116SQL = `
ALTER TABLE threads ADD COLUMN fork_source_thread_id TEXT NOT NULL DEFAULT '';
ALTER TABLE threads ADD COLUMN fork_cut_turn_index INTEGER NOT NULL DEFAULT 0;
ALTER TABLE threads ADD COLUMN fork_cut_item_index INTEGER NOT NULL DEFAULT 0;
ALTER TABLE threads ADD COLUMN fork_source_title TEXT NOT NULL DEFAULT '';

CREATE TABLE thread_fork_lineage (
    thread_id      TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    depth          INTEGER NOT NULL CHECK(depth BETWEEN 1 AND ` + forkLineageMaxDepthSQL + `),
    ancestor_id    TEXT    NOT NULL,
    cut_turn_index INTEGER NOT NULL,
    cut_item_index INTEGER NOT NULL,
    PRIMARY KEY (thread_id, depth)
) WITHOUT ROWID;
CREATE INDEX idx_thread_fork_lineage_ancestor ON thread_fork_lineage(ancestor_id, thread_id);

CREATE TABLE thread_fork_hidden (
    thread_id TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    item_id   TEXT NOT NULL,
    PRIMARY KEY (thread_id, item_id)
) WITHOUT ROWID;

CREATE INDEX idx_threads_fork_source ON threads(fork_source_thread_id) WHERE fork_source_thread_id <> '';
CREATE INDEX idx_items_unsettled ON items(thread_id, turn_index, item_index) WHERE status IN ('running', 'streaming');

` + dropPayloadSnapshotCopyOnWriteSQL + `
UPDATE payloads
   SET data = (SELECT r.data FROM resolved_payloads r WHERE r.thread_id = payloads.thread_id AND r.id = payloads.id)
 WHERE EXISTS (SELECT 1 FROM payload_snapshot_refs r WHERE r.thread_id = payloads.thread_id AND r.payload_id = payloads.id);
INSERT INTO payload_chunks(thread_id, payload_id, chunk_index, start_offset, data, created_at)
SELECT r.thread_id, r.payload_id, c.chunk_index, c.start_offset, c.data, c.created_at
  FROM payload_snapshot_refs r
  JOIN payload_snapshots s ON s.id = r.snapshot_id
  JOIN payload_chunks c ON c.thread_id = s.source_thread_id AND c.payload_id = s.payload_id
UNION ALL
SELECT r.thread_id, r.payload_id, c.chunk_index, c.start_offset, c.data, c.created_at
  FROM payload_snapshot_refs r
  JOIN payload_snapshot_chunks c ON c.snapshot_id = r.snapshot_id;
INSERT INTO edit_file_snapshots(thread_id, payload_id, path, content, created_at)
SELECT r.thread_id, r.payload_id, e.path, e.content, e.created_at
  FROM payload_snapshot_refs r
  JOIN payload_snapshots s ON s.id = r.snapshot_id
  JOIN edit_file_snapshots e ON e.thread_id = s.source_thread_id AND e.payload_id = s.payload_id
UNION ALL
SELECT r.thread_id, r.payload_id, e.path, e.content, e.created_at
  FROM payload_snapshot_refs r
  JOIN payload_snapshot_edits e ON e.snapshot_id = r.snapshot_id;
DELETE FROM payload_snapshot_refs;
DELETE FROM payload_snapshots;
DROP TRIGGER trg_payload_snapshot_refs_gc;
DROP TRIGGER trg_payload_snapshots_import_gc;
DROP TRIGGER trg_thread_import_chunks_gc;
CREATE TRIGGER trg_thread_import_chunks_gc AFTER DELETE ON thread_import_chunks BEGIN
    DELETE FROM import_history_chunks WHERE id = OLD.chunk_id
      AND NOT EXISTS (SELECT 1 FROM thread_import_chunks WHERE chunk_id = OLD.chunk_id);
END;
DELETE FROM import_history_chunks
 WHERE NOT EXISTS (SELECT 1 FROM thread_import_chunks WHERE chunk_id = import_history_chunks.id);

DROP VIEW timeline_items;
DROP VIEW timeline_payloads;
DROP VIEW resolved_payloads;
DROP VIEW timeline_payload_chunks;
DROP VIEW timeline_edit_file_snapshots;
` + pointerForkViewsSQL + forkTriggersSQL

// forkLineageMaxDepth caps how many ancestor levels one thread reads through.
// Each level is one more pair of index-ordered arms in an ordered read, so the
// cap bounds statement size. A chain this deep needs 32 nested forks of forks
// that all still read through their sources; CreatePointerFork refuses the
// 33rd rather than degrade.
const (
	forkLineageMaxDepth    = 32
	forkLineageMaxDepthSQL = "32"
)

// dropPayloadSnapshotCopyOnWriteSQL removes the v107 BEFORE triggers that
// preserved a borrowed payload graph. It is the frozen v107 trigger list.
const dropPayloadSnapshotCopyOnWriteSQL = `DROP TRIGGER trg_snapshot_payloads_UPDATE;
DROP TRIGGER trg_snapshot_payloads_DELETE;
DROP TRIGGER trg_snapshot_payload_chunks_INSERT;
DROP TRIGGER trg_snapshot_payload_chunks_UPDATE;
DROP TRIGGER trg_snapshot_payload_chunks_DELETE;
DROP TRIGGER trg_snapshot_edit_file_snapshots_INSERT;
DROP TRIGGER trg_snapshot_edit_file_snapshots_UPDATE;
DROP TRIGGER trg_snapshot_edit_file_snapshots_DELETE;
`

// inheritedItemVisibleSQL is the lineage arm's half of the view semantics,
// written against the lineage row `l` and the ancestor row `items`. A row is
// visible to l.thread_id when it sits before that level's cut and neither the
// fork nor any nearer ancestor hid its id. Every lineage arm in this package
// states it.
const inheritedItemVisibleSQL = `(items.turn_index, items.item_index) < (l.cut_turn_index, l.cut_item_index)
   AND NOT EXISTS (
       SELECT 1 FROM thread_fork_hidden hidden
        WHERE hidden.thread_id = l.thread_id AND hidden.item_id = items.id
   )
   AND NOT EXISTS (
       SELECT 1 FROM thread_fork_lineage nearer
         JOIN thread_fork_hidden hidden ON hidden.thread_id = nearer.ancestor_id AND hidden.item_id = items.id
        WHERE nearer.thread_id = l.thread_id AND nearer.depth < l.depth
   )`

// payloadHeldBySQL reports whether thread `thread` holds payload `id` itself,
// locally or through its own imported history. The imported probe starts
// from the payload's chunk rows and probes each chunk's membership in the
// thread, never the thread's chunk references.
func payloadHeldBySQL(thread, id string) string {
	return `(EXISTS (SELECT 1 FROM payloads held WHERE held.thread_id = ` + thread + ` AND held.id = ` + id + `)
        OR EXISTS (SELECT 1 FROM import_history_payloads held
                     CROSS JOIN thread_import_chunks held_refs
                       ON held_refs.chunk_id = held.chunk_id AND held_refs.thread_id = ` + thread + `
                    WHERE held.id = ` + id + `))`
}

// inheritedPayloadVisibleSQL resolves a payload id to the nearest thread in
// the lineage that holds it: the fork itself, then ancestors by depth. A fork
// holds a copy only beside an item it copied, so the nearest holder is the
// thread that owns the row referencing the payload.
func inheritedPayloadVisibleSQL(id string) string {
	return `NOT ` + payloadHeldBySQL("l.thread_id", id) + `
   AND NOT EXISTS (
       SELECT 1 FROM thread_fork_lineage nearer
        WHERE nearer.thread_id = l.thread_id AND nearer.depth < l.depth
          AND ` + payloadHeldBySQL("nearer.ancestor_id", id) + `
   )`
}

// inheritedTurnVisibleSQL is the turn half. A fork owns a copy of the turn
// row its cut falls in (settled or trimmed as the cut requires) and every
// later turn row, so an ancestor's turn row is visible only below that
// level's cut turn, and only when no nearer thread holds its own row there.
const inheritedTurnVisibleSQL = `turns.turn_index < l.cut_turn_index
   AND NOT EXISTS (
       SELECT 1 FROM turns held WHERE held.thread_id = l.thread_id AND held.turn_index = turns.turn_index
   )
   AND NOT EXISTS (
       SELECT 1 FROM thread_fork_lineage nearer
         JOIN turns held ON held.thread_id = nearer.ancestor_id AND held.turn_index = turns.turn_index
        WHERE nearer.thread_id = l.thread_id AND nearer.depth < l.depth
   )`

const timelineItemViewColumns = `items.id, %s AS thread_id, items.turn_index, items.item_index,
    items.kind, items.role, items.status, items.summary, items.payload_id,
    items.parent_id, items.is_background, items.completion_of, items.tool_name,
    items.decision, items.meta, items.created_at, items.updated_at,
    items.input_payload_id, %s AS rev`

var pointerForkViewsSQL = `
CREATE VIEW timeline_items AS
SELECT ` + fmt.Sprintf(timelineItemViewColumns, "items.thread_id", "items.rev") + `
  FROM items
UNION ALL
SELECT ` + fmt.Sprintf(timelineItemViewColumns, "refs.thread_id", importedItemRevExpr) + `
  FROM thread_import_chunks refs
  JOIN import_history_items items ON items.chunk_id = refs.chunk_id
 WHERE ` + importedNotOverridden + `
UNION ALL
SELECT ` + fmt.Sprintf(timelineItemViewColumns, "l.thread_id", importedItemRevExpr) + `
  FROM thread_fork_lineage l
  JOIN items ON items.thread_id = l.ancestor_id
 WHERE ` + inheritedItemVisibleSQL + `
UNION ALL
SELECT ` + fmt.Sprintf(timelineItemViewColumns, "l.thread_id", importedItemRevExpr) + `
  FROM thread_fork_lineage l
  JOIN thread_import_chunks refs ON refs.thread_id = l.ancestor_id
  JOIN import_history_items items ON items.chunk_id = refs.chunk_id
 WHERE ` + importedNotOverridden + `
   AND ` + inheritedItemVisibleSQL + `;

-- Keep length(column) beside the physical blob: length(data) of a compound
-- would evaluate the whole blob just to answer a preview's byte count.
CREATE VIEW timeline_payloads AS
SELECT p.thread_id, p.id, p.kind, p.meta, p.data, p.created_at, p.preview_spans, p.spans, length(p.data) AS data_length
  FROM payloads p
UNION ALL
SELECT refs.thread_id, p.id, p.kind, p.meta, p.data, p.created_at, p.preview_spans, p.spans, length(p.data)
  FROM thread_import_chunks refs
  JOIN import_history_payloads p ON p.chunk_id = refs.chunk_id
 WHERE NOT EXISTS (SELECT 1 FROM payloads local WHERE local.thread_id = refs.thread_id AND local.id = p.id)
UNION ALL
SELECT l.thread_id, p.id, p.kind, p.meta, p.data, p.created_at, p.preview_spans, p.spans, length(p.data)
  FROM thread_fork_lineage l
  JOIN payloads p ON p.thread_id = l.ancestor_id
 WHERE ` + inheritedPayloadVisibleSQL("p.id") + `
UNION ALL
SELECT l.thread_id, p.id, p.kind, p.meta, p.data, p.created_at, p.preview_spans, p.spans, length(p.data)
  FROM thread_fork_lineage l
  JOIN thread_import_chunks refs ON refs.thread_id = l.ancestor_id
  JOIN import_history_payloads p ON p.chunk_id = refs.chunk_id
 WHERE NOT EXISTS (SELECT 1 FROM payloads local WHERE local.thread_id = refs.thread_id AND local.id = p.id)
   AND ` + inheritedPayloadVisibleSQL("p.id") + `;

CREATE VIEW timeline_payload_chunks AS
SELECT c.thread_id, c.payload_id, c.chunk_index, c.start_offset, c.data, c.created_at, length(c.data) AS data_length
  FROM payload_chunks c
UNION ALL
SELECT l.thread_id, c.payload_id, c.chunk_index, c.start_offset, c.data, c.created_at, length(c.data)
  FROM thread_fork_lineage l
  JOIN payload_chunks c ON c.thread_id = l.ancestor_id
 WHERE ` + inheritedPayloadVisibleSQL("c.payload_id") + `;

CREATE VIEW timeline_edit_file_snapshots AS
SELECT e.thread_id, e.payload_id, e.path, e.content, e.created_at
  FROM edit_file_snapshots e
UNION ALL
SELECT l.thread_id, e.payload_id, e.path, e.content, e.created_at
  FROM thread_fork_lineage l
  JOIN edit_file_snapshots e ON e.thread_id = l.ancestor_id
 WHERE ` + inheritedPayloadVisibleSQL("e.payload_id") + `;

CREATE VIEW timeline_turns AS
SELECT turns.turn_id, turns.thread_id, turns.turn_index, turns.started_at, turns.completed_at,
       turns.stop_reason, turns.assistant_message_id, turns.token_usage_json,
       turns.error_message, turns.provider_turn_id
  FROM turns
UNION ALL
SELECT turns.turn_id, l.thread_id, turns.turn_index, turns.started_at, turns.completed_at,
       turns.stop_reason, turns.assistant_message_id, turns.token_usage_json,
       turns.error_message, turns.provider_turn_id
  FROM thread_fork_lineage l
  JOIN turns ON turns.thread_id = l.ancestor_id
 WHERE ` + inheritedTurnVisibleSQL + `;
`

// forkTriggersSQL is the latest DDL for the pointer-fork triggers. Migration
// v116 installs it and RestoreFrom reinstalls it after the row copy, which
// runs without these triggers so restored rows are the snapshot's exactly.
//
//   - trg_threads_fork_history: a fork's reads include its ancestors' rows,
//     so its history stamp must move whenever theirs does. The lineage is
//     flattened, so one UPDATE reaches every descendant; recursive_triggers
//     is OFF, so the descendants' own bump does not re-enter this trigger.
//     Epoch moves with the ancestor's epoch. A write after the cut bumps the
//     fork too: stale is safe, fresh would not be.
//   - trg_threads_fork_source_delete: deleting a source must detach its
//     forks first (detachForkDescendantsTx), or they would read a thread
//     that no longer exists and never learn why.
//   - trg_items_fork_position / trg_items_fork_position_update: a fork's own
//     rows sit at or after its cut. The only exception is a row that
//     replaces an inherited one, which is hidden first.
//   - trg_items_fork_snapshot: a row an ancestor inserts below a fork's cut
//     after the fork was made (a background child, a late completion) is
//     not part of the fork's history, so the fork hides it. A row that
//     replaces one the inserting thread already showed under the same id
//     (a copy of a row it inherited, or its own imported row localized) is
//     the same history and stays visible to the forks.
//   - trg_items_fork_snapshot_move: the same rule for a row an ancestor
//     moves from at or after a fork's cut to before it. A move of a row a
//     fork reads is handed off first (handOffIDsTx), so the fork keeps
//     its position.
const forkTriggersSQL = `
CREATE TRIGGER trg_threads_fork_history AFTER UPDATE OF history_rev, history_epoch ON threads
WHEN NEW.history_rev IS NOT OLD.history_rev OR NEW.history_epoch IS NOT OLD.history_epoch
BEGIN
  UPDATE threads
     SET history_rev = history_rev + 1,
         history_epoch = history_epoch + (NEW.history_epoch IS NOT OLD.history_epoch)
   WHERE id IN (SELECT thread_id FROM thread_fork_lineage WHERE ancestor_id = NEW.id);
END;

CREATE TRIGGER trg_threads_fork_source_delete BEFORE DELETE ON threads
WHEN EXISTS (SELECT 1 FROM thread_fork_lineage WHERE ancestor_id = OLD.id)
BEGIN
  SELECT RAISE(ABORT, 'thread is the source of pointer forks; detach them before deleting it');
END;

CREATE TRIGGER trg_items_fork_position BEFORE INSERT ON items
WHEN EXISTS (
  SELECT 1 FROM thread_fork_lineage l
   WHERE l.thread_id = NEW.thread_id AND l.depth = 1
     AND (NEW.turn_index, NEW.item_index) < (l.cut_turn_index, l.cut_item_index)
) AND NOT EXISTS (
  SELECT 1 FROM thread_fork_hidden WHERE thread_id = NEW.thread_id AND item_id = NEW.id
)
BEGIN
  SELECT RAISE(ABORT, 'item position precedes the fork cut');
END;

CREATE TRIGGER trg_items_fork_position_update BEFORE UPDATE OF turn_index, item_index ON items
WHEN (OLD.turn_index IS NOT NEW.turn_index OR OLD.item_index IS NOT NEW.item_index)
 AND EXISTS (
  SELECT 1 FROM thread_fork_lineage l
   WHERE l.thread_id = NEW.thread_id AND l.depth = 1
     AND (NEW.turn_index, NEW.item_index) < (l.cut_turn_index, l.cut_item_index)
)
BEGIN
  SELECT RAISE(ABORT, 'item position precedes the fork cut');
END;

CREATE TRIGGER trg_items_fork_snapshot AFTER INSERT ON items
WHEN EXISTS (
  SELECT 1 FROM thread_fork_lineage l
   WHERE l.ancestor_id = NEW.thread_id
     AND (NEW.turn_index, NEW.item_index) < (l.cut_turn_index, l.cut_item_index)
) AND NOT EXISTS (
  SELECT 1 FROM thread_fork_hidden WHERE thread_id = NEW.thread_id AND item_id = NEW.id
) AND NOT EXISTS (
  SELECT 1 FROM thread_import_item_overrides WHERE thread_id = NEW.thread_id AND item_id = NEW.id
)
BEGIN
  INSERT OR IGNORE INTO thread_fork_hidden(thread_id, item_id)
  SELECT l.thread_id, NEW.id
    FROM thread_fork_lineage l
   WHERE l.ancestor_id = NEW.thread_id
     AND (NEW.turn_index, NEW.item_index) < (l.cut_turn_index, l.cut_item_index);
END;

CREATE TRIGGER trg_items_fork_snapshot_move AFTER UPDATE OF turn_index, item_index ON items
WHEN (OLD.turn_index IS NOT NEW.turn_index OR OLD.item_index IS NOT NEW.item_index)
 AND EXISTS (
  SELECT 1 FROM thread_fork_lineage l
   WHERE l.ancestor_id = NEW.thread_id
     AND (NEW.turn_index, NEW.item_index) < (l.cut_turn_index, l.cut_item_index)
     AND (OLD.turn_index, OLD.item_index) >= (l.cut_turn_index, l.cut_item_index)
)
BEGIN
  INSERT OR IGNORE INTO thread_fork_hidden(thread_id, item_id)
  SELECT l.thread_id, NEW.id
    FROM thread_fork_lineage l
   WHERE l.ancestor_id = NEW.thread_id
     AND (NEW.turn_index, NEW.item_index) < (l.cut_turn_index, l.cut_item_index)
     AND (OLD.turn_index, OLD.item_index) >= (l.cut_turn_index, l.cut_item_index);
END;
`

const dropForkTriggersSQL = `DROP TRIGGER IF EXISTS trg_threads_fork_history;
DROP TRIGGER IF EXISTS trg_threads_fork_source_delete;
DROP TRIGGER IF EXISTS trg_items_fork_position;
DROP TRIGGER IF EXISTS trg_items_fork_position_update;
DROP TRIGGER IF EXISTS trg_items_fork_snapshot;
DROP TRIGGER IF EXISTS trg_items_fork_snapshot_move;`
