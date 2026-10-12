package store

// Migration v147 indexes a local spawn request by the thread it created
// (docs/specs/standing-agents.md, "Sidebar agent row").
//
// A spawn row is the only record of which thread started another: the
// sidebar reads a thread's spawner from it, and a standing agent's row
// counts its finished children through it. Both reads look a row up by
// `target_thread_id`, which only `idx_thread_requests_caller` ignored. The
// index is partial: a spawn on another computer names a thread this store
// has no row for, so the sidebar never reads it.
const spawnTargetIndexMigrationVersion = 147

const spawnTargetIndexV147SQL = `
CREATE INDEX idx_thread_requests_spawn_target
    ON thread_requests(target_thread_id)
 WHERE kind = 'spawn' AND target_computer_id = '';
`
