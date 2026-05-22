package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/discussion"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
)

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
		t.Fatalf("first codex model = %q, want gpt-5.5", codexModels[0].Slug)
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
    printf '{"jsonrpc":"2.0","id":%s,"result":{"data":[{"model":"gpt-5.5","displayName":"GPT-5.5","hidden":false,"supportedReasoningEfforts":[{"reasoningEffort":"high","description":"High"}],"defaultReasoningEffort":"high","additionalSpeedTiers":["fast"]}],"nextCursor":null}}\n' "$id"
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
    printf '{"jsonrpc":"2.0","id":%%s,"result":{"data":[{"model":%[2]q,"displayName":%[2]q,"hidden":false,"supportedReasoningEfforts":[{"reasoningEffort":"high","description":"High"}],"defaultReasoningEffort":"high","additionalSpeedTiers":["fast"]}],"nextCursor":null}}\n' "$id"
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

func TestSwitchThreadAutoResumesStoredSession(t *testing.T) {
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

	got, err := app.SwitchThread(thread.ID)
	if err != nil {
		t.Fatalf("SwitchThread() error = %v", err)
	}
	if got.ID != thread.ID {
		t.Fatalf("thread ID = %q, want %q", got.ID, thread.ID)
	}
	if err := app.AutoResumeThread(thread.ID); err != nil {
		t.Fatalf("AutoResumeThread() error = %v", err)
	}

	select {
	case threadID := <-started:
		if threadID != thread.ID {
			t.Fatalf("startSession thread = %q, want %q", threadID, thread.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for auto-resume")
	}
}

func TestSwitchThreadAutoResumeSessionInitDoesNotTouchUpdatedAt(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-auto-preserve-updated")
	thread.SessionRef = "provider-session-1"
	thread.UpdatedAt = 1000
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	insertCompletedTurnForAppTest(t, app, thread.ID, "turn-auto-preserve-updated", 1200, 1500)

	started := make(chan struct{}, 1)
	app.startSessionFn = func(threadID string) error {
		if err := app.store.UpdateSessionRef(threadID, "provider-session-2"); err != nil {
			return err
		}
		started <- struct{}{}
		return nil
	}

	if _, err := app.SwitchThread(thread.ID); err != nil {
		t.Fatalf("SwitchThread() error = %v", err)
	}
	if err := app.AutoResumeThread(thread.ID); err != nil {
		t.Fatalf("AutoResumeThread() error = %v", err)
	}

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for auto-resume")
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.UpdatedAt != thread.UpdatedAt {
		t.Fatalf("UpdatedAt = %d, want %d", stored.UpdatedAt, thread.UpdatedAt)
	}
	if stored.LastReadAt == nil {
		t.Fatalf("LastReadAt = nil, want thread marked read")
	}
	if stored.LatestTurnCompletedAt == nil {
		t.Fatalf("LatestTurnCompletedAt = nil, want completed turn")
	}
	if *stored.LastReadAt < *stored.LatestTurnCompletedAt {
		t.Fatalf("LastReadAt = %d, want >= latest turn completed %d", *stored.LastReadAt, *stored.LatestTurnCompletedAt)
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

func TestSwitchThreadCoalescesConcurrentAutoResume(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-auto-coalesce")
	thread.SessionRef = "provider-session-1"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	started := make(chan string, 2)
	release := make(chan struct{})
	app.startSessionFn = func(threadID string) error {
		started <- threadID
		<-release
		return nil
	}

	if _, err := app.SwitchThread(thread.ID); err != nil {
		t.Fatalf("first SwitchThread() error = %v", err)
	}
	if err := app.AutoResumeThread(thread.ID); err != nil {
		t.Fatalf("first AutoResumeThread() error = %v", err)
	}

	select {
	case threadID := <-started:
		if threadID != thread.ID {
			t.Fatalf("startSession thread = %q, want %q", threadID, thread.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first auto-resume")
	}

	if _, err := app.SwitchThread(thread.ID); err != nil {
		t.Fatalf("second SwitchThread() error = %v", err)
	}
	if err := app.AutoResumeThread(thread.ID); err != nil {
		t.Fatalf("second AutoResumeThread() error = %v", err)
	}

	select {
	case threadID := <-started:
		t.Fatalf("unexpected duplicate auto-resume for %s", threadID)
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
}

func TestSwitchThreadSkipsAutoResumeWithoutSessionRef(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-no-resume")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	started := make(chan string, 1)
	app.startSessionFn = func(threadID string) error {
		started <- threadID
		return nil
	}

	if _, err := app.SwitchThread(thread.ID); err != nil {
		t.Fatalf("SwitchThread() error = %v", err)
	}
	if err := app.AutoResumeThread(thread.ID); err != nil {
		t.Fatalf("AutoResumeThread() error = %v", err)
	}

	select {
	case threadID := <-started:
		t.Fatalf("unexpected auto-resume for %s", threadID)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestSwitchThreadAutoResumeFailureEmitsErrorEvent(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-resume-error")
	thread.SessionRef = "provider-session-1"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	emitted := collectErrorItemUpserts(t, app, 1)
	app.startSessionFn = func(threadID string) error {
		return fmt.Errorf("boom")
	}

	if _, err := app.SwitchThread(thread.ID); err != nil {
		t.Fatalf("SwitchThread() error = %v", err)
	}
	if err := app.AutoResumeThread(thread.ID); err != nil {
		t.Fatalf("AutoResumeThread() error = %v", err)
	}

	select {
	case item := <-emitted:
		if item.ThreadID != thread.ID {
			t.Fatalf("error thread = %q, want %q", item.ThreadID, thread.ID)
		}
		if item.Summary != "auto-resume failed: boom" {
			t.Fatalf("error content = %q, want %q", item.Summary, "auto-resume failed: boom")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for auto-resume error item")
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
	if thread.ContextWindow != 200000 {
		t.Fatalf("initial sonnet context = %d, want 200000", thread.ContextWindow)
	}

	opus, err := app.UpdateThreadModel(thread.ID, "claude-opus-4-7")
	if err != nil {
		t.Fatalf("UpdateThreadModel(opus): %v", err)
	}
	if opus.ContextWindow != 200000 {
		t.Fatalf("opus context = %d, want 200000", opus.ContextWindow)
	}

	sonnet, err := app.UpdateThreadModel(thread.ID, "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("UpdateThreadModel(sonnet): %v", err)
	}
	if sonnet.ContextWindow != 200000 {
		t.Fatalf("remembered sonnet context = %d, want 200000", sonnet.ContextWindow)
	}

	opusProfile, err := app.store.GetChatModelProfile("claude", "claude-opus-4-7")
	if err != nil {
		t.Fatalf("GetChatModelProfile(opus): %v", err)
	}
	if opusProfile.ContextWindow != 200000 {
		t.Fatalf("stored opus context = %d, want 200000", opusProfile.ContextWindow)
	}

	sonnetProfile, err := app.store.GetChatModelProfile("claude", "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("GetChatModelProfile(sonnet): %v", err)
	}
	if sonnetProfile.ContextWindow != 200000 {
		t.Fatalf("stored sonnet context = %d, want 200000", sonnetProfile.ContextWindow)
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
	if next.ContextWindow != 200000 {
		t.Fatalf("next context = %d, want remembered 200000", next.ContextWindow)
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

func TestUpdateThreadModelRestartsActiveSession(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-model-active")
	thread.UpdatedAt = 1_700_000_000_000
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	app.sessions[thread.ID] = session{provider: string(provider.Codex), token: "active-model-token"}

	var started []string
	app.startSessionFn = func(threadID string) error {
		started = append(started, threadID)
		return nil
	}

	updated, err := app.UpdateThreadModel(thread.ID, "gpt-5.4-mini")
	if err != nil {
		t.Fatalf("UpdateThreadModel() error = %v", err)
	}
	if len(started) != 1 || started[0] != thread.ID {
		t.Fatalf("startSession calls = %v, want [%s]", started, thread.ID)
	}
	if updated.Model != "gpt-5.4-mini" {
		t.Fatalf("returned model = %q, want %q", updated.Model, "gpt-5.4-mini")
	}
	if updated.UpdatedAt != thread.UpdatedAt {
		t.Fatalf("returned UpdatedAt = %d, want %d", updated.UpdatedAt, thread.UpdatedAt)
	}
}

func TestUpdateThreadModelRollsBackOnRestartFailure(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-model-rollback")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	app.sessions[thread.ID] = session{provider: string(provider.Codex), token: "active-model-token"}
	app.startSessionFn = func(string) error {
		return fmt.Errorf("restart boom")
	}

	_, err := app.UpdateThreadModel(thread.ID, "gpt-5.4-mini")
	if err == nil {
		t.Fatal("UpdateThreadModel() error = nil, want restart failure")
	}
	if !strings.Contains(err.Error(), "restart session with updated model") {
		t.Fatalf("UpdateThreadModel() error = %v, want restart context", err)
	}

	stored, getErr := app.store.GetThread(thread.ID)
	if getErr != nil {
		t.Fatalf("GetThread() error = %v", getErr)
	}
	if stored.Model != thread.Model {
		t.Fatalf("stored model = %q, want rollback to %q", stored.Model, thread.Model)
	}
	_, profileErr := app.store.GetChatModelProfile(thread.Provider, "gpt-5.4-mini")
	if !errors.Is(profileErr, sql.ErrNoRows) {
		t.Fatalf("failed model profile error = %v, want sql.ErrNoRows", profileErr)
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
		store:               st,
		sessions:            make(map[string]session),
		startingSessions:    make(map[string]*sessionStart),
		threadSystemPrompts: make(map[string]string),
		deliberations:       make(map[string]*discussion.Deliberation),
		gitWatchPumps:       make(map[string]*gitWatchPump),
	}
	app.appCtx, app.appCancel = context.WithCancel(context.Background())
	t.Cleanup(app.appCancel)
	ensureDefaultTestProject(t, app)
	return app
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
