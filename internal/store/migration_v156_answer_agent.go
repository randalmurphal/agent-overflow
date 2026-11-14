package store

// Migration v156 records whether the thread that answered a request is a
// standing agent, and its role line, as they were when the answer was
// written (docs/specs/standing-agents.md, "Agent home contents"). The receipt
// records them at its settlement and the source row copies them when it
// collects the answer, so a wake names an agent on another computer as it
// names one here. `answer_agent` separates an agent whose instructions.md has
// no role line from a thread that is not an agent. A late reply records
// its own pair (`late_reply_agent`), so each answer keeps the role it was
// written under.
//
// A source row written before it has `answer_agent` NULL: nothing was
// recorded, and the wake reads a thread on this computer when it renders.
// A receipt written before it reads as answered by no agent.
const answerAgentMigrationVersion = 156

const answerAgentV156SQL = `
ALTER TABLE thread_requests ADD COLUMN answer_agent INTEGER CHECK(answer_agent IN (0,1));
ALTER TABLE thread_requests ADD COLUMN answer_agent_role TEXT NOT NULL DEFAULT '';
ALTER TABLE thread_request_receipts ADD COLUMN answer_agent INTEGER NOT NULL DEFAULT 0 CHECK(answer_agent IN (0,1));
ALTER TABLE thread_request_receipts ADD COLUMN answer_agent_role TEXT NOT NULL DEFAULT '';
ALTER TABLE thread_requests ADD COLUMN late_reply_agent INTEGER NOT NULL DEFAULT 0 CHECK(late_reply_agent IN (0,1));
ALTER TABLE thread_requests ADD COLUMN late_reply_agent_role TEXT NOT NULL DEFAULT '';
ALTER TABLE thread_request_receipts ADD COLUMN late_reply_agent INTEGER NOT NULL DEFAULT 0 CHECK(late_reply_agent IN (0,1));
ALTER TABLE thread_request_receipts ADD COLUMN late_reply_agent_role TEXT NOT NULL DEFAULT '';
`
