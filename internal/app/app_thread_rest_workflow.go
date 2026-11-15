package app

import (
	"fmt"

	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflowapp"
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
	for _, run := range runs {
		// First, so no scheduled resume of the tree fires into the pause or
		// after it, the boot sweep's included.
		if err := a.workflowApplication().DisarmTreeAutoResumes(run.ID); err != nil {
			result.Failures = append(result.Failures, fmt.Sprintf("cancel the scheduled resumes of its workflow run %s: %v", run.ID, err))
		}
		// Only a running root rests on the pause; a parked one keeps its own
		// rest, and only its running descendants are paused.
		var hold *workflowapp.WakeHold
		if engine.State(run.State) == engine.StateRunning {
			hold = a.workflowApplication().HoldRestWake(run.ID, threadID)
		}
		err := a.pauseWorkflowRunAndEndSessions(run.ID)
		if hold != nil {
			hold.ReleaseCurrentRest()
		}
		if err != nil {
			result.Failures = append(result.Failures, fmt.Sprintf("pause its workflow run %s: %v", run.ID, err))
		}
	}
}
