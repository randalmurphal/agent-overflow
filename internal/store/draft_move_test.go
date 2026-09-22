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
