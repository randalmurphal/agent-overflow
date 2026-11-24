package store

import (
	"database/sql"
	"maps"
	"strings"
	"testing"
	"time"
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
// rows, histories and documents upgrades through v164 with every row but
// the documents kept: its wall times are 0, so each scope is due once, an
// issue that holds memberships gets the digest no set has, so its next
// card write replaces them, and one without holds the empty digest; an
// issue whose stored history was final for its card has its status time
// final for that update, the rule v162 derived it by; the documents are
// dropped with their table and no issue holds a body. The upgraded schema
// is the schema of a database made fresh.
func TestTrackerMirrorMigration(t *testing.T) {
	db := migrateThrough(t, trackerMigrationVersion)
	mustExec(t, db, `INSERT INTO tracker_connections(id,kind,site,account,created_at) VALUES
		('j1','jira','https://example.atlassian.net','user1@example.com',1)`)
	mustExec(t, db, `INSERT INTO tracker_cache_roots(connection_id,created_at) VALUES ('j1',1)`)
	mustExec(t, db, `INSERT INTO tracker_cache_boards(root_id,board_id,name,read_at) VALUES (1,'10','Web',900)`)
	mustExec(t, db, `INSERT INTO tracker_cache_issues(root_id,issue_id,issue_key,updated_at,read_at,status_since,history_for,history_wait) VALUES
		(1,'10001','WEB-1',5,901,3,5,0), (1,'10002','WEB-2',5,902,0,5,60000), (1,'10003','WEB-3',6,903,2,5,0)`)
	mustExec(t, db, `INSERT INTO tracker_cache_board_issues(root_id,issue_id,board_id,order_key,read_at) VALUES
		(1,'10001','10','a',901), (1,'10002','10','b',902)`)
	mustExec(t, db, `INSERT INTO tracker_cache_issue_sprints(root_id,issue_id,sprint_id) VALUES (1,'10001','21'), (1,'10001','22')`)
	mustExec(t, db, `INSERT INTO tracker_cache_sync(root_id,board_id,read_at,reconciled_at) VALUES (1,'10',903,904)`)
	mustExec(t, db, `INSERT INTO tracker_cache_issue_details(root_id,issue_id,opened_at,read_at,data) VALUES (1,'10001',905,906,x'7b7d')`)
	migrateMirrorFrom(t, db)
	assertRows(t, db, `SELECT issue_id || ':' || read_at || ':' || hex(sprint_set) || ':' || status_since || ':' || status_final_for || ':' ||
		body_updated_at || body_read_at || body_wall || body_opened_at || hex(body_digest) FROM tracker_cache_issues ORDER BY issue_id`,
		"10001:901:00:3:5:0000,10002:902::0:0:0000,10003:903::2:0:0000")
	assertRows(t, db, `SELECT count(*) FROM tracker_cache_issue_sprints`, "2")
	assertRows(t, db, `SELECT group_concat(issue_id || ':' || order_key || ':' || read_at) FROM tracker_cache_board_issues`, "10001:a:901,10002:b:902")
	assertRows(t, db, `SELECT count(*) FROM pragma_table_info('tracker_cache_board_issues') WHERE name = 'listed'`, "0")
	assertRows(t, db, `SELECT board_id || ':' || read_at || ':' || structure_wall FROM tracker_cache_boards`, "10:900:0")
	assertRows(t, db, `SELECT read_at || ':' || read_wall || ':' || reconciled_at || ':' || reconciled_wall || ':' || failed_wall FROM tracker_cache_sync`, "903:0:904:0:0")
	assertRows(t, db, `SELECT count(*) FROM sqlite_master WHERE name LIKE 'tracker_cache_issue_details%' OR name LIKE 'idx_tracker_cache_issue_details%'`, "0")
	assertRows(t, db, `SELECT count(*) FROM tracker_cache_issue_bodies`, "0")
	if upgraded, fresh := migrationSchema(t, db), migrationSchema(t, migrateThrough(t, trackerMirrorMigrationVersion)); !maps.Equal(upgraded, fresh) {
		for name, sql := range fresh {
			if upgraded[name] != sql {
				t.Errorf("%s upgraded from v162:\n%s\nfresh:\n%s", name, upgraded[name], sql)
			}
		}
		t.Fatalf("the schema upgraded from v162 differs from a fresh one")
	}
}

// The upgrade of a large mirror: 50,000 issues, half of them in a sprint
// and half with a final history, and 200 documents. v164 rewrites each
// issue that holds memberships or a final history once, adds its columns
// without rewriting the others, drops the documents with their table, and
// builds the bodies' index over no body. The time is logged, measured on
// the test's in-memory database.
func TestTrackerMirrorMigrationOfALargeMirror(t *testing.T) {
	if testing.Short() {
		t.Skip("fills 50,000 issues")
	}
	db := migrateThrough(t, trackerMigrationVersion)
	mustExec(t, db, `INSERT INTO tracker_connections(id,kind,site,account,created_at) VALUES
		('j1','jira','https://example.atlassian.net','user1@example.com',1)`)
	mustExec(t, db, `INSERT INTO tracker_cache_roots(connection_id,created_at) VALUES ('j1',1)`)
	mustExec(t, db, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 50000)
		INSERT INTO tracker_cache_issues(root_id,issue_id,issue_key,updated_at,read_at,status_since,history_for,history_wait,data)
		SELECT 1, 'i' || i, 'WEB-' || i, 5, 900, 3, iif(i % 2 = 0, 5, 0), 0, randomblob(200) FROM n`)
	mustExec(t, db, `INSERT INTO tracker_cache_issue_sprints(root_id,issue_id,sprint_id)
		SELECT 1, issue_id, '21' FROM tracker_cache_issues WHERE rowid % 2 = 1`)
	mustExec(t, db, `INSERT INTO tracker_cache_issue_details(root_id,issue_id,opened_at,read_at,data)
		SELECT 1, issue_id, 905, 906, randomblob(16000) FROM tracker_cache_issues LIMIT 200`)
	began := time.Now()
	migrateMirrorFrom(t, db)
	t.Logf("v163 and v164 over 50000 issues, 25000 sprint memberships and 200 documents: %v", time.Since(began))
	assertRows(t, db, `SELECT count(*) FROM tracker_cache_issues WHERE status_final_for = 5`, "25000")
	assertRows(t, db, `SELECT count(*) FROM tracker_cache_issues WHERE sprint_set = x'00'`, "25000")
	assertRows(t, db, `SELECT count(*) FROM tracker_cache_issues WHERE body_updated_at > 0`, "0")
}

// v164 adds the open tickets of the mirror, one bind of an issue at a time.
// Each refused statement below is valid but for the one rule it names; a
// root's delete takes its rows, and the keep rule's probe and a root's
// drain read the issue index.
func TestTrackerOpenTicketsTable(t *testing.T) {
	db := migrateThrough(t, trackerMirrorMigrationVersion)
	mustExec(t, db, `INSERT INTO tracker_connections(id,kind,site,account,created_at) VALUES
		('j1','jira','https://example.atlassian.net','user1@example.com',1), ('l1','linear','workspace-a','',1)`)
	mustExec(t, db, `INSERT INTO tracker_cache_roots(connection_id,created_at) VALUES ('j1',1), ('l1',1)`)
	const open = `INSERT INTO tracker_cache_open_tickets(root_id,issue_id,session,pump,opened_at) VALUES `
	// A closed ticket's row whose drop is owed stands beside the reopened
	// ticket's: an issue may have two.
	mustExec(t, db, open+`(1,'10001',7,1,5), (2,'issue-1',7,3,5), (1,'10002',8,1,5), (1,'10001',7,2,6)`)
	for name, bad := range map[string]string{
		"missing root":    open + `(9,'10003',7,4,5)`,
		"empty issue":     open + `(1,'',7,4,5)`,
		"pump zero":       open + `(1,'10003',7,0,5)`,
		"a pump's second": open + `(1,'10003',7,1,5)`,
		"no keep time":    open + `(1,'10003',7,4,0)`,
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

// v164 adds the bodies of the issue records. Each refused statement below
// is valid but for the one rule it names; an issue's delete takes its
// body, and the cap's eviction walks the bodies' index in its order.
func TestTrackerIssueBodiesTable(t *testing.T) {
	db := migrateThrough(t, trackerMirrorMigrationVersion)
	mustExec(t, db, `INSERT INTO tracker_connections(id,kind,site,account,created_at) VALUES
		('j1','jira','https://example.atlassian.net','user1@example.com',1)`)
	mustExec(t, db, `INSERT INTO tracker_cache_roots(connection_id,created_at) VALUES ('j1',1)`)
	mustExec(t, db, `INSERT INTO tracker_cache_issues(root_id,issue_id,issue_key,updated_at,body_updated_at,body_read_at,body_opened_at)
		VALUES (1,'10001','WEB-1',5,5,7,7), (1,'10002','WEB-2',5,0,0,0)`)
	const body = `INSERT INTO tracker_cache_issue_bodies(root_id,issue_id,data) VALUES `
	mustExec(t, db, body+`(1,'10001',x'7b7d')`)
	for name, bad := range map[string]string{
		"missing issue": body + `(1,'10009',x'7b7d')`,
		"no body":       body + `(1,'10002',NULL)`,
		"a second body": body + `(1,'10001',x'7b7d')`,
		"negative read": `UPDATE tracker_cache_issues SET body_read_at = -1 WHERE issue_id = '10002'`,
	} {
		if _, err := db.Exec(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	plan := queryPlanText(t, db, `EXPLAIN QUERY PLAN SELECT issue_id FROM tracker_cache_issues WHERE root_id = ?1 AND body_updated_at > 0
		ORDER BY body_opened_at DESC, issue_id DESC`, 1)
	if !strings.Contains(plan, "idx_tracker_cache_issues_body_opened") || strings.Contains(plan, "TEMP B-TREE") {
		t.Errorf("the bodies' walk plan:\n%s\nwant idx_tracker_cache_issues_body_opened and no sort", plan)
	}
	mustExec(t, db, `DELETE FROM tracker_cache_issues WHERE issue_id = '10001'`)
	assertRows(t, db, `SELECT count(*) FROM tracker_cache_issue_bodies`, "0")
}
