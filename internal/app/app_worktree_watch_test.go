package app

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/sessionruntime"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/worktreewatch"
)

// A worktree removed outside the app (a terminal's `git worktree remove`, an
// `rm -rf`) has no RPC and no provider event. The registry watcher is the
// signal, and the reaction is the same sweep the in-app removal runs. These
// tests call the sweep directly for its rules and run the watcher end to end
// once to prove the wire between them.

type watchFixture struct {
	app     *App
	repo    string
	project store.Project
	rows    *threadRowRecorder
}

func newWatchFixture(t *testing.T) watchFixture {
	t.Helper()
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	rows := &threadRowRecorder{}
	app.testEmitHook = rows.hook
	return watchFixture{app: app, repo: repo, project: project, rows: rows}
}

// thread creates a thread row on workspace; a linked worktree sets both
// path columns and the branch, the way every in-app move does.
func (f watchFixture) thread(t *testing.T, id, workspace, branch string) store.Thread {
	t.Helper()
	thread := testThread(id)
	thread.ProjectID = f.project.ID
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = workspace
	thread.Branch = branch
	if !samePath(workspace, f.repo) {
		thread.WorktreePath = workspace
	}
	thread.UpdatedAt = 1_700_000_000_000
	if err := f.app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread(%s): %v", id, err)
	}
	return thread
}

// worktree cuts a linked worktree where the app would: under the project's
// worktrees base dir, which the watcher also observes.
func (f watchFixture) worktree(t *testing.T, branch string) string {
	t.Helper()
	path := filepath.Join(f.app.worktreesBaseDir(f.repo), branch)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir worktrees base: %v", err)
	}
	testutil.RunGit(t, f.repo, "worktree", "add", "-b", branch, path)
	return path
}

func (f watchFixture) row(t *testing.T, id string) store.Thread {
	t.Helper()
	row, err := f.app.store.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread(%s): %v", id, err)
	}
	return row
}

func (f watchFixture) assertAtRoot(t *testing.T, id string) store.Thread {
	t.Helper()
	row := f.row(t, id)
	if !samePath(row.WorkspacePath, f.repo) || row.WorktreePath != "" || row.Branch != "main" {
		t.Errorf("thread %s = {workspace %q worktree %q branch %q}, want the project root on main", id, row.WorkspacePath, row.WorktreePath, row.Branch)
	}
	return row
}

