package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/triage"
)

// The provider-driven checkout move (EnterWorktree / ExitWorktree) lands on
// the thread row without a restart and without the idle gate. These tests
// drive the session event handler exactly as the read loop would and read
// the row back, because the follow runs on its own goroutine.

type followFixture struct {
	app     *App
	repo    string
	project store.Project
	thread  store.Thread
	rows    *threadRowRecorder
}

// threadRowRecorder collects `thread:updated` broadcasts so a test can prove
// the row reached every client, not only the store.
type threadRowRecorder struct {
	mu     sync.Mutex
	events []triage.ThreadUpdateEvent
}

func (r *threadRowRecorder) hook(name string, data any) {
	if name != "thread:updated" {
		return
	}
	evt, ok := data.(triage.ThreadUpdateEvent)
	if !ok {
		return
	}
	r.mu.Lock()
	r.events = append(r.events, evt)
	r.mu.Unlock()
}

func (r *threadRowRecorder) fullRowsFor(threadID string) []store.Thread {
	r.mu.Lock()
	defer r.mu.Unlock()
	var rows []store.Thread
	for _, evt := range r.events {
		if evt.Action == triage.ThreadActionFull && evt.Thread != nil && evt.Thread.ID == threadID {
			rows = append(rows, *evt.Thread)
		}
	}
	return rows
}

func newFollowFixture(t *testing.T, threadID string) followFixture {
	t.Helper()
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread(threadID)
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	thread.Provider = string(provider.Claude)
	thread.Branch = "main"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	rows := &threadRowRecorder{}
	app.testEmitHook = rows.hook
	return followFixture{app: app, repo: repo, project: project, thread: thread, rows: rows}
}

// cliWorktree cuts the checkout the CLI's EnterWorktree would: a linked
// worktree at <repo>/.claude/worktrees/<name> on branch worktree-<name>.
func (f followFixture) cliWorktree(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(f.repo, ".claude", "worktrees", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir worktrees: %v", err)
	}
	testutil.RunGit(t, f.repo, "worktree", "add", "-b", "worktree-"+name, path)
	return path
}

func (f followFixture) send(t *testing.T, token string, change provider.WorkspaceChangeMeta) {
	t.Helper()
	meta, err := json.Marshal(change)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	handler := f.app.sessionEventHandler(f.thread.ID, token, string(provider.Claude))
	handler(provider.ProviderEvent{
		Kind:      provider.EventWorkspaceChanged,
		ThreadID:  f.thread.ID,
		ItemID:    "tool-wt-1",
		Meta:      meta,
		Timestamp: time.Now(),
	})
}

func (f followFixture) waitRow(t *testing.T, threadID string, want func(store.Thread) bool) store.Thread {
	t.Helper()
	var row store.Thread
	waitUntil(t, 5*time.Second, func() bool {
		got, err := f.app.store.GetThread(threadID)
		if err != nil {
			return false
		}
		row = got
		return want(got)
	})
	return row
}

func TestFollowEnterWorktreeMovesThreadRowWithoutRestart(t *testing.T) {
	f := newFollowFixture(t, "thread-follow-enter")
	worktree := f.cliWorktree(t, "feature-x")
	// A live session under this token must survive the move untouched.
	f.app.sessionManager().put(f.thread.ID, session{Provider: string(provider.Claude), Token: "token-live"})

	f.send(t, "token-live", provider.WorkspaceChangeMeta{
		Tool: "EnterWorktree", Cwd: worktree, WorktreePath: worktree, Branch: "worktree-feature-x",
	})

	row := f.waitRow(t, f.thread.ID, func(r store.Thread) bool { return samePath(r.WorkspacePath, worktree) })
	if !samePath(row.WorktreePath, worktree) {
		t.Errorf("WorktreePath = %q, want %q", row.WorktreePath, worktree)
	}
	if row.Branch != "worktree-feature-x" {
		t.Errorf("Branch = %q, want worktree-feature-x", row.Branch)
	}
	if row.UpdatedAt != f.thread.UpdatedAt {
		t.Errorf("UpdatedAt bumped by a system-driven move: %d -> %d", f.thread.UpdatedAt, row.UpdatedAt)
	}
	if current, ok := f.app.sessionManager().get(f.thread.ID); !ok || current.Token != "token-live" {
		t.Errorf("live session disturbed by the follow: present=%v token=%q", ok, current.Token)
	}
	waitUntil(t, 5*time.Second, func() bool { return len(f.rows.fullRowsFor(f.thread.ID)) > 0 })
	broadcast := f.rows.fullRowsFor(f.thread.ID)[0]
	if !samePath(broadcast.WorkspacePath, worktree) || broadcast.Branch != "worktree-feature-x" {
		t.Errorf("broadcast row = %+v, want the moved workspace", broadcast)
	}
}

