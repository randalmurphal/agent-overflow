package app

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/gitroot"
	"agent-overflow/internal/worktreewatch"
)

// A worktree can be removed by anything: `git worktree remove` in a
// terminal, `rm -rf`, another tool. The thread row is the app's only record
// of where a thread works, and a row pointing at a directory that no longer
// exists breaks everything downstream: git status, the workspace strip, the
// next start. A live session does not notice either: neither CLI exits when
// its cwd is deleted (Codex fails every later command; Claude's shell falls
// back to the home directory and keeps going there). The two removal paths
// the app already reacts to, its own RemoveOtherWorktree and the CLI's
// `ExitWorktree remove`, reattach every thread of the dead checkout to the
// project root. This file gives the external removal the same reaction, from
// a watch on git's own worktree registry, and tells each moved thread what
// happened.

// startWorktreeWatch builds the registry watcher. It watches nothing until
// armWorktreeWatch runs: a sweep moves rows and stops sessions, which is
// work that acts on its own and so waits for the activation gate like every
// other unattended worker. Built early because the project chokepoints read
// the field without a lock.
func (a *App) startWorktreeWatch() {
	a.worktreeWatch = worktreewatch.NewManager(worktreewatch.Config{
		OnChange: a.reconcileProjectWorktrees,
		ExtraDir: a.worktreesBaseDir,
	})
}

// armWorktreeWatch lets the chokepoints hand projects to the watcher and
// reads every registry once, after the first client's catalog reads like
// every other unattended scan. A removal made while the app was not
// running has no event to replay; this first read is where it is seen.
func (a *App) armWorktreeWatch() {
	if a.worktreeWatch == nil {
		return
	}
	a.worktreeWatchArmed.Store(true)
	go func() {
		if err := a.awaitFirstReadsSettled(a.lifeCtx()); err != nil {
			return
		}
		a.syncWorktreeWatch()
	}()
}

// syncWorktreeWatch makes the watched set equal to the project rows,
// archived ones included: an archived project's threads keep their rows
// and are reattached like any other. Every project-row chokepoint calls it,
// so a project created or deleted by any binding is followed without that
// binding knowing the watcher exists.
func (a *App) syncWorktreeWatch() {
	if a.worktreeWatch == nil || a.store == nil || !a.worktreeWatchArmed.Load() {
		return
	}
	projects, err := a.store.ListAllProjects()
	if err != nil {
		log.Printf("worktree watch: list projects: %v", err)
		return
	}
	paths := make([]string, 0, len(projects))
	for _, project := range projects {
		paths = append(paths, project.Path)
	}
	a.worktreeWatch.SetProjects(paths)
}

func (a *App) closeWorktreeWatch() {
	if a.worktreeWatch != nil {
		a.worktreeWatch.Close()
	}
}

// reconcileProjectWorktrees reattaches every thread of the project whose
// checkout is no longer one of the repository's registered, present
// worktrees, and announces every worktree the watcher saw vanish (removed)
// whether or not a thread row points at it: draft placeholders live only
// in clients. It is the watcher's callback and runs on the watcher's
// goroutine, serialized per project; it is also safe to call directly.
//
// "Gone" is decided against git's registry read from disk and a stat of
// each registration: a checkout deleted with `rm -rf` is still registered
// and no longer present, one removed with `git worktree remove` is neither.
// Both are the same thing to the thread: a directory it cannot run in.
//
// A retired row (a conversation moved to another computer, a holder) keeps
// the path it had and is never reattached, so it does not make a path gone:
// it would otherwise run the sweep again on every registry change.
func (a *App) reconcileProjectWorktrees(projectPath string, removed []string) {
	if a.shuttingDown.Load() {
		return
	}
	project, err := a.store.GetProjectByPath(projectPath)
	if err != nil {
		// Deleted between the registry event and this callback; the
		// deletion reattached or removed its threads itself.
		log.Printf("worktree watch: project %s: %v", projectPath, err)
		return
	}
	if _, ok := gitroot.RegistryDir(project.Path); !ok {
		// Not a checkout right now (the repository itself is gone or not
		// initialised). There is no root to reattach anything to, and
		// reading "no worktrees" out of that would move every thread of the
		// project onto a path that does not exist either.
		return
	}
	registered, err := gitroot.RegisteredWorktrees(project.Path)
	if err != nil {
		log.Printf("worktree watch: project %s: %v", project.Path, err)
		return
	}
	live := make(map[string]struct{}, len(registered))
	for _, path := range registered {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			live[gitops.CanonicalPath(path)] = struct{}{}
		}
	}
	refs, err := a.store.ListThreadWorkspaceRefsByProject(project.ID)
	if err != nil {
		log.Printf("worktree watch: project %s: %v", project.Path, err)
		return
	}
	// gone maps each vanished directory's canonical path to the spelling the
	// sweep and its announcement use. A reported removal keeps git's
	// spelling, the one the worktree list gives clients; a row's own
	// spelling, which can differ through a symlink, matches it canonically.
	gone := make(map[string]string)
	announced := make(map[string]struct{}, len(removed))
	for _, path := range removed {
		canonical := gitops.CanonicalPath(path)
		if _, ok := live[canonical]; ok {
			// Back on disk by the time this read ran.
			continue
		}
		announced[canonical] = struct{}{}
		gone[canonical] = path
	}
	for _, ref := range refs {
		for _, path := range []string{ref.WorktreePath, ref.WorkspacePath} {
			path = strings.TrimSpace(path)
			if path == "" || gitops.SameFilesystemPath(path, project.Path) {
				continue
			}
			canonical := gitops.CanonicalPath(path)
			if _, ok := live[canonical]; ok {
				continue
			}
			if _, ok := gone[canonical]; ok {
				continue
			}
			retired, err := a.threadApplication().CheckCleanup(ref.ID)
			if err == nil && retired {
				continue
			}
			// An unreadable ownership answer counts the path: the sweep
			// rechecks under the thread lock and reports what it cannot.
			gone[canonical] = path
		}
	}
	canonicals := make([]string, 0, len(gone))
	for canonical := range gone {
		canonicals = append(canonicals, canonical)
	}
	slices.Sort(canonicals)
	for _, canonical := range canonicals {
		path := gone[canonical]
		_, announce := announced[canonical]
		removal := &worktreeRemoval{projectID: project.ID, project: project.Path, path: path, cause: removedOutsideCause}
		if a.appWorktreeRemovals.contains(path) {
			// The app is removing it (removeWorkflowWorktree) and reacts
			// the same way; this sweep only got there first.
			removal.cause = ""
		}
		a.reclaimRemovedWorktree(removal, "", announce, func(threadIDs []string, problem string) {
			for _, id := range threadIDs {
				a.emitErrorToThread(id, fmt.Sprintf("worktree %s no longer exists, but %s", path, problem))
			}
		})
	}
}

