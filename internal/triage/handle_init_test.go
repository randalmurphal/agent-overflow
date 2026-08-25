package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

func initEventWithSessionID(threadID, sessionID string) provider.ProviderEvent {
	meta, err := json.Marshal(provider.SessionInfo{SessionID: sessionID})
	if err != nil {
		panic(err)
	}
	return provider.ProviderEvent{
		Kind:      provider.EventInit,
		ThreadID:  threadID,
		Meta:      meta,
		Timestamp: time.Now(),
	}
}

func threadPatchSessionRefs(emissions *emissionLog) []string {
	var out []string
	for _, e := range filterEmissions(emissions.snapshot(), "thread:updated") {
		payload, ok := e.data.(ThreadUpdateEvent)
		if !ok || payload.Action != "patch" || payload.SessionRef == nil {
			continue
		}
		out = append(out, *payload.SessionRef)
	}
	return out
}

// TestHandleInit_SessionRefAssignment_EmitsThreadPatch pins the
// sessionRef push: the sidebar's fork affordance gates on the cached
// row's sessionRef, and handleInit's UpdateSessionRef is the only
// writer during a session — so the assignment must announce itself as
// a thread:updated patch, exactly once per actual change. A resumed
// session restating the same id on every init stays silent (the
// duplicate would be harmless but noisy), and a moved ref announces
// again.
func TestHandleInit_SessionRefAssignment_EmitsThreadPatch(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(initEventWithSessionID("t1", "session-a")); err != nil {
		t.Fatalf("init: %v", err)
	}
	refs := threadPatchSessionRefs(emissions)
	if len(refs) != 1 || refs[0] != "session-a" {
		t.Fatalf("sessionRef patches after first init = %v, want [session-a]", refs)
	}

	// Same session id on a later init (resume): no re-announcement.
	if err := router.Handle(initEventWithSessionID("t1", "session-a")); err != nil {
		t.Fatalf("re-init same session: %v", err)
	}
	if refs := threadPatchSessionRefs(emissions); len(refs) != 1 {
		t.Fatalf("sessionRef patches after restated init = %v, want no new emission", refs)
	}

	// The ref moved (e.g. a fresh session after a rollback): announce again.
	if err := router.Handle(initEventWithSessionID("t1", "session-b")); err != nil {
		t.Fatalf("init with new session: %v", err)
	}
	refs = threadPatchSessionRefs(emissions)
	if len(refs) != 2 || refs[1] != "session-b" {
		t.Fatalf("sessionRef patches after moved ref = %v, want [session-a session-b]", refs)
	}
	patches := filterEmissions(emissions.snapshot(), "thread:updated")
	if payload, ok := patches[0].data.(ThreadUpdateEvent); !ok || payload.ID != "t1" {
		t.Fatalf("patch payload = %+v, want ID t1", patches[0].data)
	}
}

// TestHandleInit_PendingSendPresent_FiresHandleTurnStart pins the
// Phase F wiring for fresh AO sends: when triage.handleInit sees a
// pending-send marker for the thread, it routes through handleTurnStart
// (the same handler the synthetic EventTurnStart used to invoke). The
// observable effect is a provider:turn_started emission with a per-round
// uuid as TurnID. Without this routing, Claude turns would never open on
// the frontend post-Phase-F because the synthetic EventTurnStart has
// been deleted.
func TestHandleInit_PendingSendPresent_FiresHandleTurnStart(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Mirror the send path: register the pending-send before the wire
	// init arrives.
	router.RegisterPendingSendWithExpectation("t1", "user:0", 0, PendingSendExpectation{})

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventInit,
		ThreadID:  "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle init with pending send: %v", err)
	}

	starts := filterEmissions(emissions.snapshot(), "provider:turn_started")
	if len(starts) != 1 {
		t.Fatalf("expected 1 provider:turn_started emission, got %d (handleInit pending-send branch must run handleTurnStart)", len(starts))
	}
	payload, ok := starts[0].data.(TurnStartedEvent)
	if !ok {
		t.Fatalf("emission payload type = %T, want TurnStartedEvent", starts[0].data)
	}
	if payload.TurnID == "" {
		t.Errorf("payload.TurnID is empty — handleTurnStart must allocate a per-round uuid")
	}
	if payload.ThreadID != "t1" {
		t.Errorf("payload.ThreadID = %q, want t1", payload.ThreadID)
	}
}

// TestHandleInit_PendingSendPresent_DoesNotConsumeIt locks in the
// invariant that handleInit only GATES on the pending-send marker — it
// does not pop it. The pop happens in handleUserText when the matching
// replay envelope arrives. Popping here would race the wire user-text
// envelope and either leave a stranded marker (if pop here) or leak a
// double-consume into handleUserText (if the order swaps), breaking the
// AO/wire correlation.
func TestHandleInit_PendingSendPresent_DoesNotConsumeIt(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	router.RegisterPendingSendWithExpectation("t1", "user:0", 0, PendingSendExpectation{})

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventInit,
		ThreadID:  "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle init: %v", err)
	}

	if !router.HasPendingSendForThread("t1") {
		t.Fatal("handleInit must NOT consume the pending-send marker — pop is owned by handleUserText")
	}
	head, ok := router.consumeMatchingPendingSend("t1", "")
	if !ok {
		t.Fatalf("expected the pending-send entry to still be poppable after handleInit")
	}
	if head.AOItemID != "user:0" || head.TurnIndex != 0 {
		t.Errorf("consumed entry = %+v, want {user:0, turnIndex=0} (FIFO head intact)", head)
	}
}

