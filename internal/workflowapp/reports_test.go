package workflowapp

import (
	"errors"
	"slices"
	"sync"
	"testing"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/wake"
)

// reportFixture is holdFixture with the report obligation observed: what
// the emit saw and which threads the app was told of.
type reportFixture struct {
	*holdFixture
	mu       sync.Mutex
	told     []string
	changes  []store.WorkItemOriginChange
	atEmit   []bool
	sendFail error
}

func newReportFixture(t *testing.T) *reportFixture {
	t.Helper()
	f := &reportFixture{holdFixture: newHoldFixture(t)}
	f.service.deps.ObligationChanged = func(threadID string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.told = append(f.told, threadID)
	}
	f.service.deps.OriginChanged = func(change store.WorkItemOriginChange) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.changes = append(f.changes, change)
	}
	queue := f.service.deps.WakeDelivery.QueueMessage
	f.service.deps.WakeDelivery.QueueMessage = func(threadID, workflowID string, message wake.Message, onDurable func()) error {
		if f.sendFail != nil {
			return f.sendFail
		}
		return queue(threadID, workflowID, message, onDurable)
	}
	return f
}

// transition emits the run's move from one state to another through the
// service, as the engine does after its write, recording whether the
// thread was held when the event could first be observed.
func (f *reportFixture) transition(from, to engine.State) {
	f.t.Helper()
	f.service.HandleEngineEvent(eventchan.WorkflowItemState, engine.StateEvent{
		ItemID: f.item.ID, ProjectID: f.item.ProjectID, From: from, To: to,
	}, func(eventchan.Channel, any) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.atEmit = append(f.atEmit, f.owed())
	})
}

// owe emits the run's move from running to the rest the store holds.
func (f *reportFixture) owe() {
	f.t.Helper()
	item, err := f.store.GetWorkItem(f.item.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	f.transition(engine.StateRunning, engine.State(item.State))
}

func (f *reportFixture) owed() bool {
	_, owed := f.service.ReportOwedTo(f.thread)
	return owed
}

// toldSince lists the threads the app was told of after the first n.
func (f *reportFixture) toldSince(n int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.told[n:])
}

func (f *reportFixture) toldCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.told)
}

