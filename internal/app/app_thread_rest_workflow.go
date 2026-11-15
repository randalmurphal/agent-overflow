package app

import (
	"fmt"
)

// pauseRunsOfRestingThread pauses every workflow run that reports to the
// resting thread and has not ended, whoever started it, with its members'
// sessions ended (Pause and End Sessions), so no transition of the run starts
// the thread again (docs/specs/standing-agents.md, "At rest"). A run stays
// bound and resumable from the workflows view. The rest the pause makes is
// taken as delivered to the thread, which the Stop already tells, so it does
// not wake the thread however late it arrives; a rest after somebody resumes
// the run wakes it as usual. A run already paused or parked keeps its state,
// with its members' sessions ended and the resumes scheduled in its tree
// cancelled. Each run that could not be paused, or whose scheduled resumes
// could not be cancelled, is named in the result.
func (a *App) pauseRunsOfRestingThread(threadID string, result *ThreadAgentStop) {
	runs, err := a.store.ListWorkItemsReportingTo(threadID)
	if err != nil {
		result.Failures = append(result.Failures, "list its workflow runs: "+err.Error())
		return
	}
	if hook := a.restRunsListedForTest; hook != nil {
		hook(runs)
	}
	for _, run := range runs {
		// The run's wakes to the thread are held before anything below can
		// move it, whatever state it was listed in: a scheduled resume
		// already firing leaves it running for the pause, whose rest must
		// not wake the thread. The hold is released with the rest the pause
		// left, a parked run's own included, taken as delivered.
		hold := a.workflowApplication().HoldRestWake(run.ID, threadID)
		// Then, so no scheduled resume of the tree fires into the pause or
		// after it, the boot sweep's included.
		if err := a.workflowApplication().DisarmTreeAutoResumes(run.ID); err != nil {
			result.Failures = append(result.Failures, fmt.Sprintf("cancel the scheduled resumes of its workflow run %s: %v", run.ID, err))
		}
		err := a.pauseWorkflowRunAndEndSessions(run.ID)
		hold.ReleaseCurrentRest()
		if err != nil {
			result.Failures = append(result.Failures, fmt.Sprintf("pause its workflow run %s: %v", run.ID, err))
		}
	}
}
