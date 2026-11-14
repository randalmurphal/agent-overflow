package store

import (
	"strings"
	"testing"
)

// v149 adds the carrier a request's answer was delivered with. Every row the
// store already holds was delivered by its own message or not at all, so the
// column is empty for all of them, and the lookup from a carrier to its
// riders has its index.
func TestThreadWakeCarrierMigrationDefaultsToTheRowsOwnMessage(t *testing.T) {
	db := migrateThrough(t, threadWakeCarrierMigrationVersion-1)
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at)
		VALUES ('p-carrier', '/tmp/carrier', 'Carrier', 1, 1)`)
	mustExec(t, db, `
		INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
			created_at, updated_at, archived, mode)
		VALUES ('t-carrier', 'p-carrier', 'Caller', 'claude', '/tmp/carrier', 'test-model', 1, 1, 0, 'chat')`)
	mustExec(t, db, `
		INSERT INTO thread_requests (token, caller_thread_id, kind, state, settled_at,
			delivered_at, delivered_how, created_at, updated_at)
		VALUES ('tok-old', 't-carrier', 'send', 'finished', 5, 6, 'queued', 1, 1)`)

	migrateFrom(t, db, threadWakeCarrierMigrationVersion-1)

	var carrier string
	if err := db.QueryRow(`SELECT delivered_with FROM thread_requests WHERE token = 'tok-old'`).Scan(&carrier); err != nil {
		t.Fatalf("read delivered_with: %v", err)
	}
	if carrier != "" {
		t.Fatalf("delivered_with = %q on a row from before the column, want empty", carrier)
	}
	var plan string
	rows, err := db.Query(`EXPLAIN QUERY PLAN `+threadWakeRidersToDraftSQL, ThreadWakeDraft, 2, "tok-old", ThreadWakeQueued)
	if err != nil {
		t.Fatalf("explain the rider lookup: %v", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("close the plan rows: %v", err)
		}
	}()
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan += detail + "\n"
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read plan: %v", err)
	}
	if want := "idx_thread_requests_delivered_with"; !strings.Contains(plan, want) {
		t.Fatalf("rider lookup plan = %q, want %s", plan, want)
	}
}
