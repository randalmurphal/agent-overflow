package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/forgefake"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/worktreesetup"
)

// A moved thread's notice names the worktree by its directory and the
// project root as Base, never by path; a removal the user asked for says
// nothing.
func TestWorktreeRemovalNoticeNamesNoPaths(t *testing.T) {
	t.Parallel()
	base := worktreeRemoval{project: "/home/me/src/widgets", path: "/home/me/src/widgets/.worktrees/fix-pr-diff-fetch"}
	cases := []struct {
		name     string
		cause    string
		mergedPR string
		stopped  bool
		text     string
		kind     string
	}{
		{name: "merged", mergedPR: "PR #90", stopped: true, kind: noticeKindWorktreeCleanup,
			text: "PR #90 merged. Removed worktree fix-pr-diff-fetch; this thread now runs in Base."},
		{name: "merged request", mergedPR: "MR !12", kind: noticeKindWorktreeCleanup,
			text: "MR !12 merged. Removed worktree fix-pr-diff-fetch; this thread now runs in Base."},
		{name: "outside, stopped", cause: removedOutsideCause, stopped: true, kind: noticeKindWarning,
			text: "Worktree fix-pr-diff-fetch was removed outside Agent Overflow. Its session was stopped, and this thread now runs in Base; the next message starts it there."},
		{name: "outside, idle", cause: removedOutsideCause, kind: noticeKindWarning,
			text: "Worktree fix-pr-diff-fetch was removed outside Agent Overflow. This thread now runs in Base."},
		{name: "by Claude", cause: removedByClaudeCause, kind: noticeKindWarning,
			text: "Worktree fix-pr-diff-fetch was removed by Claude in another thread. This thread now runs in Base."},
	}
	for _, tc := range cases {
		removal := base
		removal.cause, removal.mergedPR = tc.cause, tc.mergedPR
		text, kind, ok := removal.notice(tc.stopped)
		if !ok || text != tc.text || kind != tc.kind {
			t.Errorf("%s: notice = (%q, %q, %v), want (%q, %q, true)", tc.name, text, kind, ok, tc.text, tc.kind)
		}
	}
	if text, _, ok := base.notice(true); ok {
		t.Errorf("a requested removal posted %q", text)
	}
}

// mergeCleanupThread creates a thread running in a new linked worktree of
// repo on branch, and returns it with the worktree's path.
func mergeCleanupThread(t *testing.T, app *App, repo, branch string) (store.Thread, string) {
	t.Helper()
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace: %v", err)
	}
	owner := testThread("merge-owner-" + strings.ReplaceAll(branch, "/", "-"))
	owner.ProjectID = project.ID
	owner.WorkspacePath = repo
	if err := app.store.CreateThread(owner); err != nil {
		t.Fatalf("CreateThread(owner): %v", err)
	}
	path, err := app.GitCreateWorktree(owner.ID, branch)
	if err != nil {
		t.Fatalf("GitCreateWorktree: %v", err)
	}
	thread := testThread("merge-thread-" + strings.ReplaceAll(branch, "/", "-"))
	thread.ProjectID = project.ID
	thread.WorkspacePath = path
	thread.WorktreePath = path
	thread.Branch = branch
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	return thread, path
}

