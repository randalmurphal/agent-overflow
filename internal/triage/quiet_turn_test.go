package triage

import (
	"context"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// quietTurnRouter is a router whose classifier names every top-level turn
// of t1 quiet when quiet is set, and a thread t1 read up to now.
func quietTurnRouter(t *testing.T, quiet bool) (*Router, *store.Store, *emissionLog, int64) {
	t.Helper()
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	router.SetQuietTurnClassifier(func(threadID string, _ int) bool { return quiet && threadID == "t1" })
	if _, _, err := st.MarkThreadReadNow(context.Background(), "t1"); err != nil {
		t.Fatalf("mark t1 read: %v", err)
	}
	thread, err := st.GetThread("t1")
	if err != nil {
		t.Fatal(err)
	}
	return router, st, emissions, thread.UpdatedAt
}

func millis(at int64) time.Time { return time.UnixMilli(at) }

func runTurn(t *testing.T, router *Router, startAt, completeAt int64, meta provider.TurnCompleteMeta) {
	t.Helper()
	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0, Timestamp: millis(startAt)}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: meta, Timestamp: millis(completeAt)}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}
}

func completedEvents(emissions *emissionLog) []TurnCompletedEvent {
	var out []TurnCompletedEvent
	for _, e := range filterEmissions(emissions.snapshot(), eventchan.ProviderTurnCompleted.String()) {
		out = append(out, e.data.(TurnCompletedEvent))
	}
	return out
}

// A tick that ends cleanly leaves the thread where it was in the sidebar
// and read past its completion, announces the row before the event, and
// the event says it was quiet.
func TestQuietTurnKeepsAReadThreadReadAndInPlace(t *testing.T) {
	router, st, emissions, updatedAt := quietTurnRouter(t, true)
	completeAt := time.Now().UnixMilli() + 10_000
	runTurn(t, router, completeAt-1_000, completeAt, normalTurnCompleteMeta())

	thread, err := st.GetThread("t1")
	if err != nil {
		t.Fatal(err)
	}
	if thread.UpdatedAt != updatedAt {
		t.Errorf("updated_at = %d, want %d: a quiet turn is not activity", thread.UpdatedAt, updatedAt)
	}
	if thread.LastReadAt == nil || *thread.LastReadAt != completeAt {
		t.Errorf("last_read_at = %v, want the completion %d", thread.LastReadAt, completeAt)
	}
	events := completedEvents(emissions)
	if len(events) != 1 || !events[0].Quiet {
		t.Fatalf("turn_completed = %+v, want one quiet event", events)
	}
	// The row frame carrying the kept marker precedes the event.
	sawRow := false
	for _, e := range emissions.snapshot() {
		if row, ok := e.data.(ThreadUpdateEvent); ok && row.Thread != nil && row.Thread.ID == "t1" &&
			row.Thread.LastReadAt != nil && *row.Thread.LastReadAt == completeAt {
			sawRow = true
		}
		if e.eventName == eventchan.ProviderTurnCompleted.String() && !sawRow {
			t.Fatal("turn_completed arrived before the row that kept the thread read")
		}
	}
}

