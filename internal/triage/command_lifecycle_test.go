package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

const testCommandUUID = "3f6a1c9e-0000-4000-8000-000000000001"

func commandLifecycleEvent(threadID, commandUUID string, state provider.CommandLifecycleState) provider.ProviderEvent {
	meta, err := json.Marshal(provider.CommandLifecycleMeta{CommandUUID: commandUUID, State: state})
	if err != nil {
		panic(err)
	}
	return provider.ProviderEvent{
		Kind:      provider.EventCommandLifecycle,
		ThreadID:  threadID,
		ItemID:    commandUUID,
		Meta:      meta,
		Timestamp: time.Now(),
	}
}

func lastCommandLifecycleEmission(t *testing.T, emissions *emissionLog) CommandLifecycleEvent {
	t.Helper()
	var found *CommandLifecycleEvent
	for _, e := range emissions.snapshot() {
		if e.eventName != "provider:command_lifecycle" {
			continue
		}
		payload, ok := e.data.(CommandLifecycleEvent)
		if !ok {
			t.Fatalf("provider:command_lifecycle payload is %T, want CommandLifecycleEvent", e.data)
		}
		found = &payload
	}
	if found == nil {
		t.Fatal("no provider:command_lifecycle emission")
	}
	return *found
}

func countCommandLifecycleEmissions(emissions *emissionLog) int {
	var n int
	for _, e := range emissions.snapshot() {
		if e.eventName == "provider:command_lifecycle" {
			n++
		}
	}
	return n
}

// registerCommandPendingSend puts a pending send carrying the wire uuid in
// the FIFO, which is the state every real send path leaves behind BEFORE
// the stdin write the acks answer.
func registerCommandPendingSend(r *Router, threadID, itemID, commandUUID string, turnIndex int) {
	r.RegisterPendingSendExpecting(threadID, itemID, turnIndex, commandUUID)
}

func TestCommandLifecycle_QueuedResolvesUserItemID(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	registerCommandPendingSend(router, "t1", "user:0:flush:1", testCommandUUID, 1)

	if err := router.Handle(commandLifecycleEvent("t1", testCommandUUID, provider.CommandQueued)); err != nil {
		t.Fatalf("queued: %v", err)
	}
	got := lastCommandLifecycleEmission(t, emissions)
	if got.UserItemID != "user:0:flush:1" {
		t.Fatalf("UserItemID = %q, want user:0:flush:1", got.UserItemID)
	}
	if got.State != provider.CommandQueued {
		t.Fatalf("State = %q, want queued", got.State)
	}
	if got.Delivery != "" {
		t.Fatalf("Delivery = %q, want empty on queued", got.Delivery)
	}
}

// Queued while a wire round is open and started before that round closed:
// Claude drained the message into the running turn.
func TestCommandLifecycle_StartedInsideSameRoundIsMidTurn(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	openTestRound(t, router, "t1", 0)
	registerCommandPendingSend(router, "t1", "user:0:flush:1", testCommandUUID, 1)

	if err := router.Handle(commandLifecycleEvent("t1", testCommandUUID, provider.CommandQueued)); err != nil {
		t.Fatalf("queued: %v", err)
	}
	if err := router.Handle(commandLifecycleEvent("t1", testCommandUUID, provider.CommandStarted)); err != nil {
		t.Fatalf("started: %v", err)
	}

	got := lastCommandLifecycleEmission(t, emissions)
	if got.Delivery != CommandDeliveredMidTurn {
		t.Fatalf("Delivery = %q, want %q", got.Delivery, CommandDeliveredMidTurn)
	}
	if got.UserItemID != "user:0:flush:1" {
		t.Fatalf("UserItemID = %q, want user:0:flush:1", got.UserItemID)
	}
}

