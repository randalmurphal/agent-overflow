package store

import (
	"testing"
)

// A link stored at v166 keeps its ticket, key, title, origin and time
// through v167, with no status, no root and not missing, which reads as
// not checked; the new columns refuse values outside their ranges.
func TestThreadTicketSeenMigration(t *testing.T) {
	db := migrateThrough(t, trackerBoardNamesMigrationVersion)
	mustExec(t, db, `INSERT INTO projects(id, path, name, slug, created_at, updated_at) VALUES ('p', '/tmp/p', 'p', 'p', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads(id, project_id, provider, workspace_path, created_at, updated_at) VALUES ('t', 'p', 'claude', '/tmp/p', 1, 1)`)
	mustExec(t, db, `INSERT INTO thread_tickets(thread_id, kind, site, issue_id, issue_key, issue_title, origin, linked_at)
		VALUES ('t', 'jira', 'https://example.atlassian.net', '10001', 'WEB-1', 'Fix login', 'manual', 5)`)
	m := migrationByVersion(t, threadTicketSeenMigrationVersion)
	if err := applyMigration(db, m); err != nil {
		t.Fatalf("apply v%d: %v", m.Version, err)
	}
	assertRows(t, db, `SELECT kind || ':' || site || ':' || issue_id || ':' || issue_key || ':' || issue_title || ':' || origin || ':' || linked_at
		|| ':' || status_id || ':' || status_name || ':' || status_group || ':' || seen_root || ':' || missing FROM thread_tickets`,
		"jira:https://example.atlassian.net:10001:WEB-1:Fix login:manual:5:::0:0:0")
	for _, bad := range []string{`status_group = 6`, `status_group = -1`, `seen_root = -1`, `missing = 2`} {
		if _, err := db.Exec(`UPDATE thread_tickets SET ` + bad); err == nil {
			t.Errorf("%s was stored", bad)
		}
	}
}
