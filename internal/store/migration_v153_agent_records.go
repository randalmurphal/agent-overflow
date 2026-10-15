package store

// Migration v153 makes a standing agent a record of its own rather than a
// mark on a thread (docs/specs/standing-agents.md, "Standing agent").
//
// `agents` is the assistant: its own id, its current thread, its home, its
// tick setting and the assistant that created it. The unique `thread_id`
// keeps one current thread per assistant, and it cascades from that thread,
// so deleting the current thread deletes the assistant. `agent_past_threads`
// lists the threads an assistant had before, with when each was superseded;
// it cascades from the thread and from the assistant, so deleting a past
// thread leaves the assistant alone and deleting the assistant leaves its
// past threads as ordinary threads. `thread_requests.made_in_thread_id` is
// the thread that made a request a handoff moved to its assistant's next
// thread, empty when the caller made it.
//
// Every standing_agents row becomes an assistant whose id is its thread's
// id, so the home `<config>/agents/<thread-id>` the promotion made is the
// record's own `<config>/agents/<agent-id>`.
const agentRecordsMigrationVersion = 153

const agentRecordsV153SQL = `
CREATE TABLE agents (
    id               TEXT PRIMARY KEY,
    thread_id        TEXT NOT NULL UNIQUE REFERENCES threads(id) ON DELETE CASCADE,
    home             TEXT NOT NULL,
    notify_on_tick   INTEGER NOT NULL DEFAULT 0 CHECK(notify_on_tick IN (0,1)),
    creator_agent_id TEXT NOT NULL DEFAULT '',
    created_at       INTEGER NOT NULL
);
CREATE TABLE agent_past_threads (
    thread_id        TEXT PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE,
    agent_id         TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    superseded_at    INTEGER NOT NULL
);
CREATE INDEX idx_agent_past_threads_agent ON agent_past_threads(agent_id, superseded_at DESC);
INSERT INTO agents (id, thread_id, home, notify_on_tick, creator_agent_id, created_at)
    SELECT thread_id, thread_id, home, notify_on_tick, '', promoted_at FROM standing_agents;
DROP TABLE standing_agents;
ALTER TABLE thread_requests ADD COLUMN made_in_thread_id TEXT NOT NULL DEFAULT '';
`
