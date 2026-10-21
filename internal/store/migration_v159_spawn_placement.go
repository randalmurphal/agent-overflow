package store

// Migration v159 records where the sidebar shows a thread: nested under the
// standing agent that spawned it, grouped in a group of its own project, or
// standalone as a plain row (docs/specs/standing-agents.md ruling 36).
//
// The CHECK makes a row grouped exactly when it has a group, so neither
// half of membership can be written without the other. SQLite tests a CHECK
// on an added column against every existing row, and no constant default
// satisfies it for both grouped and ungrouped rows, so the groups are set
// aside while the column is added and restored with grouped in one write.
// An ungrouped thread that a local spawn row names was nested under its
// agent and stays nested; every other thread is standalone, an assistant's
// current thread included (an assistant another one created has a spawn
// row naming it, and its own row takes no placement). An assistant's past
// threads live outside every project, under their assistant, and take no
// placement either.
//
// Nested needs that link. A thread created later is inserted standalone or
// grouped, and only the write that names a local spawn's thread and
// SetThreadPlacement make it nested, each with the link present. The
// trigger keeps the rule when the last link goes: a spawner's delete (its
// spawn rows cascade) leaves the thread standalone.
const spawnPlacementMigrationVersion = 159

const spawnPlacementV159SQL = `
CREATE TEMP TABLE v159_thread_groups AS
  SELECT id, group_id FROM threads WHERE group_id IS NOT NULL;
UPDATE threads SET group_id = NULL WHERE group_id IS NOT NULL;

ALTER TABLE threads ADD COLUMN placement TEXT NOT NULL DEFAULT 'standalone'
    CHECK(placement IN ('nested','grouped','standalone')
      AND (placement = 'grouped') = (group_id IS NOT NULL));

UPDATE threads SET placement = 'grouped',
       group_id = (SELECT g.group_id FROM v159_thread_groups AS g WHERE g.id = threads.id)
 WHERE id IN (SELECT id FROM v159_thread_groups);
DROP TABLE v159_thread_groups;

UPDATE threads SET placement = 'nested'
 WHERE placement = 'standalone'
   AND NOT EXISTS (SELECT 1 FROM agents WHERE agents.thread_id = threads.id)
   AND EXISTS (SELECT 1 FROM thread_requests
                WHERE kind = 'spawn' AND target_computer_id = '' AND target_thread_id = threads.id);

CREATE TRIGGER trg_thread_placement_spawn_gone AFTER DELETE ON thread_requests
WHEN OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.target_thread_id <> ''
BEGIN
  UPDATE threads SET placement = 'standalone'
   WHERE id = OLD.target_thread_id AND placement = 'nested'
     AND NOT EXISTS (SELECT 1 FROM thread_requests
                      WHERE kind = 'spawn' AND target_computer_id = '' AND target_thread_id = OLD.target_thread_id);
END;
`
