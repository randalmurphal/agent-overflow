package store

// Migration v157 lets the Agents segment's tree find what it keeps without
// reading the spawns a thread ever made (thread_agent_tree.go).
// idx_thread_requests_open holds the open requests a listing counts as
// work (openAgentRequestSQL), by caller, so the threads with open work are
// read one seek per thread and the walk climbs from them to the spawns
// above. idx_thread_requests_spawn_settled orders a caller's local spawns
// by settlement, so a read of named rows finds the spawns that are open or
// settled inside the window without the settled ones before it.
const agentTreeIndexesMigrationVersion = 157

const agentTreeIndexesV157SQL = `
CREATE INDEX idx_thread_requests_open
    ON thread_requests(caller_thread_id)
 WHERE lineage_only = 0 AND settled_at IS NULL AND (kind = 'spawn' OR (kind IN ('send','ask') AND notify = 1));
CREATE INDEX idx_thread_requests_spawn_settled
    ON thread_requests(caller_thread_id, settled_at)
 WHERE kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0;
`
