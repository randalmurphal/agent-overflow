package app

import (
	"slices"
	"testing"
	"time"
)

// A wake for a caller that is not archived does not wait for the caller's
// action lock: a Stop, an archive or a revert holding it while a settlement
// delivers must not hold the delivery up, and the archive that follows
// takes the wake back.
func TestAWakeForAnUnarchivedCallerDoesNotTakeItsLock(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.app.startSessionFn = func(string) error { return nil }
	token := owedWakeSeeds()[0].seed(t, f)

	unlock := f.app.threadLocks().Lock(f.caller.ID)
	defer unlock()
	delivered := make(chan struct{})
	go func() {
		f.app.deliverThreadWake(token, false)
		close(delivered)
	}()
	select {
	case <-delivered:
	case <-time.After(10 * time.Second):
		t.Fatal("the wake waited for the caller's action lock")
	}
	if durable, _ := queuedSendIDs(t, f.app, f.caller.ID); !slices.Equal(durable, []string{threadWakeSendID(token)}) {
		t.Fatalf("queue = %q, want the wake", durable)
	}
}

// A wake for an archived caller decides the unarchive under the caller's
// action lock, which an archive holds while it disarms the thread's wakes:
// a wake that waited behind that lock finds its request disarmed, leaves
// the thread archived and is not written.
func TestAWakeForAnArchivedCallerDecidesUnderItsLock(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.app.startSessionFn = func(string) error { return nil }
	token := owedWakeSeeds()[0].seed(t, f)
	if err := f.app.ArchiveThread(f.caller.ID); err != nil {
		t.Fatalf("ArchiveThread: %v", err)
	}
	if _, err := f.app.store.SetThreadRequestNotify(token, true); err != nil {
		t.Fatalf("SetThreadRequestNotify: %v", err)
	}

	unlock := f.app.threadLocks().Lock(f.caller.ID)
	delivered := make(chan struct{})
	go func() {
		f.app.deliverThreadWake(token, false)
		close(delivered)
	}()
	waitUntil(t, 10*time.Second, func() bool { return f.app.threadLocks().Refs(f.caller.ID) == 2 })
	if _, err := f.app.store.DisarmThreadRequestWakesForCaller(f.caller.ID); err != nil {
		unlock()
		t.Fatalf("DisarmThreadRequestWakesForCaller: %v", err)
	}
	unlock()
	select {
	case <-delivered:
	case <-time.After(10 * time.Second):
		t.Fatal("the wake did not finish once the lock was released")
	}
	f.app.waitAgentNoticeWork()

	thread, err := f.app.store.GetThread(f.caller.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	durable, _ := queuedSendIDs(t, f.app, f.caller.ID)
	if row := f.request(t, token); !thread.Archived || len(durable) != 0 || row.DeliveredAt != 0 {
		t.Fatalf("archived %v, queue %q, delivered at %d; want the thread left archived and the wake owed", thread.Archived, durable, row.DeliveredAt)
	}
}
