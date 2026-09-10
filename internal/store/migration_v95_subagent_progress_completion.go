package store

// A detached Claude launch's final subagent counters (`meta.subagentProgress`)
// used to be written onto the launch row at its terminal. The launch row is
// the immutable spawn event; the completion sibling (`completion_of`) is the
// record that carries the execution's settled values, for Claude and Codex
// alike (docs/specs/agent-visibility.md §Immutable agent history). Move the
// numbers a sibling is missing onto it and strip them from launches that have
// a sibling. Codex spawns never carried them. A background launch that never
// settled keeps its numbers: there is no sibling to move them to yet, and the
// session-end settle that writes one folds the live entry, not the row.
const subagentProgressCompletionV95SQL = `
UPDATE items
   SET meta = json_set(
         CASE WHEN json_valid(meta) THEN meta ELSE '{}' END,
         '$.subagentProgress',
         (SELECT json(json_extract(launch.meta, '$.subagentProgress'))
            FROM items launch
           WHERE launch.thread_id = items.thread_id
             AND launch.id = items.completion_of)
       )
 WHERE completion_of <> ''
   AND kind = 'tool_completion'
   AND tool_name <> 'collab_agent'
   AND (NOT json_valid(meta) OR json_extract(meta, '$.subagentProgress') IS NULL)
   AND EXISTS (
     SELECT 1 FROM items launch
      WHERE launch.thread_id = items.thread_id
        AND launch.id = items.completion_of
        AND launch.kind = 'tool_call'
        AND launch.is_background = 1
        AND json_valid(launch.meta)
        AND json_type(launch.meta, '$.subagentProgress') = 'object'
   );

UPDATE items
   SET meta = json_remove(meta, '$.subagentProgress')
 WHERE kind = 'tool_call'
   AND is_background = 1
   AND tool_name <> 'collab_agent'
   AND json_valid(meta)
   AND json_extract(meta, '$.subagentProgress') IS NOT NULL
   AND EXISTS (
     SELECT 1 FROM items c
      WHERE c.thread_id = items.thread_id
        AND c.completion_of = items.id
        AND c.completion_of <> ''
   );
`
