package store

import (
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// Migration v19 adds highlighted_content to items and channel_messages.
// These tests pin:
//   - the column exists on both tables after migration
//   - inserts, upserts, and list/get paths round-trip the field
//   - UpdateItemHighlight rewrites the column as a single-statement write
//     (render runs outside the SQLite writer lock; see items.go)
//   - concurrent two-phase writes (AppendItemSummary + UpdateItemHighlight)
//     converge on matching summary / html for every item

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

// TestUpdateItemHighlight pins the single-statement HTML write used by
// the throttled streaming path. AppendItemSummary extends summary in
// phase 1; the caller renders outside any open TX; UpdateItemHighlight
// flushes the result. The column must take the exact string written
// with no inter-column side effects.
func TestUpdateItemHighlight(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t-u", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := s.InsertItem(Item{
		ID: "u-1", ThreadID: "t-u", TurnIndex: 0, ItemIndex: 0,
		Kind: "assistant_text", Role: "assistant",
		Status: "streaming", Summary: "hello",
		CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if err := s.UpdateItemHighlight("u-1", "<p>hello</p>"); err != nil {
		t.Fatalf("update highlight: %v", err)
	}
	got, ok, err := s.GetItem("u-1")
	if err != nil || !ok {
		t.Fatalf("get: err=%v ok=%v", err, ok)
	}
	if got.HighlightedContent != "<p>hello</p>" {
		t.Errorf("HighlightedContent = %q, want <p>hello</p>", got.HighlightedContent)
	}
	if got.Summary != "hello" {
		t.Errorf("Summary should be untouched, got %q", got.Summary)
	}
	if got.UpdatedAt != 1 {
		t.Errorf("UpdatedAt should be untouched, got %d", got.UpdatedAt)
	}

	// Empty html is a legitimate write — it triggers the plain-text
	// fallback in the frontend.
	if err := s.UpdateItemHighlight("u-1", ""); err != nil {
		t.Fatalf("update empty: %v", err)
	}
	got, _, _ = s.GetItem("u-1")
	if got.HighlightedContent != "" {
		t.Errorf("HighlightedContent should clear, got %q", got.HighlightedContent)
	}
}

func TestUpdateItemHighlightMissingItem(t *testing.T) {
	s := newTestStore(t)
	err := s.UpdateItemHighlight("nope", "<p>x</p>")
	if err == nil {
		t.Fatal("expected error for missing item")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error should mention the item id, got %v", err)
	}
}

// TestUpdateItemHighlightSkipsSettledRow pins the status-guarded write:
// once a row transitions out of 'streaming' (via interrupt or settle on
// a parallel goroutine), a late streaming render must NOT overwrite the
// terminal HTML the settle wrote. The UPDATE's WHERE clause includes
// status = 'streaming', so the late write is a silent no-op instead of
// a stale-HTML regression. This is the store-level guard that fixes
// the delta/interrupt race in the triage layer.
func TestUpdateItemHighlightSkipsSettledRow(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t-race2", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := s.InsertItem(Item{
		ID: "race-1", ThreadID: "t-race2", TurnIndex: 0, ItemIndex: 0,
		Kind: "assistant_text", Role: "assistant",
		Status: "errored", Summary: "hello — stopped",
		HighlightedContent: "<p>hello — stopped</p>",
		CreatedAt:          1, UpdatedAt: 2,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Late delta's render tries to flush stale "hello" HTML; the row is
	// already settled (status='errored'), so the write is a no-op.
	if err := s.UpdateItemHighlight("race-1", "<p>hello</p>"); err != nil {
		t.Fatalf("update highlight on settled row should silently skip: %v", err)
	}
	got, _, _ := s.GetItem("race-1")
	if got.HighlightedContent != "<p>hello — stopped</p>" {
		t.Errorf("settled HTML got clobbered: HighlightedContent = %q", got.HighlightedContent)
	}
}

// TestAppendItemSummarySkipsSettledRow mirrors
// TestUpdateItemHighlightSkipsSettledRow for the summary column: once a
// row is no longer streaming, a late delta's AppendItemSummary must not
// extend the terminal summary. Returns ErrItemSettled so the triage
// caller can distinguish "drop the delta" from "row missing" and avoid
// the UpsertItem fallback that would resurrect a settled row.
func TestAppendItemSummarySkipsSettledRow(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t-race3", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := s.InsertItem(Item{
		ID: "race-2", ThreadID: "t-race3", TurnIndex: 0, ItemIndex: 0,
		Kind: "assistant_text", Role: "assistant",
		Status: "completed", Summary: "final",
		CreatedAt: 1, UpdatedAt: 2,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	_, err := s.AppendItemSummary("race-2", "stale delta", 3)
	if !errors.Is(err, ErrItemSettled) {
		t.Fatalf("expected ErrItemSettled for settled row, got %v", err)
	}
	got, _, _ := s.GetItem("race-2")
	if got.Summary != "final" {
		t.Errorf("settled summary got extended: Summary = %q", got.Summary)
	}

	// Genuinely missing row still returns sql.ErrNoRows so the caller's
	// UpsertItem fallback path stays untouched.
	if _, err := s.AppendItemSummary("not-here", "x", 1); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("missing row should return sql.ErrNoRows, got %v", err)
	}
}

// TestTwoPhaseStreamingWriteAtomic: the streaming path now runs
// AppendItemSummary (commits summary) → render outside TX →
// UpdateItemHighlight (commits html). Concurrent deltas across
// distinct items must converge: each item ends with summary and html
// that match a single rendering of the final summary. This catches
// cross-item state bleed that would have existed if UpdateItemHighlight
// accidentally updated the wrong row.
func TestTwoPhaseStreamingWriteAtomic(t *testing.T) {
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
				row, err := s.AppendItemSummary(id, "a", int64(base*1000+j+1))
				if err != nil {
					t.Errorf("append %s[%d]: %v", id, j, err)
					return
				}
				html := render(row.Summary)
				if err := s.UpdateItemHighlight(id, html); err != nil {
					t.Errorf("update highlight %s[%d]: %v", id, j, err)
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
