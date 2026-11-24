package app

import (
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
)

// A wake already handed to a live, mid-turn session waits there for the turn
// boundary. A Stop before the session confirms it takes the message back
// with the session, and the request must not go on reading as delivered: it
// is owed again and disarmed (WithdrawThreadWake), so the person's Stop does
// not start the caller again and thread_status still returns the answer.
func TestAStopThatTakesBackAnUnconfirmedWakeLeavesItsRequestUndelivered(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "mid-thought")
	f.app.configureTriageQueueCallbacks()
	t.Cleanup(func() { f.app.flushDispatch.wg.Wait() })
	if _, err := f.app.SendMessageWithOptions(t.Context(), f.caller.ID, "look into the backfill", SendMessageOptions{}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		_, open, err := f.app.store.GetActiveTurn(f.caller.ID)
		return err == nil && open
	})
	token := f.settledNotifyRequest(t, "the backfill touched 412 rows")
	f.app.deliverThreadWake(token, false)
	wake := f.wakeRow(t, token)
	waitUntil(t, 10*time.Second, func() bool { return len(durableQueueRows(t, f.app, f.caller.ID)) == 0 })
	if row := f.request(t, token); row.DeliveredHow != store.ThreadWakeQueued {
		t.Fatalf("request delivered %q before the stop, want queued", row.DeliveredHow)
	}

	if err := f.app.StopSession(f.caller.ID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}

	if _, found, err := f.app.store.GetThreadItem(f.caller.ID, wake.ID); err != nil || found {
		t.Fatalf("wake row after the stop: found=%v err=%v, want it taken back", found, err)
	}
	row := f.request(t, token)
	if row.DeliveredAt != 0 || row.DeliveredHow != "" || row.Notify {
		t.Fatalf("request after the stop = delivered %q at %d, notify %v; want it owed again and disarmed",
			row.DeliveredHow, row.DeliveredAt, row.Notify)
	}
}

// A Stop clears the caller's queue, a queued wake with it. The requests that
// message answered must not go on reading as delivered: the agent never saw
// it, so they are owed again and disarmed, and thread_status is then where
// the answer is read. A late reply's wake records its own route.
func TestAStopThatDropsAQueuedWakeLeavesItsRequestsUndelivered(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "mid-thought")
	f.holdCallerTurn(t)
	carrier := f.settledNotifyRequest(t, "the turn ended")
	rider := f.settledNotifyRequest(t, "the turn ended")
	late := f.settledNotifyRequest(t, "the first answer")
	if _, err := f.app.store.MarkThreadRequestDelivered(late, store.ThreadWakeInline, 0, false); err != nil {
		t.Fatalf("MarkThreadRequestDelivered: %v", err)
	}
	if stored, err := f.app.store.StoreThreadRequestLateReply(late, []byte("a late reply"), 0, nil, true); err != nil || !stored {
		t.Fatalf("StoreThreadRequestLateReply: stored=%v err=%v", stored, err)
	}
	// Not dropped: a wake another thread's queue holds.
	other, err := createTestThread(t, f.app, "claude", t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("create other caller: %v", err)
	}
	f.app.deliverThreadWakes([]threadWakeRef{{token: carrier}, {token: rider}, {token: late, late: true}})
	if wakes := f.wakesQueuedFor(t, carrier, rider); len(wakes) != 1 {
		t.Fatalf("queued first wakes = %+v, want one message for both", wakes)
	}
	waitUntil(t, 10*time.Second, func() bool { return f.request(t, late).LateDeliveredAt != 0 })
	for _, token := range []string{carrier, rider} {
		if row := f.request(t, token); row.DeliveredHow != store.ThreadWakeQueued {
			t.Fatalf("request %s delivered %q before the stop, want queued", token, row.DeliveredHow)
		}
	}
	if row := f.request(t, late); row.DeliveredHow != store.ThreadWakeInline || row.LateDeliveredHow != store.ThreadWakeQueued {
		t.Fatalf("request %s delivered %q, late %q before the stop; want the first answer inline and the late reply queued", late, row.DeliveredHow, row.LateDeliveredHow)
	}
	// The other thread's dispatch is held, so its wake stays on its queue.
	app := f.app
	app.triage.SetFlushDispatcher(func(threadID string, items []triage.QueuedFlushItem) {
		if threadID != other.ID {
			app.enqueueFlushDispatch(threadID, items)
		}
	})
	elsewhere := f.settledNotifyRequest(t, "for the other thread")
	f.execWaiting(t, `UPDATE thread_requests SET caller_thread_id = '`+other.ID+`' WHERE token = '`+elsewhere+`'`)
	f.app.deliverThreadWake(elsewhere, false)
	waitUntil(t, 10*time.Second, func() bool { return f.request(t, elsewhere).DeliveredAt != 0 })

	if err := f.app.StopSession(f.caller.ID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}

	if rows := durableQueueRows(t, f.app, f.caller.ID); len(rows) != 0 {
		t.Fatalf("queue after the stop = %+v, want it cleared", rows)
	}
	for _, token := range []string{carrier, rider} {
		row := f.request(t, token)
		if row.DeliveredAt != 0 || row.DeliveredHow != "" || row.DeliveredWith != "" || row.Notify {
			t.Errorf("request %s after the stop = delivered %q at %d with %q, notify %v; want it owed again and disarmed",
				token, row.DeliveredHow, row.DeliveredAt, row.DeliveredWith, row.Notify)
		}
	}
	if row := f.request(t, late); row.LateDeliveredAt != 0 || row.DeliveredAt == 0 || row.Notify {
		t.Errorf("late reply after the stop = late delivered at %d, first at %d, notify %v; want only the late reply undelivered",
			row.LateDeliveredAt, row.DeliveredAt, row.Notify)
	}
	if row := f.request(t, elsewhere); row.DeliveredHow != store.ThreadWakeQueued || !row.Notify {
		t.Errorf("another thread's wake = delivered %q, notify %v; want it untouched", row.DeliveredHow, row.Notify)
	}
	// Nothing queues the dropped message again behind the person's Stop.
	f.app.retryUndeliveredThreadWakes(time.Now().Add(time.Hour))
	if rows := durableQueueRows(t, f.app, f.caller.ID); len(rows) != 0 {
		t.Fatalf("queue after a retry pass = %+v, want the dropped wakes left dropped", rows)
	}
	// thread_status reports the answer, and reading it there delivers it.
	report, err := f.adapter().RequestStates(t.Context(), f.callerIdentity(), threadtools.StatusCall{Tokens: []string{carrier}})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(report.Requests) != 1 || report.Requests[0].Answer != "the turn ended" ||
		report.Requests[0].Delivered != store.ThreadWakeInline || report.Requests[0].WakeQueued {
		t.Fatalf("status = %+v, want the answer delivered by this read", report.Requests)
	}
}
