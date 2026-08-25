package main

import (
	"errors"
	"fmt"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/scheduler"
)

// The mutating half of project deletion (decision D25).
// `app_project_delete_workflow.go` says what the cleanup will do; everything
// here does it — stopping what is still running, ending the provider sessions
// those runs held, and removing the checkouts the app created for them.
//
// No branch is deleted here, and there is no code path from here that can
// delete one. That is the difference between this and the D23 discard
// (`app_workflow_discard_apply.go`): a discard is the human saying the work is
// not wanted, so it removes the checkout AND the branch that held its commits.
// Deleting a project says nothing about the work — only that the user is done
// managing it here — so every commit those runs produced stays reachable
// through its branch afterwards.

// RetainedWorktree is a checkout the deletion left on disk, and why. Git
// refuses to remove a worktree carrying uncommitted or untracked work, and the
// app does not override that; the user is told which ones so they can deal with
// them (`git worktree remove`) rather than discover them in `git worktree list`
// months later.
type RetainedWorktree struct {
	Path   string `json:"path"`
	Branch string `json:"branch"`
	Reason string `json:"reason"`
}

// cleanUpProjectWorkflowWork stops every run the project owns, ends the
// provider sessions they were running on, and removes the worktrees the app
// created for them. It reports the checkouts it did not manage to remove rather
// than failing over them: a checkout git declined is an outcome, not a fault,
// and the deletion the user asked for still has to happen.
//
// It MUST run before DeleteProject takes any thread lock (invariant 35).
// Cancelling a live run reaches App.InterruptTurn through the engine's teardown
// → Runner.Stop path, and InterruptTurn takes a.threadLocks().Lock(threadID) on
// the run's phase thread — one of the very locks DeleteProject would already be
// holding. Cancelling under those locks deadlocks.
//
// A failure here aborts the deletion with the project intact. Everything it
// does tolerates having been done before, so the retry picks up where this left
// off.
func (a *App) cleanUpProjectWorkflowWork(
	projectID string, footprint projectWorkflowFootprint,
) ([]RetainedWorktree, error) {
	project, err := a.store.GetProject(projectID)
	if err != nil {
		return nil, err
	}
	if err := a.stopProjectWorkflowRuns(footprint); err != nil {
		return nil, err
	}
	// Re-read rather than reuse the preview's collection: cancelling settles
	// runs, and the paths and branches recorded on a member can change as its
	// teardown lands. The same walk also reports what is still in flight, so the
	// check below is against the state the cancel actually left behind.
	checkouts, err := a.projectWorkflowCheckouts(footprint, project.Path)
	if err != nil {
		return nil, err
	}
	if len(checkouts.live) > 0 {
		return nil, fmt.Errorf(
			"run %s is still in flight after being cancelled; the project cannot be deleted while it runs",
			checkouts.live[0],
		)
	}
	return a.removeProjectRunWorktrees(project.Path, checkouts.targets)
}

