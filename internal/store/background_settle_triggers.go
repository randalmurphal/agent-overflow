package store

// Schema-owned settlement of the background-launch liveness flag.
//
// A backgrounded `tool_call` stays `status='running'` forever
// (invariant 24): its terminal state is a SIBLING row that names it
// through `completion_of`. "Live" is therefore not expressible as a
// status, and every reader spells it as
// `running AND live_background_active != 0 AND NOT EXISTS <completion
// sibling>`. That third term is the one a partial index cannot carry —
// it is correlated — so before these triggers the partial "live"
// indexes (`idx_items_live_background`, `idx_items_running_bg_tool_calls`)
// matched every historical launch a thread had ever backgrounded
// (measured 2026-09-02 on a real history: 2,883 rows across 157
// threads, 494 on one thread) and every seed that started from them
// walked the whole thread.
//
// These four triggers move the correlated half onto the launch row at
// write time, so `live_background_active != 0` on a running background
// launch means "no session teardown AND no completion sibling" and the
// partial indexes hold only genuinely live rows.
//
// Readers keep their existing predicates. The no-completion-sibling
// term is now redundant with the flag, but it is the documented
// contract and the belt that keeps a reader correct if a row is ever
// written around the triggers (an ATTACH-side copy, a hand repair);
// each such probe is one covering-index seek on `idx_items_completion_of`.
//
// Recursion: SQLite's `recursive_triggers` is OFF by default and this
// store never turns it on (`dsn.go` carries no such pragma), so a
// trigger's own UPDATE fires nothing further. The chain would terminate
// anyway — every WHEN clause requires `live_background_active != 0`
// (or, for the revive trigger, `= 0`), which the trigger's own UPDATE
// makes false — so the property does not depend on the pragma.
// `TestBackgroundSettleTriggersDoNotRecurse` pins it either way.
//
// `updated_at` is deliberately NOT touched: settlement is a derived
// fact about a row whose user-visible content did not change, and the
// tray/timeline clock retention off the completion sibling's
// `created_at`.
const backgroundSettleTriggersSQL = `CREATE TRIGGER trg_items_settle_bg_launch_on_completion
AFTER INSERT ON items
WHEN NEW.completion_of <> ''
BEGIN
  UPDATE items
     SET meta = json_set(
           CASE WHEN json_valid(meta) THEN meta ELSE '{}' END,
           '$.live_background_active',
           json('false')
         )
   WHERE thread_id = NEW.thread_id
     AND id = NEW.completion_of
     AND kind = 'tool_call'
     AND status = 'running'
     AND is_background = 1
     AND COALESCE(json_extract(meta, '$.live_background_active'), 1) != 0;
END;

CREATE TRIGGER trg_items_settle_bg_launch_on_launch_insert
AFTER INSERT ON items
WHEN NEW.kind = 'tool_call'
 AND NEW.status = 'running'
 AND NEW.is_background = 1
 AND COALESCE(json_extract(NEW.meta, '$.live_background_active'), 1) != 0
 AND EXISTS (
   SELECT 1 FROM items c
    WHERE c.thread_id = NEW.thread_id
      AND c.completion_of = NEW.id
      AND c.completion_of <> ''
 )
BEGIN
  UPDATE items
     SET meta = json_set(
           CASE WHEN json_valid(meta) THEN meta ELSE '{}' END,
           '$.live_background_active',
           json('false')
         )
   WHERE thread_id = NEW.thread_id
     AND id = NEW.id;
END;

CREATE TRIGGER trg_items_settle_bg_launch_on_update
AFTER UPDATE ON items
WHEN NEW.kind = 'tool_call'
 AND NEW.status = 'running'
 AND NEW.is_background = 1
 AND COALESCE(json_extract(NEW.meta, '$.live_background_active'), 1) != 0
 AND EXISTS (
   SELECT 1 FROM items c
    WHERE c.thread_id = NEW.thread_id
      AND c.completion_of = NEW.id
      AND c.completion_of <> ''
 )
BEGIN
  UPDATE items
     SET meta = json_set(
           CASE WHEN json_valid(meta) THEN meta ELSE '{}' END,
           '$.live_background_active',
           json('false')
         )
   WHERE thread_id = NEW.thread_id
     AND id = NEW.id;
END;

CREATE TRIGGER trg_items_revive_bg_launch_on_completion_delete
AFTER DELETE ON items
WHEN OLD.completion_of <> ''
BEGIN
  UPDATE items
     SET meta = json_remove(meta, '$.live_background_active')
   WHERE thread_id = OLD.thread_id
     AND id = OLD.completion_of
     AND kind = 'tool_call'
     AND status = 'running'
     AND is_background = 1
     AND json_valid(meta)
     AND json_extract(meta, '$.live_background_active') = 0
     AND NOT EXISTS (
       SELECT 1 FROM items c
        WHERE c.thread_id = OLD.thread_id
          AND c.completion_of = OLD.completion_of
          AND c.completion_of <> ''
     );
END;`

