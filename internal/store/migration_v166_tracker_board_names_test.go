package store

import (
	"strings"
	"testing"
)

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
