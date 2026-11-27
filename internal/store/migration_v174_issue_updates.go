package store

// Migration v174 adds idx_tracker_cache_issues_updated, the issues of a
// root by their update time, so the dashboard export's sprint intervals
// read the histories of the issues updated since a time without visiting a
// board's others (sprintIntervalsSQL).
const issueUpdatesV174SQL = `
CREATE INDEX idx_tracker_cache_issues_updated ON tracker_cache_issues(root_id, updated_at);
`

const issueUpdatesMigrationVersion = 174
