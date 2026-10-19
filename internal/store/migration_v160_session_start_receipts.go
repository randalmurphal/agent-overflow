package store

// Migration v160 records, per thread and SessionStart hook, the digest of
// the context the hook last delivered to the thread's model and the
// provider session that received it, so a resume runs only the hooks whose
// context the model does not hold (docs/specs/standing-agents.md, "Agent
// home contents"). The row names the provider session it describes, like
// codex_developer_instructions.
const sessionStartReceiptsMigrationVersion = 160

const sessionStartReceiptsV160SQL = `CREATE TABLE session_start_receipts (
 thread_id TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
 hook TEXT NOT NULL CHECK(hook <> ''),
 session_ref TEXT NOT NULL CHECK(session_ref <> ''),
 digest TEXT NOT NULL CHECK(digest <> ''),
 PRIMARY KEY (thread_id, hook)
);`
