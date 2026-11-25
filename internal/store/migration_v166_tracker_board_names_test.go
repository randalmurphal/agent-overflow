package store

import (
	"strings"
	"testing"
)

// v166 clears a verification recorded before it, which names no token, and
// keeps each connection's other fields; the token column takes an empty
// digest or a SHA-256 one and refuses any other length.
func TestTrackerVerifiedTokenMigrationClearsUnboundVerifications(t *testing.T) {
	db := migrateThrough(t, trackerBoardNamesMigrationVersion-1)
	mustExec(t, db, `INSERT INTO tracker_connections(id, kind, site, account, label, verified_account_id, verified_name, verified_at, token_expires_at, created_at)
		VALUES ('c', 'jira', 'https://example.atlassian.net', 'user1@example.com', 'Work', 'acc-user1', 'User 1', 50, 900, 1)`)

	migrateFrom(t, db, trackerBoardNamesMigrationVersion-1)

	assertRows(t, db, `SELECT label || ':' || verified_account_id || ':' || verified_name || ':' || verified_at || ':' || length(verified_token) || ':' || token_expires_at FROM tracker_connections`, "Work:::0:0:900")
	mustExec(t, db, `UPDATE tracker_connections SET verified_token = randomblob(32)`)
	if _, err := db.Exec(`UPDATE tracker_connections SET verified_token = randomblob(31)`); err == nil {
		t.Error("a verified token digest of 31 bytes was accepted, want the CHECK to refuse it")
	}
}

// v166 names a project's boards. A listing stored before it keeps its
// place and opens-first with empty text, and the columns refuse text past
// their bound.
func TestTrackerBoardNamesMigrationKeepsListings(t *testing.T) {
	db := migrateThrough(t, trackerBoardNamesMigrationVersion-1)
	mustExec(t, db, `INSERT INTO projects(id, path, name, created_at, updated_at) VALUES ('p', '/p', 'p', 1, 1)`)
	mustExec(t, db, `INSERT INTO tracker_connections(id, kind, site, account, created_at) VALUES ('c', 'jira', 'https://example.atlassian.net', 'user1@example.com', 1)`)
	mustExec(t, db, `INSERT INTO project_tracker_boards(project_id, connection_id, board_id, position, opens_first) VALUES ('p', 'c', '10', 0, 1)`)

	migrateFrom(t, db, trackerBoardNamesMigrationVersion-1)

	assertRows(t, db, `SELECT board_id || ':' || position || ':' || opens_first || ':' || name || ':' || tracker_project FROM project_tracker_boards`, "10:0:1::")
	mustExec(t, db, `UPDATE project_tracker_boards SET name = ?, tracker_project = 'WEB'`, strings.Repeat("é", maxTrackerBoardTextRunes))
	for _, column := range []string{"name", "tracker_project"} {
		if _, err := db.Exec(`UPDATE project_tracker_boards SET `+column+` = ?`, strings.Repeat("x", maxTrackerBoardTextRunes+1)); err == nil {
			t.Errorf("a %s of %d characters was accepted, want the CHECK to refuse it", column, maxTrackerBoardTextRunes+1)
		}
	}
}
