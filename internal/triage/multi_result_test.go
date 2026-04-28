package triage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// The tests in this file pin the architectural fix for the
// multi-result-per-turn class of data-loss bugs.
//
// Background: Claude Code emits two `result` envelopes within one
// logical agent-overflow turn when its CLI synthesizes a `type:"user"`
// envelope from a background-task notification — the assistant's first
// `end_turn` lands as a `result`, the synthesized user prompt provokes
// another model call, and the second response lands as a second
// `result`. Both belong to the same agent-overflow turn from the user's
// perspective (one user-typed prompt, one cascade of work).
//
// The original bug: clearOpenTurn (called from handleTurnComplete) was
// prefix-sweeping id-allocating counters (segmentIndexByScope,
// blockIndexByScope, errorSeqByScope, terminalInteractionSeq) on every
// turn-complete. After the first close, the surviving second-half
// events found the counters wiped, restarted from zero, and computed
// ids that collided with rows already persisted under this same turn.
// UpsertItem's INSERT-OR-UPDATE silently overwrote the prior content
// while preserving item_index — so the persisted row ended up with the
// LATER text/thinking/error/waited content but the EARLIER row's
// position. Verified in real user data.
//
// The fix: counters are id-allocators with item-row lifetime, not
// per-turn flow-control. They survive turn boundaries (cleared only at
// CleanupThread); the LastTurnIndex fallback in currentTurnIndex
// re-attaches post-clearOpenTurn events to the same turn so the
// surviving counter advances correctly. handleTurnComplete is now
// idempotent — a second turn-complete on an already-closed turn
// returns early.
//
// Each test below exercises one variant of the bug shape and asserts
// the architectural fix preserves distinct rows where the original
// would silently overwrite.

// TestMultipleResultsPerTurn_TextSegmentsDoNotCollide is the canonical
// repro: stream three text segments before the first turn-complete,
// then stream a fourth after. Without the fix, the fourth segment
// computed `text:0:1` and overwrote segment 1. With the fix, the
// counter survives and the fourth lands at `text:0:3`.
func TestMultipleResultsPerTurn_TextSegmentsDoNotCollide(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	// Three text segments split by content_block_stop boundaries (we
	// drive that via settleStreamingScope so each delta opens a new
	// segment).
	for i, content := range []string{
		"I'll start the two background commands first.",
		"Both background tasks are running. Now the inline command:",
		"Inline command completed in 5s. The two background tasks are still running.",
	} {
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventTextDelta, ThreadID: "t1",
			Content: content, Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("text delta %d: %v", i, err)
		}
		if err := router.settleStreamingScope("t1", ""); err != nil {
			t.Fatalf("close text segment %d: %v", i, err)
		}
	}

	// First turn-complete — the wire-level "first end_turn".
	// In the multi-result wire pattern, NO EventTurnStart re-fires
	// between the two results: agent-overflow only fires EventTurnStart
	// once per user-typed send (synthesized in app_send.go), not on
	// Claude's system.init. So clearOpenTurn fires here, the surviving
	// segment counter is what protects subsequent rows from collision.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("first turn-complete: %v", err)
	}

	// The fourth text segment — the post-Reads final summary. This
	// arrives WITHOUT a fresh EventTurnStart, hitting the path where
	// currentTurnIndex falls back to LastTurnIndex (the turn row is
	// already persisted at index 0 from the first complete). With the
	// architectural fix, segmentIndexByScope is still alive at value 2,
	// so the new segment gets text:0:3. Without the fix, the wipe in
	// clearOpenTurn would have left the counter at zero-value 0, and
	// the new segment would have collided with text:0:1.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1",
		Content:   "Both background tasks finished. Final summary.",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("fourth text delta: %v", err)
	}
	if err := router.settleStreamingScope("t1", ""); err != nil {
		t.Fatalf("close fourth segment: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("second turn-complete: %v", err)
	}

	items, err := st.ListTurnItems("t1", 0)
	if err != nil {
		t.Fatalf("list turn items: %v", err)
	}
	textItems := make(map[string]string)
	for _, it := range items {
		if it.Kind != itemKindAssistantText {
			continue
		}
		textItems[it.ID] = it.Summary
	}
	if len(textItems) != 4 {
		t.Fatalf("expected 4 distinct assistant_text rows, got %d: %+v", len(textItems), textItems)
	}
	for _, want := range []string{"text:0:0", "text:0:1", "text:0:2", "text:0:3"} {
		if _, ok := textItems[want]; !ok {
			t.Errorf("missing row %q in %+v", want, textItems)
		}
	}
	// Block-2 content (which the original bug overwrote) must survive.
	if !strings.Contains(textItems["text:0:1"], "Both background tasks are running") {
		t.Errorf("text:0:1 = %q, want to contain block-2 content (original bug overwrote with block-4)", textItems["text:0:1"])
	}
	if !strings.Contains(textItems["text:0:3"], "Both background tasks finished") {
		t.Errorf("text:0:3 = %q, want to contain block-4 content", textItems["text:0:3"])
	}
}

