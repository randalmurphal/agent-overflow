package store

import (
	"testing"
	"time"
)

func TestThreadDraftGetMissingReturnsFalse(t *testing.T) {
	s := newTestStore(t)

	got, ok, err := s.GetThreadDraft("nonexistent")
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for missing draft")
	}
	// The empty fallback should use empty JSON arrays so the frontend can
	// parse without special-casing null.
	if got.ThreadID != "nonexistent" {
		t.Fatalf("threadID: got %q", got.ThreadID)
	}
	if got.Attachments != "[]" || got.TerminalChips != "[]" {
		t.Fatalf("expected empty arrays for missing draft, got %+v", got)
	}
}

func TestThreadDraftInsertAndReadBack(t *testing.T) {
	s := newTestStore(t)

	thread := makeThread("draft-thread", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	draft := ThreadDraft{
		ThreadID:      thread.ID,
		Content:       "hello @file.ts",
		Attachments:   `["att-1","att-2"]`,
		TerminalChips: `[{"id":"chip-1","preview":"ls -la"}]`,
		UpdatedAt:     time.Now().UnixMilli(),
	}
	if err := s.UpsertThreadDraft(draft); err != nil {
		t.Fatalf("UpsertThreadDraft: %v", err)
	}

	got, ok, err := s.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	if !ok {
		t.Fatal("expected draft to exist")
	}
	if got != draft {
		t.Fatalf("draft mismatch: got %+v want %+v", got, draft)
	}
}

func TestThreadDraftUpsertReplaces(t *testing.T) {
	s := newTestStore(t)

	thread := makeThread("upsert-thread", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	first := ThreadDraft{
		ThreadID:      thread.ID,
		Content:       "v1",
		Attachments:   "[]",
		TerminalChips: "[]",
		UpdatedAt:     1000,
	}
	if err := s.UpsertThreadDraft(first); err != nil {
		t.Fatalf("insert v1: %v", err)
	}
	second := ThreadDraft{
		ThreadID:      thread.ID,
		Content:       "v2",
		Attachments:   `["att-9"]`,
		TerminalChips: "[]",
		UpdatedAt:     2000,
	}
	if err := s.UpsertThreadDraft(second); err != nil {
		t.Fatalf("upsert v2: %v", err)
	}

	got, _, err := s.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	if got != second {
		t.Fatalf("expected upserted draft, got %+v", got)
	}
}

func TestThreadDraftDeleteIsIdempotent(t *testing.T) {
	s := newTestStore(t)

	thread := makeThread("delete-draft-thread", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	draft := ThreadDraft{
		ThreadID:      thread.ID,
		Content:       "to be deleted",
		Attachments:   "[]",
		TerminalChips: "[]",
		UpdatedAt:     time.Now().UnixMilli(),
	}
	if err := s.UpsertThreadDraft(draft); err != nil {
		t.Fatalf("UpsertThreadDraft: %v", err)
	}
	if err := s.DeleteThreadDraft(thread.ID); err != nil {
		t.Fatalf("DeleteThreadDraft: %v", err)
	}
	// Second delete must be a no-op, not an error.
	if err := s.DeleteThreadDraft(thread.ID); err != nil {
		t.Fatalf("second DeleteThreadDraft: %v", err)
	}

	_, ok, err := s.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft after delete: %v", err)
	}
	if ok {
		t.Fatal("expected draft to be gone")
	}
}

func TestThreadDraftCascadesOnThreadDelete(t *testing.T) {
	s := newTestStore(t)

	thread := makeThread("cascade-draft-thread", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	draft := ThreadDraft{
		ThreadID:      thread.ID,
		Content:       "should cascade",
		Attachments:   "[]",
		TerminalChips: "[]",
		UpdatedAt:     time.Now().UnixMilli(),
	}
	if err := s.UpsertThreadDraft(draft); err != nil {
		t.Fatalf("UpsertThreadDraft: %v", err)
	}

	if err := s.DeleteThread(thread.ID); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}

	_, ok, err := s.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	if ok {
		t.Fatal("expected draft to cascade on thread delete")
	}
}

func TestThreadDraftRequiresThreadID(t *testing.T) {
	s := newTestStore(t)
	err := s.UpsertThreadDraft(ThreadDraft{UpdatedAt: 1})
	if err == nil {
		t.Fatal("expected error for empty thread id")
	}
}

func TestThreadDraftNormalisesEmptyJSONFields(t *testing.T) {
	s := newTestStore(t)

	thread := makeThread("json-fill-thread", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	err := s.UpsertThreadDraft(ThreadDraft{
		ThreadID:  thread.ID,
		Content:   "blank arrays",
		UpdatedAt: 1,
	})
	if err != nil {
		t.Fatalf("UpsertThreadDraft: %v", err)
	}
	got, _, err := s.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	if got.Attachments != "[]" || got.TerminalChips != "[]" {
		t.Fatalf("expected normalised JSON arrays, got %+v", got)
	}
}
