package app

import (
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/worktreesetupapp"
)

// GetThreadWorktreeSetup returns the current worktree-setup state for a
// thread: the live run if one is going, the retained failure if the last one
// failed, otherwise idle. A durable failure with no retained run (the app
// restarted since) reports "failed" with no steps and no output — the failure
// survived; the transcript did not.
//
// LocalOnly: the payload is the stdout/stderr of local commands run against
// the user's checkout, the same data class as GetTerminalReplay.
//
//ao:scope terminal:operate
func (a *App) GetThreadWorktreeSetup(threadID string) (WorktreeSetupRunState, error) {
	state, err := a.worktreeSetupService().GetThreadWorktreeSetup(threadID)
	return projectWorktreeSetupState(state), err
}

// RetryThreadWorktreeSetup re-runs the project's recipe against the thread's
// current worktree. It re-reads the recipe rather than replaying the failed
// run's copy, so fixing the recipe in Settings and hitting Retry does what the
// user means.
//
// Refusals are loud and specific: a thread that is no longer in a worktree, a
// project with nothing configured, and a run already in flight are three
// different mistakes and say so.
//
// LocalOnly: this executes the project's argv commands. RCE-equivalent.
//
//ao:scope terminal:operate
func (a *App) RetryThreadWorktreeSetup(threadID string) error {
	return a.worktreeSetupService().RetryThreadWorktreeSetup(threadID)
}

func (a *App) worktreeSetupService() *worktreesetupapp.Service {
	a.worktreeSetupAppOnce.Do(func() {
		var setupStore worktreesetupapp.Store
		if a.store != nil {
			setupStore = a.store
		}
		a.worktreeSetupApp = worktreesetupapp.New(worktreesetupapp.Config{
			Store:         setupStore,
			Events:        appWorktreeSetupEvents{app: a},
			Context:       a.lifeCtx,
			ShutdownError: ErrShuttingDown,
		})
	})
	return a.worktreeSetupApp
}

type appWorktreeSetupEvents struct{ app *App }

func (e appWorktreeSetupEvents) Setup(event worktreesetupapp.Event) {
	e.app.emitEvent(eventchan.WorktreeSetup, projectWorktreeSetupEvent(event))
}

func (e appWorktreeSetupEvents) ThreadUpdated(thread store.Thread) {
	e.app.emitEvent(eventchan.ThreadUpdated, triage.ThreadUpdateEvent{Action: triage.ThreadActionFull, Thread: &thread})
}

func (a *App) startThreadWorktreeSetup(thread store.Thread) {
	a.worktreeSetupService().StartThread(thread)
}

func (a *App) launchThreadWorktreeSetup(thread store.Thread, requireRecipe bool) error {
	return a.worktreeSetupService().LaunchThread(thread, requireRecipe)
}

func (a *App) cancelThreadWorktreeSetup(threadID string) {
	a.worktreeSetupService().CancelThread(threadID)
}

func (a *App) releaseThreadWorktreeSetup(threadID, workspacePath string) {
	a.worktreeSetupService().ReleaseThread(threadID, workspacePath)
}

func (a *App) stopThreadWorktreeSetups() {
	a.worktreeSetupService().Stop()
}

func (a *App) sweepCrashedWorktreeSetups() {
	a.worktreeSetupService().SweepCrashed()
}

func (a *App) cancelWorktreeSetupsForPath(worktreePath string) {
	a.worktreeSetupService().CancelPath(worktreePath)
}

func projectWorktreeSetupState(state worktreesetupapp.RunState) WorktreeSetupRunState {
	return WorktreeSetupRunState{
		ThreadID: state.ThreadID, RunID: state.RunID, State: state.State,
		Steps: projectWorktreeSetupSteps(state.Steps), StepStatuses: state.StepStatuses,
		Output: state.Output, OutputSeq: state.OutputSeq, Error: state.Error,
		WorktreePath: state.WorktreePath, StartedAt: state.StartedAt, FinishedAt: state.FinishedAt,
	}
}

func projectWorktreeSetupSteps(steps []worktreesetupapp.Step) []WorktreeSetupStep {
	projected := make([]WorktreeSetupStep, len(steps))
	for index, step := range steps {
		projected[index] = WorktreeSetupStep{Index: step.Index, Kind: step.Kind, Label: step.Label, Argv: step.Argv}
	}
	return projected
}

func projectWorktreeSetupEvent(event worktreesetupapp.Event) WorktreeSetupEvent {
	return WorktreeSetupEvent{
		Phase: event.Phase, ThreadID: event.ThreadID, RunID: event.RunID,
		WorktreePath: event.WorktreePath, Steps: projectWorktreeSetupSteps(event.Steps),
		StepIndex: event.StepIndex, Seq: event.Seq, Chunk: event.Chunk,
		State: event.State, Error: event.Error, StartedAt: event.StartedAt,
		FinishedAt: event.FinishedAt,
	}
}
