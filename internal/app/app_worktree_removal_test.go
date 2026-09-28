package app

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/terminal"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/threadmode"
)

// What every removal shares, whoever performs it: the moved rows reach the
// caller, one worktree:removed event reaches every client, terminals opened
// in the checkout close, and sessions stop without restarting.

// removalFixture is a watchFixture with a caller thread at the project root
// and one occupant on a linked worktree.
type removalFixture struct {
	watchFixture
	worktree string
	caller   string
	occupant string
}

func newRemovalFixture(t *testing.T, branch string) removalFixture {
	t.Helper()
	f := newWatchFixture(t)
	worktree := f.worktree(t, branch)
	caller := f.thread(t, "thread-removal-caller", f.repo, "main")
	occupant := f.thread(t, "thread-removal-occupant", worktree, branch)
	return removalFixture{watchFixture: f, worktree: worktree, caller: caller.ID, occupant: occupant.ID}
}

func (f removalFixture) remove(t *testing.T) WorktreeRemoval {
	t.Helper()
	removal, err := f.app.RemoveOtherWorktree(WorkspaceRef{ProjectID: f.project.ID, WorkspacePath: f.repo}, f.worktree, true)
	if err != nil {
		t.Fatalf("RemoveOtherWorktree: %v", err)
	}
	return removal
}

// The in-app removal returns the rows it moved, so the caller's pane never
// acts on a row the thread:updated broadcast has not reached yet. An idle
// session there is stopped and not restarted, and nobody is told: the user
// asked for the removal.
func TestRemoveOtherWorktreeReturnsMovedRowsAndStopsSessionsQuietly(t *testing.T) {
	f := newRemovalFixture(t, "feature-in-app")
	f.app.sessionManager().put(f.occupant, session{Provider: string(provider.Claude), Token: "token-occupant"})
	var stops, starts []string
	f.app.stopSessionFn = func(id string) error { stops = append(stops, id); return nil }
	f.app.startSessionFn = func(id string) error { starts = append(starts, id); return nil }

	removal := f.remove(t)

	if len(removal.Reattached) != 1 || removal.Reattached[0].ID != f.occupant ||
		!samePath(removal.Reattached[0].WorkspacePath, f.repo) || removal.Reattached[0].WorktreePath != "" || removal.Reattached[0].Branch != "main" {
		t.Fatalf("Reattached = %+v, want the occupant at the project root on main", removal.Reattached)
	}
	if !samePath(removal.Workspace.WorkspacePath, f.repo) || removal.Workspace.Branch != "main" {
		t.Errorf("Workspace = %+v, want the project root on main", removal.Workspace)
	}
	if !slices.Equal(stops, []string{f.occupant}) || len(starts) != 0 {
		t.Errorf("stops = %v starts = %v, want one stop of the occupant and no start", stops, starts)
	}
	if f.app.sessionManager().runtime.PendingConfigReconnect(f.occupant) {
		t.Error("a deferred restart was armed by the removal")
	}
	if notices := f.notices(t, f.occupant); len(notices) != 0 {
		t.Errorf("notices = %q, want none for a removal the user asked for", notices)
	}
	removals := f.removals()
	if len(removals) != 1 || !samePath(removals[0].Path, f.worktree) || removals[0].Branch != "main" ||
		!slices.Equal(removals[0].ThreadIDs, []string{f.occupant}) {
		t.Errorf("worktree:removed events = %+v, want one naming the occupant", removals)
	}
}

// GitRemoveWorktree returns its own thread's row and every sibling's.
func TestGitRemoveWorktreeReturnsEveryMovedRow(t *testing.T) {
	f := newWatchFixture(t)
	owner := f.thread(t, "thread-git-remove-owner", f.repo, "main")
	worktree, err := f.app.GitCreateWorktree(owner.ID, "feature/git-remove-rows")
	if err != nil {
		t.Fatalf("GitCreateWorktree: %v", err)
	}
	sibling := f.thread(t, "thread-git-remove-sibling", worktree, "feature/git-remove-rows")

	removal, err := f.app.GitRemoveWorktree(owner.ID)
	if err != nil {
		t.Fatalf("GitRemoveWorktree: %v", err)
	}
	var ids []string
	for _, row := range removal.Reattached {
		if !samePath(row.WorkspacePath, f.repo) || row.WorktreePath != "" {
			t.Errorf("returned row %s = %+v, want it at the project root", row.ID, row)
		}
		ids = append(ids, row.ID)
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []string{owner.ID, sibling.ID}) {
		t.Errorf("Reattached ids = %v, want %s and %s", ids, owner.ID, sibling.ID)
	}
}

