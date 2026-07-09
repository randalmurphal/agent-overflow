package main

import (
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"slices"
	"strings"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
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

// WorktreeListItem is the picker-facing worktree shape. DeleteBlocked is true
// when at least one attached thread has an active turn or a running background
// task. Removal repeats the authoritative check while holding the thread locks;
// this field only keeps the UI affordance in sync with that backend rule.
type WorktreeListItem struct {
	Path          string `json:"path"`
	Branch        string `json:"branch"`
	Head          string `json:"head"`
	DeleteBlocked bool   `json:"deleteBlocked"`
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
	previousWorkspace := thread.WorkspacePath
	thread.WorktreePath = worktreePath
	thread.WorkspacePath = worktreePath
	thread.Branch = resolvedBranch
	var purge []string
	if !gitops.SameFilesystemPath(previousWorkspace, thread.WorkspacePath) {
		// Carry the Claude transcript to the new slug BEFORE committing. A
		// relocation that can't preserve the conversation refuses the whole
		// create — tear the worktree back down and leave the thread resumable
		// from its current workspace rather than silently start fresh.
		moved, err := a.copyClaudeSessionForWorkspaceChange(thread, previousWorkspace)
		if err != nil {
			_ = core.RemoveWorktreeForce(project, worktreePath, true)
			return store.Thread{}, fmt.Errorf("create worktree: %w", err)
		}
		purge = moved
		if err := a.updateThreadAndInvalidateCheckpointsForWorkspaceChange(thread, "create worktree", project); err != nil {
			_ = core.RemoveWorktreeForce(project, worktreePath, true)
			return store.Thread{}, err
		}
	} else if err := a.store.UpdateThread(thread); err != nil {
		// Worktree was created on disk but the store update failed. Clean up
		// so we don't leak a worktree directory.
		_ = core.RemoveWorktreeForce(project, worktreePath, true)
		return store.Thread{}, err
	}
	// Commit succeeded — drop the stale pre-move copies (best-effort).
	a.purgeRelocatedClaudeSessions(threadID, purge)
	refreshed, err := a.restartSessionIfAffected(threadID, "workspace")
	if err != nil {
		return store.Thread{}, fmt.Errorf("create worktree: refresh thread after workspace switch: %w", err)
	}
	return refreshed, nil
}

// AttachThreadWorktree creates a worktree pointing at an existing branch and
// switches the thread to it. Distinct from PrepareThreadWorktree (which always
// creates a new branch via `git worktree add -b`): this path is `git worktree
// add <path> <existing>` and refuses if the branch is already checked out
// elsewhere — git's own one-branch-one-worktree invariant. Frontend dedups by
// flipping to the existing worktree before calling here.
func (a *App) AttachThreadWorktree(threadID, branch string) (store.Thread, error) {
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

	branch = strings.TrimSpace(branch)
	if branch == "" {
		return store.Thread{}, fmt.Errorf("attach worktree: branch is required")
	}

	core := a.gitCore()

	worktreePath, err := a.defaultWorktreePath(project, branch)
	if err != nil {
		return store.Thread{}, err
	}
	if err := core.AttachWorktree(project, worktreePath, branch); err != nil {
		return store.Thread{}, err
	}

	previousWorkspace := thread.WorkspacePath
	thread.WorktreePath = worktreePath
	thread.WorkspacePath = worktreePath
	thread.Branch = branch
	var purge []string
	if !gitops.SameFilesystemPath(previousWorkspace, thread.WorkspacePath) {
		// Carry the Claude transcript to the new slug before committing; refuse
		// the attach (and tear the worktree back down) if it can't be preserved.
		moved, err := a.copyClaudeSessionForWorkspaceChange(thread, previousWorkspace)
		if err != nil {
			_ = core.RemoveWorktreeForce(project, worktreePath, true)
			return store.Thread{}, fmt.Errorf("attach worktree: %w", err)
		}
		purge = moved
		if err := a.updateThreadAndInvalidateCheckpointsForWorkspaceChange(thread, "attach worktree", project); err != nil {
			_ = core.RemoveWorktreeForce(project, worktreePath, true)
			return store.Thread{}, err
		}
	} else if err := a.store.UpdateThread(thread); err != nil {
		_ = core.RemoveWorktreeForce(project, worktreePath, true)
		return store.Thread{}, err
	}
	a.purgeRelocatedClaudeSessions(threadID, purge)
	refreshed, err := a.restartSessionIfAffected(threadID, "workspace")
	if err != nil {
		return store.Thread{}, fmt.Errorf("attach worktree: refresh thread after workspace switch: %w", err)
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
	return a.removeProjectWorktree(project, threadID, worktreePath, force)
}

// RemoveOtherWorktreeForProject removes a project worktree without requiring a
// thread row. currentWorkspacePath is the caller's transient workspace state;
// when it points at the removed worktree the returned state resets the caller
// to the project root.
func (a *App) RemoveOtherWorktreeForProject(projectID, currentWorkspacePath, worktreePath string, force bool) (GitWorkspaceState, error) {
	project, err := a.gitProjectPath(projectID)
	if err != nil {
		return GitWorkspaceState{}, err
	}
	if err := a.removeProjectWorktree(project, "", worktreePath, force); err != nil {
		return GitWorkspaceState{}, err
	}
	return a.resolveProjectWorkspaceStateAfterRemoval(project, currentWorkspacePath, worktreePath)
}

func (a *App) removeProjectWorktree(project, callerThreadID, worktreePath string, force bool) error {
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
	if callerThreadID != "" && !slices.Contains(attached, callerThreadID) {
		attached = append(attached, callerThreadID)
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
		previousWorkspace := t.WorkspacePath
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
		if !gitops.SameFilesystemPath(previousWorkspace, t.WorkspacePath) {
			refs, err := a.store.DeleteCheckpointsForThreadReturningRefs(id)
			if err != nil {
				sweepErrs = append(sweepErrs, fmt.Errorf("thread %s checkpoint cleanup failed: %w", id, err))
			} else {
				a.deleteCheckpointRefsBestEffort(id, "remove worktree", project, refs)
			}
			// Claude resolves --resume against the slug of the current cwd, so
			// reattaching to the project root strands the transcript under the
			// deleted worktree's slug — the next resume would fail with "No
			// conversation found". Move it so the conversation survives; we never
			// clear the session ref or silently start fresh. The reattach is
			// already committed above and the worktree is gone, so unlike
			// switch/create/attach this can't be aborted: a hard failure is
			// surfaced and resume is left to fail loudly. This runs before the
			// restart below, which re-resolves the session at the new cwd.
			moved, err := a.copyClaudeSessionForWorkspaceChange(t, previousWorkspace)
			if err != nil {
				sweepErrs = append(sweepErrs, fmt.Errorf("thread %s session relocate failed: %w", id, err))
			} else {
				a.purgeRelocatedClaudeSessions(id, moved)
			}
		}
		// Other panes only know to re-render when the thread:updated event
		// fires — without it the sibling pane keeps showing the deleted
		// worktree path until the user navigates. The caller's pane gets a
		// redundant echo (the binding return already syncs it), which the
		// pane store treats as idempotent.
		a.emitEvent("thread:updated", triage.ThreadUpdateEvent{Action: "full", Thread: &t})
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

// claudeSessionRefs returns a Claude thread's deduped, non-empty session refs
// (the live SessionRef plus any PendingForkRef) in a stable order. Both can
// have a transcript on disk that a workspace change must carry along.
func claudeSessionRefs(t store.Thread) []string {
	refs := make([]string, 0, 2)
	for _, ref := range []string{t.SessionRef, t.PendingForkRef} {
		ref = strings.TrimSpace(ref)
		if ref != "" && !slices.Contains(refs, ref) {
			refs = append(refs, ref)
		}
	}
	return refs
}

// copyClaudeSessionForWorkspaceChange copies a Claude thread's session
// transcript(s) into t.WorkspacePath's project slug so `claude --resume` keeps
// resolving them after the workspace changes. Claude keys session lookup on the
// cwd slug, so moving a thread to a directory that never held its session would
// otherwise brick the next resume with "No conversation found".
//
// It is the COPY half of a move and does NOT delete the originals: it returns
// the source paths so the caller can purge them with purgeRelocatedClaudeSessions
// AFTER it commits the workspace change. That ordering is what makes the change
// abort-safe — a hard error here leaves every source transcript intact, so a
// caller that can roll back (switch/create/attach) refuses the change with the
// conversation still resumable from its current workspace.
//
// Returns a hard error (and no purge list) when ANY ref would be stranded: the
// destination slug is uncomputable, or the transcript copy failed. A genuinely
// missing transcript (nothing to relocate) and a partial subagent-subdir copy
// are both non-fatal and logged — never a reason to fabricate a fresh session.
//
// Both the headless `claude` provider and `claude-tui` run the same CLI binary
// with cwd-keyed `--resume` (claude-tui learns its session id from the
// SessionStart hook and persists it through the same handleInit path), so both
// brick identically on a workspace change and both relocate here. Codex no-ops:
// it resumes by thread id from ~/.codex, not a cwd slug, so it has no equivalent
// failure.
func (a *App) copyClaudeSessionForWorkspaceChange(t store.Thread, fromWorkspace string) ([]string, error) {
	if t.Provider != string(provider.Claude) && t.Provider != string(provider.ClaudeTUI) {
		return nil, nil
	}
	var purge []string
	for _, ref := range claudeSessionRefs(t) {
		src, dest, err := sessionfork.RelocateSession(ref, fromWorkspace, t.WorkspacePath)
		switch {
		case err == nil:
			if src != dest {
				purge = append(purge, src)
			}
		case errors.Is(err, sessionfork.ErrSessionFileNotFound):
			// Nothing on disk to relocate — the session is already gone, or this
			// thread never produced a transcript under the old slug. Resume will
			// surface "No conversation found" if truly gone; we deliberately do
			// NOT fabricate a fresh session in its place.
			log.Printf("thread %s: claude session %s not on disk during workspace change; leaving resume to surface", t.ID, ref)
		case errors.Is(err, sessionfork.ErrSubagentCopyIncomplete):
			// Soft: the main transcript relocated (resume works), but the subagent
			// subdir copy was partial. Surface it and keep going, but deliberately
			// do NOT purge the source — keeping it preserves the un-copied subagent
			// history under the old slug (a harmless duplicate of the main
			// transcript, overwritten if the thread returns) rather than losing it.
			log.Printf("thread %s: claude session %s relocated but subagent history incomplete: %v", t.ID, ref, err)
		default:
			// Hard: the conversation would be stranded at the new cwd. Abort with
			// nothing moved; the caller decides whether to refuse or surface.
			return nil, fmt.Errorf("relocate claude session %s: %w", ref, err)
		}
	}
	return purge, nil
}

// purgeRelocatedClaudeSessions removes the pre-move source transcripts returned
// by copyClaudeSessionForWorkspaceChange. It MUST run only after the workspace
// change has committed: a removal failure leaves a harmless orphan (the
// authoritative copy is already at the new slug, and every relocation overwrites
// the destination), so it is logged, never surfaced.
func (a *App) purgeRelocatedClaudeSessions(threadID string, purge []string) {
	for _, src := range purge {
		if err := sessionfork.RemoveSessionTranscript(src); err != nil {
			log.Printf("thread %s: purge stale claude transcript %s after workspace change: %v", threadID, src, err)
		}
	}
}

func (a *App) resolveProjectWorkspaceStateAfterRemoval(project, currentWorkspacePath, removedWorktreePath string) (GitWorkspaceState, error) {
	core := a.gitCore()
	currentWorkspacePath = strings.TrimSpace(currentWorkspacePath)
	switch {
	case currentWorkspacePath == "":
		return GitWorkspaceState{
			WorkspacePath: project,
			Branch:        core.CurrentBranch(project),
		}, nil
	case gitops.SameFilesystemPath(currentWorkspacePath, project):
		return GitWorkspaceState{
			WorkspacePath: project,
			Branch:        core.CurrentBranch(project),
		}, nil
	case gitops.SameFilesystemPath(currentWorkspacePath, removedWorktreePath):
		return GitWorkspaceState{
			WorkspacePath: project,
			Branch:        core.CurrentBranch(project),
		}, nil
	}

	worktree, ok, err := a.findWorktree(project, currentWorkspacePath)
	if err != nil {
		log.Printf("worktree removal: refresh workspace %q after removing %q: %v", currentWorkspacePath, removedWorktreePath, err)
		return GitWorkspaceState{
			WorkspacePath: project,
			Branch:        core.CurrentBranch(project),
		}, nil
	}
	if !ok {
		return GitWorkspaceState{
			WorkspacePath: project,
			Branch:        core.CurrentBranch(project),
		}, nil
	}
	branch := strings.TrimSpace(worktree.Branch)
	if branch == "" {
		branch = core.CurrentBranch(worktree.Path)
	}
	return GitWorkspaceState{
		WorkspacePath: worktree.Path,
		WorktreePath:  worktree.Path,
		Branch:        branch,
	}, nil
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

// GitWorktreeStatusForProject classifies a project worktree without requiring a
// thread row.
func (a *App) GitWorktreeStatusForProject(projectID, worktreePath string) (WorktreeStatus, error) {
	project, err := a.gitProjectPath(projectID)
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
func (a *App) GitListWorktrees(threadID string) ([]WorktreeListItem, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return nil, err
	}

	project, _, err := a.resolveGitPaths(thread)
	if err != nil {
		return nil, err
	}

	return a.listWorktreesForPicker(project)
}

// GitListWorktreesForProject lists worktrees for a project without requiring
// a thread row.
func (a *App) GitListWorktreesForProject(projectID string) ([]WorktreeListItem, error) {
	project, err := a.gitProjectPath(projectID)
	if err != nil {
		return nil, err
	}
	return a.listWorktreesForPicker(project)
}

func (a *App) listWorktreesForPicker(project string) ([]WorktreeListItem, error) {
	worktrees, err := a.gitCore().ListWorktrees(project)
	if err != nil {
		return nil, err
	}
	refs, err := a.store.ListThreadWorkspaceRefsWithActivity()
	if err != nil {
		return nil, err
	}

	items := make([]WorktreeListItem, len(worktrees))
	for i, worktree := range worktrees {
		items[i] = WorktreeListItem{
			Path:   worktree.Path,
			Branch: worktree.Branch,
			Head:   worktree.HEAD,
		}
	}

	itemByPath := make(map[string]int, len(items))
	for i := range items {
		itemByPath[gitops.CanonicalPath(items[i].Path)] = i
	}
	for _, ref := range refs {
		if !ref.WorkspaceChangeBlocked {
			continue
		}
		for _, path := range []string{ref.WorktreePath, ref.WorkspacePath} {
			if strings.TrimSpace(path) == "" {
				continue
			}
			if i, ok := itemByPath[gitops.CanonicalPath(path)]; ok {
				items[i].DeleteBlocked = true
			}
		}
	}
	return items, nil
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
	previousWorkspace := thread.WorkspacePath
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
	var purge []string
	if !gitops.SameFilesystemPath(previousWorkspace, thread.WorkspacePath) {
		// Carry the Claude transcript to the target slug before committing. If it
		// can't be preserved, refuse the switch: the thread stays in its current
		// workspace, where its history still resolves, rather than moving into a
		// state where resume fails.
		moved, err := a.copyClaudeSessionForWorkspaceChange(thread, previousWorkspace)
		if err != nil {
			return store.Thread{}, fmt.Errorf("switch workspace: %w", err)
		}
		purge = moved
		if err := a.updateThreadAndInvalidateCheckpointsForWorkspaceChange(thread, "switch workspace", project); err != nil {
			return store.Thread{}, err
		}
	} else if err := a.store.UpdateThread(thread); err != nil {
		return store.Thread{}, err
	}
	a.purgeRelocatedClaudeSessions(threadID, purge)
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
