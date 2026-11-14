package app

import (
	"context"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// A cancel whose caller goes away after the receipt says cancelled still
// takes the queued message back: the record and the work it describes do
// not part.
func TestThreadCancelCutAfterSettlingStillTakesTheMessageBack(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	target := f.busyThread(t, "cut-cancel")
	ack, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{ThreadID: target.ID, Message: "take this back"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	// Hold the lock the removal waits for, so the cut lands between the
	// settlement and the removal.
	unlock, err := f.app.threadApplication().LockMutable(t.Context(), target.ID)
	if err != nil {
		t.Fatalf("LockMutable: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	type result struct {
		effect string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		effect, err := f.app.stopThreadRequestWork(ctx, ack.Token)
		done <- result{effect, err}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for f.receipt(t, ack.Token).State != store.ThreadReceiptCancelled {
		if time.Now().After(deadline) {
			unlock()
			t.Fatal("the receipt never settled cancelled")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	time.Sleep(50 * time.Millisecond)
	unlock()
	select {
	case got := <-done:
		if got.err != nil || got.effect != threadtools.EffectQueuedRemoved {
			t.Fatalf("stop after the cut = %q, %v; want the queued message removed", got.effect, got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stop never finished")
	}
	if rows := durableQueueRows(t, f.app, target.ID); len(rows) != 0 {
		t.Fatalf("cancelled message is still queued: %+v", rows)
	}
}

// A cancel repeated after an earlier one settled the receipt but stopped
// nothing finishes the stop instead of reporting that nothing is left.
func TestThreadCancelRepeatedAfterAnInterruptedStopFinishesIt(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	target := f.busyThread(t, "repeat-cancel")
	ack, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{ThreadID: target.ID, Message: "take this back"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	// What an earlier cancel cut after its settlement left behind.
	if err := f.app.settleThreadReceipt(ack.Token, store.ThreadReceiptOpenStates(), store.ThreadRequestSettlement{
		State: store.ThreadReceiptCancelled, Answer: []byte("The sender cancelled this request."), AnswerKind: store.ThreadAnswerError,
	}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if rows := durableQueueRows(t, f.app, target.ID); len(rows) != 1 {
		t.Fatalf("queued rows before the repeat = %+v, want the message still queued", rows)
	}

	report, err := f.adapter().Cancel(t.Context(), f.callerIdentity(), threadtools.CancelCall{Token: ack.Token})
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if report.Effect != threadtools.EffectQueuedRemoved || report.State != threadtools.RequestCancelled {
		t.Fatalf("repeated cancel = %+v, want the queued message removed", report)
	}
	if rows := durableQueueRows(t, f.app, target.ID); len(rows) != 0 {
		t.Fatalf("cancelled message is still queued: %+v", rows)
	}
	if peer, err := f.app.stopThreadRequestWork(t.Context(), ack.Token); err != nil || peer != threadtools.EffectNothing {
		t.Fatalf("forwarded repeat after the stop = %q, %v; want nothing left", peer, err)
	}
}

// A repeated cancel of a receipt that names no turn, whose message has left
// the queue, touches nothing on the thread: the calls the thread's current
// turn has parked keep waiting, and it answers that nothing is left to stop.
func TestThreadCancelRepeatedWithoutATurnTouchesNoTurn(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	target := f.busyThread(t, "repeat-no-turn")
	ack, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{ThreadID: target.ID, Message: "already past"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := f.app.settleThreadReceipt(ack.Token, store.ThreadReceiptOpenStates(), store.ThreadRequestSettlement{
		State: store.ThreadReceiptCancelled, Answer: []byte("The sender cancelled this request."), AnswerKind: store.ThreadAnswerError,
	}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if receipt := f.receipt(t, ack.Token); receipt.TurnID != "" {
		t.Fatalf("receipt names turn %q, want none", receipt.TurnID)
	}
	// The message left the queue and reached the thread's open turn.
	sendID := threadRequestSendID(ack.Token)
	if removal, err := f.app.removeQueuedItem(t.Context(), target.ID, sendID); err != nil || removal != queueRemovalRemoved {
		t.Fatalf("take the message off the queue: %v, %v", removal, err)
	}
	now := time.Now().UnixMilli()
	if err := f.app.store.InsertItem(store.Item{
		ID: "user:1:flush:0", ThreadID: target.ID, TurnIndex: 1, ItemIndex: -1,
		Kind: "user_text", Role: "user", Status: "completed", Summary: "already past",
		Meta: `{"sendId":"` + sendID + `"}`, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("insert the dispatched message: %v", err)
	}
	parked, end := f.app.beginRemoteWait(t.Context(), target.ID, "computer", "request")
	defer end()

	effect, err := f.app.stopThreadRequestWork(t.Context(), ack.Token)
	if err != nil || effect != threadtools.EffectNothing {
		t.Fatalf("repeated cancel = %q, %v; want nothing to stop", effect, err)
	}
	if parked.Err() != nil {
		t.Fatalf("the repeated cancel ended a call the thread's turn parked: %v", context.Cause(parked))
	}
}

// An interrupt fenced on a turn the thread is not running leaves the calls
// the running turn parked alone; only one that interrupts that turn ends
// them.
func TestFencedInterruptOfAnotherTurnLeavesParkedCalls(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "working")
	spawn := f.runningRequest(t, "keep going")
	receipt := f.receipt(t, spawn.Token)
	index, ok := threadRequestTurnIndex(receipt.TargetThreadID, receipt.TurnID)
	if !ok {
		t.Fatalf("receipt turn %q has no index", receipt.TurnID)
	}
	waitUntil(t, 10*time.Second, func() bool {
		live, err := f.adapter().LiveState(t.Context(), receipt.TargetThreadID)
		return err == nil && live.ActiveTurn && f.app.triage.OpenTurnIndex(receipt.TargetThreadID) == index
	})
	parked, end := f.app.beginRemoteWait(t.Context(), receipt.TargetThreadID, "computer", "request")
	defer end()

	interrupted, err := f.app.interruptTurnAtIndex(t.Context(), receipt.TargetThreadID, index+5, stopByWorkflow())
	if err != nil || interrupted {
		t.Fatalf("interrupt of another turn = %v, %v; want nothing sent", interrupted, err)
	}
	if parked.Err() != nil {
		t.Fatalf("an interrupt of another turn ended the running turn's parked call: %v", context.Cause(parked))
	}
	if interrupted, err := f.app.interruptTurnAtIndex(t.Context(), receipt.TargetThreadID, index, stopByWorkflow()); err != nil || !interrupted {
		t.Fatalf("interrupt of the running turn = %v, %v", interrupted, err)
	}
	if parked.Err() == nil {
		t.Fatal("interrupting the running turn left its parked call waiting")
	}
}
