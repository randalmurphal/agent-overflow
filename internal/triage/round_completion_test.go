package triage

import (
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
)

// roundEnds pairs every provider:turn_started with its
// provider:turn_completed by round id and reports the rounds left open.
func roundEnds(t *testing.T, emissions []emitted) (open []string, completed []TurnCompletedEvent) {
	t.Helper()
	started := map[string]bool{}
	for _, e := range emissions {
		switch e.eventName {
		case "provider:turn_started":
			started[e.data.(TurnStartedEvent).TurnID] = true
		case "provider:turn_completed":
			done := e.data.(TurnCompletedEvent)
			delete(started, done.TurnID)
			completed = append(completed, done)
		}
	}
	for id := range started {
		open = append(open, id)
	}
	return open, completed
}

// A soft close settles the turn; a continuation with no init (a ToolSearch
// or ExitPlanMode resume) reopens it for a second round, and the next soft
// close settles it again. The turns row's completed_at is never before a
// round's announced completion, so a read stamped at the row's completion
// covers every round a client saw end.
func TestContinuationReopensSoftClosedTurn(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	base := time.UnixMilli(1_700_000_000_000)
	handle := func(evt provider.ProviderEvent, at time.Duration) {
		t.Helper()
		evt.ThreadID = "t1"
		evt.Timestamp = base.Add(at)
		if err := router.Handle(evt); err != nil {
			t.Fatalf("%s: %v", evt.Kind, err)
		}
	}

	turnRow := func() store.Turn {
		t.Helper()
		turn, found, err := st.GetTurn("t1:0")
		if err != nil || !found {
			t.Fatalf("turn row: found=%v err=%v", found, err)
		}
		return turn
	}

	handle(provider.ProviderEvent{Kind: provider.EventTurnStart}, 0)
	handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: &provider.SoftRoundCloseMeta{StopReason: "end_turn"}}, time.Second)
	if turn := turnRow(); turn.CompletedAt == nil || *turn.CompletedAt != base.Add(time.Second).UnixMilli() {
		t.Fatalf("turn row after the soft close: %+v, want settled at it", turn)
	}
	handle(parentContentBlockStart("t1", "text"), 2*time.Second)
	if turn := turnRow(); turn.CompletedAt != nil {
		t.Fatalf("turn row during the continuation: completed_at %d, want reopened", *turn.CompletedAt)
	}
	handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: &provider.SoftRoundCloseMeta{StopReason: "end_turn"}}, 3*time.Second)
	handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"}}, 4*time.Second)

	open, completed := roundEnds(t, emissions.snapshot())
	if len(open) != 0 || len(completed) != 2 {
		t.Fatalf("rounds: open=%v completed=%d, want both rounds closed", open, len(completed))
	}
	for _, round := range completed {
		if round.TurnIndex != 0 {
			t.Fatalf("round on turn %d, want both rounds on turn 0", round.TurnIndex)
		}
	}
	turn := turnRow()
	if turn.CompletedAt == nil || *turn.CompletedAt < completed[1].CompletedAt {
		t.Fatalf("completed_at = %v, want at or after the last round's %d", turn.CompletedAt, completed[1].CompletedAt)
	}
}

// A completion whose payload cannot be read still ends the round it took.
func TestUnreadableTurnCompleteStillEndsItsRound(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: (*provider.WireTurnCompleteMeta)(nil), Timestamp: time.Now(),
	}); err == nil {
		t.Fatal("unreadable completion reported no error")
	}
	open, completed := roundEnds(t, emissions.snapshot())
	if len(open) != 0 || len(completed) != 1 {
		t.Fatalf("rounds: open=%v completed=%d, want the round closed", open, len(completed))
	}
	if completed[0].TurnIndex != 0 {
		t.Fatalf("completion turn index = %d, want the round's 0", completed[0].TurnIndex)
	}
	if _, active := router.ActiveTurnSnapshot("t1"); active {
		t.Fatal("router still reports the round active")
	}
}

// A round the dying session opens after cleanup synthesized its truncated
// complete is ended by the cleanup too, without counting as activity: no
// turns row settles for it.
func TestCleanupEndsARoundOpenedAfterItsSynthesizedComplete(t *testing.T) {
	st := storetest.Clone(t)
	createTestThread(t, st, "t1")
	emissions := &emissionLog{}
	var (
		router     *Router
		reopenOnce sync.Once
		reopenErr  error
	)
	router = NewRouter(st, func(channel eventchan.Channel, data any) {
		emissions.add(emitted{channel.String(), data})
		// The synthesized complete is the first completion; a frame the
		// session delivered before its stopped flag opens a new round.
		if channel == eventchan.ProviderTurnCompleted {
			reopenOnce.Do(func() {
				reopenErr = router.Handle(provider.ProviderEvent{Kind: provider.EventInit, ThreadID: "t1", Timestamp: time.Now()})
			})
		}
	})
	t.Cleanup(router.flushAllUsage)
	t.Cleanup(router.DrainWireItemRefresh)

	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	router.CleanupThread("t1")
	if reopenErr != nil {
		t.Fatal(reopenErr)
	}
	open, completed := roundEnds(t, emissions.snapshot())
	if len(completed) != 2 {
		t.Fatalf("completions = %d, want the synthesized one and the stranded round's", len(completed))
	}
	if len(open) != 0 {
		t.Fatalf("rounds left open after cleanup: %v", open)
	}
	if completed[1].CountsAsActivity {
		t.Fatal("stranded round's completion counts as activity")
	}
}
