package store

import (
	"database/sql"
	"slices"
	"testing"
)

// v151 over a populated database: every request reads as a target whose
// provider is unknown with nothing open under it, and no other column
// changes.
func TestMigrationV151AgentRowsRemote(t *testing.T) {
	path := fileDBThrough(t, agentRowsRemoteMigrationVersion-1)
	db, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/p', 'p', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived) VALUES
		('caller', 'p', 'Caller', 'claude', '/p', '', 1, 1, 0)`)
	mustExec(t, db, `INSERT INTO thread_requests (token, caller_thread_id, kind, target_computer_id, target_thread_id, target_thread_title, state, answer, settled_at, delivered_at, delivered_how, late_reply, late_delivered_at, confirmed_at, created_at, updated_at) VALUES
		('local', 'caller', 'spawn', '', 't', '', 'finished', 'a', 10, 20, 'queued', NULL, NULL, 20, 1, 1),
		('remote', 'caller', 'spawn', 'c', 't', 'Their thread', 'running', NULL, NULL, NULL, '', NULL, NULL, NULL, 1, 1),
		('send', 'caller', 'send', 'c', 't', 'Their thread', 'replied', 'a', 10, NULL, '', NULL, NULL, NULL, 1, 1)`)
	before, err := queryIDs(db, `SELECT `+threadRequestColumnsBeforeV150+` FROM thread_requests ORDER BY token`)
	if err != nil {
		t.Fatal(err)
	}

	if err := applyMigration(db, migrationByVersion(t, agentRowsRemoteMigrationVersion)); err != nil {
		t.Fatalf("apply v%d: %v", agentRowsRemoteMigrationVersion, err)
	}

	if got, err := queryIDs(db, `SELECT `+threadRequestColumnsBeforeV150+` FROM thread_requests ORDER BY token`); err != nil {
		t.Fatal(err)
	} else if !slices.Equal(got, before) {
		t.Errorf("thread_requests changed:\n got %q\nwant %q", got, before)
	}
	got, err := queryIDs(db, `SELECT token || '|' || nested_open || '|' || target_provider FROM thread_requests ORDER BY token`)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"local|0|", "remote|0|", "send|0|"}; !slices.Equal(got, want) {
		t.Errorf("new columns = %q, want %q", got, want)
	}
	if _, err := db.Exec(`UPDATE thread_requests SET nested_open = 2 WHERE token = 'remote'`); err == nil {
		t.Error("nested_open accepted 2")
	}
}
