package store

import (
	"maps"
	"testing"
)

// A database that main's v162 filled with cards, memberships, boards, sync
// rows and documents upgrades through v164 with every row kept: its wall
// times are 0, so each scope is due once, an issue that holds memberships
// gets the digest no set has, so its next card write replaces them, and
// one without holds the empty digest. The upgraded schema is the schema
// of a database made fresh.
func TestTrackerMirrorClockMigration(t *testing.T) {
	db := migrateThrough(t, trackerMigrationVersion)
	mustExec(t, db, `INSERT INTO tracker_connections(id,kind,site,account,created_at) VALUES
		('j1','jira','https://example.atlassian.net','user1@example.com',1)`)
	mustExec(t, db, `INSERT INTO tracker_cache_roots(connection_id,created_at) VALUES ('j1',1)`)
	mustExec(t, db, `INSERT INTO tracker_cache_boards(root_id,board_id,name,read_at) VALUES (1,'10','Web',900)`)
	mustExec(t, db, `INSERT INTO tracker_cache_issues(root_id,issue_id,issue_key,updated_at,read_at) VALUES
		(1,'10001','WEB-1',5,901), (1,'10002','WEB-2',5,902)`)
	mustExec(t, db, `INSERT INTO tracker_cache_board_issues(root_id,issue_id,board_id,order_key,read_at) VALUES
		(1,'10001','10','a',901), (1,'10002','10','b',902)`)
	mustExec(t, db, `INSERT INTO tracker_cache_issue_sprints(root_id,issue_id,sprint_id) VALUES (1,'10001','21'), (1,'10001','22')`)
	mustExec(t, db, `INSERT INTO tracker_cache_sync(root_id,board_id,read_at,reconciled_at) VALUES (1,'10',903,904)`)
	mustExec(t, db, `INSERT INTO tracker_cache_issue_details(root_id,issue_id,opened_at,read_at,data) VALUES (1,'10001',905,906,x'7b7d')`)
	for v := trackerMigrationVersion + 1; v <= trackerMirrorClockMigrationVersion; v++ {
		if err := applyMigration(db, migrationByVersion(t, v)); err != nil {
			t.Fatalf("apply v%d: %v", v, err)
		}
	}
	assertRows(t, db, `SELECT issue_id || ':' || read_at || ':' || hex(sprint_set) FROM tracker_cache_issues ORDER BY issue_id`,
		"10001:901:00,10002:902:")
	assertRows(t, db, `SELECT count(*) FROM tracker_cache_issue_sprints`, "2")
	assertRows(t, db, `SELECT count(*) FROM tracker_cache_board_issues`, "2")
	assertRows(t, db, `SELECT board_id || ':' || read_at || ':' || structure_wall FROM tracker_cache_boards`, "10:900:0")
	assertRows(t, db, `SELECT read_at || ':' || reconciled_at || ':' || reconciled_wall FROM tracker_cache_sync`, "903:904:0")
	assertRows(t, db, `SELECT opened_at || ':' || read_at || ':' || read_wall || ':' || hex(data) FROM tracker_cache_issue_details`, "905:906:0:7B7D")
	if upgraded, fresh := migrationSchema(t, db), migrationSchema(t, migrateThrough(t, trackerMirrorClockMigrationVersion)); !maps.Equal(upgraded, fresh) {
		for name, sql := range fresh {
			if upgraded[name] != sql {
				t.Errorf("%s upgraded from v162:\n%s\nfresh:\n%s", name, upgraded[name], sql)
			}
		}
		t.Fatalf("the schema upgraded from v162 differs from a fresh one")
	}
}
