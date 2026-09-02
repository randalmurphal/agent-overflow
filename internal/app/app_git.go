package app

import (
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/gitapp"
	"agent-overflow/internal/store"
)

// WorkspaceRef is the subject of every workspace-scoped git RPC: a checkout,
// named by project id + directory. A thread id on a git RPC now means the
// subject IS the thread — its own history, or its workspace assignment.
type WorkspaceRef = gitapp.WorkspaceRef

// workspaceRefForThread is the ref a real thread's pane addresses its
// workspace-scoped git RPCs with. The frontend builds the identical pair from
// the same two columns; a draft placeholder carries them without a row.
func workspaceRefForThread(thread store.Thread) WorkspaceRef {
	return WorkspaceRef{ProjectID: thread.ProjectID, WorkspacePath: thread.WorkspacePath}
}

// GitWorkspaceState is the caller's checkout after a mutation that may have
// moved its branch. WorktreePath is empty when the workspace is the project
// root.
type GitWorkspaceState struct {
	WorkspacePath string `json:"workspacePath"`
	WorktreePath  string `json:"worktreePath,omitempty"`
	Branch        string `json:"branch"`
}

// workspaceState projects a resolved (project, workspace) pair plus the
// checkout's live branch into the wire shape.
func (a *App) workspaceState(project, workspace string) GitWorkspaceState {
	state := GitWorkspaceState{WorkspacePath: workspace, Branch: a.gitCore().CurrentBranch(workspace)}
	if !gitops.SameFilesystemPath(workspace, project) {
		state.WorktreePath = workspace
	}
	return state
}

// GetGitStatus returns git status for the referenced workspace.
//
// The answer is not the caller's alone: every other client watching this
// workspace is looking at the same checkout, so the fresh status is also
// pushed through the workspace's gitwatch stream (a no-op when nobody is
// subscribed). Doing that AFTER the synchronous fetch means the watcher's
// refresh runs against a warm PR cache, and it arms gitwatch's
// missed-event detection — a refresh that observes a change the fs watches
// never reported is how a silently dead watchpoint gets reinstalled.
func (a *App) GetGitStatus(ws WorkspaceRef) (gitops.GitStatus, error) {
	return a.gitApplication().Status(ws)
}

// GitListBranches lists repository branches from the workspace's project root.
func (a *App) GitListBranches(ws WorkspaceRef) ([]gitops.GitBranch, error) {
	return a.gitApplication().ListBranches(ws)
}

// GitMaybeFetchRemotes runs `git fetch --all` in the background if the
// last successful fetch for this repo is older than the stale window.
// Returns true when a fetch actually ran, false when the cache was
// fresh. Callers re-list branches after a true return to surface any
// new ahead/behind counts.
//
// No workspace locks or ensureWorkspaceChangeAllowed — `git fetch` only
// touches `refs/remotes/*` and never HEAD/index/working tree, so running it
// concurrently with an active turn is safe.
func (a *App) GitMaybeFetchRemotes(ws WorkspaceRef) (bool, error) {
	return a.gitApplication().MaybeFetchRemotes(ws)
}

