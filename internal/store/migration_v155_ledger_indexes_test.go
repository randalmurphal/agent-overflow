package store

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// v155 over a populated database: the ledger and wake indexes exist, each
// thread keeps its 16 newest cancelled wakes with nothing they carried, a
// live wake keeps its output, a watch whose thread is gone is deleted, and
// the rest keep every column and now cascade from their thread.
func TestMigrationV155LedgerIndexes(t *testing.T) {
	path := fileDBThrough(t, ledgerIndexesMigrationVersion-1)
	db, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/p', 'p', 1, 1)`)
	for _, id := range []string{"a", "b"} {
		mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived)
			VALUES (?, 'p', ?, 'claude', '/p', '', 1, 1, 0)`, id, id)
	}
	for i := range 20 {
		mustExec(t, db, `INSERT INTO agent_wakes (id, thread_id, kind, label, state, created_at, last_output, last_stderr)
			VALUES (?, 'a', 'watch', ?, 'cancelled', ?, 'kept output', 'kept stderr')`, fmt.Sprintf("a-%02d", i), fmt.Sprintf("l%02d", i), i)
	}
	mustExec(t, db, `INSERT INTO agent_wakes (id, thread_id, kind, label, state, created_at, last_output, last_stderr)
		VALUES ('a-live', 'a', 'watch', 'live', 'armed', 1, 'live output', 'live stderr'),
		       ('b-gone', 'b', 'loop', 'old', 'cancelled', 1, '', '')`)
	mustExec(t, db, `INSERT INTO remote_watches (computer_id, request_id, thread_id, launched_in_thread_id, fingerprint, label, receipt, error,
		notification, next_check, created_at, command, confirmed_at, wake_attempts, wake_issue)
		VALUES ('pc', 'kept', 'a', 'a', 'f', 'job', '{"id":"kept"}', 'e', 'queued', 5, 6, 'cmd', 7, 2, 'issue'),
		       ('pc', 'orphan', 'deleted-thread', 'deleted-thread', 'f', 'job', '{}', '', 'pending', 0, 1, '', NULL, 0, '')`)
	watchColumns := `computer_id || '|' || request_id || '|' || thread_id || '|' || launched_in_thread_id || '|' || fingerprint || '|' || label || '|' ||
		receipt || '|' || error || '|' || notification || '|' || next_check || '|' || created_at || '|' || command || '|' ||
		coalesce(confirmed_at, '-') || '|' || wake_attempts || '|' || wake_issue`

	if err := applyRebuildMigration(db, migrationByVersion(t, ledgerIndexesMigrationVersion)); err != nil {
		t.Fatalf("apply v%d: %v", ledgerIndexesMigrationVersion, err)
	}

	for _, index := range []string{
		"idx_thread_requests_caller_settled", "idx_thread_requests_spawn_caller", "idx_thread_requests_nested_caller",
		"idx_thread_requests_age", "idx_thread_requests_lineage", "idx_thread_request_receipts_age",
		"idx_agent_wakes_thread", "idx_remote_watches_pending", "idx_remote_watches_thread", "idx_remote_watches_done",
	} {
		if readIndexSQL(t, db, index) == "" {
			t.Errorf("index %s is missing", index)
		}
	}
	cancelled, err := queryIDs(db, `SELECT id FROM agent_wakes WHERE thread_id = 'a' AND state = 'cancelled' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for i := 4; i < 20; i++ {
		want = append(want, fmt.Sprintf("a-%02d", i))
	}
	if !slices.Equal(cancelled, want) {
		t.Errorf("cancelled rows kept = %v, want the 16 newest", cancelled)
	}
	if got, err := queryIDs(db, `SELECT id || ':' || last_output || ':' || last_stderr FROM agent_wakes WHERE id IN ('a-19', 'a-live', 'b-gone') ORDER BY id`); err != nil {
		t.Fatal(err)
	} else if want := []string{"a-19::", "a-live:live output:live stderr", "b-gone::"}; !slices.Equal(got, want) {
		t.Errorf("wake outputs = %q, want %q", got, want)
	}
	if got, err := queryIDs(db, `SELECT `+watchColumns+` FROM remote_watches`); err != nil {
		t.Fatal(err)
	} else if want := []string{`pc|kept|a|a|f|job|{"id":"kept"}|e|queued|5|6|cmd|7|2|issue`}; !slices.Equal(got, want) {
		t.Errorf("watches = %q, want %q", got, want)
	}
	var fk string
	if err := db.QueryRow(`SELECT "table" || '.' || "to" || ' ' || on_delete FROM pragma_foreign_key_list('remote_watches') WHERE "from" = 'thread_id'`).Scan(&fk); err != nil {
		t.Fatalf("remote_watches has no foreign key on thread_id: %v", err)
	}
	if fk != "threads.id CASCADE" {
		t.Errorf("remote_watches.thread_id references %q, want threads.id CASCADE", fk)
	}
	mustExec(t, db, `DELETE FROM threads WHERE id = 'a'`)
	if got, err := queryIDs(db, `SELECT request_id FROM remote_watches`); err != nil || len(got) != 0 {
		t.Errorf("watches after their thread was deleted = %v, %v", got, err)
	}
	if !strings.Contains(readIndexSQL(t, db, "idx_remote_watches_pending"), "notification='pending'") {
		t.Error("idx_remote_watches_pending lost its predicate")
	}
}
