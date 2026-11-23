package store

import (
	"maps"
	"testing"
)

// v165 adds the sprint digest of a card to a database that holds cards
// from v162: an issue that holds memberships gets the digest no set has,
// so its next card write replaces them, and one without holds the empty
// digest. The upgraded schema is the schema of a database made at v165.
func TestTrackerSprintSetsMigration(t *testing.T) {
	db := migrateThrough(t, trackerMigrationVersion)
	mustExec(t, db, `INSERT INTO tracker_connections(id,kind,site,account,created_at) VALUES
		('j1','jira','https://example.atlassian.net','user1@example.com',1)`)
	mustExec(t, db, `INSERT INTO tracker_cache_roots(connection_id,created_at) VALUES ('j1',1)`)
	mustExec(t, db, `INSERT INTO tracker_cache_issues(root_id,issue_id,issue_key,updated_at) VALUES
		(1,'10001','WEB-1',5), (1,'10002','WEB-2',5)`)
	mustExec(t, db, `INSERT INTO tracker_cache_issue_sprints(root_id,issue_id,sprint_id) VALUES (1,'10001','21'), (1,'10001','22')`)
	for v := trackerMigrationVersion + 1; v <= trackerSprintSetsMigrationVersion; v++ {
		if err := applyMigration(db, migrationByVersion(t, v)); err != nil {
			t.Fatalf("apply v%d: %v", v, err)
		}
	}
	assertRows(t, db, `SELECT issue_id || ':' || hex(sprint_set) FROM tracker_cache_issues ORDER BY issue_id`, "10001:00,10002:")
	assertRows(t, db, `SELECT count(*) FROM tracker_cache_issue_sprints`, "2")
	if upgraded, fresh := migrationSchema(t, db), migrationSchema(t, migrateThrough(t, trackerSprintSetsMigrationVersion)); !maps.Equal(upgraded, fresh) {
		t.Fatalf("the schema upgraded from v162 differs from a fresh one:\n%v\n%v", upgraded["tracker_cache_issues"], fresh["tracker_cache_issues"])
	}
}