// The cleanup waits for every kind of work a thread can have pending, each
// named by its own reason, not only a running turn.
func TestMergedPRWorktreeBusyWaitsForPendingWork(t *testing.T) {
	t.Parallel()
	app, _ := newAppForFlushQueueRPC(t)
	repo := initMainGitRepo(t)

	idle, idlePath := mergeCleanupThread(t, app, repo, "feature/idle")
	if err := app.mergedPRWorktreeBusy(idlePath, []string{idle.ID}); err != nil {
		t.Fatalf("an idle worktree reported busy: %v", err)
	}

	queued, queuedPath := mergeCleanupThread(t, app, repo, "feature/queued")
	if _, err := app.RegisterQueueItem(context.Background(), queued.ID, "after this", SendMessageOptions{}); err != nil {
		t.Fatalf("RegisterQueueItem: %v", err)
	}
	if err := app.mergedPRWorktreeBusy(queuedPath, []string{queued.ID}); err == nil || !strings.Contains(err.Error(), "queued messages") {
		t.Fatalf("a queued send: got %v, want a queued-messages refusal", err)
	}

	asking, askingPath := mergeCleanupThread(t, app, repo, "feature/asking")
	meta, err := json.Marshal(provider.ApprovalRequest{RequestID: "approval-merge", ThreadID: asking.ID, ToolName: "Bash", Title: "Approve command"})
	if err != nil {
		t.Fatalf("marshal approval: %v", err)
	}
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventApprovalRequest, ThreadID: asking.ID, ItemID: "approval-merge", Meta: meta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("approval request: %v", err)
	}
	if err := app.mergedPRWorktreeBusy(askingPath, []string{asking.ID}); err == nil || !strings.Contains(err.Error(), "waiting for an answer") {
		t.Fatalf("a pending approval: got %v, want a waiting-for-an-answer refusal", err)
	}
}

// A setup recipe still running in the worktree holds the cleanup: removing
// the worktree would cancel it.
func TestMergedPRWorktreeBusyWaitsForASetupRecipe(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	repo := initMainGitRepo(t)
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace: %v", err)
	}
	if _, _, err := app.store.UpdateProjectWorktreeSetup(project.ID, &worktreesetup.Config{Run: [][]string{{"sleep", "30"}}}); err != nil {
		t.Fatalf("UpdateProjectWorktreeSetup: %v", err)
	}
	thread := testThread("merge-thread-setup")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	updated, err := app.PrepareThreadWorktree(thread.ID, "", "feature/setup-running", false)
	if err != nil {
		t.Fatalf("PrepareThreadWorktree: %v", err)
	}
	t.Cleanup(func() {
		app.cancelWorktreeSetupsForPath(updated.WorktreePath)
		_ = app.gitCore().RemoveWorktreeForce(repo, updated.WorktreePath, true)
	})
	if err := app.mergedPRWorktreeBusy(updated.WorktreePath, []string{thread.ID}); err == nil || !strings.Contains(err.Error(), "running its setup") {
		t.Fatalf("a running setup recipe: got %v, want a running-setup refusal", err)
	}
	app.cancelWorktreeSetupsForPath(updated.WorktreePath)
	if err := app.mergedPRWorktreeBusy(updated.WorktreePath, []string{thread.ID}); err != nil {
		t.Fatalf("after the recipe was cancelled: %v", err)
	}
}

const mergeForgeToken = "merge-cleanup-token"

// mergeCase is a thread in a linked worktree whose tracked PR #5 merged a
// minute ago, an App with triage (so notices land) whose git core reads a
// fake forge, and that forge, which counts its GraphQL requests and runs
// an armed hook before answering the next one: a change landing while the
// cleanup waits on the forge.
type mergeCase struct {
	app      *App
	engine   *forgefake.Engine
	requests atomic.Int32
	hook     atomic.Pointer[func()]
	repo     string
	thread   store.Thread
	path     string
	pr       store.ThreadPullRequest
}

func newMergeCase(t *testing.T, branch string) *mergeCase {
	t.Helper()
	app, _ := newAppForFlushQueueRPC(t)
	repo := initMainGitRepo(t)
	thread, path := mergeCleanupThread(t, app, repo, branch)
	// Set after the worktree exists: nothing below fetches.
	_ = testutil.RunGitAllowError(repo, "remote", "remove", "origin")
	testutil.RunGit(t, repo, "remote", "add", "origin", "https://github.com/acme/widgets.git")
	m := &mergeCase{app: app, repo: repo, thread: thread, path: path}
	m.engine = forgefake.New(forgefake.Options{APIToken: mergeForgeToken})
	m.seed(t, "merged")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/github/graphql" {
			m.requests.Add(1)
			if hook := m.hook.Swap(nil); hook != nil {
				(*hook)()
			}
		}
		m.engine.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	svc, err := forgeapi.New(forgeapi.Options{Version: "test", Isolated: &forgeapi.Isolated{BaseURL: srv.URL, Token: mergeForgeToken}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	app.git = gitops.NewCore(gitops.WithForgeAPI(svc))
	m.pr = store.ThreadPullRequest{
		ThreadID: thread.ID, Forge: "github", Host: "github.com", Project: "acme/widgets", Number: 5,
		URL: "https://github.com/acme/widgets/pull/5", NodeID: "PR_five", Branch: branch,
		WorktreePath: path, SeenOpenAt: time.Now().Add(-time.Hour).UnixMilli(),
	}
	m.track(t, thread.ID)
	if _, err := app.store.MarkThreadPullRequestsMerged("github", "github.com", "acme/widgets", 5, time.Now().Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	return m
}

// seed puts PR #5 on the forge in state.
func (m *mergeCase) seed(t *testing.T, state string) {
	t.Helper()
	if _, err := m.engine.Seed(forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "acme/widgets", Pulls: []forgefake.Pull{
		{Number: 5, Title: "Work", HeadRef: m.branch(), State: state, NodeID: "PR_five"},
	}}}}); err != nil {
		t.Fatal(err)
	}
}

