package app

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
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

// A merged PR whose worktree directory is already gone is done: the row is
// handled as gone and git's registration of the directory is pruned, with
// no forge request.
func TestMergedPRWorktreeAlreadyGoneIsPruned(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	repo := initMainGitRepo(t)
	thread, path := mergeCleanupThread(t, app, repo, "feature/gone")
	pr := store.ThreadPullRequest{
		ThreadID: thread.ID, Forge: "github", Host: "github.com", Project: "acme/widgets", Number: 5,
		URL: "https://github.com/acme/widgets/pull/5", NodeID: "PR_gone", Branch: "feature/gone",
		WorktreePath: path, SeenOpenAt: time.Now().UnixMilli(),
	}
	if _, err := app.store.RecordOpenThreadPullRequest(pr); err != nil {
		t.Fatalf("RecordOpenThreadPullRequest: %v", err)
	}
	if _, err := app.store.MarkThreadPullRequestsMerged(pr.Forge, pr.Host, pr.Project, pr.Number, time.Now().UnixMilli()); err != nil {
		t.Fatalf("MarkThreadPullRequestsMerged: %v", err)
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("remove worktree directory: %v", err)
	}
	if _, ok, err := app.findWorktree(repo, path); err != nil || !ok {
		t.Fatalf("git no longer lists the deleted worktree before the pass (ok=%v err=%v)", ok, err)
	}

	app.mergedPRActPass(context.Background())

	row, ok, err := app.store.GetThreadPullRequest(thread.ID)
	if err != nil || !ok {
		t.Fatalf("GetThreadPullRequest: ok=%v err=%v", ok, err)
	}
	if row.HandledAt == 0 || row.Outcome != store.PROutcomeGone {
		t.Fatalf("row = handled %d outcome %q, want handled as %q", row.HandledAt, row.Outcome, store.PROutcomeGone)
	}
	if _, ok, err := app.findWorktree(repo, path); err != nil || ok {
		t.Fatalf("git still lists the deleted worktree after the pass (ok=%v err=%v)", ok, err)
	}
}
