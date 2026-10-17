package store

import (
	"database/sql"
	"strings"
	"testing"
)

// v159 over a populated database: a grouped row keeps its group and is
// grouped, a row a local spawn names or an assistant's past thread is
// nested, an assistant's own thread and every other row are standalone,
// and afterwards the CHECK refuses a write that breaks the coupling.
func TestMigrationV159SpawnPlacement(t *testing.T) {
	path := fileDBThrough(t, spawnPlacementMigrationVersion-1)
	db, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/p', 'p', 1, 1)`)
	mustExec(t, db, `INSERT INTO thread_groups (id, project_id, name, created_at, updated_at) VALUES ('g', 'p', 'Sweep', 1, 1)`)
	for _, id := range []string{"agent", "created", "spawned", "far", "past", "grouped", "grouped-spawned", "plain", "pinned-grouped"} {
		mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived)
			VALUES (?, 'p', ?, 'claude', '/p', '', 1, 1, 0)`, id, id)
	}
	mustExec(t, db, `UPDATE threads SET group_id = 'g' WHERE id IN ('grouped', 'grouped-spawned', 'pinned-grouped')`)
	mustExec(t, db, `UPDATE threads SET pinned_at = 5, pin_group = 1 WHERE id = 'pinned-grouped'`)
	mustExec(t, db, `INSERT INTO agents (id, thread_id, home, created_at) VALUES ('a', 'agent', '/h/a', 1), ('c', 'created', '/h/c', 1)`)
	mustExec(t, db, `INSERT INTO agent_past_threads (thread_id, agent_id, superseded_at) VALUES ('past', 'a', 1)`)
	mustExec(t, db, `INSERT INTO thread_requests (token, caller_thread_id, kind, target_computer_id, target_thread_id, state, created_at, updated_at) VALUES
		('t-spawned', 'agent', 'spawn', '', 'spawned', 'finished', 1, 1),
		('t-created', 'agent', 'spawn', '', 'created', 'finished', 1, 1),
		('t-grouped', 'agent', 'spawn', '', 'grouped-spawned', 'finished', 1, 1),
		('t-far', 'agent', 'spawn', 'laptop', 'far', 'finished', 1, 1),
		('t-send', 'agent', 'send', '', 'plain', 'finished', 1, 1)`)

	if err := applyMigration(db, migrationByVersion(t, spawnPlacementMigrationVersion)); err != nil {
		t.Fatalf("apply v%d: %v", spawnPlacementMigrationVersion, err)
	}

	want := map[string]string{
		"agent":           "standalone|",
		"created":         "standalone|",
		"spawned":         "nested|",
		"far":             "standalone|",
		"past":            "nested|",
		"grouped":         "grouped|g",
		"grouped-spawned": "grouped|g",
		"plain":           "standalone|",
		"pinned-grouped":  "grouped|g",
	}
	got, err := queryIDs(db, `SELECT id || '=' || placement || '|' || COALESCE(group_id, '') FROM threads ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("rows = %v", got)
	}
	for _, row := range got {
		id, rest, _ := strings.Cut(row, "=")
		if want[id] != rest {
			t.Errorf("%s = %s, want %s", id, rest, want[id])
		}
	}
	var pinnedAt, pinGroup sql.NullInt64
	if err := db.QueryRow(`SELECT pinned_at, pin_group FROM threads WHERE id = 'pinned-grouped'`).Scan(&pinnedAt, &pinGroup); err != nil {
		t.Fatal(err)
	}
	if pinnedAt.Int64 != 5 || pinGroup.Int64 != 1 {
		t.Errorf("a grouped member's pin = %v/%v, want it kept", pinnedAt, pinGroup)
	}
	if left, err := queryIDs(db, `SELECT name FROM sqlite_temp_master WHERE name = 'v159_thread_groups'`); err != nil || len(left) != 0 {
		t.Errorf("the parked groups table is still there: %v, %v", left, err)
	}

	for name, update := range map[string]string{
		"grouped without a group":  `UPDATE threads SET placement = 'grouped' WHERE id = 'plain'`,
		"a group while standalone": `UPDATE threads SET group_id = 'g' WHERE id = 'plain'`,
		"a group while nested":     `UPDATE threads SET placement = 'nested' WHERE id = 'grouped'`,
		"an unknown placement":     `UPDATE threads SET placement = 'floating' WHERE id = 'plain'`,
	} {
		if _, err := db.Exec(update); err == nil {
			t.Errorf("%s: the CHECK accepted the write", name)
		}
	}

	// The triggers end nesting with the last link.
	mustExec(t, db, `DELETE FROM thread_requests WHERE token = 't-spawned'`)
	mustExec(t, db, `DELETE FROM agent_past_threads WHERE thread_id = 'past'`)
	for _, id := range []string{"spawned", "past"} {
		var placement string
		if err := db.QueryRow(`SELECT placement FROM threads WHERE id = ?`, id).Scan(&placement); err != nil {
			t.Fatal(err)
		}
		if placement != PlacementStandalone {
			t.Errorf("%s without its link = %s, want standalone", id, placement)
		}
	}
}
