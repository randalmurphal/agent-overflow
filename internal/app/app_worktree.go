package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"slices"
	"strings"

	"agent-overflow/internal/eventchan"
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
	unlock := a.threadLocks().Lock(threadID)
	// Registered BEFORE the unlock defer so LIFO runs it AFTER the lock is
	// released: the setup run outlives this call, and starting it under the
	// thread lock would block every other operation on the thread for as long
	// as the record it registers takes to appear.
	var provisioned *store.Thread
	defer func() {
		if provisioned != nil {
			a.startThreadWorktreeSetup(*provisioned)
		}
	}()
	defer unlock()

	// Read under the lock: a freshly materialized draft thread can be deleted
	// concurrently (empty-draft cleanup), and a row read before the lock could
	// vanish before the UpdateThread below — the worktree would be cut for a
	// thread that no longer exists.
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return store.Thread{}, err
	}

	project, _, err := a.resolveGitPaths(thread)
	if err != nil {
		return store.Thread{}, err
	}

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
		return store.Thread{}, fmt.Errorf("create worktree: %w", errCarryRequiresCurrentBase)
	}

	sourceWorkspace := strings.TrimSpace(thread.WorkspacePath)
	if sourceWorkspace == "" {
		sourceWorkspace = project
	}

	worktreePath, err := a.cutWorktreeWithCarry(worktreeCutRequest{
		projectPath:       project,
		sourceWorkspace:   sourceWorkspace,
		baseBranch:        resolvedBase,
		newBranch:         resolvedBranch,
		carryLocalChanges: carryLocalChanges,
	})
	if err != nil {
		return store.Thread{}, err
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
	}
	if err := a.store.UpdateThread(thread); err != nil {
		// Worktree was created on disk but the store update failed. Clean up
		// so we don't leak a worktree directory.
		_ = core.RemoveWorktreeForce(project, worktreePath, true)
		return store.Thread{}, err
	}
	// Commit succeeded — drop the stale pre-move copies (best-effort).
	a.purgeRelocatedClaudeSessions(threadID, purge)
	// The thread just left whatever workspace it was in; a setup run still
	// going for that one describes a worktree it no longer occupies.
	a.releaseThreadWorktreeSetup(threadID, thread.WorkspacePath)
	refreshed, err := a.restartSessionIfAffected(threadID, "workspace")
	if err != nil {
		return store.Thread{}, fmt.Errorf("create worktree: refresh thread after workspace switch: %w", err)
	}
	// This call cut the worktree, so the project's recipe runs over it — see
	// the deferred kickoff above for why it is not started here.
	provisioned = &refreshed
	return refreshed, nil
}