func TestFollowExitWorktreeKeepReturnsThreadToProjectRoot(t *testing.T) {
	f := newFollowFixture(t, "thread-follow-exit-keep")
	worktree := f.cliWorktree(t, "feature-y")
	moved := f.thread
	moved.WorkspacePath, moved.WorktreePath, moved.Branch = worktree, worktree, "worktree-feature-y"
	if err := f.app.store.UpdateThread(moved); err != nil {
		t.Fatalf("UpdateThread(moved): %v", err)
	}

	f.send(t, "token-live", provider.WorkspaceChangeMeta{
		Tool: "ExitWorktree", Cwd: f.repo, WorktreePath: worktree, Branch: "worktree-feature-y",
	})

	row := f.waitRow(t, f.thread.ID, func(r store.Thread) bool { return samePath(r.WorkspacePath, f.repo) })
	if row.WorktreePath != "" {
		t.Errorf("WorktreePath = %q, want empty at the project root", row.WorktreePath)
	}
	if row.Branch != "main" {
		t.Errorf("Branch = %q, want main", row.Branch)
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Errorf("keep must leave the worktree on disk: %v", err)
	}
}

// ExitWorktree remove: the CLI already deleted the checkout. The exiting
// thread returns to the root, and any OTHER thread attached to the dead
// worktree is reattached exactly as a user-driven removal would do.
func TestFollowExitWorktreeRemoveReattachesSiblingThreads(t *testing.T) {
	f := newFollowFixture(t, "thread-follow-exit-remove")
	worktree := f.cliWorktree(t, "feature-z")
	moved := f.thread
	moved.WorkspacePath, moved.WorktreePath, moved.Branch = worktree, worktree, "worktree-feature-z"
	if err := f.app.store.UpdateThread(moved); err != nil {
		t.Fatalf("UpdateThread(moved): %v", err)
	}
	sibling := testThread("thread-follow-sibling")
	sibling.ProjectID = f.project.ID
	sibling.WorkspacePath, sibling.WorktreePath, sibling.Branch = worktree, worktree, "worktree-feature-z"
	sibling.UpdatedAt = 1_700_000_000_000
	if err := f.app.store.CreateThread(sibling); err != nil {
		t.Fatalf("CreateThread(sibling): %v", err)
	}
	// The CLI's remove half: directory and branch are gone before the
	// result reaches AO.
	testutil.RunGit(t, f.repo, "worktree", "remove", "--force", worktree)
	testutil.RunGit(t, f.repo, "branch", "-D", "worktree-feature-z")

	f.send(t, "token-live", provider.WorkspaceChangeMeta{
		Tool: "ExitWorktree", Cwd: f.repo, WorktreePath: worktree, Branch: "worktree-feature-z", RemovedWorktree: true,
	})

	f.waitRow(t, f.thread.ID, func(r store.Thread) bool { return samePath(r.WorkspacePath, f.repo) && r.WorktreePath == "" })
	reattached := f.waitRow(t, sibling.ID, func(r store.Thread) bool { return samePath(r.WorkspacePath, f.repo) })
	if reattached.WorktreePath != "" || reattached.Branch != "main" {
		t.Errorf("sibling row = %+v, want project root on main", reattached)
	}
	if reattached.UpdatedAt != sibling.UpdatedAt {
		t.Errorf("sibling UpdatedAt bumped by the sweep: %d -> %d", sibling.UpdatedAt, reattached.UpdatedAt)
	}
	waitUntil(t, 5*time.Second, func() bool { return len(f.rows.fullRowsFor(sibling.ID)) > 0 })
}

