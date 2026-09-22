package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

func TestAsyncQuestionStartsStructuredAndSurvivesTurnEnd(t *testing.T) {
	r, s, _ := newTestRouter(t)
	createTestThread(t, s, "q")
	seedOpenTurn(t, r, s, "q", 0)
	event := provider.ProviderEvent{Kind: provider.EventContentBlockStart, ThreadID: "q", ItemID: "call", Meta: json.RawMessage(`{"delivery":"async","blockType":"text","questions":[{"title":"Which?","options":["A","B"]}]}`), Timestamp: time.Now()}
	if err := r.Handle(event); err != nil {
		t.Fatal(err)
	}
	item, found, err := s.GetThreadItem("q", "question:call")
	if err != nil || !found || item.Kind != "assistant_text" || item.Status != "completed" {
		t.Fatalf("start row: %+v %v", item, err)
	}
	answer := []store.AsyncQuestionAnswer{{ItemID: item.ID, Index: 0, Answer: "B"}}
	if err := s.QueueAsyncAnswers(store.FlushQueueItem{ID: "queued", ThreadID: "q", SendID: "send", Message: "Question: Which?\nAnswer: B"}, answer); err != nil {
		t.Fatal(err)
	}
	event.Kind = provider.EventContentBlockStop
	event.Content = "prose"
	event.ContentPresent = true
	if err := r.Handle(event); err != nil {
		t.Fatal(err)
	}
	if err := r.Handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "q", TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"}, Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	r.WaitForPendingSettles()
	rows, err := s.ListAsyncQuestions("q", "")
	if err != nil || len(rows) != 1 || rows[0].State != "submitted" {
		t.Fatalf("lost questions: %+v %v", rows, err)
	}
	items, err := s.ListItems("q")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, row := range items {
		if row.Kind == "assistant_text" {
			count++
			if row.ID != item.ID || row.ItemIndex != item.ItemIndex {
				t.Fatalf("identity changed: %+v", row)
			}
		}
	}
	if count != 1 {
		t.Fatalf("prose duplicate: %d", count)
	}
}

func TestUnansweredAsyncQuestionSurvivesInterrupt(t *testing.T) {
	r, s, _ := newTestRouter(t)
	createTestThread(t, s, "q")
	seedOpenTurn(t, r, s, "q", 0)
	if err := r.Handle(provider.ProviderEvent{Kind: provider.EventContentBlockStart, ThreadID: "q", ItemID: "ask", Meta: json.RawMessage(`{"delivery":"async","questions":[{"title":"Which?"}]}`), Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := r.Handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "q", TurnComplete: &provider.WireTurnCompleteMeta{Aborted: true, StopReason: "interrupted"}, Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	r.WaitForPendingSettles()
	rows, err := s.ListAsyncQuestions("q", "")
	if err != nil || len(rows) != 1 || rows[0].State != "unanswered" {
		t.Fatalf("interrupt canceled async question: %+v %v", rows, err)
	}
}