func (m *mergeCase) branch() string { return m.thread.Branch }

// track records PR #5 as threadID's open pull request.
func (m *mergeCase) track(t *testing.T, threadID string) {
	t.Helper()
	pr := m.pr
	pr.ThreadID = threadID
	if _, err := m.app.store.RecordOpenThreadPullRequest(pr); err != nil {
		t.Fatal(err)
	}
}

// during runs fn before the forge answers its next request.
func (m *mergeCase) during(fn func()) { m.hook.Store(&fn) }

func (m *mergeCase) pass() { m.app.mergedPRActPass(context.Background()) }

// due ends every backoff, as time passing would.
func (m *mergeCase) due() {
	m.app.mergedPRs.lookups = nil
	m.app.mergedPRs.removals = nil
}

func (m *mergeCase) row(t *testing.T, threadID string) store.ThreadPullRequest {
	t.Helper()
	row, ok, err := m.app.store.GetThreadPullRequest(threadID)
	if err != nil || !ok {
		t.Fatalf("GetThreadPullRequest(%s): ok=%v err=%v", threadID, ok, err)
	}
	return row
}

func (m *mergeCase) expectOutcome(t *testing.T, outcome string) {
	t.Helper()
	if row := m.row(t, m.thread.ID); row.Outcome != outcome || (outcome == "") != (row.HandledAt == 0) {
		t.Fatalf("row = handled %d outcome %q, want %q", row.HandledAt, row.Outcome, outcome)
	}
}

func (m *mergeCase) expectWorktree(t *testing.T, present bool) {
	t.Helper()
	_, statErr := os.Stat(m.path)
	if _, listed, err := m.app.findWorktree(m.repo, m.path); err != nil || listed != present || (statErr == nil) != present {
		t.Fatalf("worktree listed=%v exists=%v (err %v), want %v", listed, statErr == nil, err, present)
	}
}

// timelineRows returns the summaries of threadID's rows of kind, and for a
// notification, of notice kind toolName.
func timelineRows(t *testing.T, st *store.Store, threadID, kind, toolName string) []string {
	t.Helper()
	items, err := st.ListItems(threadID)
	if err != nil {
		t.Fatalf("ListItems(%s): %v", threadID, err)
	}
	var out []string
	for _, item := range items {
		if item.Kind == kind && (toolName == "" || item.ToolName == toolName) {
			out = append(out, item.Summary)
		}
	}
	return out
}

func (m *mergeCase) removedNotice() string {
	return "PR #5 merged. Removed worktree " + filepath.Base(m.path) + "; this thread now runs in Base."
}

// A merged PR of an idle, clean worktree: one open-PR lookup, then the
// worktree goes, the thread moves to the root and is told once.
func TestMergedPRCleanupRemovesAfterOneLookup(t *testing.T) {
	t.Parallel()
	m := newMergeCase(t, "feature/plain")
	m.pass()
	m.expectOutcome(t, store.PROutcomeRemoved)
	m.expectWorktree(t, false)
	if n := m.requests.Load(); n != 1 {
		t.Fatalf("forge requests = %d, want the one open-PR lookup", n)
	}
	if got := timelineRows(t, m.app.store, m.thread.ID, "notification", noticeKindWorktreeCleanup); len(got) != 1 || got[0] != m.removedNotice() {
		t.Fatalf("notices = %q", got)
	}
	if moved, err := m.app.store.GetThread(m.thread.ID); err != nil || !samePath(moved.WorkspacePath, m.repo) {
		t.Fatalf("thread workspace = %q (%v), want the root", moved.WorkspacePath, err)
	}
}

