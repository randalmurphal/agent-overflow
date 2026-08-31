package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
)

// newTestApp is the light App fixture for the rollback / fork / revert
// tests: a real store cloned from the package's migrated template plus real
// settings, no triage router and no sessions unless the test wires them.
//
// It is send-capable — RevertConversationAndResendMessage and friends
// reach sendMessageLocked from here — so the provider-spawn isolation is
// applied INSIDE the fixture rather than left to each caller. A guard
// that regressed would otherwise resolve `claude` from PATH against the
// developer's real credentials; see isolateE2EProviderSpawns for what
// that costs. Callers that need a specific HOME set it after this
// returns (last t.Setenv wins); callers that need a live session install
// a mock binary over the poisoned default.
// Teardown is t.Cleanup-registered throughout (storetest.Clone closes
// its own store), so there is nothing for callers to defer.
func newTestApp(t *testing.T) *App {
	t.Helper()
	st := storetest.Clone(t)
	app := &App{store: st}
	app.setSettingsService(settings.NewService(t.TempDir()))
	// Registered before the cleanups below so (LIFO) its spawn check runs
	// last, once nothing is still in flight.
	isolateE2EProviderSpawns(t, app)
	app.appCtx, app.appCancel = context.WithCancel(context.Background())
	t.Cleanup(app.appCancel)
	return app
}

func createAppTestThread(t *testing.T, app *App, id, provider, workspace string) store.Thread {
	t.Helper()
	now := time.Now().UnixMilli()
	thread := store.Thread{
		ID:            id,
		ProjectID:     "p1",
		Title:         id,
		Provider:      provider,
		WorkspacePath: workspace,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if _, err := app.store.CreateProject(store.Project{ID: "p1", Path: workspace, Name: "p1", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	return thread
}

// seedMessageAnchor writes the per-message correlation row the send
// paths record via recordMessageAnchor.
func seedMessageAnchor(t *testing.T, st *store.Store, threadID, userItemID string, turnIndex int, providerUserMessageID, providerParentUUID string) {
	t.Helper()
	if err := st.UpsertMessageAnchor(store.MessageAnchor{
		ThreadID:              threadID,
		UserItemID:            userItemID,
		TurnIndex:             turnIndex,
		ProviderUserMessageID: providerUserMessageID,
		ProviderParentUUID:    providerParentUUID,
		CreatedAt:             time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("seed anchor %s/%s: %v", threadID, userItemID, err)
	}
}

func writeClaudeProjectSession(t *testing.T, home, workspace, sessionID, jsonl string) string {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatalf("eval workspace: %v", err)
	}
	abs, err := filepath.Abs(canonical)
	if err != nil {
		t.Fatalf("abs workspace: %v", err)
	}
	slug := "-" + strings.ReplaceAll(strings.TrimPrefix(filepath.ToSlash(abs), "/"), "/", "-")
	projectDir := filepath.Join(home, ".claude", "projects", slug)
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatalf("mkdir claude project: %v", err)
	}
	path := filepath.Join(projectDir, sessionID+".jsonl")
	if err := os.WriteFile(path, []byte(jsonl), 0o600); err != nil {
		t.Fatalf("write claude session: %v", err)
	}
	return path
}

func assertClaudeSessionText(t *testing.T, workspace, sessionID string, wantPresent []string, wantAbsent []string) {
	t.Helper()
	path, err := sessionfork.LocateSessionFile(testProviderProjectsDir(t), sessionID, workspace)
	if err != nil {
		t.Fatalf("locate claude session %q: %v", sessionID, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read claude session %s: %v", path, err)
	}
	text := string(data)
	for _, want := range wantPresent {
		if !strings.Contains(text, want) {
			t.Fatalf("claude session %q missing %q:\n%s", sessionID, want, text)
		}
	}
	for _, absent := range wantAbsent {
		if strings.Contains(text, absent) {
			t.Fatalf("claude session %q unexpectedly contains %q:\n%s", sessionID, absent, text)
		}
	}
}

func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "config", "user.email", "test@example.com")
	writeFile(t, dir, ".gitkeep", "")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "initial")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func insertUserItem(t *testing.T, st *store.Store, threadID, id string, turnIndex int, summary string) {
	t.Helper()
	insertUserItemWithMeta(t, st, threadID, id, turnIndex, summary, "")
}

func insertUserItemWithMeta(t *testing.T, st *store.Store, threadID, id string, turnIndex int, summary, meta string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := st.AppendItem(store.Item{
		ID:        id,
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   summary,
		Meta:      meta,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("append user item: %v", err)
	}
}

func insertAssistantTextItem(t *testing.T, st *store.Store, threadID, id string, turnIndex int, summary string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := st.AppendItem(store.Item{
		ID:        id,
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		Kind:      "assistant_text",
		Role:      "assistant",
		Status:    "completed",
		Summary:   summary,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("append assistant text item: %v", err)
	}
}

func insertRunningBackgroundToolCall(t *testing.T, st *store.Store, threadID, id string, turnIndex, itemIndex int) {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := st.AppendItem(store.Item{
		ID:           id,
		ThreadID:     threadID,
		TurnIndex:    turnIndex,
		ItemIndex:    itemIndex,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "running",
		Summary:      "Background task",
		IsBackground: true,
		Meta:         `{"live_background_active":true}`,
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("append running background tool call: %v", err)
	}
}
