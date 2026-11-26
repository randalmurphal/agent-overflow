package store

// Migration v170 indexes each mirror's issues by key, so a ticket key a
// pull request or a branch names resolves to the issue a mirror holds, and
// a key's project to whether a mirror holds any issue of it, with one seek
// of one root each (TicketsByKey). It is partial on a key that is not
// empty (which every row meets): only a statement that states that
// condition can use it, so a read of a root's issues that names no key
// never takes it for a walk of the root.
const issueKeysV170SQL = `
CREATE INDEX idx_tracker_cache_issues_key ON tracker_cache_issues(root_id, issue_key) WHERE issue_key <> '';
`

const issueKeysMigrationVersion = 170
