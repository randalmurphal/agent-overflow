package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
)

// GetGitStatus returns git status for the thread's active workspace.
func (a *App) GetGitStatus(threadID string) (gitops.GitStatus, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return gitops.GitStatus{}, err
	}

	_, workspace, err := resolveGitPaths(thread)
	if err != nil {
		return gitops.GitStatus{}, err
	}

	return a.gitCore().Status(workspace)
}

// GetWorkingTreeDiff returns the current combined staged and unstaged diff.
func (a *App) GetWorkingTreeDiff(threadID string) (string, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return "", err
	}

	_, workspace, err := resolveGitPaths(thread)
	if err != nil {
		return "", err
	}

	return a.gitCore().WorkingTreeDiff(workspace)
}

// GitListBranches lists repository branches from the thread's project root.
func (a *App) GitListBranches(threadID string) ([]gitops.GitBranch, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return nil, err
	}

	project, _, err := resolveGitPaths(thread)
	if err != nil {
		return nil, err
	}

	return a.gitCore().ListBranches(project)
}

// GitCommit stages all changes and commits workspace changes.
// WARNING: This stages everything (git add -A) before committing, including
// untracked files. Use GitStageAll + a direct Commit call for more control.
func (a *App) GitCommit(threadID, subject, body string) (gitops.GitActionResult, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return gitops.GitActionResult{}, err
	}

	_, workspace, err := resolveGitPaths(thread)
	if err != nil {
		return gitops.GitActionResult{}, err
	}

	core := a.gitCore()
	if err := core.StageAll(workspace); err != nil {
		return gitops.GitActionResult{}, err
	}
	sha, err := core.Commit(workspace, subject, body)
	if err != nil {
		return gitops.GitActionResult{}, err
	}

	return gitops.GitActionResult{
		Action:  "commit",
		Branch:  currentGitBranch(core, workspace),
		Commit:  sha,
		Message: "Committed changes",
	}, nil
}

// GitStageAll runs `git add -A` in the thread's workspace, staging all changes
// including untracked files. Use before GitCommit when explicit staging is desired.
func (a *App) GitStageAll(threadID string) error {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return err
	}

	_, workspace, err := resolveGitPaths(thread)
	if err != nil {
		return err
	}

	return a.gitCore().StageAll(workspace)
}

// GitPush pushes the workspace's current branch.
func (a *App) GitPush(threadID string) (gitops.GitActionResult, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return gitops.GitActionResult{}, err
	}

	_, workspace, err := resolveGitPaths(thread)
	if err != nil {
		return gitops.GitActionResult{}, err
	}

	core := a.gitCore()
	if err := core.Push(workspace); err != nil {
		return gitops.GitActionResult{}, err
	}

	return gitops.GitActionResult{
		Action:  "push",
		Branch:  currentGitBranch(core, workspace),
		Message: "Pushed branch",
	}, nil
}

// GitPull fast-forwards the workspace's current branch.
func (a *App) GitPull(threadID string) (gitops.GitActionResult, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return gitops.GitActionResult{}, err
	}

	_, workspace, err := resolveGitPaths(thread)
	if err != nil {
		return gitops.GitActionResult{}, err
	}

	core := a.gitCore()
	if err := core.Pull(workspace); err != nil {
		return gitops.GitActionResult{}, err
	}

	return gitops.GitActionResult{
		Action:  "pull",
		Branch:  currentGitBranch(core, workspace),
		Message: "Pulled latest changes",
	}, nil
}

// GitCheckout switches the workspace to an existing branch.
func (a *App) GitCheckout(threadID, branch string) error {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return err
	}

	_, workspace, err := resolveGitPaths(thread)
	if err != nil {
		return err
	}

	core := a.gitCore()
	if err := core.Checkout(workspace, branch); err != nil {
		return err
	}

	thread.Branch = currentGitBranch(core, workspace)
	thread.UpdatedAt = time.Now().UnixMilli()
	return a.store.UpdateThread(thread)
}

// GitCreateBranch creates a branch in the thread's repository.
func (a *App) GitCreateBranch(threadID, name string) error {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return err
	}

	project, _, err := resolveGitPaths(thread)
	if err != nil {
		return err
	}

	return a.gitCore().CreateBranch(project, name)
}

