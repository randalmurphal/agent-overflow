package store

import (
	"context"
	"testing"
)

// TestStreamEndIsTheStreamedByteLength: every item read reports a
// streaming text row's stream end as the byte length of its streamed
// payload, base blob and append chunks both, and reports none for a
// settled row or another kind.
func TestStreamEndIsTheStreamedByteLength(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(Thread{
		ID: "t", ProjectID: defaultTestProjectID, Title: "T", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: 1000, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	seed := func(id, kind, payloadKind, text string) {
		t.Helper()
		if err := seedPayloadRow(s, "t", Payload{ID: "pay-" + id, Kind: payloadKind, Meta: "{}", Data: []byte(text), CreatedAt: 2000}); err != nil {
			t.Fatalf("insert payload %s: %v", id, err)
		}
		if err := insertCarded(s, Item{
			ID: id, ThreadID: "t", TurnIndex: 0, ItemIndex: len(id),
			Kind: kind, Role: "assistant", Status: "streaming",
			Summary: text, PayloadID: "pay-" + id,
			CreatedAt: 2000, UpdatedAt: 2000,
		}); err != nil {
			t.Fatalf("insert item %s: %v", id, err)
		}
	}
	seed("text", "assistant_text", "assistant_text", "héllo")
	seed("think", "thinking", "thinking", "hmm")
	seed("tool-run", "tool_call", "command_output", "output")
	if _, err := s.AppendItemSummaryAndPayloadData("t", "text", " wörld", "pay-text", []byte(" wörld"), 3000); err != nil {
		t.Fatalf("append text: %v", err)
	}
	for _, chunk := range []string{" 🙂", " ok"} {
		if _, err := s.AppendItemSummaryTailAndPayloadData("t", "think", chunk, 400, "pay-think", []byte(chunk), 3000); err != nil {
			t.Fatalf("append thinking: %v", err)
		}
	}
	want := map[string]int64{
		"text":     int64(len("héllo wörld")),
		"think":    int64(len("hmm 🙂 ok")),
		"tool-run": -1,
	}
	check := func(read string, items []Item) {
		t.Helper()
		seen := 0
		for _, item := range items {
			end, ok := want[item.ID]
			if !ok {
				continue
			}
			seen++
			switch {
			case end < 0 && item.StreamEnd != nil:
				t.Errorf("%s: %s stream end = %d, want none", read, item.ID, *item.StreamEnd)
			case end >= 0 && (item.StreamEnd == nil || *item.StreamEnd != end):
				t.Errorf("%s: %s stream end = %v, want %d", read, item.ID, item.StreamEnd, end)
			}
		}
		if seen != len(want) {
			t.Fatalf("%s returned %d of the %d rows", read, seen, len(want))
		}
	}

	listed, err := s.ListItems("t")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	check("ListItems", listed)
	wire, err := s.ListWireItems("t", []string{"text", "think", "tool-run"})
	if err != nil {
		t.Fatalf("list wire items: %v", err)
	}
	check("ListWireItems", wire)
	var one []Item
	for id := range want {
		item, found, err := s.GetThreadItem("t", id)
		if err != nil || !found {
			t.Fatalf("get %s: found=%v err=%v", id, found, err)
		}
		one = append(one, item)
	}
	check("GetThreadItem", one)
	page, err := s.ListThreadSliceAround(context.Background(), "t", "", 50, 50, TimelineSelection{})
	if err != nil {
		t.Fatalf("slice around: %v", err)
	}
	check("ListThreadSliceAround", page.Items)
	synced, err := s.SyncThreadWindow(context.Background(), "t", "", 50, 50, HistoryStamp{}, nil, TimelineSelection{})
	if err != nil || synced.Page == nil {
		t.Fatalf("sync window: page=%v err=%v", synced.Page, err)
	}
	check("SyncThreadWindow", synced.Page.Items)

	// A replaced payload drops its chunks: the stream ends at the new blob.
	if err := s.ReplacePayloadData("t", "pay-text", []byte("fïnal"), "{}", 4000); err != nil {
		t.Fatalf("replace payload: %v", err)
	}
	want["text"] = int64(len("fïnal"))
	// A settled row no longer streams.
	completed := "completed"
	if _, err := s.UpdateItemFields("t", "think", ItemPartialUpdate{Status: &completed}); err != nil {
		t.Fatalf("settle thinking: %v", err)
	}
	want["think"] = -1
	listed, err = s.ListItems("t")
	if err != nil {
		t.Fatalf("list items after settle: %v", err)
	}
	check("ListItems after replace and settle", listed)
}
