package triage

import (
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/highlight"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// TestTextDeltaStreamPopulatesMarkdownHTML streams text_delta events that
// together form a fenced code block, then closes the block. The post-
// settle state is the canonical completion point where the frontend
// observes highlighted HTML regardless of streaming throttle decisions.
func TestTextDeltaStreamPopulatesMarkdownHTML(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	deltas := []string{
		"Here is some Go:\n",
		"```go\n",
		"package main\n\n",
		"func main() {}\n",
		"```\n",
		"Done.",
	}
	for _, d := range deltas {
		if err := router.Handle(provider.ProviderEvent{
			Kind:      provider.EventTextDelta,
			ThreadID:  "t1",
			Content:   d,
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("handle text_delta %q: %v", d, err)
		}
	}
	// Close the block so settleStreamingText fires its forced final
	// render. Without this, the render throttle may leave the last few
	// deltas unrendered until the next throttle window elapses — correct
	// for streaming, but the test wants the final painted state.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1",
		Meta: json.RawMessage(`{"blockType":"text"}`), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}

	upserts := filterEmissions(*emissions, "provider:item_upsert")
	if len(upserts) == 0 {
		t.Fatalf("expected item_upsert emissions, got none")
	}
	var final store.Item
	var ok bool
	for i := len(upserts) - 1; i >= 0; i-- {
		item, itemOK := upserts[i].data.(store.Item)
		if !itemOK || item.Kind != "assistant_text" {
			continue
		}
		final = item
		ok = true
		break
	}
	if !ok {
		t.Fatalf("no assistant_text upsert found in %d emissions", len(upserts))
	}
	if final.HighlightedContent == "" {
		t.Fatalf("post-settle assistant_text has empty HighlightedContent (summary=%q)", final.Summary)
	}
	if !strings.Contains(final.HighlightedContent, `class="ch-`) {
		t.Fatalf("final HighlightedContent missing ch- class prefix; got: %q", final.HighlightedContent)
	}
	if !strings.Contains(final.HighlightedContent, "package") {
		t.Fatalf("final HighlightedContent missing 'package' keyword; got: %q", final.HighlightedContent)
	}
}

// TestThinkingStreamPopulatesANSIHTML streams thinking deltas with an
// ANSI color sequence, closes the block, and asserts the post-settle
// upsert carries the rendered ANSI span.
func TestThinkingStreamPopulatesANSIHTML(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	deltas := []string{
		"reasoning: ",
		"\x1b[31mred\x1b[0m",
		" then normal\n",
	}
	for _, d := range deltas {
		if err := router.Handle(provider.ProviderEvent{
			Kind:      provider.EventThinking,
			ThreadID:  "t1",
			Content:   d,
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("handle thinking %q: %v", d, err)
		}
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1",
		Meta: json.RawMessage(`{"blockType":"thinking"}`), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}

	upserts := filterEmissions(*emissions, "provider:item_upsert")
	if len(upserts) == 0 {
		t.Fatalf("expected item_upsert emissions, got none")
	}
	var final store.Item
	var ok bool
	for i := len(upserts) - 1; i >= 0; i-- {
		item, itemOK := upserts[i].data.(store.Item)
		if !itemOK || item.Kind != "thinking" {
			continue
		}
		final = item
		ok = true
		break
	}
	if !ok {
		t.Fatalf("no thinking upsert found")
	}
	if final.HighlightedContent == "" {
		t.Fatalf("post-settle thinking has empty HighlightedContent (summary=%q)", final.Summary)
	}
	if !strings.Contains(final.HighlightedContent, "term-") {
		t.Fatalf("final thinking HighlightedContent missing term- class; got: %q", final.HighlightedContent)
	}
	if !strings.Contains(final.HighlightedContent, "red") {
		t.Fatalf("final thinking HighlightedContent missing 'red' payload; got: %q", final.HighlightedContent)
	}
}

// TestStreamingHighlightThrottleSkipsRapidDeltas pins the throttle
// contract: a burst of deltas fired within streamingHighlightIntervalMs
// of each other produces exactly one intermediate render (the first
// delta's persistItem render on block open). After the settle forces a
// final render, the DB row's HTML reflects the cumulative summary.
func TestStreamingHighlightThrottleSkipsRapidDeltas(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Five deltas fired with timestamps inside a single throttle window
	// (using the same millisecond tick). Only the first-block render
	// should populate HTML; the remaining four must reuse it.
	start := time.Unix(0, 1_000_000_000_000).UTC() // fixed clock
	for i, d := range []string{"# heading\n\n", "first ", "second ", "third ", "fourth"} {
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventTextDelta, ThreadID: "t1",
			Content: d, Timestamp: start.Add(time.Duration(i) * time.Millisecond),
		}); err != nil {
			t.Fatalf("delta %d: %v", i, err)
		}
	}

	// Read the row directly: mid-burst, the HTML should have rendered
	// ONLY at firstBlock (delta 0) and all subsequent deltas fell into
	// the throttle window.
	item := firstItemByKind(t, st, "t1", "assistant_text")
	if item.Summary != "# heading\n\nfirst second third fourth" {
		t.Errorf("Summary not fully appended: %q", item.Summary)
	}
	if !strings.Contains(item.HighlightedContent, "<h1") {
		t.Errorf("expected heading in mid-stream HTML, got: %q", item.HighlightedContent)
	}
	// HTML reflects delta-0 summary ("# heading\n\n") since the remaining
	// deltas were throttled. The next delta outside the throttle window
	// would catch up, and settle always forces a final render.
	if strings.Contains(item.HighlightedContent, "fourth") {
		t.Errorf("mid-stream HTML unexpectedly contains 'fourth' (throttle not applied?): %q", item.HighlightedContent)
	}

	// Close the block: settle clears HighlightedContent and persistItem
	// re-renders against the final cumulative summary.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1",
		Meta: json.RawMessage(`{"blockType":"text"}`), Timestamp: start.Add(10 * time.Millisecond),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}
	item = firstItemByKind(t, st, "t1", "assistant_text")
	if !strings.Contains(item.HighlightedContent, "fourth") {
		t.Errorf("post-settle HTML missing 'fourth': %q", item.HighlightedContent)
	}
}

