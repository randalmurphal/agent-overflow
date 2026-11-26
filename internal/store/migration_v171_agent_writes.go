package store

// Migration v171 adds tracker_agent_writes: each part of a tracker write an
// agent made through the tracker tools, by the ticket it touched (the
// tracker kind, the site and the issue, keyed as thread_tickets is, so a
// connection removed and added again keeps them, with the key a create
// made), the thread whose agent
// made it and the call it was part of, when it was sent (sent_ms) and when
// its row was written (at_ms). A comment or a create is written in state
// sent before it is sent, with a digest of what it adds, and replaced by
// its outcome, so a call repeated after a lost answer finds it. The ID is
// AUTOINCREMENT, never reused: a claim's row is deleted when its write is
// refused, and a caller still holding that ID must never settle another
// write's row. No
// foreign key to threads: a deleted thread's writes keep marking the
// ticket, without a link.
const agentWritesV171SQL = `
CREATE TABLE tracker_agent_writes (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 kind TEXT NOT NULL CHECK(kind IN ('jira', 'linear')),
 site TEXT NOT NULL CHECK(site <> ''),
 issue_id TEXT NOT NULL DEFAULT '',
 issue_key TEXT NOT NULL DEFAULT '',
 project TEXT NOT NULL DEFAULT '',
 account TEXT NOT NULL DEFAULT '',
 thread_id TEXT NOT NULL CHECK(thread_id <> ''),
 call_id TEXT NOT NULL CHECK(call_id <> ''),
 step TEXT NOT NULL CHECK(step IN ('create', 'edit', 'move', 'estimate', 'sprint_move', 'comment')),
 state TEXT NOT NULL CHECK(state IN ('sent', 'applied', 'unknown')),
 digest TEXT NOT NULL DEFAULT '',
 comment_id TEXT NOT NULL DEFAULT '',
 before_ms INTEGER NOT NULL DEFAULT 0,
 after_ms INTEGER NOT NULL DEFAULT 0,
 sent_ms INTEGER NOT NULL CHECK(sent_ms > 0),
 at_ms INTEGER NOT NULL CHECK(at_ms > 0),
 CHECK(issue_id <> '' OR step = 'create'),
 CHECK(issue_key = '' OR step = 'create')
);
CREATE INDEX idx_tracker_agent_writes_issue ON tracker_agent_writes(kind, site, issue_id, id);
CREATE INDEX idx_tracker_agent_writes_repeat ON tracker_agent_writes(thread_id, digest, id);
`

const agentWritesMigrationVersion = 171