// TestMultipleResultsPerTurn_ThinkingBlocksDoNotCollide pins the same
// fix for blockIndexByScope / thinking rows.
func TestMultipleResultsPerTurn_ThinkingBlocksDoNotCollide(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	for i, content := range []string{"thought one", "thought two"} {
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventThinking, ThreadID: "t1",
			Content: content, Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("thinking %d: %v", i, err)
		}
		if err := router.settleStreamingScope("t1", ""); err != nil {
			t.Fatalf("close thinking %d: %v", i, err)
		}
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("first turn-complete: %v", err)
	}

	// Third thinking arrives after the first complete with no fresh
	// EventTurnStart — same wire pattern as the text-segment case.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventThinking, ThreadID: "t1",
		Content: "thought three (post-clear)", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("third thinking: %v", err)
	}
	if err := router.settleStreamingScope("t1", ""); err != nil {
		t.Fatalf("close third thinking: %v", err)
	}

	items, err := st.ListTurnItems("t1", 0)
	if err != nil {
		t.Fatalf("list turn items: %v", err)
	}
	thinkingIDs := make(map[string]struct{})
	for _, it := range items {
		if it.Kind == itemKindThinking {
			thinkingIDs[it.ID] = struct{}{}
		}
	}
	if len(thinkingIDs) != 3 {
		t.Fatalf("expected 3 distinct thinking rows, got %d: %+v", len(thinkingIDs), thinkingIDs)
	}
	for _, want := range []string{"think:0:0", "think:0:1", "think:0:2"} {
		if _, ok := thinkingIDs[want]; !ok {
			t.Errorf("missing thinking row %q in %+v", want, thinkingIDs)
		}
	}
}

// TestMultipleResultsPerTurn_ErrorRowsDoNotCollide pins the fix for
// errorSeqByScope. Two errors before the first turn-close, one after —
// without the fix, the post-close error landed at error:0:0 and
// overwrote the first one.
func TestMultipleResultsPerTurn_ErrorRowsDoNotCollide(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	for i, msg := range []string{"first error", "second error"} {
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventError, ThreadID: "t1",
			Content: msg, Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("error %d: %v", i, err)
		}
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("first turn-complete: %v", err)
	}

	// Third error arrives post-clear with no fresh EventTurnStart —
	// errorSeqByScope must survive so the new error gets seq=2.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventError, ThreadID: "t1",
		Content: "third error (post-clear)", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("third error: %v", err)
	}

	items, err := st.ListTurnItems("t1", 0)
	if err != nil {
		t.Fatalf("list turn items: %v", err)
	}
	errors := make(map[string]string)
	for _, it := range items {
		if it.Kind == "error" {
			errors[it.ID] = it.Summary
		}
	}
	if len(errors) != 3 {
		t.Fatalf("expected 3 distinct error rows, got %d: %+v", len(errors), errors)
	}
	for _, want := range []string{"error:0:0", "error:0:1", "error:0:2"} {
		if _, ok := errors[want]; !ok {
			t.Errorf("missing error row %q in %+v", want, errors)
		}
	}
	if errors["error:0:0"] != "first error" {
		t.Errorf("error:0:0 = %q, want %q (third error overwrote first?)", errors["error:0:0"], "first error")
	}
}

// TestSyntheticTruncatedTurnComplete_ThenRealResult_NoDuplicateEmission
// pins the second known trigger of doubled-clearOpenTurn: a fatal
// EventError synthesizes a truncated turn-complete (handleError →
// synthesizeTruncatedTurnComplete), then a real wire EventTurnComplete
// arrives anyway because the subprocess kept streaming. Without the
// idempotent guard, the real complete re-runs the full handler and
// emits a duplicate provider:turn_completed; with it, the second call
// returns early.
func TestSyntheticTruncatedTurnComplete_ThenRealResult_NoDuplicateEmission(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	// A fatal error WITHOUT expect_turn_complete=true: handleError
	// synthesizes a truncated turn-complete which calls handleTurnComplete
	// and ends up running clearOpenTurn.
	fatalMeta, _ := json.Marshal(map[string]any{"fatal": true})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventError, ThreadID: "t1",
		Content: "subprocess died", Meta: fatalMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("fatal error: %v", err)
	}

	// Despite the synthetic complete, a real wire complete arrives.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("real turn-complete: %v", err)
	}

	completed := filterEmissions(*emissions, "provider:turn_completed")
	if len(completed) != 1 {
		t.Errorf("expected exactly 1 provider:turn_completed emission, got %d (idempotent guard regression)", len(completed))
	}
}

