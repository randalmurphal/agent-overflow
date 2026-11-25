package store

import (
	"errors"
	"testing"
)

// v168 deletes the worklogs and read days stored before it, and from then
// on a Tempo row added, turned on or off, or deleted deletes those of its
// Jira connection's root and no other: a write that leaves enabled as it
// was, and a change to another connection's Tempo row, keep them.
func TestWorklogSourceMigration(t *testing.T) {
	db := migrateThrough(t, threadTicketCopyMigrationVersion)
	mustExec(t, db, `INSERT INTO tracker_connections(id, kind, site, account, created_at) VALUES
		('j1', 'jira', 'https://example.atlassian.net', 'user1@example.com', 1),
		('j2', 'jira', 'https://example.atlassian.net', 'user2@example.com', 1)`)
	mustExec(t, db, `INSERT INTO tracker_cache_roots(id, connection_id, created_at) VALUES (7, 'j1', 1), (8, 'j2', 1)`)
	logTime := func() {
		t.Helper()
		mustExec(t, db, `INSERT OR REPLACE INTO tracker_cache_worklogs(root_id, worklog_id, issue_id, author_id, day, spent_seconds)
			VALUES (7, 'w1', '10001', 'acc-user1', '2026-01-05', 60), (8, 'w2', '10001', 'acc-user2', '2026-01-05', 60)`)
		mustExec(t, db, `INSERT OR REPLACE INTO tracker_cache_worklog_days(root_id, author_id, day, read_at)
			VALUES (7, 'acc-user1', '2026-01-05', 1), (8, 'acc-user2', '2026-01-05', 1)`)
	}
	const kept = "7:w1:1,8:w2:1"
	rows := `SELECT w.root_id || ':' || w.worklog_id || ':' || d.read_at FROM tracker_cache_worklogs w
		JOIN tracker_cache_worklog_days d USING (root_id, author_id, day) ORDER BY w.root_id`
	count := func(want string) {
		t.Helper()
		assertRows(t, db, `SELECT (SELECT count(*) FROM tracker_cache_worklogs) || ':' || (SELECT count(*) FROM tracker_cache_worklog_days)`, want)
	}
	logTime()
	m := migrationByVersion(t, worklogSourceMigrationVersion)
	if err := applyMigration(db, m); err != nil {
		t.Fatalf("apply v%d: %v", m.Version, err)
	}
	count("0:0")
	for _, step := range []struct{ name, sql, want string }{
		{"Tempo added", `INSERT INTO tracker_connections(id, kind, site, parent_id, created_at) VALUES ('t1', 'tempo', 'https://api.tempo.io', 'j1', 1)`, "8:w2:1"},
		{"Tempo turned off", `UPDATE tracker_connections SET enabled = 0 WHERE id = 't1'`, "8:w2:1"},
		{"Tempo left off", `UPDATE tracker_connections SET enabled = 0 WHERE id = 't1'`, kept},
		{"Tempo relabeled", `UPDATE tracker_connections SET label = 'Time' WHERE id = 't1'`, kept},
		{"Tempo turned on", `UPDATE tracker_connections SET enabled = 1 WHERE id = 't1'`, "8:w2:1"},
		{"Jira turned off", `UPDATE tracker_connections SET enabled = 0 WHERE id = 'j1'`, kept},
		{"Tempo removed", `DELETE FROM tracker_connections WHERE id = 't1'`, "8:w2:1"},
	} {
		logTime()
		mustExec(t, db, step.sql)
		assertRows(t, db, rows, step.want)
		if t.Failed() {
			t.Fatalf("after %s", step.name)
		}
	}
}

// A replace names the time source it read from, and one the root's
// connection no longer uses is refused whole in the replace's
// transaction: committed after the change of source it stores nothing,
// and committed before it the change's trigger deletes what it stored.
func TestMirrorWorklogsFollowTheirTimeSource(t *testing.T) {
	t.Parallel()
	s, c, root := newMirror(t)
	day := []MirrorWorklog{webWorklog("w1", "2026-01-05", 60)}
	replace := func(source string, at int64) error {
		_, err := s.ReplaceMirrorWorklogs(root, source, "acc-user1", "2026-01-05", "2026-01-05", mirrorReadAt(at), day)
		return err
	}
	stored := func(want string) {
		t.Helper()
		assertRows(t, s.db, `SELECT (SELECT count(*) FROM tracker_cache_worklogs) || ':' || (SELECT count(*) FROM tracker_cache_worklog_days)`, want)
	}
	refused := func(source string, at int64) {
		t.Helper()
		if err := replace(source, at); !errors.Is(err, ErrTrackerCacheSourceChanged) {
			t.Fatalf("a replace from %q at %d: %v", source, at, err)
		}
		stored("0:0")
	}
	if err := replace("", 1); err != nil {
		t.Fatal(err)
	}
	stored("1:1")
	tempo := mustCreateTrackerConnection(t, s, NewTrackerConnection{Kind: TrackerKindTempo, Site: "https://api.tempo.io", ParentID: c.ID, CreatedAt: 10})
	stored("0:0")
	refused("", 2)
	if err := replace(tempo.ID, 3); err != nil {
		t.Fatal(err)
	}
	stored("1:1")
	if _, _, err := s.SetTrackerConnectionEnabled(tempo.ID, false); err != nil {
		t.Fatal(err)
	}
	stored("0:0")
	refused(tempo.ID, 4)
	if err := replace("", 5); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SetTrackerConnectionEnabled(tempo.ID, true); err != nil {
		t.Fatal(err)
	}
	stored("0:0")
	refused("", 6)
	if err := replace(tempo.ID, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteTrackerConnection(tempo.ID); err != nil {
		t.Fatal(err)
	}
	stored("0:0")
	refused(tempo.ID, 8)
	if err := replace("", 9); err != nil {
		t.Fatal(err)
	}
	stored("1:1")
}