// expectSettledOnce checks the rest's report is owed until its wake job
// ends, and no longer after, with the app told of the thread as it ends.
func (f *reportFixture) expectSettledOnce(t *testing.T, unblock func()) {
	t.Helper()
	if !f.owed() {
		t.Fatal("the report is not owed while its wake job waits")
	}
	before := f.toldCount()
	unblock()
	if f.owed() {
		t.Fatal("the report is still owed after its wake job")
	}
	if n := f.service.ReportObligations(); n != 0 {
		t.Fatalf("%d obligations left after the wake job", n)
	}
	if told := f.toldSince(before); !slices.Contains(told, f.thread) {
		t.Fatalf("the app was told of %v as the report ended; want %s", told, f.thread)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Equal(f.atEmit, []bool{true}) {
		t.Fatalf("owed at the event's emit: %v; want true", f.atEmit)
	}
}

// A run's rest owes the thread it reports to a report from before its
// event is emitted until its wake job ends, whatever the job did with it.
func TestARunsRestOwesItsReportUntilItsWakeJobEnds(t *testing.T) {
	t.Run("delivered", func(t *testing.T) {
		f := newReportFixture(t)
		unblock := f.block()
		f.owe()
		f.expectSettledOnce(t, unblock)
		if f.wakes() != 1 {
			t.Fatalf("delivered %d wakes; want 1", f.wakes())
		}
	})
	t.Run("coalesced", func(t *testing.T) {
		f := newReportFixture(t)
		f.rest()
		if f.wakes() != 1 {
			t.Fatalf("first rest delivered %d wakes; want 1", f.wakes())
		}
		unblock := f.block()
		f.owe()
		f.expectSettledOnce(t, unblock)
		if f.wakes() != 1 {
			t.Fatalf("the repeat delivered: %d wakes; want 1", f.wakes())
		}
	})
	t.Run("held back", func(t *testing.T) {
		f := newReportFixture(t)
		hold := f.service.HoldRestWake(f.item.ID, f.thread)
		unblock := f.block()
		f.owe()
		f.expectSettledOnce(t, unblock)
		if f.wakes() != 0 {
			t.Fatalf("a held rest delivered %d wakes", f.wakes())
		}
		f.release(hold, f.read())
	})
	t.Run("no thread", func(t *testing.T) {
		f := newReportFixture(t)
		if _, _, err := f.store.ArchiveThread(f.thread); err != nil {
			t.Fatal(err)
		}
		unblock := f.block()
		f.owe()
		f.expectSettledOnce(t, unblock)
		f.mu.Lock()
		defer f.mu.Unlock()
		// The stale binding's clear names the thread it left.
		if want := []store.WorkItemOriginChange{{ItemID: f.item.ID, Previous: f.thread}}; !slices.Equal(f.changes, want) {
			t.Fatalf("binding changes %+v; want %+v", f.changes, want)
		}
	})
	t.Run("failed", func(t *testing.T) {
		f := newReportFixture(t)
		f.sendFail = errors.New("the thread refused it")
		unblock := f.block()
		f.owe()
		f.expectSettledOnce(t, unblock)
	})
}

// A bound run holds its thread from its running event with no gap: through
// the engine's write of its rest, before that rest's event, until the
// rest's wake job ends. Running again holds it again.
func TestABoundRunHoldsItsThreadFromRunningToItsReport(t *testing.T) {
	f := newReportFixture(t)
	if f.owed() {
		t.Fatal("a parked run with nothing owed holds its thread")
	}
	f.write(engine.StateRunning, "")
	f.transition(engine.StateNeedsHuman, engine.StateRunning)
	if !f.owed() {
		t.Fatal("a running run does not hold its thread")
	}
	if goal, _ := f.service.ReportOwedTo(f.thread); goal != f.item.Goal {
		t.Fatalf("the hold reads goal %q; want %q", goal, f.item.Goal)
	}
	// The engine's write of the rest, before its event.
	f.write(engine.StateNeedsHuman, engine.ReasonGate)
	if !f.owed() {
		t.Fatal("the run stopped holding its thread between its rest's write and its event")
	}
	unblock := f.block()
	f.mu.Lock()
	f.atEmit = nil
	f.mu.Unlock()
	f.owe()
	f.expectSettledOnce(t, unblock)

	f.write(engine.StateRunning, "")
	f.transition(engine.StateNeedsHuman, engine.StateRunning)
	if !f.owed() {
		t.Fatal("a run running again does not hold its thread")
	}
	f.service.WaitWake()
}

// A running run's binding moving moves its hold: the thread it left rests,
// the one it reports to now is held, and both are told; unbound, it holds
// no thread.
func TestABindingMovesTheHold(t *testing.T) {
	f := newReportFixture(t)
	other := "thread-2"
	if err := f.store.CreateThread(store.Thread{
		ID: other, ProjectID: f.project.ID, ProjectPath: f.project.Path,
		Title: "Other", Provider: "claude", Mode: threadmode.ModeChat, CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	f.write(engine.StateRunning, "")
	f.transition(engine.StateNeedsHuman, engine.StateRunning)
	before := f.toldCount()
	change, err := f.store.UpdateWorkItemOriginThread(f.item.ID, other)
	if err != nil {
		t.Fatal(err)
	}
	f.service.BindingMoved(change)
	if f.owed() {
		t.Fatal("the thread the run left is still held")
	}
	if _, owed := f.service.ReportOwedTo(other); !owed {
		t.Fatal("the thread the run reports to now is not held")
	}
	if told := f.toldSince(before); !slices.Contains(told, f.thread) || !slices.Contains(told, other) {
		t.Fatalf("the app was told of %v; want both threads", told)
	}
	f.mu.Lock()
	changes := slices.Clone(f.changes)
	f.mu.Unlock()
	if want := []store.WorkItemOriginChange{{ItemID: f.item.ID, Previous: f.thread, Current: other}}; !slices.Equal(changes, want) {
		t.Fatalf("binding changes %+v; want %+v", changes, want)
	}
	change, err = f.store.UpdateWorkItemOriginThread(f.item.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	f.service.BindingMoved(change)
	if _, owed := f.service.ReportOwedTo(other); owed {
		t.Fatal("an unbound run still holds a thread")
	}
}

// The engine's start records the runs the store says are running, before
// it emits anything of them.
func TestTheEnginesStartRecordsTheRunningRuns(t *testing.T) {
	f := newReportFixture(t)
	f.write(engine.StateRunning, "")
	if err := f.service.seedReportObligations(); err != nil {
		t.Fatal(err)
	}
	if !f.owed() {
		t.Fatal("a run running at the engine's start does not hold its thread")
	}
}
