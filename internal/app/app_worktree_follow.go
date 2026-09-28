package app

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// The provider can move its own working directory mid-session: Claude's
// EnterWorktree creates or enters a checkout under `<repo>/.claude/worktrees/`
// and chdirs the process into it; ExitWorktree returns to the launch
// directory and optionally deletes the worktree. The thread row is the app's
// only record of where a thread works (git status, diffs, the workspace strip
// and the next resume all read it), so it follows the process.
//
// This is the fourth way a thread's checkout moves, beside cutting, attaching
// and switching (commitThreadCheckout). It differs from all three on purpose:
//
//   - It is not gated on an idle thread. The move already happened, mid-turn
//     by definition, and refusing to record it is the defect.
//   - The session is not restarted. The process already lives at the new
//     directory; restarting would cut the turn that moved it.
//   - The transcript is not relocated by the app. The CLI moves it between
//     project slugs itself as it changes directory (verified 2.1.257: the
//     file follows the cwd, with `relocated` rows marking each move) and
//     finds it again on `--resume` from either directory.
//     settleClaudeTranscriptForWorkspace at the next session start is the
//     safety net for a row and a transcript that disagree.

// followProviderWorkspaceChange is the session event handler's entry point.
// It runs off the thread's event worker: the thread action lock is taken on
// a goroutine because lock holders (a switch waiting on a session restart)
// can themselves be waiting on this very worker, whose queue a stop drains.
func (a *App) followProviderWorkspaceChange(threadID, sessionToken string, evt provider.ProviderEvent) {
	var change provider.WorkspaceChangeMeta
	if err := json.Unmarshal(evt.Meta, &change); err != nil || strings.TrimSpace(change.Cwd) == "" {
		a.settlePendingWorktreeExit(threadID, sessionToken, false)
		a.emitWireErrorToThread(threadID, "Claude changed its working directory, but AO could not read the new path from the event.")
		return
	}
	// Recorded here, on the event worker, before the move is applied: the
	// registry watch may already be sweeping the removed worktree and must
	// leave this thread's session alone (reclaimRemovedWorktree). The
	// pending call is settled only after, so a sweep always sees one of the
	// two.
	removed := strings.TrimSpace(change.WorktreePath)
	followed := change.RemovedWorktree && removed != ""
	if followed {
		a.providerWorktreeExits.Store(threadID, removed)
	}
	a.settlePendingWorktreeExit(threadID, sessionToken, followed)
	go func() {
		if followed {
			defer a.providerWorktreeExits.CompareAndDelete(threadID, removed)
		}
		a.applyProviderWorkspaceChange(threadID, sessionToken, change)
	}()
}

// providerExitingWorktree reports whether threadID's own CLI reported
// removing worktreePath and the app has not finished following that move.
func (a *App) providerExitingWorktree(threadID, worktreePath string) bool {
	value, ok := a.providerWorktreeExits.Load(threadID)
	if !ok {
		return false
	}
	path, _ := value.(string)
	return gitops.SameFilesystemPath(path, worktreePath)
}