// A terminal opened in the checkout or below it closes with the removal; the
// status the confirmation reads counts them first. One opened elsewhere is
// left alone.
func TestWorktreeRemovalClosesTerminalsOpenedInIt(t *testing.T) {
	f := newRemovalFixture(t, "feature-terminals")
	f.app.terminals = terminal.NewManager(nil, nil)
	t.Cleanup(func() { _ = f.app.terminals.Shutdown() })
	sub := filepath.Join(f.worktree, "nested")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	open := func(owner, cwd string) terminal.SessionSummary {
		t.Helper()
		summary, err := f.app.terminals.Open(owner, terminal.SessionOptions{Shell: "/bin/sh", Args: []string{"-c", "sleep 30"}, Cwd: cwd})
		if err != nil {
			t.Fatalf("open terminal in %s: %v", cwd, err)
		}
		return summary
	}
	open(f.occupant, f.worktree)
	open("draft:placeholder", sub)
	kept := open(f.caller, f.repo)

	status, err := f.app.GitWorktreeStatus(WorkspaceRef{ProjectID: f.project.ID, WorkspacePath: f.repo}, f.worktree)
	if err != nil {
		t.Fatalf("GitWorktreeStatus: %v", err)
	}
	if status.Terminals != 2 {
		t.Fatalf("status.Terminals = %d, want 2", status.Terminals)
	}

	f.remove(t)

	if got := f.app.terminals.Matching(func(terminal.SessionSummary) bool { return true }); len(got) != 1 || got[0].TerminalID != kept.TerminalID {
		t.Fatalf("open terminals after removal = %+v, want only %s", got, kept.TerminalID)
	}
}

// The external path closes them too: the removal is seen after the
// directory is gone, and the terminal's recorded cwd still has to match.
func TestExternalWorktreeRemovalClosesTerminalsOpenedInIt(t *testing.T) {
	f := newRemovalFixture(t, "feature-external-terminals")
	f.app.terminals = terminal.NewManager(nil, nil)
	t.Cleanup(func() { _ = f.app.terminals.Shutdown() })
	if _, err := f.app.terminals.Open("draft:placeholder", terminal.SessionOptions{Shell: "/bin/sh", Args: []string{"-c", "sleep 30"}, Cwd: f.worktree}); err != nil {
		t.Fatalf("open terminal: %v", err)
	}

	testutil.RunGit(t, f.repo, "worktree", "remove", "--force", f.worktree)
	f.app.reconcileProjectWorktrees(f.repo, []string{f.worktree})

	if got := f.app.terminals.Matching(func(terminal.SessionSummary) bool { return true }); len(got) != 0 {
		t.Fatalf("open terminals after the external removal = %+v, want none", got)
	}
}

// A removal the watcher saw is announced even when no thread row points
// at the path (only a draft placeholder did); a registry re-read that saw
// no removal announces nothing.
func TestReconcileProjectWorktreesAnnouncesRemovalWithoutThreads(t *testing.T) {
	f := newWatchFixture(t)
	worktree := f.worktree(t, "feature-draft-only")
	testutil.RunGit(t, f.repo, "worktree", "remove", "--force", worktree)

	f.app.reconcileProjectWorktrees(f.repo, nil)
	if got := f.removals(); len(got) != 0 {
		t.Fatalf("worktree:removed without a reported removal = %+v, want none", got)
	}

	f.app.reconcileProjectWorktrees(f.repo, []string{worktree})
	got := f.removals()
	if len(got) != 1 || !samePath(got[0].Path, worktree) || got[0].ProjectID != f.project.ID || got[0].Branch != "main" || len(got[0].ThreadIDs) != 0 {
		t.Fatalf("worktree:removed = %+v, want one for %s with no threads", got, worktree)
	}
}

// A retired row keeps the path it had and is never moved, so it does not
// make a path gone. Were it counted, every registry change would sweep the
// path again, taking the retired thread's lock each time.
func TestReconcileProjectWorktreesIgnoresRetiredRows(t *testing.T) {
	f := newWatchFixture(t)
	worktree := f.worktree(t, "feature-retired")
	holder := f.thread(t, "thread-retired-holder", worktree, "feature-retired")
	holder.Mode = threadmode.ModeHolder
	if err := f.app.store.UpdateThread(holder); err != nil {
		t.Fatalf("UpdateThread(holder): %v", err)
	}
	if retired, err := f.app.threadApplication().CheckCleanup(holder.ID); err != nil || !retired {
		t.Fatalf("CheckCleanup(holder) = %v, %v; want a retired row", retired, err)
	}
	testutil.RunGit(t, f.repo, "worktree", "remove", "--force", worktree)

	unlock := f.app.threadLocks().Lock(holder.ID)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.app.reconcileProjectWorktrees(f.repo, nil)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		unlock()
		<-done
		t.Fatal("the sweep waited on a retired row's lock: the row counted the path as gone")
	}
	unlock()
	if row := f.row(t, holder.ID); !samePath(row.WorkspacePath, worktree) {
		t.Errorf("retired row moved: %+v", row)
	}
	if got := f.removals(); len(got) != 0 {
		t.Errorf("worktree:removed = %+v, want none", got)
	}
}

