package app

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/chatmodel"
	"agent-overflow/internal/codexmodels"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/triage"
)

func TestGetThreadDefaultsDoesNotLoadColdCodexCatalog(t *testing.T) {
	app := newTestAppWithStore(t)
	calls := 0
	app.providerDiscoveryCaches.CodexModels = codexmodels.NewWith(time.Minute, func(context.Context, string) ([]provider.ModelInfo, error) {
		calls++
		return provider.ModelsForProvider(string(provider.Codex)), nil
	}, time.Now)
	profile := chatmodel.FallbackProfile(string(provider.Codex), "gpt-5.5")
	profile.UpdatedAt = time.Now().UnixMilli()
	if err := app.store.UpsertChatModelProfile(profile); err != nil {
		t.Fatalf("UpsertChatModelProfile: %v", err)
	}
	defaults, err := app.GetThreadDefaults(CreateThreadOptions{ProjectID: defaultTestProjectID})
	if err != nil {
		t.Fatalf("GetThreadDefaults: %v", err)
	}
	if calls != 0 {
		t.Fatalf("Codex catalog loads = %d, want 0 on the new-thread paint path", calls)
	}
	if defaults.Provider != profile.Provider || defaults.Model != profile.Model {
		t.Fatalf("defaults provider/model = %s/%s, want stored %s/%s", defaults.Provider, defaults.Model, profile.Provider, profile.Model)
	}
	if _, err := app.CreateThread(t.Context(), CreateThreadOptions{ProjectID: defaultTestProjectID, Provider: defaults.Provider, Model: defaults.Model}); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if calls != 1 {
		t.Fatalf("Codex catalog loads after materialization = %d, want 1 authoritative validation", calls)
	}
}

func TestGetThreadDefaultsUsesWarmCodexCatalogWithoutReloading(t *testing.T) {
	app := newTestAppWithStore(t)
	calls := 0
	app.providerDiscoveryCaches.CodexModels = codexmodels.NewWith(time.Minute, func(context.Context, string) ([]provider.ModelInfo, error) {
		calls++
		return []provider.ModelInfo{{
			Slug: "gpt-5.5", Provider: string(provider.Codex),
			ReasoningEfforts: []provider.ReasoningEffortOption{{Slug: string(provider.EffortUltra), Default: true}},
		}}, nil
	}, time.Now)
	profile := chatmodel.FallbackProfile(string(provider.Codex), "gpt-5.5")
	profile.ReasoningEffort = string(provider.EffortHigh)
	profile.FastMode = true
	profile.UpdatedAt = time.Now().UnixMilli()
	if err := app.store.UpsertChatModelProfile(profile); err != nil {
		t.Fatalf("UpsertChatModelProfile: %v", err)
	}
	if _, err := app.GetModelsForProvider(string(provider.Codex)); err != nil {
		t.Fatalf("warm Codex catalog: %v", err)
	}
	defaults, err := app.GetThreadDefaults(CreateThreadOptions{ProjectID: defaultTestProjectID})
	if err != nil {
		t.Fatalf("GetThreadDefaults: %v", err)
	}
	if calls != 1 {
		t.Fatalf("Codex catalog loads = %d, want the one explicit warmup", calls)
	}
	if defaults.ReasoningEffort != string(provider.EffortUltra) {
		t.Fatalf("ReasoningEffort = %q, want warm-catalog default %q", defaults.ReasoningEffort, provider.EffortUltra)
	}
	if defaults.FastMode {
		t.Fatal("FastMode = true, want warm catalog without a fast tier to disable it")
	}
}

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

