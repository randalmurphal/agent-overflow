package triage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
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
	starts := filterEmissions(emissions.snapshot(), "provider:turn_started")
	if len(starts) != 2 {
		t.Fatalf("expected 2 provider:turn_started emissions (one per wire round), got %d: %+v", len(starts), starts)
	}
	completes := filterEmissions(emissions.snapshot(), "provider:turn_completed")
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
	emissions.reset() // discard the round-1 turn_started/turn_completed

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventInit, ThreadID: "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("re-init: %v", err)
	}

	starts := filterEmissions(emissions.snapshot(), "provider:turn_started")
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
//   - EventTurnComplete finds an empty currentRoundByThread slot
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

	starts := filterEmissions(emissions.snapshot(), "provider:turn_started")
	if len(starts) != 0 {
		t.Errorf("expected 0 provider:turn_started emissions on recovery resume, got %d: %+v", len(starts), starts)
	}
	completes := filterEmissions(emissions.snapshot(), "provider:turn_completed")
	if len(completes) != 0 {
		t.Errorf("expected 0 provider:turn_completed emissions on orphan complete (no open round), got %d: %+v", len(completes), completes)
	}
}

// TestCurrentRoundByThreadIsBoundedByCleanupThread pins the round snapshot
// leak guard. CleanupThread MUST wipe currentRoundByThread along with the
// other per-thread maps so a long-running session bouncing across
// many threads doesn't accumulate stale round entries.
//
// Mirrors TestCounterMapsBoundedByCleanupThread for the round snapshot
// map specifically.
func TestCurrentRoundByThreadIsBoundedByCleanupThread(t *testing.T) {
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
	openRound, hasOpenRound := router.currentRoundByThread["t1"]
	router.mu.Unlock()
	if !hasOpenRound || openRound.TurnID == "" {
		t.Fatalf("expected currentRoundByThread[t1] to be set after EventTurnStart, got %+v (present=%v)", openRound, hasOpenRound)
	}

	router.CleanupThread("t1")

	router.mu.Lock()
	defer router.mu.Unlock()
	if _, leaked := router.currentRoundByThread["t1"]; leaked {
		t.Errorf("currentRoundByThread leaked entry for t1 past CleanupThread")
	}
	if got := len(router.currentRoundByThread); got != 0 {
		t.Errorf("currentRoundByThread has %d entries past CleanupThread, want 0", got)
	}
}

// TestRoundEmission_CrossThreadIsolation pins the per-thread keying
// of currentRoundByThread: thread A's round id must never be returned for
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

	starts := filterEmissions(emissions.snapshot(), "provider:turn_started")
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
	emissions.reset()
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("t1 turn complete: %v", err)
	}
	completes := filterEmissions(emissions.snapshot(), "provider:turn_completed")
	if len(completes) != 1 {
		t.Fatalf("expected 1 provider:turn_completed for t1, got %d", len(completes))
	}
	if completes[0].data.(TurnCompletedEvent).TurnID != t1Round {
		t.Errorf("t1 turn_completed TurnID = %q, want %q", completes[0].data.(TurnCompletedEvent).TurnID, t1Round)
	}
	// t2's slot must still be open.
	router.mu.Lock()
	t2RoundStillOpen := router.currentRoundByThread["t2"]
	router.mu.Unlock()
	if t2RoundStillOpen.TurnID != t2Round {
		t.Errorf("t2 round slot was disturbed: got %q, want %q", t2RoundStillOpen.TurnID, t2Round)
	}

	// Complete t2 — emit must carry t2's round id, not t1's.
	emissions.reset()
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t2",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("t2 turn complete: %v", err)
	}
	completes = filterEmissions(emissions.snapshot(), "provider:turn_completed")
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

	starts := filterEmissions(emissions.snapshot(), "provider:turn_started")
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

	completed := filterEmissions(emissions.snapshot(), "provider:turn_completed")
	if len(completed) != 1 {
		t.Errorf("expected exactly 1 provider:turn_completed emission, got %d (idempotent guard regression)", len(completed))
	}
}

