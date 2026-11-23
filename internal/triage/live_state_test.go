package triage

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// TestLiveStateSnapshot_FlushedItemsAreComposerMarkersOnly: FlushedItems
// names the pending sends drawn above the composer and no send whose row
// the timeline already shows.
//
// A direct send's row is persisted and emitted before it is registered, so
// it is never a marker. Flush shapes route by where their row already is:
// deferred (no row yet) and quiet (row persisted without an item event,
// revealed on consumption) are markers; a row anchored at an interrupt was
// emitted at its final position, so it is only a timeline row, and the
// claim outranks any copy still retained on the entry.
func TestLiveStateSnapshot_FlushedItemsAreComposerMarkersOnly(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	deferred := store.Item{ID: "user:2:flush:1", ThreadID: "t1", Kind: "user_text", Summary: "queued send"}
	quiet := store.Item{ID: "user:2:flush:2", ThreadID: "t1", Kind: "user_text", Summary: "quiet send"}
	anchored := store.Item{ID: "user:2:flush:3", ThreadID: "t1", Kind: "user_text", Summary: "anchored send"}
	// The contradictory shape: a retained DEFERRED copy on an entry that
	// also claims the interrupt anchor. Neither interrupt path produces it
	// (the eager persist drops DeferredItem in the same locked step that
	// sets the claim), but the app marks by AOItemID, and flush ids are
	// deterministic per sendId — a re-registration under the same id can be
	// marked alongside the original. The anchor claim only ever follows a
	// successful persist+emit, so the row is on screen and the entry is a
	// marker for nothing.
	anchoredCopy := store.Item{ID: "user:2:flush:5", ThreadID: "t1", Kind: "user_text", Summary: "anchored with a retained copy"}
	router.mu.Lock()
	state := router.state("t1")
	state.pendingSends = append(state.pendingSends,
		pendingSend{AOItemID: "user:1", Shape: sendShapeDirect},
		pendingSend{AOItemID: deferred.ID, QueueItemID: "queue:1", Shape: sendShapeFlush, DeferredItem: &deferred},
		pendingSend{AOItemID: quiet.ID, QueueItemID: "queue:2", Shape: sendShapeFlush, QuietItem: &quiet},
		pendingSend{AOItemID: anchored.ID, QueueItemID: "queue:3", Shape: sendShapeFlush, QuietItem: &anchored, AnchoredAtInterrupt: true},
		pendingSend{AOItemID: "user:2:flush:4", QueueItemID: "queue:4", Shape: sendShapeFlush},
		pendingSend{
			AOItemID:            anchoredCopy.ID,
			QueueItemID:         "queue:5",
			Shape:               sendShapeFlush,
			DeferredItem:        &anchoredCopy,
			AnchoredAtInterrupt: true,
		},
	)
	router.mu.Unlock()

	snap := router.LiveStateSnapshotForThread("t1")

	gotFlushed := make([]string, 0, len(snap.FlushedItems))
	for _, item := range snap.FlushedItems {
		gotFlushed = append(gotFlushed, item.UserItemID)
	}
	if len(gotFlushed) != 2 || gotFlushed[0] != deferred.ID || gotFlushed[1] != quiet.ID {
		t.Fatalf("FlushedItems ids = %v, want [%s %s]: a direct or anchored send's row is already in the timeline", gotFlushed, deferred.ID, quiet.ID)
	}
	if snap.FlushedItems[1].QueueItemID != "queue:2" || snap.FlushedItems[1].Message != quiet.Summary {
		t.Fatalf("quiet FlushedItem = %+v, want queue:2 carrying the persisted row's text", snap.FlushedItems[1])
	}
}

