package store

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
)

// v149 over a populated database: every request row survives unchanged and
// reads lineage_only 0, every schema object but thread_requests is
// unchanged, thread_requests gains exactly lineage_only, the one new object
// is the partial index on a local spawn's target, and the spawner lookup
// the sidebar makes uses it.
func TestSpawnLineageMigration(t *testing.T) {
	path := fileDBThrough(t, spawnLineageMigrationVersion-1)
	db, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/p', 'p', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived) VALUES
		('agent', 'p', 'Agent', 'claude', '/p', '', 1, 1, 0),
		('child', 'p', 'Child', 'claude', '/p', '', 1, 2, 0)`)
	mustExec(t, db, `INSERT INTO thread_requests (token, caller_thread_id, kind, target_computer_id, target_thread_id, state, created_at, updated_at) VALUES
		('tok-local', 'agent', 'spawn', '', 'child', 'finished', 1, 1),
		('tok-remote', 'agent', 'spawn', 'laptop', 'far', 'finished', 2, 2),
		('tok-send', 'agent', 'send', '', 'child', 'finished', 3, 3)`)
	requestRows := func() []string {
		t.Helper()
		got, err := queryIDs(db, `SELECT token || '|' || caller_thread_id || '|' || kind || '|' || target_computer_id || '|' || target_thread_id || '|' || state
			FROM thread_requests ORDER BY token`)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	rowsBefore := requestRows()
	schemaBefore := migrationSchema(t, db)

	if err := applyMigration(db, migrationByVersion(t, spawnLineageMigrationVersion)); err != nil {
		t.Fatalf("apply v%d: %v", spawnLineageMigrationVersion, err)
	}

	if got := requestRows(); !slices.Equal(got, rowsBefore) {
		t.Errorf("request rows changed:\n got %q\nwant %q", got, rowsBefore)
	}
	if got, err := queryIDs(db, `SELECT DISTINCT CAST(lineage_only AS TEXT) FROM thread_requests`); err != nil {
		t.Fatal(err)
	} else if want := []string{"0"}; !slices.Equal(got, want) {
		t.Errorf("lineage_only on the existing rows = %q, want %q", got, want)
	}
	if got, err := queryIDs(db, `SELECT name || '|' || type || '|' || "notnull" || '|' || dflt_value
		FROM pragma_table_info('thread_requests') WHERE name = 'lineage_only'`); err != nil {
		t.Fatal(err)
	} else if want := []string{"lineage_only|INTEGER|1|0"}; !slices.Equal(got, want) {
		t.Errorf("lineage_only column = %q, want %q", got, want)
	}
	if _, err := db.Exec(`UPDATE thread_requests SET lineage_only = 2 WHERE token = 'tok-local'`); err == nil {
		t.Error("lineage_only accepted 2")
	}
	schemaAfter := migrationSchema(t, db)
	if got := schemaAfter["thread_requests"]; got == schemaBefore["thread_requests"] || !strings.Contains(got, "lineage_only") {
		t.Errorf("thread_requests after v%d = %q, want lineage_only added", spawnLineageMigrationVersion, got)
	}
	for name, sqlText := range schemaBefore {
		if name == "thread_requests" {
			continue
		}
		if schemaAfter[name] != sqlText {
			t.Errorf("schema object %s changed:\n got %q\nwant %q", name, schemaAfter[name], sqlText)
		}
	}
	var added []string
	for name := range schemaAfter {
		if _, ok := schemaBefore[name]; !ok {
			added = append(added, name)
		}
	}
	if want := []string{"idx_thread_requests_spawn_target"}; !slices.Equal(added, want) {
		t.Errorf("v%d added %v, want %v", spawnLineageMigrationVersion, added, want)
	}
	if got, err := queryIDs(db, `SELECT name FROM pragma_index_info('idx_thread_requests_spawn_target') ORDER BY seqno`); err != nil {
		t.Fatal(err)
	} else if want := []string{"target_thread_id"}; !slices.Equal(got, want) {
		t.Errorf("index columns = %q, want %q", got, want)
	}
	// Partial on exactly the rows the sidebar reads: the planner takes the
	// index for a local spawn lookup and leaves it alone for a remote one.
	plan := func(where string) string {
		t.Helper()
		rows, err := db.Query(`EXPLAIN QUERY PLAN SELECT caller_thread_id FROM thread_requests WHERE ` + where)
		if err != nil {
			t.Fatal(err)
		}
		var steps []string
		for rows.Next() {
			var id, parent, notUsed int
			var detail string
			if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
				t.Fatal(err)
			}
			steps = append(steps, detail)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			t.Fatal(err)
		}
		return strings.Join(steps, "\n")
	}
	if got := plan(`kind = 'spawn' AND target_computer_id = '' AND target_thread_id = 'child'`); !strings.Contains(got, "idx_thread_requests_spawn_target") {
		t.Errorf("local spawn lookup does not use the index:\n%s", got)
	}
	if got := plan(`kind = 'spawn' AND target_computer_id = 'laptop' AND target_thread_id = 'far'`); strings.Contains(got, "idx_thread_requests_spawn_target") {
		t.Errorf("remote spawn lookup uses the partial index:\n%s", got)
	}
}
