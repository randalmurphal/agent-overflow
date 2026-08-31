package app

import (
	"errors"
	"fmt"
	"log"
	"strings"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

func (a *App) gitProjectPath(projectID string) (string, error) {
	return a.gitApplication().ProjectPath(projectID)
}

func (a *App) resolveProjectGitPaths(projectID, workspacePath string) (project string, workspace string, worktreePath string, err error) {
	project, err = a.gitProjectPath(projectID)
	if err != nil {
		return "", "", "", err
	}
	workspace = strings.TrimSpace(workspacePath)
	if workspace == "" {
		return project, project, "", nil
	}
	if gitops.SameFilesystemPath(workspace, project) {
		return project, project, "", nil
	}
	worktree, ok, err := a.findWorktree(project, workspace)
	if err != nil {
		return "", "", "", fmt.Errorf("git project workspace: validate worktree: %w", err)
	}
	if !ok {
		return "", "", "", fmt.Errorf("git project workspace: %q is not a worktree of project %s", workspace, project)
	}
	return project, worktree.Path, worktree.Path, nil
}

type GitWorkspaceState struct {
	WorkspacePath string `json:"workspacePath"`
	WorktreePath  string `json:"worktreePath,omitempty"`
	Branch        string `json:"branch"`
}

// GetGitStatus returns git status for the thread's active workspace.
//
// The answer is not the caller's alone: every other client watching this
// workspace is looking at the same checkout, so the fresh status is also
// pushed through the workspace's gitwatch stream (a no-op when nobody is
// subscribed). Doing that AFTER the synchronous fetch means the watcher's
// refresh runs against a warm PR cache, and it arms gitwatch's
// missed-event detection — a refresh that observes a change the fs watches
// never reported is how a silently dead watchpoint gets reinstalled.
//
//ao:scope git:operate
func (a *App) GetGitStatus(threadID string) (gitops.GitStatus, error) {
	return a.gitApplication().Status(threadID)
}

// GetGitStatusFastForProject returns git status for a project root using only
// cached open-PR info — no gh/glab network call — and without requiring a
// thread row. The one caller is the composer's draft placeholder: it has no
// thread, so it can hold no git-status subscription, and it wants the local
// dirty bit rather than a forge round-trip. Every thread-backed surface reads
// the shared workspace-keyed git-status store instead.
//
//ao:scope git:operate
func (a *App) GetGitStatusFastForProject(projectID string) (gitops.GitStatus, error) {
	return a.gitApplication().StatusFastForProject(projectID)
}

// GetWorkingTreeDiff returns the current combined staged and unstaged diff.
//
//ao:scope files:read
func (a *App) GetWorkingTreeDiff(threadID string) (string, error) {
	return a.gitApplication().WorkingTreeDiff(threadID)
}

// GitListBranches lists repository branches from the thread's project root.
//
//ao:scope git:operate
func (a *App) GitListBranches(threadID string) ([]gitops.GitBranch, error) {
	return a.gitApplication().ListBranches(threadID)
}

// GitListBranchesForProject lists repository branches from a project root
// without requiring a thread row.
//
//ao:scope git:operate
func (a *App) GitListBranchesForProject(projectID string) ([]gitops.GitBranch, error) {
	return a.gitApplication().ListBranchesForProject(projectID)
}

// GitMaybeFetchRemotes runs `git fetch --all` in the background if the
// last successful fetch for this repo is older than the stale window.
// Returns true when a fetch actually ran, false when the cache was
// fresh. Callers re-list branches after a true return to surface any
// new ahead/behind counts.
//
// No threadLocks().Lock or ensureWorkspaceChangeAllowed — `git fetch`
// only touches `refs/remotes/*` and never HEAD/index/working tree, so
// running it concurrently with an active turn is safe.
//
//ao:scope git:operate
func (a *App) GitMaybeFetchRemotes(threadID string) (bool, error) {
	return a.gitApplication().MaybeFetchRemotes(threadID)
}

// GitMaybeFetchRemotesForProject is the project-root counterpart to
// GitMaybeFetchRemotes for draft placeholders.
//
//ao:scope git:operate
func (a *App) GitMaybeFetchRemotesForProject(projectID string) (bool, error) {
	return a.gitApplication().MaybeFetchRemotesForProject(projectID)
}

// GitSyncBranch fast-forwards branch from its configured upstream.
// The thread lock is held across the current-branch read so a concurrent
// checkout can't flip the path between the check and the operation.
//
//ao:scope git:operate
func (a *App) GitSyncBranch(threadID string, branch string) ([]gitops.GitBranch, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return nil, err
	}
	project, workspace, err := a.resolveGitPaths(thread)
	if err != nil {
		return nil, err
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return nil, fmt.Errorf("git sync branch is required")
	}

	core := a.gitCore()

	unlock := a.threadLocks().Lock(threadID)
	defer unlock()

	return a.syncBranchInWorkspace(core, project, workspace, branch)
}

