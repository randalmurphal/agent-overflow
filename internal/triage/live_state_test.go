package triage

import (
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// TestLiveStateSnapshot_DeferredItemsCoverAllPendingSendShapes: every
// pending send's deferred row rides the snapshot in FIFO order, direct
// and flush shapes alike — the SQLite slice a refresh reconciles against
// is structurally blind to them, so this is the frontend's only source.
// FlushedItems stays flush-shaped only (the composer's queue preview).
func TestLiveStateSnapshot_DeferredItemsCoverAllPendingSendShapes(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	direct := store.Item{ID: "user:1", ThreadID: "t1", Kind: "user_text", Summary: "direct send"}
	flush := store.Item{ID: "user:2:flush:1", ThreadID: "t1", Kind: "user_text", Summary: "queued send"}
	router.mu.Lock()
	state := router.state("t1")
	state.pendingSends = append(state.pendingSends,
		pendingSend{AOItemID: direct.ID, Shape: sendShapeDirect, DeferredItem: &direct},
		pendingSend{AOItemID: flush.ID, QueueItemID: "queue:1", Shape: sendShapeFlush, DeferredItem: &flush},
	)
	router.mu.Unlock()

	snap := router.LiveStateSnapshotForThread("t1")

	gotIDs := make([]string, 0, len(snap.DeferredItems))
	for _, item := range snap.DeferredItems {
		gotIDs = append(gotIDs, item.ID)
	}
	if len(gotIDs) != 2 || gotIDs[0] != direct.ID || gotIDs[1] != flush.ID {
		t.Fatalf("DeferredItems ids = %v, want [%s %s]", gotIDs, direct.ID, flush.ID)
	}
	if len(snap.FlushedItems) != 1 || snap.FlushedItems[0].QueueItemID != "queue:1" {
		t.Fatalf("FlushedItems = %+v, want only the flush-shaped entry", snap.FlushedItems)
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
