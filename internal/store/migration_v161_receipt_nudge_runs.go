package store

// Migration v161 records the runs nudges of a paired computer's owned
// worker here start (receipt_runs.go, StartNudgeRun): a message the person
// or another thread sends the worker while no run of the spawner's is open
// on it. This computer alone sees the worker rest, so each run is kept here,
// one row in thread_receipt_nudges, reported to the spawner's computer in
// its agent_rows answers until that computer says it took the run's end.
// `link_token` is the spawn's link (its receipt), `target_thread_id` the
// worker, `started_at` and `run_base_*` the run's start and the worker's
// figures over its life then; `run_open` is 1 until the worker's rest, its
// sidebar row or the link's end, then 0 with `ended_at` and the worker's
// figures over its life then (`run_tool_calls`, `run_tokens`,
// `run_turn_ms`). idx_thread_receipt_nudges_open holds the open runs by
// thread, for the rest pass; idx_thread_receipt_nudges_link a link's runs
// for its answer; idx_thread_receipt_nudges_ended the ended runs by end, for
// retention.
//
// The table is new: no row is rewritten.
const receiptNudgeRunsMigrationVersion = 161

const receiptNudgeRunsV161SQL = `
CREATE TABLE thread_receipt_nudges (
    token               TEXT    PRIMARY KEY,
    link_token          TEXT    NOT NULL REFERENCES thread_request_receipts(token) ON DELETE CASCADE,
    target_thread_id    TEXT    NOT NULL,
    started_at          INTEGER NOT NULL,
    run_open            INTEGER NOT NULL CHECK(run_open IN (0,1)),
    run_base_tool_calls INTEGER NOT NULL,
    run_base_tokens     INTEGER NOT NULL,
    run_base_turn_ms    INTEGER NOT NULL,
    run_tool_calls      INTEGER,
    run_tokens          INTEGER,
    run_turn_ms         INTEGER,
    ended_at            INTEGER
);
CREATE INDEX idx_thread_receipt_nudges_open
    ON thread_receipt_nudges(target_thread_id)
 WHERE run_open = 1;
CREATE INDEX idx_thread_receipt_nudges_link
    ON thread_receipt_nudges(link_token, started_at);
CREATE INDEX idx_thread_receipt_nudges_ended
    ON thread_receipt_nudges(ended_at)
 WHERE run_open = 0;
`
