package store

import (
	"testing"
)

// v179 gives a stored source the state credit reads of it (no act owed,
// no good read yet, no viewer), adds the uses of a connection and the
// matches of a root, both removed with what they belong to, and has a
// card write that changes a ticket's key or creation advance the totals
// of the boards holding it.
func TestTheForgeCreditMigrationAddsWhatCreditReads(t *testing.T) {
	db := migrateThrough(t, forgeCreditMigrationVersion-1)
	mustExec(t, db, `INSERT INTO tracker_connections(id, kind, site, account, created_at) VALUES ('j1', 'jira', 'https://example.atlassian.net', 'user1@example.com', 1)`)
	mustExec(t, db, `INSERT INTO tracker_cache_roots(id, connection_id, created_at) VALUES (7, 'j1', 1)`)
	mustExec(t, db, `INSERT INTO tracker_cache_boards(root_id, board_id) VALUES (7, '10'), (7, '11')`)
	mustExec(t, db, `INSERT INTO tracker_cache_issues(root_id, issue_id, issue_key, created_at, updated_at) VALUES (7, '10001', 'WEB-1', 5, 5)`)
	mustExec(t, db, `INSERT INTO tracker_cache_board_issues(root_id, board_id, issue_id, order_key, read_at) VALUES (7, '10', '10001', 'a', 1)`)
	mustExec(t, db, `INSERT INTO forge_history_sources(id, repo_key, forge, host, namespace, repo) VALUES (3, 'github:acme/web', 'github', 'github.com', 'acme', 'web')`)
	migrateFromThrough(t, db, forgeCreditMigrationVersion-1, forgeCreditMigrationVersion)

	assertRows(t, db, `SELECT acts_owed || ' ' || good_at || ' [' || viewer || ']' FROM forge_history_sources WHERE id = 3`, "0 0 []")
	for name, stmt := range map[string]string{
		"an act owed that is no flag": `UPDATE forge_history_sources SET acts_owed = 2 WHERE id = 3`,
		"a good read before time":     `UPDATE forge_history_sources SET good_at = -1 WHERE id = 3`,
		"a use of no connection":      `INSERT INTO forge_history_uses(connection_id, source_id) VALUES ('none', 3)`,
		"a use of no source":          `INSERT INTO forge_history_uses(connection_id, source_id) VALUES ('j1', 99)`,
		"a match of no root":          `INSERT INTO tracker_cache_forge_people(root_id, host, login, checked_at) VALUES (99, 'github.com', 'user2', 1)`,
		"a match of no host":          `INSERT INTO tracker_cache_forge_people(root_id, host, login, checked_at) VALUES (7, '', 'user2', 1)`,
		"a match of no login":         `INSERT INTO tracker_cache_forge_people(root_id, host, login, checked_at) VALUES (7, 'github.com', '', 1)`,
		"a match never checked":       `INSERT INTO tracker_cache_forge_people(root_id, host, login, checked_at) VALUES (7, 'github.com', 'user2', 0)`,
	} {
		if _, err := db.Exec(stmt); err == nil {
			t.Errorf("%s was stored", name)
		}
	}
	mustExec(t, db, `INSERT INTO forge_history_uses(connection_id, source_id) VALUES ('j1', 3)`)
	mustExec(t, db, `INSERT INTO tracker_cache_forge_people(root_id, host, login, checked_at) VALUES (7, 'github.com', 'user2', 1)`)
	assertRows(t, db, `SELECT name || '|' || person FROM tracker_cache_forge_people`, "|")
	assertRows(t, db, `SELECT group_concat(name, ',') FROM pragma_index_info('forge_history_uses_source')`, "source_id")

	totals := func() string {
		t.Helper()
		var out string
		if err := db.QueryRow(`SELECT group_concat(board_id || '=' || totals_revision, ' ') FROM (SELECT board_id, totals_revision FROM tracker_cache_boards ORDER BY board_id)`).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	was := totals()
	mustExec(t, db, `UPDATE tracker_cache_issues SET issue_key = 'WEB-1', created_at = 5, title = 'Edited' WHERE issue_id = '10001'`)
	if got := totals(); got != was {
		t.Fatalf("a write that keeps the key and creation moved the totals: %s -> %s", was, got)
	}
	mustExec(t, db, `UPDATE tracker_cache_issues SET issue_key = 'APP-9' WHERE issue_id = '10001'`)
	mustExec(t, db, `UPDATE tracker_cache_issues SET created_at = 6 WHERE issue_id = '10001'`)
	var ten, eleven int64
	if err := db.QueryRow(`SELECT (SELECT totals_revision FROM tracker_cache_boards WHERE board_id = '10'), (SELECT totals_revision FROM tracker_cache_boards WHERE board_id = '11')`).Scan(&ten, &eleven); err != nil {
		t.Fatal(err)
	}
	if ten != 3 || eleven != 0 {
		t.Fatalf("totals of the board holding the issue %d, of the other %d (from %s); want two advances of the first alone", ten, eleven, was)
	}

	// A use goes with its source and with its connection, a match with its root.
	mustExec(t, db, `DELETE FROM forge_history_sources WHERE id = 3`)
	assertRows(t, db, `SELECT count(*) FROM forge_history_uses`, "0")
	mustExec(t, db, `INSERT INTO forge_history_sources(id, repo_key, forge, host, namespace, repo) VALUES (4, 'github:acme/web', 'github', 'github.com', 'acme', 'web')`)
	mustExec(t, db, `INSERT INTO forge_history_uses(connection_id, source_id) VALUES ('j1', 4)`)
	mustExec(t, db, `DELETE FROM tracker_cache_roots WHERE id = 7`)
	assertRows(t, db, `SELECT count(*) FROM tracker_cache_forge_people`, "0")
	mustExec(t, db, `DELETE FROM tracker_connections WHERE id = 'j1'`)
	assertRows(t, db, `SELECT count(*) FROM forge_history_uses`, "0")
}
