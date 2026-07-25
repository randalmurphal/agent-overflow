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

type preparedWorkflowWorkspace struct {
	path    string
	project store.Project
}

func (r *workflowAppRunner) prepareWorkspace(ctx context.Context, request engine.RunRequest) (preparedWorkflowWorkspace, error) {
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
		return preparedWorkflowWorkspace{path: item.WorktreePath, project: project}, nil
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
	return preparedWorkflowWorkspace{path: worktreePath, project: project}, nil
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