func (a *App) applyProviderWorkspaceChange(threadID, sessionToken string, change provider.WorkspaceChangeMeta) {
	unlock, err := a.threadLocks().LockCtx(a.lifeCtx(), threadID)
	if err != nil {
		log.Printf("thread %s: workspace change from %s abandoned: %v", threadID, change.Tool, err)
		return
	}
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()

	// A replacement session launched from the row's workspace is the
	// authority now; a late result from the session it replaced describes
	// a process that no longer exists. A session that has since died still
	// counts: its move is where the transcript's next resume lands.
	if current, ok := a.sessionManager().get(threadID); ok && current.Token != sessionToken {
		log.Printf("thread %s: workspace change from %s ignored: session replaced", threadID, change.Tool)
		return
	}
	if err := a.threadApplication().CheckMutable(threadID); err != nil {
		a.emitWireErrorToThread(threadID, fmt.Sprintf("Claude moved to %s, but the thread cannot follow: %v", change.Cwd, err))
		return
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		log.Printf("thread %s: workspace change from %s abandoned: %v", threadID, change.Tool, err)
		return
	}
	project, _, err := a.resolveGitPaths(thread)
	if err != nil {
		a.emitWireErrorToThread(threadID, fmt.Sprintf("Claude moved to %s, but the thread cannot follow: %v", change.Cwd, err))
		return
	}

	target, err := a.resolveProviderWorkspaceTarget(project, change.Cwd)
	if err != nil {
		a.emitWireErrorToThread(threadID, fmt.Sprintf("Claude is now working in %s, but AO cannot show it there: %v. The thread still shows %s.", change.Cwd, err, thread.WorkspacePath))
		return
	}

	moved := !gitops.SameFilesystemPath(thread.WorkspacePath, target.WorkspacePath) ||
		thread.WorktreePath != target.WorktreePath ||
		thread.Branch != target.Branch
	thread.WorkspacePath = target.WorkspacePath
	thread.WorktreePath = target.WorktreePath
	thread.Branch = target.Branch
	if moved {
		if err := a.store.UpdateThread(thread); err != nil {
			a.emitWireErrorToThread(threadID, fmt.Sprintf("Claude moved to %s, but the thread could not be updated: %v", change.Cwd, err))
			return
		}
		// A setup run still going for the workspace the thread left
		// describes a checkout it no longer occupies.
		a.releaseThreadWorktreeSetup(threadID, thread.WorkspacePath)
		a.broadcastThreadRow(triage.ThreadActionFull, thread)
	}

	if change.RemovedWorktree {
		// Released first: the sweep locks the other occupants in id order,
		// and holding this thread's lock across it would invert that order
		// against a registry sweep that is locking the same set.
		locked = false
		unlock()
		a.reclaimProviderRemovedWorktree(thread.ProjectID, project, change.WorktreePath, threadID)
	}
}

// resolveProviderWorkspaceTarget maps the directory the provider reports onto
// the workspace vocabulary the thread row holds: the project root, or one of
// the repository's registered worktrees. Anything else (a worktree of a
// nested repository, a hook-provisioned directory outside git) has no
// representation on the row and is refused with the reason.
func (a *App) resolveProviderWorkspaceTarget(project, cwd string) (GitWorkspaceState, error) {
	core := a.gitCore()
	if gitops.SameFilesystemPath(cwd, project) {
		return GitWorkspaceState{WorkspacePath: project, Branch: core.CurrentBranch(project)}, nil
	}
	worktree, ok, err := a.findWorktree(project, cwd)
	if err != nil {
		return GitWorkspaceState{}, fmt.Errorf("list worktrees of %s: %w", project, err)
	}
	if !ok {
		return GitWorkspaceState{}, fmt.Errorf("%s is not the project root or a worktree of %s", cwd, project)
	}
	branch := strings.TrimSpace(worktree.Branch)
	if branch == "" {
		branch = core.CurrentBranch(worktree.Path)
	}
	return GitWorkspaceState{WorkspacePath: worktree.Path, WorktreePath: worktree.Path, Branch: branch}, nil
}

// reclaimProviderRemovedWorktree is the app's half of an `ExitWorktree
// remove`: the CLI deleted the directory and branch, so every OTHER thread
// still attached there is reattached to the project root the way an outside
// removal reattaches it: its session stops and it is told why. The exiting
// thread itself already followed the move and keeps its running session
// (the CLI relocated itself), so it is excluded from the occupant set. A
// failure is reported on the exiting thread, whose tool
// result this reaction belongs to.
func (a *App) reclaimProviderRemovedWorktree(projectID, project, worktreePath, exitingThreadID string) {
	removal := &worktreeRemoval{projectID: projectID, project: project, path: worktreePath, cause: removedByClaudeCause}
	a.reclaimRemovedWorktree(removal, exitingThreadID, true, func(_ []string, problem string) {
		a.emitWireErrorToThread(exitingThreadID, fmt.Sprintf("Claude removed worktree %s, but %s", worktreePath, problem))
	})
}

