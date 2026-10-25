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

// TestHandleInit_NoPendingSend_PriorTurnSettled_OpensProviderStartedTurn
// pins the self-wake: after a turn settles, an init no AO send started is
// a CLI turn the provider started (a background-task notification woke
// it), so it opens the NEXT logical turn with its own turns row, open-turn
// state and round. Rows it streams are that turn's, never the settled
// turn's.
func TestHandleInit_NoPendingSend_PriorTurnSettled_OpensProviderStartedTurn(t *testing.T) {
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
		t.Fatalf("turn complete: %v", err)
	}
	emissions.reset()

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventInit,
		ThreadID:  "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle self-wake init: %v", err)
	}

	starts := filterEmissions(emissions.snapshot(), "provider:turn_started")
	if len(starts) != 1 {
		t.Fatalf("expected 1 provider:turn_started for the provider-started turn, got %d", len(starts))
	}
	payload, ok := starts[0].data.(TurnStartedEvent)
	if !ok {
		t.Fatalf("payload type = %T, want TurnStartedEvent", starts[0].data)
	}
	if payload.TurnIndex != 1 {
		t.Errorf("provider-started turn index = %d, want 1 (a turn of its own)", payload.TurnIndex)
	}
	if got := router.OpenTurnIndex("t1"); got != 1 {
		t.Errorf("OpenTurnIndex = %d, want 1", got)
	}
	active, found, err := st.GetActiveTurn("t1")
	if err != nil {
		t.Fatalf("GetActiveTurn: %v", err)
	}
	if !found || active.TurnIndex != 1 {
		t.Errorf("GetActiveTurn = (%+v, %v), want turn 1 open", active, found)
	}
}

// TestHandleInit_InternalCommandOpensNoTurn: AO's own `/effort`, `/fast`
// and `/rename` run as CLI turns of their own (lifecycle started → init →
// synthetic output → result). They are AO bookkeeping, not conversation,
// so their init opens nothing even after a settled turn.
func TestHandleInit_InternalCommandOpensNoTurn(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}
	emissions.reset()

	meta, err := json.Marshal(provider.SessionInfo{TurnOpener: provider.TurnOpenerInternalCommand})
	if err != nil {
		t.Fatal(err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventInit, ThreadID: "t1", Meta: meta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle internal-command init: %v", err)
	}
	if starts := filterEmissions(emissions.snapshot(), "provider:turn_started"); len(starts) != 0 {
		t.Errorf("internal command init emitted %d provider:turn_started, want 0", len(starts))
	}
	if got := router.OpenTurnIndex("t1"); got != -1 {
		t.Errorf("OpenTurnIndex = %d, want -1", got)
	}
	if _, found, err := st.GetActiveTurn("t1"); err != nil || found {
		t.Errorf("GetActiveTurn found=%v err=%v, want no active turn", found, err)
	}
}

// TestHandleInit_PeerTurnOnFreshSessionOpensTurn: a turn another session's
// message started carries a command opener, so it opens a turn even
// before this session settled one. With no opener the same init is an
// idle attach and opens nothing (TestHandleInit_NoPendingSend_NoSettledTurn_NoEmission).
func TestHandleInit_PeerTurnOnFreshSessionOpensTurn(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	meta, err := json.Marshal(provider.SessionInfo{TurnOpener: provider.TurnOpenerCommand})
	if err != nil {
		t.Fatal(err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventInit, ThreadID: "t1", Meta: meta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle peer init: %v", err)
	}
	if starts := filterEmissions(emissions.snapshot(), "provider:turn_started"); len(starts) != 1 {
		t.Fatalf("peer init emitted %d provider:turn_started, want 1", len(starts))
	}
	if got := router.OpenTurnIndex("t1"); got != 0 {
		t.Errorf("OpenTurnIndex = %d, want 0", got)
	}
}

// TestHandleInit_PendingSendPlusSettledTurn_PrefersTurnStartPath pins
// the precedence rule: when both a pending-send marker AND a settled
// prior turn are present, handleInit must choose the handleTurnStart
// path (the send's turn at its stamped index), not a provider-started
// turn.
//
// The discriminator is the opened index: handleTurnStart opens the
// send's stamped turn 1, while openProviderStartedTurn would take
// NextTurnIndex past the persisted user row (turn 2). The frontend
// turn_started payload carries the same index, so the frontend opens a
// fresh row for the send's turn rather than re-lighting the settled one.
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
	// persisted under the send's turnIndex BEFORE the wire init arrives.
	// handleTurnStart opens the index the pending send carries; a
	// provider-started turn would take NextTurnIndex past this row.
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

	// handleTurnStart opens the send's stamped index; a provider-started
	// turn would open past the persisted user row, so the index is the
	// signal that distinguishes the two paths.
	router.mu.Lock()
	openState := router.threadStateIfPresent("t1")
	openIdx, openOK := openState.openTurn, openState.openTurnSet
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
		t.Errorf("payload.TurnIndex = %d, want 1 (handleTurnStart opens the pending send's stamped turn)", payload.TurnIndex)
	}
}