// AttachThreadWorktree creates a worktree pointing at an existing branch and
// switches the thread to it. Distinct from PrepareThreadWorktree (which always
// creates a new branch via `git worktree add -b`): this path is `git worktree
// add <path> <existing>` and refuses if the branch is already checked out
// elsewhere — git's own one-branch-one-worktree invariant. Frontend dedups by
// flipping to the existing worktree before calling here.
func (a *App) AttachThreadWorktree(threadID, branch string) (store.Thread, error) {
	unlock := a.threadLocks().Lock(threadID)
	// Registered BEFORE the unlock defer so LIFO runs it AFTER the lock is
	// released — same rationale as PrepareThreadWorktree's kickoff.
	var provisioned *store.Thread
	defer func() {
		if provisioned != nil {
			a.startThreadWorktreeSetup(*provisioned)
		}
	}()
	defer unlock()

	// Read under the lock — see PrepareThreadWorktree for why a pre-lock read
	// races the empty-draft cleanup's delete.
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return store.Thread{}, err
	}

	project, _, err := a.resolveGitPaths(thread)
	if err != nil {
		return store.Thread{}, err
	}

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
	}
	if err := a.store.UpdateThread(thread); err != nil {
		_ = core.RemoveWorktreeForce(project, worktreePath, true)
		return store.Thread{}, err
	}
	a.purgeRelocatedClaudeSessions(threadID, purge)
	// The thread just left whatever workspace it was in; a setup run still
	// going for that one describes a worktree it no longer occupies.
	a.releaseThreadWorktreeSetup(threadID, thread.WorkspacePath)
	refreshed, err := a.restartSessionIfAffected(threadID, "workspace")
	if err != nil {
		return store.Thread{}, fmt.Errorf("attach worktree: refresh thread after workspace switch: %w", err)
	}
	// This call cut the worktree. The branch already existed, but the checkout
	// is freshly created (attach refuses a branch checked out anywhere else),
	// so the project's recipe runs over it like any other fresh cut — the
	// recipe is a convention about the directory, not the branch.
	provisioned = &refreshed
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
	// Membership is validated on BOTH force paths — force skips the
	// loss-of-work gate below, not the "is this actually one of the
	// project's worktrees" boundary. Without this, a forced removal's
	// only guard against an arbitrary path is git's own refusal.
	if _, ok, err := a.findWorktree(project, worktreePath); err != nil {
		return fmt.Errorf("validate worktree: %w", err)
	} else if !ok {
		return fmt.Errorf("%s is not a worktree of project %s", worktreePath, project)
	}
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

	// Identify every thread that points at the worktree. Each of them gets
	// reattached to the project root by the best-effort sweep below, so
	// each must be idle and locked against concurrent workspace mutations
	// before we touch git.
	attached, err := a.threadsReferencingWorkspace(worktreePath)
	if err != nil {
		return err
	}
	// Sorted + deduped so the post-lock recompute below can compare sets.
	slices.Sort(attached)
	attached = slices.Compact(attached)
	// The caller is locked too (serializing its other workspace ops against
	// this removal) but is only activity-checked when it actually occupies
	// the worktree: a running thread may clean up worktrees it isn't in.
	// Clone before append/sort — sharing attached's backing array would let
	// the in-place sort swap the caller into attached's view and silently
	// drop an occupying thread from the check and the reattach sweep.
	locked := slices.Clone(attached)
	if callerThreadID != "" && !slices.Contains(locked, callerThreadID) {
		locked = append(locked, callerThreadID)
	}
	slices.Sort(locked)
	locked = slices.Compact(locked)

	unlocks := make([]func(), 0, len(locked))
	defer func() {
		// Release in reverse order to match LIFO mutex hygiene.
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}()
	for _, id := range locked {
		unlocks = append(unlocks, a.threadLocks().Lock(id))
	}

	// The occupancy snapshot above ran unlocked; a thread can have
	// switched into the worktree in that window (it only holds its own
	// lock, which we don't). Same recompute-and-refuse guard as
	// DeleteProjectAndThreads — a changed set means our lock set no
	// longer covers the occupants, so refuse rather than strand the
	// newcomer on a deleted path. Accepted residual window (same as
	// project deletion): a switch that commits after this recheck but
	// before RemoveWorktreeForce finishes still lands on the deleted
	// path; closing it would need removal and every workspace-entry
	// path to contend on a shared project-scoped lock.
	recheck, err := a.threadsReferencingWorkspace(worktreePath)
	if err != nil {
		return err
	}
	slices.Sort(recheck)
	if !slices.Equal(attached, slices.Compact(recheck)) {
		return fmt.Errorf("worktree %s occupancy changed during removal; retry", worktreePath)
	}

	for _, id := range attached {
		reason, err := a.threadActivityBlockReason(id)
		if err != nil {
			return fmt.Errorf("thread %s: %w", id, err)
		}
		if reason != "" {
			// The final colon segment must stand alone: the frontend's
			// userFacingError keeps only the last `: `-segment for toasts.
			return fmt.Errorf("worktree %s in use by thread %s: cannot remove worktree while %s", worktreePath, id, reason)
		}
	}

	// Cancel by directory and block on the join. A recipe still writing into the
	// path would race git's removal and could recreate entries after removal.
	// A run can belong to a thread that has since moved away, so the attached
	// thread list is not an authoritative set of processes to stop.
	a.cancelWorktreeSetupsForPath(worktreePath)
	// Then clear the durable state of every thread that pointed here — a "Setup
	// failed" pill for a worktree that no longer exists has nothing to offer.
	// Their runs are already gone, so this is the column and nothing else.
	for _, id := range attached {
		a.cancelThreadWorktreeSetup(id)
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
		a.emitEvent(eventchan.ThreadUpdated, triage.ThreadUpdateEvent{Action: triage.ThreadActionFull, Thread: &t})
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
	projectsDir, err := a.claudeProjectsDir()
	if err != nil {
		return nil, err
	}
	var purge []string
	for _, ref := range claudeSessionRefs(t) {
		src, dest, err := sessionfork.RelocateSession(projectsDir, ref, fromWorkspace, t.WorkspacePath)
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

// threadsReferencingWorkspace returns every thread id whose workspace or
// worktree path matches the supplied path (a worktree or the project root).
func (a *App) threadsReferencingWorkspace(path string) ([]string, error) {
	return a.worktreeApplication().ThreadsReferencingWorkspace(path)
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
	status, err := a.worktreeApplication().Status(project, worktreePath)
	return WorktreeStatus(status), err
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
	items, err := a.worktreeApplication().List(project)
	if err != nil {
		return nil, err
	}
	result := make([]WorktreeListItem, len(items))
	for index := range items {
		result[index] = WorktreeListItem(items[index])
	}
	return result, nil
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
	previousWorktree := thread.WorktreePath
	previousBranch := thread.Branch
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
	}
	if err := a.store.UpdateThread(thread); err != nil {
		return store.Thread{}, err
	}
	a.purgeRelocatedClaudeSessions(threadID, purge)
	// Switching into an existing workspace never runs setup — the target was
	// provisioned (or not) by whoever cut it. Switching AWAY releases the run
	// whose worktree the thread has left; switching back into the same path is
	// a no-op the helper recognises by comparing paths.
	a.releaseThreadWorktreeSetup(threadID, thread.WorkspacePath)
	refreshed, err := a.restartSessionIfAffected(threadID, "workspace")
	if err != nil {
		return store.Thread{}, fmt.Errorf("switch workspace: refresh thread after workspace switch: %w", err)
	}
	// Broadcast so a second attached client's pane follows the thread to its
	// new checkout. `store.UpdateThread` rewrites the whole row, so the
	// no-change test is the three fields this switch owns — re-selecting the
	// workspace the thread already sits in moves nothing and says nothing.
	a.broadcastThreadRowIfChanged(triage.ThreadActionFull, refreshed,
		previousWorkspace != thread.WorkspacePath ||
			previousWorktree != thread.WorktreePath ||
			previousBranch != thread.Branch)
	return refreshed, nil
}

// findWorktree resolves a path to one of the project's registered worktrees.
// Returns ok=false (no error) when the path doesn't match any known worktree.
func (a *App) findWorktree(project, path string) (gitops.Worktree, bool, error) {
	return a.worktreeApplication().Find(project, path)
}

// cutWorktreeFromFreshBase is the app's one entry point for cutting a NEW
// worktree branch from a base branch that may also live on origin: chat
// threads (new-thread and switch-to-worktree) and workflow items all route
// through it, so the fetch-first rule can't drift between them.
//
// It owns the failure posture that internal/git deliberately leaves to the
// caller: a fetch that fails or times out is a log line and nothing more.
// The user asked for a worktree, not for a network round trip, and the cut
// they asked for succeeded — from the local base, exactly as it did before
// this behaviour existed.
func (a *App) cutWorktreeFromFreshBase(ctx context.Context, projectPath, worktreePath, baseBranch, newBranch string) error {
	seed, err := a.gitCore().CreateWorktreeFromFreshBase(ctx, projectPath, worktreePath, baseBranch, newBranch)
	if seed.FetchErr != nil {
		log.Printf("create worktree %s: fetch origin for base %q: %v (cutting from the local branch)",
			projectPath, baseBranch, seed.FetchErr)
	}
	return err
}

// worktreeCutRequest states one "cut a new worktree off a base branch"
// operation. Thread and workflow callers share it so the carry bracket below
// cannot drift between them.
type worktreeCutRequest struct {
	projectPath string
	// sourceWorkspace is where a carried dirty tree is stashed FROM: the
	// thread's own workspace for a thread-scoped cut. Read only when
	// carryLocalChanges is set.
	sourceWorkspace   string
	baseBranch        string
	newBranch         string
	carryLocalChanges bool
}

// cutWorktreeWithCarry runs the stash → cut → apply → drop bracket and returns
// the path of the worktree it created.
//
// On stash-apply failure the worktree is removed and the stash entry is kept so
// the user can recover via `git stash list`; the message names the entry.
func (a *App) cutWorktreeWithCarry(req worktreeCutRequest) (string, error) {
	core := a.gitCore()

	stashMessage := ""
	stashed := false
	if req.carryLocalChanges {
		stashMessage = fmt.Sprintf("ao-carry-%s", gitops.RandomStashSuffix())
		created, err := core.StashPushIncludeUntracked(req.sourceWorkspace, stashMessage)
		if err != nil {
			return "", fmt.Errorf("create worktree: stash local changes: %w", err)
		}
		stashed = created
	}

	worktreePath, err := a.defaultWorktreePath(req.projectPath, req.newBranch)
	if err != nil {
		if stashed {
			a.restoreStashOnError(req.sourceWorkspace, stashMessage)
		}
		return "", err
	}
	// Carry-over pins the cut to the LOCAL base. The dirty tree about to be
	// applied was authored against the branch as it exists on this machine,
	// so starting the worktree at origin's newer tip would turn "move my
	// changes" into "rebase my changes" — and a conflict there fails the
	// whole create (the worktree is torn down, the stash left for manual
	// recovery). Every other cut takes the fetched base.
	if req.carryLocalChanges {
		err = core.CreateWorktreeFromBranch(req.projectPath, worktreePath, req.baseBranch, req.newBranch)
	} else {
		err = a.cutWorktreeFromFreshBase(a.lifeCtx(), req.projectPath, worktreePath, req.baseBranch, req.newBranch)
	}
	if err != nil {
		if stashed {
			a.restoreStashOnError(req.sourceWorkspace, stashMessage)
		}
		return "", err
	}

	if stashed {
		if err := core.StashApplyByMessage(worktreePath, stashMessage); err != nil {
			// Apply failed in the new worktree. Tear the worktree down and
			// keep the stash so the user can recover the changes manually.
			_ = core.RemoveWorktreeForce(req.projectPath, worktreePath, true)
			return "", fmt.Errorf("create worktree: apply local changes in new worktree: %w (recover with: git stash list  →  git stash apply <ref> for entry %q)", err, stashMessage)
		}
		if err := core.StashDropByMessage(req.sourceWorkspace, stashMessage); err != nil {
			// Apply succeeded but the source stash drop failed. The new
			// worktree has the changes; leave the stash in place and log
			// — the user-facing flow stays successful.
			log.Printf("create worktree: drop carried stash %q: %v", stashMessage, err)
		}
	}
	return worktreePath, nil
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