// What changes while the cleanup waits on the forge is checked again with
// the threads locked, immediately before the removal.
func TestMergedPRCleanupRechecksUnderTheLocks(t *testing.T) {
	t.Parallel()
	t.Run("a user message after the merge keeps the worktree", func(t *testing.T) {
		t.Parallel()
		m := newMergeCase(t, "feature/messaged")
		m.during(func() {
			now := time.Now().UnixMilli()
			if err := m.app.store.InsertItem(store.Item{
				ID: "late-message", ThreadID: m.thread.ID, TurnIndex: 1, Kind: "user_text", Role: "user",
				Status: "completed", Summary: "one more thing", CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Errorf("InsertItem: %v", err)
			}
		})
		m.pass()
		m.expectOutcome(t, store.PROutcomeSkipped)
		m.expectWorktree(t, true)
	})
	t.Run("another branch keeps the worktree", func(t *testing.T) {
		t.Parallel()
		m := newMergeCase(t, "feature/switched")
		m.during(func() { testutil.RunGit(t, m.path, "checkout", "-q", "-b", "feature/next") })
		m.pass()
		m.expectOutcome(t, store.PROutcomeSkipped)
		m.expectWorktree(t, true)
	})
	t.Run("a thread that joins is checked before the removal", func(t *testing.T) {
		t.Parallel()
		m := newMergeCase(t, "feature/joined")
		joiner := testThread("merge-joiner")
		joiner.ProjectID = m.thread.ProjectID
		joiner.WorkspacePath, joiner.WorktreePath, joiner.Branch = m.path, m.path, m.thread.Branch
		m.during(func() {
			if err := m.app.store.CreateThread(joiner); err != nil {
				t.Errorf("CreateThread: %v", err)
			}
		})
		m.pass()
		m.expectOutcome(t, "")
		m.expectWorktree(t, true)
		m.due()
		m.pass()
		m.expectOutcome(t, store.PROutcomeRemoved)
		m.expectWorktree(t, false)
		for _, id := range []string{m.thread.ID, joiner.ID} {
			if moved, err := m.app.store.GetThread(id); err != nil || !samePath(moved.WorkspacePath, m.repo) {
				t.Fatalf("thread %s workspace = %q (%v), want the root", id, moved.WorkspacePath, err)
			}
		}
	})
}

// A worktree removed outside the app while the cleanup waits on the forge
// is already gone: no error row, nothing failed; the registry watch moves
// the thread.
func TestMergedPRWorktreeRemovedOutsideMeanwhileIsGone(t *testing.T) {
	t.Parallel()
	m := newMergeCase(t, "feature/raced")
	m.during(func() { testutil.RunGit(t, m.repo, "worktree", "remove", "--force", m.path) })
	m.pass()
	m.due()
	m.pass()
	m.expectOutcome(t, store.PROutcomeGone)
	if rows := timelineRows(t, m.app.store, m.thread.ID, "error", ""); len(rows) != 0 {
		t.Fatalf("error rows = %q, want none", rows)
	}
	if rows := timelineRows(t, m.app.store, m.thread.ID, "notification", ""); len(rows) != 0 {
		t.Fatalf("notices = %q, want none from the cleanup", rows)
	}
}

// A removal that fails because the worktree went outside the app in the
// moment before git ran is gone, with no error row; one git completed
// whose reaction then failed is removed, and its threads are told.
func TestMergedPRRemovalErrorsAfterTheChecks(t *testing.T) {
	t.Parallel()
	m := newMergeCase(t, "feature/vanished")
	site := &mergedPRSite{path: m.path, project: m.repo, projectID: m.thread.ProjectID, threadIDs: []string{m.thread.ID}}
	testutil.RunGit(t, m.repo, "worktree", "remove", "--force", m.path)
	refused := errors.New("git worktree remove failed: fatal: '" + m.path + "' is not a working tree")
	if got := m.app.mergedPRRemovalFailed(m.pr, site, []string{m.thread.ID}, refused, time.Now()); got != mergedPRGone {
		t.Fatalf("outcome = %v, want gone", got)
	}
	if rows := timelineRows(t, m.app.store, m.thread.ID, "error", ""); len(rows) != 0 {
		t.Fatalf("error rows = %q, want none", rows)
	}
	followUp := &errWorktreeRemovedFollowUp{err: errors.New("thread x update failed")}
	if got := m.app.mergedPRRemovalFailed(m.pr, site, []string{m.thread.ID}, followUp, time.Now()); got != mergedPRRemoved {
		t.Fatalf("outcome = %v, want removed", got)
	}
	if rows := timelineRows(t, m.app.store, m.thread.ID, "error", ""); len(rows) != 1 || strings.Contains(rows[0], m.path) {
		t.Fatalf("error rows = %q, want one naming no path", rows)
	}
}

// git refusing to remove a clean worktree is told once with the amber
// notice, retried after a backoff, and the grey notice follows when a
// retry succeeds.
func TestMergedPRRemovalFailureIsToldOnceAndRetried(t *testing.T) {
	t.Parallel()
	m := newMergeCase(t, "feature/refused")
	vendored := testutil.InitGitRepo(t)
	testutil.RunGit(t, m.path, "-c", "protocol.file.allow=always", "submodule", "--quiet", "add", vendored, "vendored")
	testutil.RunGit(t, m.path, "commit", "-q", "-m", "vendor a submodule")
	kept := "PR #5 merged. Kept worktree " + filepath.Base(m.path) + ": removing it failed."
	warnings := func() []string { return timelineRows(t, m.app.store, m.thread.ID, "notification", noticeKindWarning) }

	m.pass()
	m.expectOutcome(t, "")
	m.expectWorktree(t, true)
	if got := warnings(); len(got) != 1 || got[0] != kept {
		t.Fatalf("warnings = %q, want %q", got, kept)
	}
	if m.row(t, m.thread.ID).RemovalFailedAt == 0 {
		t.Fatal("the failure notice was not recorded on the row")
	}

	before := m.requests.Load()
	m.pass()
	if n := m.requests.Load() - before; n != 0 {
		t.Fatalf("a pass inside the backoff made %d forge requests", n)
	}
	m.due()
	m.pass()
	if n := m.requests.Load() - before; n != 1 {
		t.Fatalf("a due retry made %d forge requests, want 1", n)
	}
	m.expectOutcome(t, "")
	if got := warnings(); len(got) != 1 {
		t.Fatalf("a retry repeated the warning: %q", got)
	}

	// git keeps refusing while the worktree's admin directory holds the
	// submodule's repository.
	testutil.RunGit(t, m.path, "rm", "-q", "vendored")
	testutil.RunGit(t, m.path, "commit", "-q", "-m", "drop the submodule")
	gitDir := strings.TrimSpace(gitOutput(t, m.path, "rev-parse", "--absolute-git-dir"))
	if err := os.RemoveAll(filepath.Join(gitDir, "modules")); err != nil {
		t.Fatal(err)
	}
	m.due()
	m.pass()
	m.expectOutcome(t, store.PROutcomeRemoved)
	m.expectWorktree(t, false)
	if got := timelineRows(t, m.app.store, m.thread.ID, "notification", noticeKindWorktreeCleanup); len(got) != 1 || got[0] != m.removedNotice() {
		t.Fatalf("after the retry, notices = %q", got)
	}
}

// A failed open-PR lookup, and one where the forge still lists the merged
// PR as open, back off per repository instead of asking every pass.
func TestMergedPROpenLookupBacksOff(t *testing.T) {
	t.Parallel()
	for name, setup := range map[string]func(t *testing.T, m *mergeCase){
		"failed":  func(_ *testing.T, m *mergeCase) { m.engine.SetOffline(true) },
		"lagging": func(t *testing.T, m *mergeCase) { m.seed(t, "open") },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newMergeCase(t, "feature/backoff-"+name)
			setup(t, m)
			key := repositoryKey(m.pr)
			m.pass()
			first := m.app.mergedPRs.lookups[key]
			if first == nil || first.failures != 1 || m.requests.Load() == 0 {
				t.Fatalf("after one pass: backoff %+v, requests %d", first, m.requests.Load())
			}
			firstWait := time.Until(first.next)
			before := m.requests.Load()
			m.pass()
			if n := m.requests.Load() - before; n != 0 {
				t.Fatalf("a pass inside the backoff made %d requests", n)
			}
			first.next = time.Now()
			m.pass()
			second := m.app.mergedPRs.lookups[key]
			if second.failures != 2 || time.Until(second.next) <= firstWait {
				t.Fatalf("second failure: %+v, first wait %v", second, firstWait)
			}
			m.expectOutcome(t, "")
			m.expectWorktree(t, true)
		})
	}
}