// TestHandleInit_NoPendingSend_NoSettledTurn_NoEmission pins the idle
// session-attach case: the app reattaches to a thread (or a fresh
// session boots) with no AO send in flight and no prior settled turn
// for this thread. Both gates fail, so handleInit emits nothing. A
// false-positive emission here would lie to the frontend about the
// model being engaged when it isn't (regression mode for "blinking
// indicator on every reconnect").
func TestHandleInit_NoPendingSend_NoSettledTurn_NoEmission(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventInit,
		ThreadID:  "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle init on idle thread: %v", err)
	}

	starts := filterEmissions(emissions.snapshot(), "provider:turn_started")
	if len(starts) != 0 {
		t.Errorf("expected 0 provider:turn_started for idle attach (no pending-send, no settled turn), got %d: %+v", len(starts), starts)
	}
}

// TestHandleInit_NoPendingSend_PriorTurnSettled_FiresReRound locks in
// the cascade re-round path post-Phase-F. With no pending-send marker,
// handleInit falls through to maybeEmitReRoundOnInit; a settled prior
// turn unlocks the re-round emission so the frontend re-lights its
// working indicator for the second result envelope in the
// multi-result-per-turn cascade. Without this fallback, Claude's
// task_notification → CLI-synthesized user envelope → second result
// pattern would leave the frontend in "complete" state for the second
// half of the cascade.
func TestHandleInit_NoPendingSend_PriorTurnSettled_FiresReRound(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Open and complete a logical turn so settledTurns marks t1:0.
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
		t.Fatalf("turn complete: %v", err)
	}
	emissions.reset() // discard round-1 turn_started/turn_completed

	// No pending-send: handleInit must fall through to
	// maybeEmitReRoundOnInit and emit a fresh round.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventInit,
		ThreadID:  "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle re-init: %v", err)
	}

	starts := filterEmissions(emissions.snapshot(), "provider:turn_started")
	if len(starts) != 1 {
		t.Fatalf("expected 1 provider:turn_started emission via re-round path, got %d", len(starts))
	}
	payload, ok := starts[0].data.(TurnStartedEvent)
	if !ok {
		t.Fatalf("payload type = %T, want TurnStartedEvent", starts[0].data)
	}
	if payload.TurnID == "" {
		t.Errorf("re-round payload.TurnID is empty")
	}
	if payload.TurnIndex != 0 {
		t.Errorf("re-round payload.TurnIndex = %d, want 0 (same logical turn)", payload.TurnIndex)
	}
}

// TestHandleInit_PendingSendPlusSettledTurn_PrefersTurnStartPath pins
// the precedence rule: when both a pending-send marker AND a settled
// prior turn are present (an AO send launched during the cascade settle
// window), handleInit must choose the handleTurnStart path — round 1
// of a NEW logical turn — not the re-round path on the prior turn.
//
// The discriminator: handleTurnStart sets `openTurns[t1]` via
// setOpenTurn; the re-round path explicitly does NOT (per the
// load-bearing invariant in internal/triage/CLAUDE.md "setOpenTurn
// does NOT fire from handleInit"). The presence of an entry in
// openTurns is the wire-typed signal that handleTurnStart ran. The
// frontend turn_started payload also carries the new turnIndex —
// 1, not 0 — so the frontend opens a fresh row for the new logical
// turn rather than re-lighting the settled one.
func TestHandleInit_PendingSendPlusSettledTurn_PrefersTurnStartPath(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Settle round 1 of turn 0 so settledTurns[t1:0] = true.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t1",
		TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start (turn 0): %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("turn complete (turn 0): %v", err)
	}
	emissions.reset()

	// Mirror the production sequence in app_send.go: the AO user item is
	// persisted under the new turnIndex BEFORE the wire init arrives.
	// handleTurnStart's LastTurnIndex fallback then resolves the
	// turnIndex to the user item's row, not the prior settled turn's
	// row. (Without this, the handleTurnStart fallback would land on
	// turn 0 and re-stamp the settled row instead of opening turn 1.)
	now := time.Now().UnixMilli()
	userItem := store.Item{
		ID:        "user:1",
		ThreadID:  "t1",
		TurnIndex: 1,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "Implement",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := router.PersistItem(userItem, nil); err != nil {
		t.Fatalf("persist user item for turn 1: %v", err)
	}

	// Now AO sends turn 1 — pending-send is registered for turn 1
	// while settledTurns[t1:0] is still set. handleInit must take the
	// handleTurnStart path so the new logical turn opens correctly.
	router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{})

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventInit,
		ThreadID:  "t1",
		Timestamp: time.UnixMilli(now + 1),
	}); err != nil {
		t.Fatalf("handle init: %v", err)
	}

	// handleTurnStart calls setOpenTurn(threadID, turnIndex). The re-round
	// path explicitly does NOT, so this assertion is the load-bearing
	// distinguishing signal between the two paths.
	router.mu.Lock()
	openIdx, openOK := router.openTurns["t1"]
	router.mu.Unlock()
	if !openOK {
		t.Fatal("openTurns[t1] not set — handleTurnStart's setOpenTurn must have run")
	}
	if openIdx != 1 {
		t.Errorf("openTurns[t1] = %d, want 1 (handleTurnStart opens the new logical turn, not the prior settled one)", openIdx)
	}

	// Frontend emission shape: a per-round uuid for the new logical turn.
	starts := filterEmissions(emissions.snapshot(), "provider:turn_started")
	if len(starts) != 1 {
		t.Fatalf("expected 1 provider:turn_started, got %d", len(starts))
	}
	payload, ok := starts[0].data.(TurnStartedEvent)
	if !ok {
		t.Fatalf("payload type = %T, want TurnStartedEvent", starts[0].data)
	}
	if payload.TurnIndex != 1 {
		t.Errorf("payload.TurnIndex = %d, want 1 (handleTurnStart resolves the new logical turn via LastTurnIndex over the freshly-persisted user row)", payload.TurnIndex)
	}
}
