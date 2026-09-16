package triage

import (
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// TestLiveStateSnapshot_PendingSendAppearsInExactlyOneList: a pending send
// is either a timeline row the SQLite slice is blind to (DeferredItems) or
// a composer marker (FlushedItems), never both. A refresh taken mid-send
// merges the first into the window and draws the second above the composer,
// so an entry in both lists put the same message on screen twice.
//
// Flush shapes route by where their row already is: deferred (no row yet)
// and quiet (row persisted without an item event, revealed on consumption)
// are markers; a row anchored at an interrupt was emitted at its final
// position, so it is only a timeline row — the claim outranks any copy
// still retained on the entry.
func TestLiveStateSnapshot_PendingSendAppearsInExactlyOneList(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	direct := store.Item{ID: "user:1", ThreadID: "t1", Kind: "user_text", Summary: "direct send"}
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
		pendingSend{AOItemID: direct.ID, Shape: sendShapeDirect, DeferredItem: &direct},
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

	gotDeferred := make([]string, 0, len(snap.DeferredItems))
	for _, item := range snap.DeferredItems {
		gotDeferred = append(gotDeferred, item.ID)
	}
	if len(gotDeferred) != 1 || gotDeferred[0] != direct.ID {
		t.Fatalf("DeferredItems ids = %v, want [%s] — an anchored entry's row is already in SQLite", gotDeferred, direct.ID)
	}

	gotFlushed := make([]string, 0, len(snap.FlushedItems))
	for _, item := range snap.FlushedItems {
		gotFlushed = append(gotFlushed, item.UserItemID)
	}
	if len(gotFlushed) != 2 || gotFlushed[0] != deferred.ID || gotFlushed[1] != quiet.ID {
		t.Fatalf("FlushedItems ids = %v, want [%s %s]", gotFlushed, deferred.ID, quiet.ID)
	}
	if snap.FlushedItems[1].QueueItemID != "queue:2" || snap.FlushedItems[1].Message != quiet.Summary {
		t.Fatalf("quiet FlushedItem = %+v, want queue:2 carrying the persisted row's text", snap.FlushedItems[1])
	}
	for _, item := range snap.DeferredItems {
		for _, flushed := range snap.FlushedItems {
			if item.ID == flushed.UserItemID {
				t.Fatalf("%s is in both DeferredItems and FlushedItems", item.ID)
			}
		}
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

	router.setOpenRoundSnapshot(ActiveTurnSnapshot{ThreadID: "running", TurnID: "round-1", TurnIndex: 3, StartedAt: 1_000})
	router.setPendingApproval("blocked", pendingApprovalState{Request: provider.ApprovalRequest{RequestID: "approval-2"}})
	router.setPendingApproval("blocked", pendingApprovalState{Request: provider.ApprovalRequest{RequestID: "approval-1"}})
	router.setPendingUserInput("blocked", provider.UserInputRequest{RequestID: "question-1"})
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

	// A closed round drops the thread from the list on the next read.
	if _, ok := router.takeOpenRound("running"); !ok {
		t.Fatal("takeOpenRound(running) = false, want the open round")
	}
	for _, entry := range router.LiveActivitySnapshot() {
		if entry.ThreadID == "running" {
			t.Fatalf("running still listed after its round closed: %+v", entry)
		}
	}
}
