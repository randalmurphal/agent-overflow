package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
)

// preparedWorkflowWorkspace is one resolved workspace: where work runs and, when
// that is an isolated worktree, the branch it is checked out on. Branch travels
// with the path because every consumer that needs one needs the other — the
// thread row, a unit's sub-worktree base, the run record.
type preparedWorkflowWorkspace struct {
	path    string
	branch  string
	project store.Project
}

// prepareWorkspace resolves (and, on first use, provisions) the item's primary
// workspace.
//
// Provisioning is serialized per item because a fan-out starts several units at
// once and every one of them resolves this same workspace: two concurrent
// resolutions of an item that has no worktree yet would each cut one, and the
// loser would be orphaned on disk with nothing in the run record pointing at
// it. Serializing here rather than at the call sites means a future caller
// cannot forget.
func (r *workflowAppRunner) prepareWorkspace(ctx context.Context, request engine.RunRequest) (preparedWorkflowWorkspace, error) {
	unlock := r.workspaceLocks.Lock(request.Key.ItemID)
	defer unlock()
	return r.resolveWorkspace(ctx, request)
}

func (r *workflowAppRunner) resolveWorkspace(ctx context.Context, request engine.RunRequest) (preparedWorkflowWorkspace, error) {
	project, err := r.app.store.GetProject(request.Item.ProjectID)
	if err != nil {
		return preparedWorkflowWorkspace{}, fmt.Errorf("load project: %w", err)
	}
	if def.DeriveWorkspaceNeed(request.Workflow) == def.WorkspaceProjectRoot {
		return preparedWorkflowWorkspace{path: project.Path, project: project}, nil
	}
	item, err := r.app.store.GetWorkItem(request.Key.ItemID)
	if err != nil {
		return preparedWorkflowWorkspace{}, fmt.Errorf("load work item: %w", err)
	}

	if item.WorktreePath != "" || item.Branch != "" {
		if item.WorktreePath == "" || item.Branch == "" || item.BaseBranch == "" {
			return preparedWorkflowWorkspace{}, fmt.Errorf("work item has incomplete workspace fields")
		}
		info, statErr := os.Stat(item.WorktreePath)
		if statErr != nil {
			if errors.Is(statErr, fs.ErrNotExist) {
				return preparedWorkflowWorkspace{}, fmt.Errorf("recorded worktree %q is missing", item.WorktreePath)
			}
			return preparedWorkflowWorkspace{}, fmt.Errorf("inspect recorded worktree %q: %w", item.WorktreePath, statErr)
		}
		if !info.IsDir() {
			return preparedWorkflowWorkspace{}, fmt.Errorf("recorded worktree %q is not a directory", item.WorktreePath)
		}
		registered, ok, findErr := r.app.findWorktree(project.Path, item.WorktreePath)
		if findErr != nil {
			return preparedWorkflowWorkspace{}, fmt.Errorf("verify recorded worktree %q: %w", item.WorktreePath, findErr)
		}
		if !ok || registered.Branch != item.Branch {
			return preparedWorkflowWorkspace{}, fmt.Errorf("recorded worktree %q is not registered on branch %q", item.WorktreePath, item.Branch)
		}
		return preparedWorkflowWorkspace{path: item.WorktreePath, branch: item.Branch, project: project}, nil
	}

	projectProfile, err := r.projectProfile(ctx, item.ProjectID)
	if err != nil {
		return preparedWorkflowWorkspace{}, err
	}
	baseBranch := strings.TrimSpace(item.BaseBranch)
	if baseBranch == "" {
		baseBranch = strings.TrimSpace(projectProfile.BaseBranch)
	}
	if baseBranch == "" {
		baseBranch = strings.TrimSpace(r.app.gitCore().CurrentBranch(project.Path))
	}
	if baseBranch == "" {
		return preparedWorkflowWorkspace{}, fmt.Errorf("resolve base branch: repository has no current branch")
	}
	core := r.app.gitCore()
	branchPrefix := workflowWorktreeBranchPrefix(r.app.worktreeBranchPrefix(), request.Workflow.ID, item.ID)
	branch, worktreePath, err := findInterruptedWorkflowProvisioning(core, project.Path, branchPrefix)
	if err != nil {
		return preparedWorkflowWorkspace{}, err
	}
	if worktreePath == "" {
		branch = gitops.BuildTemporaryWorktreeBranchNameWithPrefix(branchPrefix)
		worktreePath, err = r.app.defaultWorktreePath(project.Path, branch)
		if err != nil {
			return preparedWorkflowWorkspace{}, fmt.Errorf("choose worktree path: %w", err)
		}
		if err := core.CreateWorktreeFromBranch(project.Path, worktreePath, baseBranch, branch); err != nil {
			return preparedWorkflowWorkspace{}, fmt.Errorf("create worktree from %q: %w", baseBranch, err)
		}
	}
	rollback := func(cause error) error {
		if removeErr := core.RemoveWorktreeForce(project.Path, worktreePath, true); removeErr != nil {
			return errors.Join(cause, fmt.Errorf("rollback worktree %q: %w", worktreePath, removeErr))
		}
		// Clear provisioning state but keep the intake-time base branch: it is
		// user intent from enqueue, not a product of the rolled-back provisioning.
		if clearErr := r.app.store.UpdateWorkItemWorkspace(item.ID, "", "", item.BaseBranch); clearErr != nil {
			return errors.Join(cause, fmt.Errorf("clear rolled-back worktree %q: %w", worktreePath, clearErr))
		}
		return cause
	}
	if err := runWorkflowWorktreeSetup(ctx, project.Path, worktreePath, projectProfile.WorktreeSetup); err != nil {
		return preparedWorkflowWorkspace{}, rollback(err)
	}
	if err := r.app.store.UpdateWorkItemWorkspace(item.ID, worktreePath, branch, baseBranch); err != nil {
		return preparedWorkflowWorkspace{}, rollback(fmt.Errorf("persist workspace: %w", err))
	}
	return preparedWorkflowWorkspace{path: worktreePath, branch: branch, project: project}, nil
}

