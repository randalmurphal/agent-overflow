package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/composerdraft"
	"agent-overflow/internal/itemmeta"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// rollbackToMessage drives the kept destructive rollback tail the way
// the un-send path does, minus the live-session interrupt plumbing:
// load the thread + user item, project the composer draft, resolve the
// message anchor, and hand off to rollbackConversationLocked.
func rollbackToMessage(app *App, threadID, userItemID string) error {
	unlock := app.threadLocks().Lock(threadID)
	defer unlock()
	thread, err := app.store.GetThread(threadID)
	if err != nil {
		return err
	}
	item, found, err := app.store.GetThreadItem(threadID, userItemID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("user item %s not found", userItemID)
	}
	promptDraft, err := composerdraft.FromUserItem(threadID, item, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	anchor := app.resolveMessageAnchor("test rollback", threadID, item)
	_, err = app.rollbackConversationLocked(rollbackConversationLockedArgs{
		thread:      thread,
		userItem:    item,
		anchor:      anchor,
		promptDraft: &promptDraft,
		errorPrefix: "test rollback",
	})
	return err
}

func TestConversationRollbackDeletesSelectedPromptAndRestoresDraft(t *testing.T) {
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
	thread := createAppTestThread(t, app, "t1", "claude", workspace)
	thread.SessionRef = sessionID
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	seedMessageAnchor(t, app.store, thread.ID, "user:1", 1, "provider-user-1", "")

	if err := rollbackToMessage(app, thread.ID, "user:1"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 || items[0].ID != "user:0" {
		t.Fatalf("items after rollback = %+v", items)
	}
	draft, ok, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("get draft: %v", err)
	}
	if !ok || draft.Content != "second" {
		t.Fatalf("draft = %+v ok=%v", draft, ok)
	}
}

func TestConversationRollbackRestoresDraftAttachments(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createAppTestThread(t, app, "t-attachments", "claude", t.TempDir())
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
	seedMessageAnchor(t, app.store, thread.ID, "user:0", 0, "provider-user-0", "")

	if err := rollbackToMessage(app, thread.ID, "user:0"); err != nil {
		t.Fatalf("rollback: %v", err)
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

func TestConversationRollbackRejectsMissingClaudeSessionForLaterTurn(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createAppTestThread(t, app, "t-missing-session", "claude", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	seedMessageAnchor(t, app.store, thread.ID, "user:1", 1, "provider-user-1", "")

	err := rollbackToMessage(app, thread.ID, "user:1")
	if err == nil {
		t.Fatal("rollback succeeded, want missing session reference error")
	}
	if !strings.Contains(err.Error(), "requires Claude session reference") {
		t.Fatalf("error = %q, want missing session reference", err)
	}
}

func TestConversationRollbackRejectsMissingClaudeSessionFileForLaterTurn(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createAppTestThread(t, app, "t-missing-session-file", "claude", t.TempDir())
	thread.SessionRef = "missing-session"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	seedMessageAnchor(t, app.store, thread.ID, "user:1", 1, "", "")

	err := rollbackToMessage(app, thread.ID, "user:1")
	if err == nil {
		t.Fatal("rollback succeeded, want missing session file error")
	}
	if !strings.Contains(err.Error(), "session file not found") {
		t.Fatalf("error = %q, want missing session file", err)
	}
	updated, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if updated.SessionRef != "missing-session" {
		t.Fatalf("session ref after rejected rollback = %q, want original", updated.SessionRef)
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items after rejected rollback = %+v, want conversation preserved", items)
	}
}

func TestConversationRollbackSlicesClaudeSessionByTurnBoundary(t *testing.T) {
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
	thread := createAppTestThread(t, app, "t-slice-session", "claude", workspace)
	thread.SessionRef = sessionID
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	seedMessageAnchor(t, app.store, thread.ID, "user:1", 1, "u1", "")

	if err := rollbackToMessage(app, thread.ID, "user:1"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	// The anchor row follows its item out via the FK cascade.
	if _, ok, err := app.store.GetMessageAnchor(thread.ID, "user:1"); err != nil {
		t.Fatalf("get anchor: %v", err)
	} else if ok {
		t.Fatal("anchor still exists after rollback deleted its item")
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

// TestConversationRollbackKeepsSharedTurnPrefix — rolling back to a
// queued flush message that shares its turn with the prompt that was
// running when it was enqueued. Claude truncation is item-granular
// (DeleteConversationFromItem), so the original prompt and the agent
// work before the queued send survive in SQLite, and the session slice
// cuts the JSONL at the queued message's uuid — the two stay aligned.
func TestConversationRollbackKeepsSharedTurnPrefix(t *testing.T) {
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
{"type":"user","uuid":"uq","parentUuid":"a0","sessionId":"source-session","message":{"role":"user","content":"queued follow-up"}}
{"type":"assistant","uuid":"a1","parentUuid":"uq","sessionId":"source-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 1"}]}}
{"type":"user","uuid":"u1","parentUuid":"a1","sessionId":"source-session","message":{"role":"user","content":"second"}}
{"type":"assistant","uuid":"a2","parentUuid":"u1","sessionId":"source-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 2"}]}}
`)
	thread := createAppTestThread(t, app, "t-shared-turn", "claude", workspace)
	thread.SessionRef = sessionID
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	// Turn 0 holds the original prompt, its streamed reply, the queued
	// flush row (eager-persisted mid-turn), and the post-queue reply.
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertAssistantTextItem(t, app.store, thread.ID, "text:0:0", 0, "reply 0")
	insertUserItemWithMeta(t, app.store, thread.ID, "flush:0", 0, "queued follow-up", `{"provider_item_id":"uq"}`)
	insertAssistantTextItem(t, app.store, thread.ID, "text:0:1", 0, "reply 1")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	insertAssistantTextItem(t, app.store, thread.ID, "text:1:0", 1, "reply 2")
	seedMessageAnchor(t, app.store, thread.ID, "user:0", 0, "u0", "")
	seedMessageAnchor(t, app.store, thread.ID, "flush:0", 0, "uq", "")
	seedMessageAnchor(t, app.store, thread.ID, "user:1", 1, "u1", "")

	if err := rollbackToMessage(app, thread.ID, "flush:0"); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	gotIDs := make([]string, len(items))
	for i, it := range items {
		gotIDs[i] = it.ID
	}
	if strings.Join(gotIDs, ",") != "user:0,text:0:0" {
		t.Fatalf("items after rollback = %v, want [user:0 text:0:0]", gotIDs)
	}
	if _, ok, err := app.store.GetMessageAnchor(thread.ID, "user:0"); err != nil || !ok {
		t.Fatalf("original prompt's anchor should survive (ok=%v err=%v)", ok, err)
	}
	if _, ok, _ := app.store.GetMessageAnchor(thread.ID, "flush:0"); ok {
		t.Fatal("rolled-back flush row's anchor should cascade away with its item")
	}
	draft, ok, err := app.store.GetThreadDraft(thread.ID)
	if err != nil || !ok {
		t.Fatalf("get draft: ok=%v err=%v", ok, err)
	}
	if draft.Content != "queued follow-up" {
		t.Fatalf("draft content = %q, want the rolled-back queued message", draft.Content)
	}
	updated, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if updated.SessionRef == "" || updated.SessionRef == sessionID {
		t.Fatalf("thread session ref = %q, want recovered fork session", updated.SessionRef)
	}
	assertClaudeSessionText(t, workspace, updated.SessionRef,
		[]string{"first", "reply 0"},
		[]string{"queued follow-up", "reply 1", "second", "reply 2"})
}

// midTurnAnchorFixture seeds the shared shape behind the mid-turn
// uuid-edge rollback tests: a Claude session whose transcript never
// consumed the queued message (the trailing assistant entry is the
// interrupted round's tail), and a timeline whose promoted flush row
// shares turn 0 with the original prompt. flushProviderID is the
// anchor's stored wire id — empty models "echo never arrived",
// non-empty-but-absent models fork-remap drift. flushParentID is the
// anchor's provider_parent_uuid; "a1" (the transcript's last entry)
// models a prior rollback whose provider slice committed and remapped
// before a later step failed.
func midTurnAnchorFixture(t *testing.T, app *App, threadID, sessionID, flushProviderID, flushParentID string) store.Thread {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"`+sessionID+`","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"`+sessionID+`","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"assistant","uuid":"a1","parentUuid":"a0","sessionId":"`+sessionID+`","message":{"role":"assistant","content":[{"type":"text","text":"interrupted tail"}]}}
`)
	thread := createAppTestThread(t, app, threadID, "claude", workspace)
	thread.SessionRef = sessionID
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	promotedMeta, err := itemmeta.MarkPromotedAtInterrupt("")
	if err != nil {
		t.Fatalf("mark promoted: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertAssistantTextItem(t, app.store, thread.ID, "text:0:0", 0, "reply 0")
	insertUserItemWithMeta(t, app.store, thread.ID, "flush:0", 0, "queued follow-up", promotedMeta)
	insertAssistantTextItem(t, app.store, thread.ID, "text:0:1", 0, "interrupted tail")
	seedMessageAnchor(t, app.store, thread.ID, "user:0", 0, "u0", "")
	seedMessageAnchor(t, app.store, thread.ID, "flush:0", 0, flushProviderID, flushParentID)
	return thread
}

// TestConversationRollbackMidTurnAnchorWithoutUUIDClonesFullTranscript
// pins the R1 fallback: a mid-turn anchor (queued flush row sharing its turn
// with the running prompt) whose anchor never got a provider id — the
// CLI never consumed the message, so it never reached the JSONL — must clone
// the transcript WHOLE. The ordinal walk keeps whole turns, so it would
// slice at end-of-turn-N-1 and drop the shared turn's kept prefix from the
// provider session while SQLite retains it.
func TestConversationRollbackMidTurnAnchorWithoutUUIDClonesFullTranscript(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	const sessionID = "midturn-nouuid-session"
	thread := midTurnAnchorFixture(t, app, "t-midturn-nouuid", sessionID, "", "")

	if err := rollbackToMessage(app, thread.ID, "flush:0"); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// The promoted anchor's cut keeps the interrupted tail in
	// SQLite (it precedes the never-consumed message in provider
	// order) — matching the full transcript kept on the JSONL side.
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	gotIDs := make([]string, len(items))
	for i, it := range items {
		gotIDs[i] = it.ID
	}
	if strings.Join(gotIDs, ",") != "user:0,text:0:0,text:0:1" {
		t.Fatalf("items after rollback = %v, want [user:0 text:0:0 text:0:1]", gotIDs)
	}
	draft, ok, err := app.store.GetThreadDraft(thread.ID)
	if err != nil || !ok {
		t.Fatalf("get draft: ok=%v err=%v", ok, err)
	}
	if draft.Content != "queued follow-up" {
		t.Fatalf("draft content = %q, want the rolled-back queued message", draft.Content)
	}
	updated, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if updated.SessionRef == "" || updated.SessionRef == sessionID {
		t.Fatalf("thread session ref = %q, want recovered fork session", updated.SessionRef)
	}
	assertClaudeSessionText(t, updated.WorkspacePath, updated.SessionRef,
		[]string{"first", "reply 0", "interrupted tail"},
		nil)
}

// TestConversationRollbackMidTurnAnchorStaleUUIDFails pins D2/CT4:
// a mid-turn anchor carrying a NON-EMPTY provider id that the
// transcript doesn't contain proves the message WAS consumed (only the
// echo stamps the id) and the stored id went stale (fork remap drift).
// Cloning the full transcript would resume a session that still
// contains the rolled-back prompt and its response; slicing ordinally
// would drop the shared turn's kept prefix. The rollback must FAIL
// loudly and mutate nothing: SessionRef, timeline rows, anchors, and
// the draft all stay as they were.
func TestConversationRollbackMidTurnAnchorStaleUUIDFails(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	const sessionID = "midturn-staleuuid-session"
	thread := midTurnAnchorFixture(t, app, "t-midturn-staleuuid", sessionID, "uq-stale", "uq-parent-stale")

	err := rollbackToMessage(app, thread.ID, "flush:0")
	if err == nil {
		t.Fatal("rollback with a stale consumed uuid must fail, not silently diverge")
	}
	if !strings.Contains(err.Error(), "uq-stale") {
		t.Errorf("error should name the stale uuid: %v", err)
	}

	// Nothing moved: the provider rollback failed before any truncation.
	updated, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if updated.SessionRef != sessionID {
		t.Errorf("session ref = %q, want untouched %q", updated.SessionRef, sessionID)
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 4 {
		t.Errorf("items after failed rollback = %d, want 4 untouched", len(items))
	}
	if _, ok, err := app.store.GetMessageAnchor(thread.ID, "flush:0"); err != nil || !ok {
		t.Errorf("message anchor must survive a failed rollback (ok=%v err=%v)", ok, err)
	}
	if _, ok, err := app.store.GetThreadDraft(thread.ID); err != nil {
		t.Fatalf("get draft: %v", err)
	} else if ok {
		t.Error("failed rollback must not restore the composer draft")
	}
}

// TestConversationRollbackMidTurnAnchorRetryAfterPartialFailure pins
// CT-3 (round 3) and R5-6 (round 5): a rollback whose provider slice
// COMMITTED (SessionRef repointed, surviving ids remapped) but whose
// later step failed leaves the anchor's own uuid legitimately absent
// from the new transcript while its remapped PARENT uuid is present. A
// retry must recognize that state as "already cut here" and finish the
// rollback — re-slicing THROUGH the parent and truncating SQLite —
// instead of failing forever on the stale-uuid guard. Through the
// parent, not a whole clone: rows appended after the failed rollback
// must not be resurrected into the retried cut.
func TestConversationRollbackMidTurnAnchorRetryAfterPartialFailure(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	// The fixture transcript ends at "interrupted tail" (a1) and does NOT
	// contain the anchor: exactly what a committed prior slice leaves
	// behind. The anchor carries the post-remap state — its own uuid
	// stale ("uq-cut", cut away so never remapped), its parent current
	// ("a1", remapped to the sliced file's id).
	const sessionID = "midturn-retry-session"
	thread := midTurnAnchorFixture(t, app, "t-midturn-retry", sessionID, "uq-cut", "a1")

	// A row appended AFTER the partial failure (the session resumed and
	// streamed before the user retried). The through-parent re-slice
	// must cut it (R5-6).
	sessionPath, err := sessionfork.LocateSessionFile(sessionID, thread.WorkspacePath)
	if err != nil {
		t.Fatalf("locate session: %v", err)
	}
	f, err := os.OpenFile(sessionPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open session for append: %v", err)
	}
	if _, err := f.WriteString(`{"type":"assistant","uuid":"a2","parentUuid":"a1","sessionId":"` + sessionID + `","message":{"role":"assistant","content":[{"type":"text","text":"post-failure noise"}]}}` + "\n"); err != nil {
		t.Fatalf("append post-failure row: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close session: %v", err)
	}

	if err := rollbackToMessage(app, thread.ID, "flush:0"); err != nil {
		t.Fatalf("retry rollback: %v", err)
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	gotIDs := make([]string, len(items))
	for i, it := range items {
		gotIDs[i] = it.ID
	}
	if strings.Join(gotIDs, ",") != "user:0,text:0:0,text:0:1" {
		t.Fatalf("items after retry = %v, want [user:0 text:0:0 text:0:1]", gotIDs)
	}
	draft, ok, err := app.store.GetThreadDraft(thread.ID)
	if err != nil || !ok {
		t.Fatalf("get draft: ok=%v err=%v", ok, err)
	}
	if draft.Content != "queued follow-up" {
		t.Fatalf("draft content = %q, want the rolled-back queued message", draft.Content)
	}
	updated, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if updated.SessionRef == "" || updated.SessionRef == sessionID {
		t.Fatalf("thread session ref = %q, want a fresh re-slice of the already-cut transcript", updated.SessionRef)
	}
	assertClaudeSessionText(t, updated.WorkspacePath, updated.SessionRef,
		[]string{"first", "reply 0", "interrupted tail"},
		[]string{"post-failure noise"})
}

// TestConversationRollbackMidTurnAnchorRetryUsesItemMetaParent pins
// the R5-8 fallback (round 5): the already-cut retry's slice-through
// parent comes from the anchor's provider_parent_uuid — but that
// copy is written by a follow-up UPDATE that can fail after the item
// meta stamp committed. The item meta carries the parent too (stamped
// atomically with the item id), so a retry whose ANCHOR parent is
// empty must still recognize the already-cut transcript and finish the
// rollback through the item-meta parent instead of refusing forever.
func TestConversationRollbackMidTurnAnchorRetryUsesItemMetaParent(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	const sessionID = "midturn-metaparent-session"
	// Anchor: stale wire uuid, NO parent copy (the follow-up
	// UpdateMessageAnchorProviderIDs never committed).
	thread := midTurnAnchorFixture(t, app, "t-midturn-metaparent", sessionID, "uq-cut", "")

	// The item row's meta carries the parent — stamped in the same tx
	// as the item id at the echo (R5-8).
	flushRow, found, err := app.store.GetThreadItem(thread.ID, "flush:0")
	if err != nil || !found {
		t.Fatalf("load flush row: found=%v err=%v", found, err)
	}
	patched, err := usermessage.MergeProviderIDs(flushRow.Meta, "", "a1")
	if err != nil {
		t.Fatalf("merge parent into item meta: %v", err)
	}
	if err := app.store.UpdateItemMeta(thread.ID, "flush:0", patched); err != nil {
		t.Fatalf("update item meta: %v", err)
	}

	if err := rollbackToMessage(app, thread.ID, "flush:0"); err != nil {
		t.Fatalf("retry rollback via item-meta parent: %v", err)
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	gotIDs := make([]string, len(items))
	for i, it := range items {
		gotIDs[i] = it.ID
	}
	if strings.Join(gotIDs, ",") != "user:0,text:0:0,text:0:1" {
		t.Fatalf("items after retry = %v, want [user:0 text:0:0 text:0:1]", gotIDs)
	}
	updated, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if updated.SessionRef == "" || updated.SessionRef == sessionID {
		t.Fatalf("thread session ref = %q, want a fresh re-slice", updated.SessionRef)
	}
	assertClaudeSessionText(t, updated.WorkspacePath, updated.SessionRef,
		[]string{"first", "reply 0", "interrupted tail"},
		nil)
}

// TestClaudeSliceAnchorUUIDs pins CT4-6 (round 4): the slice anchor
// prefers the anchor row's provider uuid but falls back to the item
// row's durable meta stamp — the two are written at different moments,
// and a failed UpdateMessageAnchorProviderIDs after the stamp committed
// must not misclassify a consumed message into the unconsumed
// full-clone path.
func TestClaudeSliceAnchorUUIDs(t *testing.T) {
	stamped := store.Item{ThreadID: "t1", ID: "flush:0", Meta: `{"provider_item_id":"uq-item"}`}
	cases := []struct {
		name     string
		anchorID string
		item     store.Item
		want     []string
	}{
		{"both copies, anchor first (R5-7 retry order)", "uq-chk", stamped, []string{"uq-chk", "uq-item"}},
		{"matching copies dedupe", "uq-item", stamped, []string{"uq-item"}},
		{"item meta fallback (CT4-6)", "", stamped, []string{"uq-item"}},
		{"both empty", "", store.Item{ThreadID: "t1", ID: "flush:0", Meta: "{}"}, nil},
	}
	for _, tc := range cases {
		got := claudeSliceAnchorUUIDs(store.MessageAnchor{ProviderUserMessageID: tc.anchorID}, tc.item)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s: claudeSliceAnchorUUIDs = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestWriteClaudeSessionSliceRetriesNextAnchorCandidate pins R5-7
// (round 5): when the first anchor candidate (the anchor row's copy)
// misses the transcript but the second (the item row's meta stamp) is
// present, the slice retries and lands on the exact UUID-keyed cut —
// it must NOT fall through to the ordinal walk or a mid-turn refusal.
// The split arises because remapClaudeProviderIDs refreshes items
// before anchors with per-row autocommit: a crash between the
// loops leaves the anchor copy a generation staler than the item's.
func TestWriteClaudeSessionSliceRetriesNextAnchorCandidate(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "retry-candidate-session.jsonl")
	jsonl := `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"retry-candidate-session","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"retry-candidate-session","message":{"role":"assistant","content":[{"type":"text","text":"first reply"}]}}
{"type":"user","uuid":"u2","parentUuid":"a1","sessionId":"retry-candidate-session","message":{"role":"user","content":"second"}}
{"type":"assistant","uuid":"a2","parentUuid":"u2","sessionId":"retry-candidate-session","message":{"role":"assistant","content":[{"type":"text","text":"second reply"}]}}
`
	if err := os.WriteFile(srcPath, []byte(jsonl), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	_, newPath, _, err := writeClaudeSessionSlice(srcPath, []string{"uq-stale-anchor", "u2"}, nil, 0, false, "test slice")
	if err != nil {
		t.Fatalf("slice with stale first candidate: %v", err)
	}
	data, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatalf("read slice: %v", err)
	}
	text := string(data)
	for _, want := range []string{"first", "first reply"} {
		if !strings.Contains(text, want) {
			t.Fatalf("slice missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "second") {
		t.Fatalf("slice kept the cut turn — the item-uuid candidate was not used:\n%s", text)
	}
}

// TestConversationRollbackSurvivesCompactBoundary is the regression
// guard for the ordinal-walk off-by-N bug. The fixture places a
// /compact summary BETWEEN the turn we want to keep (turn 1) and an
// earlier turn (turn 0) — that ordering is what trips the legacy
// ordinal walk. Counting cs1 as a "real" user prompt pushes every
// subsequent real prompt's index off by one; FindUUIDBeforeUserTurn
// then returns the parent of cs1 (= a0) instead of the parent of u1
// (= cs1). The fork ends up sliced at end-of-turn-0 (losing u1+a1)
// instead of end-of-turn-1.
//
// With the UUID-keyed path (driven by the anchor's stored wire id
// u2), the slice is immune to that drift — proving the structural
// fix at the integration level. The companion fallback test below
// proves the tightened ordinal walk reaches the same answer.
func TestConversationRollbackSurvivesCompactBoundary(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	const sessionID = "compact-rollback-session"
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"compact-rollback-session","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"compact-rollback-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"cs1","parentUuid":"a0","sessionId":"compact-rollback-session","isCompactSummary":true,"isVisibleInTranscriptOnly":true,"message":{"role":"user","content":"This session is being continued from a previous conversation."}}
{"type":"user","uuid":"u1","parentUuid":"cs1","sessionId":"compact-rollback-session","message":{"role":"user","content":"second"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"compact-rollback-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 1"}]}}
{"type":"user","uuid":"u2","parentUuid":"a1","sessionId":"compact-rollback-session","message":{"role":"user","content":"third"}}
{"type":"assistant","uuid":"a2","parentUuid":"u2","sessionId":"compact-rollback-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 2"}]}}
`)
	thread := createAppTestThread(t, app, "t-compact-rollback", "claude", workspace)
	thread.SessionRef = sessionID
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	insertUserItem(t, app.store, thread.ID, "user:2", 2, "third")
	seedMessageAnchor(t, app.store, thread.ID, "user:2", 2, "u2", "")

	if err := rollbackToMessage(app, thread.ID, "user:2"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	updated, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if updated.SessionRef == "" || updated.SessionRef == sessionID {
		t.Fatalf("thread session ref = %q, want sliced fork session", updated.SessionRef)
	}
	// "second" / "reply 1" MUST survive — the bug was rolling back one
	// turn too far and losing them. "third" / "reply 2" MUST be gone —
	// that's the turn the user rolled back away from.
	assertClaudeSessionText(t, workspace, updated.SessionRef,
		[]string{"first", "reply 0", "second", "reply 1"},
		[]string{"third", "reply 2"})
}

// TestConversationRollbackFallbackHandlesCompactBoundary covers the
// legacy-row branch in writeRolledBackClaudeSession: an anchor
// without a stored ProviderUserMessageID drops into the ordinal
// walk. The tightened isRealUserPrompt filter (which rejects
// isCompactSummary) keeps that fallback correct even in the presence
// of a /compact entry — so rows that never got the wire-id stamp
// still slice in the right place. Fixture mirrors the UUID-keyed test
// so the bug-trigger condition (compact summary before the rollback
// target) is present in both paths.
func TestConversationRollbackFallbackHandlesCompactBoundary(t *testing.T) {
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
	thread := createAppTestThread(t, app, "t-compact-fallback", "claude", workspace)
	thread.SessionRef = sessionID
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	insertUserItem(t, app.store, thread.ID, "user:2", 2, "third")
	// Note: NO ProviderUserMessageID — simulates a row that never got
	// the wire-id stamp.
	seedMessageAnchor(t, app.store, thread.ID, "user:2", 2, "", "")

	if err := rollbackToMessage(app, thread.ID, "user:2"); err != nil {
		t.Fatalf("rollback: %v", err)
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

// TestConversationRollbackFallsBackToOrdinalWhenStampedUUIDAbsent
// pins the safety net the send-time-uuid change leans on. An anchor
// DOES carry a ProviderUserMessageID, but that id is absent from the
// session JSONL — the case if the undocumented "Claude honors our
// top-level uuid" contract ever drifts (a CLI version stops persisting
// the supplied uuid; see docs/references/claude-wire.md §"Outbound user
// message"). The UUID-keyed slice returns ErrMessageNotFound, and
// writeClaudeSessionSlice must fall THROUGH to the ordinal walk, which —
// with the tightened isRealUserPrompt filter — still lands on the SAME
// turn boundary the uuid would have.
//
// Distinct coverage:
//   - FallbackHandlesCompactBoundary uses an EMPTY uuid, so it never
//     enters the UUID-keyed branch; this test forces that branch and its
//     ErrMessageNotFound fall-through.
//   - TolerantOfMissingJSONLAnchor also has an absent uuid, but its
//     ordinal walk hits EOF and CLONES; this one slices mid-transcript.
//
// The compact-summary entry between turn 0 and turn 1 is the off-by-N
// trap: a fallback that counted cs1 as a real prompt would slice one
// turn too far back and lose "second"/"reply 1". Mirrors the UUID-keyed
// happy path's fixture so both paths must reach the identical answer.
func TestConversationRollbackFallsBackToOrdinalWhenStampedUUIDAbsent(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	const sessionID = "uuid-drift-session"
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"uuid-drift-session","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"uuid-drift-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"cs1","parentUuid":"a0","sessionId":"uuid-drift-session","isCompactSummary":true,"isVisibleInTranscriptOnly":true,"message":{"role":"user","content":"This session is being continued from a previous conversation."}}
{"type":"user","uuid":"u1","parentUuid":"cs1","sessionId":"uuid-drift-session","message":{"role":"user","content":"second"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"uuid-drift-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 1"}]}}
{"type":"user","uuid":"u2","parentUuid":"a1","sessionId":"uuid-drift-session","message":{"role":"user","content":"third"}}
{"type":"assistant","uuid":"a2","parentUuid":"u2","sessionId":"uuid-drift-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 2"}]}}
`)
	thread := createAppTestThread(t, app, "t-uuid-drift", "claude", workspace)
	thread.SessionRef = sessionID
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	insertUserItem(t, app.store, thread.ID, "user:2", 2, "third")
	// Stamped id is NON-EMPTY but matches no JSONL entry — forces the
	// UUID-keyed branch to run, return ErrMessageNotFound, and fall
	// through to the ordinal walk at TurnIndex-1.
	seedMessageAnchor(t, app.store, thread.ID, "user:2", 2, "drifted-uuid-not-in-jsonl", "")

	if err := rollbackToMessage(app, thread.ID, "user:2"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	updated, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if updated.SessionRef == "" || updated.SessionRef == sessionID {
		t.Fatalf("thread session ref = %q, want sliced fork session", updated.SessionRef)
	}
	// Same boundary as the UUID-keyed happy path: turn 1 survives, turn 2
	// is gone. A fallback that miscounted the compact summary would also
	// drop "second"/"reply 1".
	assertClaudeSessionText(t, workspace, updated.SessionRef,
		[]string{"first", "reply 0", "second", "reply 1"},
		[]string{"third", "reply 2"})
}

// TestConversationRollbackTolerantOfMissingJSONLAnchor pins the
// bricked-session recovery path: AO recorded a user_text row at
// TurnIndex=N, but the Claude subprocess died before persisting that
// prompt to its session JSONL (the JSONL only has prompts at indexes
// 0..N-1). Without the slice-at-EOF fallback, the rollback returned
// "Requested N, found N" (sessionfork.ErrUserTurnAtTranscriptEnd
// formatted into a toast) and the user was stuck — neither edit nor
// retry was possible from the UI.
//
// With the fallback in place, the rollback succeeds and clones the
// full JSONL under a new session id. The composer rehydrates the
// missing message from AO's DB, the new SessionRef is durable, and
// the user can edit/resend.
func TestConversationRollbackTolerantOfMissingJSONLAnchor(t *testing.T) {
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
	thread := createAppTestThread(t, app, "t-bricked", "claude", workspace)
	thread.SessionRef = sessionID
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	// Anchor references a JSONL UUID that does NOT exist in the file
	// (the message was never persisted). The UUID-keyed branch returns
	// ErrMessageNotFound, then the ordinal walk returns
	// ErrUserTurnAtTranscriptEnd, and the fallback clones the JSONL
	// as-is.
	seedMessageAnchor(t, app.store, thread.ID, "user:1", 1, "u1-never-persisted", "")

	if err := rollbackToMessage(app, thread.ID, "user:1"); err != nil {
		t.Fatalf("rollback: %v", err)
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

// TestConversationRollbackRejectsLargerJSONLGap is the negative
// boundary for the slice-at-EOF fallback above. The recovery path is
// scoped to the single off-by-one case (AO recorded TurnIndex=N but
// the JSONL has prompts only through N-1); a larger gap means
// something else is wrong (corrupt JSONL, wrong session ref,
// misaligned indices). Hard-erroring on gap >= 2 keeps the fallback's
// "this is the bricked-session recovery, not a generic anchor-missing
// blanket" contract documented at the test level — if sessionfork
// ever widens ErrUserTurnAtTranscriptEnd to cover gap >= 1, this test
// will fail and force a deliberate scope decision.
func TestConversationRollbackRejectsLargerJSONLGap(t *testing.T) {
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
	thread := createAppTestThread(t, app, "t-wide-gap", "claude", workspace)
	thread.SessionRef = sessionID
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	insertUserItem(t, app.store, thread.ID, "user:2", 2, "third")
	seedMessageAnchor(t, app.store, thread.ID, "user:2", 2, "u2-never-persisted", "")

	err := rollbackToMessage(app, thread.ID, "user:2")
	if err == nil {
		t.Fatal("rollback with gap >= 2: err = nil, want ErrUserTurnOutOfRange")
	}
	if !errors.Is(err, sessionfork.ErrUserTurnOutOfRange) {
		t.Fatalf("rollback with gap >= 2: err = %v, want wrapping sessionfork.ErrUserTurnOutOfRange", err)
	}
	if errors.Is(err, sessionfork.ErrUserTurnAtTranscriptEnd) {
		t.Fatalf("rollback with gap >= 2: error matched ErrUserTurnAtTranscriptEnd (the off-by-one sentinel), want only the broader ErrUserTurnOutOfRange — fallback scope leaked")
	}
}

func TestConversationRollbackSlicesClaudeSessionFromPendingForkRef(t *testing.T) {
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
	thread := createAppTestThread(t, app, "t-pending-rollback", "claude", workspace)
	thread.PendingForkRef = sessionID
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	seedMessageAnchor(t, app.store, thread.ID, "user:1", 1, "u1", "")

	if err := rollbackToMessage(app, thread.ID, "user:1"); err != nil {
		t.Fatalf("rollback: %v", err)
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

func TestConversationRollbackCanRollBackAgainAfterClaudeSessionFork(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := t.TempDir()
	const sessionID = "source-session"
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"source-session","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"source-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"source-session","message":{"role":"user","content":"second"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"source-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 1"}]}}
{"type":"user","uuid":"u2","parentUuid":"a1","sessionId":"source-session","message":{"role":"user","content":"third"}}
{"type":"assistant","uuid":"a2","parentUuid":"u2","sessionId":"source-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 2"}]}}
`)
	thread := createAppTestThread(t, app, "t-double-rollback", "claude", workspace)
	thread.SessionRef = sessionID
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	insertUserItem(t, app.store, thread.ID, "user:2", 2, "third")
	seedMessageAnchor(t, app.store, thread.ID, "user:1", 1, "u1", "a0")
	seedMessageAnchor(t, app.store, thread.ID, "user:2", 2, "u2", "a1")

	if err := rollbackToMessage(app, thread.ID, "user:2"); err != nil {
		t.Fatalf("first rollback: %v", err)
	}
	afterFirst, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread after first rollback: %v", err)
	}
	if afterFirst.SessionRef == "" || afterFirst.SessionRef == sessionID {
		t.Fatalf("session after first rollback = %q, want forked session", afterFirst.SessionRef)
	}
	assertClaudeSessionText(t, workspace, afterFirst.SessionRef, []string{"first", "second"}, []string{"third"})

	if err := rollbackToMessage(app, thread.ID, "user:1"); err != nil {
		t.Fatalf("second rollback: %v", err)
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 || items[0].ID != "user:0" {
		t.Fatalf("items after second rollback = %+v, want only first user item", items)
	}
	draft, ok, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("get draft: %v", err)
	}
	if !ok || draft.Content != "second" {
		t.Fatalf("draft after second rollback = %+v ok=%v, want second prompt", draft, ok)
	}
	afterSecond, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread after second rollback: %v", err)
	}
	assertClaudeSessionText(t, workspace, afterSecond.SessionRef, []string{"first"}, []string{"second", "third"})
}

// TestConversationRollbackHeadHealedFirstPromptDropsSession pins
// R8-1 (round 8): a deferred turn-0 prompt healed at the turn HEAD
// (UpsertItemAtTurnHead, R7-4) sits at a NEGATIVE item_index below its
// response rows. It still opens turn 0, so rolling it back must take
// the drop-the-session branch — the previous exact-zero turn-initial
// check sent it down the slice path, which fails on the empty prefix.
func TestConversationRollbackHeadHealedFirstPromptDropsSession(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := t.TempDir()
	const sessionID = "head-healed-session"
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"head-healed-session","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"head-healed-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
`)
	thread := createAppTestThread(t, app, "t-headheal", "claude", workspace)
	thread.SessionRef = sessionID
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	now := time.Now().UnixMilli()
	if err := app.store.InsertItem(store.Item{
		ID: "user:0:flush:0", ThreadID: thread.ID, TurnIndex: 0, ItemIndex: -1,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "first", Meta: `{"provider_item_id":"u0"}`,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("insert head-healed prompt: %v", err)
	}
	if err := app.store.InsertItem(store.Item{
		ID: "text:0:0", ThreadID: thread.ID, TurnIndex: 0, ItemIndex: 0,
		Kind: "assistant_text", Role: "assistant", Status: "completed",
		Summary: "reply 0", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("insert response row: %v", err)
	}
	seedMessageAnchor(t, app.store, thread.ID, "user:0:flush:0", 0, "u0", "")

	if err := rollbackToMessage(app, thread.ID, "user:0:flush:0"); err != nil {
		t.Fatalf("rollback head-healed first prompt: %v", err)
	}
	reverted, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("reload thread: %v", err)
	}
	if reverted.SessionRef != "" {
		t.Errorf("SessionRef = %q, want empty — rolling back the turn-opening prompt keeps nothing", reverted.SessionRef)
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("items after rollback = %+v, want none", items)
	}
}