// GitSyncBranch fast-forwards branch from its configured upstream. Every
// thread in the workspace is locked across the current-branch read so a
// concurrent checkout can't flip the path between the check and the
// operation.
func (a *App) GitSyncBranch(ws WorkspaceRef, branch string) ([]gitops.GitBranch, error) {
	project, workspace, err := a.gitApplication().ResolveWorkspace(ws)
	if err != nil {
		return nil, err
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return nil, fmt.Errorf("git sync branch is required")
	}

	_, release, err := a.lockWorkspaceThreads(workspace)
	if err != nil {
		return nil, err
	}
	defer release()

	core := a.gitCore()
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
// untracked files.
func (a *App) GitCommit(ws WorkspaceRef, subject, body string) (gitops.GitActionResult, error) {
	return a.gitApplication().Commit(ws, subject, body)
}

// GitPush pushes the workspace's current branch.
func (a *App) GitPush(ws WorkspaceRef) (gitops.GitActionResult, error) {
	return a.gitApplication().Push(ws)
}

// GitPull fast-forwards the workspace's current branch.
func (a *App) GitPull(ws WorkspaceRef) (gitops.GitActionResult, error) {
	return a.gitApplication().Pull(ws)
}

// GitCheckout switches the workspace to an existing branch and returns the
// checkout's resulting state.
//
// Every thread row in the directory is re-branched through the same
// workspace-keyed write UpdateThreadBranch uses (and broadcast the same way),
// so a sibling thread sharing the worktree syncs without the frontend
// guessing which rows moved.
func (a *App) GitCheckout(ws WorkspaceRef, branch string) (GitWorkspaceState, error) {
	project, workspace, err := a.gitApplication().ResolveWorkspace(ws)
	if err != nil {
		return GitWorkspaceState{}, err
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return GitWorkspaceState{}, fmt.Errorf("git checkout branch is required")
	}

	_, release, err := a.lockWorkspaceThreads(workspace)
	if err != nil {
		return GitWorkspaceState{}, err
	}
	defer release()

	if err := a.gitCore().Checkout(workspace, branch); err != nil {
		return GitWorkspaceState{}, err
	}

	// Checkout swaps the working tree to a different branch — bust the
	// @-mention picker cache so it reflects the new tree.
	if a.workspaceFiles != nil {
		a.workspaceFiles.Invalidate(workspace)
	}

	state := a.workspaceState(project, workspace)
	if _, err := a.UpdateThreadBranch(workspace, state.Branch); err != nil {
		return GitWorkspaceState{}, fmt.Errorf("git checkout: record branch on threads: %w", err)
	}
	return state, nil
}

// GitCreateBranchFrom creates a new branch in the referenced workspace
// (project root or one of its worktrees), pointed at baseBranch, then checks
// it out.
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
// Thread rows in the workspace are re-branched as for GitCheckout. Does not
// restart provider sessions because the cwd is unchanged.
func (a *App) GitCreateBranchFrom(ws WorkspaceRef, name, baseBranch string, carryLocalChanges bool) (GitWorkspaceState, error) {
	project, workspace, err := a.gitApplication().ResolveWorkspace(ws)
	if err != nil {
		return GitWorkspaceState{}, err
	}

	_, release, err := a.lockWorkspaceThreads(workspace)
	if err != nil {
		return GitWorkspaceState{}, err
	}
	defer release()

	core := a.gitCore()
	sanitized, resolvedBase, baseIsCurrent, err := resolveBranchCreatePlan(
		core.CurrentBranch(workspace), name, baseBranch, carryLocalChanges)
	if err != nil {
		return GitWorkspaceState{}, err
	}

	if !baseIsCurrent {
		if err := core.EnsureLocalBranchDoesNotExist(workspace, sanitized); err != nil {
			return GitWorkspaceState{}, fmt.Errorf("create branch: %w", err)
		}
	}

	if err := a.createBranchInWorkspace(workspace, sanitized, resolvedBase, baseIsCurrent); err != nil {
		return GitWorkspaceState{}, err
	}

	if a.workspaceFiles != nil {
		a.workspaceFiles.Invalidate(workspace)
	}
	state := a.workspaceState(project, workspace)
	if _, err := a.UpdateThreadBranch(workspace, state.Branch); err != nil {
		return GitWorkspaceState{}, fmt.Errorf("create branch: record branch on threads: %w", err)
	}
	return state, nil
}

// errCarryRequiresCurrentBase is the ONE refusal every "Local with changes"
// check issues. Carrying uncommitted work forward is a move; carrying it onto
// an unrelated base is a rebase, which is a different request the UI does not
// offer. Three call sites used to spell this sentence out verbatim — the
// branch-create path through resolveBranchCreatePlan below, and the two
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
// the one thing that must not be guessed at from the project root when the
// caller is in a worktree.
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
func (a *App) GitCreatePR(ws WorkspaceRef, title, body string, draft bool) (gitops.GitActionResult, error) {
	return a.gitApplication().CreatePR(ws, title, body, draft)
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
//
// This is the THREAD-scoped resolver, for RPCs whose subject is the thread
// itself. A caller-supplied workspace path must go through
// gitapp.Service.ResolveWorkspace instead — that, and only that, is where an
// outside path is validated against a project.
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

// lockWorkspaceThreads takes the per-thread action lock of EVERY thread
// referencing a checkout, in sorted id order so two mutators of the same
// directory can never deadlock against each other. It returns that sorted,
// deduped id set (which callers that also mutate the rows need) and one
// release func.
//
// Every workspace mutator goes through here. Taking a single thread's lock
// by guess is what let thread B mutate a worktree thread A was working in:
// two threads sharing a checkout is first-class, so the lock set is the
// directory's occupants, never one caller.
func (a *App) lockWorkspaceThreads(workspace string) ([]string, func(), error) {
	occupants, err := a.threadsReferencingWorkspace(workspace)
	if err != nil {
		return nil, nil, err
	}
	slices.Sort(occupants)
	occupants = slices.Compact(occupants)

	unlocks := make([]func(), 0, len(occupants))
	for _, id := range occupants {
		unlocks = append(unlocks, a.threadLocks().Lock(id))
	}
	return occupants, func() {
		// Release in reverse order to match LIFO mutex hygiene.
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}, nil
}

// ensureWorkspaceChangeAllowed refuses to DELETE a checkout while any thread
// in it is working. The entity is the DIRECTORY, not the conversation: two
// threads sharing a worktree is first-class, so a removal requested from
// thread B would otherwise pull the directory out from under thread A's
// running agent.
//
// It gates removal ONLY. Branch changes (GitCheckout, GitCreateBranchFrom,
// GitPull, GitSyncBranch) are deliberately never gated on agent activity:
// the user owns the branch and switches it whenever they like, agent or no
// agent (ruling 2026-09-02). Do not add this check to them.
//
// It reads the same WorkspaceActivity projection the frontend's affordances
// gate on, so a live button and a backend refusal cannot disagree — including
// the words: these are the frontend workspace-change lock's two states
// (TURN_REASON / TASKS_REASON), so the toast and the disabled-button tooltip
// say the same thing. action names what the user asked for ("remove this
// worktree") because the final `: `-segment must
// stand alone — the frontend's userFacingError keeps only that segment — so
// these messages carry no colon and cannot borrow context from a prefix.
//
// The busy thread is deliberately NOT named: a thread id is a uuid, and a uuid
// in a toast is noise the user cannot act on.
func (a *App) ensureWorkspaceChangeAllowed(action, workspace string) error {
	activity, err := a.worktreeApplication().Activity(workspace)
	if err != nil {
		return err
	}
	if len(activity.BusyThreads) == 0 {
		return nil
	}
	busy := activity.BusyThreads[0]
	if busy.ActiveTurn {
		return fmt.Errorf("cannot %s while an agent is responding in it", action)
	}
	return fmt.Errorf("cannot %s while %d background task(s) are running in it",
		action, busy.RunningBackgroundTasks)
}

// ensureThreadChangeAllowed refuses to move ONE thread out of its workspace
// while that thread's own turn or background tools are in flight — the
// provider session is bound to the cwd and changing it mid-turn would orphan
// output. Deliberately NOT the directory question: moving an idle thread out
// of a checkout a sibling is working in touches only the idle thread's row.
func (a *App) ensureThreadChangeAllowed(threadID string) error {
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
