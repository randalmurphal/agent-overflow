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
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
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
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
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
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
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
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
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

// TestRoundEmissionPerWireResult pins the per-round emission cadence
// for the multi-result-per-turn cascade. Each wire `result` envelope
// (Claude) emits its own `provider:turn_started` / `provider:turn_completed`
// pair so the frontend's working indicator, Stop button, and composer
// block correctly track "model is engaged right now" — flipping off
// between rounds and back on for the second model call provoked by
// the CLI-synthesized `type:"user"` envelope.
//
// Persistence stays at LOGICAL-TURN granularity: the `turns` row is
// UPDATE-d once (claimTurnSettlement gate), checkpoints capture once,
// streaming items settle once. Late token usage from the second
// `result` folds onto the existing turns row via persistLateTurnPayload.
//
// Wire shape modeled here (matches the Claude
// interactive_outlives_taskoutput_monitor.ndjson fixture):
//
//	user-send → EventTurnStart (round 1 begins)
//	... text/tool ...
//	EventTurnComplete (round 1 ends; result envelope #1)
//	EventInit (Claude system.init re-emit; round 2 begins)
//	... text/tool ...
//	EventTurnComplete (round 2 ends; result envelope #2)
func TestRoundEmissionPerWireResult(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Round 1.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("first turn complete: %v", err)
	}

	// Round 2 begins via Claude system.init replay (EventInit). Under
	// the per-round cadence this synthesizes a fresh provider:turn_started
	// with a new round id, opens a new round slot, and DOES NOT call
	// setOpenTurn (id-allocating counters survive — see
	// TestMultipleResultsPerTurn_TextSegmentsDoNotCollide).
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventInit, ThreadID: "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("re-init: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("second turn complete: %v", err)
	}

	// Two starts, two completes — one per wire round.
	starts := filterEmissions(*emissions, "provider:turn_started")
	if len(starts) != 2 {
		t.Fatalf("expected 2 provider:turn_started emissions (one per wire round), got %d: %+v", len(starts), starts)
	}
	completes := filterEmissions(*emissions, "provider:turn_completed")
	if len(completes) != 2 {
		t.Fatalf("expected 2 provider:turn_completed emissions (one per wire round), got %d: %+v", len(completes), completes)
	}

	// Round ids are distinct across rounds, and each completed pairs
	// with its preceding start.
	r1Start := starts[0].data.(TurnStartedEvent).TurnID
	r1Complete := completes[0].data.(TurnCompletedEvent).TurnID
	r2Start := starts[1].data.(TurnStartedEvent).TurnID
	r2Complete := completes[1].data.(TurnCompletedEvent).TurnID
	if r1Start == "" || r2Start == "" {
		t.Fatalf("round ids must be non-empty: r1=%q r2=%q", r1Start, r2Start)
	}
	if r1Start == r2Start {
		t.Errorf("round 1 and round 2 share the same id %q — each wire round must allocate a fresh uuid", r1Start)
	}
	if r1Complete != r1Start {
		t.Errorf("round 1 complete TurnID = %q, want matching start id %q", r1Complete, r1Start)
	}
	if r2Complete != r2Start {
		t.Errorf("round 2 complete TurnID = %q, want matching start id %q", r2Complete, r2Start)
	}

	// Persistence stays at logical-turn granularity: one turns row,
	// stamped completed_at exactly once. A second UPDATE would have
	// re-stamped completed_at on the second wire complete; the
	// claimTurnSettlement gate guarantees it doesn't.
	turn, found, err := st.GetTurn("t1:0")
	if err != nil || !found {
		t.Fatalf("get turns row: found=%v err=%v", found, err)
	}
	if turn.CompletedAt == nil {
		t.Fatalf("expected completed_at set after first wire complete")
	}
}

// TestRoundStartedAfterSystemInit pins the re-round path in
// handleInit: when an EventInit arrives for a thread whose current
// logical turn is already settled (provider:turn_completed has fired
// at least once), a fresh provider:turn_started is emitted with a
// per-round uuid as TurnID. This is what flips the frontend's
// working indicator back on for the second model call in the
// multi-result-per-turn pattern.
func TestRoundStartedAfterSystemInit(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}
	*emissions = nil // discard the round-1 turn_started/turn_completed

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventInit, ThreadID: "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("re-init: %v", err)
	}

	starts := filterEmissions(*emissions, "provider:turn_started")
	if len(starts) != 1 {
		t.Fatalf("expected 1 provider:turn_started after re-init, got %d: %+v", len(starts), starts)
	}
	payload := starts[0].data.(TurnStartedEvent)
	if payload.TurnID == "" {
		t.Errorf("re-round payload.TurnID is empty — handleInit must allocate a fresh round uuid")
	}
	if payload.TurnIndex != 0 {
		t.Errorf("re-round payload.TurnIndex = %d, want 0 (same logical turn)", payload.TurnIndex)
	}
	if payload.ThreadID != "t1" {
		t.Errorf("re-round payload.ThreadID = %q, want t1", payload.ThreadID)
	}
}

