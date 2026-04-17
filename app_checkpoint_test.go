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
		ID:              threadID,
		Title:           "Checkpoint Test",
		Provider:        provider,
		WorkspacePath:   workspace,
		Model:           "test",
		InteractionMode: "default",
		CreatedAt:       now,
		UpdatedAt:       now,
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
		ID:            "chk-" + threadID + "-" + string(rune('0'+turnIndex)),
		ThreadID:      threadID,
		TurnIndex:     turnIndex,
		RefName:       ref,
		CapturedAt:    time.Now().UnixMilli(),
		WorkspacePath: workspace,
	}
	if err := app.store.SaveCheckpoint(record); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}
	return record
}

// -- tests --

func TestGetTurnDiffBetweenTwoCheckpoints(t *testing.T) {
	app, workspace := newCheckpointTestApp(t)
	seedCheckpointThread(t, app, "t1", workspace, string(provider.Codex))

	captureForTest(t, app, "t1", workspace, 0)
	// Agent edits README between turn 0 and turn 1.
	writeCheckpointFile(t, workspace, "README", "modified\n")
	captureForTest(t, app, "t1", workspace, 1)

	diff, err := app.GetTurnDiff("t1", 0)
	if err != nil {
		t.Fatalf("get turn diff: %v", err)
	}
	if !strings.Contains(diff, "+modified") {
		t.Errorf("expected +modified in diff, got:\n%s", diff)
	}
}

func TestGetTurnDiffLatestTurnUsesWorktree(t *testing.T) {
	app, workspace := newCheckpointTestApp(t)
	seedCheckpointThread(t, app, "t1", workspace, string(provider.Codex))

	captureForTest(t, app, "t1", workspace, 0)
	// No turn 1 checkpoint. Modify worktree directly.
	writeCheckpointFile(t, workspace, "README", "live edit\n")

	diff, err := app.GetTurnDiff("t1", 0)
	if err != nil {
		t.Fatalf("get turn diff: %v", err)
	}
	if !strings.Contains(diff, "+live edit") {
		t.Errorf("expected +live edit in worktree diff, got:\n%s", diff)
	}
}

func TestGetTurnDiffMissingCheckpointErrors(t *testing.T) {
	app, workspace := newCheckpointTestApp(t)
	seedCheckpointThread(t, app, "t1", workspace, string(provider.Codex))

	if _, err := app.GetTurnDiff("t1", 0); err == nil {
		t.Errorf("expected error when no checkpoint exists")
	}
}

func TestGetCheckpointToWorktreeDiffShowsPendingChanges(t *testing.T) {
	app, workspace := newCheckpointTestApp(t)
	seedCheckpointThread(t, app, "t1", workspace, string(provider.Codex))

	captureForTest(t, app, "t1", workspace, 0)
	writeCheckpointFile(t, workspace, "new-file.txt", "brand new\n")

	diff, err := app.GetCheckpointToWorktreeDiff("t1", 0)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !strings.Contains(diff, "new-file.txt") {
		t.Errorf("expected new-file.txt in diff, got:\n%s", diff)
	}
}

func TestRevertToTurnForkReturnsNewThreadID(t *testing.T) {
	app, workspace := newCheckpointTestApp(t)
	// Claude thread so fork path picks the pending-ref branch.
	thread := seedCheckpointThread(t, app, "t-claude", workspace, string(provider.Claude))
	thread.SessionRef = "claude-session-ref"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	// ForkThread requires at least one timeline item.
	if err := app.store.InsertItem(store.Item{
		ID: "item-1", ThreadID: "t-claude", TurnIndex: 1, ItemIndex: 0,
		Kind: "text", Role: "user", Summary: "hi", CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("insert item: %v", err)
	}
	captureForTest(t, app, "t-claude", workspace, 0)

	newID, err := app.RevertToTurn("t-claude", 0, "fork")
	if err != nil {
		t.Fatalf("revert fork: %v", err)
	}
	if newID == "" {
		t.Errorf("expected new thread ID from fork")
	}
	if newID == "t-claude" {
		t.Errorf("fork should create a NEW thread, got same id")
	}
	if _, err := app.store.GetThread(newID); err != nil {
		t.Errorf("forked thread should be retrievable: %v", err)
	}
}

func TestRevertToTurnRestoreRejectsClaude(t *testing.T) {
	app, workspace := newCheckpointTestApp(t)
	seedCheckpointThread(t, app, "t-claude", workspace, string(provider.Claude))
	captureForTest(t, app, "t-claude", workspace, 0)

	_, err := app.RevertToTurn("t-claude", 0, "restore")
	if err == nil {
		t.Errorf("expected error: Claude threads cannot be restored in place")
	}
	if !strings.Contains(err.Error(), "fork") {
		t.Errorf("error should suggest fork mode, got: %v", err)
	}
}

func TestRevertToTurnRestoreAppliesForCodex(t *testing.T) {
	app, workspace := newCheckpointTestApp(t)
	seedCheckpointThread(t, app, "t-codex", workspace, string(provider.Codex))

	captureForTest(t, app, "t-codex", workspace, 0)
	writeCheckpointFile(t, workspace, "README", "junk\n")
	writeCheckpointFile(t, workspace, "agent-junk.txt", "should go\n")

	if _, err := app.RevertToTurn("t-codex", 0, "restore"); err != nil {
		t.Fatalf("revert restore: %v", err)
	}

	if got, _ := os.ReadFile(filepath.Join(workspace, "README")); string(got) != "hello\n" {
		t.Errorf("README not restored: %q", got)
	}
	if _, err := os.Stat(filepath.Join(workspace, "agent-junk.txt")); err == nil {
		t.Errorf("agent-junk.txt should have been removed by restore")
	}
}

func TestRevertToTurnUnknownModeErrors(t *testing.T) {
	app, workspace := newCheckpointTestApp(t)
	seedCheckpointThread(t, app, "t1", workspace, string(provider.Codex))
	captureForTest(t, app, "t1", workspace, 0)

	if _, err := app.RevertToTurn("t1", 0, "wat"); err == nil {
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
