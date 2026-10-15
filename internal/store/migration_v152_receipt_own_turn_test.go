package store

import (
	"database/sql"
	"slices"
	"testing"
)

// v152 over a populated database: every receipt, bound or not, reads as
// delivered into a turn it did not open; no other column changes.
func TestMigrationV152ReceiptOwnTurn(t *testing.T) {
	path := fileDBThrough(t, receiptOwnTurnMigrationVersion-1)
	db, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/p', 'p', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived) VALUES
		('target', 'p', 'Target', 'claude', '/p', '', 1, 1, 0)`)
	mustExec(t, db, `INSERT INTO thread_request_receipts (token, owner_device_id, kind, target_thread_id, message_item_id, turn_id, state, answer, settled_at, created_at, updated_at) VALUES
		('bound', 'local', 'send', 'target', 'user:0', 'target:0', 'running', NULL, NULL, 1, 1),
		('settled', 'local', 'send', 'target', 'user:1', 'target:1', 'finished', 'a', 10, 1, 1),
		('unbound', 'local', 'send', 'target', '', '', 'accepted', NULL, NULL, 1, 1)`)
	const before = threadReceiptColumnsBeforeV150 + ` || '|' || message_item_id || '|' || turn_id || '|' || answer_agent || '|' || answer_agent_role`
	want, err := queryIDs(db, `SELECT `+before+` FROM thread_request_receipts ORDER BY token`)
	if err != nil {
		t.Fatal(err)
	}

	if err := applyMigration(db, migrationByVersion(t, receiptOwnTurnMigrationVersion)); err != nil {
		t.Fatalf("apply v%d: %v", receiptOwnTurnMigrationVersion, err)
	}

	if got, err := queryIDs(db, `SELECT `+before+` FROM thread_request_receipts ORDER BY token`); err != nil {
		t.Fatal(err)
	} else if !slices.Equal(got, want) {
		t.Errorf("thread_request_receipts changed:\n got %q\nwant %q", got, want)
	}
	got, err := queryIDs(db, `SELECT token || '|' || own_turn FROM thread_request_receipts ORDER BY token`)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"bound|0", "settled|0", "unbound|0"}; !slices.Equal(got, want) {
		t.Errorf("own turns = %q, want %q", got, want)
	}
}