// pendingWorktreeExits holds, per thread, the top-level `ExitWorktree`
// call its Claude session started and has not answered. The CLI deletes the
// worktree before it writes the result, so the registry watch can see the
// removal while only the tool_use has been read; that session is the one
// removing it and must not be stopped as an outside removal's victim.
//
// A sweep that leaves a thread to its pending call marks the entry
// deferred. If the call settles without the app following a removal (it was
// refused, kept the worktree, or the turn or session ended first), the
// thread's project is swept again so the row is not left on a vanished
// path. One entry per thread, removed when the call, turn or session ends.
type pendingWorktreeExits struct {
	mu       sync.Mutex
	byThread map[string]*pendingWorktreeExit
}

type pendingWorktreeExit struct {
	token     string
	toolUseID string
	deferred  bool
}

func (p *pendingWorktreeExits) start(threadID, token, toolUseID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.byThread == nil {
		p.byThread = make(map[string]*pendingWorktreeExit)
	}
	p.byThread[threadID] = &pendingWorktreeExit{token: token, toolUseID: toolUseID}
}

// toolUseID returns the pending call of the session identified by token.
func (p *pendingWorktreeExits) toolUseID(threadID, token string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.byThread[threadID]
	if !ok || entry.token != token {
		return "", false
	}
	return entry.toolUseID, true
}

// claim reports whether the session identified by token has a pending call
// and, if so, records that a sweep left the thread to it.
func (p *pendingWorktreeExits) claim(threadID, token string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.byThread[threadID]
	if !ok || entry.token != token {
		return false
	}
	entry.deferred = true
	return true
}

// settle removes the session's pending call and reports whether a sweep
// deferred to it.
func (p *pendingWorktreeExits) settle(threadID, token string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.byThread[threadID]
	if !ok || entry.token != token {
		return false
	}
	delete(p.byThread, threadID)
	return entry.deferred
}

// observeClaudeWorktreeExit tracks a Claude session's pending `ExitWorktree`
// call on the thread's event worker, in wire order. The call is settled by
// the workspace change that follows its result (followProviderWorkspaceChange),
// by a refused or unreadable result, by the end of the top-level turn, or by
// the session closing.
func (a *App) observeClaudeWorktreeExit(threadID, sessionToken string, evt provider.ProviderEvent) {
	if claude.WorktreeRemovalStarted(evt) {
		a.pendingWorktreeExits.start(threadID, sessionToken, evt.ItemID)
		return
	}
	toolUseID, ok := a.pendingWorktreeExits.toolUseID(threadID, sessionToken)
	if !ok {
		return
	}
	if claude.ToolCallRefused(evt, toolUseID) ||
		(evt.Kind == provider.EventError && evt.ItemID == toolUseID) ||
		(evt.Kind == provider.EventTurnComplete && strings.TrimSpace(evt.ParentToolUseID) == "") {
		a.settlePendingWorktreeExit(threadID, sessionToken, false)
	}
}

// settlePendingWorktreeExit ends the session's pending `ExitWorktree` call.
// followed is true when the app is following the removal the call reported,
// which sweeps the worktree itself; otherwise a sweep that deferred to the
// call runs again for the thread's project.
func (a *App) settlePendingWorktreeExit(threadID, sessionToken string, followed bool) {
	if !a.pendingWorktreeExits.settle(threadID, sessionToken) || followed {
		return
	}
	go a.resweepThreadProject(threadID)
}

// resweepThreadProject reconciles the worktrees of threadID's project
// against the registry, reattaching the thread if its worktree is gone.
func (a *App) resweepThreadProject(threadID string) {
	thread, err := a.store.GetThread(threadID)
	if err == nil {
		var project store.Project
		project, err = a.store.GetProject(thread.ProjectID)
		if err == nil {
			a.reconcileProjectWorktrees(project.Path, nil)
			return
		}
	}
	a.emitErrorToThread(threadID, fmt.Sprintf("AO could not recheck this thread's worktree after Claude's ExitWorktree call: %v", err))
}
