package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/discussion"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
)

func TestWorkflowBoundMethodsRegisteredOnApp(t *testing.T) {
	appType := reflect.TypeOf((*App)(nil))
	for _, name := range []string{
		"WorkflowStartRun", "WorkflowCancelItem", "WorkflowResumeItem",
		"WorkflowAnswerQuestion", "WorkflowResolveGate", "WorkflowRerunItem",
		"WorkflowSetGlobalPause", "WorkflowGetEngineState",
		"WorkflowListItems", "WorkflowListUnresolvedItems", "WorkflowListItemCosts", "WorkflowGetItem",
		"WorkflowCompleteTakeover",
		"WorkflowMergeItem", "WorkflowCreateItemPR", "WorkflowDiscardItem",
		"WorkflowFetchPRReviewComments", "WorkflowSendPRReviewCommentsToThread", "WorkflowDiscussPR",
		"WorkflowGetJobNotes", "WorkflowSetJobNotes", "WorkflowListDefinitions",
	} {
		if _, ok := appType.MethodByName(name); !ok {
			t.Errorf("App method %s is not registered", name)
		}
	}
}

// D32 deleted every workflow affordance that spawned a NEW chat thread. A bound
// method is a wire RPC and a generated TS binding, so re-exporting one of these
// would silently put the removed surface back within reach of any caller.
func TestWorkflowThreadSpawningMethodsAreNotBound(t *testing.T) {
	appType := reflect.TypeOf((*App)(nil))
	for _, name := range []string{
		"WorkflowOpenTriageThread", "WorkflowOpenTriageAgent",
		"WorkflowOpenStudioThread", "WorkflowOpenInThread",
	} {
		if _, ok := appType.MethodByName(name); ok {
			t.Errorf("App method %s is bound again; D32 removed it", name)
		}
	}
}

