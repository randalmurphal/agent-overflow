package store

import (
	"strings"
	"testing"
)

// v170 indexes the mirrored issues by key and root, over the issues stored
// before it too, so a key resolves to its issue with one seek.
func TestIssueKeysMigration(t *testing.T) {
	db := migrateThrough(t, worklogNamesMigrationVersion)
	mustExec(t, db, `INSERT INTO tracker_connections(id, kind, site, account, created_at) VALUES
		('j1', 'jira', 'https://example.atlassian.net', 'user1@example.com', 1)`)
	mustExec(t, db, `INSERT INTO tracker_cache_roots(id, connection_id, created_at) VALUES (7, 'j1', 1)`)
	mustExec(t, db, `INSERT INTO tracker_cache_issues(root_id, issue_id, issue_key, title, updated_at) VALUES (7, '10001', 'WEB-1', 'Retry cursor', 1)`)
	m := migrationByVersion(t, issueKeysMigrationVersion)
	if err := applyMigration(db, m); err != nil {
		t.Fatalf("apply v%d: %v", m.Version, err)
	}
	assertRows(t, db, `SELECT group_concat(name, ',') FROM pragma_index_info('idx_tracker_cache_issues_key')`, "issue_key,root_id")
	var plan strings.Builder
	rows, err := db.Query(`EXPLAIN QUERY PLAN SELECT issue_id FROM tracker_cache_issues WHERE root_id = 7 AND issue_key = 'WEB-1'`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), "idx_tracker_cache_issues_key") {
		t.Fatalf("a key lookup after v170:\n%s\nwant the key index", plan.String())
	}
	assertRows(t, db, `SELECT issue_id FROM tracker_cache_issues WHERE root_id = 7 AND issue_key = 'WEB-1'`, "10001")
}
