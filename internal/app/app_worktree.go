package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"agent-overflow/internal/eventchan"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/store"
	"agent-overflow/internal/terminal"
	"agent-overflow/internal/triage"
)

// WorktreeStatus describes a worktree's safety classification for the cleanup
// UI: whether the working tree has uncommitted changes, whether the branch
// has unpushed commits, whether an upstream is configured, how many threads
// are currently attached to the worktree, and how many open terminals
// removing it would close.
type WorktreeStatus struct {
	Path             string `json:"path"`
	Branch           string `json:"branch"`
	Dirty            bool   `json:"dirty"`
	UncommittedCount int    `json:"uncommittedCount"`
	UnpushedCommits  int    `json:"unpushedCommits"`
	HasUpstream      bool   `json:"hasUpstream"`
	AttachedThreads  int    `json:"attachedThreads"`
	Terminals        int    `json:"terminals"`
}

// WorktreeRemoval is what an in-app worktree removal answers its caller.
type WorktreeRemoval struct {
	// Workspace is the caller's workspace after the removal: unchanged when
	// it removed some other worktree, the project root when it removed its
	// own.
	Workspace GitWorkspaceState `json:"workspace"`
	// Reattached are the thread rows the removal moved to the project root,
	// as they now stand. The caller applies them before it reads its own
	// workspace again: the thread:updated broadcast of the same rows is an
	// event, and events can arrive after this reply.
	Reattached []store.Thread `json:"reattached"`
}

// WorktreeRemovedEvent is the payload of `worktree:removed`, emitted once for
// every removal of a project's worktree the app performs or observes: its
// own removal, a workflow's cleanup, Claude's `ExitWorktree remove`, and a
// removal made outside the app. Draft placeholders live only in clients, so
// this is how every client learns to move the ones parked on Path.
type WorktreeRemovedEvent struct {
	ProjectID string `json:"projectId"`
	Path      string `json:"path"`
	// Branch is the project root's current branch, the branch a draft moved
	// to the root shows.
	Branch string `json:"branch"`
	// ThreadIDs are the threads this removal moved to the project root.
	ThreadIDs []string `json:"threadIds"`
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
	// Missing is true when git still registers the worktree but its
	// directory is gone (deleted without `git worktree remove`).
	Missing bool `json:"missing"`
}

