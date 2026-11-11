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
