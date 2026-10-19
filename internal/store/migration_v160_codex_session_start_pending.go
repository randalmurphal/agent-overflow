package store

// Migration v160 records, per thread, the SessionStart hook delivery its
// Codex thread still owes. Codex keeps the hooks a start or a compaction
// queued only in the loaded thread, so a session that ends before a turn
// ran them leaves the next session to register them for its resume
// (docs/references/codex-wire.md, SessionStart hooks). The row names the
// Codex thread it describes, like codex_developer_instructions.
const codexSessionStartPendingMigrationVersion = 160

const codexSessionStartPendingV160SQL = `CREATE TABLE codex_session_start_pending (
 thread_id TEXT PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE,
 session_ref TEXT NOT NULL CHECK(session_ref <> ''),
 after TEXT NOT NULL CHECK(after IN ('start', 'compaction'))
);`
