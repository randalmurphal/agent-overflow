package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// TestSessionStatusErrorPersistsAndSynthesizesTurnComplete pins the
// promotion of EventSessionStatus{"error"} into three loosely-coupled
// outputs: a session_died notification row, a typed
// provider:session_died emission, and (when a turn is open) a
// synthesized truncated EventTurnComplete that clears the frontend
// working indicator without a wire turn-complete.
func TestSessionStatusErrorPersistsAndSynthesizesTurnComplete(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Open a turn so the synthesis branch fires.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t1",
		TurnID:    "turn-1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn-start: %v", err)
	}
	emissions.reset()

	exitMeta, _ := json.Marshal(provider.ProcessExitInfo{
		Reason:     "provider process exited unexpectedly",
		ExitCode:   1,
		StderrTail: "error: unknown option '--thinking-display'",
	})
	evt := provider.ProviderEvent{
		Kind:      provider.EventSessionStatus,
		ThreadID:  "t1",
		Content:   "error",
		Meta:      exitMeta,
		Timestamp: time.Now(),
	}
	if err := router.Handle(evt); err != nil {
		t.Fatalf("session-status error: %v", err)
	}

	// 1. Notification row persisted with kind=session_died.
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var sawNotif bool
	for _, it := range items {
		if it.Kind != "notification" {
			continue
		}
		var meta map[string]any
		_ = json.Unmarshal([]byte(it.Meta), &meta)
		if meta["kind"] != sessionDiedNotificationKind {
			continue
		}
		sawNotif = true
		// Reason must round-trip onto meta so the frontend can render
		// the exit detail without re-decoding the wire envelope.
		if got := meta["exitCode"]; got != float64(1) {
			t.Fatalf("notification meta.exitCode: got %v, want 1", got)
		}
		// The captured stderr tail rides along so the timeline row can
		// show the actual failure text, not just the exit code.
		if got := meta["stderrTail"]; got != "error: unknown option '--thinking-display'" {
			t.Fatalf("notification meta.stderrTail: got %v", got)
		}
	}
	if !sawNotif {
		t.Fatalf("expected a notification item with kind=session_died, got %+v", items)
	}

	// 2. provider:session_died event emitted with ThreadID + ExitCode.
	died := filterEmissions(emissions.snapshot(), "provider:session_died")
	if len(died) != 1 {
		t.Fatalf("expected 1 provider:session_died, got %d (%+v)", len(died), emissions.snapshot())
	}
	deathPayload, ok := died[0].data.(SessionDiedEvent)
	if !ok {
		t.Fatalf("payload type: got %T, want SessionDiedEvent", died[0].data)
	}
	if deathPayload.ThreadID != "t1" || deathPayload.ExitCode != 1 {
		t.Fatalf("payload: got %+v", deathPayload)
	}
	if deathPayload.StderrTail != "error: unknown option '--thinking-display'" {
		t.Fatalf("payload.StderrTail: got %q", deathPayload.StderrTail)
	}

	// 3. Synthesized truncated turn-complete reached the frontend.
	completed := filterEmissions(emissions.snapshot(), "provider:turn_completed")
	if len(completed) != 1 {
		t.Fatalf("expected 1 provider:turn_completed (truncated synthesis), got %d (%+v)", len(completed), emissions.snapshot())
	}
}