// TestMarkUserInterrupt_ThenTurnCompleteThenLateText pins the
// user-Esc + late-text race: the user hits Esc mid-stream
// (MarkUserInterrupt does NOT clear the open turn), the wire's real
// EventTurnComplete arrives next (clearOpenTurn fires), and a late
// text delta arrives AFTER the turn-close. Without the architectural
// fix the counter is wiped at clearOpenTurn and the late delta would
// collide with a row already persisted earlier in the same turn.
//
// Stream two text segments before Esc so the counter advances to 1,
// fire EventTurnComplete to trigger clearOpenTurn (the wipe point),
// then fire a late delta. Without the fix the late delta computes
// text:0:1 (counter wiped to 0, +1) and overwrites the second pre-Esc
// row. With the fix the counter survives at 1, the late delta lands
// at text:0:2.
func TestMarkUserInterrupt_ThenTurnCompleteThenLateText(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	for _, content := range []string{"pre-Esc segment 0", "pre-Esc segment 1"} {
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventTextDelta, ThreadID: "t1",
			Content: content, Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("pre-Esc delta %q: %v", content, err)
		}
		if err := router.settleStreamingScope("t1", ""); err != nil {
			t.Fatalf("close pre-Esc segment: %v", err)
		}
	}

	if _, err := router.MarkUserInterrupt("t1"); err != nil {
		t.Fatalf("user interrupt: %v", err)
	}

	// EventTurnComplete fires clearOpenTurn — this is the wipe point
	// that, on master, would wipe the segment counter and set up the
	// collision for the next text delta.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn-complete: %v", err)
	}

	// Late text delta after clearOpenTurn — the wire continued emitting
	// while Esc was being processed. Under the architectural fix the
	// segment counter is preserved across clearOpenTurn, so this lands
	// at text:0:2; on master it would land at text:0:1 (counter wiped)
	// and overwrite the second pre-Esc row.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1",
		Content: "post-clear text (late)", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("post-clear delta: %v", err)
	}
	if err := router.settleStreamingScope("t1", ""); err != nil {
		t.Fatalf("close post-clear segment: %v", err)
	}

	items, err := st.ListTurnItems("t1", 0)
	if err != nil {
		t.Fatalf("list turn items: %v", err)
	}
	textIDs := make(map[string]string)
	for _, it := range items {
		if it.Kind == itemKindAssistantText {
			textIDs[it.ID] = it.Summary
		}
	}
	if len(textIDs) != 3 {
		t.Fatalf("expected 3 distinct text rows (2 pre-Esc + 1 late), got %d: %+v", len(textIDs), textIDs)
	}
	if !strings.Contains(textIDs["text:0:0"], "segment 0") {
		t.Errorf("text:0:0 = %q, want pre-Esc segment 0", textIDs["text:0:0"])
	}
	if !strings.Contains(textIDs["text:0:1"], "segment 1") {
		t.Errorf("text:0:1 = %q, want pre-Esc segment 1 (overwritten by late delta on master)", textIDs["text:0:1"])
	}
	if !strings.Contains(textIDs["text:0:2"], "post-clear") {
		t.Errorf("text:0:2 = %q, want post-clear late delta", textIDs["text:0:2"])
	}
}

