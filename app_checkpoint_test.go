package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/checkpoint"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/store"
)

func TestRevertToMessageCheckpointDeletesSelectedPromptAndRestoresDraft(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := t.TempDir()
	const sessionID = "source-session"
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"source-session","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"source-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"source-session","message":{"role":"user","content":"second"}}
`)
	thread := createCheckpointTestThread(t, app, "t1", "claude", workspace)
	thread.SessionRef = sessionID
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	if err := app.store.SaveCheckpoint(store.Checkpoint{
		ID:                    "chk-1",
		ThreadID:              thread.ID,
		UserItemID:            "user:1",
		TurnIndex:             1,
		ProviderUserMessageID: "provider-user-1",
		RefName:               checkpoint.ThreadRefPrefix(thread.ID) + "message/test",
		CapturedAt:            time.Now().UnixMilli(),
		WorkspacePath:         thread.WorkspacePath,
	}); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	if err := app.RevertToMessageCheckpoint(thread.ID, "user:1", RevertModeConversationOnly); err != nil {
		t.Fatalf("revert: %v", err)
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 || items[0].ID != "user:0" {
		t.Fatalf("items after revert = %+v", items)
	}
	draft, ok, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("get draft: %v", err)
	}
	if !ok || draft.Content != "second" {
		t.Fatalf("draft = %+v ok=%v", draft, ok)
	}
}

func TestRevertToMessageCheckpointRestoresDraftAttachments(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createCheckpointTestThread(t, app, "t-attachments", "claude", t.TempDir())
	meta, err := json.Marshal(userMessageMeta{
		Attachments: []userMessageAttachmentMeta{
			{ID: "att-1", ThreadID: thread.ID, Filename: "one.png", MimeType: "image/png"},
			{ID: "att-2", ThreadID: thread.ID, Filename: "two.png", MimeType: "image/png"},
		},
	})
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	insertUserItemWithMeta(t, app.store, thread.ID, "user:0", 0, "look at [Image #1] and [Image #2]", string(meta))
	if err := app.store.SaveCheckpoint(store.Checkpoint{
		ID:                    "chk-attachments",
		ThreadID:              thread.ID,
		UserItemID:            "user:0",
		TurnIndex:             0,
		ProviderUserMessageID: "provider-user-0",
		RefName:               checkpoint.ThreadRefPrefix(thread.ID) + "message/attachments",
		CapturedAt:            time.Now().UnixMilli(),
		WorkspacePath:         thread.WorkspacePath,
	}); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	if err := app.RevertToMessageCheckpoint(thread.ID, "user:0", RevertModeConversationOnly); err != nil {
		t.Fatalf("revert: %v", err)
	}
	draft, ok, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("get draft: %v", err)
	}
	if !ok {
		t.Fatal("draft missing")
	}
	var got []string
	if err := json.Unmarshal([]byte(draft.Attachments), &got); err != nil {
		t.Fatalf("decode draft attachments: %v", err)
	}
	if strings.Join(got, ",") != "att-1,att-2" {
		t.Fatalf("draft attachments = %v, want [att-1 att-2]", got)
	}
}

func TestRevertToMessageCheckpointRestoresTrackedFiles(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	workspace := initCheckpointRepo(t)
	thread := createCheckpointTestThread(t, app, "t-files", "claude", workspace)

	writeFile(t, workspace, "notes.txt", "before\n")
	ref := checkpoint.ThreadRefPrefix(thread.ID) + "message/before-edit"
	if err := app.checkpointStore().CaptureRef(t.Context(), workspace, ref); err != nil {
		t.Fatalf("capture ref: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "edit notes")
	if err := app.store.SaveCheckpoint(store.Checkpoint{
		ID:                    "chk-0",
		ThreadID:              thread.ID,
		UserItemID:            "user:0",
		TurnIndex:             0,
		ProviderUserMessageID: "provider-user-0",
		RefName:               ref,
		CapturedAt:            time.Now().UnixMilli(),
		WorkspacePath:         workspace,
	}); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}
	if err := app.store.UpsertTrackedFiles(thread.ID, 0, []string{"notes.txt"}); err != nil {
		t.Fatalf("track file: %v", err)
	}
	writeFile(t, workspace, "notes.txt", "after\n")

	if err := app.RevertToMessageCheckpoint(thread.ID, "user:0", RevertModeConversationAndFiles); err != nil {
		t.Fatalf("revert: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(workspace, "notes.txt"))
	if err != nil {
		t.Fatalf("read notes: %v", err)
	}
	if string(got) != "before\n" {
		t.Fatalf("notes.txt = %q, want before", got)
	}
}

func TestRevertToMessageCheckpointRejectsMissingClaudeSessionForLaterTurn(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createCheckpointTestThread(t, app, "t-missing-session", "claude", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	if err := app.store.SaveCheckpoint(store.Checkpoint{
		ID:                    "chk-missing-session",
		ThreadID:              thread.ID,
		UserItemID:            "user:1",
		TurnIndex:             1,
		ProviderUserMessageID: "provider-user-1",
		RefName:               checkpoint.ThreadRefPrefix(thread.ID) + "message/missing-session",
		CapturedAt:            time.Now().UnixMilli(),
		WorkspacePath:         thread.WorkspacePath,
	}); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	err := app.RevertToMessageCheckpoint(thread.ID, "user:1", RevertModeConversationOnly)
	if err == nil {
		t.Fatal("revert succeeded, want missing session reference error")
	}
	if !strings.Contains(err.Error(), "requires Claude session reference") {
		t.Fatalf("error = %q, want missing session reference", err)
	}
}

func TestRevertToMessageCheckpointRejectsMissingClaudeSessionFileForLaterTurn(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createCheckpointTestThread(t, app, "t-missing-session-file", "claude", t.TempDir())
	thread.SessionRef = "missing-session"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	if err := app.store.SaveCheckpoint(store.Checkpoint{
		ID:            "chk-missing-session-file",
		ThreadID:      thread.ID,
		UserItemID:    "user:1",
		TurnIndex:     1,
		RefName:       checkpoint.ThreadRefPrefix(thread.ID) + "message/missing-session-file",
		CapturedAt:    time.Now().UnixMilli(),
		WorkspacePath: thread.WorkspacePath,
	}); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	err := app.RevertToMessageCheckpoint(thread.ID, "user:1", RevertModeConversationOnly)
	if err == nil {
		t.Fatal("revert succeeded, want missing session file error")
	}
	if !strings.Contains(err.Error(), "session file not found") {
		t.Fatalf("error = %q, want missing session file", err)
	}
	updated, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if updated.SessionRef != "missing-session" {
		t.Fatalf("session ref after rejected revert = %q, want original", updated.SessionRef)
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items after rejected revert = %+v, want conversation preserved", items)
	}
}

func TestRevertToMessageCheckpointRejectsMismatchedTurnIndex(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createCheckpointTestThread(t, app, "t-mismatch-turn", "claude", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	if err := app.store.SaveCheckpoint(store.Checkpoint{
		ID:            "chk-mismatch-turn",
		ThreadID:      thread.ID,
		UserItemID:    "user:1",
		TurnIndex:     0,
		RefName:       checkpoint.ThreadRefPrefix(thread.ID) + "message/mismatch-turn",
		CapturedAt:    time.Now().UnixMilli(),
		WorkspacePath: thread.WorkspacePath,
	}); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	err := app.RevertToMessageCheckpoint(thread.ID, "user:1", RevertModeConversationOnly)
	if err == nil {
		t.Fatal("revert succeeded, want turn index mismatch error")
	}
	if !strings.Contains(err.Error(), "checkpoint turn index") {
		t.Fatalf("error = %q, want turn index mismatch", err)
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 || items[0].ID != "user:1" {
		t.Fatalf("items after rejected revert = %+v, want selected user item preserved", items)
	}
}

func TestRevertToMessageCheckpointSlicesClaudeSessionByTurnBoundary(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	const sessionID = "source-session"
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"source-session","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"source-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"source-session","message":{"role":"user","content":"second"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"source-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 1"}]}}
`)
	thread := createCheckpointTestThread(t, app, "t-slice-session", "claude", workspace)
	thread.SessionRef = sessionID
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	if err := app.store.SaveCheckpoint(store.Checkpoint{
		ID:                    "chk-1",
		ThreadID:              thread.ID,
		UserItemID:            "user:1",
		TurnIndex:             1,
		ProviderUserMessageID: "u1",
		RefName:               checkpoint.ThreadRefPrefix(thread.ID) + "message/slice-session",
		CapturedAt:            time.Now().UnixMilli(),
		WorkspacePath:         workspace,
	}); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	if err := app.RevertToMessageCheckpoint(thread.ID, "user:1", RevertModeConversationOnly); err != nil {
		t.Fatalf("revert: %v", err)
	}
	record, ok, err := app.store.GetCheckpointByUserItemID(thread.ID, "user:1")
	if err != nil {
		t.Fatalf("get checkpoint: %v", err)
	}
	if ok {
		t.Fatalf("checkpoint still exists after revert: %+v", record)
	}
	updated, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if updated.SessionRef == "" || updated.SessionRef == sessionID {
		t.Fatalf("thread session ref = %q, want recovered fork session", updated.SessionRef)
	}
	if updated.PendingForkRef != "" {
		t.Fatalf("thread pending fork ref = %q, want empty", updated.PendingForkRef)
	}
	assertClaudeSessionText(t, workspace, updated.SessionRef, []string{"first"}, []string{"second"})
}