// A turn the classifier does not name, and a tick that failed, are the
// user's: the thread moves up and is owed a read.
func TestTurnsThatAreNotQuietStayActivity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		quiet    bool
		meta     *provider.WireTurnCompleteMeta
		quietOut bool
	}{
		{name: "not a tick", quiet: false, meta: normalTurnCompleteMeta()},
		{name: "a tick that failed", quiet: true, meta: &provider.WireTurnCompleteMeta{StopReason: "error_during_execution", ErrorMessage: "boom"}},
		{name: "a tick that was interrupted", quiet: true, meta: &provider.WireTurnCompleteMeta{StopReason: "end_turn", Aborted: true}},
		{name: "a tick whose result carried an error", quiet: true, meta: &provider.WireTurnCompleteMeta{StopReason: "end_turn", ErrorMessage: "boom"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, st, emissions, _ := quietTurnRouter(t, tc.quiet)
			before, err := st.GetThread("t1")
			if err != nil {
				t.Fatal(err)
			}
			completeAt := time.Now().UnixMilli() + 10_000
			runTurn(t, router, completeAt-1_000, completeAt, tc.meta)

			thread, err := st.GetThread("t1")
			if err != nil {
				t.Fatal(err)
			}
			if thread.UpdatedAt != completeAt {
				t.Errorf("updated_at = %d, want the completion %d", thread.UpdatedAt, completeAt)
			}
			if *thread.LastReadAt != *before.LastReadAt {
				t.Errorf("last_read_at = %d, want it left at %d", *thread.LastReadAt, *before.LastReadAt)
			}
			for _, event := range completedEvents(emissions) {
				if event.Quiet {
					t.Errorf("turn_completed = %+v, want it not quiet", event)
				}
			}
		})
	}
}

// A tick the model continues after a soft close (a ToolSearch or
// ExitPlanMode resume) is one turn with a later round: the round advances
// the turn's completion and keeps the thread read past it.
func TestQuietTurnLaterRoundStaysRead(t *testing.T) {
	router, st, emissions, updatedAt := quietTurnRouter(t, true)
	first := time.Now().UnixMilli() + 10_000
	runTurn(t, router, first-1_000, first, &provider.SoftRoundCloseMeta{StopReason: "end_turn"})
	second := first + 5_000
	resume := parentContentBlockStart("t1", "text")
	resume.Timestamp = millis(second - 1_000)
	if err := router.Handle(resume); err != nil {
		t.Fatalf("continuation: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta(), Timestamp: millis(second)}); err != nil {
		t.Fatalf("second complete: %v", err)
	}

	thread, err := st.GetThread("t1")
	if err != nil {
		t.Fatal(err)
	}
	if thread.LatestTurnCompletedAt == nil || *thread.LatestTurnCompletedAt != second {
		t.Fatalf("latest completion = %v, want the second round's %d", thread.LatestTurnCompletedAt, second)
	}
	if thread.LastReadAt == nil || *thread.LastReadAt != second {
		t.Errorf("last_read_at = %v, want %d", thread.LastReadAt, second)
	}
	if thread.UpdatedAt != updatedAt {
		t.Errorf("updated_at = %d, want %d", thread.UpdatedAt, updatedAt)
	}
	events := completedEvents(emissions)
	if len(events) != 2 || !events[0].Quiet || !events[1].Quiet || events[1].TurnIndex != 0 {
		t.Errorf("turn_completed = %+v, want both rounds of turn 0 quiet", events)
	}
}

// A loop's or watch's wake row is not activity when it is persisted, and a
// person's message joined to one is.
func TestTickUserTextDoesNotBumpThreadActivity(t *testing.T) {
	for _, tc := range []struct {
		name  string
		meta  string
		bumps bool
	}{
		{name: "a wake", meta: `{"sendId":"agent-wake:w1:1","origin":"agent-wake"}`},
		{name: "wakes joined", meta: `{"sendId":"agent-wake:w1:1","joinedSendIds":["agent-wake:w1:1","agent-wake:w2:1"],"origin":"agent-wake"}`},
		{name: "a person's message joined to a wake", meta: `{"sendId":"agent-wake:w1:1","joinedSendIds":["agent-wake:w1:1","send-1"],"origin":"agent-wake"}`, bumps: true},
		{name: "an outside send", meta: `{"sendId":"agent-send:s1","origin":"agent-wake"}`, bumps: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, st, _ := newTestRouter(t)
			createTestThread(t, st, "t1")
			before := readThreadUpdatedAt(t, st, "t1")
			now := before + 5_000
			if err := router.PersistItem(store.Item{
				ID: "user:0", ThreadID: "t1", Kind: "user_text", Role: "user", Status: "completed",
				Summary: "Loop fired.", Meta: tc.meta, CreatedAt: now, UpdatedAt: now,
			}, nil); err != nil {
				t.Fatalf("persist: %v", err)
			}
			want := before
			if tc.bumps {
				want = now
			}
			if got := readThreadUpdatedAt(t, st, "t1"); got != want {
				t.Errorf("updated_at = %d, want %d", got, want)
			}
		})
	}
}