func TestSyntheticTruncatedTurnCompleteSettlesCodexWireTurnID(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	const wireTurnID = "turn-codex-fatal"
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnID: wireTurnID, TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	fatalMeta, _ := json.Marshal(map[string]any{"fatal": true})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventError, ThreadID: "t1",
		Content: "codex fatal error", Meta: fatalMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("fatal error: %v", err)
	}

	wireTurn, found, err := st.GetTurn(wireTurnID)
	if err != nil {
		t.Fatalf("get wire turn: %v", err)
	}
	if !found {
		t.Fatalf("expected wire turn row %q", wireTurnID)
	}
	if wireTurn.CompletedAt == nil {
		t.Fatalf("wire turn %q was not settled", wireTurnID)
	}
	if wireTurn.StopReason != "interrupted" {
		t.Errorf("wire turn stop_reason = %q, want interrupted", wireTurn.StopReason)
	}
	if _, synthFound, err := st.GetTurn("t1:0"); err != nil {
		t.Fatalf("get synthetic turn: %v", err)
	} else if synthFound {
		t.Fatalf("synthetic fallback turn row should not be created for Codex wire turn")
	}
	if active, ok, err := st.GetActiveTurn("t1"); err != nil {
		t.Fatalf("get active turn: %v", err)
	} else if ok {
		t.Fatalf("expected no active turn after fatal synthesis, got %+v", active)
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
	if got := len(router.currentRoundByThread); got != 0 {
		t.Errorf("currentRoundByThread leaked %d entries past CleanupThread", got)
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
	emissions.reset()

	// assistant.error envelope → EventError{fatal:true,
	// expect_turn_complete:true, error: rate_limit}. The opt-in flag
	// suppresses synthesis so the real wire result settles the turn.
	errMeta, _ := json.Marshal(map[string]any{
		"api_error_enum":       "rate_limit",
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
	const apiError = "API Error: 529 Overloaded. This is a server-side issue, usually temporary - try again in a moment. If it persists, check status.claude.com."
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: &provider.WireTurnCompleteMeta{
			StopReason:   "error",
			ErrorMessage: apiError,
		},
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("real turn complete: %v", err)
	}

	completed := filterEmissions(emissions.snapshot(), "provider:turn_completed")
	if len(completed) != 1 {
		t.Fatalf("expected exactly 1 provider:turn_completed, got %d", len(completed))
	}
	payload, ok := completed[0].data.(TurnCompletedEvent)
	if !ok {
		t.Fatalf("completed payload type = %T, want TurnCompletedEvent", completed[0].data)
	}
	if payload.StopReason != "error" {
		t.Fatalf("completed stopReason = %q, want error", payload.StopReason)
	}
	if payload.ErrorMessage != apiError {
		t.Fatalf("completed errorMessage = %q, want %q", payload.ErrorMessage, apiError)
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
	turn, found, err := st.GetTurn("t1:0")
	if err != nil || !found {
		t.Fatalf("get turn: found=%v err=%v", found, err)
	}
	if turn.StopReason != "error" {
		t.Fatalf("turn stopReason = %q, want error", turn.StopReason)
	}
	if turn.ErrorMessage != apiError {
		t.Fatalf("turn errorMessage = %q, want %q", turn.ErrorMessage, apiError)
	}
}

func TestNormalizedAPIErrorEnumCreatesAPIErrorRow(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	errMeta, _ := json.Marshal(map[string]any{
		"api_error_enum": "future_provider_enum",
		"error":          "future_provider_enum",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventError, ThreadID: "t1",
		Content: "Future provider API error", Meta: errMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("api error: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v, want exactly one api_error row", items)
	}
	if items[0].Kind != itemKindAPIError {
		t.Fatalf("kind = %q, want %q", items[0].Kind, itemKindAPIError)
	}
}

func TestCodexFatalNotificationAndFailedTurnCreateOneGenericErrorRow(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	const errorMessage = "Your access token could not be refreshed because your refresh token was revoked. Please log out and sign in again."
	codexMeta, _ := json.Marshal(map[string]any{
		"fatal": true,
		"error": errorMessage,
		"wire": map[string]any{
			"error": map[string]any{"message": errorMessage},
		},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventError, ThreadID: "t1",
		Content: errorMessage, Meta: codexMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("codex fatal notification: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventError, ThreadID: "t1",
		Content:   errorMessage,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("codex failed turn duplicate: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var genericErrors, apiErrors int
	for _, it := range items {
		switch it.Kind {
		case "error":
			genericErrors++
			if it.Summary != errorMessage {
				t.Fatalf("error summary = %q, want %q", it.Summary, errorMessage)
			}
		case itemKindAPIError:
			apiErrors++
		}
	}
	if genericErrors != 1 {
		t.Fatalf("generic error rows = %d, want 1; items=%+v", genericErrors, items)
	}
	if apiErrors != 0 {
		t.Fatalf("api_error rows = %d, want 0; items=%+v", apiErrors, items)
	}
}

func TestDuplicateFatalErrorStillDrainsInterruptQueue(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	const errorMessage = "provider failure"
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventError, ThreadID: "t1",
		Content:   errorMessage,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("non-fatal error: %v", err)
	}

	insertToolCallItem(t, st, "t1", "launch-ok", "Bash", "Bash", statusRunning)
	valid := validDrainCompletion("complete:launch-ok", "launch-ok", 11, 2)
	valid.item.Status = statusCompleted
	router.mu.Lock()
	router.interruptQueue["t1"] = []queuedPersistence{valid}
	router.mu.Unlock()

	fatalMeta, _ := json.Marshal(map[string]any{"fatal": true})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventError, ThreadID: "t1",
		Content: errorMessage, Meta: fatalMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("duplicate fatal error: %v", err)
	}

	done := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(done) != 1 {
		t.Fatalf("background_done rows = %+v, want one drained row", done)
	}
	if done[0].Status != statusErrored {
		t.Fatalf("background_done status = %q, want %q", done[0].Status, statusErrored)
	}
	if !strings.Contains(done[0].Summary, "— interrupted") {
		t.Fatalf("background_done summary = %q, want interrupted suffix", done[0].Summary)
	}

	errors := findItemsByKind(t, st, "t1", "error")
	if len(errors) != 1 {
		t.Fatalf("error rows = %+v, want duplicate fatal to reuse existing row", errors)
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
	completedAfterSoft := filterEmissions(emissions.snapshot(), "provider:turn_completed")
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
	completedAfterReal := filterEmissions(emissions.snapshot(), "provider:turn_completed")
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

func TestSoftThenRealTurnComplete_LateResultErrorUpdatesSettledTurn(t *testing.T) {
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
			AssistantMessageID: "msg_softA",
		},
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("soft turn complete: %v", err)
	}

	const apiError = "API Error: 529 Overloaded. This is a server-side issue, usually temporary - try again in a moment. If it persists, check status.claude.com."
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: &provider.WireTurnCompleteMeta{
			StopReason:         "error",
			AssistantMessageID: "msg_softA",
			ErrorMessage:       apiError,
			Usage:              &provider.TokenUsage{InputTokens: 6, OutputTokens: 34},
		},
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("late error result: %v", err)
	}

	completed := filterEmissions(emissions.snapshot(), "provider:turn_completed")
	if len(completed) != 1 {
		t.Fatalf("expected only the soft provider:turn_completed emission, got %d", len(completed))
	}
	payload, ok := completed[0].data.(TurnCompletedEvent)
	if !ok {
		t.Fatalf("completed payload type = %T, want TurnCompletedEvent", completed[0].data)
	}
	if payload.StopReason != "end_turn" || payload.ErrorMessage != "" {
		t.Fatalf("soft payload = (%q, %q), want initial non-error close", payload.StopReason, payload.ErrorMessage)
	}

	turn, found, err := st.GetTurn("t1:0")
	if err != nil || !found {
		t.Fatalf("get turn after late error: found=%v err=%v", found, err)
	}
	if turn.StopReason != "error" {
		t.Fatalf("stop_reason after late error = %q, want error", turn.StopReason)
	}
	if turn.ErrorMessage != apiError {
		t.Fatalf("error_message after late error = %q, want %q", turn.ErrorMessage, apiError)
	}
	if !strings.Contains(turn.TokenUsageJSON, `"inputTokens":6`) {
		t.Fatalf("token_usage_json after late error = %q, want fold-in of inputTokens=6", turn.TokenUsageJSON)
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
	starts := filterEmissions(emissions.snapshot(), "provider:turn_started")
	if len(starts) != 2 {
		t.Errorf("expected 2 provider:turn_started, got %d", len(starts))
	}
	completes := filterEmissions(emissions.snapshot(), "provider:turn_completed")
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

// parentContentBlockStart drives a parent (parent_tool_use_id == "")
// content_block_start, the wire event that re-arms a settled round in
// the parent-content-resume path.
func parentContentBlockStart(threadID, blockType string) provider.ProviderEvent {
	return provider.ProviderEvent{
		Kind:      provider.EventContentBlockStart,
		ThreadID:  threadID,
		Meta:      json.RawMessage(`{"blockType":"` + blockType + `"}`),
		Timestamp: time.Now(),
	}
}

// TestParentContentResumeReArmsAfterSoftClose pins the Claude 2.1.154+
// fix: the CLI splits one logical turn into multiple wire messages,
// closing each segment with a parent message_delta stop_reason (the
// soft round-close) and resuming the SAME turn with a fresh parent
// message — no intervening `result` or `system.init`. The soft-close
// fires provider:turn_completed (clearing the working indicator + Stop
// button); the first parent content block of the resumed segment must
// re-emit provider:turn_started so the indicator lights back up.
//
// Models the real captured sequence (provider-events-2026-05-28.ndjson.2,
// thread a20f339e / 501fa978): two soft-close→resume cycles in one
// logical turn (e.g. ToolSearch end_turn, then ExitPlanMode end_turn),
// each followed by a thinking/text resume. Asserts the indicator
// re-arms on EACH cycle — a single-cycle test under-covers it.
func TestParentContentResumeReArmsAfterSoftClose(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	// Two soft-close → parent-resume cycles within one logical turn.
	for cycle := 1; cycle <= 2; cycle++ {
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventTurnComplete, ThreadID: "t1",
			TurnComplete: &provider.SoftRoundCloseMeta{StopReason: "end_turn"},
			Timestamp:    time.Now(),
		}); err != nil {
			t.Fatalf("cycle %d soft close: %v", cycle, err)
		}

		emissions.reset() // discard this cycle's turn_completed

		// Parent resumes the SAME turn with a fresh content block. First
		// block re-arms; a second block in the same resumed round must NOT
		// emit again (no blink per content block).
		if err := router.Handle(parentContentBlockStart("t1", "thinking")); err != nil {
			t.Fatalf("cycle %d resume block 1: %v", cycle, err)
		}
		if err := router.Handle(parentContentBlockStart("t1", "text")); err != nil {
			t.Fatalf("cycle %d resume block 2: %v", cycle, err)
		}

		starts := filterEmissions(emissions.snapshot(), "provider:turn_started")
		if len(starts) != 1 {
			t.Fatalf("cycle %d: expected exactly 1 provider:turn_started on parent resume, got %d: %+v",
				cycle, len(starts), starts)
		}
		payload := starts[0].data.(TurnStartedEvent)
		if payload.TurnID == "" {
			t.Errorf("cycle %d: re-arm payload.TurnID is empty — must allocate a fresh round uuid", cycle)
		}
		if payload.TurnIndex != 0 {
			t.Errorf("cycle %d: re-arm payload.TurnIndex = %d, want 0 (same logical turn)", cycle, payload.TurnIndex)
		}
	}
}

// TestParentContentResumeDoesNotCollideRowIDs is the row-integrity
// guard for the parent-content-resume re-arm path — the analogue of
// TestMultipleResultsPerTurn_TextSegmentsDoNotCollide for the
// soft-close→content_block_start re-round (rather than the
// system.init re-round). It pins that re-arming via
// maybeReopenSettledRound does NOT call setOpenTurn: the id-allocating
// segment counter must survive the segment boundary so post-resume
// text rows get fresh ids instead of overwriting earlier rows.
//
// Without the emission-only tests this is uncovered: a regression
// making maybeReopenSettledRound reset counters (e.g. calling
// setOpenTurn) would still emit the right turn_started events and pass
// all four emission tests, while silently reintroducing the
// multi-result id-collision data-loss bug. This test fails in that
// case because the resumed segment would collide with text:0:0.
//
// It also asserts the wire-round cadence directly: exactly one
// provider:turn_completed per soft-close (proving each re-armed round
// is properly closed — no stuck-open round).
func TestParentContentResumeDoesNotCollideRowIDs(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	// Segment 1 (round 1): one text block, then the parent soft-closes.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1",
		Content: "Segment one before the soft close.", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("seg1 delta: %v", err)
	}
	if err := router.settleStreamingScope("t1", ""); err != nil {
		t.Fatalf("close seg1: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: &provider.SoftRoundCloseMeta{StopReason: "end_turn"},
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("soft close 1: %v", err)
	}

	// Parent resumes the SAME logical turn: content_block_start re-arms
	// (no system.init, no fresh EventTurnStart), then segment 2 streams.
	if err := router.Handle(parentContentBlockStart("t1", "text")); err != nil {
		t.Fatalf("resume block: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1",
		Content: "Segment two after the resume.", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("seg2 delta: %v", err)
	}
	if err := router.settleStreamingScope("t1", ""); err != nil {
		t.Fatalf("close seg2: %v", err)
	}

	// Real wire result terminates the logical turn.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("final complete: %v", err)
	}

	// Row integrity: two distinct assistant_text rows, no collision.
	items, err := st.ListTurnItems("t1", 0)
	if err != nil {
		t.Fatalf("list turn items: %v", err)
	}
	textItems := make(map[string]string)
	for _, it := range items {
		if it.Kind == itemKindAssistantText {
			textItems[it.ID] = it.Summary
		}
	}
	if len(textItems) != 2 {
		t.Fatalf("expected 2 distinct assistant_text rows across the resume, got %d: %+v", len(textItems), textItems)
	}
	if !strings.Contains(textItems["text:0:0"], "Segment one") {
		t.Errorf("text:0:0 = %q, want segment-1 content (counter must survive the resume, not overwrite)", textItems["text:0:0"])
	}
	if !strings.Contains(textItems["text:0:1"], "Segment two") {
		t.Errorf("text:0:1 = %q, want segment-2 content at a fresh id", textItems["text:0:1"])
	}

	// Wire-round cadence: one turn_completed per soft-close + one for the
	// final result = the round re-armed by the resume was properly closed.
	completes := filterEmissions(emissions.snapshot(), "provider:turn_completed")
	if len(completes) != 2 {
		t.Fatalf("expected exactly 2 provider:turn_completed (soft close + final result), got %d: %+v",
			len(completes), completes)
	}
}

// TestSubagentContentDoesNotReArmDuringSoftClose pins invariant 27: in
// the local_agent-outlives case the parent legitimately ends (soft
// end_turn) while a Task subagent keeps running until the trailing
// `result`. The indicator SHOULD stay cleared through that wait, so
// subagent content (parent_tool_use_id != "") must NOT re-arm the
// parent's round. Without the parent-only gate in handleContentBlockStart
// this would wrongly light the indicator during the subagent wait.
func TestSubagentContentDoesNotReArmDuringSoftClose(t *testing.T) {
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
		TurnComplete: &provider.SoftRoundCloseMeta{StopReason: "end_turn"},
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("soft close: %v", err)
	}
	emissions.reset()

	// Subagent streams content while the parent round is closed.
	subBlock := parentContentBlockStart("t1", "text")
	subBlock.ParentToolUseID = "task-abc"
	if err := router.Handle(subBlock); err != nil {
		t.Fatalf("subagent content block: %v", err)
	}

	starts := filterEmissions(emissions.snapshot(), "provider:turn_started")
	if len(starts) != 0 {
		t.Fatalf("subagent content must NOT re-arm the parent round (invariant 27), got %d turn_started: %+v",
			len(starts), starts)
	}
}

// TestParentContentDoesNotReArmMidRound guards against over-firing: a
// parent content block while the round is still OPEN (ordinary
// streaming, no soft-close yet) must not emit a spurious
// provider:turn_started. Round 1 is not settled until its first
// complete, so the settled guard alone covers this — but the
// no-open-round guard is the belt-and-suspenders that keeps every
// in-round block start a no-op.
func TestParentContentDoesNotReArmMidRound(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	emissions.reset() // discard round-1 turn_started

	if err := router.Handle(parentContentBlockStart("t1", "thinking")); err != nil {
		t.Fatalf("mid-round content block: %v", err)
	}
	starts := filterEmissions(emissions.snapshot(), "provider:turn_started")
	if len(starts) != 0 {
		t.Fatalf("mid-round (open, unsettled) content must not re-arm, got %d: %+v", len(starts), starts)
	}
}

// TestParentContentDoesNotReArmFreshSession guards the fresh-attach
// case: a session attaches to a thread (no prior turn settled in this
// process) and the model streams. A parent content block must not
// synthesize a turn_started out of nothing — only a thread with a
// settled logical turn (a real prior complete) is eligible for re-round.
func TestParentContentDoesNotReArmFreshSession(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventInit, ThreadID: "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("init: %v", err)
	}
	emissions.reset()

	if err := router.Handle(parentContentBlockStart("t1", "thinking")); err != nil {
		t.Fatalf("fresh-session content block: %v", err)
	}
	starts := filterEmissions(emissions.snapshot(), "provider:turn_started")
	if len(starts) != 0 {
		t.Fatalf("fresh-session content must not synthesize a round, got %d: %+v", len(starts), starts)
	}
}

// TestLateFoldSettlesOrphanStreamingItem is the Bug 2 regression test for
// thread fc24607e — the permanent "thinking spinner". A logical turn
// settles once (soft round-close), the CLI re-rounds the SAME turn (a
// task_notification continuation), and the re-round opens a streaming
// thinking block that STALLS — no content_block_stop ever arrives. When
// the trailing wire `result` lands it is a late-fold (the turn was
// already settled), and handleTurnComplete returns early without calling
// settleTurnStreaming. The orphaned thinking item is therefore left at
// status=streaming forever, which the frontend renders as a thinking
// block that never resolves.
//
// FAILS before Fix B: the orphan stays status=streaming after the final
// result. PASSES after: the late-fold path settles orphan streams too.
func TestLateFoldSettlesOrphanStreamingItem(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Turn 0 starts, streams a thinking block that closes cleanly, and the
	// parent soft-closes — the FIRST settlement of the logical turn.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventThinking, ThreadID: "t1",
		Content: "Round-one reasoning that closes cleanly.", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("round-1 thinking: %v", err)
	}
	if err := router.settleStreamingScope("t1", ""); err != nil {
		t.Fatalf("settle round-1: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: &provider.SoftRoundCloseMeta{StopReason: "end_turn"}, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("soft close: %v", err)
	}
	router.WaitForPendingSettles()

	// The CLI re-rounds the SAME logical turn: content_block_start re-arms,
	// then attempt-1 thinking streams... and STALLS. No content_block_stop
	// is ever emitted for it — it is orphaned at status=streaming.
	if err := router.Handle(parentContentBlockStart("t1", "thinking")); err != nil {
		t.Fatalf("re-round re-arm: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventThinking, ThreadID: "t1",
		Content: "Re-round reasoning that never gets a content_block_stop.", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("orphan thinking: %v", err)
	}

	// Setup invariant: exactly one orphan thinking item at status=streaming
	// before the final result (round-1 thinking already settled above).
	var orphanID string
	for _, it := range findItemsByKind(t, st, "t1", itemKindThinking) {
		if it.Status == statusStreaming {
			if orphanID != "" {
				t.Fatalf("setup: more than one streaming thinking item")
			}
			orphanID = it.ID
		}
	}
	if orphanID == "" {
		t.Fatalf("setup: expected an orphan thinking item at status=streaming")
	}

	// The trailing wire `result` settles the logical turn — a late-fold,
	// since the soft close already claimed settlement.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("final result: %v", err)
	}
	router.WaitForPendingSettles()

	settled, found, err := st.GetThreadItem("t1", orphanID)
	if err != nil {
		t.Fatalf("get orphan after final result: %v", err)
	}
	if !found {
		t.Fatalf("orphan thinking item vanished")
	}
	if settled.Status == statusStreaming {
		t.Fatalf("Bug 2: orphan thinking item stuck at status=streaming after turn complete — the permanent thinking spinner")
	}
	if settled.Status != statusCompleted {
		t.Errorf("orphan thinking settled to %q, want %q", settled.Status, statusCompleted)
	}
}

// TestLateFoldForceClosesOrphanToolCall covers the tool-call sibling of
// the late-fold settlement gap (invariant 23). A foreground tool_call
// opened on a re-round whose tool_result the provider dropped is left at
// status=running; if the trailing wire `result` is a late-fold it skips
// forceCloseOrphanToolCalls and the tool card hangs "running" forever.
//
// FAILS before Fix B: the tool_call stays status=running. PASSES after:
// the late-fold path force-closes orphan foreground tool_calls too.
func TestLateFoldForceClosesOrphanToolCall(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	// First settlement of the logical turn.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: &provider.SoftRoundCloseMeta{StopReason: "end_turn"}, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("soft close: %v", err)
	}

	// A re-round opens a foreground tool_call whose tool_result never
	// arrives. It is orphaned at status=running on the same logical turn.
	insertToolCallItem(t, st, "t1", "tool-orphan", "Bash: long task", "Bash", statusRunning)

	// Trailing wire result late-folds the already-settled turn.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("final result: %v", err)
	}
	router.WaitForPendingSettles()

	settled, found, err := st.GetThreadItem("t1", "tool-orphan")
	if err != nil {
		t.Fatalf("get tool_call after final result: %v", err)
	}
	if !found {
		t.Fatalf("tool_call vanished")
	}
	if settled.Status == statusRunning {
		t.Fatalf("Bug 2 (tool variant): orphan foreground tool_call stuck at status=running after turn complete")
	}
	if settled.Status != statusErrored {
		t.Errorf("orphan tool_call settled to %q, want %q", settled.Status, statusErrored)
	}
}

// TestLateFoldOrphanSettlesErroredWhenTruncated pins the errored branch of
// the late-fold orphan settle: when the trailing wire complete is
// truncated/aborted (not a clean end_turn), the orphan streaming item must
// settle to errored, not completed — mirroring the full path's
// settledTurnStatus choice. Without exercising this, the truncated branch
// of the late-fold settle is unguarded.
func TestLateFoldOrphanSettlesErroredWhenTruncated(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	// First settlement of the logical turn (soft close).
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: &provider.SoftRoundCloseMeta{StopReason: "end_turn"}, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("soft close: %v", err)
	}

	// Re-round opens an orphan thinking item that never closes.
	if err := router.Handle(parentContentBlockStart("t1", "thinking")); err != nil {
		t.Fatalf("re-round re-arm: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventThinking, ThreadID: "t1",
		Content: "Reasoning interrupted before it closes.", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("orphan thinking: %v", err)
	}
	var orphanID string
	for _, it := range findItemsByKind(t, st, "t1", itemKindThinking) {
		if it.Status == statusStreaming {
			orphanID = it.ID
		}
	}
	if orphanID == "" {
		t.Fatalf("setup: expected an orphan thinking item at status=streaming")
	}

	// Trailing wire complete is aborted — a truncated late-fold.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "interrupted", Aborted: true},
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("final aborted result: %v", err)
	}
	router.WaitForPendingSettles()

	settled, found, err := st.GetThreadItem("t1", orphanID)
	if err != nil {
		t.Fatalf("get orphan after aborted result: %v", err)
	}
	if !found {
		t.Fatalf("orphan thinking item vanished")
	}
	if settled.Status == statusStreaming {
		t.Fatalf("orphan thinking stuck at status=streaming after truncated turn complete")
	}
	if settled.Status != statusErrored {
		t.Errorf("orphan thinking settled to %q, want %q (truncated late-fold)", settled.Status, statusErrored)
	}
}

