package main

import (
	"fmt"
	"os"
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

	_, workspace, err := a.resolveGitPaths(thread)
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

	_, workspace, err := a.resolveGitPaths(thread)
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

	project, _, err := a.resolveGitPaths(thread)
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

	_, workspace, err := a.resolveGitPaths(thread)
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

	_, workspace, err := a.resolveGitPaths(thread)
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

	_, workspace, err := a.resolveGitPaths(thread)
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

	_, workspace, err := a.resolveGitPaths(thread)
	if err != nil {
		return gitops.GitActionResult{}, err
	}

	core := a.gitCore()
	if err := core.Pull(workspace); err != nil {
		return gitops.GitActionResult{}, err
	}

	// Pull mutates the working tree — bust the @-mention picker cache so
	// it reflects pulled additions/removals on the next composer query.
	if a.workspaceFiles != nil {
		a.workspaceFiles.Invalidate(workspace)
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

	project, workspace, err := a.resolveGitPaths(thread)
	if err != nil {
		return err
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return fmt.Errorf("git checkout branch is required")
	}

	unlock := sendThreadMuRegistry.lockFor(threadID)
	defer unlock()
	if err := a.ensureWorkspaceChangeAllowed(threadID); err != nil {
		return err
	}

	core := a.gitCore()
	if !gitops.SameFilesystemPath(workspace, project) && gitBranchIsDefault(core, project, branch) {
		workspace = project
		thread.WorkspacePath = project
		thread.WorktreePath = ""
	}
	if err := core.Checkout(workspace, branch); err != nil {
		return err
	}

	// Checkout swaps the working tree to a different branch — bust the
	// @-mention picker cache so it reflects the new tree.
	if a.workspaceFiles != nil {
		a.workspaceFiles.Invalidate(workspace)
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

	project, _, err := a.resolveGitPaths(thread)
	if err != nil {
		return err
	}

	return a.gitCore().CreateBranch(project, name)
}

// GitCreatePR opens a pull request for the workspace's current branch. When
// draft is true the PR is opened as a GitHub draft (gh pr create --draft).
func (a *App) GitCreatePR(threadID, title, body string, draft bool) (gitops.GitActionResult, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return gitops.GitActionResult{}, err
	}

	_, workspace, err := a.resolveGitPaths(thread)
	if err != nil {
		return gitops.GitActionResult{}, err
	}

	core := a.gitCore()
	url, err := core.CreatePR(workspace, title, body, draft)
	if err != nil {
		return gitops.GitActionResult{}, err
	}
	// Drop any stale "no open PR for this branch" cache entry so the
	// next status refresh (the watcher's debounce will fire ~250ms
	// after the gh write touches .git/refs) reflects the new PR
	// immediately instead of showing "Create PR available" for up to
	// prLookupTTL.
	core.InvalidatePRCache(workspace)

	return gitops.GitActionResult{
		Action:  "pr",
		Branch:  currentGitBranch(core, workspace),
		PRURL:   url,
		Message: "Created pull request",
	}, nil
}

// GitCreateWorktree creates a new worktree for the requested branch and returns its path.
func (a *App) GitCreateWorktree(threadID, branch string) (string, error) {
	updated, err := a.PrepareThreadWorktree(threadID, "", branch)
	if err != nil {
		return "", err
	}
	return updated.WorktreePath, nil
}

// PrepareThreadWorktree creates a new worktree from baseBranch, switches the
// thread to it, and returns the updated thread. requestedBranch is optional:
// blank means "create a temporary auto branch using the configured prefix".
func (a *App) PrepareThreadWorktree(threadID, baseBranch, requestedBranch string) (store.Thread, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return store.Thread{}, err
	}

	project, _, err := a.resolveGitPaths(thread)
	if err != nil {
		return store.Thread{}, err
	}

	unlock := sendThreadMuRegistry.lockFor(threadID)
	defer unlock()
	if err := a.ensureWorkspaceChangeAllowed(threadID); err != nil {
		return store.Thread{}, err
	}

	resolvedBranch := a.resolveWorktreeBranch(requestedBranch)
	if resolvedBranch == "" {
		return store.Thread{}, fmt.Errorf("create worktree: branch is required")
	}

	resolvedBase := strings.TrimSpace(baseBranch)
	if resolvedBase == "" {
		resolvedBase = strings.TrimSpace(thread.Branch)
	}
	if resolvedBase == "" {
		resolvedBase = currentGitBranch(a.gitCore(), project)
	}
	if resolvedBase == "" {
		return store.Thread{}, fmt.Errorf("create worktree: base branch is required")
	}

	worktreePath, err := a.defaultWorktreePath(project, resolvedBranch)
	if err != nil {
		return store.Thread{}, err
	}
	core := a.gitCore()
	if err := core.CreateWorktreeFromBranch(project, worktreePath, resolvedBase, resolvedBranch); err != nil {
		return store.Thread{}, err
	}
	// ProjectID is already set on the thread; the project's Path is the
	// git repo root. WorktreePath + WorkspacePath diverge at this point.
	thread.WorktreePath = worktreePath
	thread.WorkspacePath = worktreePath
	thread.Branch = resolvedBranch
	thread.UpdatedAt = time.Now().UnixMilli()
	if err := a.store.UpdateThread(thread); err != nil {
		// Worktree was created on disk but the store update failed. Clean up
		// so we don't leak a worktree directory.
		_ = core.RemoveWorktreeForce(project, worktreePath, true)
		return store.Thread{}, err
	}
	refreshed, err := a.restartSessionIfAffected(threadID, "workspace")
	if err != nil {
		return store.Thread{}, fmt.Errorf("create worktree: refresh thread after workspace switch: %w", err)
	}
	return refreshed, nil
}

