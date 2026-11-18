package app

// A held request's read of rest that fails is read again within the retry
// delay, whether or not the request sweep sleeps when it fails, and
// whether or not the ledger's schedule reads.

import (
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// sweepSleep is one sleep the request sweep asked for: how long, and the
// channel that ends it.
type sweepSleep struct {
	d    time.Duration
	wake chan time.Time
}

// driveSweepClock makes the request sweep's sleeps the test's: each is
// sent on the returned channel and ends when the test sends on its wake.
func (f *requestFixture) driveSweepClock() <-chan sweepSleep {
	sleeps := make(chan sweepSleep, 64)
	f.app.sweepSleepForTest = func(d time.Duration) <-chan time.Time {
		sleep := sweepSleep{d: d, wake: make(chan time.Time, 1)}
		sleeps <- sleep
		return sleep.wake
	}
	return sleeps
}

// A read of rest fails at the worker's turn end while the sweep sleeps on
// its long clock: recording the failure wakes the sweep, which sleeps no
// longer than the retry delay, and once the source reads again that sleep's
// end settles the request.
func TestAFailedReadWakesASleepingSweep(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	worker, token, gate, _ := f.heldWorker(t, 1)
	sleeps := f.driveSweepClock()
	f.runSweeps()
	var asleep sweepSleep
	select {
	case asleep = <-sleeps:
	case <-time.After(20 * time.Second):
		t.Fatal("the sweep never slept")
	}
	if asleep.d <= restRecoveryRetry {
		t.Fatalf("the sweep sleeps %v with nothing owed; the test needs its long clock", asleep.d)
	}
	restore := f.failRestSource(t)
	f.endWorkerTurn(t, worker, gate)
	f.expectHeld(t, worker, token, "with its sources unread")
	var retry sweepSleep
	for retry.wake == nil {
		select {
		case sleep := <-sleeps:
			if sleep.d <= restRecoveryRetry {
				retry = sleep
			}
		case <-time.After(20 * time.Second):
			t.Fatal("the failed read did not wake the sleeping sweep into a retry")
		}
	}
	restore()
	// The sweep may have slept again since, woken by a later failed read:
	// end every retry sleep it takes until the request settles, and no
	// longer one.
	settled := make(chan struct{})
	var waking sync.WaitGroup
	waking.Go(func() {
		retry.wake <- time.Now()
		for {
			select {
			case <-settled:
				return
			case sleep := <-sleeps:
				if sleep.d <= restRecoveryRetry {
					sleep.wake <- time.Now()
				}
			}
		}
	})
	row := f.awaitRequestState(t, token, store.ThreadRequestFinished)
	close(settled)
	waking.Wait()
	if string(row.Answer) != "Handed it on." {
		t.Fatalf("settled %+v; want finished with the worker's message", row)
	}
}

// With a failed read of rest owed, the sweep's sleep is the retry delay
// even when the ledger's schedule cannot be read.
func TestTheRetryDoesNotWaitForTheSchedule(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.app.noteRestRead("unread-thread", errUnreadable)
	unreadable := f.breakTable(t, "thread_owner_ops")
	if _, err := f.app.store.NextThreadRequestWork(nil); err == nil {
		t.Fatal("the schedule still reads; the test needs it not to")
	}
	if delay := f.app.threadRequestSweepDelay(time.Now()); delay > restRecoveryRetry {
		t.Fatalf("the sweep sleeps %v with a failed read owed and no schedule; want at most %v", delay, restRecoveryRetry)
	}
	unreadable()
}

// A loop's next fire bounds the sweep's sleep even when the ledger's
// schedule cannot be read.
func TestALoopsFireDoesNotWaitForTheSchedule(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	fire := 2 * time.Minute
	if _, err := f.app.store.RegisterAgentWake(store.AgentWake{ID: "soon", ThreadID: f.caller.ID, Kind: store.AgentWakeLoop, Label: "board",
		IntervalSeconds: 600, NextAt: time.Now().Add(fire).UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	unreadable := f.breakTable(t, "thread_owner_ops")
	defer unreadable()
	if delay := f.app.threadRequestSweepDelay(time.Now()); delay > fire {
		t.Fatalf("the sweep sleeps %v with a loop due in %v and no schedule; want at most that", delay, fire)
	}
}

// A retry whose read of rest heals but whose settlement then cannot read the
// held receipt keeps the retry owed: once the receipt reads again, the
// sweep's next retry settles the request, with no new event.
func TestARetryIsOwedUntilItsSettlementCompletes(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	worker, token, gate, _ := f.heldWorker(t, 1)
	restoreSource := f.failRestSource(t)
	f.endWorkerTurn(t, worker, gate)
	f.app.threadRequestsWG.Wait()
	f.expectHeld(t, worker, token, "while the source read failed")
	if !f.app.restReadsFailed() {
		t.Fatal("the failed read did not owe a retry")
	}
	restoreSource()
	restoreReceipts := f.breakTable(t, "thread_request_receipts")
	f.app.retryFailedRestReads()
	f.expectHeld(t, worker, token, "while the held receipt could not be read")
	if !f.app.restReadsFailed() {
		t.Fatal("the retry stopped being owed though its settlement failed")
	}
	restoreReceipts()
	f.app.retryFailedRestReads()
	if row := f.request(t, token); row.State != store.ThreadRequestFinished {
		t.Fatalf("request %+v after the receipt read again; want finished", row)
	}
	if f.app.restReadsFailed() {
		t.Fatal("the retry is still owed once the request settled")
	}
}

// A retry ends with the attempt it makes: one whose read finds the thread
// not at rest is no longer owed, the request staying held for the change
// that rests it; one for a thread none of whose requests is held any
// longer is dropped.
func TestARetryEndsWithItsAttempt(t *testing.T) {
	t.Parallel()
	t.Run("not at rest", func(t *testing.T) {
		t.Parallel()
		f := newRequestFixture(t)
		worker, token, gate, _ := f.heldWorker(t, 1)
		restoreSource := f.failRestSource(t)
		f.endWorkerTurn(t, worker, gate)
		f.app.threadRequestsWG.Wait()
		if !f.app.restReadsFailed() {
			t.Fatal("the failed read did not owe a retry")
		}
		f.armChildLoop(t, worker, "board")
		restoreSource()
		f.app.retryFailedRestReads()
		f.expectHeld(t, worker, token, "with a loop armed")
		if f.app.restReadsFailed() {
			t.Fatal("the retry is still owed though its read found the thread not at rest")
		}
	})
	t.Run("settled by a pass", func(t *testing.T) {
		t.Parallel()
		f := newRequestFixture(t)
		worker, token, gate, _ := f.heldWorker(t, 1)
		restoreSource := f.failRestSource(t)
		f.endWorkerTurn(t, worker, gate)
		f.app.threadRequestsWG.Wait()
		if !f.app.restReadsFailed() {
			t.Fatal("the failed read did not owe a retry")
		}
		restoreSource()
		f.app.settleRestedReceipts()
		if row := f.request(t, token); row.State != store.ThreadRequestFinished {
			t.Fatalf("request %+v after a pass with its sources read again; want finished", row)
		}
		if f.app.restReadsFailed() {
			t.Fatal("the retry is still owed once a pass settled the request")
		}
	})
	t.Run("nothing held", func(t *testing.T) {
		t.Parallel()
		f := newRequestFixture(t)
		f.app.noteRestRead("unread-thread", errUnreadable)
		f.app.retryFailedRestReads()
		if f.app.restReadsFailed() {
			t.Fatal("the retry is still owed for a thread with no held request")
		}
	})
}