// stopProjectWorkflowRuns cancels every run the project owns that is still in
// flight, then ends the provider sessions its phases and fan-out units were
// running on.
func (a *App) stopProjectWorkflowRuns(footprint projectWorkflowFootprint) error {
	var errs []error
	for _, root := range footprint.roots {
		members, err := a.workflowRunTree(root.ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := a.cancelWorkflowTreeMembers(members); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := a.stopWorkflowTreeSessions(members); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// stopWorkflowTreeSessions ends the provider processes the tree's phases and
// fan-out units were running on.
//
// Those runs have just been cancelled, so the sessions have nothing left to
// produce. Stopping them settles their open turn rows synchronously — session
// teardown runs triage's cleanup, which synthesizes the truncated turn-complete
// — and that is what lets DeleteProject's thread-activity check see an idle
// project instead of racing a provider that was told to stop and has not yet
// said so. A CLI that acks an interrupt and then never emits a result would
// otherwise block the deletion forever.
//
// The run's origin thread is deliberately untouched: that is the human's own
// conversation, which the ordinary thread teardown handles like any other.
func (a *App) stopWorkflowTreeSessions(members []store.WorkItem) error {
	var errs []error
	for _, member := range members {
		threadIDs, err := a.workflowRunThreadIDs(member)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, threadID := range threadIDs {
			if err := a.stopSession(threadID); err != nil {
				errs = append(errs, fmt.Errorf("stop workflow session on thread %s: %w", threadID, err))
			}
		}
	}
	return errors.Join(errs...)
}

// workflowRunThreadIDs lists the threads one run's phase attempts and fan-out
// units ran on, deduplicated — a re-entered phase reuses its thread.
func (a *App) workflowRunThreadIDs(item store.WorkItem) ([]string, error) {
	phases, err := a.store.ListWorkItemPhases(item.ID)
	if err != nil {
		return nil, fmt.Errorf("list phases of run %s: %w", item.ID, err)
	}
	units, err := a.store.ListWorkItemUnits(item.ID)
	if err != nil {
		return nil, fmt.Errorf("list fan-out units of run %s: %w", item.ID, err)
	}
	threadIDs := make([]string, 0, len(phases)+len(units))
	seen := make(map[string]bool, len(phases)+len(units))
	add := func(threadID string) {
		if threadID == "" || seen[threadID] {
			return
		}
		seen[threadID] = true
		threadIDs = append(threadIDs, threadID)
	}
	for _, phase := range phases {
		add(phase.ThreadID)
	}
	for _, unit := range units {
		add(unit.ThreadID)
	}
	return threadIDs, nil
}

// removeProjectRunWorktrees removes each checkout the app created for the
// project's runs, and returns the ones that are still there afterwards.
//
// The run records' workspace pointers are deliberately not cleared: the rows
// are deleted a few statements later in the same call. The only window in which
// a pointer outlives its checkout is between a deletion that aborted after this
// ran and the retry that follows it, and the retry re-derives everything from
// the registry — a checkout already removed is simply no longer registered.
// Writing to rows that are about to be dropped, to close a window nothing reads
// through, is code that cannot matter.
func (a *App) removeProjectRunWorktrees(
	projectPath string, targets []workflowWorktreeTarget,
) ([]RetainedWorktree, error) {
	retained := make([]RetainedWorktree, 0)
	if len(targets) == 0 {
		return retained, nil
	}
	registry, err := a.readProjectWorktrees(projectPath, "project deletion cleanup")
	if err != nil {
		return nil, err
	}
	for _, target := range targets {
		if isProjectCheckout(projectPath, target.path) {
			// projectWorkflowCheckouts already filters the user's own checkout
			// out; the mutating step re-checks because acting on it is the one
			// mistake here that could not be undone.
			continue
		}
		branch, registered := registeredWorktreeBranch(registry, target)
		if !registered {
			// There is nothing to ask git — it no longer knows this path, or
			// there is no repository left to ask, which is the same absent
			// registry. A directory still sitting there is reported, so the user
			// is never told a cleanup happened that did not.
			if !worktreeDirectoryPresent(target.path) {
				continue
			}
			reason := retainedNotRegistered
			if !registry.present {
				reason = retainedRepositoryGone
			}
			retained = append(retained, RetainedWorktree{Path: target.path, Branch: branch, Reason: reason})
			continue
		}
		// Non-force on purpose. `git worktree remove` refuses a checkout carrying
		// uncommitted or untracked work, and that refusal is the whole safety
		// valve here: the app created this directory so it cleans it up, but it
		// does not get to destroy what the user left inside it.
		if err := a.gitCore().RemoveWorktree(projectPath, target.path); err != nil {
			retained = append(retained, RetainedWorktree{
				Path: target.path, Branch: branch,
				Reason: a.explainRetainedWorktree(target.path, err),
			})
			continue
		}
		if a.workspaceFiles != nil {
			a.workspaceFiles.Invalidate(target.path)
		}
	}
	return retained, nil
}

// explainRetainedWorktree says why a checkout survived its own removal, by
// asking the checkout what state it is in rather than by reading git's message.
//
// Matching git's refusal text would be a string match against a sentence that
// is localized and free to change between versions. The working-tree question
// is instead the same one `git worktree remove` asks itself, so a dirty answer
// explains the refusal structurally and in words the user can act on. When it
// does not explain it — the checkout is clean, or the question itself failed —
// git broke rather than refused, and its own words are reported unchanged
// rather than replaced by a guess.
func (a *App) explainRetainedWorktree(path string, removeErr error) string {
	if _, total, err := a.gitCore().WorkingTreeChanges(path, 0); err == nil && total > 0 {
		return gitops.RetainedDirtyReason(total)
	}
	return removeErr.Error()
}

// refreshWorkflowSchedule recomputes the automation schedule after rows have
// been deleted, so the scheduler drops them from its in-memory arming instead
// of firing a trigger whose definition is gone.
//
// A nil scheduler — a bare test App, or a boot that never reached workflow
// init — has no schedule to correct, and neither does a stopped one: it arms
// nothing, so the deleted automations are already unreachable.
func (a *App) refreshWorkflowSchedule() error {
	if a.workflowScheduler == nil {
		return nil
	}
	if err := a.workflowScheduler.Refresh(); err != nil && !errors.Is(err, scheduler.ErrStopped) {
		return err
	}
	return nil
}
