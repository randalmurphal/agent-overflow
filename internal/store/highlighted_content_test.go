package store

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// Migration v19 adds highlighted_content to items and channel_messages.
// These tests pin:
//   - the column exists on both tables after migration
//   - inserts, upserts, and list/get paths round-trip the field
//   - AppendItemSummaryAndHighlight writes summary + html atomically
//   - a nil render callback leaves highlighted_content empty (the caller
//     may legitimately want that for kinds that don't server-render)

func TestMigrationV19AddsHighlightedContentColumns(t *testing.T) {
	s := newTestStore(t)

	for _, table := range []string{"items", "channel_messages"} {
		cols, err := tableColumns(s.db, table)
		if err != nil {
			t.Fatalf("tableColumns(%s): %v", table, err)
		}
		if !cols["highlighted_content"] {
			t.Errorf("%s.highlighted_content column missing (columns=%v)", table, cols)
		}
	}
}

func TestItemHighlightedContentRoundTrip(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t-hc", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	item := Item{
		ID:                 "assist-1",
		ThreadID:           "t-hc",
		TurnIndex:          0,
		ItemIndex:          0,
		Kind:               "assistant_text",
		Role:               "assistant",
		Status:             "completed",
		Summary:            "# heading\n\nhello",
		HighlightedContent: `<h1>heading</h1><p>hello</p>`,
		CreatedAt:          1000,
		UpdatedAt:          1000,
	}
	if err := s.InsertItem(item); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, ok, err := s.GetItem("assist-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !ok {
		t.Fatal("item not found")
	}
	if got.HighlightedContent != item.HighlightedContent {
		t.Errorf("HighlightedContent after InsertItem = %q, want %q", got.HighlightedContent, item.HighlightedContent)
	}

	// UpsertItem on the same id must preserve HighlightedContent updates.
	item.HighlightedContent = `<h1>heading</h1><p>hello world</p>`
	item.Summary = "# heading\n\nhello world"
	item.UpdatedAt = 2000
	if _, err := s.UpsertItem(item, nil); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	list, err := s.ListItems("t-hc")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 item, got %d", len(list))
	}
	if list[0].HighlightedContent != item.HighlightedContent {
		t.Errorf("HighlightedContent after upsert = %q, want %q", list[0].HighlightedContent, item.HighlightedContent)
	}
}

func TestChannelMessageHighlightedContentRoundTrip(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("t-ch", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	now := time.Now().UnixMilli()
	ch := Channel{ID: "c", ThreadID: thread.ID, Type: "deliberation", Status: "open", CreatedAt: now, UpdatedAt: now}
	if err := s.CreateChannel(ch); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	msg := ChannelMessage{
		ID:                 "m-1",
		ChannelID:          ch.ID,
		FromType:           "human",
		FromID:             "user-1",
		Content:            "**bold** message",
		HighlightedContent: `<p><strong>bold</strong> message</p>`,
		CreatedAt:          now,
	}
	seq, err := s.InsertChannelMessageAtomic(msg)
	if err != nil {
		t.Fatalf("InsertChannelMessageAtomic: %v", err)
	}
	if seq != 0 {
		t.Fatalf("first message sequence = %d, want 0", seq)
	}

	got, err := s.ListChannelMessages(ch.ID, -1, 0)
	if err != nil {
		t.Fatalf("ListChannelMessages: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 message, got %d", len(got))
	}
	if got[0].HighlightedContent != msg.HighlightedContent {
		t.Errorf("HighlightedContent after insert = %q, want %q", got[0].HighlightedContent, msg.HighlightedContent)
	}

	// Empty HighlightedContent is a valid state — the frontend treats it
	// as "render content as plain text".
	msg2 := ChannelMessage{
		ID:        "m-2",
		ChannelID: ch.ID,
		FromType:  "agent",
		FromID:    "thread-a",
		Content:   "plain",
		CreatedAt: now + 1,
	}
	if _, err := s.InsertChannelMessageAtomic(msg2); err != nil {
		t.Fatalf("insert m-2: %v", err)
	}
	got, err = s.ListChannelMessages(ch.ID, -1, 0)
	if err != nil {
		t.Fatalf("ListChannelMessages 2: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2, got %d", len(got))
	}
	if got[1].HighlightedContent != "" {
		t.Errorf("empty HighlightedContent not preserved: got %q", got[1].HighlightedContent)
	}
}

// TestAppendItemSummaryAndHighlight pins the hot-path streaming contract:
// each call extends the summary AND rewrites highlighted_content atomically
// from the cumulative summary. The render callback must receive the NEW
// cumulative summary (existing || delta), not the delta alone.
func TestAppendItemSummaryAndHighlight(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t-stream", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	first := Item{
		ID: "stream-1", ThreadID: "t-stream", TurnIndex: 0, ItemIndex: 0,
		Kind: "assistant_text", Role: "assistant",
		Status: "streaming", Summary: "hello ",
		HighlightedContent: "<p>hello</p>",
		CreatedAt:          1000, UpdatedAt: 1000,
	}
	if err := s.InsertItem(first); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Record the cumulative summary the render callback receives so we
	// can assert both the in-SQL concat and the callback's input.
	var seen []string
	render := func(full string) string {
		seen = append(seen, full)
		return "<p>" + full + "</p>"
	}

	got, err := s.AppendItemSummaryAndHighlight("stream-1", "world", render, 2000)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if got.Summary != "hello world" {
		t.Errorf("Summary = %q, want %q", got.Summary, "hello world")
	}
	if got.HighlightedContent != "<p>hello world</p>" {
		t.Errorf("HighlightedContent = %q, want %q", got.HighlightedContent, "<p>hello world</p>")
	}
	if got.UpdatedAt != 2000 {
		t.Errorf("UpdatedAt = %d, want 2000", got.UpdatedAt)
	}
	if got.CreatedAt != 1000 {
		t.Errorf("CreatedAt drifted: got %d, want 1000", got.CreatedAt)
	}
	if len(seen) != 1 || seen[0] != "hello world" {
		t.Errorf("render received %v, want [\"hello world\"]", seen)
	}

	// Second append chains on the new state.
	got2, err := s.AppendItemSummaryAndHighlight("stream-1", "!", render, 3000)
	if err != nil {
		t.Fatalf("append2: %v", err)
	}
	if got2.Summary != "hello world!" {
		t.Errorf("Summary2 = %q, want %q", got2.Summary, "hello world!")
	}
	if got2.HighlightedContent != "<p>hello world!</p>" {
		t.Errorf("HighlightedContent2 = %q, want %q", got2.HighlightedContent, "<p>hello world!</p>")
	}
	if len(seen) != 2 || seen[1] != "hello world!" {
		t.Errorf("render second call received %v", seen)
	}

	// Thread updated_at moves in the same tx.
	thr, err := s.GetThread("t-stream")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if thr.UpdatedAt != 3000 {
		t.Errorf("thread UpdatedAt = %d, want 3000", thr.UpdatedAt)
	}
}

