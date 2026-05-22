package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/checkpoint"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/settings"
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

func TestRevertToMessageCheckpointRejectsRunningBackgroundTasks(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createCheckpointTestThread(t, app, "t-running-background-reject", "claude", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertRunningBackgroundToolCall(t, app.store, thread.ID, "bg:0", 0, 1)

	err := app.RevertToMessageCheckpoint(thread.ID, "user:0", RevertModeConversationOnly)
	if err == nil {
		t.Fatal("revert succeeded, want running background task guard")
	}
	if !strings.Contains(err.Error(), "running background tasks must be killed") {
		t.Fatalf("error = %q, want running background tasks guard", err)
	}
}

func TestRevertToMessageCheckpointRejectsStashedBackgroundTasks(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createCheckpointTestThread(t, app, "t-stashed-background-reject", "claude", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertRunningBackgroundToolCall(t, app.store, thread.ID, "bg:0", 0, 1)
	if err := app.store.UpsertPendingBackgroundTerminal(store.PendingBackgroundTaskTerminal{
		ThreadID:  thread.ID,
		TaskID:    "task-bg-0",
		ToolUseID: "bg:0",
		Status:    "completed",
		Source:    "task_updated",
		CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("upsert pending terminal: %v", err)
	}

	err := app.RevertToMessageCheckpoint(thread.ID, "user:0", RevertModeConversationOnly)
	if err == nil {
		t.Fatal("revert succeeded, want stashed background task guard")
	}
	if !strings.Contains(err.Error(), "running background tasks must be killed") {
		t.Fatalf("error = %q, want running background tasks guard", err)
	}
}

func TestRevertToMessageCheckpointWithOptionsClearsRunningBackgroundTasks(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := t.TempDir()
	const sessionID = "background-revert-session"
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"background-revert-session","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"background-revert-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"background-revert-session","message":{"role":"user","content":"second"}}
`)
	thread := createCheckpointTestThread(t, app, "t-running-background-confirm", "claude", workspace)
	thread.SessionRef = sessionID
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertRunningBackgroundToolCall(t, app.store, thread.ID, "bg:0", 0, 1)
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	if err := app.store.SaveCheckpoint(store.Checkpoint{
		ID:                    "chk-background-confirm",
		ThreadID:              thread.ID,
		UserItemID:            "user:1",
		TurnIndex:             1,
		ProviderUserMessageID: "u1",
		RefName:               checkpoint.ThreadRefPrefix(thread.ID) + "message/background-confirm",
		CapturedAt:            time.Now().UnixMilli(),
		WorkspacePath:         workspace,
	}); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	err := app.RevertToMessageCheckpointWithOptions(thread.ID, "user:1", RevertModeConversationOnly, RevertToMessageCheckpointOptions{
		KillRunningBackgroundTasks: true,
	})
	if err != nil {
		t.Fatalf("revert with background confirmation: %v", err)
	}

	live, err := app.store.ListLiveBackgroundTasks(thread.ID, 0)
	if err != nil {
		t.Fatalf("list live background tasks: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("live background tasks after confirmed revert = %+v, want none", live)
	}
	bg, ok, err := app.store.GetThreadItem(thread.ID, "bg:0")
	if err != nil {
		t.Fatalf("get background item: %v", err)
	}
	if !ok {
		t.Fatal("background item missing")
	}
	if bg.Status != "running" {
		t.Fatalf("background status = %q, want running", bg.Status)
	}
	if !strings.Contains(bg.Meta, `"live_background_active":false`) {
		t.Fatalf("background meta = %q, want live_background_active=false", bg.Meta)
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

// TestRevertToMessageCheckpointSurvivesCompactBoundary is the
// regression guard for the ordinal-walk off-by-N bug. The fixture
// places a /compact summary BETWEEN the turn we want to keep (turn
// 1) and an earlier turn (turn 0) — that ordering is what trips the
// legacy ordinal walk. Counting cs1 as a "real" user prompt pushes
// every subsequent real prompt's index off by one; FindUUIDBeforeUserTurn
// then returns the parent of cs1 (= a0) instead of the parent of u1
// (= cs1). The fork ends up sliced at end-of-turn-0 (losing u1+a1)
// instead of end-of-turn-1.
//
// With the UUID-keyed path (driven by the checkpoint's stored wire
// id u2), the slice is immune to that drift — proving the structural
// fix at the integration level. The companion fallback test below
// proves the tightened ordinal walk reaches the same answer.
func TestRevertToMessageCheckpointSurvivesCompactBoundary(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	const sessionID = "compact-revert-session"
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"compact-revert-session","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"compact-revert-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"cs1","parentUuid":"a0","sessionId":"compact-revert-session","isCompactSummary":true,"isVisibleInTranscriptOnly":true,"message":{"role":"user","content":"This session is being continued from a previous conversation."}}
{"type":"user","uuid":"u1","parentUuid":"cs1","sessionId":"compact-revert-session","message":{"role":"user","content":"second"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"compact-revert-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 1"}]}}
{"type":"user","uuid":"u2","parentUuid":"a1","sessionId":"compact-revert-session","message":{"role":"user","content":"third"}}
{"type":"assistant","uuid":"a2","parentUuid":"u2","sessionId":"compact-revert-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 2"}]}}
`)
	thread := createCheckpointTestThread(t, app, "t-compact-revert", "claude", workspace)
	thread.SessionRef = sessionID
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	insertUserItem(t, app.store, thread.ID, "user:2", 2, "third")
	if err := app.store.SaveCheckpoint(store.Checkpoint{
		ID:                    "chk-compact",
		ThreadID:              thread.ID,
		UserItemID:            "user:2",
		TurnIndex:             2,
		ProviderUserMessageID: "u2",
		RefName:               checkpoint.ThreadRefPrefix(thread.ID) + "message/compact-revert",
		CapturedAt:            time.Now().UnixMilli(),
		WorkspacePath:         workspace,
	}); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	if err := app.RevertToMessageCheckpoint(thread.ID, "user:2", RevertModeConversationOnly); err != nil {
		t.Fatalf("revert: %v", err)
	}
	updated, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if updated.SessionRef == "" || updated.SessionRef == sessionID {
		t.Fatalf("thread session ref = %q, want sliced fork session", updated.SessionRef)
	}
	// "second" / "reply 1" MUST survive — the bug was reverting one turn
	// too far and losing them. "third" / "reply 2" MUST be gone — that's
	// the turn the user reverted away from.
	assertClaudeSessionText(t, workspace, updated.SessionRef,
		[]string{"first", "reply 0", "second", "reply 1"},
		[]string{"third", "reply 2"})
}

// TestRevertToMessageCheckpointFallbackHandlesCompactBoundary covers
// the legacy-row branch in `writeRevertedClaudeSession`: a checkpoint
// without a stored `ProviderUserMessageID` drops into the ordinal
// walk. The tightened `isRealUserPrompt` filter (which now rejects
// `isCompactSummary`) keeps that fallback correct even in the
// presence of a /compact entry — so pre-fix rows that never got the
// wire-id stamp still slice in the right place. Fixture mirrors
// the UUID-keyed test so the bug-trigger condition (compact summary
// before the revert target) is present in both paths.
func TestRevertToMessageCheckpointFallbackHandlesCompactBoundary(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	const sessionID = "compact-fallback-session"
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"compact-fallback-session","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"compact-fallback-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"cs1","parentUuid":"a0","sessionId":"compact-fallback-session","isCompactSummary":true,"isVisibleInTranscriptOnly":true,"message":{"role":"user","content":"This session is being continued from a previous conversation."}}
{"type":"user","uuid":"u1","parentUuid":"cs1","sessionId":"compact-fallback-session","message":{"role":"user","content":"second"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"compact-fallback-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 1"}]}}
{"type":"user","uuid":"u2","parentUuid":"a1","sessionId":"compact-fallback-session","message":{"role":"user","content":"third"}}
{"type":"assistant","uuid":"a2","parentUuid":"u2","sessionId":"compact-fallback-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 2"}]}}
`)
	thread := createCheckpointTestThread(t, app, "t-compact-fallback", "claude", workspace)
	thread.SessionRef = sessionID
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	insertUserItem(t, app.store, thread.ID, "user:2", 2, "third")
	// Note: NO ProviderUserMessageID — simulates legacy row pre-dating
	// the wire-id stamp.
	if err := app.store.SaveCheckpoint(store.Checkpoint{
		ID:            "chk-fallback",
		ThreadID:      thread.ID,
		UserItemID:    "user:2",
		TurnIndex:     2,
		RefName:       checkpoint.ThreadRefPrefix(thread.ID) + "message/compact-fallback",
		CapturedAt:    time.Now().UnixMilli(),
		WorkspacePath: workspace,
	}); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	if err := app.RevertToMessageCheckpoint(thread.ID, "user:2", RevertModeConversationOnly); err != nil {
		t.Fatalf("revert: %v", err)
	}
	updated, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if updated.SessionRef == "" || updated.SessionRef == sessionID {
		t.Fatalf("thread session ref = %q, want sliced fork session", updated.SessionRef)
	}
	assertClaudeSessionText(t, workspace, updated.SessionRef,
		[]string{"first", "reply 0", "second", "reply 1"},
		[]string{"third", "reply 2"})
}

// TestRevertToMessageCheckpointTolerantOfMissingJSONLAnchor pins the
// bricked-session recovery path: AO recorded a user_text row at
// TurnIndex=N, but the Claude subprocess died before persisting that
// prompt to its session JSONL (the JSONL only has prompts at indexes
// 0..N-1). Without the slice-at-EOF fallback, revert returned
// "Requested N, found N" (sessionfork.ErrUserTurnAtTranscriptEnd
// formatted into a toast) and the user was stuck — neither edit nor
// retry was possible from the UI.
//
// With the fallback in place, the revert succeeds and clones the
// full JSONL under a new session id. The composer rehydrates the
// missing message from AO's DB, the new SessionRef is durable, and
// the user can edit/resend.
func TestRevertToMessageCheckpointTolerantOfMissingJSONLAnchor(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	const sessionID = "bricked-session"
	// JSONL has only ONE real user prompt (u0) — simulates the
	// subprocess dying after AO sent the next prompt but before
	// Claude persisted it. AO's DB still has both user_text rows.
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"bricked-session","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"bricked-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
`)
	thread := createCheckpointTestThread(t, app, "t-bricked", "claude", workspace)
	thread.SessionRef = sessionID
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	// Checkpoint references a JSONL UUID that does NOT exist in the
	// file (the message was never persisted). The UUID-keyed branch
	// returns ErrMessageNotFound, then the ordinal walk returns
	// ErrUserTurnAtTranscriptEnd, and the fallback clones the JSONL
	// as-is.
	if err := app.store.SaveCheckpoint(store.Checkpoint{
		ID:                    "chk-bricked",
		ThreadID:              thread.ID,
		UserItemID:            "user:1",
		TurnIndex:             1,
		ProviderUserMessageID: "u1-never-persisted",
		RefName:               checkpoint.ThreadRefPrefix(thread.ID) + "message/bricked",
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
		t.Fatalf("thread session ref = %q, want fresh fork session", updated.SessionRef)
	}
	// The cloned JSONL must preserve every original entry — that's the
	// whole point of slice-at-EOF.
	assertClaudeSessionText(t, workspace, updated.SessionRef,
		[]string{"first", "reply 0"},
		nil)
}

