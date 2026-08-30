package triage

import (
	"testing"

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
