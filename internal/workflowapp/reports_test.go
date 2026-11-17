package workflowapp

import (
	"errors"
	"slices"
	"sync"
	"testing"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/wake"
)

// reportFixture is holdFixture with the owed report observed: what the
// emit saw and which settlements the app was told of.
type reportFixture struct {
	*holdFixture
	mu       sync.Mutex
	settled  []string
	changes  []store.WorkItemOriginChange
	atEmit   []bool
	sendFail error
}

func newReportFixture(t *testing.T) *reportFixture {
	t.Helper()
	f := &reportFixture{holdFixture: newHoldFixture(t)}
	f.service.deps.ReportSettled = func(itemID string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.settled = append(f.settled, itemID)
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

// owe emits the run's move from running to the rest the store holds, as
// the engine does, recording whether the report was owed when the event
// could first be observed.
func (f *reportFixture) owe() {
	f.t.Helper()
	item, err := f.store.GetWorkItem(f.item.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	f.service.HandleEngineEvent(eventchan.WorkflowItemState, engine.StateEvent{
		ItemID: item.ID, ProjectID: item.ProjectID,
		From: engine.StateRunning, To: engine.State(item.State), Reason: engine.Reason(item.Reason),
	}, func(eventchan.Channel, any) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.atEmit = append(f.atEmit, f.owed())
	})
}

func (f *reportFixture) owed() bool {
	return slices.Contains(f.service.OwedReports(), f.item.ID)
}

// expectSettledOnce checks the rest's report is owed until its wake job
// ends, and no longer after, with the app told once.
func (f *reportFixture) expectSettledOnce(t *testing.T, unblock func()) {
	t.Helper()
	if !f.owed() {
		t.Fatal("the report is not owed while its wake job waits")
	}
	unblock()
	if f.owed() {
		t.Fatalf("the report is still owed after its wake job: %v", f.service.OwedReports())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Equal(f.atEmit, []bool{true}) {
		t.Fatalf("owed at the event's emit: %v; want true", f.atEmit)
	}
	if !slices.Equal(f.settled, []string{f.item.ID}) {
		t.Fatalf("the app was told of %v; want the run once", f.settled)
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
		f.mu.Lock()
		f.settled = nil
		f.mu.Unlock()
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
		if want := []store.WorkItemOriginChange{{Previous: f.thread}}; !slices.Equal(f.changes, want) {
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

// A run that runs owes nothing: the store says it runs. Its report is owed
// from its rest's event, recorded before that event is emitted, until the
// rest's wake job ends; running again owes nothing more.
func TestARunOwesItsReportFromItsRestsEvent(t *testing.T) {
	f := newReportFixture(t)
	if err := f.store.UpdateWorkItemState(f.item.ID, string(engine.StateRunning), "", 0); err != nil {
		t.Fatal(err)
	}
	f.service.HandleEngineEvent(eventchan.WorkflowItemState, engine.StateEvent{
		ItemID: f.item.ID, From: engine.StateNeedsHuman, To: engine.StateRunning,
	}, func(eventchan.Channel, any) {})
	if f.owed() {
		t.Fatal("a running run is recorded as owing a report")
	}
	f.write(engine.StateNeedsHuman, engine.ReasonGate)
	unblock := f.block()
	f.owe()
	f.expectSettledOnce(t, unblock)

	f.service.HandleEngineEvent(eventchan.WorkflowItemState, engine.StateEvent{
		ItemID: f.item.ID, From: engine.StateNeedsHuman, To: engine.StateRunning,
	}, func(eventchan.Channel, any) {})
	if f.owed() {
		t.Fatal("a run running again is recorded as owing a report")
	}
	f.service.WaitWake()
}
