package store

import (
	"testing"
	"time"
)

// v175 gives every stored card its labels' names from its data: a card of
// the version the app writes holds each label by its name, else its ID,
// in the card's order, and skips one with neither; a card with none, a
// card of another version and data that is not JSON hold none.
func TestTrackerLabelsMigration(t *testing.T) {
	db := migrateThrough(t, activityMigrationVersion)
	mustExec(t, db, `INSERT INTO tracker_connections(id, kind, site, account, created_at) VALUES
		('j1', 'jira', 'https://example.atlassian.net', 'user1@example.com', 1)`)
	mustExec(t, db, `INSERT INTO tracker_cache_roots(id, connection_id, created_at) VALUES (7, 'j1', 1)`)
	mustExec(t, db, `INSERT INTO tracker_cache_issues(root_id, issue_id, issue_key, updated_at, data) VALUES
		(7, '1', 'WEB-1', 1, CAST('{"v":2,"type_name":"Story","labels":[{"id":"support","name":"support"},{"id":"l2"},{"name":""},{"id":"auth","name":"Auth"}]}' AS BLOB)),
		(7, '2', 'WEB-2', 1, CAST('{"v":2,"type_name":"Story"}' AS BLOB)),
		(7, '3', 'WEB-3', 1, CAST('{"v":1,"labels":[{"id":"support","name":"support"}]}' AS BLOB)),
		(7, '4', 'WEB-4', 1, x'00ff'),
		(7, '5', 'WEB-5', 1, x'')`)
	mustExec(t, db, `WITH RECURSIVE seq(x) AS (SELECT 6 UNION ALL SELECT x + 1 FROM seq WHERE x < 20000)
		INSERT INTO tracker_cache_issues(root_id, issue_id, issue_key, updated_at, data)
		SELECT 7, 'i' || x, 'WEB-' || x, 1, CAST('{"v":2,"labels":[{"id":"frontend","name":"frontend"},{"id":"auth","name":"auth"}]}' AS BLOB) FROM seq`)
	m := migrationByVersion(t, trackerLabelsMigrationVersion)
	began := time.Now()
	if err := applyMigration(db, m); err != nil {
		t.Fatalf("apply v%d: %v", m.Version, err)
	}
	t.Logf("v175 over 20,000 issues: %v", time.Since(began))
	assertRows(t, db, `SELECT group_concat(issue_id || '=' || labels, ' ') FROM (SELECT issue_id, labels FROM tracker_cache_issues
		WHERE issue_id NOT LIKE 'i%' ORDER BY issue_id)`, `1=["support","l2","Auth"] 2=[] 3=[] 4=[] 5=[]`)
	assertRows(t, db, `SELECT count(*) FROM tracker_cache_issues WHERE labels = '["frontend","auth"]'`, "19995")
	// A card stored after it holds no labels until its writer names some.
	mustExec(t, db, `INSERT INTO tracker_cache_issues(root_id, issue_id, issue_key, updated_at) VALUES (7, '6', 'WEB-6', 1)`)
	assertRows(t, db, `SELECT labels FROM tracker_cache_issues WHERE issue_id = '6'`, "[]")
}
