package triage

import (
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
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

// A later round of a settled turn announces its own completion time, and
// the turns row follows it: a read clamps to the row, so a read taken
// after the later round clears it on every client.
func TestLaterRoundAdvancesTurnCompletedAt(t *testing.T) {
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
	completedAt := func() int64 {
		t.Helper()
		turn, found, err := st.GetTurn("t1:0")
		if err != nil || !found || turn.CompletedAt == nil {
			t.Fatalf("turn row: found=%v err=%v %+v", found, err, turn)
		}
		return *turn.CompletedAt
	}

	handle(provider.ProviderEvent{Kind: provider.EventTurnStart}, 0)
	handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: &provider.SoftRoundCloseMeta{StopReason: "end_turn"}}, time.Second)
	first := completedAt()
	handle(provider.ProviderEvent{Kind: provider.EventInit}, 2*time.Second)
	handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: &provider.SoftRoundCloseMeta{StopReason: "end_turn"}}, 3*time.Second)

	open, completed := roundEnds(t, emissions.snapshot())
	if len(open) != 0 || len(completed) != 2 {
		t.Fatalf("rounds: open=%v completed=%d, want both rounds closed", open, len(completed))
	}
	if got, want := completedAt(), completed[1].CompletedAt; got != want || got <= first {
		t.Fatalf("completed_at = %d, want the later round's %d (first round %d)", got, want, first)
	}

	// The trailing wire result took no round and announced nothing, so
	// it leaves the row where the announcement put it.
	settled := completedAt()
	handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"}}, 4*time.Second)
	if got := completedAt(); got != settled {
		t.Fatalf("roundless trailing result moved completed_at %d -> %d", settled, got)
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