// provisionUnitWorktree cuts one writing fan-out unit's isolated sub-worktree
// from the item's branch (spec §9). The branch and path are registered on the
// unit row before the session starts, so a crash between `git worktree add` and
// the first turn can never strand either.
//
// The unit's branch name is a deterministic function of the item's branch, the
// unit id, and the unit's try number: re-entering the same try finds its own
// worktree and adopts it instead of cutting a second, and a retry (a new try
// number) always cuts fresh from the item branch rather than inheriting what
// the failed try left behind.
func (r *workflowAppRunner) provisionUnitWorktree(
	ctx context.Context, request engine.RunRequest, primary preparedWorkflowWorkspace,
) (preparedWorkflowWorkspace, error) {
	if primary.branch == "" {
		// DeriveWorkspaceNeed counts a writing unit, so an item whose units write
		// always has a worktree by the time one starts. Reaching here means the
		// frozen definition and the provisioned workspace disagree.
		return preparedWorkflowWorkspace{}, fmt.Errorf(
			"unit %q declares write access but item %q has no branch to cut from",
			request.Key.UnitID, request.Key.ItemID,
		)
	}
	branch := workflowUnitBranch(primary.branch, request.Key.UnitID, request.UnitAttempt)
	if err := gitops.ValidateBranchName(branch); err != nil {
		return preparedWorkflowWorkspace{}, fmt.Errorf("unit branch %q: %w", branch, err)
	}
	core := r.app.gitCore()
	worktreePath, adopted, err := r.existingUnitWorktree(primary.project.Path, branch)
	if err != nil {
		return preparedWorkflowWorkspace{}, err
	}
	if !adopted {
		worktreePath, err = r.app.defaultWorktreePath(primary.project.Path, branch)
		if err != nil {
			return preparedWorkflowWorkspace{}, fmt.Errorf("choose unit worktree path: %w", err)
		}
		if err := core.CreateWorktreeFromBranch(primary.project.Path, worktreePath, primary.branch, branch); err != nil {
			return preparedWorkflowWorkspace{}, fmt.Errorf("create unit worktree from %q: %w", primary.branch, err)
		}
	}
	rollback := func(cause error) error {
		if removeErr := core.RemoveWorktreeForce(primary.project.Path, worktreePath, true); removeErr != nil {
			return errors.Join(cause, fmt.Errorf("rollback unit worktree %q: %w", worktreePath, removeErr))
		}
		return cause
	}
	if err := r.app.store.AttachWorkItemUnitWorkspace(
		request.Key.ItemID, request.Key.PhaseID, request.Key.Attempt, request.Key.UnitID, branch, worktreePath,
	); err != nil {
		return preparedWorkflowWorkspace{}, rollback(fmt.Errorf("register unit workspace: %w", err))
	}
	projectProfile, err := r.projectProfile(ctx, request.Item.ProjectID)
	if err != nil {
		return preparedWorkflowWorkspace{}, err
	}
	// A sub-worktree is a fresh checkout: without the project's setup hooks it
	// lacks exactly the installed state the item's own worktree was given.
	if err := runWorkflowWorktreeSetup(ctx, primary.project.Path, worktreePath, projectProfile.WorktreeSetup); err != nil {
		return preparedWorkflowWorkspace{}, err
	}
	return preparedWorkflowWorkspace{path: worktreePath, branch: branch, project: primary.project}, nil
}

