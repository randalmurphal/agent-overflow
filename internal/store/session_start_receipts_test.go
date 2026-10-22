package store

import "testing"

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
	mustExec(t, db, `INSERT INTO session_start_receipts(thread_id,session_ref,digest) VALUES ('t','s-1','d1')`)
	for _, bad := range []string{
		`INSERT INTO session_start_receipts(thread_id,session_ref,digest) VALUES ('t','s-2','d2')`,
		`INSERT INTO session_start_receipts(thread_id,session_ref,digest) VALUES ('u','','d')`,
		`INSERT INTO session_start_receipts(thread_id,session_ref,digest) VALUES ('u','s-1','')`,
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

func TestSessionStartReceiptIsPerSession(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThreadWithSessionRef(t, s, "t", "s-1")
	read := func(sessionRef string) string {
		t.Helper()
		digest, err := s.SessionStartReceipt("t", sessionRef)
		if err != nil {
			t.Fatal(err)
		}
		return digest
	}
	put := func(sessionRef, digest string) {
		t.Helper()
		if err := s.PutSessionStartReceipt("t", sessionRef, digest); err != nil {
			t.Fatal(err)
		}
	}

	if got := read("s-1"); got != "" {
		t.Fatalf("before any write = %q", got)
	}
	put("s-1", "m1")
	put("s-1", "m2")
	if got := read("s-1"); got != "m2" {
		t.Fatalf("read = %q, want the latest", got)
	}
	// A rollback repoints the thread: the row describes the old session and
	// answers nothing for the new one, until it receives the hook's context,
	// which replaces it.
	if got := read("s-2"); got != "" {
		t.Fatalf("read for another session = %q", got)
	}
	put("s-2", "m3")
	if got := read("s-2"); got != "m3" {
		t.Fatalf("read after the new session's receipt = %q", got)
	}
	if got := read("s-1"); got != "" {
		t.Fatalf("the old session's receipt = %q, want it replaced", got)
	}

	if err := s.ForgetSessionStartReceipt("t"); err != nil {
		t.Fatal(err)
	}
	if got := read("s-2"); got != "" {
		t.Fatalf("read after Forget = %q", got)
	}
	if err := s.ForgetSessionStartReceipt("t"); err != nil {
		t.Fatalf("forgetting nothing: %v", err)
	}

	// A thread deleted before the write lands records nothing.
	if err := s.PutSessionStartReceipt("gone", "s-3", "m"); err != nil {
		t.Fatalf("write for a deleted thread: %v", err)
	}
	if got, err := s.SessionStartReceipt("gone", "s-3"); err != nil || got != "" {
		t.Fatalf("read for a deleted thread = %q, %v", got, err)
	}
	for _, args := range [][3]string{{"t", "", "m"}, {"t", "s-1", ""}, {"", "s-1", "m"}} {
		if err := s.PutSessionStartReceipt(args[0], args[1], args[2]); err == nil {
			t.Errorf("receipt %q was accepted", args)
		}
	}
}
