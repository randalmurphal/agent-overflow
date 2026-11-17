package workflowapp

import (
	"maps"
	"slices"
	"sync"

	"agent-overflow/internal/workflow/engine"
)

// Owed reports: a run reporting to a thread keeps that thread from rest
// (docs/specs/standing-agents.md, "Owned workers") from the time it runs
// until its report is in the thread's hands. The store says when it runs;
// this records the rest of that span, the report owed: from the run's rest
// until that rest's wake job has ended, whatever it did (delivered the
// report into the thread's queue or a turn, which hold the thread from then
// on; found it a repeat; kept it back for a call that waits on the run; or
// found no thread or failed). A rest is recorded before its event is
// emitted to anything that reassesses a thread on it (HandleEngineEvent).
// Between the engine's write of the rest and that record the store says
// the run rests and nothing is owed yet; the event that follows is the
// change a decision on the thread's rest compares against
// (app_thread_rest_assess.go).
//
// It is memory: a crash between a rest and its wake job loses the report,
// as it always could (wake_hold.go).
type owedReports struct {
	mu    sync.Mutex
	byRun map[string]int
}

// noteReportTransition records a rest before it is emitted: it owes the
// report until its wake job ends (settleOwedReport). It counts a rest
// exactly when afterWorkflowStateEvent queues that rest's wake job.
func (s *Service) noteReportTransition(event engine.StateEvent) {
	if event.From == event.To || !restingWorkflowState(event.To) {
		return
	}
	s.reports.mu.Lock()
	defer s.reports.mu.Unlock()
	if s.reports.byRun == nil {
		s.reports.byRun = map[string]int{}
	}
	s.reports.byRun[event.ItemID]++
}

// settleOwedReport ends one rest's owed report, its wake job done, and
// tells the app, whose thread's rest may have changed with it.
func (s *Service) settleOwedReport(itemID string) {
	s.reports.mu.Lock()
	if s.reports.byRun[itemID] > 1 {
		s.reports.byRun[itemID]--
	} else {
		delete(s.reports.byRun, itemID)
	}
	s.reports.mu.Unlock()
	if s.deps.ReportSettled != nil {
		s.deps.ReportSettled(itemID)
	}
}

// OwedReports lists the runs that rested and whose rest's report is not
// yet in the thread's hands. A run that runs is not among them: the store
// says it runs.
func (s *Service) OwedReports() []string {
	s.reports.mu.Lock()
	defer s.reports.mu.Unlock()
	return slices.Collect(maps.Keys(s.reports.byRun))
}
