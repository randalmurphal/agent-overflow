package main

import (
	"errors"
	"fmt"
	"strings"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/engine"
)

// The mutating half of discard (decision D23). `app_workflow_discard.go` builds
// the target set and the loss preview a human consents to; everything here acts
// on that same set — stopping what is still running, removing the checkouts, and
// deleting the branches they held.

// discardWorkflowTree stops what is still running, then removes every checkout
// the tree registered and deletes the branches those checkouts held — the only
// place in the app that deletes a branch.
//
// Branch deletion is what makes discard different from cleanup: cleanup frees a
// checkout and leaves the commits recoverable through the branch, while discard
// is the human saying the work itself is not wanted. That is why it requires the
// preview's consent and refuses while anything is still running.
func (a *App) discardWorkflowTree(root store.WorkItem) (WorkflowDiscardResult, error) {
	result := WorkflowDiscardResult{
		Members: make([]string, 0), Cancelled: make([]string, 0),
		RemovedWorktrees: make([]string, 0), DeletedBranches: make([]string, 0),
	}
	members, err := a.workflowRunTree(root.ID)
	if err != nil {
		return result, err
	}
	cancelled, err := a.cancelWorkflowTreeForDiscard(members)
	if err != nil {
		return result, err
	}
	result.Cancelled = cancelled
	// Re-read: cancelling settles runs, and the paths and branches recorded on
	// a member can change as its teardown lands. workflowRunTree re-reads the
	// root as well as its callees, so the state checked below is the state the
	// cancel left behind.
	members, err = a.workflowRunTree(root.ID)
	if err != nil {
		return result, err
	}
	for _, member := range members {
		result.Members = append(result.Members, member.ID)
		if engine.State(member.State) == engine.StateRunning {
			return result, fmt.Errorf(
				"workflow discard %s: run %s is still in flight; cancel it before discarding the tree",
				root.ID, member.ID,
			)
		}
	}
	project, err := a.store.GetProject(root.ProjectID)
	if err != nil {
		return result, err
	}
	targets, err := a.workflowTreeWorktrees(members, "", project.Path)
	if err != nil {
		return result, err
	}
	if len(targets) == 0 {
		// Nothing of the run's own was ever checked out (or it is already gone):
		// the discard is the receipt the caller writes, and git has nothing to do.
		return result, nil
	}
	registry, err := a.readProjectWorktrees(project.Path, fmt.Sprintf("workflow discard %s", root.ID))
	if err != nil {
		return result, err
	}
	if !registry.present {
		// The repository that registered these checkouts and held their branches
		// is no longer on disk, so there is nothing left for git to destroy and
		// no repository to ask. Clearing the pointers is the whole of what is
		// left to do; refusing instead would make the run undiscardable forever.
		return result, a.clearDiscardedTreeWorkspaces(members)
	}
	var errs []error
	for _, target := range targets {
		removed, err := a.removeDiscardedWorktree(project.Path, registry.worktrees, target)
		if err != nil {
			errs = append(errs, err)
		}
		if removed.path != "" {
			result.RemovedWorktrees = append(result.RemovedWorktrees, removed.path)
		}
		if removed.branch != "" {
			result.DeletedBranches = append(result.DeletedBranches, removed.branch)
		}
	}
	if joined := errors.Join(errs...); joined != nil {
		return result, fmt.Errorf("workflow discard %s: %w", root.ID, joined)
	}
	return result, a.clearDiscardedTreeWorkspaces(members)
}

// cancelWorkflowTreeForDiscard stops the members still in flight and reports
// which ones it stopped. Cancel is itself tree-aware, so the shallowest live
// member brings its own descendants down; deeper members are skipped once an
// ancestor has cancelled them.
func (a *App) cancelWorkflowTreeForDiscard(members []store.WorkItem) ([]string, error) {
	stopped := make([]string, 0)
	live := make([]store.WorkItem, 0)
	for _, member := range members {
		if engine.State(member.State) == engine.StateRunning {
			live = append(live, member)
		}
	}
	if len(live) == 0 {
		return stopped, nil
	}
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return stopped, err
	}
	cancelled := make(map[string]bool, len(live))
	var errs []error
	for _, member := range live {
		if cancelled[member.ParentItemID] {
			cancelled[member.ID] = true
			stopped = append(stopped, member.ID)
			continue
		}
		if err := workflowEngine.Cancel(member.ID); err != nil {
			errs = append(errs, fmt.Errorf("cancel run %s before discard: %w", member.ID, err))
			continue
		}
		cancelled[member.ID] = true
		stopped = append(stopped, member.ID)
	}
	return stopped, errors.Join(errs...)
}

// discardRemoval is what one target actually cost: the checkout that was
// removed and the branch that was deleted, either of which can be empty when
// the target had already been released.
type discardRemoval struct {
	path   string
	branch string
}

// removeDiscardedWorktree removes one checkout and deletes its branch. An
// unregistered path is left alone — it is not this project's to remove — but
// its branch is still deleted if it exists, because the branch is the run's
// either way.
func (a *App) removeDiscardedWorktree(
	projectPath string, worktrees []gitops.Worktree, target discardWorktreeTarget,
) (discardRemoval, error) {
	if isProjectCheckout(projectPath, target.path) {
		// workflowTreeWorktrees already filters these out; the destructive step
		// re-checks because removing the user's checkout or deleting the branch
		// they are sitting on is the one mistake here that cannot be undone.
		return discardRemoval{}, nil
	}
	registered := false
	branch := target.branch
	for _, worktree := range worktrees {
		if gitops.SameFilesystemPath(worktree.Path, target.path) {
			registered = true
			if branch == "" {
				branch = worktree.Branch
			}
			break
		}
	}
	removal := discardRemoval{}
	if registered {
		if err := a.gitCore().RemoveWorktreeForce(projectPath, target.path, true); err != nil {
			return removal, fmt.Errorf("remove worktree %q: %w", target.path, err)
		}
		removal.path = target.path
		if a.workspaceFiles != nil {
			a.workspaceFiles.Invalidate(target.path)
		}
	}
	if branch == "" {
		return removal, nil
	}
	if err := a.gitCore().DeleteBranch(projectPath, branch, true); err != nil {
		return removal, fmt.Errorf("delete branch %q: %w", branch, err)
	}
	removal.branch = branch
	return removal, nil
}

// clearDiscardedTreeWorkspaces drops the workspace columns of every member and
// unit whose checkout is gone, so nothing in the run record still points at a
// path that no longer exists.
func (a *App) clearDiscardedTreeWorkspaces(members []store.WorkItem) error {
	var errs []error
	for _, member := range members {
		if member.WorktreePath != "" {
			if err := a.store.UpdateWorkItemWorkspace(member.ID, "", "", member.BaseBranch); err != nil {
				errs = append(errs, err)
			}
		}
		units, err := a.store.ListWorkItemUnits(member.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("list fan-out units of %s: %w", member.ID, err))
			continue
		}
		for _, unit := range units {
			if strings.TrimSpace(unit.WorktreePath) == "" {
				continue
			}
			if err := a.store.AttachWorkItemUnitWorkspace(
				unit.ItemID, unit.PhaseID, unit.Attempt, unit.UnitID, unit.Branch, "",
			); err != nil {
				errs = append(errs, fmt.Errorf("clear unit %q worktree path: %w", unit.UnitID, err))
			}
		}
	}
	return errors.Join(errs...)
}
