package store

import (
	"strings"
	"testing"
)

// v134 indexes the rows that carry a provider item id, so the lookup by
// that id seeks it instead of reading the turn.
func TestProviderItemIndexMigration(t *testing.T) {
	db := migrateThrough(t, providerItemIndexMigrationVersion-1)
	seedMigrationThread(t, db, "t")
	for _, row := range []migrationRow{
		{id: "plain", kind: "assistant_text", item: 0},
		{id: "streamed", kind: "assistant_text", item: 1, meta: `{"provider_item_id":"prov-1"}`},
		{id: "thought", kind: "thinking", item: 2, meta: `{"provider_item_id":"prov-2"}`},
	} {
		if err := insertMigrationRow(db, "t", row); err != nil {
			t.Fatalf("seed %s: %v", row.id, err)
		}
	}
	if err := applyMigration(db, migrationByVersion(t, providerItemIndexMigrationVersion)); err != nil {
		t.Fatalf("apply v%d: %v", providerItemIndexMigrationVersion, err)
	}
	if got := readIndexSQL(t, db, "idx_items_provider_item"); normalizeSQLText(got) != normalizeSQLText(providerItemIndexV134SQL) {
		t.Errorf("idx_items_provider_item:\n got  %s\n want %s", got, providerItemIndexV134SQL)
	}
	var indexed int
	if err := db.QueryRow(`SELECT COUNT(*) FROM items INDEXED BY idx_items_provider_item
		WHERE thread_id = 't' AND json_extract(meta, '$.provider_item_id') IS NOT NULL`).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if indexed != 2 {
		t.Errorf("index holds %d rows, want the 2 with a provider item id", indexed)
	}
	const lookup = `SELECT id FROM items
		WHERE thread_id = ? AND turn_index = ? AND json_extract(meta, '$.provider_item_id') = ?`
	rows, err := db.Query("EXPLAIN QUERY PLAN "+lookup, "t", 0, "prov-1")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want := "idx_items_provider_item (thread_id=? AND <expr>=? AND turn_index=?)"; !strings.Contains(strings.Join(plan, "\n"), want) {
		t.Errorf("lookup plan does not seek %s:\n%s", want, strings.Join(plan, "\n"))
	}
	var id string
	if err := db.QueryRow(lookup, "t", 0, "prov-1").Scan(&id); err != nil || id != "streamed" {
		t.Errorf("lookup = %q, %v; want streamed", id, err)
	}
}
