package store

import "testing"

// v135 indexes the summary-only tool_result payloads, the rows a Codex turn
// diff can upgrade, and a payload meta that is not JSON still writes.
func TestSummaryOnlyDiffIndexMigration(t *testing.T) {
	db := migrateThrough(t, summaryOnlyDiffIndexMigrationVersion-1)
	seedMigrationThread(t, db, "t")
	for _, p := range []struct{ id, kind, meta string }{
		{"summary", "tool_result", `{"inlineDiff":{"availability":"summary_only"}}`},
		{"exact", "tool_result", `{"inlineDiff":{"availability":"exact_patch"}}`},
		{"other-kind", "diff", `{"inlineDiff":{"availability":"summary_only"}}`},
		{"no-diff", "tool_result", `{}`},
	} {
		mustExec(t, db, `INSERT INTO payloads (thread_id, id, kind, meta, data, created_at) VALUES ('t', ?, ?, ?, x'', 1)`, p.id, p.kind, p.meta)
	}
	if err := applyMigration(db, migrationByVersion(t, summaryOnlyDiffIndexMigrationVersion)); err != nil {
		t.Fatalf("apply v%d: %v", summaryOnlyDiffIndexMigrationVersion, err)
	}
	if got := readIndexSQL(t, db, "idx_payloads_summary_only_diff"); normalizeSQLText(got) != normalizeSQLText(summaryOnlyDiffIndexV135SQL) {
		t.Errorf("idx_payloads_summary_only_diff:\n got  %s\n want %s", got, summaryOnlyDiffIndexV135SQL)
	}
	mustExec(t, db, `INSERT INTO payloads (thread_id, id, kind, meta, data, created_at) VALUES ('t', 'malformed', 'tool_result', 'not json', x'', 1)`)
	var ids []string
	rows, err := db.Query(`SELECT id FROM payloads INDEXED BY idx_payloads_summary_only_diff
		WHERE thread_id = 't' AND ` + summaryOnlyDiffPayloadSQL + ` ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
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
	if len(ids) != 1 || ids[0] != "summary" {
		t.Errorf("index holds %v, want the one summary-only tool result", ids)
	}
}
