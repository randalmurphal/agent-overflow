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

// reportFixture is holdFixture with the owed report observed: whether the
// store held the thread at the emit, and what it said when the app was told
// a report settled.
type reportFixture struct {
	*holdFixture
	mu        sync.Mutex
	settled   []bool
	changes   []store.WorkItemOriginChange
	atEmit    []bool
	sendFail  error
	reportsTo string
}

func newReportFixture(t *testing.T) *reportFixture {
	t.Helper()
	f := &reportFixture{holdFixture: newHoldFixture(t)}
	f.reportsTo = f.thread
	f.service.deps.ReportSettled = func(itemID string) {
		if itemID != f.item.ID {
			t.Errorf("told run %s settled; want %s", itemID, f.item.ID)
		}
		// The wake job's goroutine: a failed read is reported, not fatal.
		src, err := f.store.ReadThreadRestSources(f.reportsTo)
		if err != nil {
			t.Errorf("read the rest of %s: %v", f.reportsTo, err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.settled = append(f.settled, src.Workflow)
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
		owed := f.owed()
		f.mu.Lock()
		defer f.mu.Unlock()
		f.atEmit = append(f.atEmit, owed)
	})
}

// owe runs the parked run and parks it at its gate again, each write before
// its event, as the engine does.
func (f *reportFixture) owe() {
	f.t.Helper()
	f.write(engine.StateRunning, "")
	f.write(engine.StateNeedsHuman, engine.ReasonGate)
	f.transition(engine.StateRunning, engine.StateNeedsHuman)
}

// owed reports whether the store says a run holds the thread the fixture's
// run reports to.
func (f *reportFixture) owed() bool {
	f.t.Helper()
	src, err := f.store.ReadThreadRestSources(f.reportsTo)
	if err != nil {
		f.t.Fatal(err)
	}
	return src.Workflow
}

// expectSettledOnce checks the rest's report is owed until its wake job
// ends, and no longer after, with the app told once the store says so. A
// run the job unbound is bound back first, so a report still owed would
// show.
func (f *reportFixture) expectSettledOnce(t *testing.T, unblock func()) {
	t.Helper()
	if !f.owed() {
		t.Fatal("the report is not owed while its wake job waits")
	}
	unblock()
	if item, err := f.store.GetWorkItem(f.item.ID); err != nil {
		t.Fatal(err)
	} else if item.OriginThreadID == "" {
		if _, err := f.store.UpdateWorkItemOriginThread(f.item.ID, f.thread); err != nil {
			t.Fatal(err)
		}
	}
	if f.owed() {
		t.Fatal("the report is still owed after its wake job")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Equal(f.settled, []bool{false}) {
		t.Fatalf("told of settled reports with the thread held %v; want once, at rest", f.settled)
	}
	if !slices.Equal(f.atEmit, []bool{true}) {
		t.Fatalf("owed at the event's emit: %v; want true", f.atEmit)
	}
}

// A run's rest owes the thread it reports to a report from the engine's
// write of the rest until its wake job ends, whatever the job did with it.
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
		f.owe()
		f.service.WaitWake()
		if f.wakes() != 1 {
			t.Fatalf("first rest delivered %d wakes; want 1", f.wakes())
		}
		f.mu.Lock()
		f.settled, f.atEmit = nil, nil
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

// A bound run holds its thread with no gap: from the engine's write of its
// running state, through the write of its rest, before that rest's event,
// until the rest's wake job ends. Running again holds it again; a write
// that changes nothing owes nothing.
func TestABoundRunHoldsItsThreadFromRunningToItsReport(t *testing.T) {
	f := newReportFixture(t)
	if f.owed() {
		t.Fatal("a parked run with nothing owed holds its thread")
	}
	f.write(engine.StateNeedsHuman, engine.ReasonTakenOver)
	if f.owed() {
		t.Fatal("a write that left the run parked owes a report")
	}
	f.write(engine.StateRunning, "")
	if !f.owed() {
		t.Fatal("a running run does not hold its thread")
	}
	f.write(engine.StateNeedsHuman, engine.ReasonGate)
	if !f.owed() {
		t.Fatal("the run stopped holding its thread between its rest's write and its event")
	}
	unblock := f.block()
	f.transition(engine.StateRunning, engine.StateNeedsHuman)
	f.expectSettledOnce(t, unblock)

	f.write(engine.StateRunning, "")
	if !f.owed() {
		t.Fatal("a run running again does not hold its thread")
	}
}

// A run's binding moving moves its hold, running or with its report owed:
// the thread it left rests and the one it reports to now is held, with
// nothing told. Unbound, it holds no thread.
func TestABindingMovesTheHold(t *testing.T) {
	for _, held := range []string{"running", "report owed"} {
		t.Run(held, func(t *testing.T) {
			f := newReportFixture(t)
			other := "thread-2"
			if err := f.store.CreateThread(store.Thread{
				ID: other, ProjectID: f.project.ID, ProjectPath: f.project.Path,
				Title: "Other", Provider: "claude", Mode: threadmode.ModeChat, CreatedAt: 1, UpdatedAt: 1,
			}); err != nil {
				t.Fatal(err)
			}
			f.write(engine.StateRunning, "")
			if held == "report owed" {
				f.write(engine.StateNeedsHuman, engine.ReasonGate)
			}
			if _, err := f.store.UpdateWorkItemOriginThread(f.item.ID, other); err != nil {
				t.Fatal(err)
			}
			if f.owed() {
				t.Fatal("the thread the run left is still held")
			}
			f.reportsTo = other
			if !f.owed() {
				t.Fatal("the thread the run reports to now is not held")
			}
			if _, err := f.store.UpdateWorkItemOriginThread(f.item.ID, ""); err != nil {
				t.Fatal(err)
			}
			if f.owed() {
				t.Fatal("an unbound run still holds a thread")
			}
			if threads, err := f.store.ThreadsHeldByWorkflowRuns(); err != nil || len(threads) != 0 {
				t.Fatalf("held threads %v, %v; want none", threads, err)
			}
		})
	}
}

// The engine's start forgets the reports a previous process owed, whose
// wake jobs died with it, and keeps a running run's hold.
func TestTheEnginesStartForgetsTheReportsOwed(t *testing.T) {
	f := newReportFixture(t)
	f.write(engine.StateRunning, "")
	f.write(engine.StateNeedsHuman, engine.ReasonGate)
	if err := f.service.clearReportsOwed(); err != nil {
		t.Fatal(err)
	}
	if f.owed() {
		t.Fatal("a report owed before the engine's start still holds its thread")
	}
	f.write(engine.StateRunning, "")
	if err := f.service.clearReportsOwed(); err != nil {
		t.Fatal(err)
	}
	if !f.owed() {
		t.Fatal("a run running at the engine's start does not hold its thread")
	}
}