func TestGetSettingsReturnsCurrentServiceState(t *testing.T) {
	dir := t.TempDir()
	svc := settings.NewService(dir)
	if _, err := svc.Update(map[string]any{"theme": "dark"}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	app := &App{settings: svc}
	got, err := app.GetSettings()
	if err != nil {
		t.Fatalf("GetSettings() error = %v", err)
	}
	if got.Theme != "dark" {
		t.Fatalf("Theme = %q, want dark", got.Theme)
	}
}

func TestUpdateSettingsPersistsPatch(t *testing.T) {
	dir := t.TempDir()
	app := &App{settings: settings.NewService(dir)}

	got, err := app.UpdateSettings(map[string]any{
		"theme":       "dark",
		"paneDensity": "spacious",
	})
	if err != nil {
		t.Fatalf("UpdateSettings() error = %v", err)
	}
	if got.Theme != "dark" {
		t.Fatalf("Theme = %q, want dark", got.Theme)
	}
	if got.PaneDensity != "spacious" {
		t.Fatalf("PaneDensity = %q, want spacious", got.PaneDensity)
	}

	reloaded := settings.NewService(dir).Get()
	if reloaded.Theme != "dark" {
		t.Fatalf("reloaded Theme = %q, want dark", reloaded.Theme)
	}
	if reloaded.PaneDensity != "spacious" {
		t.Fatalf("reloaded PaneDensity = %q, want spacious", reloaded.PaneDensity)
	}
}

func TestSettingsRollbackPatchRestoresEveryPatchedField(t *testing.T) {
	previous := settings.DefaultSettings
	previous.Theme = "light"
	previous.WorkflowPaused = true
	rollback, err := settingsRollbackPatch(previous, map[string]any{
		"theme": "dark", "workflowPaused": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rollback["theme"] != "light" || rollback["workflowPaused"] != true {
		t.Fatalf("rollback patch = %#v", rollback)
	}
}

func TestUpdateSettingsRollsBackMixedPatchWhenWorkflowEngineRejects(t *testing.T) {
	app := newTestAppWithStore(t)
	if err := app.initWorkflowEngine(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := app.workflowEngine.Close(); err != nil {
		t.Fatal(err)
	}
	previous := app.currentSettings()
	if _, err := app.UpdateSettings(map[string]any{
		"theme": "dark", "workflowPaused": true,
	}); err == nil {
		t.Fatal("UpdateSettings with closed workflow engine succeeded")
	}
	current := app.currentSettings()
	if current.Theme != previous.Theme || current.WorkflowPaused != previous.WorkflowPaused {
		t.Fatalf("mixed patch was not rolled back: got %+v, previous %+v", current, previous)
	}
}

func TestGetModelsForProvider(t *testing.T) {
	svc := settings.NewService(t.TempDir())
	if _, err := svc.Update(map[string]any{"codexBinaryPath": writeModelListCodexBinary(t)}); err != nil {
		t.Fatalf("Update codexBinaryPath: %v", err)
	}
	app := &App{settings: svc}

	claudeModels, err := app.GetModelsForProvider("claude")
	if err != nil {
		t.Fatalf("GetModelsForProvider(claude) error = %v", err)
	}
	if len(claudeModels) == 0 {
		t.Fatal("expected claude models")
	}
	if claudeModels[0].Provider != "claude" {
		t.Fatalf("Provider = %q, want claude", claudeModels[0].Provider)
	}
	codexModels, err := app.GetModelsForProvider("codex")
	if err != nil {
		t.Fatalf("GetModelsForProvider(codex) error = %v", err)
	}
	if len(codexModels) == 0 {
		t.Fatal("expected codex models")
	}
	if codexModels[0].Slug != "gpt-5.5" {
		t.Fatalf("first codex model = %q, want fake app-server gpt-5.5", codexModels[0].Slug)
	}

	unknown, err := app.GetModelsForProvider("unknown")
	if err != nil {
		t.Fatalf("GetModelsForProvider(unknown) error = %v", err)
	}
	if unknown != nil {
		t.Fatalf("unknown provider models = %v, want nil", unknown)
	}
}

func TestGetModelsForProviderCachesCodexCatalogByBinary(t *testing.T) {
	svc := settings.NewService(t.TempDir())
	counter := filepath.Join(t.TempDir(), "calls")
	if _, err := svc.Update(map[string]any{"codexBinaryPath": writeCountingModelListCodexBinary(t, counter, "gpt-5.5")}); err != nil {
		t.Fatalf("Update codexBinaryPath: %v", err)
	}
	app := &App{settings: svc}

	for i := 0; i < 2; i++ {
		models, err := app.GetModelsForProvider("codex")
		if err != nil {
			t.Fatalf("GetModelsForProvider(codex) #%d: %v", i+1, err)
		}
		if len(models) != 1 || models[0].Slug != "gpt-5.5" {
			t.Fatalf("models #%d = %#v, want gpt-5.5", i+1, models)
		}
	}

	if got := strings.TrimSpace(readFileForTest(t, counter)); got != "1" {
		t.Fatalf("codex model/list process count = %q, want 1", got)
	}
}

func TestUpdateSettingsInvalidatesCodexCatalogOnBinaryChange(t *testing.T) {
	svc := settings.NewService(t.TempDir())
	counter := filepath.Join(t.TempDir(), "calls")
	first := writeCountingModelListCodexBinary(t, counter, "gpt-5.4")
	second := writeCountingModelListCodexBinary(t, counter, "gpt-5.5")
	if _, err := svc.Update(map[string]any{"codexBinaryPath": first}); err != nil {
		t.Fatalf("Update codexBinaryPath: %v", err)
	}
	app := &App{settings: svc}

	if _, err := app.GetModelsForProvider("codex"); err != nil {
		t.Fatalf("GetModelsForProvider first: %v", err)
	}
	if _, err := app.UpdateSettings(map[string]any{"codexBinaryPath": second}); err != nil {
		t.Fatalf("UpdateSettings codexBinaryPath: %v", err)
	}
	models, err := app.GetModelsForProvider("codex")
	if err != nil {
		t.Fatalf("GetModelsForProvider second: %v", err)
	}
	if len(models) != 1 || models[0].Slug != "gpt-5.5" {
		t.Fatalf("models after binary change = %#v, want gpt-5.5", models)
	}
	if got := strings.TrimSpace(readFileForTest(t, counter)); got != "2" {
		t.Fatalf("codex model/list process count = %q, want 2", got)
	}
}

func writeModelListCodexBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex")
	script := `#!/usr/bin/env bash
set -euo pipefail
while IFS= read -r line; do
  id="$(printf '%s\n' "$line" | sed -n 's/.*"id":[[:space:]]*\([0-9][0-9]*\).*/\1/p')"
  if [[ "$line" == *'"method":"initialize"'* ]]; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
  elif [[ "$line" == *'"method":"model/list"'* ]]; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{"data":[{"model":"gpt-5.5","displayName":"GPT-5.5","hidden":false,"supportedReasoningEfforts":[{"reasoningEffort":"high","description":"High"}],"defaultReasoningEffort":"high","serviceTiers":[{"id":"priority","name":"Fast","description":"1.5x speed, increased usage"}]}],"nextCursor":null}}\n' "$id"
  fi
done
`
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatalf("write fake codex binary: %v", err)
	}
	return path
}

func writeCountingModelListCodexBinary(t *testing.T, counterPath, model string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex")
	script := fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
count=0
if [[ -f %[1]q ]]; then
  count="$(cat %[1]q)"
fi
printf '%%s\n' "$((count + 1))" > %[1]q
while IFS= read -r line; do
  id="$(printf '%%s\n' "$line" | sed -n 's/.*"id":[[:space:]]*\([0-9][0-9]*\).*/\1/p')"
  if [[ "$line" == *'"method":"initialize"'* ]]; then
    printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
  elif [[ "$line" == *'"method":"model/list"'* ]]; then
    printf '{"jsonrpc":"2.0","id":%%s,"result":{"data":[{"model":%[2]q,"displayName":%[2]q,"hidden":false,"supportedReasoningEfforts":[{"reasoningEffort":"high","description":"High"}],"defaultReasoningEffort":"high","serviceTiers":[{"id":"priority","name":"Fast","description":"1.5x speed, increased usage"}]}],"nextCursor":null}}\n' "$id"
  fi
done
`, counterPath, model)
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatalf("write counting fake codex binary: %v", err)
	}
	return path
}

func readFileForTest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestCreateThreadDefaultsMode(t *testing.T) {
	app := newTestAppWithStore(t)

	thread, err := createTestThread(t, app, string(provider.Codex), "/tmp/workspace", "gpt-5.4", "")
	if err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if thread.Mode != "chat" {
		t.Fatalf("returned Mode = %q, want chat", thread.Mode)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Mode != "chat" {
		t.Fatalf("stored Mode = %q, want chat", stored.Mode)
	}
	project, err := app.store.GetProject(stored.ProjectID)
	if err != nil {
		t.Fatalf("GetProject() error = %v", err)
	}
	if project.Path != "/tmp/workspace" {
		t.Fatalf("project path = %q, want /tmp/workspace", project.Path)
	}
}

// CreateThread inherits a sibling thread's worktree state when WorktreePath
// is supplied (used by "Implement plan in new thread"). The path must
// resolve to a real worktree of the project — bogus paths are rejected so
// a future caller can't spawn a provider session inside an arbitrary
// directory like ~/.ssh.
func TestCreateThreadInheritsWorktreeAndBranch(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)
	worktreeDir := filepath.Join(t.TempDir(), "inherit-worktree")
	testutil.RunGit(t, repo, "worktree", "add", "-b", "feat/foo", worktreeDir)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace: %v", err)
	}

	thread, err := app.CreateThread(CreateThreadOptions{
		ProjectID:    project.ID,
		Provider:     string(provider.Codex),
		Model:        "gpt-5.4",
		Mode:         "chat",
		Title:        "Implement Foo",
		WorktreePath: worktreeDir,
		// Branch left empty so the validation falls back to the worktree's
		// own branch (proves we're reading from gitops.ListWorktrees).
	})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if thread.Title != "Implement Foo" {
		t.Errorf("Title = %q, want %q", thread.Title, "Implement Foo")
	}
	if !samePath(thread.WorkspacePath, worktreeDir) {
		t.Errorf("WorkspacePath = %q, want %q", thread.WorkspacePath, worktreeDir)
	}
	if !samePath(thread.WorktreePath, worktreeDir) {
		t.Errorf("WorktreePath = %q, want %q", thread.WorktreePath, worktreeDir)
	}
	if thread.Branch != "feat/foo" {
		t.Errorf("Branch = %q, want feat/foo", thread.Branch)
	}
}

// CreateThread rejects a WorktreePath that isn't actually a registered
// worktree of the project. Without this check, a misbehaving (or future
// careless) caller could spawn a provider session with WorkDir set to
// any path on disk — directly under LocalOnlyMethods is enough for now,
// but defense in depth is cheap.
func TestCreateThreadRejectsUnknownWorktreePath(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace: %v", err)
	}

	bogus := filepath.Join(t.TempDir(), "not-a-worktree")
	if err := os.MkdirAll(bogus, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	_, err = app.CreateThread(CreateThreadOptions{
		ProjectID:    project.ID,
		Provider:     string(provider.Codex),
		Model:        "gpt-5.4",
		Mode:         "chat",
		WorktreePath: bogus,
	})
	if err == nil {
		t.Fatal("CreateThread accepted a non-worktree path; expected rejection")
	}
	if !strings.Contains(err.Error(), "not a worktree") {
		t.Fatalf("error = %q, want substring 'not a worktree'", err)
	}
}

func TestCreateThreadDetectsGitProjectPath(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)
	workspace := filepath.Join(repo, "nested", "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	thread, err := createTestThread(t, app, string(provider.Codex), workspace, "gpt-5.4", "")
	if err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	project, err := app.store.GetProject(thread.ProjectID)
	if err != nil {
		t.Fatalf("GetProject() error = %v", err)
	}
	if !samePath(project.Path, repo) {
		t.Fatalf("returned project.Path = %q, want %q", project.Path, repo)
	}
	if !samePath(thread.ProjectPath, repo) {
		t.Fatalf("returned thread.ProjectPath = %q, want %q", thread.ProjectPath, repo)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.ProjectID != project.ID {
		t.Fatalf("stored ProjectID = %q, want %q", stored.ProjectID, project.ID)
	}
	if !samePath(stored.ProjectPath, repo) {
		t.Fatalf("stored ProjectPath = %q, want %q", stored.ProjectPath, repo)
	}
}

func TestCreateThreadAddsRecentWorkspace(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())

	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	if _, err := createTestThread(t, app, string(provider.Codex), workspace, "gpt-5.4", ""); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	got := app.settings.Get()
	if len(got.RecentWorkspaces) != 1 {
		t.Fatalf("len(RecentWorkspaces) = %d, want 1", len(got.RecentWorkspaces))
	}
	if !samePath(got.RecentWorkspaces[0], workspace) {
		t.Fatalf("RecentWorkspaces[0] = %q, want %q", got.RecentWorkspaces[0], workspace)
	}
}

func TestAutoResumeThreadIsNoOp(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-auto")
	thread.SessionRef = "provider-session-1"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	started := make(chan string, 1)
	app.startSessionFn = func(threadID string) error {
		started <- threadID
		return nil
	}

	if err := app.AutoResumeThread(thread.ID); err != nil {
		t.Fatalf("AutoResumeThread() error = %v", err)
	}

	select {
	case threadID := <-started:
		t.Fatalf("unexpected session start for %s — AutoResumeThread should be a no-op", threadID)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestSwitchThreadMarksThreadReadBeforeReturning(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-switch-read")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	completedAt := time.Now().UnixMilli() + 60_000
	insertCompletedTurnForAppTest(t, app, thread.ID, "turn-switch-read", completedAt-1000, completedAt)

	got, err := app.SwitchThread(thread.ID)
	if err != nil {
		t.Fatalf("SwitchThread() error = %v", err)
	}
	if got.LatestTurnCompletedAt == nil {
		t.Fatalf("returned LatestTurnCompletedAt = nil, want completed turn")
	}
	if got.LastReadAt == nil {
		t.Fatalf("returned LastReadAt = nil, want >= latest turn completed")
	}
	if *got.LastReadAt < *got.LatestTurnCompletedAt {
		t.Fatalf("returned LastReadAt = %d, want >= latest turn completed %d", *got.LastReadAt, *got.LatestTurnCompletedAt)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.LatestTurnCompletedAt == nil {
		t.Fatalf("stored LatestTurnCompletedAt = nil, want completed turn")
	}
	if stored.LastReadAt == nil {
		t.Fatalf("stored LastReadAt = nil, want >= latest turn completed")
	}
	if *stored.LastReadAt < *stored.LatestTurnCompletedAt {
		t.Fatalf("stored LastReadAt = %d, want >= latest turn completed %d", *stored.LastReadAt, *stored.LatestTurnCompletedAt)
	}
}

func TestReconnectSessionStopsThenStarts(t *testing.T) {
	app := &App{}
	var calls []string
	app.stopSessionFn = func(threadID string) error {
		calls = append(calls, "stop:"+threadID)
		return nil
	}
	app.startSessionFn = func(threadID string) error {
		calls = append(calls, "start:"+threadID)
		return nil
	}

	if err := app.ReconnectSession("thread-1"); err != nil {
		t.Fatalf("ReconnectSession() error = %v", err)
	}

	want := []string{"stop:thread-1", "start:thread-1"}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("calls[%d] = %q, want %q", i, calls[i], want[i])
		}
	}
}

func TestUpdateThreadModelUpdatesStoredModelWithoutRestartWhenSessionInactive(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-model-inactive")
	thread.UpdatedAt = 1_700_000_000_000
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	started := false
	app.startSessionFn = func(string) error {
		started = true
		return nil
	}

	updated, err := app.UpdateThreadModel(thread.ID, "gpt-5.4-mini")
	if err != nil {
		t.Fatalf("UpdateThreadModel() error = %v", err)
	}
	if started {
		t.Fatal("UpdateThreadModel() unexpectedly restarted an inactive thread")
	}
	if updated.Model != "gpt-5.4-mini" {
		t.Fatalf("returned model = %q, want %q", updated.Model, "gpt-5.4-mini")
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Model != "gpt-5.4-mini" {
		t.Fatalf("stored model = %q, want %q", stored.Model, "gpt-5.4-mini")
	}
	if stored.UpdatedAt != thread.UpdatedAt {
		t.Fatalf("stored UpdatedAt = %d, want %d", stored.UpdatedAt, thread.UpdatedAt)
	}
}

func TestUpdateThreadModelRemembersClaudeModelAndContextDefaults(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())

	thread, err := createTestThread(t, app, "claude", "/tmp/claude-model-context", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	if thread.ContextWindow != provider.ClaudeStandardContextWindow {
		t.Fatalf("initial sonnet context = %d, want %d", thread.ContextWindow, provider.ClaudeStandardContextWindow)
	}

	// Switching models adopts the *new* model's registry default, and the
	// large models default to the 1M tier while Sonnet keeps 1M opt-in — so
	// this round-trip also proves the two defaults stay independent.
	opus, err := app.UpdateThreadModel(thread.ID, "claude-opus-4-7")
	if err != nil {
		t.Fatalf("UpdateThreadModel(opus): %v", err)
	}
	if opus.ContextWindow != provider.ClaudeExtendedContextWindow {
		t.Fatalf("opus context = %d, want %d", opus.ContextWindow, provider.ClaudeExtendedContextWindow)
	}

	sonnet, err := app.UpdateThreadModel(thread.ID, "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("UpdateThreadModel(sonnet): %v", err)
	}
	if sonnet.ContextWindow != provider.ClaudeStandardContextWindow {
		t.Fatalf("remembered sonnet context = %d, want %d", sonnet.ContextWindow, provider.ClaudeStandardContextWindow)
	}

	opusProfile, err := app.store.GetChatModelProfile("claude", "claude-opus-4-7")
	if err != nil {
		t.Fatalf("GetChatModelProfile(opus): %v", err)
	}
	if opusProfile.ContextWindow != provider.ClaudeExtendedContextWindow {
		t.Fatalf("stored opus context = %d, want %d", opusProfile.ContextWindow, provider.ClaudeExtendedContextWindow)
	}

	sonnetProfile, err := app.store.GetChatModelProfile("claude", "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("GetChatModelProfile(sonnet): %v", err)
	}
	if sonnetProfile.ContextWindow != provider.ClaudeStandardContextWindow {
		t.Fatalf("stored sonnet context = %d, want %d", sonnetProfile.ContextWindow, provider.ClaudeStandardContextWindow)
	}
}

func TestUpdateThreadModelSanitizesStaleProfileContext(t *testing.T) {
	app := newTestAppWithStore(t)
	if err := app.store.UpsertChatModelProfile(store.ChatModelProfile{
		Provider:        "codex",
		Model:           "gpt-5.3-codex-spark",
		ReasoningEffort: "high",
		ContextWindow:   provider.CodexStandardContextWindow,
		RuntimeMode:     "default",
	}); err != nil {
		t.Fatalf("UpsertChatModelProfile: %v", err)
	}
	thread, err := createTestThread(t, app, "codex", "/tmp/spark-stale-profile", "gpt-5.5", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	updated, err := app.UpdateThreadModel(thread.ID, "gpt-5.3-codex-spark")
	if err != nil {
		t.Fatalf("UpdateThreadModel(spark): %v", err)
	}
	if updated.ContextWindow != provider.CodexSparkContextWindow {
		t.Fatalf("ContextWindow = %d, want spark %d", updated.ContextWindow, provider.CodexSparkContextWindow)
	}
}

func TestUpdateThreadModelSelectionUsesTargetProviderModelProfile(t *testing.T) {
	app := newTestAppWithStore(t)
	if err := app.store.UpsertChatModelProfile(store.ChatModelProfile{
		Provider:        "codex",
		Model:           "gpt-5.4",
		ReasoningEffort: "xhigh",
		FastMode:        true,
		ContextWindow:   provider.CodexExtendedContextWindow,
		RuntimeMode:     "auto-accept-edits",
	}); err != nil {
		t.Fatalf("UpsertChatModelProfile: %v", err)
	}
	thread, err := createTestThread(t, app, "claude", "/tmp/provider-model-profile", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	thread.ReasoningEffort = "max"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("UpdateThread(max): %v", err)
	}

	updated, err := app.UpdateThreadModelSelection(thread.ID, "codex", "gpt-5.4")
	if err != nil {
		t.Fatalf("UpdateThreadModelSelection: %v", err)
	}
	if updated.Provider != "codex" || updated.Model != "gpt-5.4" {
		t.Fatalf("provider/model = %s/%s, want codex/gpt-5.4", updated.Provider, updated.Model)
	}
	if updated.ReasoningEffort != "xhigh" {
		t.Fatalf("ReasoningEffort = %q, want xhigh", updated.ReasoningEffort)
	}
	if !updated.FastMode {
		t.Fatal("FastMode = false, want true from saved profile")
	}
	if updated.ContextWindow != provider.CodexExtendedContextWindow {
		t.Fatalf("ContextWindow = %d, want %d", updated.ContextWindow, provider.CodexExtendedContextWindow)
	}
	if updated.RuntimeMode != "auto-accept-edits" {
		t.Fatalf("RuntimeMode = %q, want auto-accept-edits", updated.RuntimeMode)
	}
}

func TestUpdateThreadModelSelectionClampsStaleCodexGPT55Profile(t *testing.T) {
	app := newTestAppWithStore(t)
	if err := app.store.UpsertChatModelProfile(store.ChatModelProfile{
		Provider:        "codex",
		Model:           "gpt-5.5",
		ReasoningEffort: "medium",
		FastMode:        true,
		ContextWindow:   provider.CodexExtendedContextWindow,
		RuntimeMode:     "auto-accept-edits",
	}); err != nil {
		t.Fatalf("UpsertChatModelProfile: %v", err)
	}
	thread, err := createTestThread(t, app, "claude", "/tmp/provider-model-stale-gpt55-profile", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	updated, err := app.UpdateThreadModelSelection(thread.ID, "codex", "gpt-5.5")
	if err != nil {
		t.Fatalf("UpdateThreadModelSelection: %v", err)
	}
	if updated.Provider != "codex" || updated.Model != "gpt-5.5" {
		t.Fatalf("provider/model = %s/%s, want codex/gpt-5.5", updated.Provider, updated.Model)
	}
	if updated.ContextWindow != provider.CodexStandardContextWindow {
		t.Fatalf("ContextWindow = %d, want %d", updated.ContextWindow, provider.CodexStandardContextWindow)
	}
}

func TestUpdateThreadModelSelectionFallsBackToTargetModelDefaults(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/provider-model-defaults", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	thread.ReasoningEffort = "max"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("UpdateThread(max): %v", err)
	}

	updated, err := app.UpdateThreadModelSelection(thread.ID, "codex", "gpt-5.5")
	if err != nil {
		t.Fatalf("UpdateThreadModelSelection: %v", err)
	}
	if updated.Provider != "codex" || updated.Model != "gpt-5.5" {
		t.Fatalf("provider/model = %s/%s, want codex/gpt-5.5", updated.Provider, updated.Model)
	}
	if updated.ReasoningEffort != "medium" {
		t.Fatalf("ReasoningEffort = %q, want gpt-5.5 default medium", updated.ReasoningEffort)
	}
}

func TestUpdateThreadModelSelectionClearsProviderResumeRefs(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/provider-model-refs", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	thread.SessionRef = "claude-session"
	thread.PendingForkRef = "claude-fork"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("UpdateThread(refs): %v", err)
	}

	updated, err := app.UpdateThreadModelSelection(thread.ID, "codex", "gpt-5.5")
	if err != nil {
		t.Fatalf("UpdateThreadModelSelection: %v", err)
	}
	if updated.SessionRef != "" || updated.PendingForkRef != "" {
		t.Fatalf("refs = session %q pending %q, want both cleared", updated.SessionRef, updated.PendingForkRef)
	}
}

func TestUpdateThreadModelSelectionRejectsCrossProviderAfterItems(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/provider-model-locked", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	now := time.Now().UnixMilli()
	if err := app.store.InsertItem(store.Item{
		ID:        "provider-model-lock-item",
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

	_, err = app.UpdateThreadModelSelection(thread.ID, "codex", "gpt-5.5")
	if err == nil {
		t.Fatal("UpdateThreadModelSelection on used thread error = nil, want lock error")
	}
	if !strings.Contains(err.Error(), "locked to claude") {
		t.Fatalf("UpdateThreadModelSelection error = %v, want locked to claude", err)
	}
	after, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if after.Provider != "claude" || after.Model != "claude-sonnet-4-6" {
		t.Fatalf("provider/model after rejection = %s/%s, want claude/claude-sonnet-4-6", after.Provider, after.Model)
	}
}

func TestSwitchThreadSanitizesStaleSparkContext(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "codex", "/tmp/spark-stale-thread", "gpt-5.3-codex-spark", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	before := thread.UpdatedAt
	thread.ContextWindow = provider.CodexStandardContextWindow
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("UpdateThread stale context: %v", err)
	}

	switched, err := app.SwitchThread(thread.ID)
	if err != nil {
		t.Fatalf("SwitchThread: %v", err)
	}
	if switched.ContextWindow != provider.CodexSparkContextWindow {
		t.Fatalf("ContextWindow = %d, want spark %d", switched.ContextWindow, provider.CodexSparkContextWindow)
	}
	if switched.UpdatedAt != before {
		t.Fatalf("UpdatedAt = %d, want %d", switched.UpdatedAt, before)
	}
}

func TestUpdateThreadProviderDoesNotRememberTransientProviderModelPair(t *testing.T) {
	app := newTestAppWithStore(t)

	thread, err := createTestThread(t, app, "claude", "/tmp/provider-transient", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	if _, err := app.UpdateThreadProvider(thread.ID, "codex"); err != nil {
		t.Fatalf("UpdateThreadProvider: %v", err)
	}

	_, profileErr := app.store.GetChatModelProfile("codex", "claude-sonnet-4-6")
	if !errors.Is(profileErr, sql.ErrNoRows) {
		t.Fatalf("transient profile error = %v, want sql.ErrNoRows", profileErr)
	}
}

func TestCreateThreadUsesRememberedClaudeModelAndContext(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())

	source, err := createTestThread(t, app, "claude", "/tmp/remember-source", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("create source thread: %v", err)
	}
	if _, err := app.UpdateThreadModel(source.ID, "claude-opus-4-7"); err != nil {
		t.Fatalf("UpdateThreadModel(opus): %v", err)
	}

	next, err := createTestThread(t, app, "claude", "/tmp/remember-next", "", "")
	if err != nil {
		t.Fatalf("create next thread: %v", err)
	}
	if next.Model != "claude-opus-4-7" {
		t.Fatalf("next model = %q, want remembered opus", next.Model)
	}
	// The remembered opus profile carries opus's own registry default (1M),
	// not the sonnet window the source thread started on.
	if next.ContextWindow != provider.ClaudeExtendedContextWindow {
		t.Fatalf("next context = %d, want remembered %d", next.ContextWindow, provider.ClaudeExtendedContextWindow)
	}
	if next.Mode != "chat" {
		t.Fatalf("next mode = %q, want chat", next.Mode)
	}
}

func TestSwitchThreadDoesNotChangeRememberedContext(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())

	if err := app.store.UpsertChatModelProfile(store.ChatModelProfile{
		Provider:        "claude",
		Model:           "claude-sonnet-4-6",
		ReasoningEffort: "high",
		ContextWindow:   200000,
		RuntimeMode:     "default",
	}); err != nil {
		t.Fatalf("UpsertChatModelProfile: %v", err)
	}
	opened, err := createTestThread(t, app, "claude", "/tmp/open-1m", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("create opened thread: %v", err)
	}
	if err := app.store.UpdateContextSettings(opened.ID, 1000000, 0, 0); err != nil {
		t.Fatalf("UpdateContextSettings(opened): %v", err)
	}

	if _, err := app.SwitchThread(opened.ID); err != nil {
		t.Fatalf("SwitchThread(opened): %v", err)
	}

	next, err := createTestThread(t, app, "", "/tmp/new-after-open-1m", "", "")
	if err != nil {
		t.Fatalf("create next thread: %v", err)
	}
	if next.Model != "claude-sonnet-4-6" {
		t.Fatalf("next model = %q, want claude-sonnet-4-6", next.Model)
	}
	if next.ContextWindow != 200000 {
		t.Fatalf("next context = %d, want remembered 200000", next.ContextWindow)
	}
}

// TestUpdateThreadModelReconnectsSessionWithoutLiveUpdateSurface pins the
// restart-fallback path of the config reconciler: a registered session with
// no live-update surface (no provider handle to live-apply onto) cannot
// absorb a model change in place, so the deferred-reconnect watcher must
// restart it — asynchronously, once the thread is quiet — while the binding
// returns the persisted selection immediately.
func TestUpdateThreadModelReconnectsSessionWithoutLiveUpdateSurface(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-model-active")
	thread.UpdatedAt = 1_700_000_000_000
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	app.sessions[thread.ID] = session{provider: string(provider.Codex), token: "active-model-token"}

	started := make(chan string, 1)
	app.startSessionFn = func(threadID string) error {
		started <- threadID
		return nil
	}

	updated, err := app.UpdateThreadModel(thread.ID, "gpt-5.4-mini")
	if err != nil {
		t.Fatalf("UpdateThreadModel() error = %v", err)
	}
	if updated.Model != "gpt-5.4-mini" {
		t.Fatalf("returned model = %q, want %q", updated.Model, "gpt-5.4-mini")
	}
	if updated.UpdatedAt != thread.UpdatedAt {
		t.Fatalf("returned UpdatedAt = %d, want %d", updated.UpdatedAt, thread.UpdatedAt)
	}

	select {
	case threadID := <-started:
		if threadID != thread.ID {
			t.Fatalf("startSession thread = %q, want %q", threadID, thread.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deferred config reconnect never restarted the session")
	}
}

// TestUpdateThreadModelKeepsSelectionOnRestartFailure pins the new failure
// semantics: a failed reconnect no longer rolls the row back — the
// persisted selection stays authoritative (surfaced as thread error state)
// and a later lazy start converges on it.
func TestUpdateThreadModelKeepsSelectionOnRestartFailure(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-model-restart-failure")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	app.sessions[thread.ID] = session{provider: string(provider.Codex), token: "active-model-token"}
	restartAttempted := make(chan struct{}, 1)
	app.startSessionFn = func(string) error {
		restartAttempted <- struct{}{}
		return fmt.Errorf("restart boom")
	}

	updated, err := app.UpdateThreadModel(thread.ID, "gpt-5.4-mini")
	if err != nil {
		t.Fatalf("UpdateThreadModel() error = %v", err)
	}
	if updated.Model != "gpt-5.4-mini" {
		t.Fatalf("returned model = %q, want %q", updated.Model, "gpt-5.4-mini")
	}

	select {
	case <-restartAttempted:
	case <-time.After(5 * time.Second):
		t.Fatal("deferred config reconnect never attempted a restart")
	}

	stored, getErr := app.store.GetThread(thread.ID)
	if getErr != nil {
		t.Fatalf("GetThread() error = %v", getErr)
	}
	if stored.Model != "gpt-5.4-mini" {
		t.Fatalf("stored model = %q, want persisted selection %q", stored.Model, "gpt-5.4-mini")
	}
}

func TestUnarchiveThreadRestoresThread(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-unarchive")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if err := app.ArchiveThread(thread.ID); err != nil {
		t.Fatalf("ArchiveThread() error = %v", err)
	}

	got, err := app.UnarchiveThread(thread.ID)
	if err != nil {
		t.Fatalf("UnarchiveThread() error = %v", err)
	}
	if got.ID != thread.ID {
		t.Fatalf("UnarchiveThread() id = %q, want %q", got.ID, thread.ID)
	}
	if got.Archived {
		t.Fatal("UnarchiveThread() returned archived=true, want false")
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Archived {
		t.Fatal("stored archived flag still true after UnarchiveThread()")
	}
}

func TestUnarchiveUnknownThreadReturnsError(t *testing.T) {
	app := newTestAppWithStore(t)

	if _, err := app.UnarchiveThread("does-not-exist"); err == nil {
		t.Fatal("UnarchiveThread() error = nil, want not-found")
	}
}

func TestUpdateThreadModelRejectsBlankModel(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-model-blank")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	_, err := app.UpdateThreadModel(thread.ID, "   ")
	if err == nil {
		t.Fatal("UpdateThreadModel() error = nil, want blank model validation error")
	}
	if !strings.Contains(err.Error(), "cannot be empty") {
		t.Fatalf("UpdateThreadModel() error = %v, want empty-model message", err)
	}
}

func newTestAppWithStore(t *testing.T) *App {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "app.db")
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
	})

	app := &App{
		store:                  st,
		settings:               settings.NewService(t.TempDir()),
		sessions:               make(map[string]session),
		startingSessions:       make(map[string]*sessionStart),
		reconnectingThreads:    make(map[string]bool),
		autoReconnectAttempted: make(map[string]bool),
		threadSystemPrompts:    make(map[string]string),
		deliberations:          make(map[string]*discussion.Deliberation),
		gitWatchPumps:          make(map[string]*gitWatchPump),
		gitWatchHandles:        make(map[string]*gitWatchPump),
	}
	app.appCtx, app.appCancel = context.WithCancel(context.Background())
	// The same structural spawn/home isolation setupE2EApp gets. This
	// fixture is the majority one (~600 call sites); before this call was
	// here, its defaults left `claude`/`codex` resolvable from PATH and
	// HOME real, and the only thing between a detached side-effect
	// goroutine (thread titles, commit messages) and the developer's real
	// ~/.claude was testThread() happening not to use the default title —
	// the exact shape of incident 2026-08-03. Includes the Codex catalog
	// stub and the textgen poison; tests that assert those behaviors
	// install their own fakes over the top.
	isolateE2EProviderSpawns(t, app)
	t.Cleanup(app.appCancel)
	ensureDefaultTestProject(t, app)
	return app
}

// resetProviderBinarySettings restores the bare-name binary defaults that
// isolateE2EProviderSpawns poisons. For tests that assert PATH-resolution and
// provider-fallback behavior through an injected lookPathFn — the fake makes
// a real spawn impossible, and the assertions are about the names "claude"
// and "codex", not about a poison path.
func resetProviderBinarySettings(t *testing.T, app *App) {
	t.Helper()
	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath": "claude",
		"codexBinaryPath":  "codex",
	}); err != nil {
		t.Fatalf("reset provider binary settings: %v", err)
	}
}

func insertCompletedTurnForAppTest(t *testing.T, app *App, threadID, turnID string, startedAt, completedAt int64) {
	t.Helper()
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    turnID,
		ThreadID:  threadID,
		TurnIndex: 0,
		StartedAt: startedAt,
	}); err != nil {
		t.Fatalf("InsertTurn(%s): %v", turnID, err)
	}
	if err := app.store.UpdateTurnCompleted(turnID, completedAt, "end_turn", "", "", ""); err != nil {
		t.Fatalf("UpdateTurnCompleted(%s): %v", turnID, err)
	}
}

// testThread returns a Thread with the v13 shape, pre-attached to a
// stable test project row. Callers must have created that project via
// newTestAppWithStore (it creates a default project so inline Thread
// literals can hang off a valid FK).
func testThread(id string) store.Thread {
	now := time.Now().UnixMilli()
	return store.Thread{
		ID:            id,
		ProjectID:     defaultTestProjectID,
		Title:         "Test Thread",
		Provider:      string(provider.Codex),
		WorkspacePath: "/tmp/workspace",
		Model:         "gpt-5.4",
		Mode:          "chat",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}
