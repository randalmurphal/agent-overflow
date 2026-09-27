package store

import (
	"strings"
	"testing"
)

// v136 indexes the top-level rows of local and imported history, and
// leaves every child row out of them.
func TestTopLevelTimelineIndexMigration(t *testing.T) {
	db := migrateThrough(t, topLevelTimelineIndexMigrationVersion-1)
	seedMigrationThread(t, db, "t")
	for _, row := range []migrationRow{
		{id: "ask", kind: "user_text", role: "user", item: 0},
		{id: "launch", kind: "tool_call", tool: "Task", item: 1},
		{id: "child", kind: "tool_call", tool: "Grep", parent: "launch", item: 2},
		{id: "grandchild", kind: "assistant_text", parent: "child", item: 3},
		{id: "reply", kind: "assistant_text", item: 4},
	} {
		if err := insertMigrationRow(db, "t", row); err != nil {
			t.Fatalf("seed %s: %v", row.id, err)
		}
	}
	if err := applyMigration(db, migrationByVersion(t, topLevelTimelineIndexMigrationVersion)); err != nil {
		t.Fatalf("apply v%d: %v", topLevelTimelineIndexMigrationVersion, err)
	}
	statements := strings.SplitN(topLevelTimelineIndexV136SQL, ";", 2)
	for i, name := range []string{"idx_items_top_level", "idx_import_history_items_top_level"} {
		if got := readIndexSQL(t, db, name); normalizeSQLText(got) != normalizeSQLText(statements[i]) {
			t.Errorf("%s:\n got  %s\n want %s", name, got, statements[i])
		}
	}
	rows, err := db.Query(`SELECT id FROM items INDEXED BY idx_items_top_level
		WHERE thread_id = 't' AND NOT (kind = 'notification' AND tool_name = 'plan_update') AND parent_id = ''
		ORDER BY turn_index, item_index`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(ids, ",") != "ask,launch,reply" {
		t.Errorf("top-level index holds %v, want ask, launch, reply", ids)
	}
}
