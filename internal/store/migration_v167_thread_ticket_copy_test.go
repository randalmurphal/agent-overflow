package store

import (
	"testing"
)

// A link stored at v166 keeps its ticket, key, title, origin and time
// through v167, with no status; the status group refuses values outside
// its range. Every table of the mirror is left empty, the connection the
// mirror read stays, and a root made after it takes a new ID. The new
// title column and absence table take their values.
func TestThreadTicketCopyMigration(t *testing.T) {
	db := migrateThrough(t, trackerBoardNamesMigrationVersion)
	mustExec(t, db, `INSERT INTO projects(id, path, name, slug, created_at, updated_at) VALUES ('p', '/tmp/p', 'p', 'p', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads(id, project_id, provider, workspace_path, created_at, updated_at) VALUES ('t', 'p', 'claude', '/tmp/p', 1, 1)`)
	mustExec(t, db, `INSERT INTO thread_tickets(thread_id, kind, site, issue_id, issue_key, issue_title, origin, linked_at)
		VALUES ('t', 'jira', 'https://example.atlassian.net', '10001', 'WEB-1', 'Fix login', 'manual', 5)`)
	mustExec(t, db, `INSERT INTO tracker_connections(id, kind, site, account, created_at)
		VALUES ('c', 'jira', 'https://example.atlassian.net', 'user1@example.com', 1)`)
	mustExec(t, db, `INSERT INTO tracker_cache_roots(id, connection_id, created_at) VALUES (7, 'c', 1)`)
	mustExec(t, db, `INSERT INTO tracker_cache_statuses(root_id, status_id, name, status_group, read_at, changed_at, done_changed_at)
		VALUES (7, '1', 'To Do', 2, 1, 1, 0)`)
	mustExec(t, db, `INSERT INTO tracker_cache_issues(root_id, issue_id, issue_key, status_id, updated_at, read_at, data)
		VALUES (7, '10001', 'WEB-1', '1', 1, 1, x'7b7d')`)
	mustExec(t, db, `INSERT INTO tracker_cache_issue_sprints(root_id, issue_id, sprint_id) VALUES (7, '10001', '20')`)
	m := migrationByVersion(t, threadTicketCopyMigrationVersion)
	if err := applyMigration(db, m); err != nil {
		t.Fatalf("apply v%d: %v", m.Version, err)
	}
	assertRows(t, db, `SELECT kind || ':' || site || ':' || issue_id || ':' || issue_key || ':' || issue_title || ':' || origin || ':' || linked_at
		|| ':' || status_id || ':' || status_name || ':' || status_group FROM thread_tickets`,
		"jira:https://example.atlassian.net:10001:WEB-1:Fix login:manual:5:::0")
	for _, bad := range []string{`status_group = 6`, `status_group = -1`} {
		if _, err := db.Exec(`UPDATE thread_tickets SET ` + bad); err == nil {
			t.Errorf("%s was stored", bad)
		}
	}
	tables, err := queryIDs(db, `SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE 'tracker\_cache\_%' ESCAPE '\' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range tables {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM "` + table + `"`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s keeps %d rows", table, n)
		}
	}
	assertRows(t, db, `SELECT id FROM tracker_connections`, "c")
	mustExec(t, db, `INSERT INTO tracker_cache_roots(connection_id, created_at) VALUES ('c', 2)`)
	assertRows(t, db, `SELECT id > 7 FROM tracker_cache_roots`, "1")
	mustExec(t, db, `INSERT INTO tracker_cache_issues(root_id, issue_id, issue_key, status_id, updated_at, read_at, data, title)
		SELECT id, '10001', 'WEB-1', '1', 1, 1, x'', 'Fix login' FROM tracker_cache_roots`)
	mustExec(t, db, `INSERT INTO tracker_cache_issue_missing(root_id, issue_id, read_at) SELECT id, '10001', 2 FROM tracker_cache_roots`)
	assertRows(t, db, `SELECT i.title || ':' || m.read_at FROM tracker_cache_issues i JOIN tracker_cache_issue_missing m USING (root_id, issue_id)`,
		"Fix login:2")
	for _, bad := range []string{`read_at = 0`, `issue_id = ''`} {
		if _, err := db.Exec(`UPDATE tracker_cache_issue_missing SET ` + bad); err == nil {
			t.Errorf("an absence with %s was stored", bad)
		}
	}
	mustExec(t, db, `DELETE FROM tracker_cache_roots`)
	assertRows(t, db, `SELECT count(*) FROM tracker_cache_issue_missing`, "0")
}
