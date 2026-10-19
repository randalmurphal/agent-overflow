package store

import (
	"maps"
	"testing"
)

func TestSessionStartReceiptsMigrationAddsAnEmptyTable(t *testing.T) {
	db := migrateThrough(t, sessionStartReceiptsMigrationVersion-1)
	mustExec(t, db, `INSERT INTO projects(id,path,name,slug,sort_position,created_at,updated_at) VALUES ('p','/p','p','p',0,1,1)`)
	mustExec(t, db, `INSERT INTO threads(id,project_id,title,provider,model,workspace_path,created_at,updated_at) VALUES ('t','p','T','claude','m','/p',1,1)`)
	if err := applyMigration(db, migrationByVersion(t, sessionStartReceiptsMigrationVersion)); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM session_start_receipts`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rows = %d, %v; want an empty table", rows, err)
	}
	mustExec(t, db, `INSERT INTO session_start_receipts(thread_id,hook,session_ref,digest) VALUES ('t','MEMORY.md','s-1','d1')`)
	mustExec(t, db, `INSERT INTO session_start_receipts(thread_id,hook,session_ref,digest) VALUES ('t','instructions.md','s-1','d2')`)
	for _, bad := range []string{
		`INSERT INTO session_start_receipts(thread_id,hook,session_ref,digest) VALUES ('t','MEMORY.md','s-1','d3')`,
		`INSERT INTO session_start_receipts(thread_id,hook,session_ref,digest) VALUES ('t','','s-1','d')`,
		`INSERT INTO session_start_receipts(thread_id,hook,session_ref,digest) VALUES ('t','notes.md','','d')`,
		`INSERT INTO session_start_receipts(thread_id,hook,session_ref,digest) VALUES ('t','notes.md','s-1','')`,
	} {
		if _, err := db.Exec(bad); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
	mustExec(t, db, `PRAGMA foreign_keys = ON`)
	mustExec(t, db, `DELETE FROM threads WHERE id = 't'`)
	if err := db.QueryRow(`SELECT COUNT(*) FROM session_start_receipts`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rows after the thread's delete = %d, %v; want the cascade", rows, err)
	}
}

func TestSessionStartReceiptsArePerHookAndSession(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThreadWithSessionRef(t, s, "t", "s-1")
	read := func(sessionRef string) map[string]string {
		t.Helper()
		receipts, err := s.SessionStartReceipts("t", sessionRef)
		if err != nil {
			t.Fatal(err)
		}
		return receipts
	}
	put := func(sessionRef, hook, digest string) {
		t.Helper()
		if err := s.PutSessionStartReceipt("t", sessionRef, hook, digest); err != nil {
			t.Fatal(err)
		}
	}

	if got := read("s-1"); len(got) != 0 {
		t.Fatalf("before any write = %v", got)
	}
	put("s-1", "instructions.md", "i1")
	put("s-1", "MEMORY.md", "m1")
	put("s-1", "MEMORY.md", "m2")
	if got, want := read("s-1"), map[string]string{"instructions.md": "i1", "MEMORY.md": "m2"}; !maps.Equal(got, want) {
		t.Fatalf("read = %v, want %v", got, want)
	}
	// A rollback repoints the thread: the rows describe the old session and
	// answer nothing for the new one, until it receives a hook's context.
	if got := read("s-2"); len(got) != 0 {
		t.Fatalf("read for another session = %v", got)
	}
	put("s-2", "MEMORY.md", "m3")
	if got, want := read("s-2"), map[string]string{"MEMORY.md": "m3"}; !maps.Equal(got, want) {
		t.Fatalf("read after the new session's receipt = %v, want %v", got, want)
	}
	if got, want := read("s-1"), map[string]string{"instructions.md": "i1"}; !maps.Equal(got, want) {
		t.Fatalf("the old session's rows = %v, want %v", got, want)
	}

	if err := s.ForgetSessionStartReceipts("t"); err != nil {
		t.Fatal(err)
	}
	if got := read("s-1"); len(got) != 0 {
		t.Fatalf("read after Forget = %v", got)
	}
	if err := s.ForgetSessionStartReceipts("t"); err != nil {
		t.Fatalf("forgetting nothing: %v", err)
	}

	// A thread deleted before the write lands records nothing.
	if err := s.PutSessionStartReceipt("gone", "s-3", "MEMORY.md", "m"); err != nil {
		t.Fatalf("write for a deleted thread: %v", err)
	}
	if got, err := s.SessionStartReceipts("gone", "s-3"); err != nil || len(got) != 0 {
		t.Fatalf("read for a deleted thread = %v, %v", got, err)
	}
	for _, args := range [][4]string{{"t", "", "MEMORY.md", "m"}, {"t", "s-1", "", "m"}, {"t", "s-1", "MEMORY.md", ""}} {
		if err := s.PutSessionStartReceipt(args[0], args[1], args[2], args[3]); err == nil {
			t.Errorf("receipt %q was accepted", args)
		}
	}
}
