package store

import (
	"database/sql"
	"maps"
	"strings"
	"testing"
)

// migrateMirrorFrom applies to db, made at v162, every migration through
// v164.
func migrateMirrorFrom(t *testing.T, db *sql.DB) {
	t.Helper()
	for v := trackerMigrationVersion + 1; v <= trackerMirrorMigrationVersion; v++ {
		m := migrationByVersion(t, v)
		apply := applyMigration
		if m.Rebuild {
			apply = applyRebuildMigration
		}
		if err := apply(db, m); err != nil {
			t.Fatalf("apply v%d: %v", v, err)
		}
	}
}

// A database that main's v162 filled with cards, memberships, boards, sync
// rows and documents upgrades through v164 with every row kept: its wall
// times are 0, so each scope is due once, an issue that holds memberships
// gets the digest no set has, so its next card write replaces them, and
// one without holds the empty digest. The upgraded schema is the schema
// of a database made fresh.
func TestTrackerMirrorMigration(t *testing.T) {
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
	migrateMirrorFrom(t, db)
	assertRows(t, db, `SELECT issue_id || ':' || read_at || ':' || hex(sprint_set) FROM tracker_cache_issues ORDER BY issue_id`,
		"10001:901:00,10002:902:")
	assertRows(t, db, `SELECT count(*) FROM tracker_cache_issue_sprints`, "2")
	assertRows(t, db, `SELECT count(*) FROM tracker_cache_board_issues`, "2")
	assertRows(t, db, `SELECT board_id || ':' || read_at || ':' || structure_wall FROM tracker_cache_boards`, "10:900:0")
	assertRows(t, db, `SELECT read_at || ':' || read_wall || ':' || reconciled_at || ':' || reconciled_wall || ':' || failed_wall FROM tracker_cache_sync`, "903:0:904:0:0")
	assertRows(t, db, `SELECT opened_at || ':' || read_at || ':' || read_wall || ':' || hex(data) FROM tracker_cache_issue_details`, "905:906:0:7B7D")
	if upgraded, fresh := migrationSchema(t, db), migrationSchema(t, migrateThrough(t, trackerMirrorMigrationVersion)); !maps.Equal(upgraded, fresh) {
		for name, sql := range fresh {
			if upgraded[name] != sql {
				t.Errorf("%s upgraded from v162:\n%s\nfresh:\n%s", name, upgraded[name], sql)
			}
		}
		t.Fatalf("the schema upgraded from v162 differs from a fresh one")
	}
}

// v164 adds the open tickets of the mirror. Each refused statement below is
// valid but for the one rule it names; a root's delete takes its rows, and
// the keep rule's probe and a root's drain read the issue index.
func TestTrackerOpenTicketsTable(t *testing.T) {
	db := migrateThrough(t, trackerMirrorMigrationVersion)
	mustExec(t, db, `INSERT INTO tracker_connections(id,kind,site,account,created_at) VALUES
		('j1','jira','https://example.atlassian.net','user1@example.com',1), ('l1','linear','workspace-a','',1)`)
	mustExec(t, db, `INSERT INTO tracker_cache_roots(connection_id,created_at) VALUES ('j1',1), ('l1',1)`)
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
