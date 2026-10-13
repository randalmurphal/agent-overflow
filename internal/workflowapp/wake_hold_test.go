package workflowapp

import (
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/workflow/engine"
)

// holdFixture is a run parked at a gate and bound to an idle thread, whose
// wakes are counted as ordinary sends. Transitions go through the engine's
// event path, so the watch ring records them before their wake jobs queue.
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
	f.item.OriginThreadID = f.thread
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

// emit delivers a transition the store already holds, as the engine does
// after its write. It does not wait for the wake jobs it queues.
func (f *holdFixture) emit(from engine.State) {
	f.t.Helper()
	item, err := f.store.GetWorkItem(f.item.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	f.service.AfterEngineEvent(eventchan.WorkflowItemState, engine.StateEvent{
		ItemID: item.ID, ProjectID: item.ProjectID,
		From: from, To: engine.State(item.State), Reason: engine.Reason(item.Reason),
	})
}

// write changes the run's stored state without emitting anything: the
// engine's write before its event.
func (f *holdFixture) write(to engine.State, reason engine.Reason) engine.State {
	f.t.Helper()
	item, err := f.store.GetWorkItem(f.item.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.store.UpdateWorkItemState(f.item.ID, string(to), string(reason), 0); err != nil {
		f.t.Fatal(err)
	}
	return engine.State(item.State)
}

// move is one engine transition: the write, then its event.
func (f *holdFixture) move(to engine.State, reason engine.Reason) {
	f.t.Helper()
	f.emit(f.write(to, reason))
}

// rest delivers the run's transition to the rest it is stored at, and waits
// for the wake half to finish with it.
func (f *holdFixture) rest() {
	f.t.Helper()
	f.emit(engine.StateRunning)
	f.service.WaitWake()
}

// read is what a waiting call reads: the ring head, then the run's state.
func (f *holdFixture) read() *ReturnedRest {
	f.t.Helper()
	cursor := f.service.watch.Head()
	item, err := f.store.GetWorkItem(f.item.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return &ReturnedRest{Cursor: cursor, State: item.State, Reason: item.Reason}
}

func (f *holdFixture) release(hold *WakeHold, returned *ReturnedRest) {
	hold.Release(returned)
	f.service.WaitWake()
}

// block holds the wake queue until the returned func is called, so jobs queue
// up behind it in the order they were asked for.
func (f *holdFixture) block() func() {
	gate := make(chan struct{})
	f.service.wake.Go(func() { <-gate })
	return func() {
		close(gate)
		f.service.WaitWake()
	}
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
	f.release(hold, f.read())
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
	f.release(hold, f.read())
	f.rest()
	if f.wakes() != 0 {
		t.Fatalf("the rest's late wake repeated what the call returned: %d wakes", f.wakes())
	}
}

// The rest's wake job was queued before the call returned the rest: the job
// runs after the release was asked for, and must still find the hold.
func TestAWakeQueuedBeforeTheReleaseStillFindsTheHold(t *testing.T) {
	f := newHoldFixture(t)
	hold := f.service.HoldRestWake(f.item.ID, f.thread)
	unblock := f.block()
	f.emit(engine.StateRunning)
	hold.Release(f.read())
	unblock()
	if f.wakes() != 0 {
		t.Fatalf("a rest the call returned woke the thread: %d wakes", f.wakes())
	}
}

// A call that ends without returning the rest (it backgrounded, failed, or
// its response never reached the client) leaves the thread owed exactly the
// wake it held back.
func TestAKeptBackRestWakesWhenTheCallEndsWithoutIt(t *testing.T) {
	f := newHoldFixture(t)
	hold := f.service.HoldRestWake(f.item.ID, f.thread)
	f.rest()
	f.release(hold, nil)
	if f.wakes() != 1 {
		t.Fatalf("a kept-back rest delivered %d wakes, want 1", f.wakes())
	}
	// Released twice, it still delivers once.
	f.release(hold, nil)
	if f.wakes() != 1 {
		t.Fatalf("a second release delivered again: %d wakes", f.wakes())
	}
}

// A backgrounded call that held nothing back leaves the wake half alone: the
// rest that comes after it wakes the thread once.
func TestABackgroundedCallLeavesTheLaterRestToWake(t *testing.T) {
	f := newHoldFixture(t)
	hold := f.service.HoldRestWake(f.item.ID, f.thread)
	f.release(hold, nil)
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
	f.release(hold, f.read())
	f.move(engine.StateRunning, "")
	f.move(engine.StateNeedsHuman, engine.ReasonGate)
	f.service.WaitWake()
	if f.wakes() != 1 {
		t.Fatalf("the rest after a resume delivered %d wakes, want 1", f.wakes())
	}
}

// The call returned the gate rest; before its release ran, the run was
// resumed and came to rest again. The later rest is news.
func TestARestAfterTheOneTheCallReturnedStillWakes(t *testing.T) {
	for name, later := range map[string]struct {
		state  engine.State
		reason engine.Reason
	}{
		"done":      {engine.StateDone, ""},
		"same gate": {engine.StateNeedsHuman, engine.ReasonGate},
	} {
		t.Run(name, func(t *testing.T) {
			f := newHoldFixture(t)
			hold := f.service.HoldRestWake(f.item.ID, f.thread)
			f.rest() // kept back; the call reads it
			returned := f.read()
			f.move(engine.StateRunning, "")
			f.move(later.state, later.reason)
			f.service.WaitWake() // the later rest is kept back too
			f.release(hold, returned)
			if f.wakes() != 1 {
				t.Fatalf("the rest after the one the call returned delivered %d wakes, want 1", f.wakes())
			}
		})
	}
}

// Control for the case above: the same transitions with no call waiting.
func TestTwoRestsWithNoCallWaitingWakeTwice(t *testing.T) {
	f := newHoldFixture(t)
	f.rest()
	f.move(engine.StateRunning, "")
	f.move(engine.StateDone, "")
	f.service.WaitWake()
	if f.wakes() != 2 {
		t.Fatalf("two rests delivered %d wakes, want 2", f.wakes())
	}
}

// Two calls from the thread; the first returns the rest, the second then ends
// without it. One delivery in total, so no wake.
func TestADeliveredAndAnUndeliveredCallTogetherWakeNothing(t *testing.T) {
	f := newHoldFixture(t)
	first := f.service.HoldRestWake(f.item.ID, f.thread)
	second := f.service.HoldRestWake(f.item.ID, f.thread)
	f.rest()
	f.release(first, f.read())
	f.release(second, nil)
	if f.wakes() != 0 {
		t.Fatalf("a rest one call returned also woke the thread: %d wakes", f.wakes())
	}
}

// The engine writes a transition before it records it. A release that runs
// in between finds nothing in the ring after the call's read, but the store
// already holds a different rest, which is not the one the call returned.
func TestARestWrittenButNotYetRecordedIsNotTheOneTheCallReturned(t *testing.T) {
	f := newHoldFixture(t)
	hold := f.service.HoldRestWake(f.item.ID, f.thread)
	f.rest()
	returned := f.read()
	from := f.write(engine.StateDone, "")
	f.release(hold, returned)
	f.emit(from)
	f.service.WaitWake()
	if f.wakes() != 1 {
		t.Fatalf("a rest the call never returned delivered %d wakes, want 1", f.wakes())
	}
}

// An action's clear and the rest after it were queued before the release: the
// release lands after the clear spent the old record, so the record it writes
// stands and the rest's own wake, kept back, is not repeated.
func TestADeliveredReleaseLandsBehindTheWorkQueuedBeforeIt(t *testing.T) {
	f := newHoldFixture(t)
	hold := f.service.HoldRestWake(f.item.ID, f.thread)
	f.rest()
	f.release(hold, nil) // an earlier rest, woken
	hold = f.service.HoldRestWake(f.item.ID, f.thread)
	unblock := f.block()
	f.move(engine.StateRunning, "")
	f.move(engine.StateNeedsHuman, engine.ReasonGate)
	hold.Release(f.read())
	unblock()
	if f.wakes() != 1 {
		t.Fatalf("the rest the call returned delivered %d more wakes, want 0", f.wakes()-1)
	}
}

// An action and the rest after it came behind the release in the queue. The
// kept-back rest is spent by that action whether or not the call returned it;
// the later rest wakes once, from its own job.
func TestAnActionQueuedBehindTheReleaseLeavesItsRestOneWake(t *testing.T) {
	for name, delivered := range map[string]bool{"returned": true, "not returned": false} {
		t.Run(name, func(t *testing.T) {
			f := newHoldFixture(t)
			hold := f.service.HoldRestWake(f.item.ID, f.thread)
			f.rest()
			var returned *ReturnedRest
			if delivered {
				returned = f.read()
			}
			unblock := f.block()
			hold.Release(returned)
			f.move(engine.StateRunning, "")
			f.move(engine.StateNeedsHuman, engine.ReasonGate)
			unblock()
			if f.wakes() != 1 {
				t.Fatalf("the later rest delivered %d wakes, want 1", f.wakes())
			}
		})
	}
}

// The rest's wake job runs after an action and a re-rest were recorded. The
// kept-back rest is measured from when its wake was queued, not from when
// the job ran, so the release sees the action and leaves the re-rest to its
// own wake.
func TestAKeptBackRestIsMeasuredFromWhenItsWakeWasQueued(t *testing.T) {
	f := newHoldFixture(t)
	hold := f.service.HoldRestWake(f.item.ID, f.thread)
	unblock := f.block()
	f.emit(engine.StateRunning) // the rest; its wake job waits
	hold.Release(nil)
	f.move(engine.StateRunning, "")
	f.move(engine.StateNeedsHuman, engine.ReasonGate)
	unblock()
	if f.wakes() != 1 {
		t.Fatalf("the re-rest delivered %d wakes, want 1", f.wakes())
	}
}

// A rest named without the cursor it was read at cannot be shown to be the
// run's current one, so it is not taken as delivered.
func TestARestWithoutAReadPointIsNotDelivered(t *testing.T) {
	f := newHoldFixture(t)
	hold := f.service.HoldRestWake(f.item.ID, f.thread)
	f.rest()
	returned := f.read()
	returned.Cursor = 0
	f.release(hold, returned)
	if f.wakes() != 1 {
		t.Fatalf("a rest with no read point delivered %d wakes, want 1", f.wakes())
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
	f.release(hold, f.read())
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
	f.release(first, nil)
	if f.wakes() != 0 {
		t.Fatalf("a rest another call still holds woke the thread: %d wakes", f.wakes())
	}
	f.release(second, f.read())
	if f.wakes() != 0 {
		t.Fatalf("a rest the second call returned woke the thread: %d wakes", f.wakes())
	}
	if len(f.service.holds.byRun) != 0 {
		t.Fatalf("released holds stayed registered: %v", f.service.holds.byRun)
	}
}

// A call from another thread that returned the rest delivered it to that
// thread, not to the one the run is bound to.
func TestAnotherThreadsDeliveryDoesNotSilenceTheBoundThread(t *testing.T) {
	f := newHoldFixture(t)
	f.release(f.service.HoldRestWake(f.item.ID, "other-thread"), f.read())
	f.rest()
	if f.wakes() != 1 {
		t.Fatalf("another thread's delivery silenced the bound thread: %d wakes", f.wakes())
	}
}

// usageClaim reserves the usage-attention generation a usage-limited rest
// would carry to the bound thread.
func (f *holdFixture) usageClaim() store.WorkflowProviderUsageAttentionClaim {
	f.t.Helper()
	scope, err := f.store.OpenWorkflowProviderUsageScope("claude", "account", 1, 1)
	if err != nil {
		f.t.Fatal(err)
	}
	claim, claimed, err := f.store.ClaimWorkflowProviderUsageAttention(scope, f.thread, f.item.ID, "token-1", 1)
	if err != nil || !claimed {
		f.t.Fatalf("claim = %v, %v", claimed, err)
	}
	return claim
}

// claimState reads where a claim ended: still queued, delivered, or released
// so the generation can be claimed again.
func (f *holdFixture) claimState(claim store.WorkflowProviderUsageAttentionClaim) string {
	f.t.Helper()
	_, again, err := f.store.ClaimWorkflowProviderUsageAttention(claim.ScopeID, f.thread, f.item.ID, "token-2", 2)
	if err != nil {
		f.t.Fatal(err)
	}
	if again {
		return "released"
	}
	queued, err := f.store.ReleaseWorkflowProviderUsageAttention(claim, 3)
	if err != nil {
		f.t.Fatal(err)
	}
	if queued {
		return "queued"
	}
	return "delivered"
}

// keepWithClaim surfaces the run's rest as a usage-limited park does, with
// the attention claim it carries, while hold stands.
func (f *holdFixture) keepWithClaim(claim store.WorkflowProviderUsageAttentionClaim) {
	f.t.Helper()
	item, err := f.store.GetWorkItem(f.item.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	done := make(chan struct{})
	f.service.wake.Go(func() {
		defer close(done)
		if !f.service.afterWorkflowResting(item, &claim, f.service.watch.Head()) {
			f.t.Error("a kept-back rest was reported undelivered")
		}
	})
	<-done
}

// The attention a kept-back rest carried settles with the rest: delivered
// with it when the call returned it or its wake went out, and given back
// when the rest was spent before anyone was told.
func TestAKeptBackRestsAttentionSettlesWithIt(t *testing.T) {
	t.Run("the call returned it", func(t *testing.T) {
		f := newHoldFixture(t)
		claim := f.usageClaim()
		hold := f.service.HoldRestWake(f.item.ID, f.thread)
		f.keepWithClaim(claim)
		f.release(hold, f.read())
		if state := f.claimState(claim); state != "delivered" {
			t.Fatalf("attention of a returned rest is %s, want delivered", state)
		}
	})
	t.Run("its wake went out", func(t *testing.T) {
		f := newHoldFixture(t)
		claim := f.usageClaim()
		hold := f.service.HoldRestWake(f.item.ID, f.thread)
		f.keepWithClaim(claim)
		f.release(hold, nil)
		if f.wakes() != 1 {
			t.Fatalf("kept-back rest delivered %d wakes, want 1", f.wakes())
		}
		if state := f.claimState(claim); state != "delivered" {
			t.Fatalf("attention of a woken rest is %s, want delivered", state)
		}
	})
	t.Run("it was spent", func(t *testing.T) {
		f := newHoldFixture(t)
		claim := f.usageClaim()
		hold := f.service.HoldRestWake(f.item.ID, f.thread)
		f.keepWithClaim(claim)
		f.move(engine.StateRunning, "")
		f.service.WaitWake()
		f.release(hold, nil)
		if f.wakes() != 0 {
			t.Fatalf("a spent rest delivered %d wakes", f.wakes())
		}
		if state := f.claimState(claim); state != "released" {
			t.Fatalf("attention of a spent rest is %s, want released", state)
		}
	})
	t.Run("a later rest replaced it", func(t *testing.T) {
		// Cancelling a parked run goes from rest to rest with no running
		// transition, so no clear re-arms the attention in between.
		f := newHoldFixture(t)
		claim := f.usageClaim()
		hold := f.service.HoldRestWake(f.item.ID, f.thread)
		f.keepWithClaim(claim)
		f.move(engine.StateCancelled, engine.ReasonInterrupted)
		f.service.WaitWake()
		if state := f.claimState(claim); state != "released" {
			t.Fatalf("attention of a replaced rest is %s, want released", state)
		}
		f.release(hold, nil)
		if f.wakes() != 1 {
			t.Fatalf("the later rest delivered %d wakes, want 1", f.wakes())
		}
	})
}
