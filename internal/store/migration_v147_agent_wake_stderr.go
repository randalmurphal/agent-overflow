package store

// Migration v147 keeps the tail of a watch's last stderr on its row, so the
// activity rail can show it when the row is expanded (docs/specs/
// standing-agents.md, "Activity rail"). A watch's stderr otherwise lives only
// in the wake it was sent with and, as one line, in `last_outcome`.
const agentWakeStderrMigrationVersion = 147

const agentWakeStderrV147SQL = `
ALTER TABLE agent_wakes ADD COLUMN last_stderr TEXT NOT NULL DEFAULT '';
`