// GitCreateWorktree creates a new worktree for the requested branch and returns its path.
// Preserves legacy semantics — no carry-over of local changes.
//
//ao:scope git:operate
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
//
//ao:scope git:operate
func (a *App) PrepareThreadWorktree(threadID, baseBranch, requestedBranch string, carryLocalChanges bool) (store.Thread, error) {
	endWork, admitErr := a.workAdmission.begin(a.lifeCtx())
	if admitErr != nil {
		return store.Thread{}, admitErr
	}
	defer endWork()
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

	if err := a.ensureThreadChangeAllowed(threadID); err != nil {
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
	move := checkoutMoveFrom(thread, "create worktree")
	// The worktree was created on disk, so any failure past here tears it
	// back down rather than leaking a directory — and a transcript that
	// cannot be relocated refuses the whole create, leaving the thread
	// resumable from the workspace it still occupies.
	move.rollback = func() { _ = core.RemoveWorktreeForce(project, worktreePath, true) }
	thread.WorktreePath = worktreePath
	thread.WorkspacePath = worktreePath
	thread.Branch = resolvedBranch
	refreshed, err := a.commitThreadCheckout(thread, move)
	if err != nil {
		return store.Thread{}, err
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
//
//ao:scope git:operate
func (a *App) AttachThreadWorktree(threadID, branch string) (store.Thread, error) {
	endWork, admitErr := a.workAdmission.begin(a.lifeCtx())
	if admitErr != nil {
		return store.Thread{}, admitErr
	}
	defer endWork()
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

	if err := a.ensureThreadChangeAllowed(threadID); err != nil {
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

	move := checkoutMoveFrom(thread, "attach worktree")
	// Same posture as the create path: the checkout exists on disk now, so a
	// transcript that cannot be relocated (or a store write that fails) tears
	// it back down instead of leaving it behind.
	move.rollback = func() { _ = core.RemoveWorktreeForce(project, worktreePath, true) }
	thread.WorktreePath = worktreePath
	thread.WorkspacePath = worktreePath
	thread.Branch = branch
	refreshed, err := a.commitThreadCheckout(thread, move)
	if err != nil {
		return store.Thread{}, err
	}
	// This call cut the worktree. The branch already existed, but the checkout
	// is freshly created (attach refuses a branch checked out anywhere else),
	// so the project's recipe runs over it like any other fresh cut — the
	// recipe is a convention about the directory, not the branch.
	provisioned = &refreshed
	return refreshed, nil
}

// GitRemoveWorktree removes the worktree the thread is currently attached to.
// Stays thread-keyed because its subject IS the thread's own attachment (the
// sidebar row action, archived threads, proposed-plan implementation); it
// resolves that attachment and hands the workspace-keyed removal the answer.
//
//ao:scope git:operate
func (a *App) GitRemoveWorktree(threadID string) (WorktreeRemoval, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return WorktreeRemoval{}, err
	}
	worktreePath := strings.TrimSpace(thread.WorktreePath)
	if worktreePath == "" {
		return WorktreeRemoval{}, fmt.Errorf("thread %s has no worktree path", threadID)
	}
	return a.RemoveOtherWorktree(workspaceRefForThread(thread), worktreePath, false)
}

// RemoveOtherWorktree removes one of the project's worktrees, optionally
// forcing through dirty/unpushed safety. Locally owned threads are reset to the
// project root and their idle sessions stop; the next send starts them there.
// Confirmed outgoing moves keep their immutable retired metadata; pending
// transfers block removal.
//
//ao:scope git:operate
func (a *App) RemoveOtherWorktree(ws WorkspaceRef, worktreePath string, force bool) (WorktreeRemoval, error) {
	project, workspace, err := a.gitApplication().ResolveWorkspace(ws)
	if err != nil {
		return WorktreeRemoval{}, err
	}
	reattached, err := a.removeProjectWorktree(ws.ProjectID, project, worktreePath, force)
	if err != nil {
		return WorktreeRemoval{}, err
	}
	state, err := a.resolveProjectWorkspaceStateAfterRemoval(project, workspace, worktreePath)
	if err != nil {
		return WorktreeRemoval{}, err
	}
	return WorktreeRemoval{Workspace: state, Reattached: reattached}, nil
}

func (a *App) removeProjectWorktree(projectID, project, worktreePath string, force bool) ([]store.Thread, error) {
	worktreePath = strings.TrimSpace(worktreePath)
	if worktreePath == "" {
		return nil, fmt.Errorf("worktree path is required")
	}
	if gitops.SameFilesystemPath(project, worktreePath) {
		return nil, fmt.Errorf("refusing to remove project root as worktree")
	}

	core := a.gitCore()
	// Membership is validated on BOTH force paths — force skips the
	// loss-of-work gate below, not the "is this actually one of the
	// project's worktrees" boundary. Without this, a forced removal's
	// only guard against an arbitrary path is git's own refusal.
	if _, ok, err := a.findWorktree(project, worktreePath); err != nil {
		return nil, fmt.Errorf("validate worktree: %w", err)
	} else if !ok {
		return nil, fmt.Errorf("%s is not a worktree of project %s", worktreePath, project)
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
		status, err := a.worktreeApplication().Status(project, worktreePath)
		if err != nil {
			return nil, fmt.Errorf("worktree status: %w", err)
		}
		if status.Dirty || status.UnpushedCommits > 0 {
			return nil, fmt.Errorf("worktree %s has unsaved work (dirty=%v unpushed=%d); pass force to discard", worktreePath, status.Dirty, status.UnpushedCommits)
		}
	}

	// Identify and lock every thread that points at the worktree. Each of
	// them gets reattached to the project root by the best-effort sweep
	// below, so each must be idle and locked against concurrent workspace
	// mutations before we touch git.
	attached, release, err := a.lockWorkspaceThreads(worktreePath)
	if err != nil {
		return nil, err
	}
	defer release()

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
		return nil, err
	}
	slices.Sort(recheck)
	if !slices.Equal(attached, slices.Compact(recheck)) {
		return nil, fmt.Errorf("worktree %s occupancy changed during removal; retry", worktreePath)
	}
	mutable, err := a.mutableWorkspaceThreads(attached)
	if err != nil {
		return nil, err
	}

	if err := a.ensureWorkspaceChangeAllowed("remove this worktree", worktreePath); err != nil {
		return nil, err
	}

	// Cancel by directory and block on the join. A recipe still writing into the
	// path would race git's removal and could recreate entries after removal.
	// A run can belong to a thread that has since moved away, so the attached
	// thread list is not an authoritative set of processes to stop.
	a.cancelWorktreeSetupsForPath(worktreePath)
	// Then clear the durable state of every thread that pointed here — a "Setup
	// failed" pill for a worktree that no longer exists has nothing to offer.
	// Their runs are already gone, so this is the column and nothing else.
	for _, id := range mutable {
		a.cancelThreadWorktreeSetup(id)
	}
	// Hold the workspace's git status off the wire while git deletes it:
	// the reads in between (every file deleted, then no repository) are
	// states of a directory on its way out. On success the hold ends with
	// the watcher, when its panes follow their threads to the root; on
	// failure it ends here and a refresh shows the tree as it stands.
	resumeStatus := a.gitApplication().SuppressStatus(worktreePath)
	if err := core.RemoveWorktreeForce(project, worktreePath, force); err != nil {
		resumeStatus()
		return nil, err
	}
	removal := &worktreeRemoval{projectID: projectID, project: project, path: worktreePath}
	reattached, sweepErr := a.reattachThreadsFromRemovedWorktree(removal, mutable)
	finishErr := a.finishWorktreeRemoval(removal, reattached, true)
	if err := errors.Join(sweepErr, finishErr); err != nil {
		return nil, fmt.Errorf("worktree removed but %w", err)
	}
	return reattached, nil
}

// removeWorkflowWorktree removes a checkout a workflow owns, for its own
// cleanup: disposition, discard, unit retirement and provisioning rollback.
// The workflow has already decided, with its own force semantics, so no idle
// or loss-of-work gate applies. The reaction is the one every removal the app
// performs gets: threads still on the path move to the project root and
// their sessions stop, without a notice, and clients are told once.
//
// The path counts as an app removal from before git runs until that
// reaction is done, so a registry sweep racing it moves the same threads
// without a notice too. The returned error is git's; a failed reaction is
// reported on the threads it concerns.
func (a *App) removeWorkflowWorktree(projectPath, worktreePath string, force bool) error {
	done := a.appWorktreeRemovals.begin(worktreePath)
	defer done()
	resumeStatus := a.gitApplication().SuppressStatus(worktreePath)
	if err := a.gitCore().RemoveWorktreeForce(projectPath, worktreePath, force); err != nil {
		resumeStatus()
		return err
	}
	project, err := a.store.GetProjectByPath(projectPath)
	if err != nil {
		// No project row, so no thread row can point at the checkout.
		log.Printf("worktree %s removed; project %s not found: %v", worktreePath, projectPath, err)
		return nil
	}
	removal := &worktreeRemoval{projectID: project.ID, project: project.Path, path: worktreePath}
	a.reclaimRemovedWorktree(removal, "", true, func(threadIDs []string, problem string) {
		log.Printf("worktree %s removed by a workflow, but %s", worktreePath, problem)
		for _, id := range threadIDs {
			a.emitErrorToThread(id, fmt.Sprintf("worktree %s was removed by a workflow, but %s", worktreePath, problem))
		}
	})
	return nil
}

// appWorktreeRemovals names the worktree paths the app is removing right now,
// so the registry watch does not report them as removed outside the app. An
// entry lives from before git runs until the app's own reaction is done.
type appWorktreeRemovals struct {
	mu     sync.Mutex
	active map[string]int
}

// begin registers path and returns the call that releases it.
func (r *appWorktreeRemovals) begin(path string) func() {
	key := canonicalExistingPrefix(path)
	r.mu.Lock()
	if r.active == nil {
		r.active = make(map[string]int)
	}
	r.active[key]++
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.active[key]--; r.active[key] <= 0 {
				delete(r.active, key)
			}
		})
	}
}

