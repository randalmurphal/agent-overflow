package main

import (
	"context"
	"fmt"
	"strings"

	"agent-overflow/internal/gitdiff"
)

// GetWorkspaceCurrentDiff returns the unified patch of everything
// currently uncommitted in the thread's workspace (tracked changes
// against HEAD plus untracked-not-ignored files). Empty for non-git
// workspaces.
func (a *App) GetWorkspaceCurrentDiff(threadID string) (string, error) {
	const action = "get workspace current diff"
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return "", fmt.Errorf("%s: %w", action, err)
	}
	_, workspace, err := a.resolveGitPaths(thread)
	if err != nil {
		return "", fmt.Errorf("%s: %w", action, err)
	}
	if !gitdiff.IsGitRepository(context.Background(), workspace) {
		return "", nil
	}
	patch, err := gitdiff.DiffWorkspaceVsHead(context.Background(), workspace)
	if err != nil {
		return "", fmt.Errorf("%s: %w", action, err)
	}
	return string(patch), nil
}

// GetBranchBaseDiff returns the combined diff of the thread's workspace
// (committed work since merge-base plus uncommitted changes) against the
// merge base of baseBranch and the workspace HEAD — i.e. what a PR onto
// baseBranch would contain.
func (a *App) GetBranchBaseDiff(threadID string, baseBranch string) (string, error) {
	const action = "get branch base diff"
	if strings.TrimSpace(baseBranch) == "" {
		return "", fmt.Errorf("%s: base branch is required", action)
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return "", fmt.Errorf("%s: %w", action, err)
	}
	_, workspace, err := a.resolveGitPaths(thread)
	if err != nil {
		return "", fmt.Errorf("%s: %w", action, err)
	}
	if !gitdiff.IsGitRepository(context.Background(), workspace) {
		return "", nil
	}
	patch, err := gitdiff.DiffBranchBaseToWorktree(context.Background(), workspace, baseBranch)
	if err != nil {
		return "", fmt.Errorf("%s: %w", action, err)
	}
	return string(patch), nil
}

// BranchCommit is the wire shape of one row in the review pane's
// per-commit selector.
type BranchCommit = gitdiff.Commit

// ListBranchCommits returns the commits a PR from the workspace HEAD
// onto baseBranch would carry (`base..HEAD`, newest first). Empty for
// non-git workspaces.
func (a *App) ListBranchCommits(threadID string, baseBranch string) ([]BranchCommit, error) {
	const action = "list branch commits"
	if strings.TrimSpace(baseBranch) == "" {
		return nil, fmt.Errorf("%s: base branch is required", action)
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	_, workspace, err := a.resolveGitPaths(thread)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	if !gitdiff.IsGitRepository(context.Background(), workspace) {
		return []BranchCommit{}, nil
	}
	commits, err := gitdiff.ListCommits(context.Background(), workspace, baseBranch)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	return commits, nil
}

// GetCommitDiff returns the unified patch a single local commit
// introduced (first-parent diff; empty-tree diff for a root commit).
func (a *App) GetCommitDiff(threadID string, sha string) (string, error) {
	const action = "get commit diff"
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return "", fmt.Errorf("%s: %w", action, err)
	}
	_, workspace, err := a.resolveGitPaths(thread)
	if err != nil {
		return "", fmt.Errorf("%s: %w", action, err)
	}
	if !gitdiff.IsGitRepository(context.Background(), workspace) {
		return "", fmt.Errorf("%s: workspace is not a git repository", action)
	}
	patch, err := gitdiff.CommitDiff(context.Background(), workspace, sha)
	if err != nil {
		return "", fmt.Errorf("%s: %w", action, err)
	}
	return string(patch), nil
}
