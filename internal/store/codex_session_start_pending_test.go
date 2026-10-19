package store

import "testing"

func TestCodexSessionStartPendingMigrationAddsAnEmptyTable(t *testing.T) {
	db := migrateThrough(t, codexSessionStartPendingMigrationVersion-1)
	mustExec(t, db, `INSERT INTO projects(id,path,name,slug,sort_position,created_at,updated_at) VALUES ('p','/p','p','p',0,1,1)`)
	mustExec(t, db, `INSERT INTO threads(id,project_id,title,provider,model,workspace_path,created_at,updated_at) VALUES ('t','p','T','codex','m','/p',1,1)`)
	if err := applyMigration(db, migrationByVersion(t, codexSessionStartPendingMigrationVersion)); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM codex_session_start_pending`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rows = %d, %v; want an empty table", rows, err)
	}
	mustExec(t, db, `INSERT INTO codex_session_start_pending(thread_id,session_ref,after) VALUES ('t','codex-1','start')`)
	mustExec(t, db, `UPDATE codex_session_start_pending SET after = 'compaction'`)
	for _, bad := range []string{
		`INSERT INTO codex_session_start_pending(thread_id,session_ref,after) VALUES ('t2','','start')`,
		`UPDATE codex_session_start_pending SET after = 'resume'`,
		`UPDATE codex_session_start_pending SET after = ''`,
	} {
		if _, err := db.Exec(bad); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
	mustExec(t, db, `PRAGMA foreign_keys = ON`)
	mustExec(t, db, `DELETE FROM threads WHERE id = 't'`)
	if err := db.QueryRow(`SELECT COUNT(*) FROM codex_session_start_pending`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rows after the thread's delete = %d, %v; want the cascade", rows, err)
	}
}

func TestCodexSessionStartPendingIsPerCodexThread(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThreadWithSessionRef(t, s, "t", "codex-1")
	read := func(sessionRef string) string {
		t.Helper()
		after, err := s.CodexSessionStartPending("t", sessionRef)
		if err != nil {
			t.Fatal(err)
		}
		return after
	}

	if got := read("codex-1"); got != "" {
		t.Fatalf("before any write = %q, want nothing owed", got)
	}
	if err := s.SetCodexSessionStartPending("t", "codex-1", "start"); err != nil {
		t.Fatal(err)
	}
	if got := read("codex-1"); got != "start" {
		t.Fatalf("read = %q, want start", got)
	}
	if err := s.SetCodexSessionStartPending("t", "codex-1", "compaction"); err != nil {
		t.Fatal(err)
	}
	if got := read("codex-1"); got != "compaction" {
		t.Fatalf("read after a compaction = %q, want compaction", got)
	}
	// A rollback that forks repoints the thread: the row describes the old
	// Codex thread and answers nothing for the new one.
	if got := read("codex-2"); got != "" {
		t.Fatalf("read for another Codex thread = %q, want nothing owed", got)
	}

	if err := s.SetCodexSessionStartPending("t", "codex-1", ""); err != nil {
		t.Fatal(err)
	}
	if got := read("codex-1"); got != "" {
		t.Fatalf("read after the clear = %q", got)
	}
	if err := s.SetCodexSessionStartPending("t", "codex-1", ""); err != nil {
		t.Fatalf("clearing a missing row: %v", err)
	}
	if err := s.SetCodexSessionStartPending("t", "codex-1", "resume"); err == nil {
		t.Fatal("an unknown event was recorded")
	}

	// A thread deleted before the write lands records nothing.
	if err := s.SetCodexSessionStartPending("gone", "codex-3", "start"); err != nil {
		t.Fatalf("write for a deleted thread: %v", err)
	}
	if got, err := s.CodexSessionStartPending("gone", "codex-3"); err != nil || got != "" {
		t.Fatalf("read for a deleted thread = %q, %v", got, err)
	}
	if err := s.SetCodexSessionStartPending("t", "", "start"); err == nil {
		t.Fatal("a record without the Codex thread it describes was accepted")
	}
}
