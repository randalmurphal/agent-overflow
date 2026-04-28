package main

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
)

// Smoke tests for the per-field thread update bindings. These sit at the
// binding boundary: they validate the input, call the store, and return
// the refreshed thread. The restart-if-affected logic is exercised with
// a stubbed active session so the binding returns after persisting even
// though the session is "live".

func TestUpdateThreadProviderPersistsAndValidates(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/tp", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	updated, err := app.UpdateThreadProvider(thread.ID, "codex")
	if err != nil {
		t.Fatalf("UpdateThreadProvider: %v", err)
	}
	if updated.Provider != "codex" {
		t.Fatalf("Provider = %q, want codex", updated.Provider)
	}

	if _, err := app.UpdateThreadProvider(thread.ID, "bogus"); err == nil {
		t.Fatal("UpdateThreadProvider(bogus) error = nil, want validation error")
	}
}

// TestUpdateThreadProviderLocksAfterFirstItem guards the "provider is
// locked once the thread has been used" invariant. Once any item lands,
// switching providers is rejected: the provider session ids aren't
// interchangeable, so the reconnect would otherwise fail with an opaque
// "no rollout found" from the new provider. Verified here by:
//  1. creating a claude thread and persisting a single user_text item,
//  2. asserting the cross-provider switch fails with a clear message
//     and leaves the provider column untouched,
//  3. asserting an idempotent same-provider call still succeeds.
func TestUpdateThreadProviderLocksAfterFirstItem(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/tlock", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	now := time.Now().UnixMilli()
	if err := app.store.InsertItem(store.Item{
		ID:        "item-lock-1",
		ThreadID:  thread.ID,
		TurnIndex: 1,
		ItemIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Summary:   "hello",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("InsertItem: %v", err)
	}

	_, err = app.UpdateThreadProvider(thread.ID, "codex")
	if err == nil {
		t.Fatal("UpdateThreadProvider(codex) on used thread error = nil, want lock error")
	}
	if !strings.Contains(err.Error(), "locked to claude") {
		t.Fatalf("UpdateThreadProvider error = %v, want 'locked to claude' context", err)
	}

	after, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if after.Provider != "claude" {
		t.Fatalf("Provider = %q after rejected switch, want claude (no mutation)", after.Provider)
	}

	// Idempotent same-provider call MUST still succeed so the composer's
	// "set provider then set model" pattern (which re-sends the current
	// provider when only the model changed) doesn't get wedged by the lock.
	same, err := app.UpdateThreadProvider(thread.ID, "claude")
	if err != nil {
		t.Fatalf("UpdateThreadProvider(claude) on used claude thread error = %v, want nil", err)
	}
	if same.Provider != "claude" {
		t.Fatalf("Provider = %q after same-provider call, want claude", same.Provider)
	}
}

func TestUpdateThreadReasoningEffortValidates(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/te", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	updated, err := app.UpdateThreadReasoningEffort(thread.ID, "xhigh")
	if err != nil {
		t.Fatalf("UpdateThreadReasoningEffort: %v", err)
	}
	if updated.ReasoningEffort != "xhigh" {
		t.Fatalf("ReasoningEffort = %q, want xhigh", updated.ReasoningEffort)
	}

	if _, err := app.UpdateThreadReasoningEffort(thread.ID, "ultranope"); err == nil {
		t.Fatal("UpdateThreadReasoningEffort(ultranope) error = nil, want validation error")
	}
}

func TestUpdateThreadFastModeToggles(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/tfm", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	if thread.FastMode {
		t.Fatal("FastMode default should be false")
	}

	updated, err := app.UpdateThreadFastMode(thread.ID, true)
	if err != nil {
		t.Fatalf("UpdateThreadFastMode(true): %v", err)
	}
	if !updated.FastMode {
		t.Fatal("FastMode = false, want true")
	}

	updated, err = app.UpdateThreadFastMode(thread.ID, false)
	if err != nil {
		t.Fatalf("UpdateThreadFastMode(false): %v", err)
	}
	if updated.FastMode {
		t.Fatal("FastMode = true, want false")
	}
}

func TestUpdateThreadContextWindowValidates(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/tcw", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	updated, err := app.UpdateThreadContextWindow(thread.ID, 200000)
	if err != nil {
		t.Fatalf("UpdateThreadContextWindow(200000): %v", err)
	}
	if updated.ContextWindow != 200000 {
		t.Fatalf("ContextWindow = %d, want 200000", updated.ContextWindow)
	}

	if _, err := app.UpdateThreadContextWindow(thread.ID, 999); err == nil {
		t.Fatal("UpdateThreadContextWindow(999) error = nil, want validation error")
	}
}

func TestCreateThreadRejectsUnsupportedContextWindow(t *testing.T) {
	app := newTestAppWithStore(t)
	project, err := app.ensureProjectForWorkspace("/tmp/create-context")
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace: %v", err)
	}

	_, err = app.CreateThread(CreateThreadOptions{
		ProjectID:     project.ID,
		Provider:      "codex",
		Model:         "gpt-5.4-mini",
		ContextWindow: 1050000,
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported context window") {
		t.Fatalf("CreateThread unsupported context error = %v, want unsupported context window", err)
	}
}

