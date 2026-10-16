package store

import (
	"strings"
	"testing"
)

func TestCodexDeveloperInstructionsMigrationAddsAnEmptyTable(t *testing.T) {
	db := migrateThrough(t, codexDeveloperInstructionsMigrationVersion-1)
	mustExec(t, db, `INSERT INTO projects(id,path,name,slug,sort_position,created_at,updated_at) VALUES ('p','/p','p','p',0,1,1)`)
	mustExec(t, db, `INSERT INTO threads(id,project_id,title,provider,model,workspace_path,created_at,updated_at) VALUES ('t','p','T','codex','m','/p',1,1)`)
	if err := applyMigration(db, migrationByVersion(t, codexDeveloperInstructionsMigrationVersion)); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM codex_developer_instructions`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rows = %d, %v; want an empty table", rows, err)
	}
	digest := strings.Repeat("a", 64)
	mustExec(t, db, `INSERT INTO codex_developer_instructions(thread_id,session_ref,digest) VALUES ('t','codex-1','`+digest+`')`)
	for _, bad := range []string{
		`INSERT INTO codex_developer_instructions(thread_id,session_ref,digest) VALUES ('t2','','` + digest + `')`,
		`UPDATE codex_developer_instructions SET digest = 'short'`,
	} {
		if _, err := db.Exec(bad); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
	mustExec(t, db, `PRAGMA foreign_keys = ON`)
	mustExec(t, db, `DELETE FROM threads WHERE id = 't'`)
	if err := db.QueryRow(`SELECT COUNT(*) FROM codex_developer_instructions`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rows after the thread's delete = %d, %v; want the cascade", rows, err)
	}
}

func TestCodexDeveloperInstructionsDigestIsPerCodexThread(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThreadWithSessionRef(t, s, "t", "codex-1")
	first, second := strings.Repeat("a", 64), strings.Repeat("b", 64)

	if _, ok, err := s.CodexDeveloperInstructionsDigest("t", "codex-1"); err != nil || ok {
		t.Fatalf("before any write = (%v, %v), want nothing recorded", ok, err)
	}
	if err := s.PutCodexDeveloperInstructionsDigest("t", "codex-1", first); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.CodexDeveloperInstructionsDigest("t", "codex-1"); err != nil || !ok || got != first {
		t.Fatalf("read = (%q, %v, %v), want the recorded digest", got, ok, err)
	}
	// A rollback that forks repoints the thread: the row describes the old
	// Codex thread and answers nothing for the new one.
	if _, ok, err := s.CodexDeveloperInstructionsDigest("t", "codex-2"); err != nil || ok {
		t.Fatalf("read for another Codex thread = (%v, %v), want nothing recorded", ok, err)
	}
	if err := s.PutCodexDeveloperInstructionsDigest("t", "codex-2", second); err != nil {
		t.Fatal(err)
	}
	if got, ok, _ := s.CodexDeveloperInstructionsDigest("t", "codex-2"); !ok || got != second {
		t.Fatalf("read after the new Codex thread's write = (%q, %v), want its digest", got, ok)
	}
	if _, ok, _ := s.CodexDeveloperInstructionsDigest("t", "codex-1"); ok {
		t.Fatal("the old Codex thread's digest survived the new one's write")
	}

	if err := s.DeleteCodexDeveloperInstructionsDigest("t"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.CodexDeveloperInstructionsDigest("t", "codex-2"); ok {
		t.Fatal("the digest survived its delete")
	}
	if err := s.DeleteCodexDeveloperInstructionsDigest("t"); err != nil {
		t.Fatalf("deleting a missing row: %v", err)
	}

	// A thread deleted before the write lands records nothing.
	if err := s.PutCodexDeveloperInstructionsDigest("gone", "codex-3", first); err != nil {
		t.Fatalf("write for a deleted thread: %v", err)
	}
	if _, ok, _ := s.CodexDeveloperInstructionsDigest("gone", "codex-3"); ok {
		t.Fatal("a digest was recorded for a thread that does not exist")
	}
	if err := s.PutCodexDeveloperInstructionsDigest("t", "", first); err == nil {
		t.Fatal("a digest without the Codex thread it describes was accepted")
	}
}
