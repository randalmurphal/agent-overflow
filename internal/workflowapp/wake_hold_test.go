package workflowapp

import (
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/workflow/engine"
)

// holdFixture is a run parked at a gate and bound to an idle thread, whose
// wakes are counted as ordinary sends.
type holdFixture struct {
	*serviceFixture
	item   store.WorkItem
	thread string
	mu     sync.Mutex
	sent   []string
}

func newHoldFixture(t *testing.T) *holdFixture {
	t.Helper()
	f := &holdFixture{serviceFixture: newServiceFixture(t), thread: "thread-1"}
	f.item = f.run("run-1", string(engine.StateNeedsHuman), string(engine.ReasonGate))
	if err := f.store.CreateThread(store.Thread{
		ID: f.thread, ProjectID: f.project.ID, ProjectPath: f.project.Path,
		Title: "Origin", Provider: "claude", Mode: threadmode.ModeChat, CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.UpdateWorkItemOriginThread(f.item.ID, f.thread); err != nil {
		t.Fatal(err)
	}
	f.service.deps.WakeDelivery.SendMessage = func(threadID, message string) error {
		if threadID != f.thread || !strings.Contains(message, f.item.ID) {
			t.Fatalf("wake to %q: %q", threadID, message)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.sent = append(f.sent, message)
		return nil
	}
	return f
}

// rest delivers the run's transition to rest as the engine would, and waits
// for the wake half to finish with it.
func (f *holdFixture) rest() {
	f.service.AfterStateEvent(engine.StateEvent{
		ItemID: f.item.ID, From: engine.StateRunning, To: engine.StateNeedsHuman, Reason: engine.ReasonGate,
	})
	f.service.WaitWake()
}

func (f *holdFixture) release(hold *WakeHold, delivered bool) {
	hold.Release(delivered)
	f.service.WaitWake()
}

func (f *holdFixture) wakes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

// A rest that arrives while a call waits is kept back, and the call returning
// it is the delivery: no wake, then or later.
func TestARestTheWaitingCallReturnedDoesNotWake(t *testing.T) {
	f := newHoldFixture(t)
	hold := f.service.HoldRestWake(f.item.ID, f.thread)
	f.rest()
	if f.wakes() != 0 {
		t.Fatalf("a rest woke the thread while a call held it: %d wakes", f.wakes())
	}
	f.release(hold, true)
	if f.wakes() != 0 {
		t.Fatalf("a rest the call returned woke the thread: %d wakes", f.wakes())
	}
	recorded, err := f.store.WorkItemWakeSignature(f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recorded == "" || strings.HasPrefix(recorded, WakeQueuedPrefix) {
		t.Fatalf("delivered rest recorded as %q", recorded)
	}
}

// A call can read the rest from the store before the engine's event queues
// its wake. Its release lands first and records the rest, so the wake that
// follows says nothing new and is suppressed.
func TestARestTheCallReturnedBeforeItsWakeRanDoesNotWake(t *testing.T) {
	f := newHoldFixture(t)
	hold := f.service.HoldRestWake(f.item.ID, f.thread)
	f.release(hold, true)
	f.rest()
	if f.wakes() != 0 {
		t.Fatalf("the rest's late wake repeated what the call returned: %d wakes", f.wakes())
	}
}

// A call that ends without returning the rest (it backgrounded, failed, or
// its client went away) leaves the thread owed exactly the wake it held back.
func TestAKeptBackRestWakesWhenTheCallEndsWithoutIt(t *testing.T) {
	f := newHoldFixture(t)
	hold := f.service.HoldRestWake(f.item.ID, f.thread)
	f.rest()
	f.release(hold, false)
	if f.wakes() != 1 {
		t.Fatalf("a kept-back rest delivered %d wakes, want 1", f.wakes())
	}
	// Released twice, it still delivers once.
	f.release(hold, false)
	if f.wakes() != 1 {
		t.Fatalf("a second release delivered again: %d wakes", f.wakes())
	}
}

// A backgrounded call that held nothing back leaves the wake half alone: the
// rest that comes after it wakes the thread once.
func TestABackgroundedCallLeavesTheLaterRestToWake(t *testing.T) {
	f := newHoldFixture(t)
	hold := f.service.HoldRestWake(f.item.ID, f.thread)
	f.release(hold, false)
	f.rest()
	if f.wakes() != 1 {
		t.Fatalf("a rest after a backgrounded call delivered %d wakes, want 1", f.wakes())
	}
}

// The call delivered one rest; the run's next rest, after somebody acts on
// it, is news and wakes the thread.
func TestALaterRestAfterADeliveredOneStillWakes(t *testing.T) {
	f := newHoldFixture(t)
	hold := f.service.HoldRestWake(f.item.ID, f.thread)
	f.rest()
	f.release(hold, true)
	// A resume: the run goes back to running, which spends the record.
	if err := f.store.UpdateWorkItemState(f.item.ID, string(engine.StateRunning), "", 0); err != nil {
		t.Fatal(err)
	}
	f.service.AfterStateEvent(engine.StateEvent{ItemID: f.item.ID, From: engine.StateNeedsHuman, To: engine.StateRunning})
	f.service.WaitWake()
	if err := f.store.UpdateWorkItemState(f.item.ID, string(engine.StateNeedsHuman), string(engine.ReasonGate), 0); err != nil {
		t.Fatal(err)
	}
	f.rest()
	if f.wakes() != 1 {
		t.Fatalf("the rest after a resume delivered %d wakes, want 1", f.wakes())
	}
}

// A hold for one thread keeps nothing back from the run's bound thread.
func TestAHoldForAnotherThreadHoldsNothing(t *testing.T) {
	f := newHoldFixture(t)
	hold := f.service.HoldRestWake(f.item.ID, "other-thread")
	f.rest()
	if f.wakes() != 1 {
		t.Fatalf("a hold for another thread kept the bound thread's wake back: %d wakes", f.wakes())
	}
	// And its delivery records nothing the bound thread was not told.
	if err := f.store.UpdateWorkItemWakeSignature(f.item.ID, ""); err != nil {
		t.Fatal(err)
	}
	f.release(hold, true)
	if recorded, err := f.store.WorkItemWakeSignature(f.item.ID); err != nil || recorded != "" {
		t.Fatalf("another thread's call recorded %q (%v) for the bound thread", recorded, err)
	}
}

// Two calls from the thread wait on the run. The first ends without the rest;
// the second still might return it, so it takes the kept-back rest over.
func TestAKeptBackRestPassesToAnotherStandingHold(t *testing.T) {
	f := newHoldFixture(t)
	first := f.service.HoldRestWake(f.item.ID, f.thread)
	second := f.service.HoldRestWake(f.item.ID, f.thread)
	f.rest()
	f.release(first, false)
	if f.wakes() != 0 {
		t.Fatalf("a rest another call still holds woke the thread: %d wakes", f.wakes())
	}
	f.release(second, true)
	if f.wakes() != 0 {
		t.Fatalf("a rest the second call returned woke the thread: %d wakes", f.wakes())
	}
	if len(f.service.holds.byRun) != 0 {
		t.Fatalf("released holds stayed registered: %v", f.service.holds.byRun)
	}
}
