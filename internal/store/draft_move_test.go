package store

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/transferwire"
)

func TestDraftTransferKeepsSourceFencedAndConsumesOnlyAtCompletion(t *testing.T) {
	s := newTestStore(t)
	source := createDraftCleanupThread(t, s, "source", "plan")
	draft := ThreadDraft{ThreadID: source.ID, Content: "portable draft", Attachments: "[]", TerminalChips: "[]"}
	if _, err := s.UpsertThreadDraft(draft); err != nil {
		t.Fatal(err)
	}
	request := transferRequest(source.ID, "copy", "outgoing")
	private, err := json.Marshal(struct {
		Draft *ThreadDraft `json:"draftToConsume"`
	}{&draft})
	if err != nil {
		t.Fatal(err)
	}
	request.PrivateState = private
	row, err := s.CreateThreadTransfer(request)
	if err != nil {
		t.Fatal(err)
	}
	archive := transferwire.Upload{SHA256: strings.Repeat("a", 64), Size: 1024}
	if _, err := s.BindThreadTransferArchive(row.ID, archive); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckThreadTransferAccess(source.ID); err == nil {
		t.Fatal("sealing the draft released its source")
	}
	if _, err := s.CreateThreadTransfer(transferRequest(source.ID, "copy", "outgoing")); err == nil {
		t.Fatal("second transfer bypassed draft fence")
	}
	target := createDraftCleanupThread(t, s, "other", "chat")
	if err := s.MoveThreadDraft(draft, ThreadDraft{ThreadID: target.ID, Content: draft.Content, Attachments: "[]", TerminalChips: "[]"}); err == nil {
		t.Fatal("local move bypassed the transfer fence")
	}
	for _, phase := range []string{"prepared", "committed"} {
		if _, err := s.AdvanceThreadTransfer(row.ID, phase, archive.SHA256); err != nil {
			t.Fatal(err)
		}
		if got, _, err := s.GetThreadDraft(source.ID); err != nil || got.Content != draft.Content {
			t.Fatalf("source consumed before activation: %+v %v", got, err)
		}
	}
	if _, err := s.AdvanceThreadTransfer(row.ID, "complete", archive.SHA256); err != nil {
		t.Fatal(err)
	}
	if got, _, err := s.GetThreadDraft(source.ID); err != nil || got.Content != "" {
		t.Fatalf("source not consumed: %+v %v", got, err)
	}
	if err := s.CheckThreadTransferAccess(source.ID); err != nil {
		t.Fatal("empty source remains fenced:", err)
	}
	// A completion retry must never consume new work entered afterwards.
	draft.Content = "a later draft"
	if _, err := s.UpsertThreadDraft(draft); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceThreadTransfer(row.ID, "complete", archive.SHA256); err != nil {
		t.Fatal(err)
	}
	if got, _, err := s.GetThreadDraft(source.ID); err != nil || got.Content != draft.Content {
		t.Fatalf("repeat completion erased new work: %+v %v", got, err)
	}
}

