package triage

import (
	"context"
	"encoding/json"
	"slices"
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

// What a turn took in is named by its deliveries, which reach the hook
// before the turn ends and whichever order Claude writes `started`, init
// and echo in. A turn the CLI opened for itself under the index of a send
// waiting for a turn took nothing in: no delivery names it, and the send's
// delivery names the turn that read it. Each delivery names the message's
// members, an id-less person's among them.
func TestDeliveriesNameWhatEachTurnTookIn(t *testing.T) {
	const uuid = "uuid-waiting-1"
	members := []string{"", "agent-wake:w1:1"}
	for _, tc := range []struct {
		name string
		take func(t *testing.T, router *Router)
		turn int
	}{
		{name: "started, init, echo", turn: 1, take: func(t *testing.T, router *Router) {
			handleAll(t, router, commandLifecycleEvent("t1", uuid, provider.CommandStarted), openerInit(t, provider.TurnOpenerCommand), echoUUID(uuid))
		}},
		{name: "init, echo, started", turn: 1, take: func(t *testing.T, router *Router) {
			handleAll(t, router, openerInit(t, ""), echoUUID(uuid), commandLifecycleEvent("t1", uuid, provider.CommandStarted))
		}},
		{name: "a turn of the CLI's own first", turn: 2, take: func(t *testing.T, router *Router) {
			handleAll(t, router, openerInit(t, ""), turnCompleteNow())
			handleAll(t, router, commandLifecycleEvent("t1", uuid, provider.CommandStarted), openerInit(t, provider.TurnOpenerCommand), echoUUID(uuid))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, _, _, _ := quietTurnRouter(t, false)
			var got []SendDelivery
			router.SetSendDeliveredHook(func(_ string, d SendDelivery) {
				if !d.AwaitingTurn {
					got = append(got, d)
				}
			})
			handleAll(t, router, provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", Timestamp: time.Now()})
			now := time.Now().UnixMilli()
			if err := router.PersistItem(store.Item{
				ID: "user:1", ThreadID: "t1", TurnIndex: 1, Kind: "user_text", Role: "user",
				Status: "completed", Summary: "sent", CreatedAt: now, UpdatedAt: now,
			}, nil); err != nil {
				t.Fatalf("persist send row: %v", err)
			}
			router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{ProviderItemID: uuid, Members: members})
			handleAll(t, router, commandLifecycleEvent("t1", uuid, provider.CommandQueued), turnCompleteNow())
			tc.take(t, router)
			if len(got) != 1 || got[0].TurnIndex != tc.turn || got[0].UserItemID != "user:1" || !slices.Equal(got[0].Members, members) {
				t.Fatalf("deliveries = %+v, want one into turn %d naming its members", got, tc.turn)
			}
		})
	}
}

func handleAll(t *testing.T, router *Router, events ...provider.ProviderEvent) {
	t.Helper()
	for _, evt := range events {
		if err := router.Handle(evt); err != nil {
			t.Fatalf("%s: %v", evt.Kind, err)
		}
	}
}

func echoUUID(uuid string) provider.ProviderEvent {
	meta, _ := json.Marshal(map[string]any{"provider_item_id": uuid})
	return provider.ProviderEvent{Kind: provider.EventUserText, ThreadID: "t1", ItemID: uuid, Content: "sent", Meta: meta, Timestamp: time.Now()}
}

func turnCompleteNow() provider.ProviderEvent {
	return provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now()}
}