func TestReconcileProjectWorktreesReattachesThreadsOfExternallyRemovedWorktree(t *testing.T) {
	f := newWatchFixture(t)
	worktree := f.worktree(t, "feature-ext")
	home := t.TempDir()
	t.Setenv("HOME", home)
	const sessionID = "0192bbbb-external-remove"
	attached := f.thread(t, "thread-ext-attached", worktree, "feature-ext")
	attached.SessionRef = sessionID
	if err := f.app.store.UpdateThread(attached); err != nil {
		t.Fatalf("UpdateThread(attached): %v", err)
	}
	src := writeClaudeProjectSession(t, home, worktree, sessionID, "{\"type\":\"user\"}\n")
	archived := f.thread(t, "thread-ext-archived", worktree, "feature-ext")
	if _, _, err := f.app.store.ArchiveThread(archived.ID); err != nil {
		t.Fatalf("ArchiveThread: %v", err)
	}
	atRoot := f.thread(t, "thread-ext-root", f.repo, "main")

	// The removal happens in a terminal: registration and directory both go.
	testutil.RunGit(t, f.repo, "worktree", "remove", "--force", worktree)

	f.app.reconcileProjectWorktrees(f.repo)

	reattached := f.assertAtRoot(t, attached.ID)
	if reattached.UpdatedAt != attached.UpdatedAt {
		t.Errorf("system-driven reattach bumped UpdatedAt: %d -> %d", attached.UpdatedAt, reattached.UpdatedAt)
	}
	if reattached.SessionRef != sessionID {
		t.Errorf("SessionRef = %q, want %q kept", reattached.SessionRef, sessionID)
	}
	f.assertAtRoot(t, archived.ID)
	if row := f.row(t, atRoot.ID); !samePath(row.WorkspacePath, f.repo) || row.UpdatedAt != atRoot.UpdatedAt {
		t.Errorf("root thread touched by the sweep: %+v", row)
	}

	// The transcript followed the row to the root slug, moved not copied.
	wantDest := filepath.Join(home, ".claude", "projects", claudeProjectSlugForTest(t, f.repo), sessionID+".jsonl")
	if _, err := os.Stat(wantDest); err != nil {
		t.Errorf("transcript not relocated under the root slug: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("stale transcript under the dead slug not purged: err=%v", err)
	}
	if located, err := sessionfork.LocateSessionFile(testProviderProjectsDir(t), sessionID, f.repo); err != nil || !samePath(located, wantDest) {
		t.Errorf("LocateSessionFile from root = %q, %v; want %q", located, err, wantDest)
	}

	// Every client learned about both rows.
	for _, id := range []string{attached.ID, archived.ID} {
		if len(f.rows.fullRowsFor(id)) == 0 {
			t.Errorf("no thread:updated broadcast for %s", id)
		}
	}
	if n := len(f.rows.fullRowsFor(atRoot.ID)); n != 0 {
		t.Errorf("root thread broadcast %d times, want none", n)
	}
}

// `rm -rf` leaves the registration behind. A registered checkout that is not
// on disk is as gone as an unregistered one.
func TestReconcileProjectWorktreesReattachesDeletedButRegisteredWorktree(t *testing.T) {
	f := newWatchFixture(t)
	worktree := f.worktree(t, "feature-rm")
	attached := f.thread(t, "thread-rm-attached", worktree, "feature-rm")

	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("rm -rf worktree: %v", err)
	}
	f.app.reconcileProjectWorktrees(f.repo)

	f.assertAtRoot(t, attached.ID)
}

func TestReconcileProjectWorktreesLeavesLiveWorktreesAlone(t *testing.T) {
	f := newWatchFixture(t)
	kept := f.worktree(t, "feature-kept")
	removed := f.worktree(t, "feature-removed")
	onKept := f.thread(t, "thread-on-kept", kept, "feature-kept")
	onRemoved := f.thread(t, "thread-on-removed", removed, "feature-removed")

	testutil.RunGit(t, f.repo, "worktree", "remove", "--force", removed)
	f.app.reconcileProjectWorktrees(f.repo)

	if row := f.row(t, onKept.ID); !samePath(row.WorkspacePath, kept) || !samePath(row.WorktreePath, kept) {
		t.Errorf("thread on the surviving worktree moved: %+v", row)
	}
	if n := len(f.rows.fullRowsFor(onKept.ID)); n != 0 {
		t.Errorf("surviving worktree's thread broadcast %d times, want none", n)
	}
	f.assertAtRoot(t, onRemoved.ID)

	// A second read of the same state is a no-op: nothing left on the path.
	f.rows.mu.Lock()
	f.rows.events = nil
	f.rows.mu.Unlock()
	f.app.reconcileProjectWorktrees(f.repo)
	if n := len(f.rows.fullRowsFor(onRemoved.ID)); n != 0 {
		t.Errorf("idempotent sweep broadcast %d rows", n)
	}
}

// A thread whose repository root is not a checkout (deleted repository) has
// nowhere to be reattached to; the sweep must not move it onto that path.
func TestReconcileProjectWorktreesSkipsProjectWhoseRootIsGone(t *testing.T) {
	f := newWatchFixture(t)
	worktree := f.worktree(t, "feature-orphan")
	attached := f.thread(t, "thread-orphan", worktree, "feature-orphan")

	if err := os.RemoveAll(f.repo); err != nil {
		t.Fatalf("remove repo: %v", err)
	}
	f.app.reconcileProjectWorktrees(f.repo)

	if row := f.row(t, attached.ID); !samePath(row.WorkspacePath, worktree) {
		t.Errorf("thread moved although the project root is gone: %+v", row)
	}
}