// TestCLIRetrySnapshotRecoversContentAndSettlesOrphan is the end-to-end
// capstone for thread fc24607e, exercising the triage side of BOTH fixes
// composing on the exact failure scenario:
//
//  1. Turn 0 settles once (message X, soft close).
//  2. A task_notification re-rounds the SAME turn; attempt-1 thinking
//     streams and STALLS (orphan, never closed) — Bug 2.
//  3. The CLI internally retries and delivers attempt 2 as a coalesced
//     snapshot with a fresh message id and no stream lifecycle. Fix A
//     surfaces those never-streamed blocks as content-bearing
//     EventContentBlockStop events (fed here as the parser would emit
//     them); triage's !active+finalContentPresent path persists them as
//     completed rows — Bug 1 recovery.
//  4. The trailing wire `result` late-folds; Fix B settles the orphan.
//
// End state: attempt-1 partial thinking settled, attempt-2 thinking and
// the recovered synthesis text both completed, and NO row left streaming.
// Before the fixes the orphan stays streaming (the user's stuck spinner)
// even though the recovered content lands — exactly the fc24607e DB state
// plus the recovery the fixes add.
func TestCLIRetrySnapshotRecoversContentAndSettlesOrphan(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	const (
		retryThinking = "Reconsidering: both sources confirmed; the edge is non-canonical."
		synthesisText = "Both sources are in. Synthesis: the finding->vendor edge is non-canonical."
	)

	// 1. Turn 0: message X streams and the parent soft-closes (first settle).
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1",
		Content: "With the source confirmed, here is the first response.", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("message X delta: %v", err)
	}
	if err := router.settleStreamingScope("t1", ""); err != nil {
		t.Fatalf("settle message X: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: &provider.SoftRoundCloseMeta{StopReason: "end_turn"}, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("soft close: %v", err)
	}
	router.WaitForPendingSettles()

	// 2. Re-round (task_notification continuation): attempt-1 thinking
	// streams and stalls — no content_block_stop. Orphan at streaming.
	if err := router.Handle(parentContentBlockStart("t1", "thinking")); err != nil {
		t.Fatalf("re-round re-arm: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventThinking, ThreadID: "t1",
		Content: "Attempt-1 partial reasoning that stalls mid-stream.", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("attempt-1 orphan thinking: %v", err)
	}

	// 3. Attempt 2 arrives as a coalesced snapshot (fresh id, no stream
	// lifecycle). Fix A emits these as content-bearing EventContentBlockStop
	// events keyed on `message.id#ordinal`; feed them exactly as the parser
	// would (separate single-block envelopes → recovery ordinals 0, 1). The
	// explicit blockType + per-block ItemID makes triage persist each as a
	// NEW completed row (the never-streamed-recovery path).
	for _, blk := range []struct{ itemID, kind, content string }{
		{"msg_attempt2#0", "thinking", retryThinking},
		{"msg_attempt2#1", "text", synthesisText},
	} {
		if err := router.Handle(provider.ProviderEvent{
			Kind:           provider.EventContentBlockStop,
			ThreadID:       "t1",
			ItemID:         blk.itemID,
			Content:        blk.content,
			ContentPresent: true,
			Meta:           json.RawMessage(`{"blockType":"` + blk.kind + `"}`),
			Timestamp:      time.Now(),
		}); err != nil {
			t.Fatalf("recover %s: %v", blk.kind, err)
		}
	}
	router.WaitForPendingSettles()

	// 4. Trailing wire result late-folds; Fix B settles the orphan stream.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn", AssistantMessageID: "msg_attempt2"},
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("final result: %v", err)
	}
	router.WaitForPendingSettles()

	// No row may be left streaming — the direct encoding of "no stuck
	// thinking spinner".
	allItems, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	for _, it := range allItems {
		if it.Status == statusStreaming {
			t.Fatalf("Bug 2: item %s (%s) left at status=streaming after recovery+late-fold", it.ID, it.Kind)
		}
	}

	// The synthesis text was recovered as a completed assistant_text row
	// with the exact content (Bug 1 recovery — the lost agent response).
	var recoveredText *store.Item
	texts := findItemsByKind(t, st, "t1", itemKindAssistantText)
	for i := range texts {
		if texts[i].Summary == synthesisText {
			recoveredText = &texts[i]
		}
	}
	if recoveredText == nil {
		t.Fatalf("Bug 1: synthesis text never recovered as an assistant_text item")
	}
	if recoveredText.Status != statusCompleted {
		t.Errorf("recovered synthesis text status = %q, want %q", recoveredText.Status, statusCompleted)
	}

	// Both the orphaned attempt-1 thinking and the recovered attempt-2
	// thinking exist and are completed (>= 2 distinct thinking rows, none
	// streaming, which the loop above already guaranteed).
	thinking := findItemsByKind(t, st, "t1", itemKindThinking)
	if len(thinking) < 2 {
		t.Fatalf("expected >= 2 thinking rows (orphan + recovered), got %d", len(thinking))
	}
	for _, it := range thinking {
		if it.Status != statusCompleted {
			t.Errorf("thinking row %s status = %q, want %q", it.ID, it.Status, statusCompleted)
		}
	}
}
