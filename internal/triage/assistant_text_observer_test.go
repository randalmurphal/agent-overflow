package triage

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

type observedTextTick struct {
	threadID string
	itemID   string
	text     string
	final    bool
}

// TestAssistantTextStreamObserver pins the observer contract the
// highlight seed push builds on: a flush-window tick carries the
// row's FULL accumulated summary (not the delta), and settle delivers
// exactly one final tick with the row's final model text.
func TestAssistantTextStreamObserver(t *testing.T) {
	router, st, _ := newTestRouter(t)
	var mu sync.Mutex
	var ticks []observedTextTick
	router.SetAssistantTextStreamObserver(func(threadID, itemID, text string, final bool) {
		mu.Lock()
		ticks = append(ticks, observedTextTick{threadID, itemID, text, final})
		mu.Unlock()
	})
	ensureTriageProject(t, st)
	now := time.Now().UnixMilli()
	if err := st.CreateThread(store.Thread{
		ID:        "t1",
		ProjectID: triageTestProjectID,
		Title:     "text-observer",
		Provider:  "claude",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Delta 1 creates the row directly (firstBlock bypasses the flush
	// buffer) — no tick yet. Delta 2 trips the byte threshold and
	// flushes inline: the observer must see the COMBINED summary.
	first := "```python\ndef f():\n"
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", Content: first, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("delta 1: %v", err)
	}
	mu.Lock()
	if len(ticks) != 0 {
		t.Fatalf("expected no observer tick from the firstBlock path, got %#v", ticks)
	}
	mu.Unlock()

	padding := "    pass  # " + strings.Repeat("x", streamPersistByteThreshold)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", Content: padding, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("delta 2: %v", err)
	}
	mu.Lock()
	if len(ticks) != 1 {
		t.Fatalf("expected 1 flush tick, got %#v", ticks)
	}
	flush := ticks[0]
	mu.Unlock()
	if flush.final {
		t.Fatalf("flush tick marked final: %#v", flush)
	}
	if flush.threadID != "t1" || flush.itemID == "" {
		t.Fatalf("flush tick identity wrong: %#v", flush)
	}
	if flush.text != first+padding {
		t.Fatalf("flush tick must carry the full accumulated summary; got %q", flush.text)
	}

	// Settle with a trailing sub-threshold delta still buffered: the
	// settle's own flush ticks first (full text), then exactly one
	// final tick with the same final content.
	tail := "\n```"
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", Content: tail, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("delta 3: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1",
		Meta: json.RawMessage(`{"blockType":"text"}`), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}
	router.WaitForPendingSettles()

	mu.Lock()
	defer mu.Unlock()
	finalTicks := 0
	var last observedTextTick
	for _, tick := range ticks {
		if tick.final {
			finalTicks++
			last = tick
		}
	}
	if finalTicks != 1 {
		t.Fatalf("expected exactly 1 final tick, got %d: %#v", finalTicks, ticks)
	}
	if last.text != first+padding+tail {
		t.Fatalf("final tick text = %q, want the full final content", last.text)
	}
	if last != ticks[len(ticks)-1] {
		t.Fatalf("final tick must be the last observation: %#v", ticks)
	}
}

// TestAssistantTextStreamEndsOnErrorFlip streams a text row past the flush
// threshold, then ends its turn by a user Stop and by a fatal truncation:
// the flip delivers the observer's final tick with the text before the
// suffix, and the settle that follows adds no seedable text.
func TestAssistantTextStreamEndsOnErrorFlip(t *testing.T) {
	for _, end := range []struct {
		name string
		run  func(router *Router) error
	}{
		{"user stop", func(router *Router) error {
			_, err := markUserInterruptForTest(router, "t1")
			return err
		}},
		{"fatal", func(router *Router) error {
			return router.markTurnItemsErrored("t1", router.OpenTurnIndex("t1"), time.Now().UnixMilli())
		}},
	} {
		t.Run(end.name, func(t *testing.T) {
			router, st, _ := newTestRouter(t)
			var mu sync.Mutex
			var ticks []observedTextTick
			router.SetAssistantTextStreamObserver(func(threadID, itemID, text string, final bool) {
				mu.Lock()
				ticks = append(ticks, observedTextTick{threadID, itemID, text, final})
				mu.Unlock()
			})
			createTestThread(t, st, "t1")
			seedOpenTurn(t, router, st, "t1", 0)
			first := "```go\nx := 1\n"
			padding := "// " + strings.Repeat("x", streamPersistByteThreshold)
			for _, delta := range []string{first, padding} {
				if err := router.Handle(provider.ProviderEvent{
					Kind: provider.EventTextDelta, ThreadID: "t1", Content: delta, Timestamp: time.Now(),
				}); err != nil {
					t.Fatalf("delta: %v", err)
				}
			}
			mu.Lock()
			if len(ticks) != 1 || ticks[0].final {
				t.Fatalf("want one flush tick before the end, got %#v", ticks)
			}
			itemID := ticks[0].itemID
			mu.Unlock()

			if err := end.run(router); err != nil {
				t.Fatalf("end turn: %v", err)
			}
			if err := router.settleTurnStreaming("t1", router.OpenTurnIndex("t1"), statusErrored, nil); err != nil {
				t.Fatalf("settle: %v", err)
			}
			router.WaitForPendingSettles()

			mu.Lock()
			defer mu.Unlock()
			var finals []observedTextTick
			for _, tick := range ticks {
				if tick.itemID == itemID && tick.final {
					finals = append(finals, tick)
				}
			}
			if len(finals) == 0 || finals[0].text != first+padding {
				t.Fatalf("flip did not end the stream with the model text: %#v", finals)
			}
			for _, tick := range finals[1:] {
				if tick.text != "" {
					t.Fatalf("a later final tick carried text to seed again: %#v", tick)
				}
			}
			if last := ticks[len(ticks)-1]; last.itemID == itemID && !last.final {
				t.Fatalf("the row's last observation is not final: %#v", last)
			}
		})
	}
}

// TestSettleWithoutStreamingRowEndsTheStream settles a row the store no
// longer holds: the settle still ends the row's observer stream, with no
// text to seed.
func TestSettleWithoutStreamingRowEndsTheStream(t *testing.T) {
	router, st, _ := newTestRouter(t)
	var ticks []observedTextTick
	router.SetAssistantTextStreamObserver(func(threadID, itemID, text string, final bool) {
		ticks = append(ticks, observedTextTick{threadID, itemID, text, final})
	})
	createTestThread(t, st, "t1")
	if err := router.settleStreamingTextRow("t1", "gone", statusCompleted, interruptedSummary, "", false, nil); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if len(ticks) != 1 || ticks[0] != (observedTextTick{"t1", "gone", "", true}) {
		t.Fatalf("observations = %#v, want one empty final tick", ticks)
	}
}