// TestSessionStatusErrorWithoutOpenTurnSkipsSynthesis covers the
// no-active-turn case: a session_died with no in-flight turn must
// still emit the banner + persist the row, but not synthesize a
// turn-complete (there's no turn to close).
func TestSessionStatusErrorWithoutOpenTurnSkipsSynthesis(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	exitMeta, _ := json.Marshal(provider.ProcessExitInfo{
		Reason: "killed by SIGKILL",
		Signal: "killed",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventSessionStatus,
		ThreadID:  "t1",
		Content:   "error",
		Meta:      exitMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("session-status error: %v", err)
	}

	died := filterEmissions(emissions.snapshot(), "provider:session_died")
	if len(died) != 1 {
		t.Fatalf("expected provider:session_died emission")
	}
	completed := filterEmissions(emissions.snapshot(), "provider:turn_completed")
	if len(completed) != 0 {
		t.Fatalf("expected no synthesized turn_completed without open turn, got %d", len(completed))
	}

	// Persist-side: the no-open-turn branch must still write the
	// notification row so the chat history records the death even
	// when there's no active turn to close.
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var sawNotif bool
	for _, it := range items {
		if it.Kind == "notification" {
			var meta map[string]any
			_ = json.Unmarshal([]byte(it.Meta), &meta)
			if meta["kind"] == sessionDiedNotificationKind {
				sawNotif = true
			}
		}
	}
	if !sawNotif {
		t.Fatalf("expected a notification row with kind=session_died even without open turn, got %+v", items)
	}
}

// TestSessionStatusErrorIsIdempotent pins the contract that two
// identical EventSessionStatus{"error"} events do NOT produce
// duplicate timeline rows or duplicate banner emissions. This is the
// guarantee handleSessionDied's documentation makes; the deterministic
// `session_died:<turnIndex>` id and the wasNew gate on the typed
// emission together enforce it. A regression here would re-introduce
// the "two banners + two rows" bug visible after a slow exit
// followed by stdout drain replays the same death wire-event.
func TestSessionStatusErrorIsIdempotent(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t1",
		TurnID:    "turn-1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn-start: %v", err)
	}
	emissions.reset()

	exitMeta, _ := json.Marshal(provider.ProcessExitInfo{
		Reason:   "provider process exited unexpectedly",
		ExitCode: 1,
	})
	died := provider.ProviderEvent{
		Kind:      provider.EventSessionStatus,
		ThreadID:  "t1",
		Content:   "error",
		Meta:      exitMeta,
		Timestamp: time.Now(),
	}
	for i := 0; i < 2; i++ {
		if err := router.Handle(died); err != nil {
			t.Fatalf("session-status error #%d: %v", i+1, err)
		}
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var notifCount int
	for _, it := range items {
		if it.Kind != "notification" {
			continue
		}
		var meta map[string]any
		_ = json.Unmarshal([]byte(it.Meta), &meta)
		if meta["kind"] == sessionDiedNotificationKind {
			notifCount++
		}
	}
	if notifCount != 1 {
		t.Fatalf("expected exactly 1 session_died notification row, got %d (%+v)", notifCount, items)
	}

	emitted := filterEmissions(emissions.snapshot(), "provider:session_died")
	if len(emitted) != 1 {
		t.Fatalf("expected exactly 1 provider:session_died emission, got %d", len(emitted))
	}
}

// TestCleanupThreadSynthesizesTruncatedTurnComplete pins the safety
// net for "session ended cleanly mid-turn without a wire
// turn-complete" — CleanupThread spots the still-open turn and
// synthesizes the truncated turn-complete before tearing down state
// so the frontend's working indicator never gets stranded.
//
// The test pins both projections of the synthesis: the
// frontend-visible TurnCompletedEvent (aborted=true) and the raw
// synthesized ProviderEvent (provider.TruncatedTurnCompleteMeta{Synthetic:true})
// via the event hook. CleanupThread is the safety-net path; emitting a
// non-truncated turn-complete here would mislead the frontend into
// rendering the turn as a clean stop.
func TestCleanupThreadSynthesizesTruncatedTurnComplete(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	observed := make(chan provider.ProviderEvent, 8)
	router.SetEventHook(func(evt provider.ProviderEvent) {
		observed <- evt
	})

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t1",
		TurnID:    "turn-1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn-start: %v", err)
	}
	emissions.reset()

	router.CleanupThread("t1")

	completed := filterEmissions(emissions.snapshot(), "provider:turn_completed")
	if len(completed) != 1 {
		t.Fatalf("expected 1 truncated turn_completed from CleanupThread, got %d (%+v)", len(completed), emissions.snapshot())
	}
	payload, ok := completed[0].data.(TurnCompletedEvent)
	if !ok {
		t.Fatalf("payload type: got %T, want TurnCompletedEvent", completed[0].data)
	}
	if !payload.Aborted {
		t.Fatalf("expected aborted=true on truncated synthesis, got %+v", payload)
	}

	close(observed)
	var sawSynth bool
	for evt := range observed {
		if evt.Kind != provider.EventTurnComplete {
			continue
		}
		meta, ok := evt.TurnComplete.(*provider.TruncatedTurnCompleteMeta)
		if ok && meta != nil && meta.Synthetic {
			sawSynth = true
		}
	}
	if !sawSynth {
		t.Fatal("expected a synthesized TurnComplete from CleanupThread")
	}
}