// GitCreatePR opens a pull request for the workspace's current branch.
func (a *App) GitCreatePR(threadID, title, body string) (gitops.GitActionResult, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return gitops.GitActionResult{}, err
	}

	_, workspace, err := resolveGitPaths(thread)
	if err != nil {
		return gitops.GitActionResult{}, err
	}

	core := a.gitCore()
	url, err := core.CreatePR(workspace, title, body)
	if err != nil {
		return gitops.GitActionResult{}, err
	}

	return gitops.GitActionResult{
		Action:  "pr",
		Branch:  currentGitBranch(core, workspace),
		PRURL:   url,
		Message: "Created pull request",
	}, nil
}

// GitCreateWorktree creates a new worktree for the requested branch and returns its path.
func (a *App) GitCreateWorktree(threadID, branch string) (string, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return "", err
	}

	project, _, err := resolveGitPaths(thread)
	if err != nil {
		return "", err
	}

	worktreePath := defaultWorktreePath(project, branch)
	core := a.gitCore()
	if err := core.CreateWorktree(project, worktreePath, branch); err != nil {
		return "", err
	}
	thread.ProjectPath = project
	thread.WorktreePath = worktreePath
	thread.WorkspacePath = worktreePath
	thread.Branch = strings.TrimSpace(branch)
	thread.UpdatedAt = time.Now().UnixMilli()
	if err := a.store.UpdateThread(thread); err != nil {
		// Worktree was created on disk but the store update failed. Clean up
		// so we don't leak a worktree directory.
		_ = core.RemoveWorktree(project, worktreePath)
		return "", err
	}
	return worktreePath, nil
}

// GitRemoveWorktree removes the worktree tracked by the thread.
func (a *App) GitRemoveWorktree(threadID string) error {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return err
	}

	project, _, err := resolveGitPaths(thread)
	if err != nil {
		return err
	}

	worktreePath := strings.TrimSpace(thread.WorktreePath)
	if worktreePath == "" {
		return fmt.Errorf("thread %s has no worktree path", threadID)
	}

	core := a.gitCore()
	if err := core.RemoveWorktree(project, worktreePath); err != nil {
		return err
	}

	thread.WorktreePath = ""
	if gitops.SameFilesystemPath(thread.WorkspacePath, worktreePath) {
		thread.WorkspacePath = project
		thread.Branch = currentGitBranch(core, project)
	}
	thread.UpdatedAt = time.Now().UnixMilli()
	return a.store.UpdateThread(thread)
}

// GitListWorktrees lists worktrees for the thread's repository.
func (a *App) GitListWorktrees(threadID string) ([]gitops.Worktree, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return nil, err
	}

	project, _, err := resolveGitPaths(thread)
	if err != nil {
		return nil, err
	}

	return a.gitCore().ListWorktrees(project)
}

func resolveGitPaths(thread store.Thread) (project string, workspace string, err error) {
	project = strings.TrimSpace(thread.ProjectPath)
	workspace = strings.TrimSpace(thread.WorkspacePath)

	switch {
	case project == "" && workspace == "":
		return "", "", fmt.Errorf("thread %s has no git paths", thread.ID)
	case project == "":
		project = workspace
	case workspace == "":
		workspace = project
	}

	return project, workspace, nil
}

func (a *App) detectProjectPath(workspacePath string) string {
	workspacePath = strings.TrimSpace(workspacePath)
	if workspacePath == "" {
		return ""
	}

	root, err := a.gitCore().RepositoryRoot(workspacePath)
	if err == nil && strings.TrimSpace(root) != "" {
		return root
	}
	return workspacePath
}

// gitCore returns the shared Core instance, lazily creating one if ServiceStartup
// has not run (e.g. in tests).
func (a *App) gitCore() *gitops.Core {
	if a.git != nil {
		return a.git
	}
	return gitops.NewCore()
}

func currentGitBranch(core *gitops.Core, cwd string) string {
	status, err := core.Status(cwd)
	if err != nil {
		return ""
	}
	return status.Branch
}

func defaultWorktreePath(projectPath, branch string) string {
	repoName := filepath.Base(projectPath)
	return filepath.Join(
		filepath.Dir(projectPath),
		repoName+"-worktrees",
		sanitizeWorktreeBranch(branch),
	)
}

func sanitizeWorktreeBranch(branch string) string {
	replacer := strings.NewReplacer(
		"/", "-",
		"\\", "-",
		" ", "-",
		"\t", "-",
	)
	sanitized := strings.Trim(replacer.Replace(strings.TrimSpace(branch)), ".-")
	if sanitized == "" {
		return "worktree"
	}
	return sanitized
}