func TestUpdateThreadRuntimeModeValidates(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/trm", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	updated, err := app.UpdateThreadRuntimeMode(thread.ID, "approval-required")
	if err != nil {
		t.Fatalf("UpdateThreadRuntimeMode: %v", err)
	}
	if updated.RuntimeMode != "approval-required" {
		t.Fatalf("RuntimeMode = %q, want approval-required", updated.RuntimeMode)
	}

	if _, err := app.UpdateThreadRuntimeMode(thread.ID, "yolo"); err == nil {
		t.Fatal("UpdateThreadRuntimeMode(yolo) error = nil, want validation error")
	}
}

func TestUpdateThreadBranchAndWorkspace(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/tbw", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	updated, err := app.UpdateThreadBranch(thread.ID, "feat/abc")
	if err != nil {
		t.Fatalf("UpdateThreadBranch: %v", err)
	}
	if updated.Branch != "feat/abc" {
		t.Fatalf("Branch = %q, want feat/abc", updated.Branch)
	}

	if _, err := app.UpdateThreadWorkspace(thread.ID, ""); err == nil ||
		!strings.Contains(err.Error(), "path is required") {
		t.Fatalf("UpdateThreadWorkspace(empty) error = %v, want 'path is required'", err)
	}
}

func TestUpdateThreadWorkspaceSwitchesRegisteredWorktree(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-workspace-switch")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	thread.Branch = "main"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	worktreePath, err := app.GitCreateWorktree(thread.ID, "feature/workspace")
	if err != nil {
		t.Fatalf("GitCreateWorktree() error = %v", err)
	}
	t.Cleanup(func() {
		_ = app.gitCore().RemoveWorktreeForce(repo, worktreePath, true)
	})

	updated, err := app.UpdateThreadWorkspace(thread.ID, repo)
	if err != nil {
		t.Fatalf("UpdateThreadWorkspace(repo) error = %v", err)
	}
	if updated.WorktreePath != "" {
		t.Fatalf("WorktreePath after root switch = %q, want empty", updated.WorktreePath)
	}
	if !samePath(updated.WorkspacePath, repo) {
		t.Fatalf("WorkspacePath after root switch = %q, want %q", updated.WorkspacePath, repo)
	}
	if updated.Branch != "main" {
		t.Fatalf("Branch after root switch = %q, want main", updated.Branch)
	}

	updated, err = app.UpdateThreadWorkspace(thread.ID, worktreePath)
	if err != nil {
		t.Fatalf("UpdateThreadWorkspace(worktree) error = %v", err)
	}
	if !samePath(updated.WorkspacePath, worktreePath) {
		t.Fatalf("WorkspacePath after worktree switch = %q, want %q", updated.WorkspacePath, worktreePath)
	}
	if !samePath(updated.WorktreePath, worktreePath) {
		t.Fatalf("WorktreePath after worktree switch = %q, want %q", updated.WorktreePath, worktreePath)
	}
	if updated.Branch != "feature/workspace" {
		t.Fatalf("Branch after worktree switch = %q, want feature/workspace", updated.Branch)
	}
}

// TestMarkThreadReadUnreadLifecycle walks MarkThreadRead then MarkThreadUnread
// through the App binding surface, verifying each flips last_read_at as the
// sidebar expects.
func TestMarkThreadReadUnreadLifecycle(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/read", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	before := time.Now().UnixMilli()
	if err := app.MarkThreadRead(thread.ID); err != nil {
		t.Fatalf("MarkThreadRead: %v", err)
	}
	got, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread after MarkThreadRead: %v", err)
	}
	if got.LastReadAt == nil {
		t.Fatalf("LastReadAt = nil after MarkThreadRead")
	}
	if *got.LastReadAt < before {
		t.Fatalf("LastReadAt = %d, want >= %d", *got.LastReadAt, before)
	}

	if err := app.MarkThreadUnread(thread.ID); err != nil {
		t.Fatalf("MarkThreadUnread: %v", err)
	}
	got, err = app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread after MarkThreadUnread: %v", err)
	}
	if got.LastReadAt == nil {
		t.Fatalf("LastReadAt = nil after MarkThreadUnread, want 0")
	}
	if *got.LastReadAt != 0 {
		t.Fatalf("LastReadAt = %d after MarkThreadUnread, want 0", *got.LastReadAt)
	}

	// Missing thread should surface sql.ErrNoRows — the store wraps
	// but unwraps cleanly through errors.Is so callers can branch on
	// the sentinel without string-matching.
	if err := app.MarkThreadRead("missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("MarkThreadRead(missing) error = %v, want sql.ErrNoRows", err)
	}
	if err := app.MarkThreadUnread("missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("MarkThreadUnread(missing) error = %v, want sql.ErrNoRows", err)
	}
}
