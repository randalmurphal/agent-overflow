package workflowapp

import (
	"fmt"
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
// found no thread or failed).
//
// The span has no gap. The engine writes a transition before it emits it,
// so between a rest's write and its event the store no longer says running
// and the event has not been seen. A run is therefore recorded from its
// running event, or from the engine's start for one already running, until
// its last rest's wake job ends, and the record is set before the event is
// emitted to anything that reassesses a thread on it (HandleEngineEvent).
//
// It is memory: a crash between a rest and its wake job loses the report,
// as it always could (wake_hold.go).
type owedReports struct {
	mu    sync.Mutex
	byRun map[string]owedReport
}

// owedReport is one run's record: running since its running event, and the
// rests whose wake jobs have not ended.
type owedReport struct {
	running bool
	rests   int
}

// seedRunningReports records the runs the store says are running, before
// the engine starts and emits anything of them.
func (s *Service) seedRunningReports() error {
	ids, err := s.deps.Store.RunningRootWorkItemIDs()
	if err != nil {
		return fmt.Errorf("workflow runtime: read the running runs: %w", err)
	}
	s.reports.mu.Lock()
	defer s.reports.mu.Unlock()
	for _, id := range ids {
		s.setReportLocked(id, func(report *owedReport) { report.running = true })
	}
	return nil
}

// noteReportTransition records a transition before it is emitted: running
// from a running event; a rest ends running and owes the report until its
// wake job ends (settleOwedReport). It counts a rest exactly when
// afterWorkflowStateEvent queues that rest's wake job.
func (s *Service) noteReportTransition(event engine.StateEvent) {
	switch {
	case event.To == engine.StateRunning:
		s.reports.mu.Lock()
		s.setReportLocked(event.ItemID, func(report *owedReport) { report.running = true })
		s.reports.mu.Unlock()
	case event.From != event.To && restingWorkflowState(event.To):
		s.reports.mu.Lock()
		s.setReportLocked(event.ItemID, func(report *owedReport) { report.running, report.rests = false, report.rests+1 })
		s.reports.mu.Unlock()
	}
}

// settleOwedReport ends one rest's owed report, its wake job done, and
// tells the app, whose thread's rest may have changed with it.
func (s *Service) settleOwedReport(itemID string) {
	s.reports.mu.Lock()
	s.setReportLocked(itemID, func(report *owedReport) { report.rests = max(0, report.rests-1) })
	s.reports.mu.Unlock()
	if s.deps.ReportSettled != nil {
		s.deps.ReportSettled(itemID)
	}
}

func (s *Service) setReportLocked(itemID string, change func(*owedReport)) {
	report := s.reports.byRun[itemID]
	change(&report)
	if !report.running && report.rests == 0 {
		delete(s.reports.byRun, itemID)
		return
	}
	if s.reports.byRun == nil {
		s.reports.byRun = map[string]owedReport{}
	}
	s.reports.byRun[itemID] = report
}

// ReportingRuns lists the runs that may still owe the thread they report
// to a report: running, or rested with the rest's report not yet in the
// thread's hands. The store's binding names the thread.
func (s *Service) ReportingRuns() []string {
	s.reports.mu.Lock()
	defer s.reports.mu.Unlock()
	return slices.Collect(maps.Keys(s.reports.byRun))
}