// LiveActivitySnapshot is the sidebar's reconnect leg: it must name every
// thread with something open and nothing else, so a client can treat the
// list as authoritative for the whole backend.
func TestLiveActivitySnapshot_NamesOnlyThreadsWithLiveActivity(t *testing.T) {
	router, st, _ := newTestRouter(t)
	for _, id := range []string{"idle", "running", "blocked", "compacting"} {
		createTestThread(t, st, id)
	}

	// An idle thread with router state (it once had a session) must not appear.
	router.mu.Lock()
	router.state("idle")
	router.mu.Unlock()

	router.setOpenRoundSnapshot(ActiveTurnSnapshot{ThreadID: "running", TurnID: "round-1", TurnIndex: 3, StartedAt: 1_000}, "")
	router.setPendingApproval("blocked", pendingApprovalState{Request: provider.ApprovalRequest{RequestID: "approval-2"}}, "")
	router.setPendingApproval("blocked", pendingApprovalState{Request: provider.ApprovalRequest{RequestID: "approval-1"}}, "")
	router.setPendingUserInput("blocked", provider.UserInputRequest{RequestID: "question-1"}, "")
	router.mu.Lock()
	router.state("compacting").compactingSince = 2_000
	router.state("compacting").compactingSinceSet = true
	router.mu.Unlock()

	got := router.LiveActivitySnapshot()
	if len(got) != 3 {
		t.Fatalf("LiveActivitySnapshot() = %+v, want 3 threads", got)
	}
	if got[0].ThreadID != "blocked" || got[1].ThreadID != "compacting" || got[2].ThreadID != "running" {
		t.Fatalf("thread order = %s, %s, %s; want sorted by id", got[0].ThreadID, got[1].ThreadID, got[2].ThreadID)
	}
	blocked, compacting, running := got[0], got[1], got[2]
	if blocked.ActiveTurn != nil || len(blocked.ApprovalRequestIDs) != 2 || blocked.ApprovalRequestIDs[0] != "approval-2" || blocked.ApprovalRequestIDs[1] != "approval-1" {
		t.Fatalf("blocked = %+v, want both approvals in arrival order and no turn", blocked)
	}
	if len(blocked.UserInputRequestIDs) != 1 || blocked.UserInputRequestIDs[0] != "question-1" {
		t.Fatalf("blocked user inputs = %v, want [question-1]", blocked.UserInputRequestIDs)
	}
	if compacting.CompactingSinceUnixMs != 2_000 || compacting.ActiveTurn != nil {
		t.Fatalf("compacting = %+v, want only the compacting window", compacting)
	}
	if running.ActiveTurn == nil || running.ActiveTurn.TurnID != "round-1" || running.ActiveTurn.TurnIndex != 3 || running.ActiveTurn.StartedAt != 1_000 {
		t.Fatalf("running.ActiveTurn = %+v, want the open round", running.ActiveTurn)
	}

	// A taken round stays listed until its end is announced (endRound), and
	// the thread drops from the list on the read after that.
	if _, ok := router.takeOpenRound("running", ""); !ok {
		t.Fatal("takeOpenRound(running) = false, want the open round")
	}
	listed := false
	for _, entry := range router.LiveActivitySnapshot() {
		if entry.ThreadID == "running" {
			listed = entry.ActiveTurn != nil && entry.ActiveTurn.TurnID == "round-1"
		}
	}
	if !listed {
		t.Fatal("running dropped from the list before its round's end was announced")
	}
	router.endRound("running", "round-1")
	for _, entry := range router.LiveActivitySnapshot() {
		if entry.ThreadID == "running" {
			t.Fatalf("running still listed after its round closed: %+v", entry)
		}
	}
}

// The round ends before provider:turn_completed goes out, so whatever the
// frame starts reads it ended; until then a snapshot carries it.
func TestATurnsRoundEndsBeforeItsCompletionIsAnnounced(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	var roundAtEmit *ActiveTurnSnapshot
	read := false
	router.emit = func(name eventchan.Channel, data any) {
		if name == eventchan.ProviderTurnCompleted && !read {
			read = true
			roundAtEmit = router.LiveStateSnapshotForThread("t1").ActiveTurn
		}
		emissions.add(emitted{name.String(), data})
	}
	base := time.UnixMilli(1_700_000_000_000)
	for _, evt := range []provider.ProviderEvent{
		{Kind: provider.EventTurnStart, ThreadID: "t1", Timestamp: base},
		{Kind: provider.EventTurnComplete, ThreadID: "t1", Timestamp: base.Add(time.Second), TurnComplete: normalTurnCompleteMeta()},
	} {
		if err := router.Handle(evt); err != nil {
			t.Fatalf("%s: %v", evt.Kind, err)
		}
	}
	if !read {
		t.Fatal("no provider:turn_completed")
	}
	if roundAtEmit != nil {
		t.Fatalf("round when provider:turn_completed went out = %+v; want it ended", roundAtEmit)
	}
}

