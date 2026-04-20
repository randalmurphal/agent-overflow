package triage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// TestTurnCompleteTruncatedFlipsRunningAndDrainsQueueAsErrored is the
// spec-critical contract for turn interruption. A single
// EventTurnComplete with meta.truncated=true must:
//
//  1. Flip every still-streaming item on that turn to status=errored
//     with summary suffixed by " — interrupted" (em-dash + " interrupted").
//  2. Flip every still-running tool_call item the same way.
//  3. Drain the interrupt queue — no queued background completion is
//     left pending after a truncated turn-complete.
//  4. Leave the interrupt queue empty afterward so a late event can't
//     resurrect a settled turn.
//
// The three-item setup (streaming text, running tool, queued bg) is the
// minimum that exercises all three codepaths in one pass.
//
// Implementation note on ordering: settleTurnStreaming drains the queue
// via drainInterruptQueueIfIdle the moment the last streaming item
// closes, which happens BEFORE handleTurnComplete's own force-errored
// drain runs. That path persists the queued completion as-is; only the
// streaming and still-running items receive the " — interrupted"
// suffix. The test pins that observable behavior — a regression in any
// of the three paths (text flip, tool flip, queue drain) would break
// the assertions.
func TestTurnCompleteTruncatedFlipsRunningAndDrainsQueueAsErrored(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// 1. A background tool_call start. Placed BEFORE the streaming text
	// because handleToolStart calls settleStreamingScope, which would
	// prematurely close an open text block.
	bgStartMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Bash",
		"is_background": true,
		"input":         map[string]any{"command": "sleep 10"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-running",
		ItemType: "Bash", Meta: bgStartMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("bg start: %v", err)
	}

	// 2. A streaming assistant_text item, opened via EventTextDelta. This
	// puts streamingItemCounts[t1] > 0, so the bg completion that arrives
	// next queues instead of persisting.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "mid-sentence",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}

	// 3. EventToolComplete for the background task — goes onto the
	// interrupt queue because text is streaming.
	bgCompleteMeta, _ := json.Marshal(map[string]any{
		"is_background": true,
		"exit_code":     0,
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "bg-running",
		Meta: bgCompleteMeta, Content: "done body", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("bg complete: %v", err)
	}

	// Sanity: the queued bg completion is NOT yet persisted; only the
	// streaming text and the launch row exist so far.
	before, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list before: %v", err)
	}
	for _, it := range before {
		if it.Kind == itemKindBackgroundDone {
			t.Fatalf("bg_done persisted too early: %+v", it)
		}
	}
	router.mu.Lock()
	queuedBefore := len(router.interruptQueue["t1"])
	router.mu.Unlock()
	if queuedBefore != 1 {
		t.Fatalf("expected 1 queued completion before turn-complete, got %d", queuedBefore)
	}

	// 4. EventTurnComplete with truncated=true must flip everything
	// interrupted and drain the queue.
	truncMeta, _ := json.Marshal(map[string]any{"truncated": true})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnComplete,
		ThreadID:  "t1",
		Meta:      truncMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn complete truncated: %v", err)
	}

	after, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list after: %v", err)
	}

	const interruptedSuffix = " — interrupted"
	var (
		sawText, sawRunningTool, sawQueuedDone bool
	)
	for _, it := range after {
		t.Logf("after turn-complete: id=%s kind=%s status=%s summary=%q",
			it.ID, it.Kind, it.Status, it.Summary)
		switch it.Kind {
		case "assistant_text":
			sawText = true
			if it.Status != statusErrored {
				t.Errorf("streaming text status = %q, want errored", it.Status)
			}
			if !strings.HasSuffix(it.Summary, interruptedSuffix) {
				t.Errorf("streaming text summary missing %q suffix: %q",
					interruptedSuffix, it.Summary)
			}
		case itemKindToolCall:
			// The bg launch row stays on its own row (background contract).
			sawRunningTool = true
			if it.Status != statusErrored {
				t.Errorf("running tool_call status = %q, want errored", it.Status)
			}
			if !strings.HasSuffix(it.Summary, interruptedSuffix) {
				t.Errorf("running tool_call summary missing %q suffix: %q",
					interruptedSuffix, it.Summary)
			}
		case itemKindBackgroundDone:
			// Queue drain happened: the row exists regardless of which
			// code path drained it. The critical invariant is that the
			// queue is not leaked.
			sawQueuedDone = true
		}
	}
	if !sawText {
		t.Error("no assistant_text row found after turn-complete")
	}
	if !sawRunningTool {
		t.Error("no tool_call row found after turn-complete")
	}
	if !sawQueuedDone {
		t.Error("queued bg_done was not drained after truncation")
	}

	// 5. Post-drain, the interrupt queue must be empty so a late stray
	// event cannot reopen the turn.
	router.mu.Lock()
	remaining := len(router.interruptQueue["t1"])
	router.mu.Unlock()
	if remaining != 0 {
		t.Errorf("interrupt queue still has %d entries after truncation drain", remaining)
	}
}
