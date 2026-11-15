package app

import (
	"errors"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
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

// A wake that resolved its caller before New Thread, Same Assistant moved
// the request finds the retired thread archived and the request no longer
// its own: the retired thread stays archived, and the message is rebuilt
// for the assistant's current thread.
func TestAWakeRacingAHandOffLeavesTheRetiredThreadArchived(t *testing.T) {
	t.Parallel()
	f := newAssistantFixture(t)
	f.app.startSessionFn = func(string) error { return nil }
	f.assistantCaller(t)
	token := f.settledNotifyRequest(t, "done")
	built := f.app.threadWakeMessages([]threadWakeRef{{token: token}})
	if len(built) != 1 {
		t.Fatalf("messages = %+v, want one", built)
	}
	successor, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatal(err)
	}
	placeInAssistantHome(t, f.app, successor.ID, f.caller.ID)
	if _, err := f.app.store.HandOffAssistant(f.caller.ID, successor.ID, time.Now().UnixMilli()); err != nil {
		t.Fatalf("HandOffAssistant: %v", err)
	}

	// The preparation a delivery that resolved the retired thread runs.
	if err := f.app.unarchiveForThreadWake(f.caller.ID, built[0].rows[0], false); !errors.Is(err, store.ErrThreadWakeNotOwed) {
		t.Fatalf("unarchiveForThreadWake = %v, want the wake refused as not owed by this caller", err)
	}
	if old := f.reload(t, f.caller.ID); !old.Archived {
		t.Fatal("the retired thread was unarchived for a request its successor owns")
	}

	// The delivery of the message built before the handoff lands on the
	// successor alone.
	f.app.deliverThreadWakeMessages(built)
	f.app.waitAgentNoticeWork()
	if old := f.reload(t, f.caller.ID); !old.Archived {
		t.Fatal("the delivery unarchived the retired thread")
	}
	if durable, _ := queuedSendIDs(t, f.app, successor.ID); !slices.Equal(durable, []string{threadWakeSendID(token)}) {
		t.Fatalf("successor queue = %q, want the wake", durable)
	}
	if durable, _ := queuedSendIDs(t, f.app, f.caller.ID); len(durable) != 0 {
		t.Fatalf("retired thread queue = %q, want nothing", durable)
	}
}