// GitSyncBranchForProject fast-forwards a branch for a draft placeholder
// without requiring a thread row.
//
//ao:scope git:operate
func (a *App) GitSyncBranchForProject(projectID, workspacePath, branch string) ([]gitops.GitBranch, error) {
	project, workspace, _, err := a.resolveProjectGitPaths(projectID, workspacePath)
	if err != nil {
		return nil, err
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return nil, fmt.Errorf("git sync branch is required")
	}

	core := a.gitCore()
	return a.syncBranchInWorkspace(core, project, workspace, branch)
}

func (a *App) syncBranchInWorkspace(core *gitops.Core, project, workspace, branch string) ([]gitops.GitBranch, error) {
	if err := core.SyncBranch(workspace, branch); err != nil {
		return nil, err
	}
	if a.workspaceFiles != nil && core.CurrentBranch(workspace) == branch {
		a.workspaceFiles.Invalidate(workspace)
	}
	return core.ListBranches(project)
}

// GitCommit stages all changes and commits workspace changes.
// WARNING: This stages everything (git add -A) before committing, including
// untracked files. Use GitStageAll + a direct Commit call for more control.
//
//ao:scope git:operate
func (a *App) GitCommit(threadID, subject, body string) (gitops.GitActionResult, error) {
	return a.gitApplication().Commit(threadID, subject, body)
}

// GitStageAll runs `git add -A` in the thread's workspace, staging all changes
// including untracked files. Use before GitCommit when explicit staging is desired.
//
//ao:scope git:operate
func (a *App) GitStageAll(threadID string) error {
	return a.gitApplication().StageAll(threadID)
}

// GitPush pushes the workspace's current branch.
//
//ao:scope git:operate
func (a *App) GitPush(threadID string) (gitops.GitActionResult, error) {
	return a.gitApplication().Push(threadID)
}

// GitPull fast-forwards the workspace's current branch.
//
//ao:scope git:operate
func (a *App) GitPull(threadID string) (gitops.GitActionResult, error) {
	return a.gitApplication().Pull(threadID)
}

// GitCheckout switches the workspace to an existing branch.
//
//ao:scope git:operate
func (a *App) GitCheckout(threadID, branch string) error {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return err
	}

	_, workspace, err := a.resolveGitPaths(thread)
	if err != nil {
		return err
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return fmt.Errorf("git checkout branch is required")
	}

	unlock := a.threadLocks().Lock(threadID)
	defer unlock()

	core := a.gitCore()
	if err := core.Checkout(workspace, branch); err != nil {
		return err
	}

	// Checkout swaps the working tree to a different branch — bust the
	// @-mention picker cache so it reflects the new tree.
	if a.workspaceFiles != nil {
		a.workspaceFiles.Invalidate(workspace)
	}

	previousBranch := thread.Branch
	thread.Branch = core.CurrentBranch(workspace)
	if err := a.store.UpdateThread(thread); err != nil {
		return err
	}
	a.broadcastThreadRowIfChanged(triage.ThreadActionFull, thread, thread.Branch != previousBranch)
	return nil
}

