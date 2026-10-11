package store

// Migration v143 adds standing agents and the wake supervisor's rows
// (docs/specs/standing-agents.md, "Standing agent" and "Wake supervisor").
//
// `standing_agents` marks a thread the user promoted: its home directory and
// whether a routine tick may notify. `agent_wakes` holds the loops and watches
// a thread registered. Both are authoritative: no provider history rebuilds
// them, and the sweep rebuilds its schedule from these rows at every boot.
// `last_output` keeps the body of a watch's most recent wake that never
// reached the agent, so the agent can read the event it missed.
// `session_start_failures` marks a thread whose session, started for a
// message the app queued, failed to start and raised the start-failure
// notification, so the first start that succeeds withdraws it even after a
// restart. All three cascade from their thread, so a deleted thread takes
// its agent row, loops, watches and mark with it.
const standingAgentsMigrationVersion = 143

const standingAgentsV143SQL = `
CREATE TABLE standing_agents (
    thread_id        TEXT PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE,
    home             TEXT NOT NULL,
    notify_on_tick   INTEGER NOT NULL DEFAULT 0 CHECK(notify_on_tick IN (0,1)),
    promoted_at      INTEGER NOT NULL
);
CREATE TABLE agent_wakes (
    id               TEXT PRIMARY KEY,
    thread_id        TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    kind             TEXT NOT NULL CHECK(kind IN ('loop','watch')),
    label            TEXT NOT NULL,
    note             TEXT NOT NULL DEFAULT '',
    command          TEXT NOT NULL DEFAULT '',
    cwd              TEXT NOT NULL DEFAULT '',
    interval_seconds INTEGER,
    timeout_seconds  INTEGER,
    long_running     INTEGER NOT NULL DEFAULT 0 CHECK(long_running IN (0,1)),
    next_at          INTEGER,
    last_run_at      INTEGER,
    last_outcome     TEXT NOT NULL DEFAULT '',
    last_output      TEXT NOT NULL DEFAULT '',
    missed           INTEGER NOT NULL DEFAULT 0,
    state            TEXT NOT NULL CHECK(state IN ('armed','running','cancelled')),
    created_at       INTEGER NOT NULL
);
CREATE INDEX idx_agent_wakes_due ON agent_wakes(state, next_at);
CREATE TABLE session_start_failures (
    thread_id        TEXT PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE,
    failed_at        INTEGER NOT NULL
);
`
