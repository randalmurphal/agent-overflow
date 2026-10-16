package store

// Migration v143 records, per thread, the digest of the developer
// instructions its Codex thread last received. A resumed Codex thread
// restores the model's context from its rollout and never shows a changed
// `developerInstructions` to the model, so the app injects the new text
// once when the digest differs (docs/references/codex-wire.md). The row
// names the Codex thread it describes: a thread repointed at another Codex
// thread reads as having no record.
const codexDeveloperInstructionsMigrationVersion = 143

const codexDeveloperInstructionsV143SQL = `CREATE TABLE codex_developer_instructions (
 thread_id TEXT PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE,
 session_ref TEXT NOT NULL CHECK(session_ref <> ''),
 digest TEXT NOT NULL CHECK(length(digest) = 64)
);`
