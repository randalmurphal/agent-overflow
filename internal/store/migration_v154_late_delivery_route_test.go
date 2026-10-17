package store

import (
	"database/sql"
	"slices"
	"testing"
)

// v154 over a populated database: a late reply delivered before it carries
// the route its delivery wrote; every other row has none; no other column
// changes.
func TestMigrationV154LateDeliveryRoute(t *testing.T) {
	path := fileDBThrough(t, lateDeliveryRouteMigrationVersion-1)
	db, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/p', 'p', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived) VALUES
		('caller', 'p', 'Caller', 'claude', '/p', '', 1, 1, 0)`)
	mustExec(t, db, `INSERT INTO thread_requests (token, caller_thread_id, kind, target_computer_id, target_thread_id, target_thread_title, state, answer, settled_at, delivered_at, delivered_how, late_reply, late_delivered_at, confirmed_at, late_confirmed_at, created_at, updated_at) VALUES
		('late-inline', 'caller', 'send', '', 't', '', 'finished', 'a', 10, 20, 'inline', 'b', 30, 20, 30, 1, 1),
		('late-owed', 'caller', 'send', '', 't', '', 'finished', 'a', 10, 20, 'queued', 'b', NULL, 20, NULL, 1, 1),
		('answered', 'caller', 'send', '', 't', '', 'replied', 'a', 10, 20, 'queued', NULL, NULL, 20, NULL, 1, 1),
		('open', 'caller', 'ask', '', '', '', 'running', NULL, NULL, NULL, '', NULL, NULL, NULL, NULL, 1, 1)`)
	before, err := queryIDs(db, `SELECT `+threadRequestColumnsBeforeV151+` FROM thread_requests ORDER BY token`)
	if err != nil {
		t.Fatal(err)
	}

	if err := applyMigration(db, migrationByVersion(t, lateDeliveryRouteMigrationVersion)); err != nil {
		t.Fatalf("apply v%d: %v", lateDeliveryRouteMigrationVersion, err)
	}

	if got, err := queryIDs(db, `SELECT `+threadRequestColumnsBeforeV151+` FROM thread_requests ORDER BY token`); err != nil {
		t.Fatal(err)
	} else if !slices.Equal(got, before) {
		t.Errorf("thread_requests changed:\n got %q\nwant %q", got, before)
	}
	got, err := queryIDs(db, `SELECT token || '|' || late_delivered_how FROM thread_requests ORDER BY token`)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"answered|", "late-inline|inline", "late-owed|", "open|"}; !slices.Equal(got, want) {
		t.Errorf("late routes = %q, want %q", got, want)
	}
	if _, err := db.Exec(`UPDATE thread_requests SET late_delivered_how = 'mail' WHERE token = 'open'`); err == nil {
		t.Error("a route outside the set was stored")
	}
}
