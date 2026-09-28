package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/terminal"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/workflow/engine"
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
	assertRemovedByClaudeNotice(t, f, sibling.ID)
}

// assertRemovedByClaudeNotice checks that a sibling moved by a sweep that
// left an exiting Claude thread alone is told Claude removed the worktree.
func assertRemovedByClaudeNotice(t *testing.T, f watchFixture, threadID string) {
	t.Helper()
	notices := f.notices(t, threadID)
	if len(notices) != 1 || !strings.Contains(notices[0], removedByClaudeCause) {
		t.Errorf("sibling notices = %q, want one saying the worktree %s", notices, removedByClaudeCause)
	}
}

// exitWorktreeStart is the tool start the Claude parser emits for a
// top-level `ExitWorktree` call.
func exitWorktreeStart(threadID, toolUseID, action string) provider.ProviderEvent {
	return provider.ProviderEvent{
		Kind:     provider.EventToolStart,
		ThreadID: threadID,
		ItemID:   toolUseID,
		ItemType: "ExitWorktree",
		Meta:     json.RawMessage(`{"toolName":"ExitWorktree","input":{"action":"` + action + `"}}`),
	}
}

// The CLI deletes the worktree before it answers `ExitWorktree`, so the
// registry sweep can run with only the tool_use read. The thread making
// that call is left alone as it is once the result arrives.
func TestReconcileProjectWorktreesLeavesAThreadWithExitWorktreeInFlight(t *testing.T) {
	f := newWatchFixture(t)
	worktree := f.worktree(t, "feature-exit-pending")
	exiting := f.thread(t, "thread-exit-pending", worktree, "feature-exit-pending")
	sibling := f.thread(t, "thread-exit-pending-sibling", worktree, "feature-exit-pending")
	f.app.sessionManager().put(exiting.ID, session{Provider: string(provider.Claude), Token: "token-exit-pending"})
	f.app.sessionManager().put(sibling.ID, session{Provider: string(provider.Claude), Token: "token-exit-sibling"})
	var mu sync.Mutex
	var stops []string
	f.app.stopSessionFn = func(id string) error {
		mu.Lock()
		defer mu.Unlock()
		stops = append(stops, id)
		return nil
	}
	f.app.sessionEventHandler(exiting.ID, "token-exit-pending", string(provider.Claude))(exitWorktreeStart(exiting.ID, "toolu_exit_pending", "remove"))

	testutil.RunGit(t, f.repo, "worktree", "remove", "--force", worktree)
	f.app.reconcileProjectWorktrees(f.repo, []string{worktree})

	if row := f.row(t, exiting.ID); !samePath(row.WorkspacePath, worktree) {
		t.Errorf("exiting thread moved by the sweep: %+v", row)
	}
	mu.Lock()
	gotStops := slices.Clone(stops)
	mu.Unlock()
	if !slices.Equal(gotStops, []string{sibling.ID}) {
		t.Errorf("stops = %v, want only the sibling %s stopped", gotStops, sibling.ID)
	}
	if notices := f.notices(t, exiting.ID); len(notices) != 0 {
		t.Errorf("notices on the exiting thread = %q, want none", notices)
	}
	f.assertAtRoot(t, sibling.ID)
	assertRemovedByClaudeNotice(t, f, sibling.ID)
}

// A pending `ExitWorktree` that ends without a removal the app follows
// (here a refused call) sweeps the project again, so the thread the sweep
// left alone does not stay on a vanished path.
func TestRefusedExitWorktreeResweepsTheThreadItDeferredTo(t *testing.T) {
	f := newWatchFixture(t)
	worktree := f.worktree(t, "feature-exit-refused")
	exiting := f.thread(t, "thread-exit-refused", worktree, "feature-exit-refused")
	f.app.sessionManager().put(exiting.ID, session{Provider: string(provider.Claude), Token: "token-exit-refused"})
	var mu sync.Mutex
	var stops []string
	f.app.stopSessionFn = func(id string) error {
		mu.Lock()
		defer mu.Unlock()
		stops = append(stops, id)
		return nil
	}
	handle := f.app.sessionEventHandler(exiting.ID, "token-exit-refused", string(provider.Claude))
	handle(exitWorktreeStart(exiting.ID, "toolu_exit_refused", "remove"))

	testutil.RunGit(t, f.repo, "worktree", "remove", "--force", worktree)
	f.app.reconcileProjectWorktrees(f.repo, []string{worktree})
	if row := f.row(t, exiting.ID); !samePath(row.WorkspacePath, worktree) {
		t.Fatalf("exiting thread moved while its call was pending: %+v", row)
	}

	handle(provider.ProviderEvent{
		Kind:     provider.EventToolComplete,
		ThreadID: exiting.ID,
		ItemID:   "toolu_exit_refused",
		Meta:     json.RawMessage(`{"is_error":true}`),
	})
	waitFor(t, "the refused call's thread to be reattached", func() bool {
		row, err := f.app.store.GetThread(exiting.ID)
		return err == nil && samePath(row.WorkspacePath, f.repo)
	})
	waitFor(t, "the refused call's session to stop", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Equal(stops, []string{exiting.ID})
	})
	if _, pending := f.app.pendingWorktreeExits.toolUseID(exiting.ID, "token-exit-refused"); pending {
		t.Error("the refused call is still recorded as pending")
	}
}

