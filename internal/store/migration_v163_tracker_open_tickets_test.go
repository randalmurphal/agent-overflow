package store

import (
	"strings"
	"testing"
)

// v163 adds the open tickets of the mirror. Each refused statement below is
// valid but for the one rule it names; a root's delete takes its rows, and
// the keep rule's probe and a root's drain read the issue index.
func TestTrackerOpenTicketsMigration(t *testing.T) {
	db := migrateThrough(t, trackerOpenTicketsMigrationVersion-1)
	mustExec(t, db, `INSERT INTO tracker_connections(id,kind,site,account,created_at) VALUES
		('j1','jira','https://example.atlassian.net','user1@example.com',1), ('l1','linear','workspace-a','',1)`)
	mustExec(t, db, `INSERT INTO tracker_cache_roots(connection_id,created_at) VALUES ('j1',1), ('l1',1)`)
	if err := applyMigration(db, migrationByVersion(t, trackerOpenTicketsMigrationVersion)); err != nil {
		t.Fatal(err)
	}
	const open = `INSERT INTO tracker_cache_open_tickets(root_id,issue_id,session,pump) VALUES `
	mustExec(t, db, open+`(1,'10001',7,1), (1,'10001',7,2), (2,'issue-1',7,3), (1,'10002',8,1)`)
	for name, bad := range map[string]string{
		"missing root":    open + `(9,'10003',7,4)`,
		"empty issue":     open + `(1,'',7,4)`,
		"pump zero":       open + `(1,'10003',7,0)`,
		"a pump's second": open + `(1,'10003',7,1)`,
	} {
		if _, err := db.Exec(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	mustExec(t, db, `DELETE FROM tracker_cache_roots WHERE id = 1`)
	assertRows(t, db, `SELECT issue_id FROM tracker_cache_open_tickets ORDER BY pump`, "issue-1")
	for _, read := range []string{
		`SELECT 1 FROM tracker_cache_open_tickets WHERE root_id = ? AND issue_id = ?`,
		`SELECT issue_id FROM tracker_cache_open_tickets WHERE root_id = ? ORDER BY issue_id`,
	} {
		plan := queryPlanText(t, db, `EXPLAIN QUERY PLAN `+read, 1, "10001")
		if !strings.Contains(plan, "idx_tracker_cache_open_tickets_issue") || strings.Contains(plan, "TEMP B-TREE") {
			t.Errorf("%s\nplan:\n%s\nwant idx_tracker_cache_open_tickets_issue and no sort", read, plan)
		}
	}
}