func TestAppendItemSummaryAndHighlightNilRenderClearsHTML(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t-nilrender", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := s.InsertItem(Item{
		ID: "i", ThreadID: "t-nilrender", TurnIndex: 0, ItemIndex: 0,
		Kind: "assistant_text", Role: "assistant",
		Status: "streaming", Summary: "seed ",
		HighlightedContent: "<p>seed</p>",
		CreatedAt:          1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// nil render callback means "no html for this kind" — highlighted_content
	// becomes empty, which the frontend treats as "render as plain text".
	got, err := s.AppendItemSummaryAndHighlight("i", "more", nil, 2)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if got.Summary != "seed more" {
		t.Errorf("Summary = %q, want seed more", got.Summary)
	}
	if got.HighlightedContent != "" {
		t.Errorf("HighlightedContent = %q, want empty", got.HighlightedContent)
	}
}

func TestAppendItemSummaryAndHighlightMissingItem(t *testing.T) {
	s := newTestStore(t)
	_, err := s.AppendItemSummaryAndHighlight("nope", "x", func(string) string { return "" }, 1)
	if err == nil {
		t.Fatal("expected error for missing item")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error should mention the item id, got %v", err)
	}
}

// TestAppendItemSummaryAndHighlightAtomic pins the "summary and html never
// drift" invariant under concurrent appends to distinct items. Each item
// gets N deltas; after all finish, every item's html matches exactly one
// re-render of its final summary.
func TestAppendItemSummaryAndHighlightAtomic(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t-race", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	const numItems = 8
	const deltas = 10
	ids := make([]string, numItems)
	for i := 0; i < numItems; i++ {
		id := "stream-" + strings.Repeat("x", i+1)
		ids[i] = id
		if err := s.InsertItem(Item{
			ID: id, ThreadID: "t-race", TurnIndex: i, ItemIndex: 0,
			Kind: "assistant_text", Role: "assistant",
			Status: "streaming", Summary: "",
			CreatedAt: int64(i + 1), UpdatedAt: int64(i + 1),
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}

	render := func(full string) string { return "<p>" + full + "</p>" }

	var wg sync.WaitGroup
	for i := 0; i < numItems; i++ {
		wg.Add(1)
		go func(id string, base int) {
			defer wg.Done()
			for j := 0; j < deltas; j++ {
				if _, err := s.AppendItemSummaryAndHighlight(id, "a", render, int64(base*1000+j+1)); err != nil {
					t.Errorf("append %s[%d]: %v", id, j, err)
					return
				}
			}
		}(ids[i], i+1)
	}
	wg.Wait()

	for _, id := range ids {
		got, ok, err := s.GetItem(id)
		if err != nil || !ok {
			t.Fatalf("get %s: err=%v ok=%v", id, err, ok)
		}
		if got.Summary != strings.Repeat("a", deltas) {
			t.Errorf("%s Summary = %q, want %d a's", id, got.Summary, deltas)
		}
		wantHTML := "<p>" + strings.Repeat("a", deltas) + "</p>"
		if got.HighlightedContent != wantHTML {
			t.Errorf("%s HighlightedContent = %q, want %q", id, got.HighlightedContent, wantHTML)
		}
	}
}