// An `ExitWorktree keep` deletes nothing, so the sweep treats its thread
// like any other.
func TestExitWorktreeKeepIsNotAPendingRemoval(t *testing.T) {
	app := newTestAppWithStore(t)
	app.sessionEventHandler("thread-exit-keep", "token-keep", string(provider.Claude))(exitWorktreeStart("thread-exit-keep", "toolu_keep", "keep"))
	app.drainProviderEvents("thread-exit-keep", "test")
	if _, pending := app.pendingWorktreeExits.toolUseID("thread-exit-keep", "token-keep"); pending {
		t.Error("ExitWorktree keep was recorded as a pending removal")
	}
	app.sessionEventHandler("thread-exit-keep", "token-keep", string(provider.Claude))(exitWorktreeStart("thread-exit-keep", "toolu_remove", "remove"))
	app.drainProviderEvents("thread-exit-keep", "test")
	if id, pending := app.pendingWorktreeExits.toolUseID("thread-exit-keep", "token-keep"); !pending || id != "toolu_remove" {
		t.Errorf("pending = %q %v, want toolu_remove recorded", id, pending)
	}
	app.sessionEventHandler("thread-exit-keep", "token-keep", string(provider.Claude))(provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "thread-exit-keep"})
	app.drainProviderEvents("thread-exit-keep", "test")
	if _, pending := app.pendingWorktreeExits.toolUseID("thread-exit-keep", "token-keep"); pending {
		t.Error("the pending call survived the end of its turn")
	}
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

// The list marks a worktree git still registers whose directory is gone, so
// a client can tell a checkout deleted with `rm -rf` from a live one.
func TestGitListWorktreesMarksARegisteredWorktreeWhoseDirectoryIsGone(t *testing.T) {
	f := newWatchFixture(t)
	live := f.worktree(t, "list-live")
	deleted := f.worktree(t, "list-deleted")
	if err := os.RemoveAll(deleted); err != nil {
		t.Fatalf("remove directory: %v", err)
	}

	items, err := f.app.GitListWorktrees(WorkspaceRef{ProjectID: f.project.ID, WorkspacePath: f.repo})
	if err != nil {
		t.Fatalf("GitListWorktrees: %v", err)
	}
	missing := map[string]bool{}
	for _, item := range items {
		missing[canonicalExistingPrefix(item.Path)] = item.Missing
	}
	for path, want := range map[string]bool{f.repo: false, live: false, deleted: true} {
		got, ok := missing[canonicalExistingPrefix(path)]
		if !ok || got != want {
			t.Errorf("%s: listed=%v missing=%v, want listed with missing=%v (items %+v)", path, ok, got, want, items)
		}
	}
}

// stopRecorder replaces session stops with a record of the thread ids.
func stopRecorder(app *App) func() []string {
	var mu sync.Mutex
	var stops []string
	app.stopSessionFn = func(id string) error {
		mu.Lock()
		defer mu.Unlock()
		stops = append(stops, id)
		return nil
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(stops)
	}
}