// reclaimRemovedWorktree reattaches every thread still on a worktree that no
// longer exists to the project root, the shared half of every removal the
// app did not perform itself: the CLI's `ExitWorktree remove` (which passes
// the exiting thread, already moved and holding its own lock) and an external
// removal (which passes none). The occupants are locked in id order, the
// order lockWorkspaceThreads uses, so this cannot deadlock against a removal
// the app is performing at the same time; that removal finishes first and
// this sweep then finds nothing left on the path.
//
// A thread whose own CLI reported removing this worktree, or has an
// `ExitWorktree` call in flight, is left to follow its move
// (applyProviderWorkspaceChange) as the exiting thread is: the registry can
// show the removal before the app has read the tool result, and that session
// is the one doing the moving. The occupants' pending events are processed
// first so a result already read is not mistaken for an outside removal.
//
// announce makes the removal reach every client even when no thread moved.
// A failure is reported to the caller with the affected thread ids rather
// than logged: the threads whose rows are wrong are where it must be seen.
func (a *App) reclaimRemovedWorktree(removal *worktreeRemoval, exitingThreadID string, announce bool, report func(threadIDs []string, problem string)) {
	removal.path = strings.TrimSpace(removal.path)
	if removal.path == "" {
		return
	}
	worktreePath := removal.path
	a.cancelWorktreeSetupsForPath(worktreePath)
	occupants, err := a.threadsReferencingWorkspace(worktreePath)
	if err != nil {
		report(nil, fmt.Sprintf("the threads attached to it could not be listed: %v", err))
		return
	}
	others := make([]string, 0, len(occupants))
	for _, id := range occupants {
		if id != exitingThreadID {
			others = append(others, id)
		}
	}
	slices.Sort(others)
	others = slices.Compact(others)
	for _, id := range others {
		if a.hasActiveSession(id) {
			a.drainProviderEvents(id, "worktree removed")
		}
	}
	unlocks := make([]func(), 0, len(others))
	defer func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}()
	for _, id := range others {
		unlock, err := a.threadLocks().LockCtx(a.lifeCtx(), id)
		if err != nil {
			// Shutdown; the next boot's watcher reads the registry again.
			return
		}
		unlocks = append(unlocks, unlock)
	}
	others = slices.DeleteFunc(others, func(id string) bool {
		exiting := a.providerExitingWorktree(id, worktreePath)
		if !exiting {
			current, ok := a.sessionManager().get(id)
			exiting = ok && a.pendingWorktreeExits.claim(id, current.Token)
		}
		if exiting && removal.cause == removedOutsideCause {
			// The registry showed the removal before the app read the
			// exiting thread's ExitWorktree result: Claude removed it.
			removal.cause = removedByClaudeCause
		}
		return exiting
	})
	mutable, err := a.mutableWorkspaceThreads(others)
	if err != nil {
		report(others, fmt.Sprintf("the threads attached to it could not be checked: %v", err))
		return
	}
	for _, id := range mutable {
		a.cancelThreadWorktreeSetup(id)
	}
	reattached, sweepErr := a.reattachThreadsFromRemovedWorktree(removal, mutable)
	if sweepErr != nil {
		report(mutable, sweepErr.Error())
	}
	if err := a.finishWorktreeRemoval(removal, reattached, announce); err != nil {
		report(mutable, err.Error())
	}
}

// worktreesBaseDir is where this app cuts a project's worktrees:
// `<configDir>/worktrees/<repo name>`, or the conventional `<repo>-worktrees`
// sibling when the app has no config dir (tests). The watcher observes it so
// an `rm -rf` of a checkout there is seen at once rather than at the next
// liveness read.
func (a *App) worktreesBaseDir(projectPath string) string {
	if strings.TrimSpace(a.configDir) != "" {
		return filepath.Join(a.configDir, "worktrees", filepath.Base(projectPath))
	}
	return gitops.DefaultWorktreesBaseDir(projectPath)
}