// A merged PR whose worktree directory is already gone is done, with no
// forge request, and git's registry is left alone: the registry watch
// reconciles a vanished worktree, and pruning would drop others'.
func TestMergedPRWorktreeAlreadyGoneIsLeftToTheRegistryWatch(t *testing.T) {
	t.Parallel()
	m := newMergeCase(t, "feature/gone")
	if err := os.RemoveAll(m.path); err != nil {
		t.Fatal(err)
	}
	m.pass()
	m.expectOutcome(t, store.PROutcomeGone)
	if _, listed, err := m.app.findWorktree(m.repo, m.path); err != nil || !listed {
		t.Fatalf("the vanished worktree's registration went (listed=%v err=%v)", listed, err)
	}
	if n := m.requests.Load(); n != 0 {
		t.Fatalf("forge requests = %d, want none", n)
	}
}

// A stat failure other than "does not exist" is not "gone": the cleanup
// waits and judges the worktree again.
func TestMergedPRWorktreeStatErrorWaits(t *testing.T) {
	t.Parallel()
	m := newMergeCase(t, "feature/unstatable")
	parent := filepath.Dir(m.path)
	if err := os.Rename(parent, parent+"-aside"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(parent, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.pass()
	m.expectOutcome(t, "")
	if err := os.Remove(parent); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(parent+"-aside", parent); err != nil {
		t.Fatal(err)
	}
	m.pass()
	m.expectOutcome(t, store.PROutcomeRemoved)
}

// A thread whose project root cannot be read is not judged to have left
// its worktree: the PR waits instead of being skipped.
func TestMergedPRThreadWithUnknownProjectWaits(t *testing.T) {
	t.Parallel()
	m := newMergeCase(t, "feature/orphan")
	m.thread.ProjectID = ""
	if err := m.app.store.UpdateThread(m.thread); err != nil {
		t.Fatalf("UpdateThread: %v", err)
	}
	m.pass()
	m.expectOutcome(t, "")
	m.expectWorktree(t, true)
}

// The once-per-disappearance memo of a lookup that stopped finding the PR
// drops a thread that leaves its worktree, and a PR once handled.
func TestMergedPRNoneReadStaysBounded(t *testing.T) {
	t.Parallel()
	m := newMergeCase(t, "feature/memo")
	// Unmerged again, both threads tracking it, the forge says merged.
	if _, err := m.app.store.MarkThreadPullRequestsHandled("github", "github.com", "acme/widgets", 5, store.PROutcomeClosed, 1); err != nil {
		t.Fatal(err)
	}
	m.track(t, m.thread.ID)
	other := testThread("merge-memo-other")
	other.ProjectID = m.thread.ProjectID
	other.WorkspacePath, other.WorktreePath, other.Branch = m.path, m.path, m.thread.Branch
	if err := m.app.store.CreateThread(other); err != nil {
		t.Fatal(err)
	}
	m.track(t, other.ID)

	if _, err := m.app.handleOpenPRObservation(context.Background(), gitops.OpenPRObservation{Cwd: m.path, Branch: m.thread.Branch}); err != nil {
		t.Fatalf("handleOpenPRObservation: %v", err)
	}
	if n := len(m.app.mergedPRs.noneRead); n != 2 {
		t.Fatalf("memo = %v, want both threads", m.app.mergedPRs.noneRead)
	}
	// The first thread leaves; the act pass prunes it, then removes the
	// worktree for the other and handles the PR.
	left := m.thread
	left.WorkspacePath, left.WorktreePath = m.repo, ""
	if err := m.app.store.UpdateThread(left); err != nil {
		t.Fatal(err)
	}
	m.pass()
	if _, ok := m.app.mergedPRs.noneRead[m.thread.ID]; ok || len(m.app.mergedPRs.noneRead) != 1 {
		t.Fatalf("memo after the thread left = %v", m.app.mergedPRs.noneRead)
	}
	if row := m.row(t, other.ID); row.HandledAt == 0 {
		t.Fatalf("the PR was not handled: %+v", row)
	}
	m.pass()
	if n := len(m.app.mergedPRs.noneRead); n != 0 {
		t.Fatalf("memo after the PR was handled = %v", m.app.mergedPRs.noneRead)
	}
}

// unmerge makes PR #5 tracked and unmerged again, so only a state read can
// find its merge.
func (m *mergeCase) unmerge(t *testing.T) {
	t.Helper()
	if _, err := m.app.store.MarkThreadPullRequestsHandled("github", "github.com", "acme/widgets", 5, store.PROutcomeClosed, 1); err != nil {
		t.Fatal(err)
	}
	m.track(t, m.thread.ID)
	if row := m.row(t, m.thread.ID); row.MergedAt != 0 || row.HandledAt != 0 {
		t.Fatalf("row not re-armed: %+v", row)
	}
}

func (m *mergeCase) setBackgroundFetch(t *testing.T, on bool) {
	t.Helper()
	if _, err := m.app.UpdateSettings(context.Background(), map[string]any{"backgroundGitFetch": on}); err != nil {
		t.Fatal(err)
	}
}

// awaitHandled waits for PR #5's row to be handled and returns it.
func (m *mergeCase) awaitHandled(t *testing.T) store.ThreadPullRequest {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		row := m.row(t, m.thread.ID)
		if row.HandledAt != 0 {
			return row
		}
		if time.Now().After(deadline) {
			t.Fatalf("PR #5 was never handled: %+v", row)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The poll runs only while Settings.BackgroundGitFetch is on. Off, the
// cleanup reads nothing on its own; turning it on starts the poll, which
// picks up the PR tracked meanwhile; off again stops and joins it; once
// the cleanup stopped, the setting starts nothing.
func TestMergedPRPollFollowsTheBackgroundFetchSetting(t *testing.T) {
	t.Parallel()
	m := newMergeCase(t, "feature/setting")
	c := &m.app.mergedPRs
	c.interval = 20 * time.Millisecond
	m.app.maintenance.firstReadsFallback = time.Millisecond
	m.unmerge(t)
	m.setBackgroundFetch(t, false)
	m.app.startMergedPRCleanup()
	t.Cleanup(m.app.stopMergedPRCleanup)

	time.Sleep(15 * c.interval)
	if c.mergedPRPollRunning() {
		t.Fatal("the poll runs with the setting off")
	}
	if n := m.requests.Load(); n != 0 {
		t.Fatalf("the cleanup made %d forge requests with the setting off", n)
	}
	if row := m.row(t, m.thread.ID); row.MergedAt != 0 || row.HandledAt != 0 {
		t.Fatalf("row changed with the setting off: %+v", row)
	}

	m.setBackgroundFetch(t, true)
	if !c.mergedPRPollRunning() {
		t.Fatal("turning the setting on did not start the poll")
	}
	if row := m.awaitHandled(t); row.Outcome != store.PROutcomeRemoved {
		t.Fatalf("outcome = %q, want removed", row.Outcome)
	}

	c.pollMu.Lock()
	pollDone := c.pollDone
	c.pollMu.Unlock()
	m.setBackgroundFetch(t, false)
	if c.mergedPRPollRunning() {
		t.Fatal("turning the setting off did not stop the poll")
	}
	select {
	case <-pollDone:
	default:
		t.Fatal("turning the setting off returned before the poll goroutine ended")
	}
	m.app.stopMergedPRCleanup()
	m.setBackgroundFetch(t, true)
	if c.mergedPRPollRunning() {
		t.Fatal("the setting started the poll of a stopped cleanup")
	}
}

// A pause (HarnessReset's) stops and joins the poll and the worker and
// makes no request while paused; resume starts both from empty state; once
// Shutdown stopped the cleanup, resume starts nothing.
func TestMergedPRCleanupPauseJoinsAndResumesFromNothing(t *testing.T) {
	t.Parallel()
	m := newMergeCase(t, "feature/pause")
	c := &m.app.mergedPRs
	c.interval = 20 * time.Millisecond
	m.app.maintenance.firstReadsFallback = time.Millisecond
	m.unmerge(t)
	m.seed(t, "open")
	m.app.startMergedPRCleanup()
	t.Cleanup(m.app.stopMergedPRCleanup)
	// The poll reads the open PR before the pause.
	deadline := time.Now().Add(10 * time.Second)
	for m.requests.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the poll never read the PR")
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.pollMu.Lock()
	pollDone := c.pollDone
	c.pollMu.Unlock()
	c.mu.Lock()
	workerDone := c.done
	c.mu.Unlock()

	resume := m.app.pauseMergedPRCleanup()
	for name, done := range map[string]chan struct{}{"poll": pollDone, "worker": workerDone} {
		select {
		case <-done:
		default:
			t.Fatalf("the pause returned before the %s ended", name)
		}
	}
	m.seed(t, "merged")
	// A request the pause cancelled can still reach the forge after the
	// poll returned; let it land before counting.
	before := m.requests.Load()
	for {
		time.Sleep(5 * c.interval)
		n := m.requests.Load()
		if n == before {
			break
		}
		before = n
	}
	// A lookup backoff left from before the pause; nothing else prunes one
	// for a repository nobody tracks.
	backoffFor(&c.lookups, "stale").next = time.Now().Add(time.Hour)
	time.Sleep(10 * c.interval)
	if n := m.requests.Load(); n != before {
		t.Fatalf("%d forge requests while paused", n-before)
	}
	if row := m.row(t, m.thread.ID); row.MergedAt != 0 {
		t.Fatalf("the merge was read while paused: %+v", row)
	}

	resume()
	c.pass.Lock()
	stale := c.lookups["stale"]
	c.pass.Unlock()
	if stale != nil {
		t.Fatal("resume kept the previous state")
	}
	if row := m.awaitHandled(t); row.Outcome != store.PROutcomeRemoved {
		t.Fatalf("outcome = %q, want removed", row.Outcome)
	}

	resume = m.app.pauseMergedPRCleanup()
	m.app.stopMergedPRCleanup()
	resume()
	c.mu.Lock()
	running := c.stop != nil
	c.mu.Unlock()
	if running || c.mergedPRPollRunning() {
		t.Fatal("resume started a cleanup Shutdown stopped")
	}
}

// With the setting off, a lookup that stops finding the tracked PR still
// reads its state once, and the merge it finds is acted on.
func TestMergedPRWatchedReadRunsWithTheSettingOff(t *testing.T) {
	t.Parallel()
	m := newMergeCase(t, "feature/watched-off")
	m.app.mergedPRs.interval = 20 * time.Millisecond
	m.app.maintenance.firstReadsFallback = time.Millisecond
	m.unmerge(t)
	m.setBackgroundFetch(t, false)
	m.app.startMergedPRCleanup()
	t.Cleanup(m.app.stopMergedPRCleanup)

	m.app.observeOpenPR(gitops.OpenPRObservation{Cwd: m.path, Branch: m.thread.Branch})
	if row := m.awaitHandled(t); row.Outcome != store.PROutcomeRemoved {
		t.Fatalf("outcome = %q, want removed", row.Outcome)
	}
	if m.app.mergedPRs.mergedPRPollRunning() {
		t.Fatal("the poll runs with the setting off")
	}
}
