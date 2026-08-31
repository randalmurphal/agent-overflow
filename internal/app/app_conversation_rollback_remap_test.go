package app

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// Tests for the rollback-path provider-id remap: every session slice
// remints every uuid (sessionfork.buildLines), so the rollback must
// refresh surviving items' `meta.provider_item_id` and surviving
// message anchors' provider ids or the NEXT rollback silently degrades
// to the ordinal-walk fallback.

const remapTestSessionJSONL = `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"remap-source","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"remap-source","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"remap-source","message":{"role":"user","content":"second"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"remap-source","message":{"role":"assistant","content":[{"type":"text","text":"reply 1"}]}}
{"type":"user","uuid":"u2","parentUuid":"a1","sessionId":"remap-source","message":{"role":"user","content":"third"}}
{"type":"assistant","uuid":"a2","parentUuid":"u2","sessionId":"remap-source","message":{"role":"assistant","content":[{"type":"text","text":"reply 2"}]}}
`

// setupRemapRollbackThread stands up a Claude thread with three user
// turns, provider-stamped items, and uuid-keyed message anchors for
// turns 1 and 2 — the shape triage produces during normal operation.
func setupRemapRollbackThread(t *testing.T, app *App) (store.Thread, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := t.TempDir()
	const sessionID = "remap-source"
	writeClaudeProjectSession(t, home, workspace, sessionID, remapTestSessionJSONL)
	thread := createAppTestThread(t, app, "t-remap-rollback", "claude", workspace)
	thread.SessionRef = sessionID
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItemWithMeta(t, app.store, thread.ID, "user:0", 0, "first", `{"provider_item_id":"u0"}`)
	insertUserItemWithMeta(t, app.store, thread.ID, "user:1", 1, "second", `{"provider_item_id":"u1"}`)
	insertUserItemWithMeta(t, app.store, thread.ID, "user:2", 2, "third", `{"provider_item_id":"u2"}`)
	seedMessageAnchor(t, app.store, thread.ID, "user:1", 1, "u1", "a0")
	seedMessageAnchor(t, app.store, thread.ID, "user:2", 2, "u2", "a1")
	return thread, workspace
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// TestConversationRollbackRemapsSurvivingProviderIDs pins the remap
// itself: after rolling back to "third", the surviving anchor and
// items must point at the NEW session's reminted uuids, and the
// follow-up rollback to "second" must take the uuid-keyed path (no
// ordinal-fallback log).
func TestConversationRollbackRemapsSurvivingProviderIDs(t *testing.T) {
	app := newTestApp(t)
	thread, workspace := setupRemapRollbackThread(t, app)

	if err := rollbackToMessage(app, thread.ID, "user:2"); err != nil {
		t.Fatalf("first rollback: %v", err)
	}
	afterFirst, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}

	// The new session's leaf scan only succeeds on uuids that exist in
	// the new file — use it to build the "valid uuid" check.
	newState, err := claude.ScanSessionLeaf(testProviderProjectsDir(t), afterFirst.SessionRef, workspace)
	if err != nil {
		t.Fatalf("scan new session leaf: %v", err)
	}
	if newState.CanonicalLeafUUID == "" {
		t.Fatalf("new session has no leaf")
	}

	surviving, ok, err := app.store.GetMessageAnchor(thread.ID, "user:1")
	if err != nil || !ok {
		t.Fatalf("get surviving anchor: ok=%v err=%v", ok, err)
	}
	if surviving.ProviderUserMessageID == "u1" {
		t.Fatalf("surviving anchor still carries source-session uuid u1 — rollback did not remap")
	}
	if surviving.ProviderParentUUID == "a0" {
		t.Fatalf("surviving anchor parent uuid still a0 — rollback did not remap")
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	for _, it := range items {
		if it.Kind != "user_text" {
			continue
		}
		if id := usermessage.ReadProviderItemID(it.Meta); id == "u0" || id == "u1" {
			t.Fatalf("item %s still carries source-session uuid %s — rollback did not remap", it.ID, id)
		}
	}

	// Second rollback must take the uuid-keyed slice — the loud ordinal
	// fallback means the remap missed.
	logBuf := captureLog(t)
	if err := rollbackToMessage(app, thread.ID, "user:1"); err != nil {
		t.Fatalf("second rollback: %v", err)
	}
	if out := logBuf.String(); strings.Contains(out, "falling back to ordinal slice") {
		t.Fatalf("second rollback hit the ordinal fallback — anchor remap is not feeding the uuid-keyed path:\n%s", out)
	}
	afterSecond, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread after second rollback: %v", err)
	}
	assertClaudeSessionText(t, workspace, afterSecond.SessionRef, []string{"first"}, []string{"second", "third"})
}

