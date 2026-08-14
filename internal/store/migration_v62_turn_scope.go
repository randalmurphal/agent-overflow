package store

// turnThreadScopeMigrationSQL moves legacy Codex turns from their bare wire
// ids into the same thread-scoped key space new writes use. Usage attribution
// is rewritten first while the old key is still available for correlation.
// Claude turns and cloned fork turns already start with `<threadID>:` and are
// deliberately left byte-for-byte unchanged.
const turnThreadScopeMigrationSQL = `
UPDATE usage_ledger AS usage
   SET turn_id = (
       SELECT turn.thread_id || ':' || turn.turn_id
         FROM turns AS turn
        WHERE turn.thread_id = usage.thread_id
          AND turn.turn_id = usage.turn_id
          AND substr(turn.turn_id, 1, length(turn.thread_id) + 1) <> turn.thread_id || ':'
   )
 WHERE usage.turn_id <> ''
   AND EXISTS (
       SELECT 1
         FROM turns AS turn
        WHERE turn.thread_id = usage.thread_id
          AND turn.turn_id = usage.turn_id
          AND substr(turn.turn_id, 1, length(turn.thread_id) + 1) <> turn.thread_id || ':'
   );

UPDATE turns
   SET turn_id = thread_id || ':' || turn_id
 WHERE substr(turn_id, 1, length(thread_id) + 1) <> thread_id || ':';
`