// A thread whose own CLI reported removing the worktree follows that move
// itself and keeps its session; the registry sweep must leave it alone even
// when it runs first.
func TestReconcileProjectWorktreesLeavesAThreadExitingItsOwnWorktree(t *testing.T) {
	f := newWatchFixture(t)
	worktree := f.worktree(t, "feature-exiting")
	exiting := f.thread(t, "thread-exiting-own-worktree", worktree, "feature-exiting")
	sibling := f.thread(t, "thread-exiting-sibling", worktree, "feature-exiting")
	f.app.sessionManager().put(exiting.ID, session{Provider: string(provider.Claude), Token: "token-exiting"})
	var stops []string
	f.app.stopSessionFn = func(id string) error { stops = append(stops, id); return nil }
	f.app.providerWorktreeExits.Store(exiting.ID, worktree)

	testutil.RunGit(t, f.repo, "worktree", "remove", "--force", worktree)
	f.app.reconcileProjectWorktrees(f.repo, []string{worktree})

	if row := f.row(t, exiting.ID); !samePath(row.WorkspacePath, worktree) {
		t.Errorf("exiting thread moved by the sweep: %+v", row)
	}
	if len(stops) != 0 {
		t.Errorf("stops = %v, want the exiting thread's session left running", stops)
	}
	if notices := f.notices(t, exiting.ID); len(notices) != 0 {
		t.Errorf("notices on the exiting thread = %q, want none", notices)
	}
	f.assertAtRoot(t, sibling.ID)
}

// A removal that moves no thread never reads the project's branch: nothing
// needs it.
func TestReattachWithNothingToMoveSkipsTheBranchRead(t *testing.T) {
	f := newWatchFixture(t)
	atRoot := f.thread(t, "thread-branch-read-root", f.repo, "main")
	removal := &worktreeRemoval{projectID: f.project.ID, project: f.repo, path: filepath.Join(f.repo, "..", "never-there")}

	moved, err := f.app.reattachThreadsFromRemovedWorktree(removal, []string{atRoot.ID})
	if err != nil {
		t.Fatalf("reattach: %v", err)
	}
	if len(moved) != 0 {
		t.Fatalf("moved = %+v, want none", moved)
	}
	if removal.branchRead {
		t.Error("the project branch was read although no thread moved")
	}
}

// The status stream of a workspace being removed stays off the wire while
// git deletes it: what git reports then describes nothing a client can use.
func TestRemoveOtherWorktreeHoldsGitStatusOfTheRemovedPath(t *testing.T) {
	f := newRemovalFixture(t, "feature-status-hold")
	stub := &stubGitWatch{current: gitops.GitStatus{IsRepo: true, Branch: "feature-status-hold"}}
	installGitWatchForTest(t, f.app, stub)
	events, mu := captureGitStatusEmissions(f.app)
	sub, err := f.app.GitStatusSubscribe(context.Background(), WorkspaceRef{ProjectID: f.project.ID, WorkspacePath: f.worktree})
	if err != nil {
		t.Fatalf("GitStatusSubscribe: %v", err)
	}
	stub.setStatus(gitops.GitStatus{IsRepo: true, Branch: "feature-status-hold", HasChanges: true, FileCount: 7})

	f.remove(t)

	time.Sleep(time.Second)
	if got := gitStatusEventsFor(events, mu, sub.Cwd); len(got) != 0 {
		t.Fatalf("git:status for the removed worktree = %+v, want none", got)
	}
}

// A removal git refuses gives the stream back and refreshes it, so the
// workspace that is still there shows its current state.
func TestRemoveOtherWorktreeResumesGitStatusWhenRemovalFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permission this test relies on")
	}
	f := newWatchFixture(t)
	parent := filepath.Join(t.TempDir(), "locked-parent")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	worktree := filepath.Join(parent, "wt")
	testutil.RunGit(t, f.repo, "worktree", "add", "-b", "feature-status-resume", worktree)
	occupant := f.thread(t, "thread-status-resume-occupant", worktree, "feature-status-resume")
	stub := &stubGitWatch{current: gitops.GitStatus{IsRepo: true, Branch: "feature-status-resume"}}
	installGitWatchForTest(t, f.app, stub)
	events, mu := captureGitStatusEmissions(f.app)
	sub, err := f.app.GitStatusSubscribe(context.Background(), WorkspaceRef{ProjectID: f.project.ID, WorkspacePath: worktree})
	if err != nil {
		t.Fatalf("GitStatusSubscribe: %v", err)
	}
	if err := os.Chmod(parent, 0o555); err != nil {
		t.Fatalf("chmod parent: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
	stub.setStatus(gitops.GitStatus{IsRepo: true, Branch: "feature-status-resume", HasChanges: true, FileCount: 3})

	if _, err := f.app.RemoveOtherWorktree(WorkspaceRef{ProjectID: f.project.ID, WorkspacePath: f.repo}, worktree, true); err == nil {
		t.Fatal("RemoveOtherWorktree succeeded in a directory it cannot delete from")
	}
	got := waitForGitStatusEvent(t, events, mu, sub.Cwd, 5*time.Second)
	if got.Status.FileCount != 3 {
		t.Fatalf("git:status after the failed removal = %+v, want the current state", got.Status)
	}
	if row := f.row(t, occupant.ID); !samePath(row.WorkspacePath, worktree) || !strings.Contains(row.Branch, "status-resume") {
		t.Errorf("occupant moved by a removal that failed: %+v", row)
	}
}
