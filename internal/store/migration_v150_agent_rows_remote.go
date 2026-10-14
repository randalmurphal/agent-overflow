package store

// Migration v150 lets the Agents segment treat a request on another
// computer as it treats a local one (docs/specs/standing-agents.md,
// "Agents segment"). `nested_open` records whether the last `agent_rows`
// answer for a remote spawn listed any open row, so a settled remote spawn
// stays listed while its thread there still runs work of its own.
// `target_provider` is the remote target's provider as its computer
// reported it with the acknowledgement, so the row shows its icon.
// `idx_thread_requests_nested_open` holds only the settled remote spawns
// with open work there, in the order the sweep pages through them.
//
// A row written before it reads as a remote spawn with nothing open there
// and a target whose provider is unknown.
const agentRowsRemoteMigrationVersion = 150

const agentRowsRemoteV150SQL = `
ALTER TABLE thread_requests ADD COLUMN nested_open INTEGER NOT NULL DEFAULT 0 CHECK(nested_open IN (0,1));
ALTER TABLE thread_requests ADD COLUMN target_provider TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_thread_requests_nested_open ON thread_requests(settled_at, token)
  WHERE nested_open = 1 AND settled_at IS NOT NULL AND kind = 'spawn'
    AND target_computer_id <> '' AND lineage_only = 0;
`