// TestRoundEmission_RecoveryResume_OrphanCompleteIsSilent pins the
// crash-recovery resume edge case: a fresh app/session starts up,
// reattaches to a thread, fires EventInit (no settled marker —
// CleanupThread wiped state on prior shutdown), and a real wire
// EventTurnComplete eventually arrives without any preceding
// EventTurnStart in this process. Per the wire-round emission
// contract:
//
//   - EventInit yields no provider:turn_started (no settled marker).
//   - EventTurnComplete finds an empty currentRoundID slot
//     (takeOpenRound returns "") and skips the wire-round emission.
//   - claimTurnSettlement gates persistence; the orphan complete folds
//     late token usage onto whatever turns row exists (or is a
//     no-op if no row exists).
//
// The frontend therefore observes nothing, and no panic occurs in
// the LastTurnIndex fallback path even when no turn row exists yet
// for the thread.
func TestRoundEmission_RecoveryResume_OrphanCompleteIsSilent(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Fresh process attach: EventInit on a thread with no settled
	// marker (no prior provider:turn_completed has run in THIS process).
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventInit, ThreadID: "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("init: %v", err)
	}

	// Real wire turn-complete arrives without any preceding TurnStart.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("orphan complete: %v", err)
	}

	starts := filterEmissions(*emissions, "provider:turn_started")
	if len(starts) != 0 {
		t.Errorf("expected 0 provider:turn_started emissions on recovery resume, got %d: %+v", len(starts), starts)
	}
	completes := filterEmissions(*emissions, "provider:turn_completed")
	if len(completes) != 0 {
		t.Errorf("expected 0 provider:turn_completed emissions on orphan complete (no open round), got %d: %+v", len(completes), completes)
	}
}

// TestCurrentRoundIDIsBoundedByCleanupThread pins the round-id
// leak guard. CleanupThread MUST wipe currentRoundID along with the
// other per-thread maps so a long-running session bouncing across
// many threads doesn't accumulate stale round entries.
//
// Mirrors TestCounterMapsBoundedByCleanupThread for the round-id
// map specifically.
func TestCurrentRoundIDIsBoundedByCleanupThread(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Open a round.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	router.mu.Lock()
	openRound, hasOpenRound := router.currentRoundID["t1"]
	router.mu.Unlock()
	if !hasOpenRound || openRound == "" {
		t.Fatalf("expected currentRoundID[t1] to be set after EventTurnStart, got %q (present=%v)", openRound, hasOpenRound)
	}

	router.CleanupThread("t1")

	router.mu.Lock()
	defer router.mu.Unlock()
	if _, leaked := router.currentRoundID["t1"]; leaked {
		t.Errorf("currentRoundID leaked entry for t1 past CleanupThread")
	}
	if got := len(router.currentRoundID); got != 0 {
		t.Errorf("currentRoundID has %d entries past CleanupThread, want 0", got)
	}
}