// An idle thread with a live session restarts it from the root: the process
// still has the deleted directory as its cwd.
func TestReconcileProjectWorktreesRestartsIdleSession(t *testing.T) {
	f := newWatchFixture(t)
	worktree := f.worktree(t, "feature-idle")
	attached := f.thread(t, "thread-idle-session", worktree, "feature-idle")
	f.app.sessionManager().put(attached.ID, session{Provider: string(provider.Claude), Token: "token-idle"})
	var stops, starts []string
	f.app.stopSessionFn = func(id string) error { stops = append(stops, id); return nil }
	f.app.startSessionFn = func(id string) error { starts = append(starts, id); return nil }

	testutil.RunGit(t, f.repo, "worktree", "remove", "--force", worktree)
	f.app.reconcileProjectWorktrees(f.repo)

	f.assertAtRoot(t, attached.ID)
	if len(stops) != 1 || stops[0] != attached.ID || len(starts) != 1 || starts[0] != attached.ID {
		t.Errorf("stops = %v starts = %v, want one of each for %s", stops, starts, attached.ID)
	}
}

// A thread mid-turn keeps its process: the row heals now, the restart waits
// for the thread to go quiet.
func TestReconcileProjectWorktreesDefersRestartMidTurn(t *testing.T) {
	f := newWatchFixture(t)
	worktree := f.worktree(t, "feature-busy")
	attached := f.thread(t, "thread-mid-turn", worktree, "feature-busy")
	// A turn in flight is what the live session counts, the same signal the
	// deferred restart waits on.
	liveness := sessionruntime.NewLiveness(time.Now())
	liveness.ActiveTurns.Store(1)
	f.app.sessionManager().put(attached.ID, session{Provider: string(provider.Claude), Token: "token-busy", Liveness: liveness})
	f.app.sessionManager().runtime.SetConfigReconnectPollOverride(10 * time.Millisecond)
	restarted := make(chan string, 1)
	f.app.stopSessionFn = func(string) error { return nil }
	f.app.startSessionFn = func(id string) error { restarted <- id; return nil }

	testutil.RunGit(t, f.repo, "worktree", "remove", "--force", worktree)
	f.app.reconcileProjectWorktrees(f.repo)

	f.assertAtRoot(t, attached.ID)
	if current, ok := f.app.sessionManager().get(attached.ID); !ok || current.Token != "token-busy" {
		t.Fatalf("mid-turn session disturbed: present=%v token=%q", ok, current.Token)
	}
	if !f.app.sessionManager().runtime.PendingConfigReconnect(attached.ID) {
		t.Fatal("no deferred restart armed for the mid-turn thread")
	}
	select {
	case id := <-restarted:
		t.Fatalf("session %s restarted while its turn was open", id)
	case <-time.After(100 * time.Millisecond):
	}
	// The process dies on its own (the CLI exits once its cwd is gone):
	// the deferred restart stands down and leaves resumption to the death
	// path, which starts from the healed row.
	f.app.unregisterSession(attached.ID, "token-busy")
	waitUntil(t, 5*time.Second, func() bool { return !f.app.sessionManager().runtime.PendingConfigReconnect(attached.ID) })
	select {
	case id := <-restarted:
		t.Fatalf("deferred restart fired for %s after the session was gone", id)
	default:
	}
}

// End to end: the registry watcher sees a terminal's removal and the row
// heals without anyone viewing or touching the thread.
func TestWorktreeWatchReattachesAfterExternalRemoval(t *testing.T) {
	f := newWatchFixture(t)
	worktree := f.worktree(t, "feature-watched")
	attached := f.thread(t, "thread-watched", worktree, "feature-watched")
	startWorktreeWatchForTest(t, f.app)
	waitUntil(t, 5*time.Second, func() bool { return slices.Contains(f.app.worktreeWatch.Projects(), f.project.Path) })

	testutil.RunGit(t, f.repo, "worktree", "remove", "--force", worktree)

	waitUntil(t, 5*time.Second, func() bool { return samePath(f.row(t, attached.ID).WorkspacePath, f.repo) })
	f.assertAtRoot(t, attached.ID)
}

