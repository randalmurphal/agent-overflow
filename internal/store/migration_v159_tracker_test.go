package store

import (
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
