package store

// Migration v175 adds idx_tracker_cache_issues_updated, the issues of a
// root by their update time, so the dashboard export's intervals read the
// histories of the issues updated since a time without visiting a board's
// others (mirrorExportUpdatedSince). It is partial on an update time
// above zero, which a read issue has, so only a statement that states
// that condition can take it: the root's other reads keep the indexes
// they had.
const issueUpdatesV175SQL = `
CREATE INDEX idx_tracker_cache_issues_updated ON tracker_cache_issues(root_id, updated_at) WHERE updated_at > 0;
`

const issueUpdatesMigrationVersion = 175
