package store

import (
	"errors"
	"testing"
)

// v168 deletes the worklogs and read days stored before it, and from then
// on a change of a Jira connection's time source (a Tempo row added on,
// turned on or off, deleted while on) advances that Jira row's
// time_source_gen and deletes the worklogs and read days of its root and
// no other. A Tempo row added off or deleted while off, a write that
// leaves enabled as it was, and a change to another connection's Tempo
// row change neither.
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
	gens := `SELECT group_concat(id || '=' || time_source_gen) FROM (SELECT id, time_source_gen FROM tracker_connections WHERE kind = 'jira' ORDER BY id)`
	logTime()
	m := migrationByVersion(t, worklogSourceMigrationVersion)
	if err := applyMigration(db, m); err != nil {
		t.Fatalf("apply v%d: %v", m.Version, err)
	}
	assertRows(t, db, `SELECT (SELECT count(*) FROM tracker_cache_worklogs) || ':' || (SELECT count(*) FROM tracker_cache_worklog_days)`, "0:0")
	assertRows(t, db, gens, "j1=0,j2=0")
	for _, step := range []struct{ name, sql, want, gens string }{
		{"Tempo added off", `INSERT INTO tracker_connections(id, kind, site, parent_id, enabled, created_at) VALUES ('t1', 'tempo', 'https://api.tempo.io', 'j1', 0, 1)`, kept, "j1=0,j2=0"},
		{"Tempo removed while off", `DELETE FROM tracker_connections WHERE id = 't1'`, kept, "j1=0,j2=0"},
		{"Tempo added", `INSERT INTO tracker_connections(id, kind, site, parent_id, created_at) VALUES ('t1', 'tempo', 'https://api.tempo.io', 'j1', 1)`, "8:w2:1", "j1=1,j2=0"},
		{"Tempo left on", `UPDATE tracker_connections SET enabled = 1 WHERE id = 't1'`, kept, "j1=1,j2=0"},
		{"Tempo turned off", `UPDATE tracker_connections SET enabled = 0 WHERE id = 't1'`, "8:w2:1", "j1=2,j2=0"},
		{"Tempo left off", `UPDATE tracker_connections SET enabled = 0 WHERE id = 't1'`, kept, "j1=2,j2=0"},
		{"Tempo relabeled", `UPDATE tracker_connections SET label = 'Time' WHERE id = 't1'`, kept, "j1=2,j2=0"},
		{"Tempo removed while off", `DELETE FROM tracker_connections WHERE id = 't1'`, kept, "j1=2,j2=0"},
		{"Tempo added again", `INSERT INTO tracker_connections(id, kind, site, parent_id, created_at) VALUES ('t1', 'tempo', 'https://api.tempo.io', 'j1', 1)`, "8:w2:1", "j1=3,j2=0"},
		{"Jira turned off", `UPDATE tracker_connections SET enabled = 0 WHERE id = 'j1'`, kept, "j1=3,j2=0"},
		{"Tempo removed while on", `DELETE FROM tracker_connections WHERE id = 't1'`, "8:w2:1", "j1=4,j2=0"},
	} {
		logTime()
		mustExec(t, db, step.sql)
		assertRows(t, db, rows, step.want)
		assertRows(t, db, gens, step.gens)
		if t.Failed() {
			t.Fatalf("after %s", step.name)
		}
	}
}

// A replace names the time source generation its read began under, and
// one under an earlier generation is refused whole in the replace's
// transaction, however many changes came between: committed after the
// change it stores nothing, and committed before it the change's trigger
// deletes what it stored.
func TestMirrorWorklogsFollowTheirTimeSource(t *testing.T) {
	t.Parallel()
	s, c, root := newMirror(t)
	day := []MirrorWorklog{webWorklog("w1", "2026-01-05", 60)}
	gen := func() int64 {
		t.Helper()
		row, err := s.GetTrackerConnection(c.ID)
		if err != nil {
			t.Fatal(err)
		}
		return row.TimeSourceGen
	}
	replace := func(g, at int64) error {
		_, err := s.ReplaceMirrorWorklogs(root, g, "acc-user1", "2026-01-05", "2026-01-05", mirrorReadAt(at), day)
		return err
	}
	stored := func(want string) {
		t.Helper()
		assertRows(t, s.db, `SELECT (SELECT count(*) FROM tracker_cache_worklogs) || ':' || (SELECT count(*) FROM tracker_cache_worklog_days)`, want)
	}
	refused := func(g, at int64) {
		t.Helper()
		if err := replace(g, at); !errors.Is(err, ErrTrackerCacheSourceChanged) {
			t.Fatalf("a replace under generation %d at %d: %v", g, at, err)
		}
		stored("0:0")
	}
	enable := func(id string, on bool) {
		t.Helper()
		if _, _, err := s.SetTrackerConnectionEnabled(id, on); err != nil {
			t.Fatal(err)
		}
	}
	if err := replace(gen(), 1); err != nil {
		t.Fatal(err)
	}
	stored("1:1")
	native := gen()
	tempo := mustCreateTrackerConnection(t, s, NewTrackerConnection{Kind: TrackerKindTempo, Site: "https://api.tempo.io", ParentID: c.ID, CreatedAt: 10})
	stored("0:0")
	refused(native, 2)
	withTempo := gen()
	if err := replace(withTempo, 3); err != nil {
		t.Fatal(err)
	}
	stored("1:1")
	// Two switches bring the same source back; a read from before them
	// is still refused.
	enable(tempo.ID, false)
	enable(tempo.ID, true)
	stored("0:0")
	refused(withTempo, 4)
	enable(tempo.ID, false)
	refused(native, 5)
	if err := replace(gen(), 6); err != nil {
		t.Fatal(err)
	}
	stored("1:1")
	// Removing the Tempo row while it is off changes no source.
	before := gen()
	if _, err := s.DeleteTrackerConnection(tempo.ID); err != nil {
		t.Fatal(err)
	}
	stored("1:1")
	if gen() != before {
		t.Fatalf("removing a Tempo connection that is off moved the generation from %d to %d", before, gen())
	}
	if err := replace(before, 7); err != nil {
		t.Fatal(err)
	}
}