// GitRemoveWorktree removes the worktree tracked by the thread.
func (a *App) GitRemoveWorktree(threadID string) error {
	return a.removeThreadWorktree(threadID, false)
}

func (a *App) removeThreadWorktree(threadID string, force bool) error {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return err
	}

	project, _, err := a.resolveGitPaths(thread)
	if err != nil {
		return err
	}

	worktreePath := strings.TrimSpace(thread.WorktreePath)
	if worktreePath == "" {
		return fmt.Errorf("thread %s has no worktree path", threadID)
	}
	if gitops.SameFilesystemPath(project, worktreePath) {
		return fmt.Errorf("refusing to remove project root as worktree")
	}
	if !force {
		shared, err := a.worktreeReferencedByOtherThread(threadID, worktreePath)
		if err != nil {
			return err
		}
		if shared {
			return fmt.Errorf("worktree %s is used by another thread", worktreePath)
		}
	}

	unlock := sendThreadMuRegistry.lockFor(threadID)
	defer unlock()
	if err := a.ensureWorkspaceChangeAllowed(threadID); err != nil {
		return err
	}

	core := a.gitCore()
	if err := core.RemoveWorktreeForce(project, worktreePath, force); err != nil {
		return err
	}

	thread.WorktreePath = ""
	if gitops.SameFilesystemPath(thread.WorkspacePath, worktreePath) {
		thread.WorkspacePath = project
		thread.Branch = currentGitBranch(core, project)
	}
	thread.UpdatedAt = time.Now().UnixMilli()
	if err := a.store.UpdateThread(thread); err != nil {
		return err
	}
	if _, err := a.restartSessionIfAffected(threadID, "workspace"); err != nil {
		return fmt.Errorf("remove worktree: refresh thread after workspace switch: %w", err)
	}
	return nil
}

// GitListWorktrees lists worktrees for the thread's repository.
func (a *App) GitListWorktrees(threadID string) ([]gitops.Worktree, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return nil, err
	}

	project, _, err := a.resolveGitPaths(thread)
	if err != nil {
		return nil, err
	}

	return a.gitCore().ListWorktrees(project)
}

// switchThreadWorkspace switches a thread to the project root or one of the
// repository's registered worktrees, keeping workspace/worktree/branch metadata
// in sync.
func (a *App) switchThreadWorkspace(threadID, path string) (store.Thread, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return store.Thread{}, err
	}
	project, _, err := a.resolveGitPaths(thread)
	if err != nil {
		return store.Thread{}, err
	}
	target := strings.TrimSpace(path)
	if target == "" {
		return store.Thread{}, fmt.Errorf("switch workspace: path is required")
	}

	unlock := sendThreadMuRegistry.lockFor(threadID)
	defer unlock()
	if err := a.ensureWorkspaceChangeAllowed(threadID); err != nil {
		return store.Thread{}, err
	}

	core := a.gitCore()
	switch {
	case gitops.SameFilesystemPath(target, project):
		thread.WorkspacePath = project
		thread.WorktreePath = ""
		thread.Branch = currentGitBranch(core, project)
	default:
		worktree, ok, err := a.findWorktree(project, target)
		if err != nil {
			return store.Thread{}, err
		}
		if !ok {
			return store.Thread{}, fmt.Errorf("switch workspace: %s is not a worktree for %s", target, project)
		}
		thread.WorkspacePath = worktree.Path
		thread.WorktreePath = worktree.Path
		thread.Branch = worktree.Branch
		if thread.Branch == "" {
			thread.Branch = currentGitBranch(core, worktree.Path)
		}
	}
	thread.UpdatedAt = time.Now().UnixMilli()
	if err := a.store.UpdateThread(thread); err != nil {
		return store.Thread{}, err
	}
	refreshed, err := a.restartSessionIfAffected(threadID, "workspace")
	if err != nil {
		return store.Thread{}, fmt.Errorf("switch workspace: refresh thread after workspace switch: %w", err)
	}
	return refreshed, nil
}