// TestShouldRenderHighlightRaceOnePerWindow pins the throttle atomicity
// invariant: N goroutines hitting shouldRenderHighlight for the same
// (threadID, itemID) within a single window produce exactly one true.
// Without the mutex-guarded read-modify-write in shouldRenderHighlight
// this test would flake under -race.
func TestShouldRenderHighlightRaceOnePerWindow(t *testing.T) {
	router, _, _ := newTestRouter(t)
	const goroutines = 64
	const now int64 = 1_000_000
	var granted int32
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if router.shouldRenderHighlight("t-race", "text:0:0", now) {
				atomic.AddInt32(&granted, 1)
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&granted); got != 1 {
		t.Fatalf("concurrent shouldRenderHighlight: got %d grants, want exactly 1", got)
	}

	// After the window elapses, a fresh call returns true again.
	if !router.shouldRenderHighlight("t-race", "text:0:0", now+streamingHighlightIntervalMs) {
		t.Fatalf("window expiry did not re-grant")
	}

	// Distinct items are independent: a different item key still gets a
	// fresh grant at the same timestamp.
	if !router.shouldRenderHighlight("t-race", "text:0:1", now) {
		t.Fatalf("distinct itemID did not get its own grant")
	}
}

func TestStreamingItemUpsertThrottleBatchesRapidDeltas(t *testing.T) {
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	var mu sync.Mutex
	var emissions []emitted
	router := NewRouter(st, func(eventName string, data any) {
		mu.Lock()
		defer mu.Unlock()
		emissions = append(emissions, emitted{eventName: eventName, data: data})
	}, highlight.New(highlight.Options{}))
	createTestThread(t, st, "t1")
	countUpserts := func() int {
		mu.Lock()
		defer mu.Unlock()
		return countAssistantTextUpserts(emissions)
	}

	start := time.Now()
	for i, d := range []string{"one ", "two ", "three ", "four ", "five"} {
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventTextDelta, ThreadID: "t1",
			Content: d, Timestamp: start.Add(time.Duration(i) * time.Millisecond),
		}); err != nil {
			t.Fatalf("delta %d: %v", i, err)
		}
	}

	item := firstItemByKind(t, st, "t1", "assistant_text")
	if item.Summary != "one two three four five" {
		t.Fatalf("summary = %q, want full accumulated stream", item.Summary)
	}
	if got := countUpserts(); got != 1 {
		t.Fatalf("assistant_text upserts before settle = %d, want first snapshot only", got)
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for countUpserts() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := countUpserts(); got != 2 {
		t.Fatalf("assistant_text upserts after trailing flush = %d, want first snapshot + trailing", got)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1",
		Meta: json.RawMessage(`{"blockType":"text"}`), Timestamp: start.Add(10 * time.Millisecond),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}
	if got := countUpserts(); got != 3 {
		t.Fatalf("assistant_text upserts after settle = %d, want first snapshot + trailing + final", got)
	}
}

func countAssistantTextUpserts(emissions []emitted) int {
	count := 0
	for _, e := range filterEmissions(emissions, "provider:item_upsert") {
		item, ok := e.data.(store.Item)
		if ok && item.Kind == "assistant_text" {
			count++
		}
	}
	return count
}