// A workflow's cleanup is a removal the app performs: a thread still on the
// checkout moves to the root and its session stops, without a notice.
func TestWorkflowDiscardReattachesThreadsWithoutANotice(t *testing.T) {
	f := newWatchFixture(t)
	worktree := f.worktree(t, "workflow-discard-occupied")
	occupant := f.thread(t, "thread-workflow-discard", worktree, "workflow-discard-occupied")
	f.app.sessionManager().put(occupant.ID, session{Provider: string(provider.Claude), Token: "token-workflow-discard"})
	stops := stopRecorder(f.app)
	item := store.WorkItem{
		ID: "workflow-discard-occupied", ProjectID: f.project.ID, Goal: "Discard",
		WorkflowID: "wf", WorkflowScope: "shared", State: string(engine.StateDone),
		WorktreePath: worktree, Branch: "workflow-discard-occupied", BaseBranch: "main",
		Source: "manual", CreatedAt: 1, StartedAt: 1, EndedAt: 2,
	}
	if err := f.app.store.CreateWorkItem(item); err != nil {
		t.Fatalf("CreateWorkItem: %v", err)
	}

	if _, err := f.app.WorkflowDiscardItem(item.ID); err != nil {
		t.Fatalf("WorkflowDiscardItem: %v", err)
	}

	f.assertAtRoot(t, occupant.ID)
	if got := stops(); !slices.Equal(got, []string{occupant.ID}) {
		t.Errorf("stops = %v, want %s stopped", got, occupant.ID)
	}
	if notices := f.notices(t, occupant.ID); len(notices) != 0 {
		t.Errorf("notices = %q, want none for a removal the app performed", notices)
	}
	if removals := f.removals(); len(removals) == 0 || !slices.Equal(removals[0].ThreadIDs, []string{occupant.ID}) {
		t.Errorf("worktree:removed events = %+v, want one naming %s", removals, occupant.ID)
	}
	if f.app.appWorktreeRemovals.contains(worktree) {
		t.Error("the removal is still registered after it finished")
	}
}

// The runner's removals (unit retirement, provisioning rollback) reach the
// same path through its host.
func TestWorkflowHostRemoveWorktreeReattachesThreadsWithoutANotice(t *testing.T) {
	f := newWatchFixture(t)
	worktree := f.worktree(t, "workflow-host-occupied")
	occupant := f.thread(t, "thread-workflow-host", worktree, "workflow-host-occupied")

	if err := (workflowHostAdapter{app: f.app}).RemoveWorktree(f.repo, worktree, false); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}

	f.assertAtRoot(t, occupant.ID)
	if notices := f.notices(t, occupant.ID); len(notices) != 0 {
		t.Errorf("notices = %q, want none for a removal the app performed", notices)
	}
}

// A registry sweep that reaches a checkout while the app is removing it
// reacts as the app's removal does: the thread moves and its session stops,
// without a notice.
func TestReconcileProjectWorktreesTreatsARemovalInProgressAsTheApps(t *testing.T) {
	f := newWatchFixture(t)
	worktree := f.worktree(t, "workflow-racing-sweep")
	occupant := f.thread(t, "thread-workflow-racing", worktree, "workflow-racing-sweep")
	f.app.sessionManager().put(occupant.ID, session{Provider: string(provider.Claude), Token: "token-workflow-racing"})
	stops := stopRecorder(f.app)

	done := f.app.appWorktreeRemovals.begin(worktree)
	testutil.RunGit(t, f.repo, "worktree", "remove", "--force", worktree)
	f.app.reconcileProjectWorktrees(f.repo, []string{worktree})
	done()

	f.assertAtRoot(t, occupant.ID)
	if got := stops(); !slices.Equal(got, []string{occupant.ID}) {
		t.Errorf("stops = %v, want %s stopped", got, occupant.ID)
	}
	if notices := f.notices(t, occupant.ID); len(notices) != 0 {
		t.Errorf("notices = %q, want none while the app is removing the worktree", notices)
	}
	if f.app.appWorktreeRemovals.contains(worktree) {
		t.Error("the removal is still registered after it was released")
	}
}

// A removal git refuses releases its registration: a later removal of the
// same path by anything else is reported as usual.
func TestWorkflowWorktreeRemovalReleasesThePathWhenGitFails(t *testing.T) {
	f := newWatchFixture(t)
	worktree := f.worktree(t, "workflow-dirty-kept")
	occupant := f.thread(t, "thread-workflow-dirty", worktree, "workflow-dirty-kept")
	if err := os.WriteFile(filepath.Join(worktree, "uncommitted.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := f.app.removeWorkflowWorktree(f.repo, worktree, false); err == nil {
		t.Fatal("unforced removal of a dirty worktree succeeded")
	}
	if f.app.appWorktreeRemovals.contains(worktree) {
		t.Fatal("the failed removal is still registered")
	}
	if row := f.row(t, occupant.ID); !samePath(row.WorkspacePath, worktree) {
		t.Fatalf("occupant moved by a removal that failed: %+v", row)
	}

	testutil.RunGit(t, f.repo, "worktree", "remove", "--force", worktree)
	f.app.reconcileProjectWorktrees(f.repo, []string{worktree})
	notices := f.notices(t, occupant.ID)
	if len(notices) != 1 || !strings.Contains(notices[0], "was removed outside Agent Overflow") {
		t.Errorf("notices = %q, want the outside removal reported", notices)
	}
}
