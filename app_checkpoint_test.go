package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/checkpoint"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// newCheckpointTestApp returns an App wired to a real temp store so the
// checkpoint bindings can be exercised end-to-end. The workspace is a real
// git repo at tmpDir; the thread row points at it.
func newCheckpointTestApp(t *testing.T) (*App, string) {
	t.Helper()

	app := newTestAppWithStore(t)
	app.checkpoints = checkpoint.NewStore()

	workspace := t.TempDir()
	runCheckpointTestCmd(t, workspace, "git", "init", "-q", "-b", "main")
	writeCheckpointFile(t, workspace, "README", "hello\n")
	runCheckpointTestCmd(t, workspace, "git", "add", "-A")
	runCheckpointTestCmd(t, workspace, "git",
		"-c", "user.email=tester@test",
		"-c", "user.name=Tester",
		"commit", "-q", "-m", "init")

	return app, workspace
}

func runCheckpointTestCmd(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Tester",
		"GIT_AUTHOR_EMAIL=tester@test",
		"GIT_COMMITTER_NAME=Tester",
		"GIT_COMMITTER_EMAIL=tester@test",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func writeCheckpointFile(t *testing.T, dir, name, body string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func seedCheckpointThread(t *testing.T, app *App, threadID, workspace string, provider string) store.Thread {
	t.Helper()
	now := time.Now().UnixMilli()
	thread := store.Thread{
		ID:            threadID,
		ProjectID:     defaultTestProjectID,
		Title:         "Checkpoint Test",
		Provider:      provider,
		WorkspacePath: workspace,
		Model:         "test",
		Mode:          "chat",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	return thread
}

func captureForTest(t *testing.T, app *App, threadID, workspace string, turnIndex int) store.Checkpoint {
	t.Helper()
	ref, err := app.checkpoints.CaptureBaseline(context.Background(), workspace, threadID, turnIndex)
	if err != nil {
		t.Fatalf("capture baseline: %v", err)
	}
	record := store.Checkpoint{
		ID:                  "chk-" + threadID + "-" + string(rune('0'+turnIndex)),
		ThreadID:            threadID,
		TurnIndex:           turnIndex,
		CheckpointTurnCount: turnIndex,
		RefName:             ref,
		CapturedAt:          time.Now().UnixMilli(),
		WorkspacePath:       workspace,
	}
	if err := app.store.SaveCheckpoint(record); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}
	return record
}

// -- tests --

func TestGetCheckpointRangeDiffBetweenTwoCheckpoints(t *testing.T) {
	app, workspace := newCheckpointTestApp(t)
	seedCheckpointThread(t, app, "t1", workspace, string(provider.Codex))

	captureForTest(t, app, "t1", workspace, 0)
	// Agent edits README between turn 0 and turn 1.
	writeCheckpointFile(t, workspace, "README", "modified\n")
	captureForTest(t, app, "t1", workspace, 1)

	diff, err := app.GetCheckpointRangeDiff("t1", 0, 1)
	if err != nil {
		t.Fatalf("get checkpoint range diff: %v", err)
	}
	if !strings.Contains(diff, "+modified") {
		t.Errorf("expected +modified in diff, got:\n%s", diff)
	}
}

func TestGetCheckpointRangeDiffMissingCheckpointErrors(t *testing.T) {
	app, workspace := newCheckpointTestApp(t)
	seedCheckpointThread(t, app, "t1", workspace, string(provider.Codex))

	if _, err := app.GetCheckpointRangeDiff("t1", 0, 1); err == nil {
		t.Errorf("expected error when no checkpoint exists")
	}
}

func TestRevertToCheckpointConversationOnlyOnClaudeClearsSessionRef(t *testing.T) {
	app, workspace := newCheckpointTestApp(t)
	thread := seedCheckpointThread(t, app, "t-claude", workspace, string(provider.Claude))
	thread.SessionRef = "claude-session-old"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	for _, turn := range []int{0, 1, 2} {
		if err := app.store.InsertItem(store.Item{
			ID: "item-t" + string(rune('0'+turn)), ThreadID: "t-claude",
			TurnIndex: turn, ItemIndex: 0, Kind: "user_text", Role: "user",
			Summary: "x", CreatedAt: time.Now().UnixMilli(),
		}); err != nil {
			t.Fatalf("insert item: %v", err)
		}
	}
	captureForTest(t, app, "t-claude", workspace, 1)

	writeCheckpointFile(t, workspace, "README", "still dirty\n")
	if err := app.RevertToCheckpoint("t-claude", 1, RevertModeConversationOnly); err != nil {
		t.Fatalf("revert checkpoint: %v", err)
	}

	got, err := app.store.GetThread("t-claude")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if got.SessionRef != "" {
		t.Errorf("SessionRef should be cleared, got %q", got.SessionRef)
	}

	items, _ := app.store.ListItems("t-claude")
	if len(items) != 1 {
		t.Errorf("expected 1 item (turn 0), got %d", len(items))
	}

	// Worktree was NOT restored — revert-conversation leaves files alone.
	if data, _ := os.ReadFile(filepath.Join(workspace, "README")); string(data) != "still dirty\n" {
		t.Errorf("worktree should be untouched by revert-conversation; got %q", data)
	}
}

func TestRevertToCheckpointKeepsConversationThroughCheckpointTurn(t *testing.T) {
	app, workspace := newCheckpointTestApp(t)
	thread := seedCheckpointThread(t, app, "t-claude", workspace, string(provider.Claude))
	thread.SessionRef = "claude-session-old"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	for _, turn := range []int{0, 1, 2} {
		if err := app.store.InsertItem(store.Item{
			ID: "item-checkpoint-" + string(rune('0'+turn)), ThreadID: "t-claude",
			TurnIndex: turn, ItemIndex: 0, Kind: "user_text", Role: "user",
			Summary: "x", CreatedAt: time.Now().UnixMilli(),
		}); err != nil {
			t.Fatalf("insert item: %v", err)
		}
	}
	captureForTest(t, app, "t-claude", workspace, 1)
	writeCheckpointFile(t, workspace, "README", "dirty\n")

	if err := app.RevertToCheckpoint("t-claude", 1, RevertModeConversationOnly); err != nil {
		t.Fatalf("revert checkpoint: %v", err)
	}

	items, _ := app.store.ListItems("t-claude")
	if len(items) != 1 {
		t.Fatalf("expected only turn 0 to survive, got %d items", len(items))
	}
	if items[0].TurnIndex != 0 {
		t.Fatalf("surviving turn = %d; want 0", items[0].TurnIndex)
	}
	if data, _ := os.ReadFile(filepath.Join(workspace, "README")); string(data) != "dirty\n" {
		t.Fatalf("conversation-only should keep current worktree, got %q", data)
	}
}

func TestRevertToCheckpointConversationAndFilesOnClaudeRestoresWorktreeAndClearsSession(t *testing.T) {
	app, workspace := newCheckpointTestApp(t)
	thread := seedCheckpointThread(t, app, "t-claude", workspace, string(provider.Claude))
	thread.SessionRef = "claude-session-old"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	for _, turn := range []int{0, 1} {
		if err := app.store.InsertItem(store.Item{
			ID: "it-" + string(rune('0'+turn)), ThreadID: "t-claude",
			TurnIndex: turn, ItemIndex: 0, Kind: "user_text", Role: "user",
			Summary: "x", CreatedAt: time.Now().UnixMilli(),
		}); err != nil {
			t.Fatalf("insert item: %v", err)
		}
	}
	captureForTest(t, app, "t-claude", workspace, 0)
	writeCheckpointFile(t, workspace, "README", "dirty\n")

	if err := app.RevertToCheckpoint("t-claude", 0, RevertModeConversationAndFiles); err != nil {
		t.Fatalf("revert checkpoint on Claude: %v", err)
	}

	got, _ := app.store.GetThread("t-claude")
	if got.SessionRef != "" {
		t.Errorf("SessionRef should be cleared, got %q", got.SessionRef)
	}
	if data, _ := os.ReadFile(filepath.Join(workspace, "README")); string(data) != "hello\n" {
		t.Errorf("README not restored: %q", data)
	}
	items, _ := app.store.ListItems("t-claude")
	if len(items) != 0 {
		t.Errorf("revert to turn 0 should drop all items, got %d", len(items))
	}
}

func TestRevertToCheckpointUnknownModeErrors(t *testing.T) {
	app, workspace := newCheckpointTestApp(t)
	seedCheckpointThread(t, app, "t1", workspace, string(provider.Codex))
	captureForTest(t, app, "t1", workspace, 0)

	if err := app.RevertToCheckpoint("t1", 0, "wat"); err == nil {
		t.Errorf("expected error for unknown mode")
	}
}

func TestListThreadCheckpointsOrdersByTurn(t *testing.T) {
	app, workspace := newCheckpointTestApp(t)
	seedCheckpointThread(t, app, "t1", workspace, string(provider.Codex))
	captureForTest(t, app, "t1", workspace, 0)
	writeCheckpointFile(t, workspace, "README", "v1\n")
	captureForTest(t, app, "t1", workspace, 1)

	list, err := app.ListThreadCheckpoints("t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 checkpoints, got %d", len(list))
	}
	if list[0].TurnIndex != 0 || list[1].TurnIndex != 1 {
		t.Errorf("ordering wrong: %+v", list)
	}
}

func TestDeleteThreadRemovesCheckpointRefsAndRows(t *testing.T) {
	app, workspace := newCheckpointTestApp(t)
	seedCheckpointThread(t, app, "t1", workspace, string(provider.Codex))
	captureForTest(t, app, "t1", workspace, 0)
	captureForTest(t, app, "t1", workspace, 1)

	refs, err := app.checkpoints.ListThreadRefs(context.Background(), workspace, "t1")
	if err != nil {
		t.Fatalf("list refs: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("pre: expected 2 refs, got %d", len(refs))
	}

	if err := app.DeleteThread("t1"); err != nil {
		t.Fatalf("delete thread: %v", err)
	}

	// Both refs should be gone.
	refs, err = app.checkpoints.ListThreadRefs(context.Background(), workspace, "t1")
	if err != nil {
		t.Fatalf("list refs post: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("expected 0 refs after delete, got %d: %v", len(refs), refs)
	}
	// Rows should be gone (CASCADE from thread delete or explicit DAO delete).
	rows, _ := app.store.ListCheckpoints("t1")
	if len(rows) != 0 {
		t.Errorf("expected 0 checkpoint rows, got %d", len(rows))
	}
}
