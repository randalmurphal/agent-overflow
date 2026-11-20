package store

// Migration v161 records, on a paired computer's spawn link here, the run a
// nudge of its worker starts (receipt_runs.go, StartNudgeRun): a message
// the person or another thread sends the owned worker while no run of the
// spawner's is open on it. This computer alone sees the worker rest, so the
// run is kept here and reported to the spawner's computer in its agent_rows
// answer, which records the nudge's row there. `nudge_token` names the run,
// `nudge_base_tool_calls`, `nudge_base_tokens` and `nudge_base_turn_ms` are
// the worker's figures over its life when it began and `nudge_started_at`
// when, and `nudge_open` is 1 while it is open and 0 once it ended, with the
// worker's figures over its life then (`nudge_tool_calls`, `nudge_tokens`,
// `nudge_turn_ms`). The link keeps its latest nudge only. NULL is none.
// idx_thread_request_receipts_nudge_open holds the open ones by thread, for
// the rest pass.
//
// The columns are added empty: no row is rewritten.
const receiptNudgeRunsMigrationVersion = 161

const receiptNudgeRunsV161SQL = `
ALTER TABLE thread_request_receipts ADD COLUMN nudge_token TEXT;
ALTER TABLE thread_request_receipts ADD COLUMN nudge_open INTEGER CHECK(nudge_open IN (0,1));
ALTER TABLE thread_request_receipts ADD COLUMN nudge_started_at INTEGER;
ALTER TABLE thread_request_receipts ADD COLUMN nudge_base_tool_calls INTEGER;
ALTER TABLE thread_request_receipts ADD COLUMN nudge_base_tokens INTEGER;
ALTER TABLE thread_request_receipts ADD COLUMN nudge_base_turn_ms INTEGER;
ALTER TABLE thread_request_receipts ADD COLUMN nudge_tool_calls INTEGER;
ALTER TABLE thread_request_receipts ADD COLUMN nudge_tokens INTEGER;
ALTER TABLE thread_request_receipts ADD COLUMN nudge_turn_ms INTEGER;
CREATE INDEX idx_thread_request_receipts_nudge_open
    ON thread_request_receipts(target_thread_id)
 WHERE nudge_open = 1;
`
