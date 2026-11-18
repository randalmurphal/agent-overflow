package workflowapp

import "fmt"

// Owed reports: a root run keeps the thread it is bound to now from rest
// (docs/specs/standing-agents.md, "Owned workers") while it runs and, once
// it rests, until that rest's report is in the thread's hands. Both are the
// store's: the engine's write of a rest counts the report owed in the same
// statement (store.UpdateWorkItemState), and the rest's wake job settles it
// when it ends, whatever it did (delivered the report into the thread's
// queue or a turn, which hold the thread from then on; found it a repeat;
// kept it back for a call that waits on the run; or found no thread or
// failed). A read of a thread's rest joins them to the run's binding as it
// stands, so a rebinding moves the hold with nothing told.
//
// The wake jobs are memory: a crash between a rest and its wake job loses
// the report, as it always could (wake_hold.go), and the engine's start
// forgets what the previous process owed (clearReportsOwed).

// clearReportsOwed forgets the reports a previous process owed, before the
// engine starts and emits anything.
func (s *Service) clearReportsOwed() error {
	if err := s.deps.Store.ClearWorkItemReportsOwed(); err != nil {
		return fmt.Errorf("workflow runtime: %w", err)
	}
	return nil
}

// settleOwedReport ends one rest's owed report, its wake job done, and
// tells the app, whose thread's rest may have changed with it. A write that
// fails is logged: the thread reads held by the run until the engine's next
// start.
func (s *Service) settleOwedReport(itemID string) {
	if err := s.deps.Store.SettleWorkItemReport(itemID); err != nil {
		s.deps.Logf("workflow report %s: record its wake job done: %v; the thread it reports to stays held until the next start", itemID, err)
		return
	}
	if s.deps.ReportSettled != nil {
		s.deps.ReportSettled(itemID)
	}
}