// GitCheckoutForProject switches a project/worktree placeholder workspace to an
// existing branch without requiring a thread row.
//
//ao:scope git:operate
func (a *App) GitCheckoutForProject(projectID, workspacePath, branch string) (GitWorkspaceState, error) {
	_, workspace, worktreePath, err := a.resolveProjectGitPaths(projectID, workspacePath)
	if err != nil {
		return GitWorkspaceState{}, err
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return GitWorkspaceState{}, fmt.Errorf("git checkout branch is required")
	}

	core := a.gitCore()
	if err := core.Checkout(workspace, branch); err != nil {
		return GitWorkspaceState{}, err
	}
	if a.workspaceFiles != nil {
		a.workspaceFiles.Invalidate(workspace)
	}
	return GitWorkspaceState{
		WorkspacePath: workspace,
		WorktreePath:  worktreePath,
		Branch:        core.CurrentBranch(workspace),
	}, nil
}

// GitCreateBranch creates a branch in the thread's repository.
//
//ao:scope git:operate
func (a *App) GitCreateBranch(threadID, name string) error {
	return a.gitApplication().CreateBranch(threadID, name)
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
//
//ao:scope git:operate
func (a *App) GitCreateBranchFrom(threadID, name, baseBranch string, carryLocalChanges bool) (store.Thread, error) {
	// Lock before the read — see PrepareThreadWorktree for why a pre-lock read
	// races the empty-draft cleanup's delete.
	unlock := a.threadLocks().Lock(threadID)
	defer unlock()

	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return store.Thread{}, err
	}
	_, workspace, err := a.resolveGitPaths(thread)
	if err != nil {
		return store.Thread{}, err
	}

	core := a.gitCore()
	currentBranch := strings.TrimSpace(thread.Branch)
	if currentBranch == "" {
		currentBranch = core.CurrentBranch(workspace)
	}
	sanitized, resolvedBase, baseIsCurrent, err := resolveBranchCreatePlan(
		currentBranch, name, baseBranch, carryLocalChanges)
	if err != nil {
		return store.Thread{}, err
	}

	if !baseIsCurrent {
		if err := a.ensureWorkspaceChangeAllowed(threadID); err != nil {
			return store.Thread{}, err
		}
		if err := core.EnsureLocalBranchDoesNotExist(workspace, sanitized); err != nil {
			return store.Thread{}, fmt.Errorf("create branch: %w", err)
		}
	}

	if err := a.createBranchInWorkspace(workspace, sanitized, resolvedBase, baseIsCurrent); err != nil {
		return store.Thread{}, err
	}

	if a.workspaceFiles != nil {
		a.workspaceFiles.Invalidate(workspace)
	}
	previousBranch := thread.Branch
	thread.Branch = core.CurrentBranch(workspace)
	if err := a.store.UpdateThread(thread); err != nil {
		return store.Thread{}, err
	}
	a.broadcastThreadRowIfChanged(triage.ThreadActionFull, thread, thread.Branch != previousBranch)
	return thread, nil
}

// errCarryRequiresCurrentBase is the ONE refusal every "Local with changes"
// check issues. Carrying uncommitted work forward is a move; carrying it onto
// an unrelated base is a rebase, which is a different request the UI does not
// offer. Four call sites used to spell this sentence out verbatim — the two
// branch-create paths through resolveBranchCreatePlan below, and the two
// worktree-cut paths that wrap it with their own "create worktree: " prefix.
//
// Callers wrap it with %w behind their operation name. The final `: `-segment
// must stand alone: the frontend's userFacingError keeps only that segment.
var errCarryRequiresCurrentBase = errors.New("'Local with changes' only applies when the base matches the current branch")

