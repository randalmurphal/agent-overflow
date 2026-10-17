package store

import (
	"database/sql"
	"slices"
	"testing"
)

// v157 over a populated database: both indexes exist and hold the rows
// their predicates name: the open spawns and the open sends whose wake is
// armed, and every local spawn.
func TestMigrationV157AgentTreeIndexes(t *testing.T) {
	path := fileDBThrough(t, agentTreeIndexesMigrationVersion-1)
	db, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/p', 'p', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived)
		VALUES ('a', 'p', 'a', 'claude', '/p', '', 1, 1, 0)`)
	mustExec(t, db, `INSERT INTO thread_requests (token, caller_thread_id, kind, target_computer_id, target_thread_id, notify, state,
		settled_at, created_at, updated_at) VALUES
		('open-send', 'a', 'send', '', 't', 1, 'running', NULL, 1, 1),
		('open-quiet-send', 'a', 'send', '', 't', 0, 'running', NULL, 1, 1),
		('settled-send', 'a', 'send', '', 't', 1, 'finished', 5, 1, 1),
		('open-spawn', 'a', 'spawn', '', 'c', 0, 'running', NULL, 1, 1),
		('settled-spawn', 'a', 'spawn', '', 'd', 0, 'finished', 5, 1, 1),
		('remote-spawn', 'a', 'spawn', 'pc', 'e', 0, 'finished', 5, 1, 1)`)

	if err := applyMigration(db, migrationByVersion(t, agentTreeIndexesMigrationVersion)); err != nil {
		t.Fatalf("apply v%d: %v", agentTreeIndexesMigrationVersion, err)
	}

	for index, want := range map[string][]string{
		"idx_thread_requests_open":          {"open-send", "open-spawn"},
		"idx_thread_requests_spawn_settled": {"open-spawn", "settled-spawn"},
	} {
		if readIndexSQL(t, db, index) == "" {
			t.Fatalf("index %s is missing", index)
		}
		got, err := queryIDs(db, `SELECT token FROM thread_requests INDEXED BY `+index+` WHERE caller_thread_id = 'a' AND `+map[string]string{
			"idx_thread_requests_open":          openAgentRequestSQL("thread_requests"),
			"idx_thread_requests_spawn_settled": `kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0`,
		}[index])
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s holds %v; want %v", index, got, want)
		}
	}
}
