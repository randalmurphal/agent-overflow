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

func captureForTest(t *testing.T, app *App, threadID, workspace string, turnIndex int, toolPaths ...string) store.Checkpoint {
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
		ToolPaths:           toolPaths,
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
	// Isolate from the developer's real ~/.claude/projects so the
	// LocateSessionFile fallback scan can't accidentally match a real
	// session UUID. The test exercises the file-not-found fallback path
	// in revertClaudeThread.
	t.Setenv("HOME", t.TempDir())
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
	t.Setenv("HOME", t.TempDir())
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
	t.Setenv("HOME", t.TempDir())
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
	// Turn 0 is the baseline; turn 1 captures the agent's edit to
	// README. The path-scoped restore looks at tool_paths from
	// post-target rows, so the turn-1 row tells the revert "README is in
	// scope".
	captureForTest(t, app, "t-claude", workspace, 0)
	writeCheckpointFile(t, workspace, "README", "agent-edited\n")
	captureForTest(t, app, "t-claude", workspace, 1, "README")
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

// TestRevertToCheckpointConversationOnlyOnClaudeWithJSONLSlicesAndKeepsContext
// pins the new in-place revert behavior for Claude: when the source
// session JSONL is on disk, the revert slices it and points SessionRef
// at a new <newID>.jsonl, preserving prior context. The old JSONL is
// left in place for user recovery.
func TestRevertToCheckpointConversationOnlyOnClaudeWithJSONLSlicesAndKeepsContext(t *testing.T) {
	app, workspace := newCheckpointTestApp(t)

	// Build a fake ~/.claude/projects layout.
	home := t.TempDir()
	t.Setenv("HOME", home)
	canonical, _ := filepath.EvalSymlinks(workspace)
	abs, _ := filepath.Abs(canonical)
	slug := "-" + filepath.ToSlash(abs)[1:]
	for i, c := range slug {
		if c == '/' {
			slug = slug[:i] + "-" + slug[i+1:]
		}
	}
	projectDir := filepath.Join(home, ".claude", "projects", slug)
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	sessionID := "src-revert-uuid"
	jsonlPath := filepath.Join(projectDir, sessionID+".jsonl")
	jsonl := `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"src-revert-uuid","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"src-revert-uuid","message":{"role":"assistant","content":[{"type":"text","text":"r0"}]}}
{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"src-revert-uuid","message":{"role":"user","content":"second"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"src-revert-uuid","message":{"role":"assistant","content":[{"type":"text","text":"r1"}]}}
{"type":"user","uuid":"u2","parentUuid":"a1","sessionId":"src-revert-uuid","message":{"role":"user","content":"third"}}
{"type":"assistant","uuid":"a2","parentUuid":"u2","sessionId":"src-revert-uuid","message":{"role":"assistant","content":[{"type":"text","text":"r2"}]}}
`
	if err := os.WriteFile(jsonlPath, []byte(jsonl), 0o600); err != nil {
		t.Fatalf("write source jsonl: %v", err)
	}

	thread := seedCheckpointThread(t, app, "t-claude-revert", workspace, string(provider.Claude))
	thread.SessionRef = sessionID
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	for _, turn := range []int{0, 1, 2} {
		if err := app.store.InsertItem(store.Item{
			ID: "rv-" + string(rune('0'+turn)), ThreadID: "t-claude-revert",
			TurnIndex: turn, ItemIndex: 0, Kind: "user_text", Role: "user",
			Summary: "x", CreatedAt: time.Now().UnixMilli(),
		}); err != nil {
			t.Fatalf("insert item: %v", err)
		}
	}
	captureForTest(t, app, "t-claude-revert", workspace, 1)

	if err := app.RevertToCheckpoint("t-claude-revert", 1, RevertModeConversationOnly); err != nil {
		t.Fatalf("revert: %v", err)
	}

	got, err := app.store.GetThread("t-claude-revert")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if got.SessionRef == "" || got.SessionRef == sessionID {
		t.Errorf("SessionRef should be a fresh UUID after slice, got %q (source was %q)", got.SessionRef, sessionID)
	}

	// New <newID>.jsonl exists in the project dir.
	newPath := filepath.Join(projectDir, got.SessionRef+".jsonl")
	if _, err := os.Stat(newPath); err != nil {
		t.Errorf("new sliced JSONL missing at %s: %v", newPath, err)
	}

	// Source JSONL must be byte-stable (user recovery).
	srcAfter, _ := os.ReadFile(jsonlPath)
	if string(srcAfter) != jsonl {
		t.Errorf("source JSONL was mutated by revert; should be untouched")
	}

	// Items truncated to turn 0 only.
	items, _ := app.store.ListItems("t-claude-revert")
	if len(items) != 1 {
		t.Errorf("expected 1 item (turn 0), got %d", len(items))
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