func TestMoveThreadDraftRollsBackWhenDestinationAlreadyHasContent(t *testing.T) {
	s := newTestStore(t)
	a := createDraftCleanupThread(t, s, "source", "chat")
	b := createDraftCleanupThread(t, s, "destination", "chat")
	source := ThreadDraft{ThreadID: a.ID, Content: "source text", Attachments: "[]", TerminalChips: "[]"}
	target := ThreadDraft{ThreadID: b.ID, Content: "destination text", Attachments: "[]", TerminalChips: "[]"}
	for _, d := range []ThreadDraft{source, target} {
		if _, err := s.UpsertThreadDraft(d); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MoveThreadDraft(source, ThreadDraft{ThreadID: b.ID, Content: source.Content, Attachments: "[]", TerminalChips: "[]"}); err == nil {
		t.Fatal("overwrote destination")
	}
	for _, want := range []ThreadDraft{source, target} {
		got, _, err := s.GetThreadDraft(want.ThreadID)
		if err != nil || got != want {
			t.Fatalf("draft changed: %+v %v", got, err)
		}
	}
}

func TestMoveThreadDraftCommitsDurablyBeforeSourceFilesCanBeRemoved(t *testing.T) {
	s := newTestStore(t)
	a := createDraftCleanupThread(t, s, "source", "chat")
	b := createDraftCleanupThread(t, s, "destination", "chat")
	draft := ThreadDraft{ThreadID: a.ID, Content: "survive power loss", Attachments: "[]", TerminalChips: "[]"}
	if _, err := s.UpsertThreadDraft(draft); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER require_durable_draft_move BEFORE DELETE ON thread_drafts BEGIN
		SELECT CASE WHEN (SELECT synchronous FROM pragma_synchronous) <> 3
		THEN RAISE(ABORT, 'draft move is not durable') END; END`); err != nil {
		t.Fatal(err)
	}
	target := draft
	target.ThreadID = b.ID
	if err := s.MoveThreadDraft(draft, target); err != nil {
		t.Fatal(err)
	}
}

func TestTransferStatusDoesNotDependOnRecentListOrExposePrivateState(t *testing.T) {
	s := newTestStore(t)
	first, err := s.CreateThreadTransfer(transferRequest("old", "copy", "outgoing"))
	if err != nil {
		t.Fatal(err)
	}
	for range 101 {
		request := transferRequest("placeholder", "copy", "incoming")
		request.ThreadID = request.ID
		if _, err := s.CreateThreadTransfer(request); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`UPDATE thread_transfers SET updated_at = 1 WHERE id = ?`, first.ID); err != nil {
		t.Fatal(err)
	}
	recent, err := s.ListRecentThreadTransfers()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range recent {
		if row.ID == first.ID {
			t.Fatal("fixture did not push old operation out")
		}
	}
	got, err := s.GetThreadTransferStatus(first.ID)
	if err != nil || got.ID != first.ID || got.ThreadID != first.ThreadID || !got.NeedsDestination {
		t.Fatalf("status: %+v %v", got, err)
	}
	if len(got.PrivateState) != 0 || len(got.PeerState) != 0 || got.ActivationHash != "" {
		t.Fatalf("private status data: %+v", got)
	}
}

// A handoff's draft move writes the next thread's draft and clears the
// previous thread's together, and refuses, changing neither, when the
// previous draft is not the one the move read.
func TestHandOffThreadDraftMovesWholeOrNotAtAll(t *testing.T) {
	s := newTestStore(t)
	a := createDraftCleanupThread(t, s, "previous", "chat")
	b := createDraftCleanupThread(t, s, "next", "chat")
	read := ThreadDraft{ThreadID: a.ID, Content: "typed note"}
	if _, err := s.UpsertThreadDraft(ThreadDraft{ThreadID: a.ID, Content: "typed note, then more"}); err != nil {
		t.Fatal(err)
	}
	moved := ThreadDraft{ThreadID: b.ID, Content: "typed note"}
	if err := s.HandOffThreadDraft(a.ID, &read, moved); err == nil {
		t.Fatal("moved a draft that changed after the move read it")
	}
	if got, _, _ := s.GetThreadDraft(a.ID); got.Content != "typed note, then more" {
		t.Fatalf("previous draft = %q, want it untouched", got.Content)
	}
	if _, found, _ := s.GetThreadDraft(b.ID); found {
		t.Fatal("the refused move wrote the next thread's draft")
	}

	if _, err := s.UpsertThreadDraft(read); err != nil {
		t.Fatal(err)
	}
	if err := s.HandOffThreadDraft(a.ID, &read, moved); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.GetThreadDraft(a.ID); found {
		t.Fatal("previous draft kept")
	}
	if got, _, _ := s.GetThreadDraft(b.ID); got.Content != "typed note" {
		t.Fatalf("next draft = %q", got.Content)
	}
	if err := s.HandOffThreadDraft(a.ID, nil, ThreadDraft{ThreadID: b.ID, Content: "carried"}); err != nil {
		t.Fatalf("a move with no previous draft: %v", err)
	}
}

// The handoff records on the past thread's row the text and attachment ids
// it moved, and nothing when it moved no draft.
func TestHandOffThreadDraftRecordsWhatItMoved(t *testing.T) {
	s := newTestStore(t)
	past := createDraftCleanupThread(t, s, "past", "chat")
	next := createDraftCleanupThread(t, s, "next", "chat")
	other := createDraftCleanupThread(t, s, "other", "chat")
	for _, q := range []string{
		`INSERT INTO agents (id, thread_id, home, created_at) VALUES ('agent', 'next', '/h', 1)`,
		`INSERT INTO agent_past_threads (thread_id, agent_id, superseded_at) VALUES ('past', 'agent', 2)`,
		`INSERT INTO agent_past_threads (thread_id, agent_id, superseded_at) VALUES ('other', 'agent', 1)`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	draft := ThreadDraft{ThreadID: past.ID, Content: "typed note", Attachments: `["att-1"]`, TerminalChips: `[{"id":"c"}]`}
	if _, err := s.UpsertThreadDraft(draft); err != nil {
		t.Fatal(err)
	}
	if err := s.HandOffThreadDraft(past.ID, &draft, ThreadDraft{ThreadID: next.ID, Content: "typed note"}); err != nil {
		t.Fatal(err)
	}
	if moved, found, err := s.MovedDraftOf(past.ID); err != nil || !found || moved.Content != "typed note" || moved.Attachments != `["att-1"]` {
		t.Fatalf("moved draft = %+v %v %v", moved, found, err)
	}
	if moved, found, err := s.MovedDraftOf(other.ID); err != nil || !found || moved.Content != "" || moved.Attachments != "[]" {
		t.Fatalf("a past thread nothing moved from = %+v %v %v", moved, found, err)
	}
	if _, found, err := s.MovedDraftOf(next.ID); err != nil || found {
		t.Fatalf("current thread found as past: %v %v", found, err)
	}
}

func TestSameDraftComparesAsTheConsumingDeleteDoes(t *testing.T) {
	stored := ThreadDraft{ThreadID: "t", Content: "x", Attachments: "[]", TerminalChips: "[]", UpdatedAt: 5}
	if !SameDraft(stored, ThreadDraft{Content: "x"}) {
		t.Fatal("empty lists and timestamps differ")
	}
	if SameDraft(stored, ThreadDraft{Content: "x", PendingPlanImplementation: `{"itemId":"i"}`}) || SameDraft(stored, ThreadDraft{Content: "y"}) {
		t.Fatal("different drafts compared equal")
	}
}
