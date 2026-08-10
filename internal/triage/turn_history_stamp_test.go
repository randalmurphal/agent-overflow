package triage

import (
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

func historyStamp(t *testing.T, st *store.Store, threadID string) store.HistoryStamp {
	t.Helper()
	stamp, found, err := st.ThreadHistoryStamp(threadID)
	if err != nil {
		t.Fatalf("thread history stamp: %v", err)
	}
	if !found {
		t.Fatalf("thread %s has no row", threadID)
	}
	return stamp
}

// TestTurnCompletedEventCarriesHistoryStamps pins the §4 contract on the
// turn-completion frame: the stamps it carries must cover every item
// event already on the wire, and must never run ahead of what the store
// actually holds. Both halves are asserted as a sandwich around the
// emission — a stamp below the pre-completion read would let a client
// mark a replica current while missing rows it was just sent, and a
// stamp above the post-settle read would do the same for rows nobody has
// written yet.
func TestTurnCompletedEventCarriesHistoryStamps(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "an answer",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}
	if err := router.settleStreamingScope("t1", ""); err != nil {
		t.Fatalf("settle streaming scope: %v", err)
	}

	// Everything the completion frame must cover has been written and
	// emitted by this point.
	covered := historyStamp(t, st, "t1")
	if covered.Rev == 0 {
		t.Fatal("fixture wrote no items: the assertion below would be vacuous")
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}
	router.WaitForPendingSettles()
	settled := historyStamp(t, st, "t1")

	completions := filterEmissions(emissions.snapshot(), "provider:turn_completed")
	if len(completions) != 1 {
		t.Fatalf("expected 1 provider:turn_completed, got %d", len(completions))
	}
	payload, ok := completions[0].data.(TurnCompletedEvent)
	if !ok {
		t.Fatalf("payload type: got %T, want TurnCompletedEvent", completions[0].data)
	}
	if payload.HistoryRev < covered.Rev {
		t.Fatalf("HistoryRev = %d, below the %d that covers the items already emitted",
			payload.HistoryRev, covered.Rev)
	}
	if payload.HistoryRev > settled.Rev {
		t.Fatalf("HistoryRev = %d, ahead of the store's %d — the client would mark a replica current for writes that do not exist",
			payload.HistoryRev, settled.Rev)
	}
	if payload.HistoryEpoch != settled.Epoch {
		t.Fatalf("HistoryEpoch = %d, want %d (a turn completion deletes and repositions nothing)",
			payload.HistoryEpoch, settled.Epoch)
	}
}

// TestTurnCompletedEventStampsVanishedThreadAsZero covers the failure
// direction: the thread row is gone by the time the round closes (the
// user deleted the thread mid-turn). The frame must still emit, with the
// zero stamp. Zero is below every real stamp, so a client treats it as
// "know nothing" and re-syncs — the safe answer. Carrying whatever the
// last successful read returned would not be.
func TestTurnCompletedEventStampsVanishedThreadAsZero(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	if err := st.DeleteThread("t1"); err != nil {
		t.Fatalf("delete thread: %v", err)
	}

	// The completion still has to reach the frontend: it is what clears
	// the working indicator. Its store writes fail with the row gone, so
	// an error return here is expected and not what this test is about.
	_ = router.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	})

	completions := filterEmissions(emissions.snapshot(), "provider:turn_completed")
	if len(completions) != 1 {
		t.Fatalf("expected 1 provider:turn_completed, got %d", len(completions))
	}
	payload, ok := completions[0].data.(TurnCompletedEvent)
	if !ok {
		t.Fatalf("payload type: got %T, want TurnCompletedEvent", completions[0].data)
	}
	if payload.HistoryRev != 0 || payload.HistoryEpoch != 0 {
		t.Fatalf("stamps for an unknown thread = (%d, %d), want (0, 0)",
			payload.HistoryRev, payload.HistoryEpoch)
	}
}