// TestRoundEmission_CrossThreadIsolation pins the per-thread keying
// of currentRoundID: thread A's round id must never be returned for
// thread B's takeOpenRound. Provider events are serialized per
// thread, so this isolation isn't a concurrency requirement today —
// but it's a correctness boundary the design depends on, and a
// regression that swapped per-thread map access for a single global
// slot would silently corrupt round routing without this assertion.
func TestRoundEmission_CrossThreadIsolation(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	createTestThread(t, st, "t2")

	// Open rounds on both threads.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("t1 turn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t2", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("t2 turn start: %v", err)
	}

	starts := filterEmissions(*emissions, "provider:turn_started")
	if len(starts) != 2 {
		t.Fatalf("expected 2 provider:turn_started emissions (one per thread), got %d", len(starts))
	}
	t1Round := ""
	t2Round := ""
	for _, e := range starts {
		payload := e.data.(TurnStartedEvent)
		switch payload.ThreadID {
		case "t1":
			t1Round = payload.TurnID
		case "t2":
			t2Round = payload.TurnID
		}
	}
	if t1Round == "" || t2Round == "" {
		t.Fatalf("missing per-thread round id: t1=%q t2=%q", t1Round, t2Round)
	}
	if t1Round == t2Round {
		t.Fatalf("threads share a round id %q — per-thread keying is broken", t1Round)
	}

	// Complete t1 — the emit MUST carry t1's round id, leaving t2's
	// slot intact.
	*emissions = nil
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("t1 turn complete: %v", err)
	}
	completes := filterEmissions(*emissions, "provider:turn_completed")
	if len(completes) != 1 {
		t.Fatalf("expected 1 provider:turn_completed for t1, got %d", len(completes))
	}
	if completes[0].data.(TurnCompletedEvent).TurnID != t1Round {
		t.Errorf("t1 turn_completed TurnID = %q, want %q", completes[0].data.(TurnCompletedEvent).TurnID, t1Round)
	}
	// t2's slot must still be open.
	router.mu.Lock()
	t2RoundStillOpen := router.currentRoundID["t2"]
	router.mu.Unlock()
	if t2RoundStillOpen != t2Round {
		t.Errorf("t2 round slot was disturbed: got %q, want %q", t2RoundStillOpen, t2Round)
	}

	// Complete t2 — emit must carry t2's round id, not t1's.
	*emissions = nil
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t2",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("t2 turn complete: %v", err)
	}
	completes = filterEmissions(*emissions, "provider:turn_completed")
	if len(completes) != 1 {
		t.Fatalf("expected 1 provider:turn_completed for t2, got %d", len(completes))
	}
	if got := completes[0].data.(TurnCompletedEvent).TurnID; got != t2Round {
		t.Errorf("t2 turn_completed TurnID = %q, want %q (cross-thread leak suspected)", got, t2Round)
	}
}

