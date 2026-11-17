package store

import (
	"slices"
	"testing"
)

// v159 indexes the local spawn links by caller: the links a database
// already holds are in it, and nothing else of the caller's is.
func TestSpawnCallerMigration(t *testing.T) {
	db := migrateThrough(t, spawnCallerMigrationVersion-1)
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/tmp/p', 'P', 1, 1)`)
	for _, id := range []string{"lead", "worker", "old"} {
		mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived, mode)
			VALUES (?, 'p', ?, 'claude', '/tmp/p', 'test-model', 1, 1, 0, 'chat')`, id, id)
	}
	for _, row := range []struct {
		token, kind, computer, target string
		superseded                    int
	}{
		{"link", "spawn", "", "worker", 0},
		{"replaced", "spawn", "", "old", 1},
		{"remote", "spawn", "pc", "there", 0},
		{"send", "send", "", "worker", 0},
		{"unnamed", "spawn", "", "", 0},
	} {
		mustExec(t, db, `INSERT INTO thread_requests (token, caller_thread_id, kind, target_computer_id, target_thread_id, superseded, state, created_at, updated_at)
			VALUES (?, 'lead', ?, ?, ?, ?, 'finished', 5, 5)`, row.token, row.kind, row.computer, row.target, row.superseded)
	}

	migrateFrom(t, db, spawnCallerMigrationVersion-1)

	rows, err := db.Query(`SELECT token FROM thread_requests INDEXED BY idx_thread_requests_spawn_caller
		WHERE caller_thread_id = 'lead' AND kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id <> ''`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			t.Fatal(err)
		}
		got = append(got, token)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"link"}) {
		t.Fatalf("the caller's spawn links = %v; want only the link", got)
	}
}
