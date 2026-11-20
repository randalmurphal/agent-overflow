package store

// Migration v159 records the run each spawn, and each send to a thread its
// caller spawned, counts (docs/specs/standing-agents.md, "Agents segment"):
// what its worker had done when the run began (`run_base_tool_calls`,
// `run_base_tokens`, `run_base_turn_ms`, from thread_tallies), and what the
// run did once the worker came to rest after it (`run_tool_calls`,
// `run_tokens`, `run_turn_ms`). A base with no total is a run still open; a
// row with no base tracks no run (request_runs.go says how such a spawn reads).
// idx_thread_requests_run_open holds only the open runs, which the Agents
// listing, a new request's base and the rest pass read by caller.
// `run_token` names the run a request counts by its first request's token,
// its own for the first: every Agents read takes one request per run
// through idx_thread_requests_run_token, and a worker's newest run through
// idx_thread_requests_run_heads, which holds each run's first request.
//
// A paired computer's request keeps the same run on its receipt here, the
// worker's computer, which alone sees the worker rest (receipt_runs.go):
// `run_open` is 1 while it is open and 0 once it ended, with the worker's
// figures over its life then (`run_tool_calls`, `run_tokens`,
// `run_turn_ms`). NULL tracks no run. idx_thread_request_receipts_run_open
// holds the open ones by thread.
//
// The columns are added empty: no row is rewritten.
const requestRunsMigrationVersion = 159

const requestRunsV159SQL = `
ALTER TABLE thread_requests ADD COLUMN run_base_tool_calls INTEGER;
ALTER TABLE thread_requests ADD COLUMN run_base_tokens INTEGER;
ALTER TABLE thread_requests ADD COLUMN run_base_turn_ms INTEGER;
ALTER TABLE thread_requests ADD COLUMN run_tool_calls INTEGER;
ALTER TABLE thread_requests ADD COLUMN run_tokens INTEGER;
ALTER TABLE thread_requests ADD COLUMN run_turn_ms INTEGER;
ALTER TABLE thread_requests ADD COLUMN run_token TEXT;
CREATE INDEX idx_thread_requests_run_open
    ON thread_requests(caller_thread_id, created_at)
 WHERE run_base_tool_calls IS NOT NULL AND run_tool_calls IS NULL;
CREATE INDEX idx_thread_requests_run_token
    ON thread_requests(run_token, settled_at, created_at, token)
 WHERE run_token IS NOT NULL;
CREATE INDEX idx_thread_requests_run_heads
    ON thread_requests(caller_thread_id, target_computer_id, target_thread_id, created_at)
 WHERE run_token = token;
ALTER TABLE thread_request_receipts ADD COLUMN run_open INTEGER CHECK(run_open IN (0,1));
ALTER TABLE thread_request_receipts ADD COLUMN run_tool_calls INTEGER;
ALTER TABLE thread_request_receipts ADD COLUMN run_tokens INTEGER;
ALTER TABLE thread_request_receipts ADD COLUMN run_turn_ms INTEGER;
CREATE INDEX idx_thread_request_receipts_run_open
    ON thread_request_receipts(target_thread_id)
 WHERE run_open = 1;
`
