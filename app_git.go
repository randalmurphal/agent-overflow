package main

import (
	"fmt"
	"log"
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
		Branch:  core.CurrentBranch(workspace),
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
		Branch:  core.CurrentBranch(workspace),
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
		Branch:  core.CurrentBranch(workspace),
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

	unlock := a.threadLocks().Lock(threadID)
	defer unlock()
	if err := a.ensureWorkspaceChangeAllowed(threadID); err != nil {
		return err
	}

	core := a.gitCore()
	if !gitops.SameFilesystemPath(workspace, project) && core.BranchIsDefault(project, branch) {
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

	thread.Branch = core.CurrentBranch(workspace)
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

// GitCreateBranchFrom creates a new branch in the thread's current
// workspace (project root or the worktree the thread occupies), pointed
// at baseBranch, then checks it out.
//
// carryLocalChanges has three meaningful combinations with baseBranch:
//   - base = current branch, carry = true: the "Local with changes" path —
//     `git checkout -b <name>` from HEAD; uncommitted changes stay attached.
//   - base = current branch, carry = false: same git command; carry is a
//     no-op since there's no checkout that would clobber the working tree.
//   - base != current branch, carry = false: the destructive path — stash
//     the working tree, checkout the base, branch off it, drop the stash.
//     The frontend gates this behind an explicit "discards uncommitted
//     changes" confirmation; we trust the caller has confirmed.
//   - base != current branch, carry = true: rejected. "Local with changes"
//     only makes sense when both ends agree on the base.
//
// Returns the refreshed thread (Branch updated). Does not call
// restartSessionIfAffected because the cwd is unchanged — the provider
// session keeps running.
func (a *App) GitCreateBranchFrom(threadID, name, baseBranch string, carryLocalChanges bool) (store.Thread, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return store.Thread{}, err
	}
	_, workspace, err := a.resolveGitPaths(thread)
	if err != nil {
		return store.Thread{}, err
	}

	name = strings.TrimSpace(name)
	if name == "" {
		return store.Thread{}, fmt.Errorf("create branch: name is required")
	}
	sanitized := gitops.SanitizeBranchNamePreservingSlashes(name)
	if sanitized == "" {
		return store.Thread{}, fmt.Errorf("create branch: name %q is not a valid branch name", name)
	}

	core := a.gitCore()
	currentBranch := strings.TrimSpace(thread.Branch)
	if currentBranch == "" {
		currentBranch = core.CurrentBranch(workspace)
	}
	resolvedBase := strings.TrimSpace(baseBranch)
	if resolvedBase == "" {
		resolvedBase = currentBranch
	}
	if resolvedBase == "" {
		return store.Thread{}, fmt.Errorf("create branch: base branch is required")
	}

	baseIsCurrent := resolvedBase == currentBranch
	if carryLocalChanges && !baseIsCurrent {
		return store.Thread{}, fmt.Errorf("create branch: 'Local with changes' only applies when the base matches the current branch")
	}

	unlock := a.threadLocks().Lock(threadID)
	defer unlock()
	if err := a.ensureWorkspaceChangeAllowed(threadID); err != nil {
		return store.Thread{}, err
	}

	if baseIsCurrent {
		// Working tree (clean or dirty) stays attached to the new branch.
		// CheckoutNewBranch validates the name through the package's
		// branch-name gate so a flag-shaped string can't reach argv.
		if err := core.CheckoutNewBranch(workspace, sanitized); err != nil {
			return store.Thread{}, fmt.Errorf("create branch: %w", err)
		}
	} else {
		// Destructive path: stash everything, checkout the base, branch
		// off it, drop the stash. The frontend has surfaced the warning.
		// Both checkout calls route through the package's typed wrappers
		// (Checkout / CheckoutNewBranch) so flag injection in the base
		// branch name is impossible regardless of the caller's input.
		stashMessage := fmt.Sprintf("ao-discard-%s", gitops.RandomStashSuffix())
		stashed, err := core.StashPushIncludeUntracked(workspace, stashMessage)
		if err != nil {
			return store.Thread{}, fmt.Errorf("create branch: stash before discard: %w", err)
		}
		if err := core.Checkout(workspace, resolvedBase); err != nil {
			if stashed {
				a.restoreStashOnError(workspace, stashMessage)
			}
			return store.Thread{}, fmt.Errorf("create branch: checkout base %s: %w", resolvedBase, err)
		}
		if err := core.CheckoutNewBranch(workspace, sanitized); err != nil {
			if stashed {
				a.restoreStashOnError(workspace, stashMessage)
			}
			return store.Thread{}, fmt.Errorf("create branch: %w", err)
		}
		if stashed {
			if err := core.StashDropByMessage(workspace, stashMessage); err != nil {
				log.Printf("create branch: drop discarded stash %q: %v", stashMessage, err)
			}
		}
	}

	if a.workspaceFiles != nil {
		a.workspaceFiles.Invalidate(workspace)
	}
	thread.Branch = core.CurrentBranch(workspace)
	thread.UpdatedAt = time.Now().UnixMilli()
	if err := a.store.UpdateThread(thread); err != nil {
		return store.Thread{}, err
	}
	return thread, nil
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
		Branch:  core.CurrentBranch(workspace),
		PRURL:   url,
		Message: "Created pull request",
	}, nil
}

// restoreStashOnError best-effort applies a previously-pushed stash back
// into the workspace and drops it. Used by both the worktree carry path
// (PrepareThreadWorktree) and the destructive branch-from path
// (GitCreateBranchFrom) when the operation between stash-push and
// stash-drop fails — so the user's working tree isn't left empty.
// Failures here are logged with the stash message so the user can
// recover via `git stash list`; the caller's original error is what
// surfaces upstream.
func (a *App) restoreStashOnError(sourceWorkspace, stashMessage string) {
	core := a.gitCore()
	if err := core.StashApplyByMessage(sourceWorkspace, stashMessage); err != nil {
		log.Printf("git stash: restore %q after error: %v", stashMessage, err)
		return
	}
	if err := core.StashDropByMessage(sourceWorkspace, stashMessage); err != nil {
		log.Printf("git stash: drop restored %q: %v", stashMessage, err)
	}
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

// ensureWorkspaceChangeAllowed refuses to mutate a thread's workspace while a
// turn or background tool call is still in flight — the provider session is
// bound to the cwd and changing it mid-turn would orphan output.
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