// The watched set follows the project rows through the broadcast chokepoints,
// so no binding has to know the watcher exists.
func TestSyncWorktreeWatchFollowsProjectRows(t *testing.T) {
	f := newWatchFixture(t)
	startWorktreeWatchForTest(t, f.app)
	// The store template carries a project of its own, so membership is
	// what is asserted, not the whole set.
	if got := f.app.worktreeWatch.Projects(); !slices.Contains(got, f.project.Path) {
		t.Fatalf("Projects() = %v, want %s among them", got, f.project.Path)
	}

	second := testutil.InitGitRepo(t)
	added, err := f.app.ensureProjectForWorkspace(second)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace(second): %v", err)
	}
	if got := f.app.worktreeWatch.Projects(); !slices.Contains(got, added.Path) {
		t.Fatalf("Projects() after create = %v, want %s among them", got, added.Path)
	}

	if err := f.app.store.DeleteProject(added.ID); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	f.app.broadcastProjectDeleted(added.ID)
	if got := f.app.worktreeWatch.Projects(); slices.Contains(got, added.Path) || !slices.Contains(got, f.project.Path) {
		t.Fatalf("Projects() after delete = %v, want %s gone and %s kept", got, added.Path, f.project.Path)
	}
}

// startWorktreeWatchForTest arms the watcher the way startup does, with
// short cadences and without the first-reads gate.
func startWorktreeWatchForTest(t *testing.T, app *App) {
	t.Helper()
	app.worktreeWatch = worktreewatch.NewManager(worktreewatch.Config{
		OnChange:         app.reconcileProjectWorktrees,
		ExtraDir:         app.worktreesBaseDir,
		Debounce:         20 * time.Millisecond,
		PollInterval:     50 * time.Millisecond,
		LivenessInterval: time.Hour,
	})
	t.Cleanup(app.closeWorktreeWatch)
	app.worktreeWatchArmed.Store(true)
	app.syncWorktreeWatch()
}

// A failed auto-reconnect is reported on the thread, not only logged: the
// death banner alone would leave the user guessing why nothing came back.
func TestAutoReconnectFailureSurfacesOnThread(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-auto-reconnect-fails")
	thread.SessionRef = "claude-resume-fails"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	errorItems := collectErrorItemUpserts(t, app, 4)
	app.sessionManager().put(thread.ID, session{Provider: string(provider.Claude), Token: "token-dead"})
	app.stopSessionFn = func(string) error { return nil }
	app.startSessionFn = func(string) error { return errors.New("chdir /gone: no such file or directory") }

	handler := app.sessionEventHandler(thread.ID, "token-dead", string(provider.Claude))
	handler(provider.ProviderEvent{Kind: provider.EventSessionStatus, ThreadID: thread.ID, Content: "error", Timestamp: time.Now()})
	handler(provider.ProviderEvent{Kind: provider.EventSessionStatus, ThreadID: thread.ID, Content: "disconnected", Timestamp: time.Now()})
	waitProviderEvents(t, app, thread.ID)

	select {
	case item := <-errorItems:
		if !strings.Contains(item.Summary, "could not be resumed automatically") || !strings.Contains(item.Summary, "chdir /gone") {
			t.Fatalf("error item = %q, want the auto-reconnect failure and its cause", item.Summary)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no error item reached the thread")
	}
}

// Nothing is watched before the activation gate opens: a sweep moves rows
// and restarts sessions, which a supervisor trial must not do.
func TestSyncWorktreeWatchWaitsForActivation(t *testing.T) {
	f := newWatchFixture(t)
	f.app.startWorktreeWatch()
	t.Cleanup(f.app.closeWorktreeWatch)

	f.app.syncWorktreeWatch()
	if got := f.app.worktreeWatch.Projects(); len(got) != 0 {
		t.Fatalf("Projects() before activation = %v, want none", got)
	}

	f.app.worktreeWatchArmed.Store(true)
	f.app.syncWorktreeWatch()
	if got := f.app.worktreeWatch.Projects(); !slices.Contains(got, f.project.Path) {
		t.Fatalf("Projects() after activation = %v, want %s among them", got, f.project.Path)
	}
}

// The sweep is provider-neutral. A Codex thread resumes by thread id from
// ~/.codex, so it gets the row move and the restart from the root but no
// transcript relocation: the Claude-only branch must not create a Claude
// projects dir for it.
func TestReconcileProjectWorktreesReattachesCodexThreadWithoutRelocation(t *testing.T) {
	f := newWatchFixture(t)
	worktree := f.worktree(t, "feature-codex")
	home := t.TempDir()
	t.Setenv("HOME", home)
	const threadRef = "codex-thread-external-remove"
	attached := f.thread(t, "thread-codex-external", worktree, "feature-codex")
	attached.Provider = string(provider.Codex)
	attached.SessionRef = threadRef
	if err := f.app.store.UpdateThread(attached); err != nil {
		t.Fatalf("UpdateThread(attached): %v", err)
	}
	f.app.sessionManager().put(attached.ID, session{Provider: string(provider.Codex), Token: "token-codex-idle"})
	var stops, starts []string
	f.app.stopSessionFn = func(id string) error { stops = append(stops, id); return nil }
	f.app.startSessionFn = func(id string) error { starts = append(starts, id); return nil }

	testutil.RunGit(t, f.repo, "worktree", "remove", "--force", worktree)
	f.app.reconcileProjectWorktrees(f.repo)

	reattached := f.assertAtRoot(t, attached.ID)
	if reattached.SessionRef != threadRef {
		t.Errorf("SessionRef = %q, want %q kept", reattached.SessionRef, threadRef)
	}
	if len(stops) != 1 || stops[0] != attached.ID || len(starts) != 1 || starts[0] != attached.ID {
		t.Errorf("stops = %v starts = %v, want one of each for %s", stops, starts, attached.ID)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "projects")); !os.IsNotExist(err) {
		t.Errorf("Claude projects dir created for a Codex thread (stat err = %v)", err)
	}
}

