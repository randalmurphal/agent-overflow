package main

import (
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"slices"
	"strings"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
)

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

	unlock := a.threadLocks().Lock(threadID)
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
		resolvedBase = core.CurrentBranch(project)
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
		stashMessage = fmt.Sprintf("ao-carry-%s", gitops.RandomStashSuffix())
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
	slices.Sort(attached)
	attached = slices.Compact(attached)

	unlocks := make([]func(), 0, len(attached))
	defer func() {
		// Release in reverse order to match LIFO mutex hygiene.
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}()
	for _, id := range attached {
		unlocks = append(unlocks, a.threadLocks().Lock(id))
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

	projectBranch := core.CurrentBranch(project)
	// Best-effort sweep: the worktree is already gone, so per-thread refresh
	// failures should NOT bail mid-loop and leave siblings pointing at a
	// deleted path. Accumulate errors and surface them together at the end —
	// any successfully-mutated thread is broadcast immediately so its UI
	// catches up regardless of what happens to its neighbours.
	//
	// We deliberately leave UpdatedAt untouched here. The sweep is a
	// system-driven reattach, not a user-driven activity event — bumping
	// the timestamp would jump every reattached thread to the top of the
	// sidebar (which sorts by updated_at DESC), erasing the order the user
	// had built up. The frontend's syncThreadRow does a max-merge on
	// updatedAt, so the unchanged timestamp in the broadcast event is
	// invariant-safe.
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

	unlock := a.threadLocks().Lock(threadID)
	defer unlock()
	if err := a.ensureWorkspaceChangeAllowed(threadID); err != nil {
		return store.Thread{}, err
	}

	core := a.gitCore()
	switch {
	case gitops.SameFilesystemPath(target, project):
		thread.WorkspacePath = project
		thread.WorktreePath = ""
		thread.Branch = core.CurrentBranch(project)
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
			thread.Branch = core.CurrentBranch(worktree.Path)
		}
	}
	if err := a.store.UpdateThread(thread); err != nil {
		return store.Thread{}, err
	}
	refreshed, err := a.restartSessionIfAffected(threadID, "workspace")
	if err != nil {
		return store.Thread{}, fmt.Errorf("switch workspace: refresh thread after workspace switch: %w", err)
	}
	return refreshed, nil
}

// findWorktree resolves a path to one of the project's registered worktrees.
// Returns ok=false (no error) when the path doesn't match any known worktree.
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

func (a *App) defaultWorktreePath(projectPath, branch string) (string, error) {
	base := gitops.DefaultWorktreesBaseDir(projectPath)
	if strings.TrimSpace(a.configDir) != "" {
		base = filepath.Join(a.configDir, "worktrees", filepath.Base(projectPath))
	}
	return gitops.UniqueWorktreePath(filepath.Join(base, gitops.SanitizeWorktreePathSegment(branch)))
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