// TestClaudeMidTurnAnchor pins R12-6 (round 12): the rollback/fork
// mid-turn predicate must consult the durable promotion marker, not
// display position alone. A promoted flush row healed at its
// dispatch-time index after a failed tail bump (R10-1) can sit at
// display index 0 while the interrupted round's tail — provider-order
// BEFORE the queued message — persists below it; a bare ItemIndex > 0
// check would classify it turn-initial and cut that retained prefix
// from the session slice.
func TestClaudeMidTurnAnchor(t *testing.T) {
	promotedMeta, err := itemmeta.MarkPromotedAtInterrupt("")
	if err != nil {
		t.Fatalf("mark promoted meta: %v", err)
	}
	cases := []struct {
		name    string
		item    store.Item
		want    bool
		wantErr bool
	}{
		{name: "plain turn-initial prompt", item: store.Item{ItemIndex: 0}, want: false},
		{name: "head-healed deferred prompt (negative index, unmarked)", item: store.Item{ItemIndex: -1}, want: false},
		{name: "display mid-turn row", item: store.Item{ItemIndex: 2}, want: true},
		{name: "promoted row self-healed to display index 0 (R10-1)", item: store.Item{ItemIndex: 0, Meta: promotedMeta}, want: true},
		{name: "promoted row above the tail", item: store.Item{ItemIndex: 3, Meta: promotedMeta}, want: true},
		{name: "corrupt promotion marker fails loudly", item: store.Item{ItemIndex: 0, Meta: `{"promoted_at_interrupt":"yes"}`}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := claudeMidTurnAnchor(tc.item)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("claudeMidTurnAnchor = (%v, nil), want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("claudeMidTurnAnchor: %v", err)
			}
			if got != tc.want {
				t.Errorf("claudeMidTurnAnchor = %v, want %v", got, tc.want)
			}
		})
	}
}
