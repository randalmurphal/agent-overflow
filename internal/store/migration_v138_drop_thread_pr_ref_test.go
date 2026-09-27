package store

import (
	"database/sql"
	"testing"
)

// v138 drops threads.pr_ref and keeps every other thread column and row.
func TestDropThreadPRRefMigration(t *testing.T) {
	db := migrateThrough(t, dropThreadPRRefMigrationVersion-1)
	seedMigrationThread(t, db, "t")
	mustExec(t, db, `UPDATE threads SET pr_ref = '{"forge":"github"}', branch = 'feature' WHERE id = 't'`)
	before := threadColumnSet(t, db)
	if !before["pr_ref"] {
		t.Fatal("pr_ref missing before v138")
	}

	if err := applyMigration(db, migrationByVersion(t, dropThreadPRRefMigrationVersion)); err != nil {
		t.Fatalf("apply v%d: %v", dropThreadPRRefMigrationVersion, err)
	}

	after := threadColumnSet(t, db)
	if after["pr_ref"] {
		t.Fatal("pr_ref survived v138")
	}
	delete(before, "pr_ref")
	for column := range before {
		if !after[column] {
			t.Errorf("column %s lost by v138", column)
		}
	}
	if len(after) != len(before) {
		t.Errorf("columns after v138 = %d, want %d", len(after), len(before))
	}
	var branch string
	if err := db.QueryRow(`SELECT branch FROM threads WHERE id = 't'`).Scan(&branch); err != nil {
		t.Fatalf("read thread after v138: %v", err)
	}
	if branch != "feature" {
		t.Fatalf("branch = %q, want the row kept", branch)
	}
}

func threadColumnSet(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_table_info('threads')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return columns
}