// TestConversationRollbackOrdinalFallbackStillReachableForStaleIDs
// pins the legacy escape hatch: when BOTH stored uuid copies — the
// anchor's and the item row's meta stamp (the R5-7 retry candidate) —
// are absent from the session file (pre-stamp rows, a wholesale
// regressed remap), the rollback still lands via the ordinal walk,
// loudly.
func TestConversationRollbackOrdinalFallbackStillReachableForStaleIDs(t *testing.T) {
	app := newTestApp(t)
	thread, workspace := setupRemapRollbackThread(t, app)

	if err := app.store.UpdateMessageAnchorProviderIDs(thread.ID, "user:2", "uuid-not-in-file", "also-not-in-file"); err != nil {
		t.Fatalf("poison anchor uuid: %v", err)
	}
	if err := app.store.UpdateItemMeta(thread.ID, "user:2", `{"provider_item_id":"uuid-not-in-file"}`); err != nil {
		t.Fatalf("poison item meta uuid: %v", err)
	}

	logBuf := captureLog(t)
	if err := rollbackToMessage(app, thread.ID, "user:2"); err != nil {
		t.Fatalf("rollback with stale uuid: %v", err)
	}
	if out := logBuf.String(); !strings.Contains(out, "falling back to ordinal slice") {
		t.Fatalf("stale-uuid rollback did not log the ordinal fallback:\n%s", out)
	}
	after, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	assertClaudeSessionText(t, workspace, after.SessionRef, []string{"first", "second"}, []string{"third"})
}

// TestConversationRollbackSlicesPoisonedAPIErrorTail is the Step 6 +
// Step 9 integration: rolling back a session whose tail carries
// deferred off-branch system/api_error rows must produce a slice whose
// leaf scan lands on the active branch (the rechain repaired the
// topology), so the next resume targets a uuid claude will accept.
func TestConversationRollbackSlicesPoisonedAPIErrorTail(t *testing.T) {
	app := newTestApp(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := t.TempDir()
	const sessionID = "poisoned-source"
	// Turn 0 (u0/a0), then turn 1 (u1/a1-mid/a1-final) whose trailing
	// api_error rows chain to the MID-turn leaf — the incident topology:
	// the file-order leaf a1-final is off the active branch. u2 opens
	// turn 2 chained onto the api_errors (where claude actually puts it).
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"poisoned-source","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"poisoned-source","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"poisoned-source","message":{"role":"user","content":"second"}}
{"type":"assistant","uuid":"a1-mid","parentUuid":"u1","sessionId":"poisoned-source","message":{"role":"assistant","content":[{"type":"text","text":"working on it"}]}}
{"type":"assistant","uuid":"a1-final","parentUuid":"a1-mid","sessionId":"poisoned-source","message":{"role":"assistant","content":[{"type":"text","text":"reply 1"}]}}
{"type":"system","subtype":"api_error","uuid":"err1","parentUuid":"a1-mid","level":"error","retryAttempt":1,"error":{"message":"Connection error."}}
{"type":"system","subtype":"api_error","uuid":"err2","parentUuid":"err1","level":"error","retryAttempt":2,"error":{"message":"Connection error."}}
{"type":"user","uuid":"u2","parentUuid":"err2","sessionId":"poisoned-source","message":{"role":"user","content":"third"}}
{"type":"assistant","uuid":"a2","parentUuid":"u2","sessionId":"poisoned-source","message":{"role":"assistant","content":[{"type":"text","text":"reply 2"}]}}
`)
	thread := createAppTestThread(t, app, "t-poisoned-rollback", "claude", workspace)
	thread.SessionRef = sessionID
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItemWithMeta(t, app.store, thread.ID, "user:0", 0, "first", `{"provider_item_id":"u0"}`)
	insertUserItemWithMeta(t, app.store, thread.ID, "user:1", 1, "second", `{"provider_item_id":"u1"}`)
	insertUserItemWithMeta(t, app.store, thread.ID, "user:2", 2, "third", `{"provider_item_id":"u2"}`)
	seedMessageAnchor(t, app.store, thread.ID, "user:2", 2, "u2", "err2")

	// Rollback to "third" keeps turns 0-1 INCLUDING the poisoned tail
	// (err1/err2 are the rows just before the slice anchor u2).
	if err := rollbackToMessage(app, thread.ID, "user:2"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	after, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	assertClaudeSessionText(t, workspace, after.SessionRef, []string{"first", "second", "reply 1"}, []string{"third"})

	// The slice's leaf must be on the active branch — i.e. the resume-at
	// the app would pass is one claude accepts. Without the rechain the
	// sliced file ends with off-branch api_error rows and the branch
	// walk would skip reply 1's row.
	state, err := claude.ScanSessionLeaf(testProviderProjectsDir(t), after.SessionRef, workspace)
	if err != nil {
		t.Fatalf("scan sliced session leaf: %v", err)
	}
	if state.CanonicalLeafUUID == "" {
		t.Fatalf("sliced session has no usable leaf")
	}
	onBranch, err := claude.ResumeAtOnActiveBranch(
		testProviderProjectsDir(t), after.SessionRef, workspace, state.CanonicalLeafUUID,
	)
	if err != nil {
		t.Fatalf("validate sliced leaf: %v", err)
	}
	if !onBranch {
		t.Fatalf("sliced session leaf %s is off the active branch — rechain did not repair the slice", state.CanonicalLeafUUID)
	}
}