// resolveGitPaths resolves the (projectPath, workspacePath) pair for a
// thread. Project path comes from the threads→projects FK; workspace
// path is the thread's own column (which may diverge when a worktree is
// active). A missing project row falls back to WorkspacePath so tests
// that pre-insert threads without a store fixture still work.
func (a *App) resolveGitPaths(thread store.Thread) (project string, workspace string, err error) {
	workspace = strings.TrimSpace(thread.WorkspacePath)
	project = strings.TrimSpace(thread.ProjectPath)
	if project == "" && thread.ProjectID != "" && a.store != nil {
		if p, pErr := a.store.GetProject(thread.ProjectID); pErr == nil {
			project = strings.TrimSpace(p.Path)
		}
	}
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

func (a *App) defaultWorktreePath(projectPath, branch string) (string, error) {
	base := defaultWorktreesBaseDir(projectPath)
	if strings.TrimSpace(a.configDir) != "" {
		base = filepath.Join(a.configDir, "worktrees", filepath.Base(projectPath))
	}
	return uniquePath(filepath.Join(base, sanitizeWorktreeBranch(branch)))
}

func defaultWorktreesBaseDir(projectPath string) string {
	repoName := filepath.Base(projectPath)
	return filepath.Join(
		filepath.Dir(projectPath),
		repoName+"-worktrees",
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

func uniquePath(path string) (string, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return path, nil
		}
		return "", fmt.Errorf("check worktree path %s: %w", path, err)
	}
	for suffix := 1; suffix < 100; suffix++ {
		candidate := fmt.Sprintf("%s-%d", path, suffix)
		if _, err := os.Stat(candidate); err != nil {
			if os.IsNotExist(err) {
				return candidate, nil
			}
			return "", fmt.Errorf("check worktree path %s: %w", candidate, err)
		}
	}
	return fmt.Sprintf("%s-%d", path, time.Now().UnixMilli()), nil
}

func (a *App) worktreeBranchPrefix() string {
	if a.settings == nil {
		return gitops.AutoWorktreeBranchPrefix
	}
	prefix := strings.TrimSpace(a.settings.Get().WorktreeBranchPrefix)
	if prefix == "" {
		return gitops.AutoWorktreeBranchPrefix
	}
	return prefix
}

func (a *App) resolveWorktreeBranch(branch string) string {
	trimmed := strings.TrimSpace(branch)
	if trimmed == "" {
		return gitops.BuildTemporaryWorktreeBranchNameWithPrefix(a.worktreeBranchPrefix())
	}
	return gitops.SanitizeBranchNamePreservingSlashes(trimmed)
}

func (a *App) ensureWorkspaceChangeAllowed(threadID string) error {
	if turn, ok, err := a.store.GetActiveTurn(threadID); err != nil {
		return fmt.Errorf("check active turn: %w", err)
	} else if ok {
		return fmt.Errorf("cannot switch workspace while turn %d is active", turn.TurnIndex)
	}
	running, err := a.store.ListRunningBackgroundToolCalls(threadID)
	if err != nil {
		return fmt.Errorf("check background tasks: %w", err)
	}
	if len(running) > 0 {
		return fmt.Errorf("cannot switch workspace while %d background task(s) are running", len(running))
	}
	return nil
}

func (a *App) findWorktree(project, path string) (gitops.Worktree, bool, error) {
	worktrees, err := a.gitCore().ListWorktrees(project)
	if err != nil {
		return gitops.Worktree{}, false, err
	}
	for _, worktree := range worktrees {
		if gitops.SameFilesystemPath(worktree.Path, path) {
			return worktree, true, nil
		}
	}
	return gitops.Worktree{}, false, nil
}

func (a *App) worktreeReferencedByOtherThread(threadID, path string) (bool, error) {
	refs, err := a.store.ListThreadWorkspaceRefs()
	if err != nil {
		return false, err
	}
	for _, ref := range refs {
		if ref.ID == threadID {
			continue
		}
		if gitops.SameFilesystemPath(ref.WorktreePath, path) ||
			gitops.SameFilesystemPath(ref.WorkspacePath, path) {
			return true, nil
		}
	}
	return false, nil
}

func gitBranchIsDefault(core *gitops.Core, project, branch string) bool {
	branches, err := core.ListBranches(project)
	if err != nil {
		return false
	}
	for _, candidate := range branches {
		if candidate.Name == branch {
			return candidate.IsDefault
		}
	}
	return false
}
