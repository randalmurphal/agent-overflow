package store

import (
	"database/sql"
	"testing"
)

// TestMigrationV133SettledLaunchesStaySettled: v133 drops both revive
// triggers and keeps the settle triggers. Before it, deleting or moving a
// launch's ending sibling made the launch live again; after it, the
// launch stays settled, and an ending sibling still settles a launch.
func TestMigrationV133SettledLaunchesStaySettled(t *testing.T) {
	db := migrateThrough(t, 132)
	triggers := func() map[string]bool {
		t.Helper()
		names, err := queryIDs(db, `SELECT name FROM sqlite_master WHERE type = 'trigger' ORDER BY name`)
		if err != nil {
			t.Fatal(err)
		}
		out := make(map[string]bool, len(names))
		for _, name := range names {
			out[name] = true
		}
		return out
	}
	settled := func(thread, id string) bool {
		t.Helper()
		var flag sql.NullInt64
		if err := db.QueryRow(`SELECT json_extract(meta, '$.live_background_active') FROM items WHERE thread_id = ? AND id = ?`,
			thread, id).Scan(&flag); err != nil {
			t.Fatal(err)
		}
		return flag.Valid && flag.Int64 == 0
	}
	seedMigrationThread(t, db, "t", "holder")
	seed := func(rows ...migrationRow) {
		t.Helper()
		for _, row := range rows {
			if err := insertMigrationRow(db, "t", row); err != nil {
				t.Fatalf("seed %s: %v", row.id, err)
			}
		}
	}
	launch := func(id string, item int) migrationRow {
		return migrationRow{id: id, kind: "tool_call", status: "running", tool: "Bash", background: 1, item: item, created: int64(item)}
	}
	ending := func(id, of string, item int) migrationRow {
		return migrationRow{id: id, kind: "tool_completion", completionOf: of, tool: "Bash", background: 1, item: item, created: int64(item)}
	}
	seed(launch("old", 0), ending("old-end", "old", 1))

	before := triggers()
	for _, name := range append(append([]string{}, backgroundSettleTriggerNames...), revivedTriggerNames...) {
		if !before[name] {
			t.Fatalf("trigger %s missing before v133", name)
		}
	}
	// The v132 schema revives: the precondition that makes the checks
	// after the migration mean something.
	mustExec(t, db, `DELETE FROM items WHERE thread_id = 't' AND id = 'old-end'`)
	if settled("t", "old") {
		t.Fatal("the v132 schema kept a launch settled when its ending sibling went")
	}

	if err := applyMigration(db, migrationByVersion(t, 133)); err != nil {
		t.Fatalf("apply v133: %v", err)
	}
	after := triggers()
	for _, name := range revivedTriggerNames {
		if after[name] {
			t.Errorf("trigger %s survived v133", name)
		}
	}
	for name := range before {
		if !after[name] && name != revivedTriggerNames[0] && name != revivedTriggerNames[1] {
			t.Errorf("v133 dropped trigger %s", name)
		}
	}
	if len(after) != len(before)-len(revivedTriggerNames) {
		t.Errorf("v133 left %d triggers, want %d", len(after), len(before)-len(revivedTriggerNames))
	}

	seed(launch("deleted", 2), ending("deleted-end", "deleted", 3), launch("moved", 4), ending("moved-end", "moved", 5))
	if !settled("t", "deleted") || !settled("t", "moved") {
		t.Fatal("an ending sibling did not settle its launch after v133")
	}
	mustExec(t, db, `DELETE FROM items WHERE thread_id = 't' AND id = 'deleted-end'`)
	mustExec(t, db, `UPDATE items SET thread_id = 'holder' WHERE thread_id = 't' AND id = 'moved-end'`)
	if !settled("t", "deleted") {
		t.Error("deleting the ending sibling revived its launch")
	}
	if !settled("t", "moved") {
		t.Error("moving the ending sibling to a holder revived its launch")
	}
	migrateFrom(t, db, 133)
}