func (r *appWorktreeRemovals) contains(path string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active[canonicalExistingPrefix(path)] > 0
}

// mutableWorkspaceThreads filters a locked occupant set down to the threads
// whose rows this process may rewrite. A pending handoff still needs its
// source state; a confirmed move's local rows are immutable history caches:
// removal may reclaim the old checkout, but must not reattach or resume
// those retired conversations.
func (a *App) mutableWorkspaceThreads(attached []string) ([]string, error) {
	mutable := make([]string, 0, len(attached))
	for _, id := range attached {
		retired, err := a.threadApplication().CheckCleanup(id)
		if err != nil {
			return nil, err
		}
		if !retired {
			mutable = append(mutable, id)
		}
	}
	return mutable, nil
}

// worktreeRemoval names one removed worktree for the reactions every removal
// shares, and remembers the project root's branch once it is read.
type worktreeRemoval struct {
	projectID string
	project   string
	path      string
	// cause is empty for a removal the app performed on request. Otherwise
	// it says who removed the worktree ("was removed outside Agent
	// Overflow"), and every thread the removal moves is told so.
	cause string

	branch     string
	branchRead bool
}

// projectBranch reads the project root's current branch on first use. A
// removal that moves no thread and tells no client never spawns git for it.
func (r *worktreeRemoval) projectBranch(core *gitops.Core) string {
	if !r.branchRead {
		r.branch = core.CurrentBranch(r.project)
		r.branchRead = true
	}
	return r.branch
}

