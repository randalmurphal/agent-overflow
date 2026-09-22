package sessionimport

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/provider"
)

func TestImportedAsyncQuestionKeepsStructureWithoutReopeningPrompt(t *testing.T) {
	s := newTestStore(t)
	thread := seedThread(t, s, testThreadID, "codex", t.TempDir())
	events := []importir.Event{
		{ProviderEvent: provider.ProviderEvent{Kind: provider.EventUserText, ThreadID: testThreadID, Content: "Work", Timestamp: at(0)}, SourceUUID: "user"},
		{ProviderEvent: provider.ProviderEvent{Kind: provider.EventContentBlockStop, ThreadID: testThreadID, ItemID: "ask", Meta: json.RawMessage(`{"delivery":"async","blockType":"text","questions":[{"title":"Which?","options":["A","B"]}]}`), Timestamp: at(1)}, SourceUUID: "question"},
	}
	batch, warnings, err := NewWriter(s, thread).Build(events)
	if err != nil || len(warnings) != 0 || len(batch.Rows) != 2 {
		t.Fatalf("build=%+v %v %v", batch, warnings, err)
	}
	if err := s.ApplyImportBatch(thread.ID, batch); err != nil {
		t.Fatal(err)
	}
	item, found, err := s.GetThreadItem(thread.ID, "question:ask")
	if err != nil || !found || item.Kind != "assistant_text" || item.Summary != "Which?" || item.Status != "completed" {
		t.Fatalf("imported card=%+v %v", item, err)
	}
	if metaKey(t, item.Meta, "delivery") != "async" {
		t.Fatal("lost delivery")
	}
	rows, err := s.ListAsyncQuestions(thread.ID, "")
	if err != nil || len(rows) != 0 {
		t.Fatalf("import resurrected prompt: %+v %v", rows, err)
	}
}
