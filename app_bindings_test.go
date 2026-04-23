package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/discussion"
	"agent-overflow/internal/highlight"
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

	got, err := app.UpdateSettings(map[string]any{"defaultProvider": "codex"})
	if err != nil {
		t.Fatalf("UpdateSettings() error = %v", err)
	}
	if got.DefaultProvider != "codex" {
		t.Fatalf("DefaultProvider = %q, want codex", got.DefaultProvider)
	}

	reloaded := settings.NewService(dir).Get()
	if reloaded.DefaultProvider != "codex" {
		t.Fatalf("reloaded DefaultProvider = %q, want codex", reloaded.DefaultProvider)
	}
}

func TestGetModelsForProvider(t *testing.T) {
	app := &App{}

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

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.ProjectID != project.ID {
		t.Fatalf("stored ProjectID = %q, want %q", stored.ProjectID, project.ID)
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

	select {
	case threadID := <-started:
		if threadID != thread.ID {
			t.Fatalf("startSession thread = %q, want %q", threadID, thread.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for auto-resume")
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
	if opus.ContextWindow != 1000000 {
		t.Fatalf("opus context = %d, want 1000000", opus.ContextWindow)
	}

	sonnet, err := app.UpdateThreadModel(thread.ID, "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("UpdateThreadModel(sonnet): %v", err)
	}
	if sonnet.ContextWindow != 200000 {
		t.Fatalf("remembered sonnet context = %d, want 200000", sonnet.ContextWindow)
	}

	cfg := app.settings.Get()
	if cfg.DefaultModelClaude != "claude-sonnet-4-6" {
		t.Fatalf("DefaultModelClaude = %q, want claude-sonnet-4-6", cfg.DefaultModelClaude)
	}
	if cfg.ModelContextWindows["claude-opus-4-7"] != 1000000 {
		t.Fatalf("stored opus context = %d, want 1000000", cfg.ModelContextWindows["claude-opus-4-7"])
	}
	if cfg.ModelContextWindows["claude-sonnet-4-6"] != 200000 {
		t.Fatalf("stored sonnet context = %d, want 200000", cfg.ModelContextWindows["claude-sonnet-4-6"])
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
	if next.ContextWindow != 1000000 {
		t.Fatalf("next context = %d, want remembered 1000000", next.ContextWindow)
	}
	if next.Mode != "chat" {
		t.Fatalf("next mode = %q, want chat", next.Mode)
	}
}

func TestUpdateThreadModelRestartsActiveSession(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-model-active")
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
		threadSlashCommands: make(map[string][]string),
		deliberations:       make(map[string]*discussion.Deliberation),
		highlighter:         highlight.New(highlight.Options{}),
	}
	ensureDefaultTestProject(t, app)
	return app
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