// notice is the timeline message a thread moved by an outside removal gets.
func (r *worktreeRemoval) notice(stopped bool) string {
	if stopped {
		return fmt.Sprintf("Worktree %s %s. Its session was stopped, and this thread now runs in Base (%s); the next message starts it there.", r.path, r.cause, r.project)
	}
	return fmt.Sprintf("Worktree %s %s. This thread now runs in Base (%s).", r.path, r.cause, r.project)
}

// finishWorktreeRemoval is what every removal ends with, whoever removed the
// checkout: its cached file list goes, every terminal opened in it or below
// it closes, and every client is told which worktree went and which threads
// moved. announce false skips the event for a sweep that neither moved a
// thread nor saw the worktree go (a row found on a path that was already
// gone). Returns the terminals that could not be closed.
func (a *App) finishWorktreeRemoval(removal *worktreeRemoval, reattached []store.Thread, announce bool) error {
	if a.workspaceFiles != nil {
		// The path no longer exists; drop any cached file list before
		// another thread's @-mention picker reaches for it.
		a.workspaceFiles.Invalidate(removal.path)
	}
	var closeErr error
	if a.terminals != nil {
		// Each close reaches the exit callback, which drops the tab on every
		// client and ends a terminal thread whose last shell this was.
		if _, err := a.terminals.CloseMatching(func(summary terminal.SessionSummary) bool {
			return pathWithin(summary.Cwd, removal.path)
		}); err != nil {
			closeErr = fmt.Errorf("terminals opened in it could not all be closed: %w", err)
		}
	}
	if !announce && len(reattached) == 0 {
		return closeErr
	}
	ids := make([]string, 0, len(reattached))
	for _, thread := range reattached {
		ids = append(ids, thread.ID)
	}
	a.emit(eventchan.WorktreeRemoved, WorktreeRemovedEvent{
		ProjectID: removal.projectID,
		Path:      removal.path,
		Branch:    removal.projectBranch(a.gitCore()),
		ThreadIDs: ids,
	})
	return closeErr
}

// worktreeTerminalCount counts the open terminals removing path would close.
func (a *App) worktreeTerminalCount(path string) int {
	if a.terminals == nil {
		return 0
	}
	return len(a.terminals.Matching(func(summary terminal.SessionSummary) bool {
		return pathWithin(summary.Cwd, path)
	}))
}

// pathWithin reports whether path is root or lies below it. Both sides are
// resolved through their longest existing ancestor, so a directory that was
// just deleted still compares equal to the spelling it had before.
func pathWithin(path, root string) bool {
	if strings.TrimSpace(path) == "" || strings.TrimSpace(root) == "" {
		return false
	}
	rel, err := filepath.Rel(canonicalExistingPrefix(root), canonicalExistingPrefix(path))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

// canonicalExistingPrefix resolves symlinks in the longest ancestor of path
// that exists and appends the rest unchanged.
func canonicalExistingPrefix(path string) string {
	path = filepath.Clean(path)
	rest := ""
	for current := path; ; {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return path
		}
		rest = filepath.Join(filepath.Base(current), rest)
		current = parent
	}
}