func TestCreateThreadNormalizesModelAlias(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, string(provider.Codex), "/tmp/talias-create", "5.4", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	if thread.Model != "gpt-5.4" {
		t.Fatalf("Model = %q, want gpt-5.4", thread.Model)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if stored.Model != "gpt-5.4" {
		t.Fatalf("stored Model = %q, want gpt-5.4", stored.Model)
	}
}

func TestCreateThreadStampsInitialReadBaseline(t *testing.T) {
	app := newTestAppWithStore(t)

	thread, err := createTestThread(t, app, string(provider.Claude), "/tmp/thread-baseline", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	if thread.LastReadAt == nil {
		t.Fatal("created thread LastReadAt = nil, want creation-time read baseline")
	}
	if *thread.LastReadAt != thread.CreatedAt {
		t.Fatalf("created thread LastReadAt = %d, want CreatedAt %d", *thread.LastReadAt, thread.CreatedAt)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if stored.LastReadAt == nil || *stored.LastReadAt != *thread.LastReadAt {
		t.Fatalf("stored LastReadAt = %v, want returned value %d", stored.LastReadAt, *thread.LastReadAt)
	}

	completedAt := thread.CreatedAt + 1
	insertCompletedTurnForAppTest(t, app, thread.ID, "turn-after-create", thread.CreatedAt, completedAt)
	afterCompletion, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread after completion: %v", err)
	}
	if afterCompletion.LatestTurnCompletedAt == nil || *afterCompletion.LatestTurnCompletedAt != completedAt {
		t.Fatalf("LatestTurnCompletedAt = %v, want %d", afterCompletion.LatestTurnCompletedAt, completedAt)
	}
	if afterCompletion.LastReadAt == nil || *afterCompletion.LatestTurnCompletedAt <= *afterCompletion.LastReadAt {
		t.Fatalf("completion is not unread: latest=%v lastRead=%v", afterCompletion.LatestTurnCompletedAt, afterCompletion.LastReadAt)
	}
}

// --- StartTerminal (terminal-mode thread creation) ---

func TestStartTerminalPerProjectRootsAtProjectPath(t *testing.T) {
	app := newTestAppWithStore(t)
	project, err := app.ensureProjectForWorkspace("/tmp/term-proj")
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace: %v", err)
	}

	term, err := app.StartTerminal(t.Context(), StartTerminalOptions{ProjectID: project.ID})
	if err != nil {
		t.Fatalf("StartTerminal: %v", err)
	}
	if term.Mode != "terminal" {
		t.Errorf("Mode = %q, want terminal", term.Mode)
	}
	if term.ProjectID != project.ID {
		t.Errorf("ProjectID = %q, want %q", term.ProjectID, project.ID)
	}
	if term.WorkspacePath != project.Path {
		t.Errorf("WorkspacePath = %q, want %q", term.WorkspacePath, project.Path)
	}
	if term.Title != "Terminal" {
		t.Errorf("Title = %q, want Terminal (default)", term.Title)
	}
	// The sentinel must satisfy the coupled (provider, reasoning_effort)
	// CHECK: a real provider and a non-empty effort.
	if term.Provider != "claude" && term.Provider != "codex" {
		t.Errorf("Provider = %q, want a real provider sentinel", term.Provider)
	}
	if term.ReasoningEffort == "" {
		t.Error("ReasoningEffort is empty; the coupled CHECK would reject it")
	}

	stored, err := app.store.GetThread(term.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if stored.Mode != "terminal" || stored.ProjectID != project.ID {
		t.Errorf("stored = {mode:%q project:%q}, want {terminal %q}", stored.Mode, stored.ProjectID, project.ID)
	}
}

func TestStartTerminalStandaloneRootsAtHome(t *testing.T) {
	app := newTestAppWithStore(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory available: %v", err)
	}

	term, err := app.StartTerminal(t.Context(), StartTerminalOptions{})
	if err != nil {
		t.Fatalf("StartTerminal: %v", err)
	}
	if term.Mode != "terminal" {
		t.Errorf("Mode = %q, want terminal", term.Mode)
	}
	if term.ProjectID != "" {
		t.Errorf("ProjectID = %q, want empty (standalone)", term.ProjectID)
	}
	if term.WorkspacePath != home {
		t.Errorf("WorkspacePath = %q, want home %q", term.WorkspacePath, home)
	}

	// A standalone terminal persists project_id as NULL; the binding must
	// round-trip it back as "" without a scan error.
	stored, err := app.store.GetThread(term.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if stored.ProjectID != "" {
		t.Errorf("stored ProjectID = %q, want empty", stored.ProjectID)
	}
}

func TestStartTerminalCwdOverrideAndCustomTitle(t *testing.T) {
	app := newTestAppWithStore(t)
	project, err := app.ensureProjectForWorkspace("/tmp/term-cwd-proj")
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace: %v", err)
	}

	term, err := app.StartTerminal(t.Context(), StartTerminalOptions{
		ProjectID: project.ID,
		Cwd:       "/tmp/term-cwd-proj/sub",
		Title:     "Logs",
	})
	if err != nil {
		t.Fatalf("StartTerminal: %v", err)
	}
	if term.WorkspacePath != "/tmp/term-cwd-proj/sub" {
		t.Errorf("WorkspacePath = %q, want the cwd override", term.WorkspacePath)
	}
	if term.ProjectID != project.ID {
		t.Errorf("ProjectID = %q, want %q (cwd override keeps the project)", term.ProjectID, project.ID)
	}
	if term.Title != "Logs" {
		t.Errorf("Title = %q, want Logs", term.Title)
	}
}

func TestStartTerminalRejectsUnknownProject(t *testing.T) {
	app := newTestAppWithStore(t)
	if _, err := app.StartTerminal(t.Context(), StartTerminalOptions{ProjectID: "does-not-exist"}); err == nil {
		t.Fatal("StartTerminal(unknown project) error = nil, want resolve error")
	}
}

func TestUpdateThreadModelNormalizesAlias(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, string(provider.Claude), "/tmp/talias-update", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	updated, err := app.UpdateThreadModel(thread.ID, "opus")
	if err != nil {
		t.Fatalf("UpdateThreadModel(opus): %v", err)
	}
	if updated.Model != "claude-opus-5" {
		t.Fatalf("Model = %q, want claude-opus-5", updated.Model)
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

	updated, err := app.UpdateThreadReasoningEffort(thread.ID, "high")
	if err != nil {
		t.Fatalf("UpdateThreadReasoningEffort: %v", err)
	}
	if updated.ReasoningEffort != "high" {
		t.Fatalf("ReasoningEffort = %q, want high", updated.ReasoningEffort)
	}

	if _, err := app.UpdateThreadReasoningEffort(thread.ID, "ultranope"); err == nil {
		t.Fatal("UpdateThreadReasoningEffort(ultranope) error = nil, want validation error")
	}
}

func TestUpdateThreadReasoningEffortAcceptsCodexMaxAndUltra(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "codex", "/tmp/tcmax", "gpt-5.6-sol", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	updated, err := app.UpdateThreadReasoningEffort(thread.ID, "max")
	if err != nil {
		t.Fatalf("UpdateThreadReasoningEffort(max): %v", err)
	}
	if updated.ReasoningEffort != "max" {
		t.Fatalf("ReasoningEffort = %q, want max", updated.ReasoningEffort)
	}

	updated, err = app.UpdateThreadReasoningEffort(thread.ID, "ultra")
	if err != nil {
		t.Fatalf("UpdateThreadReasoningEffort(ultra): %v", err)
	}
	if updated.ReasoningEffort != "ultra" {
		t.Fatalf("ReasoningEffort = %q, want ultra", updated.ReasoningEffort)
	}
}

func TestUpdateThreadFastModeToggles(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/tfm", "claude-opus-4-7", "")
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

func TestUpdateThreadFastModeRejectsUnsupportedModel(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "codex", "/tmp/tfm-unsupported", "gpt-5.4-mini", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	if _, err := app.UpdateThreadFastMode(thread.ID, true); err == nil {
		t.Fatal("UpdateThreadFastMode(true) error = nil, want unsupported model error")
	}
}

func TestCreateThreadRejectsUnsupportedExplicitFastMode(t *testing.T) {
	app := newTestAppWithStore(t)
	project, err := app.ensureProjectForWorkspace("/tmp/create-fast-unsupported")
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace: %v", err)
	}
	fast := true

	_, err = app.CreateThread(t.Context(), CreateThreadOptions{
		ProjectID:         project.ID,
		Provider:          "codex",
		Model:             "gpt-5.4-mini",
		WorkspaceOverride: project.Path,
		FastMode:          &fast,
	})
	if err == nil {
		t.Fatal("CreateThread fast mode error = nil, want unsupported model error")
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

	_, err = app.CreateThread(t.Context(), CreateThreadOptions{
		ProjectID:     project.ID,
		Provider:      "codex",
		Model:         "gpt-5.4-mini",
		ContextWindow: provider.CodexExtendedContextWindow,
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

func TestUpdateNewThreadDefaultsPersistsProfileForFutureThreads(t *testing.T) {
	app := newTestAppWithStore(t)
	project, err := app.ensureProjectForWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace: %v", err)
	}
	fastMode := false

	defaults, err := app.UpdateNewThreadDefaults(NewThreadDefaultsUpdate{
		ProjectID:       project.ID,
		Provider:        "codex",
		Model:           "5.4",
		ReasoningEffort: "high",
		FastMode:        &fastMode,
		RuntimeMode:     "approval-required",
	})
	if err != nil {
		t.Fatalf("UpdateNewThreadDefaults: %v", err)
	}
	if defaults.Provider != "codex" || defaults.Model != "gpt-5.4" {
		t.Fatalf("defaults provider/model = %s/%s, want codex/gpt-5.4", defaults.Provider, defaults.Model)
	}
	if defaults.ReasoningEffort != "high" {
		t.Fatalf("defaults ReasoningEffort = %q, want high", defaults.ReasoningEffort)
	}
	if defaults.RuntimeMode != "approval-required" {
		t.Fatalf("defaults RuntimeMode = %q, want approval-required", defaults.RuntimeMode)
	}

	thread, err := app.CreateThread(t.Context(), CreateThreadOptions{
		ProjectID: project.ID,
		Provider:  "codex",
		Model:     "gpt-5.4",
	})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if thread.RuntimeMode != "approval-required" {
		t.Fatalf("thread RuntimeMode = %q, want approval-required", thread.RuntimeMode)
	}
	if thread.ReasoningEffort != "high" {
		t.Fatalf("thread ReasoningEffort = %q, want high", thread.ReasoningEffort)
	}
}

func TestUpdateNewThreadDefaultsValidatesRuntimeMode(t *testing.T) {
	app := newTestAppWithStore(t)
	project, err := app.ensureProjectForWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace: %v", err)
	}

	if _, err := app.UpdateNewThreadDefaults(NewThreadDefaultsUpdate{
		ProjectID:   project.ID,
		Provider:    "codex",
		Model:       "gpt-5.4",
		RuntimeMode: "yolo",
	}); err == nil {
		t.Fatal("UpdateNewThreadDefaults(yolo) error = nil, want validation error")
	}
}

func TestUpdateThreadBranchAndWorkspace(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/tbw", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	updated, err := app.UpdateThreadBranch(thread.WorkspacePath, "feat/abc")
	if err != nil {
		t.Fatalf("UpdateThreadBranch: %v", err)
	}
	if len(updated) != 1 || updated[0].ID != thread.ID || updated[0].Branch != "feat/abc" {
		t.Fatalf("UpdateThreadBranch returned %+v, want the one thread on feat/abc", updated)
	}

	if _, err := app.UpdateThreadWorkspace(thread.ID, ""); err == nil ||
		!strings.Contains(err.Error(), "path is required") {
		t.Fatalf("UpdateThreadWorkspace(empty) error = %v, want 'path is required'", err)
	}
}

// TestUpdateThreadBranchRejectsUnsafeBranchNames: the persisted branch is
// read back by later git operations and reaches argv there, so the binding
// validates at the door rather than trusting that its only caller happens to
// have read the value off a git status.
func TestUpdateThreadBranchRejectsUnsafeBranchNames(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/tbw-unsafe", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	for _, branch := range []string{"--upload-pack=touch /tmp/pwned", "a..b", "bad\x00name", "ctl\x01char"} {
		if _, err := app.UpdateThreadBranch(thread.WorkspacePath, branch); err == nil {
			t.Fatalf("UpdateThreadBranch(%q) error = nil, want a validation error", branch)
		}
		got, err := app.store.GetThread(thread.ID)
		if err != nil {
			t.Fatalf("GetThread: %v", err)
		}
		if got.Branch == branch {
			t.Fatalf("branch %q was persisted despite the refusal", branch)
		}
	}

	// Clearing is still legal — that is how the column is emptied.
	if _, err := app.UpdateThreadBranch(thread.WorkspacePath, ""); err != nil {
		t.Fatalf("UpdateThreadBranch(clear): %v", err)
	}
}

// TestUpdateThreadBranchFansOutAcrossTheWorkspace is the binding-level half
// of the entity keying: the branch belongs to the checkout, so every thread
// sitting in that workspace must learn it from one observation. Two panes on
// one worktree is the normal case.
func TestUpdateThreadBranchFansOutAcrossTheWorkspace(t *testing.T) {
	app := newTestAppWithStore(t)
	first, err := createTestThread(t, app, "claude", "/tmp/tbw-shared", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread(first): %v", err)
	}
	second, err := createTestThread(t, app, "claude", "/tmp/tbw-shared", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread(second): %v", err)
	}

	updated, err := app.UpdateThreadBranch(first.WorkspacePath, "feature/live")
	if err != nil {
		t.Fatalf("UpdateThreadBranch: %v", err)
	}
	if len(updated) != 2 {
		t.Fatalf("UpdateThreadBranch returned %d rows, want 2", len(updated))
	}
	for _, id := range []string{first.ID, second.ID} {
		got, err := app.store.GetThread(id)
		if err != nil {
			t.Fatalf("GetThread(%s): %v", id, err)
		}
		if got.Branch != "feature/live" {
			t.Fatalf("thread %s Branch = %q, want feature/live", id, got.Branch)
		}
	}
}

// TestUpdateThreadBranchBroadcastsChangedRows: the CALLING client heals from
// the return value, but a second client writing the same observation matches
// zero rows — the first one already moved them — so without a broadcast its
// panes keep the superseded branch until something else refreshes them. A
// write that changed nothing must stay silent.
func TestUpdateThreadBranchBroadcastsChangedRows(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/tbw-broadcast", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	var broadcast []triage.ThreadUpdateEvent
	app.emitEventFn = func(name string, data any) {
		if name != "thread:updated" {
			return
		}
		evt, ok := data.(triage.ThreadUpdateEvent)
		if !ok {
			t.Errorf("thread:updated payload type = %T, want triage.ThreadUpdateEvent", data)
			return
		}
		broadcast = append(broadcast, evt)
	}

	if _, err := app.UpdateThreadBranch(thread.WorkspacePath, "feature/live"); err != nil {
		t.Fatalf("UpdateThreadBranch: %v", err)
	}
	if len(broadcast) != 1 {
		t.Fatalf("emitted %d thread:updated events, want 1", len(broadcast))
	}
	if broadcast[0].Thread == nil || broadcast[0].Thread.ID != thread.ID {
		t.Fatalf("broadcast row = %+v, want thread %s", broadcast[0].Thread, thread.ID)
	}
	if broadcast[0].Thread.Branch != "feature/live" {
		t.Fatalf("broadcast branch = %q, want feature/live", broadcast[0].Thread.Branch)
	}

	// The second client's identical write: nothing changed, nothing said.
	broadcast = nil
	if _, err := app.UpdateThreadBranch(thread.WorkspacePath, "feature/live"); err != nil {
		t.Fatalf("UpdateThreadBranch(repeat): %v", err)
	}
	if len(broadcast) != 0 {
		t.Fatalf("a no-op write emitted %d events, want 0", len(broadcast))
	}
}

// TestUpdateThreadBranchDropsStaleWorkspaceObservation is the binding-level
// half of the workspace keying: the caller is an unlocked async queue, so a
// branch observed in a workspace every thread has since left must land
// nowhere rather than following the thread to its new checkout.
func TestUpdateThreadBranchDropsStaleWorkspaceObservation(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/tbw-stale", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	observedWorkspace := thread.WorkspacePath

	moved, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	moved.WorkspacePath = "/tmp/tbw-stale-worktree"
	moved.WorktreePath = "/tmp/tbw-stale-worktree"
	moved.Branch = "feature/moved"
	if err := app.store.UpdateThread(moved); err != nil {
		t.Fatalf("UpdateThread: %v", err)
	}

	current, err := app.UpdateThreadBranch(observedWorkspace, "stale/branch")
	if err != nil {
		t.Fatalf("UpdateThreadBranch(stale) error = %v, want nil (a no-op is not an error)", err)
	}
	if len(current) != 0 {
		t.Fatalf("UpdateThreadBranch(stale) returned %+v, want no rows", current)
	}
	after, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if after.Branch != "feature/moved" {
		t.Fatalf("Branch = %q, want feature/moved", after.Branch)
	}

	if _, err := app.UpdateThreadBranch("  ", "x"); err == nil ||
		!strings.Contains(err.Error(), "workspace path is required") {
		t.Fatalf("UpdateThreadBranch(blank workspace) error = %v, want required-field error", err)
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
