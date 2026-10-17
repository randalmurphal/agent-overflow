package store

// Migration v149 makes a local spawn request a lasting lineage record
// (docs/specs/standing-agents.md, "Sidebar agent row").
//
// A spawn row is the only record of which thread started another: the
// sidebar reads a thread's spawner from it, and a standing agent's row
// counts its finished children through it. Both reads look a row up by
// `target_thread_id`, which only `idx_thread_requests_caller` ignored. The
// index is partial: a spawn on another computer names a thread this store
// has no row for, so the sidebar never reads it.
//
// `lineage_only` marks a spawn row the retention sweep kept past the floor
// with its payload cleared. The request ledger reads skip it, so its token
// is unknown there as a deleted row's is; the lineage reads still find it.
const spawnLineageMigrationVersion = 149

const spawnLineageV149SQL = `
CREATE INDEX idx_thread_requests_spawn_target
    ON thread_requests(target_thread_id)
 WHERE kind = 'spawn' AND target_computer_id = '';

ALTER TABLE thread_requests ADD COLUMN lineage_only INTEGER NOT NULL DEFAULT 0 CHECK(lineage_only IN (0,1));
`