// dropBackgroundSettleTriggersSQL removes the four triggers above.
// `RestoreFrom` brackets its whole-database row copy with it for the
// same reason it brackets the history-rev triggers: the snapshot's rows
// were already settled by the triggers that were live when it was
// taken, so re-deriving the flag row by row during the copy is pure
// work. A future `items` rebuild must drop and re-create them the way
// migration v72 does for the history-rev set —
// `TestItemsRebuildMigrationsReinstallBackgroundSettleTriggers` fails
// the build if one forgets.
const dropBackgroundSettleTriggersSQL = `DROP TRIGGER IF EXISTS trg_items_settle_bg_launch_on_completion;
DROP TRIGGER IF EXISTS trg_items_settle_bg_launch_on_launch_insert;
DROP TRIGGER IF EXISTS trg_items_settle_bg_launch_on_update;
DROP TRIGGER IF EXISTS trg_items_revive_bg_launch_on_completion_delete;`

// backgroundSettleTriggerMigrationVersion is the chain version that
// installs the triggers and backfills history. Named once so the
// migration entry, the rebuild tripwire, and the backfill test cannot
// disagree about it — the number is the one thing a merge can change.
const backgroundSettleTriggerMigrationVersion = 74

// backgroundSettleTriggerNames is the roster the schema tests assert
// against, so "a trigger was added but nothing re-installs it" and "a
// trigger was dropped by a rebuild" are both one-line failures.
var backgroundSettleTriggerNames = []string{
	"trg_items_settle_bg_launch_on_completion",
	"trg_items_settle_bg_launch_on_launch_insert",
	"trg_items_settle_bg_launch_on_update",
	"trg_items_revive_bg_launch_on_completion_delete",
}

// backfillSettledBackgroundLaunchesSQL stamps every pre-existing
// running background launch that already has a completion sibling, so
// the invariant the triggers maintain going forward also holds for
// history. Served by the partial `idx_items_running_bg_tool_calls`
// (full index scan, ~3.7k entries on a real 6GB history) with the
// sibling probe on the covering `idx_items_completion_of`; the
// non-empty `c.completion_of` term is what qualifies that partial
// index (see noCompletionSiblingSQL).
const backfillSettledBackgroundLaunchesSQL = `UPDATE items
   SET meta = json_set(
         CASE WHEN json_valid(meta) THEN meta ELSE '{}' END,
         '$.live_background_active',
         json('false')
       )
 WHERE kind = 'tool_call'
   AND status = 'running'
   AND is_background = 1
   AND COALESCE(json_extract(meta, '$.live_background_active'), 1) != 0
   AND EXISTS (
     SELECT 1 FROM items c
      WHERE c.thread_id = items.thread_id
        AND c.completion_of = items.id
        AND c.completion_of <> ''
   );`