// TestCounterMapsBoundedByCleanupThread pins the lifecycle: counters
// survive turn boundaries (the architectural fix) but DO get cleaned
// at thread teardown so a long-running session with many threads
// doesn't leak memory.
func TestCounterMapsBoundedByCleanupThread(t *testing.T) {
	router, st, _ := newTestRouter(t)

	// Drive 10 threads × 3 turns each. Each turn emits one text segment
	// so the counter map gets a real entry to track.
	threadIDs := make([]string, 10)
	for i := range threadIDs {
		threadIDs[i] = "thread-" + string(rune('a'+i))
		createTestThread(t, st, threadIDs[i])
		for turnIndex := 0; turnIndex < 3; turnIndex++ {
			if err := router.Handle(provider.ProviderEvent{
				Kind: provider.EventTurnStart, ThreadID: threadIDs[i], TurnIndex: turnIndex,
				Timestamp: time.Now(),
			}); err != nil {
				t.Fatalf("turn %d start on %s: %v", turnIndex, threadIDs[i], err)
			}
			if err := router.Handle(provider.ProviderEvent{
				Kind: provider.EventTextDelta, ThreadID: threadIDs[i],
				Content: "x", Timestamp: time.Now(),
			}); err != nil {
				t.Fatalf("text delta on %s/%d: %v", threadIDs[i], turnIndex, err)
			}
			if err := router.settleStreamingScope(threadIDs[i], ""); err != nil {
				t.Fatalf("close segment on %s/%d: %v", threadIDs[i], turnIndex, err)
			}
			if err := router.Handle(provider.ProviderEvent{
				Kind: provider.EventTurnComplete, ThreadID: threadIDs[i],
				Timestamp: time.Now(),
			}); err != nil {
				t.Fatalf("turn complete on %s/%d: %v", threadIDs[i], turnIndex, err)
			}
		}
	}

	// Architectural fix invariant: counters survive turn boundaries.
	router.mu.Lock()
	if len(router.segmentIndexByScope) == 0 {
		router.mu.Unlock()
		t.Fatal("segmentIndexByScope was wiped at turn-complete — Option-X regression (the fix removed the prefix-sweep but something is still clearing this map)")
	}
	preCleanupSegmentEntries := len(router.segmentIndexByScope)
	router.mu.Unlock()
	if preCleanupSegmentEntries < len(threadIDs) {
		t.Errorf("expected at least %d segment counter entries surviving turn-complete, got %d", len(threadIDs), preCleanupSegmentEntries)
	}

	// CleanupThread is the authoritative cleanup point.
	for _, threadID := range threadIDs {
		router.CleanupThread(threadID)
	}

	router.mu.Lock()
	defer router.mu.Unlock()
	if got := len(router.segmentIndexByScope); got != 0 {
		t.Errorf("segmentIndexByScope leaked %d entries past CleanupThread", got)
	}
	if got := len(router.blockIndexByScope); got != 0 {
		t.Errorf("blockIndexByScope leaked %d entries past CleanupThread", got)
	}
	if got := len(router.errorSeqByScope); got != 0 {
		t.Errorf("errorSeqByScope leaked %d entries past CleanupThread", got)
	}
	if got := len(router.terminalInteractionSeq); got != 0 {
		t.Errorf("terminalInteractionSeq leaked %d entries past CleanupThread", got)
	}
	// User-send-time carry-over maps must also clean up at thread
	// teardown — they survive turn boundaries by design but should not
	// outlive the session.
	if got := len(router.settledTurns); got != 0 {
		t.Errorf("settledTurns leaked %d entries past CleanupThread", got)
	}
	if got := len(router.committedToolPaths); got != 0 {
		t.Errorf("committedToolPaths leaked %d entries past CleanupThread", got)
	}
}

// TestClearOpenTurnSweepsPendingApprovalsAndUserInputs pins the B7 fix:
// pendingApprovals, pendingApprovalItems, and pendingUserInputs are
// inherently mid-turn (the model emits a control_request, the user
// resolves, the model continues). If EventTurnComplete fires while
// any of these are still pending, the turn ended without resolution
// (subprocess died, fatal error, model declined to emit the resolved
// meta). They should be swept on clearOpenTurn so a subsequent turn
// doesn't inherit a stale request id; without the sweep they leak
// until CleanupThread.
func TestClearOpenTurnSweepsPendingApprovalsAndUserInputs(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	// Prime each approval-flavored map. The keys used here mirror the
	// production helpers in approvals.go and user_inputs.go.
	router.setPendingApproval("t1", pendingApprovalState{
		Request: provider.ApprovalRequest{
			RequestID: "req-1",
			ThreadID:  "t1",
			Kind:      "command",
			ToolName:  "Bash",
		},
		ItemID: "tool-1",
	})
	router.rememberApprovalDecision("t1", "tool-1", "approved")
	router.setPendingUserInput("t1", provider.UserInputRequest{
		RequestID: "req-input-1",
		ThreadID:  "t1",
	})

	// Fire EventTurnComplete — clearOpenTurn must sweep all three maps.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}

	router.mu.Lock()
	defer router.mu.Unlock()
	if got := len(router.pendingApprovals); got != 0 {
		t.Errorf("pendingApprovals not swept: %d entries remain", got)
	}
	if got := len(router.pendingApprovalItems); got != 0 {
		t.Errorf("pendingApprovalItems not swept: %d entries remain", got)
	}
	if got := len(router.pendingUserInputs); got != 0 {
		t.Errorf("pendingUserInputs not swept: %d entries remain", got)
	}
}