// TestCleanupThreadWithoutOpenTurnIsNoop pins the negative branch:
// CleanupThread on a thread with no in-flight turn must NOT synthesize
// a phantom turn_completed event.
func TestCleanupThreadWithoutOpenTurnIsNoop(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	router.CleanupThread("t1")

	completed := filterEmissions(emissions.snapshot(), "provider:turn_completed")
	if len(completed) != 0 {
		t.Fatalf("expected no turn_completed synthesis without open turn, got %d", len(completed))
	}
}

// TestCleanupThreadSynthesizesAfterRound2PlusReRound pins the
// regression that left the FE working indicator stranded when a Claude
// session died mid-round-2+: handleInit's re-round branch sets
// currentRoundByThread WITHOUT calling setOpenTurn (id-allocating
// counters must survive the multi-result boundary — see
// multi_result_test.go for the rationale). Before the fix,
// CleanupThread's guard checked openTurnIndex only, missed the live
// round, and skipped synthesize; afterwards delete(currentRoundByThread)
// ran anyway, leaving the FE with a stuck activeTurnByThread entry
// and no path to a wire turn_completed.
//
// The fix gates synthesize on hasInFlightTurnOrRound (openTurns OR
// currentRoundByThread). This test models the wire sequence:
//
//	EventTurnStart    (round 1 begins; openTurns + currentRoundByThread set)
//	EventTurnComplete (round 1 ends; both cleared)
//	EventInit         (round 2 begins via maybeEmitReRoundOnInit;
//	                   currentRoundByThread set, openTurns stays empty)
//	<session dies>
//	CleanupThread     (must synthesize because round 2 is open)
func TestCleanupThreadSynthesizesAfterRound2PlusReRound(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t1",
		TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("round 1 complete: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventInit,
		ThreadID:  "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("re-init: %v", err)
	}

	// Sanity-check the precondition: openTurns is empty (clearOpenTurn
	// cleared it at the round-1 complete) but currentRoundByThread is
	// set (maybeEmitReRoundOnInit just opened round 2). This is the
	// state the bug fix targets — if either side drifts, the regression
	// guard would silently pass on a different code path.
	if _, ok := router.openTurnIndex("t1"); ok {
		t.Fatal("precondition: openTurns must be empty after round-1 complete")
	}
	if !router.hasInFlightTurnOrRound("t1") {
		t.Fatal("precondition: round 2 must be live in currentRoundByThread")
	}
	emissions.reset()

	router.CleanupThread("t1")

	completed := filterEmissions(emissions.snapshot(), "provider:turn_completed")
	if len(completed) != 1 {
		t.Fatalf("expected 1 truncated turn_completed for round 2, got %d (%+v)", len(completed), emissions.snapshot())
	}
	payload, ok := completed[0].data.(TurnCompletedEvent)
	if !ok {
		t.Fatalf("payload type: got %T, want TurnCompletedEvent", completed[0].data)
	}
	if !payload.Aborted {
		t.Fatalf("expected aborted=true on truncated synthesis, got %+v", payload)
	}
}

// TestSessionStatusErrorSynthesizesAfterRound2PlusReRound is the
// session-died (rather than CleanupThread) variant of the same gate
// regression. handleSessionDied shared the openTurnIndex-only guard
// with CleanupThread; the same hasInFlightTurnOrRound fix covers it.
func TestSessionStatusErrorSynthesizesAfterRound2PlusReRound(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t1",
		TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("round 1 complete: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventInit,
		ThreadID:  "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("re-init: %v", err)
	}
	emissions.reset()

	exitMeta, _ := json.Marshal(provider.ProcessExitInfo{
		Reason: "provider process exited unexpectedly",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventSessionStatus,
		ThreadID:  "t1",
		Content:   "error",
		Meta:      exitMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("session-status error: %v", err)
	}

	completed := filterEmissions(emissions.snapshot(), "provider:turn_completed")
	if len(completed) != 1 {
		t.Fatalf("expected 1 truncated turn_completed for round 2, got %d (%+v)", len(completed), emissions.snapshot())
	}
	payload, ok := completed[0].data.(TurnCompletedEvent)
	if !ok {
		t.Fatalf("payload type: got %T, want TurnCompletedEvent", completed[0].data)
	}
	if !payload.Aborted {
		t.Fatalf("expected aborted=true on truncated synthesis, got %+v", payload)
	}
}
