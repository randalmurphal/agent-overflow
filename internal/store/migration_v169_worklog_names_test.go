package store

import "testing"

// v169 adds each worklog's issue key and title, empty by default, and
// deletes the worklogs and read days stored before it, which hold no
// name, so their days read as unread.
func TestWorklogNamesMigration(t *testing.T) {
	db := migrateThrough(t, worklogSourceMigrationVersion)
	mustExec(t, db, `INSERT INTO tracker_connections(id, kind, site, account, created_at) VALUES
		('j1', 'jira', 'https://example.atlassian.net', 'user1@example.com', 1)`)
	mustExec(t, db, `INSERT INTO tracker_cache_roots(id, connection_id, created_at) VALUES (7, 'j1', 1)`)
	mustExec(t, db, `INSERT INTO tracker_cache_worklogs(root_id, worklog_id, issue_id, author_id, day, spent_seconds)
		VALUES (7, 'w1', '10001', 'acc-user1', '2026-01-05', 60)`)
	mustExec(t, db, `INSERT INTO tracker_cache_worklog_days(root_id, author_id, day, read_at) VALUES (7, 'acc-user1', '2026-01-05', 1)`)
	m := migrationByVersion(t, worklogNamesMigrationVersion)
	if err := applyMigration(db, m); err != nil {
		t.Fatalf("apply v%d: %v", m.Version, err)
	}
	assertRows(t, db, `SELECT (SELECT count(*) FROM tracker_cache_worklogs) || ':' || (SELECT count(*) FROM tracker_cache_worklog_days)`, "0:0")
	mustExec(t, db, `INSERT INTO tracker_cache_worklogs(root_id, worklog_id, issue_id, author_id, day, spent_seconds)
		VALUES (7, 'w2', '10001', 'acc-user1', '2026-01-05', 60)`)
	assertRows(t, db, `SELECT worklog_id || ':' || issue_key || ':' || issue_title FROM tracker_cache_worklogs`, "w2::")
}