// TestRevertToMessageCheckpointRejectsLargerJSONLGap is the negative
// boundary for the slice-at-EOF fallback above. The recovery path is
// scoped to the single off-by-one case (AO recorded TurnIndex=N but
// the JSONL has prompts only through N-1); a larger gap means
// something else is wrong (corrupt JSONL, wrong session ref,
// misaligned indices). Hard-erroring on gap >= 2 keeps the fallback's
// "this is the bricked-session recovery, not a generic anchor-missing
// blanket" contract documented at the test level — if sessionfork
// ever widens ErrUserTurnAtTranscriptEnd to cover gap >= 1, this test
// will fail and force a deliberate scope decision.
func TestRevertToMessageCheckpointRejectsLargerJSONLGap(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	const sessionID = "wide-gap-session"
	// JSONL has ONE real user prompt (u0). AO's DB will have THREE
	// (user:0, user:1, user:2) — gap of 2 between AO and JSONL.
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"wide-gap-session","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"wide-gap-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
`)
	thread := createCheckpointTestThread(t, app, "t-wide-gap", "claude", workspace)
	thread.SessionRef = sessionID
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	insertUserItem(t, app.store, thread.ID, "user:2", 2, "third")
	if err := app.store.SaveCheckpoint(store.Checkpoint{
		ID:                    "chk-wide-gap",
		ThreadID:              thread.ID,
		UserItemID:            "user:2",
		TurnIndex:             2,
		ProviderUserMessageID: "u2-never-persisted",
		RefName:               checkpoint.ThreadRefPrefix(thread.ID) + "message/wide-gap",
		CapturedAt:            time.Now().UnixMilli(),
		WorkspacePath:         workspace,
	}); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	err := app.RevertToMessageCheckpoint(thread.ID, "user:2", RevertModeConversationOnly)
	if err == nil {
		t.Fatal("revert with gap >= 2: err = nil, want ErrUserTurnOutOfRange")
	}
	if !errors.Is(err, sessionfork.ErrUserTurnOutOfRange) {
		t.Fatalf("revert with gap >= 2: err = %v, want wrapping sessionfork.ErrUserTurnOutOfRange", err)
	}
	if errors.Is(err, sessionfork.ErrUserTurnAtTranscriptEnd) {
		t.Fatalf("revert with gap >= 2: error matched ErrUserTurnAtTranscriptEnd (the off-by-one sentinel), want only the broader ErrUserTurnOutOfRange — fallback scope leaked")
	}
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
		settings:    settings.NewService(t.TempDir()),
		sessions:    make(map[string]session),
	}
	app.appCtx, app.appCancel = context.WithCancel(context.Background())
	t.Cleanup(app.appCancel)
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
