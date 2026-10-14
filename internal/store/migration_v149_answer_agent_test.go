package store

import (
	"database/sql"
	"slices"
	"testing"
)

// v149 over a populated database: every request, settled or open, reads as
// written before v149, with nothing recorded about the thread that answered;
// every receipt reads as answered by no agent; no other column changes.
func TestMigrationV149AnswerAgent(t *testing.T) {
	path := fileDBThrough(t, answerAgentMigrationVersion-1)
	db, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/p', 'p', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived) VALUES
		('caller', 'p', 'Caller', 'claude', '/p', '', 1, 1, 0)`)
	mustExec(t, db, `INSERT INTO thread_requests (token, caller_thread_id, kind, target_computer_id, target_thread_id, target_thread_title, state, answer, settled_at, delivered_at, delivered_how, late_reply, late_delivered_at, confirmed_at, created_at, updated_at) VALUES
		('local', 'caller', 'send', '', 't', '', 'finished', 'a', 10, 20, 'queued', 'b', 30, 20, 1, 1),
		('remote', 'caller', 'send', 'c', 't', 'Their thread', 'replied', 'a', 10, NULL, '', NULL, NULL, NULL, 1, 1),
		('open', 'caller', 'ask', '', '', '', 'running', NULL, NULL, NULL, '', NULL, NULL, NULL, 1, 1)`)
	mustExec(t, db, `INSERT INTO thread_request_receipts (token, owner_device_id, kind, target_thread_id, state, answer, settled_at, created_at, updated_at) VALUES
		('settled', 'local', 'send', 'caller', 'finished', 'a', 10, 1, 1),
		('open', 'local', 'send', 'caller', 'running', NULL, NULL, 1, 1)`)
	before, err := queryIDs(db, `SELECT `+threadRequestColumnsBeforeV149+` FROM thread_requests ORDER BY token`)
	if err != nil {
		t.Fatal(err)
	}
	receiptsBefore, err := queryIDs(db, `SELECT `+threadReceiptColumnsBeforeV149+` FROM thread_request_receipts ORDER BY token`)
	if err != nil {
		t.Fatal(err)
	}

	if err := applyMigration(db, migrationByVersion(t, answerAgentMigrationVersion)); err != nil {
		t.Fatalf("apply v%d: %v", answerAgentMigrationVersion, err)
	}

	if got, err := queryIDs(db, `SELECT `+threadRequestColumnsBeforeV149+` FROM thread_requests ORDER BY token`); err != nil {
		t.Fatal(err)
	} else if !slices.Equal(got, before) {
		t.Errorf("thread_requests changed:\n got %q\nwant %q", got, before)
	}
	got, err := queryIDs(db, `SELECT token || '|' || COALESCE(answer_agent, 'NULL') || '|' || answer_agent_role FROM thread_requests ORDER BY token`)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"local|NULL|", "open|NULL|", "remote|NULL|"}; !slices.Equal(got, want) {
		t.Errorf("answer agents = %q, want %q", got, want)
	}
	if got, err := queryIDs(db, `SELECT `+threadReceiptColumnsBeforeV149+` FROM thread_request_receipts ORDER BY token`); err != nil {
		t.Fatal(err)
	} else if !slices.Equal(got, receiptsBefore) {
		t.Errorf("thread_request_receipts changed:\n got %q\nwant %q", got, receiptsBefore)
	}
	got, err = queryIDs(db, `SELECT token || '|' || answer_agent || '|' || answer_agent_role FROM thread_request_receipts ORDER BY token`)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"open|0|", "settled|0|"}; !slices.Equal(got, want) {
		t.Errorf("receipt answer agents = %q, want %q", got, want)
	}
}

// The receipt columns before v149, rendered as one string per row.
const threadReceiptColumnsBeforeV149 = `token || '|' || owner_device_id || '|' || kind || '|' || COALESCE(target_thread_id, 'NULL') || '|' ||
	state || '|' || COALESCE(answer, 'NULL') || '|' || COALESCE(settled_at, 'NULL') || '|' || revision || '|' || updated_at`

// The thread_requests columns before v149, rendered as one string per row.
const threadRequestColumnsBeforeV149 = `token || '|' || caller_thread_id || '|' || kind || '|' || target_computer_id || '|' ||
	target_thread_id || '|' || target_thread_title || '|' || state || '|' || COALESCE(answer, 'NULL') || '|' ||
	COALESCE(settled_at, 'NULL') || '|' || COALESCE(delivered_at, 'NULL') || '|' || delivered_how || '|' ||
	COALESCE(late_reply, 'NULL') || '|' || COALESCE(late_delivered_at, 'NULL') || '|' || COALESCE(confirmed_at, 'NULL') || '|' ||
	COALESCE(late_confirmed_at, 'NULL') || '|' || revision || '|' || notify || '|' || lineage_only || '|' || updated_at`
