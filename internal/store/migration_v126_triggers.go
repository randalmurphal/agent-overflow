package store

// The trigger texts migrations v126 and v127 installed, frozen with their
// hashes. Migration v133 dropped the two revive triggers; the live settle
// triggers are backgroundSettleTriggersSQL and the live fork triggers
// forkTriggersSQL.

// backgroundSettleTriggersV126SQL is the settle trigger set v126 installed,
// with trg_items_revive_bg_launch_on_completion_delete.
const backgroundSettleTriggersV126SQL = `CREATE TRIGGER trg_items_settle_bg_launch_on_completion
AFTER INSERT ON items
WHEN NEW.completion_of <> '' AND NEW.status <> 'parked'
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
      AND c.status <> 'parked'
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
      AND c.status <> 'parked'
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
WHEN OLD.completion_of <> '' AND OLD.status <> 'parked'
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
          AND c.status <> 'parked'
     );
END;`

// dropBackgroundSettleTriggersV126SQL drops the set v126 replaced.
const dropBackgroundSettleTriggersV126SQL = `DROP TRIGGER IF EXISTS trg_items_settle_bg_launch_on_completion;
DROP TRIGGER IF EXISTS trg_items_settle_bg_launch_on_launch_insert;
DROP TRIGGER IF EXISTS trg_items_settle_bg_launch_on_update;
DROP TRIGGER IF EXISTS trg_items_revive_bg_launch_on_completion_delete;`

// dropForkTriggersV127SQL drops the fork triggers v127 replaced.
const dropForkTriggersV127SQL = `DROP TRIGGER IF EXISTS trg_threads_fork_source_delete;
DROP TRIGGER IF EXISTS trg_items_fork_position;
DROP TRIGGER IF EXISTS trg_items_fork_position_update;
DROP TRIGGER IF EXISTS trg_items_fork_snapshot;
DROP TRIGGER IF EXISTS trg_items_fork_snapshot_move;
DROP TRIGGER IF EXISTS trg_items_shown_update;
DROP TRIGGER IF EXISTS trg_items_shown_delete;
DROP TRIGGER IF EXISTS trg_payloads_shown_update;
DROP TRIGGER IF EXISTS trg_payload_chunks_shown_insert;
DROP TRIGGER IF EXISTS trg_payload_chunks_shown_update;
DROP TRIGGER IF EXISTS trg_payload_chunks_shown_delete;
DROP TRIGGER IF EXISTS trg_turns_shown_update;
DROP TRIGGER IF EXISTS trg_turns_shown_delete;
DROP TRIGGER IF EXISTS trg_items_revive_bg_launch_on_completion_move;
DROP TRIGGER IF EXISTS trg_thread_fork_lineage_release;`

// reviveBgLaunchOnCompletionMoveV127SQL is the revive-on-move trigger v127
// installed in place of v125's.
const reviveBgLaunchOnCompletionMoveV127SQL = `CREATE TRIGGER trg_items_revive_bg_launch_on_completion_move AFTER UPDATE OF thread_id ON items
WHEN OLD.completion_of <> '' AND OLD.status <> 'parked' AND OLD.thread_id IS NOT NEW.thread_id
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
          AND c.status <> 'parked'
     );
END;

`