// Queued into a round that closed before pickup: the message ran as its
// own turn, even though a DIFFERENT round happens to be open by then.
func TestCommandLifecycle_StartedAfterRoundClosedIsNewTurn(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	openTestRound(t, router, "t1", 0)
	registerCommandPendingSend(router, "t1", "user:0:flush:1", testCommandUUID, 1)

	if err := router.Handle(commandLifecycleEvent("t1", testCommandUUID, provider.CommandQueued)); err != nil {
		t.Fatalf("queued: %v", err)
	}
	// The queued-into round ends and the message's own round opens.
	router.takeOpenRound("t1")
	openTestRound(t, router, "t1", 1)

	if err := router.Handle(commandLifecycleEvent("t1", testCommandUUID, provider.CommandStarted)); err != nil {
		t.Fatalf("started: %v", err)
	}
	if got := lastCommandLifecycleEmission(t, emissions).Delivery; got != CommandDeliveredNewTurn {
		t.Fatalf("Delivery = %q, want %q", got, CommandDeliveredNewTurn)
	}
}

// A send with no turn running is a new turn by definition — there is no
// round it could have been folded into.
func TestCommandLifecycle_StartedWithNoRoundAtQueueIsNewTurn(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	registerCommandPendingSend(router, "t1", "user:0", testCommandUUID, 0)

	if err := router.Handle(commandLifecycleEvent("t1", testCommandUUID, provider.CommandQueued)); err != nil {
		t.Fatalf("queued: %v", err)
	}
	openTestRound(t, router, "t1", 0)
	if err := router.Handle(commandLifecycleEvent("t1", testCommandUUID, provider.CommandStarted)); err != nil {
		t.Fatalf("started: %v", err)
	}
	if got := lastCommandLifecycleEmission(t, emissions).Delivery; got != CommandDeliveredNewTurn {
		t.Fatalf("Delivery = %q, want %q", got, CommandDeliveredNewTurn)
	}
}

// Arrival-order coverage: the wire echo pops the pending-send FIFO, and
// it can land BEFORE `started`. The correlation registered at `queued`
// must outlive that pop.
func TestCommandLifecycle_StartedAfterEchoConsumedPendingSend(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	openTestRound(t, router, "t1", 0)
	registerCommandPendingSend(router, "t1", "user:0:flush:1", testCommandUUID, 1)

	if err := router.Handle(commandLifecycleEvent("t1", testCommandUUID, provider.CommandQueued)); err != nil {
		t.Fatalf("queued: %v", err)
	}
	// Simulate the echo having consumed the FIFO entry.
	router.clearPendingSendsForThread("t1")

	if err := router.Handle(commandLifecycleEvent("t1", testCommandUUID, provider.CommandStarted)); err != nil {
		t.Fatalf("started: %v", err)
	}
	got := lastCommandLifecycleEmission(t, emissions)
	if got.UserItemID != "user:0:flush:1" {
		t.Fatalf("UserItemID = %q, want the row registered at queued time", got.UserItemID)
	}
	if got.Delivery != CommandDeliveredMidTurn {
		t.Fatalf("Delivery = %q, want %q", got.Delivery, CommandDeliveredMidTurn)
	}
}

// Reverse arrival order: `started` with no preceding `queued` (a frame
// lost to a reconnect, or a uuid from a previous process) still reports
// the state honestly, but must not guess the delivery classification.
func TestCommandLifecycle_StartedWithoutQueuedCarriesNoDelivery(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	openTestRound(t, router, "t1", 0)

	if err := router.Handle(commandLifecycleEvent("t1", testCommandUUID, provider.CommandStarted)); err != nil {
		t.Fatalf("started: %v", err)
	}
	got := lastCommandLifecycleEmission(t, emissions)
	if got.Delivery != "" {
		t.Fatalf("Delivery = %q, want empty without an enqueue-time round", got.Delivery)
	}
	if got.UserItemID != "" {
		t.Fatalf("UserItemID = %q, want empty", got.UserItemID)
	}
}