// A tick that failed is the user's to read even when a later round of it
// ends cleanly: the later round does not read the failure for them.
func TestQuietLaterRoundLeavesAnEarlierFailureUnread(t *testing.T) {
	router, st, emissions, _ := quietTurnRouter(t, true)
	before, err := st.GetThread("t1")
	if err != nil {
		t.Fatal(err)
	}
	first := time.Now().UnixMilli() + 10_000
	runTurn(t, router, first-1_000, first, &provider.WireTurnCompleteMeta{StopReason: "error_during_execution", ErrorMessage: "boom"})
	second := first + 5_000
	if !router.setOpenRoundSnapshot(ActiveTurnSnapshot{ThreadID: "t1", TurnID: "round-2", TurnIndex: 0, StartedAt: second - 1_000}, "") {
		t.Fatal("the settled turn took no later round")
	}
	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta(), Timestamp: millis(second)}); err != nil {
		t.Fatalf("second complete: %v", err)
	}

	thread, err := st.GetThread("t1")
	if err != nil {
		t.Fatal(err)
	}
	if thread.LatestTurnCompletedAt == nil || *thread.LatestTurnCompletedAt != second {
		t.Fatalf("latest completion = %v, want the later round's %d", thread.LatestTurnCompletedAt, second)
	}
	if thread.LastReadAt == nil || *thread.LastReadAt != *before.LastReadAt {
		t.Errorf("last_read_at = %v, want it left at %d with the failure unread", thread.LastReadAt, *before.LastReadAt)
	}
	if thread.UpdatedAt != first {
		t.Errorf("updated_at = %d, want the failed round's %d", thread.UpdatedAt, first)
	}
	events := completedEvents(emissions)
	if len(events) != 2 {
		t.Fatalf("turn_completed = %+v, want one per round", events)
	}
	for _, event := range events {
		if event.Quiet {
			t.Errorf("turn_completed = %+v, want no round of a failed tick quiet", event)
		}
	}
}

// A turn the CLI woke itself for (a background-task notification, a
// scheduled wakeup) is never quiet, even when the classifier would take it
// for the pickup of queued wakes: no command opened it, so no queued wake
// did. A turn a queued command opened is the classifier's to name.
func TestSelfWokenTurnIsNeverQuiet(t *testing.T) {
	for _, tc := range []struct {
		name   string
		opener provider.TurnOpener
		quiet  bool
	}{
		{name: "self-woken", opener: "", quiet: false},
		{name: "a queued command", opener: provider.TurnOpenerCommand, quiet: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, _, emissions, _ := quietTurnRouter(t, true)
			completeAt := time.Now().UnixMilli() + 10_000
			runTurn(t, router, completeAt-2_000, completeAt-1_000, normalTurnCompleteMeta())
			if err := router.Handle(openerInit(t, tc.opener)); err != nil {
				t.Fatalf("init: %v", err)
			}
			if err := router.Handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta(), Timestamp: millis(completeAt)}); err != nil {
				t.Fatalf("turn complete: %v", err)
			}
			events := completedEvents(emissions)
			if len(events) != 2 || events[1].TurnIndex != 1 {
				t.Fatalf("turn_completed = %+v, want the first turn's and the provider-started turn 1's", events)
			}
			if events[1].Quiet != tc.quiet {
				t.Fatalf("provider-started turn quiet = %v, want %v", events[1].Quiet, tc.quiet)
			}
		})
	}
}
