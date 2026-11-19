package store

// Migration v159 records the run each spawn, and each send to a thread its
// caller spawned, counts (docs/specs/standing-agents.md, "Agents segment"):
// what its worker had done when the run began (`run_base_tool_calls`,
// `run_base_tokens`, `run_base_turn_ms`, from thread_tallies), and what the
// run did once the worker came to rest after it (`run_tool_calls`,
// `run_tokens`, `run_turn_ms`). A base with no total is a run still open; a
// row with no base is untracked and reads as rows did before (request_runs.go).
// The columns are added empty: no row is rewritten.
// idx_thread_requests_run_open holds only the open runs, which the Agents
// listing, a new request's base and the rest pass read by caller.
const requestRunsMigrationVersion = 159

const requestRunsV159SQL = `
ALTER TABLE thread_requests ADD COLUMN run_base_tool_calls INTEGER;
ALTER TABLE thread_requests ADD COLUMN run_base_tokens INTEGER;
ALTER TABLE thread_requests ADD COLUMN run_base_turn_ms INTEGER;
ALTER TABLE thread_requests ADD COLUMN run_tool_calls INTEGER;
ALTER TABLE thread_requests ADD COLUMN run_tokens INTEGER;
ALTER TABLE thread_requests ADD COLUMN run_turn_ms INTEGER;
CREATE INDEX idx_thread_requests_run_open
    ON thread_requests(caller_thread_id, created_at)
 WHERE run_base_tool_calls IS NOT NULL AND run_tool_calls IS NULL;
`
