package store

// Migration v149 records on a request whether the thread that answered it is
// a standing agent, and its role line (docs/specs/standing-agents.md,
// "Agent home contents"). A wake names a standing agent with its role, and a
// thread on another computer has no row here to read either from, so the
// settlement carries them. `answer_agent` separates an agent whose
// instructions.md has no role line from a thread that is not an agent.
//
// Rows written before it read as no agent recorded.
const answerAgentMigrationVersion = 149

const answerAgentV149SQL = `
ALTER TABLE thread_requests ADD COLUMN answer_agent INTEGER NOT NULL DEFAULT 0;
ALTER TABLE thread_requests ADD COLUMN answer_agent_role TEXT NOT NULL DEFAULT '';
`
