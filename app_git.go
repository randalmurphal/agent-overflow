package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
		currentBranch = currentGitBranch(core, workspace)
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

	unlock := sendThreadMuRegistry.lockFor(threadID)
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
		stashMessage := fmt.Sprintf("ao-discard-%s", shortStashID())
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
	thread.Branch = currentGitBranch(core, workspace)
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
		Branch:  currentGitBranch(core, workspace),
		PRURL:   url,
		Message: "Created pull request",
	}, nil
}

// GitCreateWorktree creates a new worktree for the requested branch and returns its path.
// Preserves legacy semantics — no carry-over of local changes.
func (a *App) GitCreateWorktree(threadID, branch string) (string, error) {
	updated, err := a.PrepareThreadWorktree(threadID, "", branch, false)
	if err != nil {
		return "", err
	}
	return updated.WorktreePath, nil
}

// PrepareThreadWorktree creates a new worktree from baseBranch, switches the
// thread to it, and returns the updated thread. requestedBranch is optional:
// blank means "create a temporary auto branch using the configured prefix".
//
// When carryLocalChanges is true, the source workspace's dirty tree (staged
// + unstaged + untracked) is stashed under a per-call message, the worktree
// is created, and the stash is applied in the new worktree before the entry
// is dropped from the stash stack. carryLocalChanges only applies when the
// new worktree's base branch matches the source thread's current branch — a
// "Local with changes" semantic only makes sense when both ends agree on the
// base. When base diverges from current, the request is rejected with a
// clear error so the caller can surface it.
//
// On stash-apply failure the worktree is removed and the stash entry is
// kept so the user can recover via `git stash list`.
func (a *App) PrepareThreadWorktree(threadID, baseBranch, requestedBranch string, carryLocalChanges bool) (store.Thread, error) {
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

	core := a.gitCore()
	resolvedBase := strings.TrimSpace(baseBranch)
	if resolvedBase == "" {
		resolvedBase = strings.TrimSpace(thread.Branch)
	}
	if resolvedBase == "" {
		resolvedBase = currentGitBranch(core, project)
	}
	if resolvedBase == "" {
		return store.Thread{}, fmt.Errorf("create worktree: base branch is required")
	}

	// Carry-over only makes sense from the thread's current branch. The
	// source workspace's dirty state is what we'd be moving — moving it
	// onto an unrelated base is a different semantic (rebase-style) we
	// deliberately don't support.
	if carryLocalChanges && resolvedBase != strings.TrimSpace(thread.Branch) {
		return store.Thread{}, fmt.Errorf("create worktree: 'Local with changes' only applies when the base matches the current branch")
	}

	sourceWorkspace := strings.TrimSpace(thread.WorkspacePath)
	if sourceWorkspace == "" {
		sourceWorkspace = project
	}

	stashMessage := ""
	stashed := false
	if carryLocalChanges {
		stashMessage = fmt.Sprintf("ao-carry-%s", shortStashID())
		created, err := core.StashPushIncludeUntracked(sourceWorkspace, stashMessage)
		if err != nil {
			return store.Thread{}, fmt.Errorf("create worktree: stash local changes: %w", err)
		}
		stashed = created
	}

	worktreePath, err := a.defaultWorktreePath(project, resolvedBranch)
	if err != nil {
		if stashed {
			a.restoreStashOnError(sourceWorkspace, stashMessage)
		}
		return store.Thread{}, err
	}
	if err := core.CreateWorktreeFromBranch(project, worktreePath, resolvedBase, resolvedBranch); err != nil {
		if stashed {
			a.restoreStashOnError(sourceWorkspace, stashMessage)
		}
		return store.Thread{}, err
	}

	if stashed {
		if err := core.StashApplyByMessage(worktreePath, stashMessage); err != nil {
			// Apply failed in the new worktree. Tear the worktree down and
			// keep the stash so the user can recover the changes manually.
			_ = core.RemoveWorktreeForce(project, worktreePath, true)
			return store.Thread{}, fmt.Errorf("create worktree: apply local changes in new worktree: %w (recover with: git stash list  →  git stash apply <ref> for entry %q)", err, stashMessage)
		}
		if err := core.StashDropByMessage(sourceWorkspace, stashMessage); err != nil {
			// Apply succeeded but the source stash drop failed. The new
			// worktree has the changes; leave the stash in place and log
			// — the user-facing flow stays successful.
			log.Printf("create worktree: drop carried stash %q: %v", stashMessage, err)
		}
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

// restoreStashOnError best-effort applies the carry-over stash back into the
// source workspace and drops it. Used when worktree creation fails before
// the stash was consumed by the new worktree, so the user's working tree
// isn't left empty. Failures here are logged; the original error is what
// surfaces to the caller.
func (a *App) restoreStashOnError(sourceWorkspace, stashMessage string) {
	core := a.gitCore()
	if err := core.StashApplyByMessage(sourceWorkspace, stashMessage); err != nil {
		log.Printf("worktree: restore carried stash %q after error: %v", stashMessage, err)
		return
	}
	if err := core.StashDropByMessage(sourceWorkspace, stashMessage); err != nil {
		log.Printf("worktree: drop restored stash %q: %v", stashMessage, err)
	}
}

// shortStashID returns a short hex token for tagging stash messages so
// concurrent carry-over operations on the same repo don't collide.
func shortStashID() string {
	var token [4]byte
	if _, err := rand.Read(token[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(token[:])
}

// GitRemoveWorktree removes the worktree the thread is currently attached to.
// Thin wrapper over RemoveOtherWorktree using the thread's own worktree path
// so the auto-reattach behavior stays unified.
func (a *App) GitRemoveWorktree(threadID string) error {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return err
	}
	worktreePath := strings.TrimSpace(thread.WorktreePath)
	if worktreePath == "" {
		return fmt.Errorf("thread %s has no worktree path", threadID)
	}
	return a.RemoveOtherWorktree(threadID, worktreePath, false)
}

// RemoveOtherWorktree removes a worktree at an explicit path, optionally
// forcing through dirty/unpushed safety. Threads attached to the worktree
// (including the calling thread) are reset back to the project root and
// their sessions restart so the workspace switch takes effect.
//
// Differs from GitRemoveWorktree in that the path is explicit (not derived
// from the calling thread) and other threads referencing the worktree are
// auto-reattached rather than blocking the call.
func (a *App) RemoveOtherWorktree(threadID, worktreePath string, force bool) error {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return err
	}
	project, _, err := a.resolveGitPaths(thread)
	if err != nil {
		return err
	}
	worktreePath = strings.TrimSpace(worktreePath)
	if worktreePath == "" {
		return fmt.Errorf("worktree path is required")
	}
	if gitops.SameFilesystemPath(project, worktreePath) {
		return fmt.Errorf("refusing to remove project root as worktree")
	}

	core := a.gitCore()
	if !force {
		// The gate refuses concrete loss-of-work signals: uncommitted
		// changes in the tree, or commits the user made that haven't
		// been pushed to the configured upstream. "No upstream" alone
		// isn't a refusal — a freshly-created worktree off main
		// rarely has one, and gating on it would make the legacy
		// GitRemoveWorktree path unusable. The UI surfaces no-upstream
		// as a visual warning and routes through force=true so this
		// gate never sees it.
		status, err := a.computeWorktreeStatus(project, worktreePath)
		if err != nil {
			return fmt.Errorf("worktree status: %w", err)
		}
		if status.Dirty || status.UnpushedCommits > 0 {
			return fmt.Errorf("worktree %s has unsaved work (dirty=%v unpushed=%d); pass force to discard", worktreePath, status.Dirty, status.UnpushedCommits)
		}
	}

	// Identify every thread that points at the worktree (the caller plus
	// any sibling threads). Each of them needs to be unlocked from
	// workspace mutations before we touch git, and each gets reattached
	// to the project root in a single transaction-like sweep.
	attached, err := a.threadsReferencingWorktree(worktreePath)
	if err != nil {
		return err
	}
	if !slices.Contains(attached, threadID) {
		attached = append(attached, threadID)
	}

	unlocks := make([]func(), 0, len(attached))
	defer func() {
		// Release in reverse order to match LIFO mutex hygiene.
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}()
	for _, id := range attached {
		unlocks = append(unlocks, sendThreadMuRegistry.lockFor(id))
		if err := a.ensureWorkspaceChangeAllowed(id); err != nil {
			return fmt.Errorf("worktree %s in use by thread %s: %w", worktreePath, id, err)
		}
	}

	if err := core.RemoveWorktreeForce(project, worktreePath, force); err != nil {
		return err
	}
	if a.workspaceFiles != nil {
		// The path no longer exists; drop any cached file list before another
		// thread's @-mention picker reaches for it.
		a.workspaceFiles.Invalidate(worktreePath)
	}

	projectBranch := currentGitBranch(core, project)
	now := time.Now().UnixMilli()
	// Best-effort sweep: the worktree is already gone, so per-thread refresh
	// failures should NOT bail mid-loop and leave siblings pointing at a
	// deleted path. Accumulate errors and surface them together at the end —
	// any successfully-mutated thread is broadcast immediately so its UI
	// catches up regardless of what happens to its neighbours.
	var sweepErrs []error
	for _, id := range attached {
		t, err := a.store.GetThread(id)
		if err != nil {
			sweepErrs = append(sweepErrs, fmt.Errorf("thread %s not refreshed: %w", id, err))
			continue
		}
		mutated := false
		if gitops.SameFilesystemPath(t.WorktreePath, worktreePath) {
			t.WorktreePath = ""
			mutated = true
		}
		if gitops.SameFilesystemPath(t.WorkspacePath, worktreePath) {
			t.WorkspacePath = project
			t.Branch = projectBranch
			mutated = true
		}
		if !mutated {
			continue
		}
		t.UpdatedAt = now
		if err := a.store.UpdateThread(t); err != nil {
			sweepErrs = append(sweepErrs, fmt.Errorf("thread %s update failed: %w", id, err))
			continue
		}
		// Other panes only know to re-render when the thread:updated event
		// fires — without it the sibling pane keeps showing the deleted
		// worktree path until the user navigates. The caller's pane gets a
		// redundant echo (the binding return already syncs it), which the
		// pane store treats as idempotent.
		a.emitEvent("thread:updated", t)
		if _, err := a.restartSessionIfAffected(id, "workspace"); err != nil {
			sweepErrs = append(sweepErrs, fmt.Errorf("thread %s session refresh failed: %w", id, err))
			continue
		}
	}
	if len(sweepErrs) > 0 {
		return fmt.Errorf("worktree removed but %d threads need attention: %w", len(sweepErrs), errors.Join(sweepErrs...))
	}
	return nil
}

// threadsReferencingWorktree returns every thread id whose workspace or
// worktree path matches the supplied worktree path.
func (a *App) threadsReferencingWorktree(worktreePath string) ([]string, error) {
	refs, err := a.store.ListThreadWorkspaceRefs()
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, ref := range refs {
		if gitops.SameFilesystemPath(ref.WorktreePath, worktreePath) ||
			gitops.SameFilesystemPath(ref.WorkspacePath, worktreePath) {
			ids = append(ids, ref.ID)
		}
	}
	return ids, nil
}


// WorktreeStatus describes a worktree's safety classification for the cleanup
// UI: whether the working tree has uncommitted changes, whether the branch
// has unpushed commits, whether an upstream is configured, and how many
// threads are currently attached to the worktree.
type WorktreeStatus struct {
	Path             string `json:"path"`
	Branch           string `json:"branch"`
	Dirty            bool   `json:"dirty"`
	UncommittedCount int    `json:"uncommittedCount"`
	UnpushedCommits  int    `json:"unpushedCommits"`
	HasUpstream      bool   `json:"hasUpstream"`
	AttachedThreads  int    `json:"attachedThreads"`
}

// GitWorktreeStatus classifies a worktree under the thread's project for the
// cleanup UI. The thread parameter is just used to resolve the project root;
// the path can be any worktree of that project.
func (a *App) GitWorktreeStatus(threadID, worktreePath string) (WorktreeStatus, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return WorktreeStatus{}, err
	}
	project, _, err := a.resolveGitPaths(thread)
	if err != nil {
		return WorktreeStatus{}, err
	}
	return a.computeWorktreeStatus(project, worktreePath)
}

// computeWorktreeStatus is the engine behind GitWorktreeStatus. Split out so
// RemoveOtherWorktree's safety gate can call it without going through a
// thread fetch the caller already did.
func (a *App) computeWorktreeStatus(project, worktreePath string) (WorktreeStatus, error) {
	worktreePath = strings.TrimSpace(worktreePath)
	if worktreePath == "" {
		return WorktreeStatus{}, fmt.Errorf("worktree path is required")
	}
	core := a.gitCore()
	worktree, ok, err := a.findWorktree(project, worktreePath)
	if err != nil {
		return WorktreeStatus{}, err
	}
	if !ok {
		return WorktreeStatus{}, fmt.Errorf("%s is not a worktree for %s", worktreePath, project)
	}

	status := WorktreeStatus{
		Path:   worktree.Path,
		Branch: worktree.Branch,
	}

	count, err := core.CountWorkingTreeChanges(worktree.Path)
	if err != nil {
		return WorktreeStatus{}, fmt.Errorf("status: %w", err)
	}
	status.UncommittedCount = count
	status.Dirty = count > 0

	if status.Branch != "" {
		unpushed, hasUpstream, err := core.CountUnpushedCommits(worktree.Path, status.Branch)
		if err != nil {
			return WorktreeStatus{}, fmt.Errorf("unpushed commits: %w", err)
		}
		status.UnpushedCommits = unpushed
		status.HasUpstream = hasUpstream
	}

	attached, err := a.threadsReferencingWorktree(worktree.Path)
	if err != nil {
		return WorktreeStatus{}, err
	}
	status.AttachedThreads = len(attached)
	return status, nil
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
