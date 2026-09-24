package store

// Write-time aggregates: subagent anchor cards (subagent_aggregate_stamps.go)
// and the thread row's turn-error pair (thread_turn_error_aggregate.go).
//
//   - idx_items_subagent_resume_prompt is the round probe: the latest
//     resume prompt under an anchor at or before a written row's position.
//     Its predicate is aggPromptSQL's first three terms.
//   - idx_items_subagent_aggregate_dirty is the dirty-anchor record: the
//     flag lives on the anchor's own meta, written by the same UPDATE that
//     invalidates the stamp, and the partial index finds a thread's dirty
//     anchors by key.
//   - subagent_aggregate_backfill is the migration's deferred phase: the
//     threads whose anchors predate the stamps. After the store opens the
//     app stamps them in paced RecomputeSubagentAggregates batches
//     (internal/app/app_subagent_aggregate_backfill.go), removing each
//     thread with its last anchor, so a quit resumes from what is left and
//     an empty table is the applied phase. Until then a listed thread's
//     unstamped anchors are read through the read-time aggregator.
//   - idx_items_running_nested_fg_tool_calls is the tray's nested
//     candidate set (ListLiveBackgroundTasks): foreground tool calls in
//     flight below the top level.
//   - the three history triggers, reinstalled from historyRevTriggersSQL.
//   - threads.newest_turn_error_at and newest_turn_error_turn, their probe
//     indexes and triggers (threadTurnErrorSchemaSQL). The backfill is one
//     inline UPDATE: it probes each thread through the same two indexes,
//     not its rows.
var writeTimeAggregatesV121SQL = `
CREATE INDEX idx_items_subagent_resume_prompt
    ON items(thread_id, parent_id, turn_index, item_index)
 WHERE kind = 'user_text' AND parent_id <> ''
   AND json_type(meta, '$.` + metaKeySubagentResumePrompt + `') = 'true';

CREATE INDEX idx_items_subagent_aggregate_dirty
    ON items(thread_id)
 WHERE ` + subagentAggregateDirtyPredicate + `;

CREATE INDEX idx_items_running_nested_fg_tool_calls
    ON items(thread_id, id)
 WHERE kind = 'tool_call'
   AND status = 'running'
   AND is_background = 0
   AND parent_id <> '';

CREATE TABLE subagent_aggregate_backfill (
    thread_id TEXT PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE
) WITHOUT ROWID;

INSERT INTO subagent_aggregate_backfill(thread_id)
SELECT id FROM threads
 WHERE EXISTS (SELECT 1 FROM items WHERE items.thread_id = threads.id AND items.kind = 'tool_call');

` + dropHistoryRevTriggersSQL + historyRevTriggersSQL + `
` + threadTurnErrorSchemaSQL + `
`

// subagentAggregateDirtyPredicate is idx_items_subagent_aggregate_dirty's
// predicate, stated verbatim by every query that selects dirty anchors.
const subagentAggregateDirtyPredicate = `kind = 'tool_call' AND json_extract(meta, '` + aggDirtyPath + `') = 1`
