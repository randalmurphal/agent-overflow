package triage

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

type observedText struct {
	threadID string
	itemID   string
	parentID string
	text     string
	end      bool
}

// textObserver records both observers in call order.
type textObserver struct {
	mu   sync.Mutex
	seen []observedText
}

func observeText(router *Router) *textObserver {
	o := &textObserver{}
	router.SetAssistantTextObservers(func(threadID, itemID, parentID, delta string) {
		o.mu.Lock()
		o.seen = append(o.seen, observedText{threadID, itemID, parentID, delta, false})
		o.mu.Unlock()
	}, func(threadID, itemID, text string) {
		o.mu.Lock()
		o.seen = append(o.seen, observedText{threadID, itemID, "", text, true})
		o.mu.Unlock()
	})
	return o
}

func (o *textObserver) snapshot() []observedText {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]observedText(nil), o.seen...)
}

// TestAssistantTextObservers pins the contract live highlighting builds on:
// every delta the row emits is observed in order as it is emitted, whether
// or not a persistence flush follows, and the settle ends the row exactly
// once, last, with its final model text.
func TestAssistantTextObservers(t *testing.T) {
	router, st, _ := newTestRouter(t)
	observer := observeText(router)
	createTestThread(t, st, "t1")

	deltas := []string{"```python\ndef f():\n", "    pass  # " + strings.Repeat("x", streamPersistByteThreshold), "\n```"}
	for i, delta := range deltas {
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventTextDelta, ThreadID: "t1", Content: delta, Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("delta %d: %v", i, err)
		}
		seen := observer.snapshot()
		if len(seen) != i+1 || seen[i].end || seen[i].text != delta || seen[i].threadID != "t1" || seen[i].itemID == "" {
			t.Fatalf("after delta %d observed %#v", i, seen)
		}
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1",
		Meta: json.RawMessage(`{"blockType":"text"}`), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}
	router.WaitForPendingSettles()

	seen := observer.snapshot()
	ends := 0
	for _, o := range seen {
		if o.end {
			ends++
		}
		if o.itemID != seen[0].itemID {
			t.Fatalf("observations for another row: %#v", seen)
		}
	}
	last := seen[len(seen)-1]
	if ends != 1 || !last.end || last.text != strings.Join(deltas, "") {
		t.Fatalf("want one final end with the full text, got %#v", seen)
	}
}

// TestAssistantTextIsObservedBeforeItIsEmitted: the observer sees each delta
// before the row's delta event is emitted, so live highlighting's first push
// for a fence reaches clients ahead of the fence's text.
func TestAssistantTextIsObservedBeforeItIsEmitted(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	var early []string
	router.SetAssistantTextObservers(func(threadID, itemID, parentID, delta string) {
		for _, e := range emissions.snapshot() {
			if evt, ok := e.data.(ItemStreamEvent); ok && evt.Action == itemStreamActionDelta && evt.Delta == delta {
				early = append(early, delta)
			}
		}
	}, func(threadID, itemID, text string) {})
	for _, delta := range []string{"```go\n", "x := 1\n"} {
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventTextDelta, ThreadID: "t1", Content: delta, Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("delta: %v", err)
		}
	}
	if len(early) > 0 {
		t.Fatalf("deltas emitted before they were observed: %q", early)
	}
	deltas := 0
	for _, e := range emissions.snapshot() {
		if evt, ok := e.data.(ItemStreamEvent); ok && evt.Action == itemStreamActionDelta {
			deltas++
		}
	}
	if deltas != 2 {
		t.Fatalf("emitted %d delta events, want 2", deltas)
	}
}

// TestAssistantTextObserverCarriesTheRowsScope: a subagent's text row is
// observed with its parentId, so live highlighting addresses its pushes to
// that transcript scope.
func TestAssistantTextObserverCarriesTheRowsScope(t *testing.T) {
	router, st, _ := newTestRouter(t)
	observer := observeText(router)
	createTestThread(t, st, "t1")
	for _, delta := range []string{"```go\n", "x := 1\n"} {
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventTextDelta, ThreadID: "t1", ParentToolUseID: "toolu_agent",
			Content: delta, Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("delta: %v", err)
		}
	}
	seen := observer.snapshot()
	if len(seen) != 2 {
		t.Fatalf("observed %#v, want both deltas", seen)
	}
	for _, o := range seen {
		if o.parentID != "toolu_agent" {
			t.Fatalf("observed %#v, want parentId toolu_agent on every delta", seen)
		}
	}
}

// TestAssistantTextStreamEndsOnErrorFlip streams a text row past the flush
// threshold, then ends its turn by a user Stop and by a fatal truncation:
// the flip ends the row with the text before the suffix, and the settle
// that follows ends it again with no text.
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
			observer := observeText(router)
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
			before := observer.snapshot()
			if len(before) != 2 || before[0].end || before[1].end {
				t.Fatalf("want the two deltas before the end, got %#v", before)
			}
			itemID := before[0].itemID

			if err := end.run(router); err != nil {
				t.Fatalf("end turn: %v", err)
			}
			if err := router.settleTurnStreaming("t1", router.OpenTurnIndex("t1"), statusErrored, nil); err != nil {
				t.Fatalf("settle: %v", err)
			}
			router.WaitForPendingSettles()

			seen := observer.snapshot()
			var ends []observedText
			for _, o := range seen {
				if o.itemID == itemID && o.end {
					ends = append(ends, o)
				}
			}
			if len(ends) == 0 || ends[0].text != first+padding {
				t.Fatalf("flip did not end the stream with the model text: %#v", ends)
			}
			for _, o := range ends[1:] {
				if o.text != "" {
					t.Fatalf("a later end carried text again: %#v", o)
				}
			}
			if last := seen[len(seen)-1]; last.itemID == itemID && !last.end {
				t.Fatalf("the row's last observation is not its end: %#v", last)
			}
		})
	}
}

// TestSettleWithoutStreamingRowEndsTheStream settles a row the store no
// longer holds: the settle still ends the row's observed stream, with no
// text.
func TestSettleWithoutStreamingRowEndsTheStream(t *testing.T) {
	router, st, _ := newTestRouter(t)
	observer := observeText(router)
	createTestThread(t, st, "t1")
	if err := router.settleStreamingTextRow("t1", "gone", statusCompleted, interruptedSummary, "", false, nil); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if seen := observer.snapshot(); len(seen) != 1 || seen[0] != (observedText{"t1", "gone", "", "", true}) {
		t.Fatalf("observations = %#v, want one empty end", seen)
	}
}

// TestRecoveredTextBlockIsObservedAsAStream persists a never-streamed
// top-level block, which the wire reveals as one streamed delta: the
// observers see that delta and the row's end, so the block's code is
// highlighted like any streamed text.
func TestRecoveredTextBlockIsObservedAsAStream(t *testing.T) {
	router, st, _ := newTestRouter(t)
	observer := observeText(router)
	createTestThread(t, st, "t1")
	content := "```go\nx := 1\n```"
	if err := router.persistCompletedTextItem("t1", 0, "", "recovered", content, nil, time.Now()); err != nil {
		t.Fatalf("persist recovered block: %v", err)
	}
	seen := observer.snapshot()
	if len(seen) != 2 || seen[0].end || seen[0].text != content || !seen[1].end || seen[1].text != content || seen[0].itemID != seen[1].itemID {
		t.Fatalf("observations = %#v, want the delta then the end", seen)
	}
}