// TestNoRoundEmissionOnRealSessionInit pins the negative case: an
// EventInit for a thread with NO settled-turn marker (real session
// start, not a re-round) MUST NOT emit provider:turn_started. The
// settled-turn marker is the wire-typed signal that distinguishes
// "Claude is starting a follow-up round of an in-flight logical
// turn" from "fresh session attaching to a thread that has no
// in-flight logical turn." Emitting from the latter case would lie
// to the frontend about the model being engaged.
func TestNoRoundEmissionOnRealSessionInit(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Fresh session: EventInit arrives before any EventTurnStart.
	// Mirrors the recovery-resume path where the app reattaches to a
	// thread whose last logical turn already completed (or was never
	// started) before the previous session ended.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventInit, ThreadID: "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("init: %v", err)
	}

	starts := filterEmissions(*emissions, "provider:turn_started")
	if len(starts) != 0 {
		t.Errorf("expected 0 provider:turn_started for real session init, got %d: %+v (handleInit must only re-light when a prior round of THIS logical turn already settled)", len(starts), starts)
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
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
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
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
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
				TurnComplete: normalTurnCompleteMeta(),
				Timestamp:    time.Now(),
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
		// Logical-turn settlement state survives turn boundaries by design
		// but should not outlive the session.
		if got := len(router.settledTurns); got != 0 {
			t.Errorf("settledTurns leaked %d entries past CleanupThread", got)
		}
	// Per-wire-round id slot — every wire complete clears its own
	// thread's slot via takeOpenRound, but CleanupThread is the safety
	// net for sessions that ended mid-round (no final wire complete).
	if got := len(router.currentRoundID); got != 0 {
		t.Errorf("currentRoundID leaked %d entries past CleanupThread", got)
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
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
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

// TestAssistantErrorThenResultSettlesExactlyOnce pins the wire
// ordering for an SDK API error mid-turn: an `assistant.error`
// envelope is emitted as EventError{fatal:true,
// expect_turn_complete:true}, FOLLOWED by a real `result{is_error:true}`
// envelope as EventTurnComplete. Together they must:
//
//  1. Produce exactly one provider:turn_completed emission (the wire
//     turn-complete settles the turn — no synthesis, since the fatal
//     opted in to "wire complete will follow").
//  2. Persist exactly one api_error row (the EventError handler routes
//     enum-tagged errors to kind:api_error rather than the generic
//     error kind).
//
// A regression here would re-introduce a race where the synthesis
// fires before the wire complete and the handler runs twice.
func TestAssistantErrorThenResultSettlesExactlyOnce(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	*emissions = nil

	// assistant.error envelope → EventError{fatal:true,
	// expect_turn_complete:true, error: rate_limit}. The opt-in flag
	// suppresses synthesis so the real wire result settles the turn.
	errMeta, _ := json.Marshal(map[string]any{
		"fatal":                true,
		"expect_turn_complete": true,
		"error":                "rate_limit",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventError, ThreadID: "t1",
		Content: "Rate limit reached", Meta: errMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("assistant.error: %v", err)
	}

	// Real wire result{is_error:true} arrives as EventTurnComplete,
	// settling the turn for real. With expect_turn_complete:true on
	// the prior fatal there is no synthesis to race against.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("real turn complete: %v", err)
	}

	completed := filterEmissions(*emissions, "provider:turn_completed")
	if len(completed) != 1 {
		t.Fatalf("expected exactly 1 provider:turn_completed, got %d", len(completed))
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var apiErrors int
	for _, it := range items {
		if it.Kind == itemKindAPIError {
			apiErrors++
		}
	}
	if apiErrors != 1 {
		t.Fatalf("expected exactly 1 api_error row, got %d (%+v)", apiErrors, items)
	}
}

// TestSoftThenRealTurnComplete_RealisticCascade pins the production
// flow on a normal (single-round) Claude turn where the soft
// EventTurnComplete fires from `message_delta.stop_reason="end_turn"`
// and ALREADY carries the peeked assistant_message_id (parser observed
// an assistant envelope earlier in the round). The trailing wire
// `result` arrives with usage and the same amid (taken via
// takeLastAssistantMessageID).
//
// Pin: token usage folds in (first non-empty wins, soft had none);
// amid stays the same value (last non-empty wins is a no-op when the
// trailing event carries the same id). completed_at is NOT re-stamped
// (first-settle clock preserved). No second `provider:turn_completed`
// emits for the same round.
func TestSoftThenRealTurnComplete_RealisticCascade(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	// Soft complete carries the peeked assistant_message_id — the
	// parser would have stamped it from the prior `assistant`
	// envelope. This is the production-path shape.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: &provider.SoftRoundCloseMeta{
			StopReason:         "end_turn",
			AssistantMessageID: "msg_softA",
		},
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("soft turn complete: %v", err)
	}

	// Soft fired exactly one provider:turn_completed for the round.
	completedAfterSoft := filterEmissions(*emissions, "provider:turn_completed")
	if len(completedAfterSoft) != 1 {
		t.Fatalf("expected exactly 1 provider:turn_completed after soft, got %d", len(completedAfterSoft))
	}

	// Settled row carries the soft's stop_reason and amid.
	turn, found, err := st.GetTurn("t1:0")
	if err != nil || !found {
		t.Fatalf("get turns row after soft: found=%v err=%v", found, err)
	}
	if turn.CompletedAt == nil {
		t.Fatalf("expected completed_at set after soft complete")
	}
	firstCompletedAt := *turn.CompletedAt
	if turn.StopReason != "end_turn" {
		t.Errorf("stop_reason after soft = %q, want %q", turn.StopReason, "end_turn")
	}
	if turn.AssistantMessageID != "msg_softA" {
		t.Errorf("assistant_message_id after soft = %q, want %q (peeked from parser)", turn.AssistantMessageID, "msg_softA")
	}
	if turn.TokenUsageJSON != "" {
		t.Errorf("token_usage_json after soft = %q, want empty (no usage on soft event)", turn.TokenUsageJSON)
	}

	// Trailing real `result` arrives ms later with the same amid
	// (taken from the parser) and the cumulative usage.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: &provider.WireTurnCompleteMeta{
			StopReason:         "end_turn",
			AssistantMessageID: "msg_softA",
			Usage:              &provider.TokenUsage{InputTokens: 6, OutputTokens: 34},
		},
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("real turn complete: %v", err)
	}

	// No second provider:turn_completed for the round.
	completedAfterReal := filterEmissions(*emissions, "provider:turn_completed")
	if len(completedAfterReal) != 1 {
		t.Fatalf("expected still exactly 1 provider:turn_completed after real, got %d", len(completedAfterReal))
	}

	turn, _, err = st.GetTurn("t1:0")
	if err != nil {
		t.Fatalf("get turns row after real: %v", err)
	}
	if turn.CompletedAt == nil || *turn.CompletedAt != firstCompletedAt {
		t.Errorf("completed_at re-stamped on trailing result: got %v, want %d (first-settle clock preserved)",
			turn.CompletedAt, firstCompletedAt)
	}
	if turn.AssistantMessageID != "msg_softA" {
		t.Errorf("assistant_message_id after real = %q, want %q (first-settle wins)",
			turn.AssistantMessageID, "msg_softA")
	}
	if !strings.Contains(turn.TokenUsageJSON, `"inputTokens":6`) {
		t.Errorf("token_usage_json after real = %q, want fold-in of inputTokens=6", turn.TokenUsageJSON)
	}
}