func TestRevertToMessageCheckpointSlicesClaudeSessionFromPendingForkRef(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	const sessionID = "pending-source-session"
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"pending-source-session","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"pending-source-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"pending-source-session","message":{"role":"user","content":"second"}}
`)
	thread := createCheckpointTestThread(t, app, "t-pending-revert", "claude", workspace)
	thread.PendingForkRef = sessionID
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	if err := app.store.SaveCheckpoint(store.Checkpoint{
		ID:                    "chk-pending",
		ThreadID:              thread.ID,
		UserItemID:            "user:1",
		TurnIndex:             1,
		ProviderUserMessageID: "u1",
		RefName:               checkpoint.ThreadRefPrefix(thread.ID) + "message/pending",
		CapturedAt:            time.Now().UnixMilli(),
		WorkspacePath:         workspace,
	}); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	if err := app.RevertToMessageCheckpoint(thread.ID, "user:1", RevertModeConversationOnly); err != nil {
		t.Fatalf("revert: %v", err)
	}
	updated, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if updated.SessionRef == "" || updated.SessionRef == sessionID {
		t.Fatalf("thread session ref = %q, want sliced session from pending fork source", updated.SessionRef)
	}
	if updated.PendingForkRef != "" {
		t.Fatalf("thread pending fork ref = %q, want empty", updated.PendingForkRef)
	}
	assertClaudeSessionText(t, workspace, updated.SessionRef, []string{"first"}, []string{"second"})
}

func TestRevertToMessageCheckpointCanRevertAgainAfterClaudeSessionFork(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := initCheckpointRepo(t)
	const sessionID = "source-session"
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"source-session","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"source-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"source-session","message":{"role":"user","content":"second"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"source-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 1"}]}}
{"type":"user","uuid":"u2","parentUuid":"a1","sessionId":"source-session","message":{"role":"user","content":"third"}}
{"type":"assistant","uuid":"a2","parentUuid":"u2","sessionId":"source-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 2"}]}}
`)
	thread := createCheckpointTestThread(t, app, "t-double-revert", "claude", workspace)
	thread.SessionRef = sessionID
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	insertUserItem(t, app.store, thread.ID, "user:2", 2, "third")

	ref1 := checkpoint.ThreadRefPrefix(thread.ID) + "message/second"
	if err := app.checkpointStore().CaptureRef(t.Context(), workspace, ref1); err != nil {
		t.Fatalf("capture second ref: %v", err)
	}
	if err := app.store.SaveCheckpoint(store.Checkpoint{
		ID:                    "chk-1",
		ThreadID:              thread.ID,
		UserItemID:            "user:1",
		TurnIndex:             1,
		ProviderUserMessageID: "u1",
		ProviderParentUUID:    "a0",
		RefName:               ref1,
		CapturedAt:            time.Now().UnixMilli(),
		WorkspacePath:         workspace,
	}); err != nil {
		t.Fatalf("save second checkpoint: %v", err)
	}
	ref2 := checkpoint.ThreadRefPrefix(thread.ID) + "message/third"
	if err := app.checkpointStore().CaptureRef(t.Context(), workspace, ref2); err != nil {
		t.Fatalf("capture third ref: %v", err)
	}
	if err := app.store.SaveCheckpoint(store.Checkpoint{
		ID:                    "chk-2",
		ThreadID:              thread.ID,
		UserItemID:            "user:2",
		TurnIndex:             2,
		ProviderUserMessageID: "u2",
		ProviderParentUUID:    "a1",
		RefName:               ref2,
		CapturedAt:            time.Now().UnixMilli(),
		WorkspacePath:         workspace,
	}); err != nil {
		t.Fatalf("save third checkpoint: %v", err)
	}

	if err := app.RevertToMessageCheckpoint(thread.ID, "user:2", RevertModeConversationOnly); err != nil {
		t.Fatalf("first revert: %v", err)
	}
	afterFirst, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread after first revert: %v", err)
	}
	if afterFirst.SessionRef == "" || afterFirst.SessionRef == sessionID {
		t.Fatalf("session after first revert = %q, want forked session", afterFirst.SessionRef)
	}
	assertClaudeSessionText(t, workspace, afterFirst.SessionRef, []string{"first", "second"}, []string{"third"})

	if err := app.RevertToMessageCheckpoint(thread.ID, "user:1", RevertModeConversationOnly); err != nil {
		t.Fatalf("second revert: %v", err)
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 || items[0].ID != "user:0" {
		t.Fatalf("items after second revert = %+v, want only first user item", items)
	}
	draft, ok, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("get draft: %v", err)
	}
	if !ok || draft.Content != "second" {
		t.Fatalf("draft after second revert = %+v ok=%v, want second prompt", draft, ok)
	}
	afterSecond, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread after second revert: %v", err)
	}
	assertClaudeSessionText(t, workspace, afterSecond.SessionRef, []string{"first"}, []string{"second", "third"})
}

func createCheckpointTestThread(t *testing.T, app *App, id, provider, workspace string) store.Thread {
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
	if err := app.store.CreateProject(store.Project{ID: "p1", Path: workspace, Name: "p1", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	return thread
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
	path, err := sessionfork.LocateSessionFile(sessionID, workspace)
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

func newTestApp(t *testing.T) (*App, func()) {
	t.Helper()
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	app := &App{
		store:       st,
		checkpoints: checkpoint.NewStore(),
		sessions:    make(map[string]session),
	}
	return app, func() { st.Close() }
}

func initCheckpointRepo(t *testing.T) string {
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