// existingUnitWorktree finds the worktree already checked out on a unit's
// branch. A found one is adopted rather than replaced: it is this unit's own
// try, re-entered.
func (r *workflowAppRunner) existingUnitWorktree(projectPath, branch string) (string, bool, error) {
	worktrees, err := r.app.gitCore().ListWorktrees(projectPath)
	if err != nil {
		return "", false, fmt.Errorf("list worktrees before unit provisioning: %w", err)
	}
	for _, worktree := range worktrees {
		if worktree.Branch == branch {
			return worktree.Path, true, nil
		}
	}
	return "", false, nil
}

// workflowUnitBranch names a fan-out unit's sub-worktree branch. It extends the
// item's own branch so every branch a run created shares one prefix — which is
// what makes the run's branches findable from the item alone.
func workflowUnitBranch(itemBranch, unitID string, unitAttempt int) string {
	if unitAttempt < 1 {
		unitAttempt = 1
	}
	return fmt.Sprintf("%s-%s-%d", itemBranch, gitops.SanitizeBranchFragment(unitID), unitAttempt)
}

func workflowWorktreeBranch(prefix, workflowID, itemID string) string {
	return gitops.BuildTemporaryWorktreeBranchNameWithPrefix(workflowWorktreeBranchPrefix(prefix, workflowID, itemID))
}

func workflowWorktreeBranchPrefix(prefix, workflowID, itemID string) string {
	configuredPrefix := strings.TrimSpace(prefix)
	if configuredPrefix != "" && !strings.HasSuffix(configuredPrefix, "-") && !strings.HasSuffix(configuredPrefix, "_") {
		configuredPrefix += "-"
	}
	return configuredPrefix + "workflow-" + gitops.SanitizeBranchFragment(workflowID) + "-" + gitops.SanitizeBranchFragment(itemID) + "-"
}

func findInterruptedWorkflowProvisioning(core *gitops.Core, projectPath, branchPrefix string) (branch, worktreePath string, err error) {
	worktrees, err := core.ListWorktrees(projectPath)
	if err != nil {
		return "", "", fmt.Errorf("list worktrees before provisioning: %w", err)
	}
	for _, worktree := range worktrees {
		if !strings.HasPrefix(worktree.Branch, branchPrefix) {
			continue
		}
		if worktreePath != "" {
			return "", "", fmt.Errorf("multiple interrupted workflow worktrees match branch prefix %q", branchPrefix)
		}
		branch, worktreePath = worktree.Branch, worktree.Path
	}
	return branch, worktreePath, nil
}