// A directory the row cannot represent (not the project root, not one of
// its worktrees) is refused with an error on the thread; the row is left
// pointing where it was rather than at a path git status cannot read.
func TestFollowRefusesDirectoryOutsideProjectWorktrees(t *testing.T) {
	f := newFollowFixture(t, "thread-follow-refuse")
	errors := collectErrorItemUpserts(t, f.app, 4)
	elsewhere := t.TempDir()

	f.send(t, "token-live", provider.WorkspaceChangeMeta{Tool: "EnterWorktree", Cwd: elsewhere, WorktreePath: elsewhere})

	select {
	case item := <-errors:
		if !strings.Contains(item.Summary, elsewhere) || !strings.Contains(item.Summary, "still shows") {
			t.Errorf("error item = %q, want the refused path and the retained workspace", item.Summary)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no error item reached the thread")
	}
	row, err := f.app.store.GetThread(f.thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if !samePath(row.WorkspacePath, f.repo) || row.WorktreePath != "" {
		t.Errorf("row moved to an unrepresentable directory: %+v", row)
	}
	if n := len(f.rows.fullRowsFor(f.thread.ID)); n != 0 {
		t.Errorf("refused move broadcast %d row updates, want none", n)
	}
}

// A result from a session that has since been replaced describes a process
// that no longer exists; the replacement launched from the row's workspace
// is the authority.
func TestFollowIgnoresResultFromReplacedSession(t *testing.T) {
	f := newFollowFixture(t, "thread-follow-stale")
	worktree := f.cliWorktree(t, "feature-stale")
	f.app.sessionManager().put(f.thread.ID, session{Provider: string(provider.Claude), Token: "token-new"})

	f.send(t, "token-old", provider.WorkspaceChangeMeta{Tool: "EnterWorktree", Cwd: worktree, WorktreePath: worktree})

	// Give the goroutine a chance to misbehave, then prove it did not.
	time.Sleep(200 * time.Millisecond)
	row, err := f.app.store.GetThread(f.thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if !samePath(row.WorkspacePath, f.repo) {
		t.Errorf("stale session moved the row: %+v", row)
	}
}

// An event whose meta cannot name the directory is a wire defect surfaced
// on the thread, never a silent no-op.
func TestFollowReportsUnreadableMeta(t *testing.T) {
	f := newFollowFixture(t, "thread-follow-badmeta")
	errors := collectErrorItemUpserts(t, f.app, 4)
	handler := f.app.sessionEventHandler(f.thread.ID, "token-live", string(provider.Claude))
	handler(provider.ProviderEvent{Kind: provider.EventWorkspaceChanged, ThreadID: f.thread.ID, Meta: json.RawMessage(`{"tool":"EnterWorktree"}`), Timestamp: time.Now()})

	select {
	case item := <-errors:
		if !strings.Contains(item.Summary, "could not read the new path") {
			t.Errorf("error item = %q", item.Summary)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no error item reached the thread")
	}
}

// A transcript filed under another workspace's slug is moved under the
// row's workspace before `--resume` runs from there.
func TestSettleClaudeTranscriptMovesFileUnderCurrentWorkspaceSlug(t *testing.T) {
	app := newTestAppWithStore(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	launchDir := t.TempDir()
	worktree := t.TempDir()
	const sessionID = "0192aaaa-settle-test"

	src := writeClaudeProjectSession(t, home, launchDir, sessionID, "{\"type\":\"user\"}\n")
	thread := testThread("thread-settle")
	thread.Provider = string(provider.Claude)
	thread.SessionRef = sessionID
	thread.WorkspacePath = worktree
	thread.WorktreePath = worktree

	app.settleClaudeTranscriptForWorkspace(thread)

	want := filepath.Join(home, ".claude", "projects", claudeProjectSlugForTest(t, worktree), sessionID+".jsonl")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("transcript not settled under the workspace slug %q: %v", want, err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source transcript should be purged after the move, stat err = %v", err)
	}
	if located, err := sessionfork.LocateSessionFile(testProviderProjectsDir(t), sessionID, worktree); err != nil || !samePath(located, want) {
		t.Errorf("LocateSessionFile = %q, %v; want %q", located, err, want)
	}
	// Already in place: a second settle is a no-op that keeps the file.
	app.settleClaudeTranscriptForWorkspace(thread)
	if _, err := os.Stat(want); err != nil {
		t.Errorf("second settle removed the transcript: %v", err)
	}
}

func TestSettleClaudeTranscriptLeavesForkSourceAndOtherProvidersAlone(t *testing.T) {
	app := newTestAppWithStore(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	launchDir := t.TempDir()
	worktree := t.TempDir()
	const sessionID = "0192bbbb-settle-fork"
	src := writeClaudeProjectSession(t, home, launchDir, sessionID, "{\"type\":\"user\"}\n")

	pendingFork := testThread("thread-settle-fork")
	pendingFork.Provider = string(provider.Claude)
	pendingFork.PendingForkRef = sessionID
	pendingFork.WorkspacePath = worktree
	app.settleClaudeTranscriptForWorkspace(pendingFork)

	codex := testThread("thread-settle-codex")
	codex.SessionRef = sessionID
	codex.WorkspacePath = worktree
	app.settleClaudeTranscriptForWorkspace(codex)

	if _, err := os.Stat(src); err != nil {
		t.Errorf("fork source / foreign provider transcript moved: %v", err)
	}
}

// Session start is where the settle runs: a Claude thread whose transcript
// sits under another slug gets it moved before the process launches.
func TestStartSessionSettlesTranscriptUnderWorkspace(t *testing.T) {
	app, _ := setupE2EApp(t)
	home := os.Getenv("HOME")
	launchDir := t.TempDir()
	workspace := t.TempDir()
	const sessionID = "0192cccc-settle-start"
	src := writeClaudeProjectSession(t, home, launchDir, sessionID, "{\"type\":\"user\"}\n")

	thread := e2eThread("thread-settle-start", string(provider.Claude), workspace)
	thread.SessionRef = sessionID
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), [][]string{{}})
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}

	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = app.StopSession(thread.ID) })

	want := filepath.Join(home, ".claude", "projects", claudeProjectSlugForTest(t, workspace), sessionID+".jsonl")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("transcript not settled at session start: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source transcript left behind: stat err = %v", err)
	}
}

// Removing a Claude-entered worktree through AO: the CLI leaves it LOCKED,
// so removal has to lift the lock, then the ordinary reattach moves the row
// back to the project root.
func TestGitRemoveWorktreeRemovesLockedProviderWorktree(t *testing.T) {
	f := newFollowFixture(t, "thread-remove-locked")
	worktree := f.cliWorktree(t, "feature-locked")
	testutil.RunGit(t, f.repo, "worktree", "lock", "--", worktree)
	moved := f.thread
	moved.WorkspacePath, moved.WorktreePath, moved.Branch = worktree, worktree, "worktree-feature-locked"
	if err := f.app.store.UpdateThread(moved); err != nil {
		t.Fatalf("UpdateThread(moved): %v", err)
	}

	if err := f.app.GitRemoveWorktree(f.thread.ID); err != nil {
		t.Fatalf("GitRemoveWorktree on the CLI's locked worktree: %v", err)
	}

	row, err := f.app.store.GetThread(f.thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !samePath(row.WorkspacePath, f.repo) || row.WorktreePath != "" || row.Branch != "main" {
		t.Errorf("row after removal = %+v, want project root on main", row)
	}
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Errorf("worktree still on disk: %v", err)
	}
}
