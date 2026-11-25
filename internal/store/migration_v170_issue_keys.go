package store

// Migration v170 indexes the mirrored issues by key, so a ticket key a
// pull request or a branch names resolves to the issue a mirror holds, and
// a key's project to the mirror that holds any issue of it, with one probe
// each (TicketsByKey).
const issueKeysV170SQL = `
CREATE INDEX idx_tracker_cache_issues_key ON tracker_cache_issues(root_id, issue_key);
`

const issueKeysMigrationVersion = 170
