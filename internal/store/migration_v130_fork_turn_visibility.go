package store

// Migration v130 makes a pointer fork read a turn row from a nearer level
// only where it reads that level. A fork owns its cut turn's row, and a
// nearer level's row of a turn replaces a deeper level's, so the fork's
// own forks read one row per turn. A split (splitShownRowsTx) lowers a
// reader's cut on the thread that reverted and leaves its cuts on the
// levels behind, whose rows the reader still reads past the lowered cut;
// the thread's later turn rows there are not the reader's history, and
// until v130 they hid the rows it read. The view and the guards of shown
// turn rows now apply the same rule.

// forkTurnVisibleSQL is the turn half of a fork's reads (inheritedTurnVisibleSQL
// until v130), frozen with v130: an ancestor's turn row is visible below
// that level's cut turn, where the reader holds no row of its own and no
// nearer level holds one below its cut.
const forkTurnVisibleSQL = `turns.turn_index < l.cut_turn_index
   AND NOT EXISTS (
       SELECT 1 FROM turns held WHERE held.thread_id = l.thread_id AND held.turn_index = turns.turn_index
   )
   AND NOT EXISTS (
       SELECT 1 FROM thread_fork_lineage nearer
         JOIN turns held ON held.thread_id = nearer.ancestor_id AND held.turn_index = turns.turn_index
        WHERE nearer.thread_id = l.thread_id AND nearer.depth < l.depth
          AND nearer.cut_turn_index > turns.turn_index
   )`

const forkTurnVisibilityV130SQL = `
DROP VIEW timeline_turns;

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
 WHERE ` + forkTurnVisibleSQL + `;

DROP TRIGGER trg_turns_shown_update;
DROP TRIGGER trg_turns_shown_delete;

CREATE TRIGGER trg_turns_shown_update BEFORE UPDATE OF turn_index, started_at, completed_at, stop_reason,
    assistant_message_id, token_usage_json, error_message, provider_turn_id ON turns
WHEN (OLD.turn_index IS NOT NEW.turn_index OR OLD.started_at IS NOT NEW.started_at
      OR OLD.completed_at IS NOT NEW.completed_at OR OLD.stop_reason IS NOT NEW.stop_reason
      OR OLD.assistant_message_id IS NOT NEW.assistant_message_id
      OR OLD.token_usage_json IS NOT NEW.token_usage_json OR OLD.error_message IS NOT NEW.error_message
      OR OLD.provider_turn_id IS NOT NEW.provider_turn_id)
 AND EXISTS (SELECT 1 FROM thread_fork_lineage l
   WHERE l.ancestor_id = OLD.thread_id AND l.cut_turn_index > OLD.turn_index
     AND NOT EXISTS (SELECT 1 FROM turns held WHERE held.thread_id = l.thread_id AND held.turn_index = OLD.turn_index)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_lineage nearer
                       JOIN turns held ON held.thread_id = nearer.ancestor_id AND held.turn_index = OLD.turn_index
                      WHERE nearer.thread_id = l.thread_id AND nearer.depth < l.depth
                        AND nearer.cut_turn_index > OLD.turn_index))
BEGIN
  SELECT RAISE(ABORT, 'history another thread shows is immutable');
END;

CREATE TRIGGER trg_turns_shown_delete BEFORE DELETE ON turns
WHEN EXISTS (SELECT 1 FROM thread_fork_lineage l
   WHERE l.ancestor_id = OLD.thread_id AND l.cut_turn_index > OLD.turn_index
     AND NOT EXISTS (SELECT 1 FROM turns held WHERE held.thread_id = l.thread_id AND held.turn_index = OLD.turn_index)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_lineage nearer
                       JOIN turns held ON held.thread_id = nearer.ancestor_id AND held.turn_index = OLD.turn_index
                      WHERE nearer.thread_id = l.thread_id AND nearer.depth < l.depth
                        AND nearer.cut_turn_index > OLD.turn_index))
BEGIN
  SELECT RAISE(ABORT, 'history another thread shows is immutable');
END;
`