func TestLiveStateSnapshotWaitsForConsumedSendPersistence(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 1)
	row := store.Item{ID: "user:1:flush:1", ThreadID: "t1", TurnIndex: 1, Kind: "user_text", Role: "user", Status: "completed", Summary: "pending", CreatedAt: 1, UpdatedAt: 1}
	router.RegisterPendingFlushSendWithExpectation("t1", "q", row, 0, PendingSendExpectation{ByClientID: true})
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	router.deferredPersistGate = func() { close(entered); <-release }
	echoDone := make(chan error, 1)
	go func() {
		echoDone <- router.Handle(provider.ProviderEvent{Kind: provider.EventUserText, ThreadID: "t1", Content: row.Summary,
			Meta: json.RawMessage(`{"provider_item_id":"native-echo","client_id":"user:1:flush:1"}`), Timestamp: time.Now()})
	}()
	defer func() {
		unblock()
		if err := <-echoDone; err != nil {
			t.Errorf("echo: %v", err)
		}
	}()
	<-entered
	done := make(chan LiveStateSnapshot, 1)
	go func() { done <- router.LiveStateSnapshotForThread("t1") }()
	select {
	case snapshot := <-done:
		t.Fatalf("snapshot returned between consumption and persistence: %+v", snapshot)
	case <-time.After(50 * time.Millisecond):
	}
	unblock()
	snapshot := <-done
	if len(snapshot.FlushedItems) != 0 {
		t.Fatalf("confirmed send still pending: %+v", snapshot.FlushedItems)
	}
	stored, found, err := st.GetThreadItem("t1", row.ID)
	if err != nil || !found || !strings.Contains(stored.Meta, "native-echo") {
		t.Fatalf("snapshot preceded confirmed history: found=%v row=%+v err=%v", found, stored, err)
	}
}

func TestLiveStateSnapshotDoesNotAllocateIdleThreadIdentity(t *testing.T) {
	router, _, _ := newTestRouter(t)
	for range 2 {
		router.LiveStateSnapshotForThread("idle")
	}
	if router.identityIfPresent("idle") != nil {
		t.Fatal("live-state read allocated a permanent identity")
	}
}

// TestLiveStateSnapshot_ATurnIsOwedFromItsSendUntilItsRoundIsPublished: a
// direct send is never a composer marker, but from its write until its
// turn's round is published the thread has a turn coming. The real start
// is held after it claimed the send and wrote the turns row: the snapshot
// still owes the turn there, and ActiveTurn takes over once it returns. A
// steer joins a running turn and opens none.
func TestLiveStateSnapshot_ATurnIsOwedFromItsSendUntilItsRoundIsPublished(t *testing.T) {
	r, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	r.RegisterPendingSteerSendWithExpectation("t1", "user:0:steer:1", 0, PendingSendExpectation{})
	if r.LiveStateSnapshotForThread("t1").TurnOwed {
		t.Fatal("a steer reads as a turn owed")
	}
	r.ClearPendingSendForFailure("t1", "user:0:steer:1")
	r.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{})
	snapshot := r.LiveStateSnapshotForThread("t1")
	if !snapshot.TurnOwed || len(snapshot.FlushedItems) != 0 {
		t.Fatalf("snapshot after a direct send = %+v; want a turn owed and no marker", snapshot)
	}
	// No wire index: the start claims the send for its turn.
	held := holdTurnStart(t, r, turnStartEvent("t1", 0), false)
	if _, unclaimed := r.turnOpeningPendingSend("t1"); unclaimed {
		t.Fatal("the held start did not claim the send")
	}
	snapshot = r.LiveStateSnapshotForThread("t1")
	if !snapshot.TurnOwed || snapshot.ActiveTurn != nil {
		t.Fatalf("snapshot inside the start = %+v; want the turn owed before its round", snapshot)
	}
	held.release()
	if err := <-held.handled; err != nil {
		t.Fatalf("turn start: %v", err)
	}
	snapshot = r.LiveStateSnapshotForThread("t1")
	if snapshot.ActiveTurn == nil || snapshot.ActiveTurn.TurnIndex != 1 || snapshot.TurnOwed {
		t.Fatalf("snapshot after the start = %+v; want its round active and nothing more owed", snapshot)
	}
}
