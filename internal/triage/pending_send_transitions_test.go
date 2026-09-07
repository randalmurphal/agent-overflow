package triage

import (
	"testing"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
)

// TestTakeUnconfirmedFlushSendsLocked_PartitionsAndIsTotal pins the one
// non-trivial registry transition the session-death drain calls: it
// takes queued FLUSH entries only, splits them by whether their echo
// already arrived, preserves FIFO order inside each partition, and
// leaves nothing behind for a replacement session's echo to match. The
// second call is the "called twice" leg — a drain that ran once must
// find the registry empty of its own entries rather than hand the same
// message back a second time.
func TestTakeUnconfirmedFlushSendsLocked_PartitionsAndIsTotal(t *testing.T) {
	r := NewRouter(nil, func(eventchan.Channel, any) {})

	deferredRow := store.Item{ID: "user:1:flush:1", ThreadID: "t1", TurnIndex: 1, Summary: "first"}
	quietRow := store.Item{ID: "user:1:flush:2", ThreadID: "t1", TurnIndex: 1, Summary: "second"}

	r.mu.Lock()
	st := r.state("t1")
	st.pendingSends = []pendingSend{
		// A direct send: no queue item id, so the drain does not own it.
		{AOItemID: "user:1", Shape: sendShapeDirect, InterruptedTurnIndex: -1},
		{AOItemID: "user:1:flush:1", QueueItemID: "queue:a", Shape: sendShapeFlush, DeferredItem: &deferredRow, InterruptedTurnIndex: -1},
		{AOItemID: "user:1:flush:2", QueueItemID: "queue:b", Shape: sendShapeFlush, QuietItem: &quietRow, EchoConsumed: true, InterruptedTurnIndex: -1},
		{AOItemID: "user:1:flush:3", QueueItemID: "queue:c", Shape: sendShapeFlush, InterruptedTurnIndex: -1},
		// The Codex post-interrupt re-send: flush-shaped, but no queue
		// item id, so the interrupt's eager persist still owns the row.
		{AOItemID: "user:1:flush:4", Shape: sendShapeFlush, InterruptedTurnIndex: -1},
	}
	restorable, echoConsumed := r.takeUnconfirmedFlushSendsLocked("t1")
	remaining := append([]pendingSend(nil), st.pendingSends...)
	r.mu.Unlock()

	if len(restorable) != 2 {
		t.Fatalf("restorable = %d entries, want the two flush sends whose echo never arrived", len(restorable))
	}
	if restorable[0].AOItemID != "user:1:flush:1" || restorable[1].AOItemID != "user:1:flush:3" {
		t.Errorf("restorable FIFO order = %q, %q — want flush:1 then flush:3", restorable[0].AOItemID, restorable[1].AOItemID)
	}
	if restorable[0].DeferredItem == nil || restorable[0].DeferredItem.Summary != "first" {
		t.Error("retained deferred copy lost — the death path could not restore the draft")
	}
	if len(echoConsumed) != 1 || echoConsumed[0].AOItemID != "user:1:flush:2" {
		t.Fatalf("echoConsumed = %+v, want only the entry whose echo proved provider consumption", echoConsumed)
	}

	if len(remaining) != 2 {
		t.Fatalf("registry kept %d entries, want the direct send and the flush re-send", len(remaining))
	}
	if remaining[0].AOItemID != "user:1" || remaining[1].AOItemID != "user:1:flush:4" {
		t.Errorf("kept entries = %q, %q — want the two the drain does not own, in order", remaining[0].AOItemID, remaining[1].AOItemID)
	}

	// Called twice: the take is total, so a second drain finds nothing of
	// its own and cannot hand the same message back again.
	r.mu.Lock()
	restorableAgain, echoConsumedAgain := r.takeUnconfirmedFlushSendsLocked("t1")
	stillRemaining := len(st.pendingSends)
	r.mu.Unlock()
	if len(restorableAgain) != 0 || len(echoConsumedAgain) != 0 {
		t.Errorf("second take returned %d restorable / %d echo-consumed, want none", len(restorableAgain), len(echoConsumedAgain))
	}
	if stillRemaining != 2 {
		t.Errorf("second take disturbed the entries it does not own: %d left, want 2", stillRemaining)
	}
}

// TestPendingSendEchoStashes_AreCopyScopedAndIdempotent covers the
// popped-copy transitions: each writes only its own field, the
// anchor-recorded claim is a one-way flag that a second call leaves
// alone, and none of it touches the registry — the copy is the echo
// path's alone until reinsertPendingSendHead hands it back.
func TestPendingSendEchoStashes_AreCopyScopedAndIdempotent(t *testing.T) {
	r := NewRouter(nil, func(eventchan.Channel, any) {})
	r.mu.Lock()
	r.state("t1").pendingSends = []pendingSend{
		{AOItemID: "user:1:flush:1", QueueItemID: "queue:a", Shape: sendShapeFlush, InterruptedTurnIndex: -1},
	}
	r.mu.Unlock()

	popped, ok := r.consumeMatchingPendingSendForEcho("t1", "", "")
	if !ok {
		t.Fatal("pending entry not consumed")
	}

	popped.stashEchoIdentity("uuid-1", "parent-1")
	popped.Confirmation = &userMessageConfirmation{Placement: true, BoundaryID: "first-echo-prefix"}

	popped.markAnchorRecordedAtEcho()
	popped.markAnchorRecordedAtEcho()

	if popped.EchoProviderItemID != "uuid-1" || popped.EchoParentUUID != "parent-1" {
		t.Errorf("echo identity = %q/%q, want uuid-1/parent-1", popped.EchoProviderItemID, popped.EchoParentUUID)
	}
	if popped.Confirmation == nil || popped.Confirmation.BoundaryID != "first-echo-prefix" {
		t.Fatal("confirmation facts lost")
	}

	if !popped.AnchorRecordedAtEcho {
		t.Error("AnchorRecordedAtEcho claim not held after two calls")
	}
	if popped.AOItemID != "user:1:flush:1" || popped.Shape != sendShapeFlush {
		t.Error("identity or shape moved — both are immutable after registration")
	}

	// Reinsert is the only way the stash reaches the registry, and it
	// carries the whole copy.
	r.reinsertPendingSendHead("t1", popped)
	r.mu.Lock()
	back := r.state("t1").pendingSends
	r.mu.Unlock()
	if len(back) != 1 {
		t.Fatalf("registry holds %d entries after reinsert, want 1", len(back))
	}
	if back[0].EchoProviderItemID != "uuid-1" || back[0].Confirmation != popped.Confirmation || !back[0].AnchorRecordedAtEcho {
		t.Errorf("reinserted entry lost stashed echo state: %+v", back[0])
	}
	if !back[0].EchoConsumed {
		t.Error("reinsert did not mark the entry EchoConsumed")
	}
}

func TestStashEchoIdentityPreservesFirstKnownFields(t *testing.T) {
	p := pendingSend{}
	p.stashEchoIdentity("first", "")
	p.stashEchoIdentity("other", "wrong-parent")
	if p.EchoProviderItemID != "first" || p.EchoParentUUID != "" {
		t.Fatalf("mixed identities: %+v", p)
	}
	p.stashEchoIdentity("first", "parent")
	p.stashEchoIdentity("first", "")
	p.stashEchoIdentity("first", "other-parent")
	if p.EchoProviderItemID != "first" || p.EchoParentUUID != "parent" {
		t.Fatalf("lost first known identity: %+v", p)
	}
}