// A live CLI appends under the old slug until it exits, so the sweep must
// not move a live session's transcript; the restart's start settles it once
// the old process is stopped. The seams model that: the stop appends the
// CLI's exit records, the start runs the settle the real start runs first.
// The root-slug transcript must then hold the exit records, with no second
// copy left under the dead slug.
func TestReconcileProjectWorktreesLeavesLiveSessionTranscriptToTheRestart(t *testing.T) {
	f := newWatchFixture(t)
	worktree := f.worktree(t, "feature-live-transcript")
	home := t.TempDir()
	t.Setenv("HOME", home)
	const sessionID = "0192cccc-live-transcript"
	attached := f.thread(t, "thread-live-transcript", worktree, "feature-live-transcript")
	attached.SessionRef = sessionID
	if err := f.app.store.UpdateThread(attached); err != nil {
		t.Fatalf("UpdateThread(attached): %v", err)
	}
	src := writeClaudeProjectSession(t, home, worktree, sessionID, "{\"type\":\"user\"}\n")
	f.app.sessionManager().put(attached.ID, session{Provider: string(provider.Claude), Token: "token-live-transcript"})
	var movedBeforeStop bool
	f.app.stopSessionFn = func(string) error {
		if _, err := os.Stat(src); err != nil {
			movedBeforeStop = true
		}
		file, err := os.OpenFile(src, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = file.WriteString("{\"type\":\"cost-state\"}\n")
		return err
	}
	f.app.startSessionFn = func(id string) error {
		row, err := f.app.store.GetThread(id)
		if err != nil {
			return err
		}
		f.app.settleClaudeTranscriptForWorkspace(row)
		return nil
	}

	testutil.RunGit(t, f.repo, "worktree", "remove", "--force", worktree)
	f.app.reconcileProjectWorktrees(f.repo)

	f.assertAtRoot(t, attached.ID)
	if movedBeforeStop {
		t.Fatal("the sweep moved a live session's transcript before the process stopped")
	}
	dest := filepath.Join(home, ".claude", "projects", claudeProjectSlugForTest(t, f.repo), sessionID+".jsonl")
	content, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("transcript not settled under the root slug after the restart: %v", err)
	}
	if !strings.Contains(string(content), "cost-state") {
		t.Errorf("the exit records did not follow the transcript to the root slug: %q", content)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("a copy remains under the dead slug: err=%v", err)
	}
}
