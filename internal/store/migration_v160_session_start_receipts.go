package store

// Migration v160 records, per thread, the digest of the context the memory
// SessionStart hook last delivered to the thread's model and the provider
// session that received it, so a resume runs the hook only when the model
// does not hold its context (docs/specs/memory.md, "Delivery"). The row
// names the provider session it describes, like
// codex_developer_instructions.
const sessionStartReceiptsMigrationVersion = 160

const sessionStartReceiptsV160SQL = `CREATE TABLE session_start_receipts (
 thread_id TEXT PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE,
 session_ref TEXT NOT NULL CHECK(session_ref <> ''),
 digest TEXT NOT NULL CHECK(digest <> '')
);`