// stopSessionForRemovedWorkspace ends the live session of a thread whose
// working directory is gone. Neither CLI exits when its cwd is deleted: Codex
// fails every later command, and Claude's shell falls back to the home
// directory and keeps running commands there. So the session is stopped,
// never restarted; the next send starts it from the row's new workspace. A
// running turn ends interrupted, as it does when any session is stopped, and
// the thread's background tasks show as died. Messages queued behind the
// turn go back to the composer, as they do when a session dies, rather than
// being dropped by a stop nobody asked for. Caller holds the thread action
// lock. Reports whether a session was stopped.
func (a *App) stopSessionForRemovedWorkspace(threadID string) (bool, error) {
	if startState, ok := a.sessionManager().startState(threadID); ok {
		<-startState.Done
	}
	if !a.hasActiveSession(threadID) {
		return false, nil
	}
	requeued := a.restoreUnconfirmedQueueLocked(threadID)
	err := a.stopSession(threadID)
	if len(requeued) > 0 {
		// The stop wiped the queue these re-entered; register them again so
		// the next start's flush still finds them.
		a.requeueUnconfirmedFlushItems(threadID, requeued)
		a.emitQueueStateChanged(threadID)
	}
	return true, err
}

// reattachThreadsFromRemovedWorktree moves every listed thread that still
// points at a worktree which no longer exists back to the project root and
// returns the rows it moved. The caller holds each thread's action lock and
// has already removed the checkout (or learned that someone else did).
//
// A moved thread's live session is stopped first (see
// stopSessionForRemovedWorkspace); with no process left writing it, the
// Claude transcript then moves to the root's slug so the next start resumes
// it. When removal.cause is set, each moved thread gets a timeline notice
// saying what happened, at its own position in the thread.
//
// Best-effort sweep: the worktree is already gone, so per-thread refresh
// failures must NOT bail mid-loop and leave siblings pointing at a deleted
// path. Errors accumulate and surface together at the end; any successfully
// mutated thread is broadcast immediately so its UI catches up regardless
// of what happens to its neighbours.
//
// UpdatedAt is deliberately left untouched. The sweep is a system-driven
// reattach, not a user-driven activity event; bumping the timestamp would
// jump every reattached thread to the top of the sidebar (which sorts by
// updated_at DESC), erasing the order the user had built up. The frontend's
// syncThreadRow does a max-merge on updatedAt, so the unchanged timestamp in
// the broadcast event is invariant-safe.
func (a *App) reattachThreadsFromRemovedWorktree(removal *worktreeRemoval, mutable []string) ([]store.Thread, error) {
	project, worktreePath := removal.project, removal.path
	core := a.gitCore()
	reattached := make([]store.Thread, 0, len(mutable))
	var sweepErrs []error
	for _, id := range mutable {
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
			t.Branch = removal.projectBranch(core)
			mutated = true
		}
		if !mutated {
			continue
		}
		stopped, err := a.stopSessionForRemovedWorkspace(id)
		if err != nil {
			sweepErrs = append(sweepErrs, fmt.Errorf("thread %s session stop failed: %w", id, err))
		}
		if err := a.store.UpdateThread(t); err != nil {
			sweepErrs = append(sweepErrs, fmt.Errorf("thread %s update failed: %w", id, err))
			continue
		}
		if !gitops.SameFilesystemPath(previousWorkspace, t.WorkspacePath) && !a.hasActiveSession(id) {
			// Claude resolves --resume against the slug of the current cwd, so
			// reattaching to the project root strands the transcript under the
			// deleted worktree's slug — the next resume would fail with "No
			// conversation found". Move it so the conversation survives; we never
			// clear the session ref or silently start fresh. The reattach is
			// already committed above and the worktree is gone, so unlike
			// switch/create/attach this can't be aborted: a hard failure is
			// surfaced and resume is left to fail loudly.
			//
			// Only when no process is alive: a live CLI appends under the old
			// slug until it exits. The stop above ended it, and a session that
			// is somehow still registered leaves the move to the settle every
			// start runs (settleClaudeTranscriptForWorkspace).
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
		if removal.cause != "" {
			if err := a.emitNoticeToThread(id, removal.notice(stopped)); err != nil {
				sweepErrs = append(sweepErrs, err)
			}
		}
		reattached = append(reattached, t)
	}
	if len(sweepErrs) > 0 {
		return reattached, fmt.Errorf("%d threads need attention: %w", len(sweepErrs), errors.Join(sweepErrs...))
	}
	return reattached, nil
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

// settleClaudeTranscriptForWorkspace moves a Claude thread's own transcript
// under its current workspace slug when it is filed elsewhere. It runs at
// session start, after the prior process is stopped, because that is the
// one moment nothing appends to the file. The CLI relocates the transcript
// itself when it changes directory (EnterWorktree / ExitWorktree, verified
// 2.1.257), so for a row that followed the move this is a no-op; it exists
// for the row and the file disagreeing, such as a move the app refused to
// record, so that `--resume` from the row's workspace still resolves.
//
// Only SessionRef is settled. A PendingForkRef names the SOURCE thread's
// transcript, which must stay where its owner resumes it.
//
// Failures are surfaced on the thread and never block the start: the CLI
// still gets its chance to resolve the transcript, and if it cannot, its
// own "No conversation found" reaches the user through the normal path.
func (a *App) settleClaudeTranscriptForWorkspace(t store.Thread) {
	if t.Provider != string(provider.Claude) && t.Provider != string(provider.ClaudeTUI) {
		return
	}
	ref := strings.TrimSpace(t.SessionRef)
	workspace := strings.TrimSpace(t.WorkspacePath)
	if ref == "" || workspace == "" {
		return
	}
	projectsDir, err := a.claudeProjectsDir()
	if err != nil {
		a.emitErrorToThread(t.ID, fmt.Sprintf("workspace change: transcript could not be located: %v", err))
		return
	}
	// fromWorkspace == destWorkspace: the primary lookup answers when the
	// file is already in place; the miss falls through to the project-dir
	// scan that finds it wherever the previous cwd filed it.
	src, dest, err := sessionfork.RelocateSession(projectsDir, ref, workspace, workspace)
	switch {
	case err == nil:
		if src != dest {
			a.purgeRelocatedClaudeSessions(t.ID, []string{src})
		}
	case errors.Is(err, sessionfork.ErrSessionFileNotFound):
		// Nothing on disk to settle; resume surfaces its own verdict.
	case errors.Is(err, sessionfork.ErrSubagentCopyIncomplete):
		log.Printf("thread %s: claude session %s settled under %s but subagent history incomplete: %v", t.ID, ref, workspace, err)
	default:
		a.emitErrorToThread(t.ID, fmt.Sprintf("workspace change: transcript for session %s could not be moved under %s: %v", ref, workspace, err))
	}
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

// GitWorktreeStatus classifies any worktree of the referenced workspace's
// project for the cleanup UI.
//
//ao:scope git:operate
func (a *App) GitWorktreeStatus(ws WorkspaceRef, worktreePath string) (WorktreeStatus, error) {
	project, _, err := a.gitApplication().ResolveWorkspace(ws)
	if err != nil {
		return WorktreeStatus{}, err
	}
	return a.computeWorktreeStatus(project, worktreePath)
}

// computeWorktreeStatus is the engine behind GitWorktreeStatus.
func (a *App) computeWorktreeStatus(project, worktreePath string) (WorktreeStatus, error) {
	status, err := a.worktreeApplication().Status(project, worktreePath)
	if err != nil {
		return WorktreeStatus{}, err
	}
	return WorktreeStatus{
		Path:             status.Path,
		Branch:           status.Branch,
		Dirty:            status.Dirty,
		UncommittedCount: status.UncommittedCount,
		UnpushedCommits:  status.UnpushedCommits,
		HasUpstream:      status.HasUpstream,
		AttachedThreads:  status.AttachedThreads,
		Terminals:        a.worktreeTerminalCount(worktreePath),
	}, nil
}

// GitListWorktrees lists the worktrees of the referenced workspace's
// repository.
//
//ao:scope git:operate
func (a *App) GitListWorktrees(ws WorkspaceRef) ([]WorktreeListItem, error) {
	project, _, err := a.gitApplication().ResolveWorkspace(ws)
	if err != nil {
		return nil, err
	}
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
	if err := a.ensureThreadChangeAllowed(threadID); err != nil {
		return store.Thread{}, err
	}

	core := a.gitCore()
	move := checkoutMoveFrom(thread, "switch workspace")
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
	// Switching into an existing workspace has nothing to roll back: the
	// target was provisioned (or not) by whoever cut it, and a refused
	// relocation leaves the thread where it already was.
	return a.commitThreadCheckout(thread, move)
}

// threadCheckoutMove is one thread's move to a different checkout: where its
// workspace / worktree / branch triple stood before the caller wrote the new
// one onto the row, plus what to do if the move cannot be committed.
type threadCheckoutMove struct {
	previousWorkspace string
	previousWorktree  string
	previousBranch    string
	// label prefixes the errors this move can produce ("create worktree").
	label string
	// rollback tears down a checkout THIS call created, when the transcript
	// relocation or the store write fails. Nil for a move between checkouts
	// that both already existed.
	rollback func()
}

// checkoutMoveFrom snapshots the triple a move is leaving, before the caller
// overwrites it.
func checkoutMoveFrom(thread store.Thread, label string) threadCheckoutMove {
	return threadCheckoutMove{
		previousWorkspace: thread.WorkspacePath,
		previousWorktree:  thread.WorktreePath,
		previousBranch:    thread.Branch,
		label:             label,
	}
}

// commitThreadCheckout persists a thread whose workspace / worktree / branch
// the caller has just rewritten, and BROADCASTS the row it moved.
//
// It is the chokepoint for the three USER-driven paths that move a thread's
// checkout (cutting a new worktree, attaching an existing branch's, and
// switching to a workspace that already exists) so no caller can move one
// silently: the RPC's return value only ever reaches the client that issued
// it. It also keeps the ORDER those paths share in one place: relocate the
// Claude transcript when no process writes it, commit, purge the stale
// copies, release a setup run for the workspace the thread has left, then
// restart the session. A live session's transcript is not touched here: the
// CLI appends under the old slug until it exits (last-prompt and cost-state
// records at the very end), so the restart moves it, through the settle
// every start runs after the old process is fully stopped
// (settleClaudeTranscriptForWorkspace). Nothing moves a file a process is
// still writing; the reattach sweep follows the same rule.
//
// The PROVIDER-driven move (EnterWorktree / ExitWorktree, see
// app_worktree_follow.go) bypasses this on purpose: the process already
// runs at the new directory and is still writing its transcript under the
// old slug, so neither the restart nor the relocation applies there.
func (a *App) commitThreadCheckout(thread store.Thread, move threadCheckoutMove) (store.Thread, error) {
	threadID := thread.ID
	rollback := func() {
		if move.rollback != nil {
			move.rollback()
		}
	}
	var purge []string
	if !gitops.SameFilesystemPath(move.previousWorkspace, thread.WorkspacePath) && !a.hasActiveSession(threadID) {
		// Carry the Claude transcript to the target slug BEFORE committing. A
		// relocation that cannot preserve the conversation refuses the whole
		// move, leaving the thread resumable from the workspace it is in
		// rather than silently starting fresh. With a live session the
		// restart's settle carries it instead, and its failure is surfaced on
		// the thread with the file left intact under the old slug.
		moved, err := a.copyClaudeSessionForWorkspaceChange(thread, move.previousWorkspace)
		if err != nil {
			rollback()
			return store.Thread{}, fmt.Errorf("%s: %w", move.label, err)
		}
		purge = moved
	}
	if err := a.store.UpdateThread(thread); err != nil {
		rollback()
		return store.Thread{}, err
	}
	// Commit succeeded — drop the stale pre-move copies (best-effort).
	a.purgeRelocatedClaudeSessions(threadID, purge)
	// The thread just left whatever workspace it was in; a setup run still
	// going for that one describes a worktree it no longer occupies. Moving
	// back into the same path is a no-op the helper recognises by comparing
	// paths.
	a.releaseThreadWorktreeSetup(threadID, thread.WorkspacePath)
	refreshed, err := a.restartSessionIfAffected(threadID, "workspace")
	if err != nil {
		return store.Thread{}, fmt.Errorf("%s: refresh thread after workspace switch: %w", move.label, err)
	}
	// Broadcast so a second attached client's pane follows the thread to its
	// new checkout. `store.UpdateThread` rewrites the whole row, so the
	// no-change test is the three fields a move owns — re-selecting the
	// workspace the thread already sits in moves nothing and says nothing.
	a.broadcastThreadRowIfChanged(triage.ThreadActionFull, refreshed,
		move.previousWorkspace != thread.WorkspacePath ||
			move.previousWorktree != thread.WorktreePath ||
			move.previousBranch != thread.Branch)
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
	return gitops.UniqueWorktreePath(filepath.Join(a.worktreesBaseDir(projectPath), gitops.SanitizeWorktreePathSegment(branch)))
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