// resolveBranchCreatePlan is the whole policy of "create branch <name> off
// <base>" for GitCreateBranchFrom: name validation, base defaulting, the
// base-is-current decision that selects between the two git sequences in
// createBranchInWorkspace, and the carry refusal.
//
// currentBranch is the branch of the checkout the caller is about to mutate —
// the one thing only the caller can resolve, and the one thing that must not
// be guessed at from the project root when the caller is in a worktree.
func resolveBranchCreatePlan(currentBranch, name, base string, carryLocalChanges bool) (string, string, bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", false, fmt.Errorf("create branch: name is required")
	}
	sanitized := gitops.SanitizeBranchNamePreservingSlashes(name)
	if sanitized == "" {
		return "", "", false, fmt.Errorf("create branch: name %q is not a valid branch name", name)
	}
	resolvedBase := strings.TrimSpace(base)
	if resolvedBase == "" {
		resolvedBase = currentBranch
	}
	if resolvedBase == "" {
		return "", "", false, fmt.Errorf("create branch: base branch is required")
	}
	baseIsCurrent := resolvedBase == currentBranch
	if carryLocalChanges && !baseIsCurrent {
		return "", "", false, fmt.Errorf("create branch: %w", errCarryRequiresCurrentBase)
	}
	return sanitized, resolvedBase, baseIsCurrent, nil
}

// createBranchInWorkspace performs the checkout half of "branch off base" in a
// checkout the caller has already decided it may mutate. name must already be
// sanitized, and baseIsCurrent must already have been computed against that
// checkout's current branch. This helper owns the two git sequences, not the
// policy that chooses between them.
//
// The destructive branch (baseIsCurrent == false) stashes everything, checks
// out the base, branches off it, and drops the stash. The frontend has surfaced
// the "discards uncommitted changes" warning by the time we get here. Both
// checkout calls route through the package's typed wrappers (Checkout /
// CheckoutNewBranch) so flag injection in the base branch name is impossible
// regardless of the caller's input.
func (a *App) createBranchInWorkspace(workspace, name, resolvedBase string, baseIsCurrent bool) error {
	core := a.gitCore()
	if baseIsCurrent {
		// Working tree (clean or dirty) stays attached to the new branch.
		// CheckoutNewBranch validates the name through the package's
		// branch-name gate so a flag-shaped string can't reach argv.
		if err := core.CheckoutNewBranch(workspace, name); err != nil {
			return fmt.Errorf("create branch: %w", err)
		}
		return nil
	}

	stashMessage := fmt.Sprintf("ao-discard-%s", gitops.RandomStashSuffix())
	stashed, err := core.StashPushIncludeUntracked(workspace, stashMessage)
	if err != nil {
		return fmt.Errorf("create branch: stash before discard: %w", err)
	}
	if err := core.Checkout(workspace, resolvedBase); err != nil {
		if stashed {
			a.restoreStashOnError(workspace, stashMessage)
		}
		return fmt.Errorf("create branch: checkout base %s: %w", resolvedBase, err)
	}
	if err := core.CheckoutNewBranch(workspace, name); err != nil {
		if stashed {
			a.restoreStashOnError(workspace, stashMessage)
		}
		return fmt.Errorf("create branch: %w", err)
	}
	if stashed {
		if err := core.StashDropByMessage(workspace, stashMessage); err != nil {
			log.Printf("create branch: drop discarded stash %q: %v", stashMessage, err)
		}
	}
	return nil
}

// GitCreatePR opens a pull request for the workspace's current branch. When
// draft is true the PR is opened as a GitHub draft (gh pr create --draft).
//
//ao:scope git:operate
func (a *App) GitCreatePR(threadID, title, body string, draft bool) (gitops.GitActionResult, error) {
	return a.gitApplication().CreatePR(threadID, title, body, draft)
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
	return a.gitApplication().ResolveThreadPaths(thread)
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
	reason, err := a.threadActivityBlockReason(threadID)
	if err != nil {
		return err
	}
	if reason != "" {
		return fmt.Errorf("cannot switch workspace while %s", reason)
	}
	return nil
}

func (a *App) threadActivityBlockReason(threadID string) (string, error) {
	if turn, ok, err := a.store.GetActiveTurn(threadID); err != nil {
		return "", fmt.Errorf("check active turn: %w", err)
	} else if ok {
		return fmt.Sprintf("turn %d is active", turn.TurnIndex), nil
	}
	count, err := a.countRunningBackgroundTasks(threadID)
	if err != nil {
		return "", fmt.Errorf("check background tasks: %w", err)
	}
	if count > 0 {
		return fmt.Sprintf("%d background task(s) are running", count), nil
	}
	return "", nil
}
