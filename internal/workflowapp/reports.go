package workflowapp

import (
	"fmt"
	"sync"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/engine"
)

// Report obligations: a root run bound to a thread keeps that thread from
// rest (docs/specs/standing-agents.md, "Owned workers") from the moment it
// is bound while it runs, or from its running event, until the report of
// its last rest is in the thread's hands: that rest's wake job has ended,
// whatever it did (delivered the report into the thread's queue or a turn,
// which hold the thread from then on; found it a repeat; kept it back for a
// call that waits on the run; or found no thread or failed).
//
// The span has no gap. The engine writes a transition before it emits it,
// so between a rest's write and its event the store no longer says the run
// runs; the obligation, recorded from the running event (or the engine's
// start for a run already running, or the binding of a running run) and
// ended only by the rest's wake job, covers that interval. Every change is
// recorded before its event is emitted (HandleEngineEvent) and told to the
// app by the thread it changed (Deps.ObligationChanged), which a decision
// on that thread's rest compares against.
//
// The obligations are kept by run and indexed by the thread they report
// to, so a read for some threads costs those threads. It is memory: a crash
// between a rest and its wake job loses the report, as it always could
// (wake_hold.go).
type reportObligations struct {
	mu       sync.Mutex
	byRun    map[string]*reportObligation
	byThread map[string]map[string]*reportObligation
}

// reportObligation is one root run's: the thread it reports to (empty for
// none), what that thread shows of it, whether it runs, and the rests whose
// wake jobs have not ended.
type reportObligation struct {
	store.WorkItemBinding
	running bool
	rests   int
}

// seedReportObligations records the root runs the store says are running,
// before the engine starts and emits anything of them.
func (s *Service) seedReportObligations() error {
	runs, err := s.deps.Store.RunningRootWorkItemBindings()
	if err != nil {
		return fmt.Errorf("workflow runtime: read the running runs: %w", err)
	}
	for _, run := range runs {
		s.changeObligation(run, func(o *reportObligation) { o.running = true })
	}
	return nil
}

// noteReportTransition records a transition before it is emitted: a running
// event holds the run's thread; a rest owes its report until its wake job
// ends (settleOwedReport), counted exactly when afterWorkflowStateEvent
// queues that rest's wake job. A run not yet recorded is read from the
// store; a read that fails is logged and the transition recorded with no
// thread, which a binding names later.
func (s *Service) noteReportTransition(event engine.StateEvent) {
	var change func(*reportObligation)
	switch {
	case event.From == event.To:
		return
	case event.To == engine.StateRunning:
		change = func(o *reportObligation) { o.running = true }
	case restingWorkflowState(event.To):
		change = func(o *reportObligation) { o.running, o.rests = false, o.rests+1 }
	default:
		return
	}
	s.obligations.mu.Lock()
	known := s.obligations.byRun[event.ItemID]
	s.obligations.mu.Unlock()
	binding := store.WorkItemBinding{ID: event.ItemID}
	if known != nil {
		binding = known.WorkItemBinding
	} else {
		read, found, err := s.deps.Store.RootWorkItemBinding(event.ItemID)
		if err != nil {
			s.deps.Logf("workflow report %s: read the thread it reports to: %v", event.ItemID, err)
		}
		if !found {
			if err == nil {
				return
			}
		} else {
			binding = read
		}
	}
	s.changeObligation(binding, change)
}

// settleOwedReport ends one rest's owed report, its wake job done.
func (s *Service) settleOwedReport(itemID string) {
	s.obligations.mu.Lock()
	known := s.obligations.byRun[itemID]
	s.obligations.mu.Unlock()
	if known == nil {
		return
	}
	s.changeObligation(known.WorkItemBinding, func(o *reportObligation) { o.rests = max(0, o.rests-1) })
}

// BindingMoved records a run's binding moved (store.UpdateWorkItemOriginThread):
// its obligation, if it has one, moves with it to the thread it reports to
// now. The app is told of the change (Deps.OriginChanged).
func (s *Service) BindingMoved(change store.WorkItemOriginChange) {
	s.obligations.mu.Lock()
	known := s.obligations.byRun[change.ItemID]
	s.obligations.mu.Unlock()
	if known != nil {
		moved := known.WorkItemBinding
		moved.ThreadID = change.Current
		s.changeObligation(moved, func(*reportObligation) {})
	}
	if s.deps.OriginChanged != nil {
		s.deps.OriginChanged(change)
	}
}

// changeObligation applies change to binding's run, recorded under
// binding's thread, and drops an obligation that ends. The threads whose
// obligations changed are told after, once the record holds the change.
func (s *Service) changeObligation(binding store.WorkItemBinding, change func(*reportObligation)) {
	o := &s.obligations
	o.mu.Lock()
	if o.byRun == nil {
		o.byRun, o.byThread = map[string]*reportObligation{}, map[string]map[string]*reportObligation{}
	}
	current := o.byRun[binding.ID]
	if current == nil {
		current = &reportObligation{}
	}
	before := current.ThreadID
	o.unindexLocked(binding.ID, before)
	next := *current
	next.WorkItemBinding = binding
	change(&next)
	if next.running || next.rests > 0 {
		o.byRun[binding.ID] = &next
		if next.ThreadID != "" {
			if o.byThread[next.ThreadID] == nil {
				o.byThread[next.ThreadID] = map[string]*reportObligation{}
			}
			o.byThread[next.ThreadID][binding.ID] = &next
		}
	} else {
		delete(o.byRun, binding.ID)
	}
	o.mu.Unlock()
	if s.deps.ObligationChanged == nil {
		return
	}
	for _, thread := range (store.WorkItemOriginChange{Previous: before, Current: binding.ThreadID}).Threads() {
		s.deps.ObligationChanged(thread)
	}
}

func (o *reportObligations) unindexLocked(itemID, thread string) {
	runs := o.byThread[thread]
	delete(runs, itemID)
	if len(runs) == 0 {
		delete(o.byThread, thread)
	}
}

// ReportOwedTo reports whether a root run holds threadID from rest: one
// bound to it that runs, or whose rest's report is not yet in its hands;
// goal is the oldest such run's.
func (s *Service) ReportOwedTo(threadID string) (goal string, owed bool) {
	s.obligations.mu.Lock()
	defer s.obligations.mu.Unlock()
	var oldest *reportObligation
	for _, o := range s.obligations.byThread[threadID] {
		if oldest == nil || o.CreatedAt < oldest.CreatedAt || o.CreatedAt == oldest.CreatedAt && o.ID < oldest.ID {
			oldest = o
		}
	}
	if oldest == nil {
		return "", false
	}
	return oldest.Goal, true
}

// ReportObligations counts the runs that hold a thread from rest or are
// recorded running, for a test that checks nothing is left.
func (s *Service) ReportObligations() int {
	s.obligations.mu.Lock()
	defer s.obligations.mu.Unlock()
	return len(s.obligations.byRun)
}