// TestSoftThenRealTurnComplete_SoftMissingAmidFoldsIn covers the
// defensive case where the parser had not yet observed an assistant
// envelope when message_delta arrived (degenerate ordering, fresh
// session attach, malformed wire). The soft event has no
// assistant_message_id; the trailing `result`-driven event carries the
// parser-derived id; the fold-in writes it onto the empty column.
func TestSoftThenRealTurnComplete_SoftMissingAmidFoldsIn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: &provider.SoftRoundCloseMeta{StopReason: "end_turn"},
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("soft turn complete: %v", err)
	}

	turn, _, _ := st.GetTurn("t1:0")
	if turn.AssistantMessageID != "" {
		t.Fatalf("expected empty amid after amid-less soft, got %q", turn.AssistantMessageID)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: &provider.WireTurnCompleteMeta{
			StopReason:         "end_turn",
			AssistantMessageID: "msg_realLate",
			Usage:              &provider.TokenUsage{InputTokens: 1, OutputTokens: 2},
		},
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("real turn complete: %v", err)
	}

	turn, _, _ = st.GetTurn("t1:0")
	if turn.AssistantMessageID != "msg_realLate" {
		t.Errorf("amid fold-in failed on empty column: got %q, want %q",
			turn.AssistantMessageID, "msg_realLate")
	}
}

// TestSoftThenInitThenSoftThenReal_ReRoundCascade pins the
// stdin-during-wait pattern from local_agent_user_input_during_wait
// fixture: parent end_turn → re-round init → another parent end_turn
// → trailing wire `result`. The frontend's working indicator must
// cycle correctly per round (off/on/off), and the persisted turn row
// must end up with the FINAL round's assistant_message_id (so
// SettledTurn.assistantMessageId points to the last assistant
// message of the turn — its documented contract).
func TestSoftThenInitThenSoftThenReal_ReRoundCascade(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: &provider.SoftRoundCloseMeta{
			StopReason:         "end_turn",
			AssistantMessageID: "msg_round1",
		},
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("soft #1: %v", err)
	}

	// Re-round triggered by Claude system.init (after task_notification
	// or stdin injection during wait window).
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventInit, ThreadID: "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("re-round init: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: &provider.SoftRoundCloseMeta{
			StopReason:         "end_turn",
			AssistantMessageID: "msg_round2",
		},
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("soft #2: %v", err)
	}

	// Trailing wire `result` for round 2 — `takeOpenRound` was taken
	// by soft #2, so this emits no third turn_completed.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: &provider.WireTurnCompleteMeta{
			StopReason:         "end_turn",
			AssistantMessageID: "msg_round2",
			Usage:              &provider.TokenUsage{InputTokens: 8, OutputTokens: 50},
		},
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("real result: %v", err)
	}

	// Per-round emissions: 2 starts (handleTurnStart + re-round init)
	// and 2 completes (soft #1 + soft #2). The trailing real result
	// emitted nothing because soft #2 already consumed round 2's slot.
	starts := filterEmissions(*emissions, "provider:turn_started")
	if len(starts) != 2 {
		t.Errorf("expected 2 provider:turn_started, got %d", len(starts))
	}
	completes := filterEmissions(*emissions, "provider:turn_completed")
	if len(completes) != 2 {
		t.Errorf("expected 2 provider:turn_completed, got %d", len(completes))
	}

	// Persistence:
	//   - amid = round 2 (the FINAL round's last assistant message);
	//     each subsequent settle overwrites so the column reflects
	//     the latest message of the turn, matching the
	//     SettledTurn.assistantMessageId contract.
	//   - token_usage_json = trailing result's usage (first non-empty
	//     wins; soft events don't carry usage so the trailing real
	//     `result` populates it).
	turn, _, _ := st.GetTurn("t1:0")
	if turn.AssistantMessageID != "msg_round2" {
		t.Errorf("expected final-round amid 'msg_round2', got %q (last-write-wins for amid)",
			turn.AssistantMessageID)
	}
	if !strings.Contains(turn.TokenUsageJSON, `"inputTokens":8`) {
		t.Errorf("expected usage folded from trailing result, got %q", turn.TokenUsageJSON)
	}
}
