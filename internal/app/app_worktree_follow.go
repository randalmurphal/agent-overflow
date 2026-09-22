package app

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/provider"
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
// It runs off the provider read loop: the thread action lock is taken on a
// goroutine because lock holders (a switch waiting on a session restart)
// can themselves be waiting on this very read loop.
func (a *App) followProviderWorkspaceChange(threadID, sessionToken string, evt provider.ProviderEvent) {
	var change provider.WorkspaceChangeMeta
	if err := json.Unmarshal(evt.Meta, &change); err != nil || strings.TrimSpace(change.Cwd) == "" {
		a.emitWireErrorToThread(threadID, "Claude changed its working directory, but AO could not read the new path from the event.")
		return
	}
	go a.applyProviderWorkspaceChange(threadID, sessionToken, change)
}

func (a *App) applyProviderWorkspaceChange(threadID, sessionToken string, change provider.WorkspaceChangeMeta) {
	unlock, err := a.threadLocks().LockCtx(a.lifeCtx(), threadID)
	if err != nil {
		log.Printf("thread %s: workspace change from %s abandoned: %v", threadID, change.Tool, err)
		return
	}
	defer unlock()

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
		a.reclaimProviderRemovedWorktree(project, change.WorktreePath, threadID)
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
// still attached there is reattached to the project root exactly as
// RemoveOtherWorktree would have done. The exiting thread itself already
// followed the move and holds its own lock, so it is excluded from the
// occupant set rather than locked twice.
func (a *App) reclaimProviderRemovedWorktree(project, worktreePath, exitingThreadID string) {
	worktreePath = strings.TrimSpace(worktreePath)
	if worktreePath == "" {
		return
	}
	a.cancelWorktreeSetupsForPath(worktreePath)
	occupants, err := a.threadsReferencingWorkspace(worktreePath)
	if err != nil {
		a.emitWireErrorToThread(exitingThreadID, fmt.Sprintf("Claude removed worktree %s, but the threads attached to it could not be listed: %v", worktreePath, err))
		return
	}
	others := make([]string, 0, len(occupants))
	for _, id := range occupants {
		if id != exitingThreadID {
			others = append(others, id)
		}
	}
	if len(others) == 0 {
		if a.workspaceFiles != nil {
			a.workspaceFiles.Invalidate(worktreePath)
		}
		return
	}
	unlocks := make([]func(), 0, len(others))
	for _, id := range others {
		unlocks = append(unlocks, a.threadLocks().Lock(id))
	}
	defer func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}()
	mutable, err := a.mutableWorkspaceThreads(others)
	if err != nil {
		a.emitWireErrorToThread(exitingThreadID, fmt.Sprintf("Claude removed worktree %s, but the threads attached to it could not be checked: %v", worktreePath, err))
		return
	}
	for _, id := range mutable {
		a.cancelThreadWorktreeSetup(id)
	}
	if err := a.reattachThreadsFromRemovedWorktree(project, worktreePath, mutable); err != nil {
		a.emitWireErrorToThread(exitingThreadID, fmt.Sprintf("Claude removed worktree %s, but %v", worktreePath, err))
	}
}
