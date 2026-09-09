package store

import "testing"

func TestDraftRecoveryIsIndependentAndAtomic(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("recovery", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	r := ThreadDraftRecovery{ThreadID: thread.ID, SendID: "send", Content: "replacement", Attachments: `["edited-attachment"]`}
	if err := s.StageThreadDraftRecovery(r); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.GetThreadDraft(thread.ID); err != nil || found {
		t.Fatalf("staging changed composer: found=%v err=%v", found, err)
	}
	if err := s.StageThreadDraftRecovery(r); err == nil {
		t.Fatal("must not overwrite outstanding recovery")
	}
	before, _, err := s.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	typed := ThreadDraft{ThreadID: thread.ID, Content: "new work", Attachments: "[]", TerminalChips: "[]"}
	if _, err := s.UpsertThreadDraft(typed); err != nil {
		t.Fatal(err)
	}
	merged := typed
	merged.Content = "replacement\n\nnew work"
	if ok, _, err := s.CommitThreadDraftRecovery(r, before, merged); err != nil || ok {
		t.Fatalf("overwrote racing draft: %v %v", ok, err)
	}
	current, _, err := s.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := s.CommitThreadDraftRecovery(r, current, merged); err != nil || !ok {
		t.Fatalf("recover: %v %v", ok, err)
	}
	rows, err := s.ListThreadDraftRecoveries()
	if err != nil || len(rows) != 0 {
		t.Fatalf("recovery remains: %+v %v", rows, err)
	}
	actual, _, err := s.GetThreadDraft(thread.ID)
	if err != nil || actual.Content != merged.Content {
		t.Fatalf("draft: %+v %v", actual, err)
	}
	// Repeated recovery cannot duplicate the prefix.
	if ok, _, err := s.CommitThreadDraftRecovery(r, actual, merged); err != nil || !ok {
		t.Fatalf("repeat: %v %v", ok, err)
	}
}

func TestDraftRecoveryCascadesWithItsThread(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("recovery", "codex")
	if err := s.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	if err := s.StageThreadDraftRecovery(ThreadDraftRecovery{ThreadID: thread.ID, SendID: "s", Content: "edit", Attachments: "[]"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteThread(thread.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListThreadDraftRecoveries()
	if err != nil || len(rows) != 0 {
		t.Fatalf("orphan recovery: %+v %v", rows, err)
	}
}

func TestMigrationV94PreservesDraftsAndProtectsRecovery(t *testing.T) {
	db := migrateThrough(t, 93)
	mustExec(t, db, `INSERT INTO threads(id, title, provider, workspace_path, created_at, updated_at) VALUES('recovery', 'Recovery', 'claude', '/tmp', 1, 1)`)
	mustExec(t, db, `INSERT INTO thread_drafts(thread_id, content, attachments, terminal_chips, updated_at, has_content) VALUES('recovery', 'existing draft', '[]', '[]', 1, 1)`)
	if err := applyMigration(db, migrationByVersion(t, 94)); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO thread_draft_recoveries(thread_id,send_id,content,attachments) VALUES('recovery','send','edit','[]')`)
	var content string
	if err := db.QueryRow(`SELECT content FROM thread_drafts WHERE thread_id='recovery'`).Scan(&content); err != nil || content != "existing draft" {
		t.Fatalf("migration changed draft: %q %v", content, err)
	}
	mustExec(t, db, `DELETE FROM threads WHERE id='recovery'`)
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM thread_draft_recoveries`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("cascade: %d %v", count, err)
	}
}

func TestRecoveryPreventsEmptyDraftCleanup(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("recovery", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	if err := s.StageThreadDraftRecovery(ThreadDraftRecovery{ThreadID: thread.ID, SendID: "s", Content: "edit", Attachments: "[]"}); err != nil {
		t.Fatal(err)
	}
	if empty, err := s.IsEmptyDraftThread(thread.ID); err != nil || empty {
		t.Fatalf("recovery considered empty: %v %v", empty, err)
	}
	if ids, err := s.ThreadIDsOlderThan(1 << 62); err != nil || len(ids) != 0 {
		t.Fatalf("retention includes recovery: %v %v", ids, err)
	}
	if deleted, err := s.DeleteEmptyDraftThread(thread.ID); err != nil || deleted {
		t.Fatalf("recovery deleted: %v %v", deleted, err)
	}
}

func TestDraftRecoveryRejectsCrossThreadCommit(t *testing.T) {
	s := newTestStore(t)
	r := ThreadDraftRecovery{ThreadID: "a", SendID: "send"}
	if _, _, err := s.CommitThreadDraftRecovery(r, ThreadDraft{ThreadID: "a"}, ThreadDraft{ThreadID: "b"}); err == nil {
		t.Fatal("cross-thread recovery accepted")
	}
}
