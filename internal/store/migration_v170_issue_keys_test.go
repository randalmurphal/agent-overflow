package store

import (
	"strings"
	"testing"
	"time"
)

// v170 indexes each mirror's issues by key, over the issues stored before
// it too, so a key resolves to its issue with one seek of its root; a
// read of a root that does not state the index's condition (a key that is
// not empty) cannot take it, so a walk of the root keeps the index it had.
func TestIssueKeysMigration(t *testing.T) {
	db := migrateThrough(t, worklogNamesMigrationVersion)
	mustExec(t, db, `INSERT INTO tracker_connections(id, kind, site, account, created_at) VALUES
		('j1', 'jira', 'https://example.atlassian.net', 'user1@example.com', 1)`)
	mustExec(t, db, `INSERT INTO tracker_cache_roots(id, connection_id, created_at) VALUES (7, 'j1', 1)`)
	mustExec(t, db, `INSERT INTO tracker_cache_issues(root_id, issue_id, issue_key, title, updated_at) VALUES (7, '10001', 'WEB-1', 'Retry cursor', 1)`)
	mustExec(t, db, `WITH RECURSIVE seq(x) AS (SELECT 2 UNION ALL SELECT x + 1 FROM seq WHERE x < 20000)
		INSERT INTO tracker_cache_issues(root_id, issue_id, issue_key, updated_at) SELECT 7, 'i' || x, 'WEB-' || x, 1 FROM seq`)
	m := migrationByVersion(t, issueKeysMigrationVersion)
	began := time.Now()
	if err := applyMigration(db, m); err != nil {
		t.Fatalf("apply v%d: %v", m.Version, err)
	}
	t.Logf("v170 over 20,000 issues: %v", time.Since(began))
	assertRows(t, db, `SELECT group_concat(name, ',') FROM pragma_index_info('idx_tracker_cache_issues_key')`, "root_id,issue_key")
	planOf := func(query string) string {
		t.Helper()
		var plan strings.Builder
		rows, err := db.Query(`EXPLAIN QUERY PLAN ` + query)
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
		return plan.String()
	}
	byKey := `SELECT issue_id FROM tracker_cache_issues WHERE root_id = 7 AND issue_key = 'WEB-1' AND issue_key <> ''`
	if plan := planOf(byKey); !strings.Contains(plan, "idx_tracker_cache_issues_key") {
		t.Fatalf("a key lookup after v170:\n%s\nwant the key index", plan)
	}
	if plan := planOf(`SELECT issue_id FROM tracker_cache_issues WHERE root_id = 7 ORDER BY issue_id`); strings.Contains(plan, "idx_tracker_cache_issues_key") {
		t.Fatalf("a walk of a root after v170:\n%s\ntakes the key index", plan)
	}
	assertRows(t, db, byKey, "10001")
}
