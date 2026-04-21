package triage

import (
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// TestHandleTextDeltaDropsAfterSettle pins the triage-layer behavior of
// the ErrItemSettled branch in handleTextDelta: when an interrupt has
// already flipped the streaming row to a terminal status, a subsequent
// text_delta must be dropped entirely — no summary extension, no
// UpsertItem fallback that would resurrect the row back to streaming.
//
// This is the user-facing side of the store-layer guard in bbed245.
// Without the ErrItemSettled-aware drop, handleTextDelta would fall
// through to the sql.ErrNoRows recovery path and UpsertItem would
// re-create the row (or resurrect it) with status='streaming' and a
// summary that contains only the late delta — overwriting the
// interrupt's "— stopped" suffix.
func TestHandleTextDeltaDropsAfterSettle(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	_ = openTestTurn(t, router, "t1")

	// Delta 1: firstBlock — creates a streaming row.
	mustHandle(t, router, textDeltaEvent("t1", "hello ", time.Now()))
	pre := firstItemByKind(t, st, "t1", "assistant_text")
	if pre.Status != "streaming" {
		t.Fatalf("after delta 1, expected streaming, got %q", pre.Status)
	}

	// Delta 2: extends the streaming row.
	mustHandle(t, router, textDeltaEvent("t1", "world", time.Now()))
	mid := firstItemByKind(t, st, "t1", "assistant_text")
	if mid.Summary != "hello world" {
		t.Fatalf("delta 2 extension failed: %q", mid.Summary)
	}

	// Simulate interrupt: flip the row to errored with a suffix (mirrors
	// what flipTurnItemsErrored does, without running the full handler).
	if err := st.UpdateItemStatus(pre.ID, "errored", "hello world — stopped", "", time.Now().UnixMilli()); err != nil {
		t.Fatalf("simulate interrupt: %v", err)
	}

	// Delta 3: arrives AFTER the settle. Must not extend summary, must
	// not resurrect the row to streaming.
	mustHandle(t, router, textDeltaEvent("t1", "STALE_DELTA", time.Now()))

	post := firstItemByKind(t, st, "t1", "assistant_text")
	if post.Status != "errored" {
		t.Errorf("late delta resurrected row: status=%q, want errored", post.Status)
	}
	if strings.Contains(post.Summary, "STALE_DELTA") {
		t.Errorf("late delta extended settled summary: %q", post.Summary)
	}
	if post.Summary != "hello world — stopped" {
		t.Errorf("settled summary got mutated: %q", post.Summary)
	}

	// Exactly one row — no UpsertItem-fallback resurrection.
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	textCount := 0
	for _, it := range items {
		if it.Kind == "assistant_text" {
			textCount++
		}
	}
	if textCount != 1 {
		t.Errorf("expected 1 assistant_text row, got %d", textCount)
	}
}

// TestHandleThinkingDropsAfterSettle mirrors the text-delta guard for
// the thinking delta path. Thinking has the additional concern that the
// payload data would ALSO be extended by AppendPayloadData if the
// ErrItemSettled drop weren't wired — so a late thinking delta under
// the old path could have extended both summary AND the payload blob.
func TestHandleThinkingDropsAfterSettle(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	_ = openTestTurn(t, router, "t1")

	// Thinking delta 1.
	mustHandle(t, router, thinkingDeltaEvent("t1", "pondering ", time.Now()))
	pre := firstItemByKind(t, st, "t1", "thinking")
	if pre.Status != "streaming" {
		t.Fatalf("after thinking delta 1: status=%q", pre.Status)
	}

	// Thinking delta 2.
	mustHandle(t, router, thinkingDeltaEvent("t1", "quietly", time.Now()))

	// Simulate interrupt.
	if err := st.UpdateItemStatus(pre.ID, "errored", "pondering quietly — stopped", pre.PayloadID, time.Now().UnixMilli()); err != nil {
		t.Fatalf("simulate interrupt: %v", err)
	}

	// Delta 3 post-settle.
	mustHandle(t, router, thinkingDeltaEvent("t1", "LATE", time.Now()))

	post := firstItemByKind(t, st, "t1", "thinking")
	if post.Status != "errored" {
		t.Errorf("thinking row resurrected: status=%q", post.Status)
	}
	if strings.Contains(post.Summary, "LATE") {
		t.Errorf("late thinking delta extended settled summary: %q", post.Summary)
	}
}

// TestAppendItemSummaryInterruptRaceUnderLoad drives the actual
// concurrent race that motivated the status-guarded writes in bbed245.
// One goroutine streams deltas (AppendItemSummary + UpdateItemHighlight)
// while another flips the row to errored mid-stream. Under -race the
// invariant is: final highlighted_content must NEVER contain
// pre-suffix HTML when Summary contains the suffix. Either both sides
// are pre-suffix (delta won the race, interrupt hadn't flipped yet) or
// both are post-suffix (interrupt won, delta skipped). The combination
// of "post-suffix summary + pre-suffix HTML" is the bug.
func TestAppendItemSummaryInterruptRaceUnderLoad(t *testing.T) {
	_, s, _ := newTestRouter(t)
	createTestThread(t, s, "t-race")

	const iters = 50
	var wins, losses int32

	for i := 0; i < iters; i++ {
		id := "stream-" + time.Now().Format("150405.000000000") + "-" + string(rune('a'+(i%26)))
		if err := s.InsertItem(store.Item{
			ID: id, ThreadID: "t-race", TurnIndex: i, ItemIndex: 0,
			Kind: "assistant_text", Role: "assistant",
			Status: "streaming", Summary: "",
			CreatedAt: int64(i + 1), UpdatedAt: int64(i + 1),
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}

		var wg sync.WaitGroup
		wg.Add(2)

		// Goroutine A: stream five deltas.
		go func(id string) {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				row, err := s.AppendItemSummary(id, "x", int64(j+10))
				if err != nil {
					// ErrItemSettled is a legitimate outcome under race.
					return
				}
				html := "<p>" + row.Summary + "</p>"
				_ = s.UpdateItemHighlight(id, html)
			}
		}(id)

		// Goroutine B: after a tiny yield, flip status.
		go func(id string) {
			defer wg.Done()
			time.Sleep(time.Microsecond)
			_ = s.UpdateItemStatus(id, "errored", "SETTLED", "", 999)
		}(id)

		wg.Wait()

		got, _, _ := s.GetItem(id)
		// Invariant 1: if summary is SETTLED, HTML must be empty or match
		// SETTLED. Pre-suffix HTML ("<p>x...") next to post-suffix summary
		// is the bug the guard prevents.
		if got.Summary == "SETTLED" {
			if got.HighlightedContent != "" && !strings.Contains(got.HighlightedContent, "SETTLED") {
				t.Errorf("iter %d: stale HTML next to settled summary: HTML=%q Summary=%q", i, got.HighlightedContent, got.Summary)
				atomic.AddInt32(&losses, 1)
				continue
			}
		}
		atomic.AddInt32(&wins, 1)
	}

	if losses > 0 {
		t.Fatalf("race produced %d/%d stale-HTML violations", losses, iters)
	}
	t.Logf("race test clean: %d iterations, 0 stale-HTML violations", wins)
}

// --- test helpers ---

func textDeltaEvent(threadID, content string, ts time.Time) provider.ProviderEvent {
	return provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  threadID,
		Content:   content,
		Timestamp: ts,
	}
}

func thinkingDeltaEvent(threadID, content string, ts time.Time) provider.ProviderEvent {
	return provider.ProviderEvent{
		Kind:      provider.EventThinking,
		ThreadID:  threadID,
		Content:   content,
		Timestamp: ts,
	}
}

func openTestTurn(t *testing.T, r *Router, threadID string) int {
	t.Helper()
	turnEvt := provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  threadID,
		TurnID:    "turn-" + threadID,
		Meta:      json.RawMessage(`{}`),
		Timestamp: time.Now(),
	}
	if err := r.Handle(turnEvt); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	return 0
}

func mustHandle(t *testing.T, r *Router, evt provider.ProviderEvent) {
	t.Helper()
	if err := r.Handle(evt); err != nil {
		t.Fatalf("handle %s: %v", evt.Kind, err)
	}
}

