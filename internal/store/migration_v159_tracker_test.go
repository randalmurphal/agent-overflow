package store

import (
	"reflect"
	"strings"
	"testing"
)

// v159 creates the durable tracker tables. Each refused statement below is
// valid but for the one rule it names.
func TestTrackerMigrationCreatesTheDurableTables(t *testing.T) {
	db := migrateThrough(t, trackerMigrationVersion-1)
	mustExec(t, db, `INSERT INTO projects(id,path,name,slug,sort_position,created_at,updated_at) VALUES ('p','/p','p','p',0,1,1), ('q','/q','q','q',1,1,1)`)
	mustExec(t, db, `INSERT INTO threads(id,project_id,title,provider,model,workspace_path,created_at,updated_at) VALUES
		('th','p','T','claude','m','/p',1,1), ('th2','p','T2','claude','m','/p',1,1)`)
	if err := applyMigration(db, migrationByVersion(t, trackerMigrationVersion)); err != nil {
		t.Fatal(err)
	}

	const connection = `INSERT INTO tracker_connections(id,kind,site,account,parent_id,created_at) VALUES `
	mustExec(t, db, connection+`
		('j1','jira','https://example.atlassian.net','user1@example.com',NULL,1),
		('j1b','jira','https://example.atlassian.net','user2@example.com',NULL,1),
		('j2','jira','https://other.example.com','user1@example.com',NULL,1),
		('t1','tempo','https://api.eu.tempo.io','','j1',1),
		('t2','tempo','https://api.eu.tempo.io','','j2',1),
		('l1','linear','workspace-a','',NULL,1)`)
	const board = `INSERT INTO project_tracker_boards(project_id,connection_id,board_id,position,opens_first) VALUES `
	mustExec(t, db, board+`('p','j1','10',0,1), ('p','l1','team-a',1,0), ('q','j1','10',0,0), ('q','t1','20',1,0)`)
	const ticket = `INSERT INTO thread_tickets(thread_id,kind,site,issue_id,issue_key,issue_title,origin,linked_at) VALUES `
	mustExec(t, db, ticket+`('th','jira','https://example.atlassian.net','10001','WEB-1','First','start_work',1)`)

	for name, bad := range map[string]string{
		"unknown kind":              connection + `('x','github','https://example.com','',NULL,1)`,
		"empty site":                connection + `('x','linear','','',NULL,1)`,
		"jira without account":      connection + `('x','jira','https://third.example.com','',NULL,1)`,
		"linear with account":       connection + `('x','linear','workspace-b','user1@example.com',NULL,1)`,
		"tempo with account":        connection + `('x','tempo','https://api.us.tempo.io','user1@example.com','j1b',1)`,
		"tempo without parent":      connection + `('x','tempo','https://api.us.tempo.io','',NULL,1)`,
		"jira with parent":          connection + `('x','jira','https://third.example.com','user1@example.com','j1',1)`,
		"tempo with missing parent": connection + `('x','tempo','https://api.us.tempo.io','','missing',1)`,
		"duplicate jira":            connection + `('x','jira','https://example.atlassian.net','user1@example.com',NULL,1)`,
		"duplicate linear":          connection + `('x','linear','workspace-a','',NULL,1)`,
		"second tempo of a jira":    connection + `('x','tempo','https://api.us.tempo.io','','j1',1)`,
		"no creation time":          connection + `('x','linear','workspace-c','',NULL,0)`,
		"enabled not a flag":        `UPDATE tracker_connections SET enabled = 2 WHERE id = 'l1'`,
		"negative verified time":    `UPDATE tracker_connections SET verified_at = -1 WHERE id = 'l1'`,
		"negative expiry":           `UPDATE tracker_connections SET token_expires_at = -1 WHERE id = 'l1'`,

		"empty board":                 board + `('p','j1','',2,0)`,
		"negative position":           board + `('p','j1','11',-1,0)`,
		"opens first not a flag":      board + `('p','j1','12',2,2)`,
		"repeated position":           board + `('p','j1','13',0,0)`,
		"second opens first":          board + `('p','j1','14',2,1)`,
		"board listed twice":          board + `('p','j1','10',2,0)`,
		"board of missing project":    board + `('missing','j1','15',0,0)`,
		"board of missing connection": board + `('p','missing','16',2,0)`,

		"ticket of tempo":          ticket + `('th2','tempo','https://api.eu.tempo.io','10001','WEB-1','','manual',1)`,
		"ticket without site":      ticket + `('th2','jira','','10001','WEB-1','','manual',1)`,
		"ticket without issue":     ticket + `('th2','jira','https://example.atlassian.net','','WEB-1','','manual',1)`,
		"ticket without key":       ticket + `('th2','jira','https://example.atlassian.net','10001','','','manual',1)`,
		"unknown origin":           ticket + `('th2','jira','https://example.atlassian.net','10001','WEB-1','','pasted',1)`,
		"no link time":             ticket + `('th2','jira','https://example.atlassian.net','10001','WEB-1','','manual',0)`,
		"second ticket of thread":  ticket + `('th','linear','workspace-a','issue-1','APP-1','','manual',1)`,
		"ticket of missing thread": ticket + `('missing','jira','https://example.atlassian.net','10001','WEB-1','','manual',1)`,
	} {
		if _, err := db.Exec(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	mustExec(t, db, ticket+`('th2','linear','workspace-a','issue-1','APP-1','','agent',2)`)

	// A Jira connection takes its Tempo connection and the boards listed
	// through either; the other site's Tempo connection stays.
	mustExec(t, db, `DELETE FROM tracker_connections WHERE id = 'j1'`)
	assertRows(t, db, `SELECT id FROM tracker_connections ORDER BY id`, "j1b,j2,l1,t2")
	assertRows(t, db, `SELECT project_id || ':' || board_id FROM project_tracker_boards ORDER BY 1`, "p:team-a")
	// A link names a site, not a connection: removing the connection keeps it.
	mustExec(t, db, `DELETE FROM tracker_connections WHERE id = 'l1'`)
	assertRows(t, db, `SELECT thread_id FROM thread_tickets ORDER BY 1`, "th,th2")
	mustExec(t, db, `DELETE FROM threads WHERE id = 'th'`)
	assertRows(t, db, `SELECT thread_id FROM thread_tickets ORDER BY 1`, "th2")
	mustExec(t, db, `INSERT INTO tracker_connections(id,kind,site,account,created_at) VALUES ('l2','linear','workspace-b','',1)`)
	mustExec(t, db, board+`('q','l2','team-b',0,1)`)
	mustExec(t, db, `DELETE FROM projects WHERE id = 'q'`)
	assertRows(t, db, `SELECT count(*) FROM project_tracker_boards`, "0")
}

// v159 creates the mirror. Each refused statement below is valid but for
// the one rule it names.
func TestTrackerMigrationCreatesTheMirror(t *testing.T) {
	db := migrateThrough(t, trackerMigrationVersion)
	mustExec(t, db, `INSERT INTO tracker_connections(id,kind,site,account,created_at) VALUES
		('j1','jira','https://example.atlassian.net','user1@example.com',1), ('l1','linear','workspace-a','',1)`)
	for _, valid := range []string{
		`INSERT INTO tracker_cache_roots(connection_id,created_at) VALUES ('j1',1), ('l1',1), (NULL,1)`,
		`INSERT INTO tracker_cache_users(root_id,user_id,name,read_at,changed_at) VALUES (1,'acc-user1','User 1',1,1)`,
		`INSERT INTO tracker_cache_statuses(root_id,status_id,name,status_group,read_at,changed_at,done_changed_at) VALUES (1,'1','To Do',2,1,1,0)`,
		`INSERT INTO tracker_cache_boards(root_id,board_id,project_id,name,plans_sprints,estimate_unit,read_at) VALUES (1,'10','100','WEB board',1,1,1)`,
		`INSERT INTO tracker_cache_board_columns(root_id,board_id,position,name) VALUES (1,'10',0,'To do'), (1,'10',1,'Done')`,
		`INSERT INTO tracker_cache_board_column_statuses(root_id,board_id,position,status_id,rank) VALUES (1,'10',0,'1',0)`,
		`INSERT INTO tracker_cache_sprints(root_id,board_id,sprint_id,position,state) VALUES (1,'10','21',0,2)`,
		`INSERT INTO tracker_cache_issues(root_id,issue_id,issue_key,updated_at) VALUES (1,'10001','WEB-1',5), (1,'10003','WEB-3',5)`,
		`INSERT INTO tracker_cache_board_issues(root_id,board_id,issue_id,order_key,estimate,read_at) VALUES (1,'10','10001','a',3,1)`,
		`INSERT INTO tracker_cache_issue_sprints(root_id,issue_id,sprint_id) VALUES (1,'10001','21')`,
		`INSERT INTO tracker_cache_changes(root_id,issue_id,gen,kind,seq,at,board_id,to_number) VALUES (1,'10001',0,3,0,2,'10',3)`,
		`INSERT INTO tracker_cache_issue_details(root_id,issue_id,opened_at,read_at) VALUES (1,'10003',7,7)`,
		`INSERT INTO tracker_cache_issue_details(root_id,issue_id,opened_at,read_at,data) VALUES (1,'10001',7,6,x'7b7d')`,
		`INSERT INTO tracker_cache_worklogs(root_id,worklog_id,issue_id,author_id,day,spent_seconds) VALUES (1,'w1','10001','acc-user1','2026-01-05',60)`,
		`INSERT INTO tracker_cache_worklog_days(root_id,author_id,day,read_at) VALUES (1,'acc-user1','2026-01-05',1)`,
		`INSERT INTO tracker_cache_sync(root_id,board_id,cursor,failed_at,failure_kind,failure_message,outcome_at) VALUES (1,'10',x'01',5,'setup','Sign in again.',5)`,
	} {
		mustExec(t, db, valid)
	}
	assertRows(t, db, `SELECT id || ':' || coalesce(connection_id, '-') FROM tracker_cache_roots ORDER BY id`, "1:j1,2:l1,3:-")
	for name, bad := range map[string]string{
		"second root of a connection": `INSERT INTO tracker_cache_roots(connection_id,created_at) VALUES ('j1',1)`,
		"root of missing connection":  `INSERT INTO tracker_cache_roots(connection_id,created_at) VALUES ('missing',1)`,
		"root with no time":           `INSERT INTO tracker_cache_roots(connection_id,created_at) VALUES (NULL,0)`,
		"row of missing root":         `INSERT INTO tracker_cache_users(root_id,user_id,read_at,changed_at) VALUES (9,'acc-user2',1,1)`,
		"user with no read time":      `INSERT INTO tracker_cache_users(root_id,user_id,read_at,changed_at) VALUES (1,'acc-user2',0,0)`,
		"user changed after its read": `INSERT INTO tracker_cache_users(root_id,user_id,read_at,changed_at) VALUES (1,'acc-user2',1,2)`,
		"status with no read time":    `INSERT INTO tracker_cache_statuses(root_id,status_id,status_group,read_at,changed_at,done_changed_at) VALUES (1,'2',2,0,0,0)`,
		"status changed after read":   `INSERT INTO tracker_cache_statuses(root_id,status_id,status_group,read_at,changed_at,done_changed_at) VALUES (1,'2',2,1,2,0)`,
		"done after the change":       `INSERT INTO tracker_cache_statuses(root_id,status_id,status_group,read_at,changed_at,done_changed_at) VALUES (1,'2',2,2,1,2)`,
		"board of missing root":       `INSERT INTO tracker_cache_boards(root_id,board_id,read_at) VALUES (9,'10',1)`,
		"board listed twice":          `INSERT INTO tracker_cache_boards(root_id,board_id,read_at) VALUES (1,'10',1)`,
		"negative board read time":    `INSERT INTO tracker_cache_boards(root_id,board_id,read_at) VALUES (1,'11',-1)`,
		"status group":                `INSERT INTO tracker_cache_statuses(root_id,status_id,status_group,read_at,changed_at,done_changed_at) VALUES (1,'2',6,1,1,0)`,
		"estimate unit":               `INSERT INTO tracker_cache_boards(root_id,board_id,estimate_unit,read_at) VALUES (1,'11',3,1)`,
		"column of missing board":     `INSERT INTO tracker_cache_board_columns(root_id,board_id,position) VALUES (1,'11',0)`,
		"status of missing column":    `INSERT INTO tracker_cache_board_column_statuses(root_id,board_id,position,status_id,rank) VALUES (1,'10',5,'3',0)`,
		"status in two columns":       `INSERT INTO tracker_cache_board_column_statuses(root_id,board_id,position,status_id,rank) VALUES (1,'10',1,'1',0)`,
		"sprint state":                `INSERT INTO tracker_cache_sprints(root_id,board_id,sprint_id,position,state) VALUES (1,'10','22',1,4)`,
		"sprint of missing board":     `INSERT INTO tracker_cache_sprints(root_id,board_id,sprint_id,position,state) VALUES (1,'11','22',1,1)`,
		"issue with no update time":   `INSERT INTO tracker_cache_issues(root_id,issue_id,issue_key,updated_at) VALUES (1,'10002','WEB-2',0)`,
		"issue with no key":           `INSERT INTO tracker_cache_issues(root_id,issue_id,issue_key,updated_at) VALUES (1,'10002','',5)`,
		"row of missing issue":        `INSERT INTO tracker_cache_board_issues(root_id,board_id,issue_id,read_at) VALUES (1,'10','10002',1)`,
		"row of missing board":        `INSERT INTO tracker_cache_board_issues(root_id,board_id,issue_id,read_at) VALUES (1,'11','10001',1)`,
		"row with no read time":       `INSERT INTO tracker_cache_board_issues(root_id,board_id,issue_id,read_at) VALUES (1,'10','10003',0)`,
		"sprint of missing issue":     `INSERT INTO tracker_cache_issue_sprints(root_id,issue_id,sprint_id) VALUES (1,'10002','21')`,
		"unknown change":              `INSERT INTO tracker_cache_changes(root_id,issue_id,gen,kind,seq,at) VALUES (1,'10001',0,5,0,2)`,
		"estimate with no board":      `INSERT INTO tracker_cache_changes(root_id,issue_id,gen,kind,seq,at) VALUES (1,'10001',0,3,1,2)`,
		"status with a board":         `INSERT INTO tracker_cache_changes(root_id,issue_id,gen,kind,seq,at,board_id) VALUES (1,'10001',0,1,0,2,'10')`,
		"status with numbers":         `INSERT INTO tracker_cache_changes(root_id,issue_id,gen,kind,seq,at,to_number) VALUES (1,'10001',0,1,0,2,1)`,
		"estimate with IDs":           `INSERT INTO tracker_cache_changes(root_id,issue_id,gen,kind,seq,at,board_id,to_value) VALUES (1,'10001',0,3,1,2,'10','x')`,
		"change with no time":         `INSERT INTO tracker_cache_changes(root_id,issue_id,gen,kind,seq,at) VALUES (1,'10001',0,1,0,0)`,
		"negative generation":         `INSERT INTO tracker_cache_changes(root_id,issue_id,gen,kind,seq,at) VALUES (1,'10001',-1,1,0,2)`,
		"detail of missing issue":     `INSERT INTO tracker_cache_issue_details(root_id,issue_id,opened_at,read_at) VALUES (1,'10002',7,7)`,
		"detail with no open time":    `UPDATE tracker_cache_issue_details SET opened_at = 0`,
		"detail with no read time":    `UPDATE tracker_cache_issue_details SET read_at = 0 WHERE issue_id = '10003'`,
		"worklog day":                 `INSERT INTO tracker_cache_worklogs(root_id,worklog_id,issue_id,author_id,day,spent_seconds) VALUES (1,'w2','1','a','2026-1-5',60)`,
		"worklog with no time":        `INSERT INTO tracker_cache_worklogs(root_id,worklog_id,issue_id,author_id,day,spent_seconds) VALUES (1,'w2','1','a','2026-01-05',0)`,
		"worklog day mark":            `INSERT INTO tracker_cache_worklog_days(root_id,author_id,day,read_at) VALUES (1,'a','2026-1-5',1)`,
		"worklog day with no read":    `INSERT INTO tracker_cache_worklog_days(root_id,author_id,day,read_at) VALUES (1,'a','2026-01-05',0)`,
		"worklog day with no author":  `INSERT INTO tracker_cache_worklog_days(root_id,author_id,day,read_at) VALUES (1,'','2026-01-05',1)`,
		"failure kind":                `INSERT INTO tracker_cache_sync(root_id,board_id,failed_at,failure_kind,outcome_at) VALUES (1,'',5,'flaky',5)`,
		"failure with no time":        `INSERT INTO tracker_cache_sync(root_id,board_id,failure_kind) VALUES (1,'','setup')`,
		"note with no failure":        `INSERT INTO tracker_cache_sync(root_id,board_id,failure_message) VALUES (1,'','x')`,
		"outcome before the failure":  `UPDATE tracker_cache_sync SET outcome_at = 4`,

		"empty user":               `UPDATE tracker_cache_users SET user_id = ''`,
		"empty status":             `UPDATE tracker_cache_statuses SET status_id = ''`,
		"empty board":              `INSERT INTO tracker_cache_boards(root_id,board_id,read_at) VALUES (1,'',1)`,
		"plans sprints not a flag": `UPDATE tracker_cache_boards SET plans_sprints = 2`,
		"retired not a flag":       `UPDATE tracker_cache_boards SET retired = 2`,
		"negative revision":        `UPDATE tracker_cache_boards SET revision = -1`,
		"negative removal time":    `UPDATE tracker_cache_boards SET rows_removed_at = -1`,
		"negative column":          `INSERT INTO tracker_cache_board_columns(root_id,board_id,position) VALUES (1,'10',-1)`,
		"empty column status":      `UPDATE tracker_cache_board_column_statuses SET status_id = ''`,
		"negative rank":            `UPDATE tracker_cache_board_column_statuses SET rank = -1`,
		"empty sprint":             `UPDATE tracker_cache_sprints SET sprint_id = ''`,
		"negative sprint position": `UPDATE tracker_cache_sprints SET position = -1`,
		"negative sprint start":    `UPDATE tracker_cache_sprints SET start_at = -1`,
		"negative sprint end":      `UPDATE tracker_cache_sprints SET end_at = -1`,
		"negative sprint complete": `UPDATE tracker_cache_sprints SET completed_at = -1`,
		"empty issue":              `INSERT INTO tracker_cache_issues(root_id,issue_id,issue_key,updated_at) VALUES (1,'','WEB-2',5)`,
		"negative created":         `UPDATE tracker_cache_issues SET created_at = -1`,
		"negative resolved":        `UPDATE tracker_cache_issues SET resolved_at = -1`,
		"negative status since":    `UPDATE tracker_cache_issues SET status_since = -1`,
		"negative history from":    `UPDATE tracker_cache_issues SET history_from = -1`,
		"negative history to":      `UPDATE tracker_cache_issues SET history_to = -1`,
		"negative board rows":      `UPDATE tracker_cache_issues SET board_rows = -1`,
		"negative history gen":     `UPDATE tracker_cache_issues SET history_gen = -1`,
		"dirty not a flag":         `UPDATE tracker_cache_issues SET history_dirty = 2`,
		"empty issue sprint":       `UPDATE tracker_cache_issue_sprints SET sprint_id = ''`,
		"negative change seq":      `UPDATE tracker_cache_changes SET seq = -1`,
		"empty worklog":            `UPDATE tracker_cache_worklogs SET worklog_id = ''`,
		"worklog with no issue":    `UPDATE tracker_cache_worklogs SET issue_id = ''`,
		"worklog with no author":   `UPDATE tracker_cache_worklogs SET author_id = ''`,
		"negative worklog update":  `UPDATE tracker_cache_worklogs SET updated_at = -1`,
		"negative cursor time":     `UPDATE tracker_cache_sync SET cursor_read_at = -1`,
		"negative read time":       `UPDATE tracker_cache_sync SET read_at = -1`,
		"negative reconcile time":  `UPDATE tracker_cache_sync SET reconciled_at = -1`,
		"negative failure time":    `UPDATE tracker_cache_sync SET failed_at = -1`,
	} {
		if _, err := db.Exec(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// The board-row count follows the rows, a board's cascade included.
	assertRows(t, db, `SELECT issue_id || ':' || board_rows FROM tracker_cache_issues ORDER BY 1`, "10001:1,10003:0")
	mustExec(t, db, `INSERT INTO tracker_cache_boards(root_id,board_id,read_at) VALUES (1,'11',1)`)
	mustExec(t, db, `INSERT INTO tracker_cache_board_issues(root_id,board_id,issue_id,read_at) VALUES (1,'11','10001',1), (1,'11','10003',1)`)
	assertRows(t, db, `SELECT issue_id || ':' || board_rows FROM tracker_cache_issues ORDER BY 1`, "10001:2,10003:1")
	mustExec(t, db, `UPDATE tracker_cache_board_issues SET order_key = 'b' WHERE board_id = '11'`)
	mustExec(t, db, `DELETE FROM tracker_cache_boards WHERE board_id = '11'`)
	assertRows(t, db, `SELECT issue_id || ':' || board_rows FROM tracker_cache_issues ORDER BY 1`, "10001:1,10003:0")

	// Removing the connection detaches its root and leaves the rows for a
	// drain.
	before := countMirrorRows(t, db)
	mustExec(t, db, `DELETE FROM tracker_connections WHERE id = 'j1'`)
	assertRows(t, db, `SELECT id || ':' || coalesce(connection_id, '-') FROM tracker_cache_roots ORDER BY id`, "1:-,2:l1,3:-")
	if after := countMirrorRows(t, db); !reflect.DeepEqual(after, before) {
		t.Errorf("removing the connection changed the mirror: %v, was %v", after, before)
	}
}

// The revision of a board advances with each change that can add, remove
// or move a row of one of its sections and with no other.
func TestTrackerMigrationRevisionTriggers(t *testing.T) {
	db := migrateThrough(t, trackerMigrationVersion)
	for _, seed := range []string{
		`INSERT INTO tracker_connections(id,kind,site,account,created_at) VALUES ('j1','jira','https://example.atlassian.net','user1@example.com',1)`,
		`INSERT INTO tracker_cache_roots(connection_id,created_at) VALUES ('j1',1)`,
		`INSERT INTO tracker_cache_boards(root_id,board_id,read_at) VALUES (1,'10',1), (1,'11',1)`,
		`INSERT INTO tracker_cache_board_columns(root_id,board_id,position) VALUES (1,'10',0), (1,'10',1)`,
		`INSERT INTO tracker_cache_issues(root_id,issue_id,issue_key,updated_at) VALUES (1,'10001','WEB-1',5), (1,'10002','WEB-2',5)`,
		`INSERT INTO tracker_cache_board_issues(root_id,board_id,issue_id,read_at) VALUES (1,'11','10002',1)`,
	} {
		mustExec(t, db, seed)
	}
	revision := func() (int, int) {
		var r10, r11 int
		if err := db.QueryRow(`SELECT max(CASE board_id WHEN '10' THEN revision END), max(CASE board_id WHEN '11' THEN revision END)
			FROM tracker_cache_boards`).Scan(&r10, &r11); err != nil {
			t.Fatal(err)
		}
		return r10, r11
	}
	for _, step := range []struct {
		name, query string
		board10     bool
		board11     bool
	}{
		{"row added", `INSERT INTO tracker_cache_board_issues(root_id,board_id,issue_id,read_at) VALUES (1,'10','10001',1)`, true, false},
		{"row reordered", `UPDATE tracker_cache_board_issues SET order_key = 'b' WHERE board_id = '10'`, true, false},
		{"row read again", `UPDATE tracker_cache_board_issues SET order_key = 'b', read_at = 2 WHERE board_id = '10'`, false, false},
		{"row estimated", `UPDATE tracker_cache_board_issues SET estimate = 3 WHERE board_id = '10'`, false, false},
		{"status changed", `UPDATE tracker_cache_issues SET status_id = '3' WHERE issue_id = '10001'`, true, false},
		{"status the same", `UPDATE tracker_cache_issues SET status_id = '3', data = x'01' WHERE issue_id = '10001'`, false, false},
		{"sprint joined", `INSERT INTO tracker_cache_issue_sprints(root_id,issue_id,sprint_id) VALUES (1,'10001','21')`, true, false},
		{"sprint left", `DELETE FROM tracker_cache_issue_sprints`, true, false},
		{"sprint added", `INSERT INTO tracker_cache_sprints(root_id,board_id,sprint_id,position,state) VALUES (1,'10','21',0,1)`, true, false},
		{"sprint started", `UPDATE tracker_cache_sprints SET state = 2`, true, false},
		{"sprint renamed", `UPDATE tracker_cache_sprints SET name = 'x', state = 2`, false, false},
		{"sprint removed", `DELETE FROM tracker_cache_sprints`, true, false},
		{"column status added", `INSERT INTO tracker_cache_board_column_statuses(root_id,board_id,position,status_id,rank) VALUES (1,'10',0,'3',0)`, true, false},
		{"column status moved", `UPDATE tracker_cache_board_column_statuses SET position = 1`, true, false},
		{"column status reranked", `UPDATE tracker_cache_board_column_statuses SET rank = 1, position = 1`, false, false},
		{"column status removed", `DELETE FROM tracker_cache_board_column_statuses`, true, false},
		{"status added", `INSERT INTO tracker_cache_statuses(root_id,status_id,status_group,read_at,changed_at,done_changed_at) VALUES (1,'3',3,1,1,0)`, false, false},
		{"status regrouped", `UPDATE tracker_cache_statuses SET status_group = 4`, false, false},
		{"row removed", `DELETE FROM tracker_cache_board_issues WHERE board_id = '10'`, true, false},
		{"other board's row", `DELETE FROM tracker_cache_board_issues WHERE board_id = '11'`, false, true},
	} {
		r10, r11 := revision()
		mustExec(t, db, step.query)
		a10, a11 := revision()
		if (a10 > r10) != step.board10 || (a11 > r11) != step.board11 || a10-r10 > 1 || a11-r11 > 1 {
			t.Errorf("%s: revisions %d, %d -> %d, %d; want board 10 bumped %v, board 11 %v", step.name, r10, r11, a10, a11, step.board10, step.board11)
		}
	}
}

// The delete of a board's last listing through a connection retires the
// board in that connection's mirror and deletes its sync state; a listing
// left in another project, or of another connection, leaves it.
func TestTrackerMigrationRetiresUnlistedBoards(t *testing.T) {
	db := migrateThrough(t, trackerMigrationVersion)
	for _, seed := range []string{
		`INSERT INTO projects(id,path,name,slug,sort_position,created_at,updated_at) VALUES ('p','/p','p','p',0,1,1), ('q','/q','q','q',1,1,1)`,
		`INSERT INTO tracker_connections(id,kind,site,account,created_at) VALUES
			('j1','jira','https://example.atlassian.net','user1@example.com',1), ('j2','jira','https://other.example.com','user1@example.com',1)`,
		`INSERT INTO tracker_cache_roots(connection_id,created_at) VALUES ('j1',1), ('j2',1)`,
		`INSERT INTO tracker_cache_boards(root_id,board_id,read_at) VALUES (1,'10',1), (2,'10',1)`,
		`INSERT INTO tracker_cache_sync(root_id,board_id,read_at) VALUES (1,'10',1), (1,'',1), (2,'10',1)`,
		`INSERT INTO project_tracker_boards(project_id,connection_id,board_id,position,opens_first) VALUES
			('p','j1','10',0,0), ('q','j1','10',0,0), ('p','j2','10',1,0)`,
	} {
		mustExec(t, db, seed)
	}
	state := `SELECT root_id || ':' || retired FROM tracker_cache_boards ORDER BY root_id`
	mustExec(t, db, `DELETE FROM project_tracker_boards WHERE project_id = 'p' AND connection_id = 'j1'`)
	assertRows(t, db, state, "1:0,2:0")
	mustExec(t, db, `DELETE FROM project_tracker_boards WHERE project_id = 'q'`)
	assertRows(t, db, state, "1:1,2:0")
	assertRows(t, db, `SELECT root_id || ':' || board_id FROM tracker_cache_sync ORDER BY 1`, "1:,2:10")
}

// The reads by ticket and by board probe their indexes.
func TestTrackerReadsUseTheirIndexes(t *testing.T) {
	db := migrateThrough(t, trackerMigrationVersion)
	for _, read := range []struct {
		query, index string
		args         []any
	}{
		{`SELECT thread_id FROM thread_tickets WHERE kind = ? AND site = ? AND issue_id = ?`, "idx_thread_tickets_issue", []any{"jira", "s", "1"}},
		{`SELECT project_id FROM project_tracker_boards WHERE connection_id = ? AND board_id = ?`, "idx_project_tracker_boards_board", []any{"c", "b"}},
		{`SELECT id FROM tracker_connections WHERE parent_id = ?`, "idx_tracker_connections_parent", []any{"c"}},
		{`SELECT connection_id, board_id FROM project_tracker_boards WHERE project_id = ? ORDER BY position`, "sqlite_autoindex_project_tracker_boards_2", []any{"p"}},
	} {
		plan := queryPlanText(t, db, `EXPLAIN QUERY PLAN `+read.query, read.args...)
		if !strings.Contains(plan, read.index) || strings.Contains(plan, "TEMP B-TREE") {
			t.Errorf("%s\nplan:\n%s\nwant %s and no sort", read.query, plan, read.index)
		}
	}
}

// assertRows compares the first column of query's rows, joined by commas.
func assertRows(t *testing.T, db sqlQueryer, query, want string) {
	t.Helper()
	got, err := queryIDs(db, query)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != want {
		t.Fatalf("%s = %q, want %q", query, strings.Join(got, ","), want)
	}
}