func TestCommandLifecycle_TerminalStatesReleaseCorrelation(t *testing.T) {
	for _, state := range []provider.CommandLifecycleState{provider.CommandCompleted, provider.CommandCancelled} {
		t.Run(string(state), func(t *testing.T) {
			router, st, emissions := newTestRouter(t)
			createTestThread(t, st, "t1")
			registerCommandPendingSend(router, "t1", "user:0:flush:1", testCommandUUID, 1)

			if err := router.Handle(commandLifecycleEvent("t1", testCommandUUID, provider.CommandQueued)); err != nil {
				t.Fatalf("queued: %v", err)
			}
			if err := router.Handle(commandLifecycleEvent("t1", testCommandUUID, state)); err != nil {
				t.Fatalf("%s: %v", state, err)
			}
			got := lastCommandLifecycleEmission(t, emissions)
			if got.State != state {
				t.Fatalf("State = %q, want %q", got.State, state)
			}
			if got.UserItemID != "user:0:flush:1" {
				t.Fatalf("UserItemID = %q, want the correlated row", got.UserItemID)
			}
			if _, ok := router.peekCommandLifecycle("t1", testCommandUUID); ok {
				t.Fatal("correlation still present after a terminal ack")
			}
		})
	}
}

// The no-ack baseline: a CLI that emits no lifecycle frames must leave
// every other routing decision untouched. Nothing to assert beyond "no
// emissions" — which is exactly the point.
func TestCommandLifecycle_NoAcksEmitNothing(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	registerCommandPendingSend(router, "t1", "user:0:flush:1", testCommandUUID, 1)

	if n := countCommandLifecycleEmissions(emissions); n != 0 {
		t.Fatalf("emissions = %d, want 0 without any ack", n)
	}
	if _, ok := router.peekCommandLifecycle("t1", testCommandUUID); ok {
		t.Fatal("correlation registered without an ack")
	}
}

func TestCommandLifecycle_CleanupThreadDropsCorrelation(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	registerCommandPendingSend(router, "t1", "user:0:flush:1", testCommandUUID, 1)
	if err := router.Handle(commandLifecycleEvent("t1", testCommandUUID, provider.CommandQueued)); err != nil {
		t.Fatalf("queued: %v", err)
	}

	router.CleanupThread("t1")
	if _, ok := router.peekCommandLifecycle("t1", testCommandUUID); ok {
		t.Fatal("correlation survived CleanupThread")
	}
}

// The cap protects against a CLI that stops sending terminal acks. Past
// it, new correlations are refused — the lifecycle detail degrades to the
// older-CLI baseline rather than growing router memory forever.
func TestCommandLifecycle_CorrelationMapIsBounded(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	for i := 0; i < maxCommandLifecycleEntriesPerThread+5; i++ {
		uuid := testCommandUUID + string(rune('a'+i%26)) + string(rune('a'+i/26))
		registerCommandPendingSend(router, "t1", "user:0:flush:1", uuid, 1)
		if err := router.Handle(commandLifecycleEvent("t1", uuid, provider.CommandQueued)); err != nil {
			t.Fatalf("queued %d: %v", i, err)
		}
	}
	router.mu.Lock()
	size := len(router.commandLifecycle["t1"])
	router.mu.Unlock()
	if size > maxCommandLifecycleEntriesPerThread {
		t.Fatalf("correlation map size = %d, want <= %d", size, maxCommandLifecycleEntriesPerThread)
	}
}

// A `queued` ack must never consume the pending-send entry: only the wire
// echo may pop it, and popping here would strand the row unconfirmed.
func TestCommandLifecycle_QueuedDoesNotConsumePendingSend(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	registerCommandPendingSend(router, "t1", "user:0:flush:1", testCommandUUID, 1)

	if err := router.Handle(commandLifecycleEvent("t1", testCommandUUID, provider.CommandQueued)); err != nil {
		t.Fatalf("queued: %v", err)
	}
	if !router.HasPendingSendForThread("t1") {
		t.Fatal("pending send consumed by a lifecycle ack")
	}
}

func openTestRound(t *testing.T, r *Router, threadID string, turnIndex int) {
	t.Helper()
	r.setOpenRoundSnapshot(ActiveTurnSnapshot{
		ThreadID:  threadID,
		TurnID:    "round-" + threadID + "-" + string(rune('0'+turnIndex)),
		TurnIndex: turnIndex,
		StartedAt: time.Now().UnixMilli(),
	})
}
