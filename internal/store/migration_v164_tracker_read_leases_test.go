package store

import "testing"

// v164 adds the read leases of the mirror. Each refused statement below is
// valid but for the one rule it names, and a root's delete takes its row.
func TestTrackerReadLeasesMigration(t *testing.T) {
	db := migrateThrough(t, trackerReadLeasesMigrationVersion-1)
	mustExec(t, db, `INSERT INTO tracker_connections(id,kind,site,account,created_at) VALUES
		('j1','jira','https://example.atlassian.net','user1@example.com',1), ('l1','linear','workspace-a','',1)`)
	mustExec(t, db, `INSERT INTO tracker_cache_roots(connection_id,created_at) VALUES ('j1',1), ('l1',1)`)
	if err := applyMigration(db, migrationByVersion(t, trackerReadLeasesMigrationVersion)); err != nil {
		t.Fatal(err)
	}
	const lease = `INSERT INTO tracker_cache_read_leases(root_id,lease) VALUES `
	mustExec(t, db, lease+`(1,60001), (2,60002)`)
	for name, bad := range map[string]string{
		"missing root":    lease + `(9,60001)`,
		"lease zero":      lease + `(1,0)`,
		"a root's second": lease + `(1,60003)`,
	} {
		if _, err := db.Exec(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	mustExec(t, db, `DELETE FROM tracker_cache_roots WHERE id = 1`)
	assertRows(t, db, `SELECT root_id || ':' || lease FROM tracker_cache_read_leases`, "2:60002")
}
