package store

import (
	"database/sql"
	"strings"
	"testing"
)

// v128 installs the stop and wake indexes the run-state reads name, and
// triggers that leave a parked sibling out of the rows a write under its
// launch stamps, where v127's stamped it.
func TestAgentStopReadsMigration(t *testing.T) {
	db := migrateThrough(t, agentStopReadsMigrationVersion-1)
	seedMigrationThread(t, db, "t")
	for _, row := range []migrationRow{
		{id: "launch", kind: "tool_call", status: "running", tool: "Agent", background: 1, created: 1000},
		{id: "stop", kind: "tool_completion", status: ItemStatusParked, tool: "Agent", completionOf: "launch", background: 1, item: 1, created: 2000},
	} {
		if err := insertMigrationRow(db, "t", row); err != nil {
			t.Fatalf("seed %s: %v", row.id, err)
		}
	}
	stopRev := func() int64 {
		t.Helper()
		var rev int64
		if err := db.QueryRow(`SELECT rev FROM items WHERE thread_id = 't' AND id = 'stop'`).Scan(&rev); err != nil {
			t.Fatalf("read the stop's rev: %v", err)
		}
		return rev
	}
	writeChild := func(id string, item int) {
		t.Helper()
		if err := insertMigrationRow(db, "t", migrationRow{id: id, kind: "tool_call", tool: "Read", parent: "launch", item: item, created: 3000}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	before := stopRev()
	writeChild("child-1", 2)
	if stopRev() == before {
		t.Fatal("v127's triggers left the parked sibling unstamped; the migration has nothing to change")
	}
	if err := applyMigration(db, migrationByVersion(t, agentStopReadsMigrationVersion)); err != nil {
		t.Fatalf("apply v%d: %v", agentStopReadsMigrationVersion, err)
	}
	before = stopRev()
	writeChild("child-2", 3)
	if got := stopRev(); got != before {
		t.Errorf("after v128 a write under the launch restamped its parked sibling from %d to %d", before, got)
	}

	for name, want := range map[string]string{
		"idx_items_completion_of": `CREATE INDEX idx_items_completion_of
    ON items(thread_id, completion_of, created_at, turn_index, item_index) WHERE completion_of <> ''`,
		"idx_items_subagent_wake": `CREATE INDEX idx_items_subagent_wake
    ON items(thread_id, parent_id, created_at)
 WHERE kind = 'user_text' AND parent_id <> '' AND ` + wakeFlagSQL(""),
	} {
		if got := readIndexSQL(t, db, name); normalizeSQLText(got) != normalizeSQLText(want) {
			t.Errorf("%s:\n got  %s\n want %s", name, got, want)
		}
	}
	installed := normalizeSQLText(historyRevTriggersSQL + subagentAggregateTriggersSQL)
	for _, name := range []string{
		"trg_items_rev_insert", "trg_items_rev_update", "trg_items_rev_delete",
		"trg_subagent_aggregates_stamp_insert", "trg_subagent_aggregates_stamp_update",
	} {
		got := readTriggerSQL(t, db, name)
		if !strings.Contains(installed, normalizeSQLText(got)) {
			t.Errorf("%s is not the trigger the store installs:\n%s", name, got)
		}
		if !strings.Contains(got, "status <> '"+ItemStatusParked+"'") {
			t.Errorf("%s stamps parked siblings:\n%s", name, got)
		}
	}
}

func readTriggerSQL(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var text string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = ?`, name).Scan(&text); err != nil {
		t.Fatalf("read trigger %s: %v", name, err)
	}
	return text
}
