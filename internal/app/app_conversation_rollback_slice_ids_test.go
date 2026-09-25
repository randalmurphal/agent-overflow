package app

import (
	"strings"
	"testing"

	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// Tests for the provider ids a Claude session slice leaves valid: every
// rollback or fork slice keeps each surviving entry's uuid and parentUuid
// (sessionfork.buildLines) and changes only the session identity, so the
// ids SQLite stored against the source resolve in the slice and the next
// rollback or fork keys its cut on them unchanged.

const sliceIDsTestSessionJSONL = `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"slice-ids-source","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"slice-ids-source","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"slice-ids-source","message":{"role":"user","content":"second"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"slice-ids-source","message":{"role":"assistant","content":[{"type":"text","text":"reply 1"}]}}
{"type":"user","uuid":"u2","parentUuid":"a1","sessionId":"slice-ids-source","message":{"role":"user","content":"third"}}
{"type":"assistant","uuid":"a2","parentUuid":"u2","sessionId":"slice-ids-source","message":{"role":"assistant","content":[{"type":"text","text":"reply 2"}]}}
`

// setupSliceIDsThread stands up a Claude thread with three user turns,
// provider-stamped items, and uuid-keyed message anchors for turns 1 and
// 2: the shape triage produces during normal operation.
func setupSliceIDsThread(t *testing.T, app *App) (store.Thread, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := t.TempDir()
	const sessionID = "slice-ids-source"
	writeClaudeProjectSession(t, home, workspace, sessionID, sliceIDsTestSessionJSONL)
	thread := createAppTestThread(t, app, "t-slice-ids", "claude", workspace)
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

// userItemBySummary returns threadID's user_text row with the given
// summary, so fork tests need not assume the id a fork serves a row under.
func userItemBySummary(t *testing.T, app *App, threadID, summary string) store.Item {
	t.Helper()
	items, err := app.store.ListItems(threadID)
	if err != nil {
		t.Fatalf("list items for %s: %v", threadID, err)
	}
	for _, it := range items {
		if it.Kind == "user_text" && it.Summary == summary {
			return it
		}
	}
	t.Fatalf("thread %s has no user_text %q", threadID, summary)
	return store.Item{}
}

// TestConversationRollbackKeepsSurvivingProviderIDs pins the slice's id
// contract on the rollback path: the rolled-back session holds the
// source's surviving rows under their original uuids and parents, the
// stored ids stay as they were, and the follow-up rollback to "second"
// slices by the stored id. Were the slice to remint uuids, that second
// rollback would find neither u1 nor its parent a0 and refuse.
func TestConversationRollbackKeepsSurvivingProviderIDs(t *testing.T) {
	app := newTestApp(t)
	thread, workspace := setupSliceIDsThread(t, app)
	sourceRows := readClaudeSessionRows(t, workspace, thread.SessionRef)

	if err := rollbackToMessage(app, thread.ID, "user:2"); err != nil {
		t.Fatalf("first rollback: %v", err)
	}
	afterFirst, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if afterFirst.SessionRef == thread.SessionRef {
		t.Fatalf("rollback kept session ref %q, want a new slice", afterFirst.SessionRef)
	}
	assertClaudeSliceKeepsSourceRows(t, workspace, afterFirst.SessionRef, sourceRows[:4])

	surviving, ok, err := app.store.GetMessageAnchor(thread.ID, "user:1")
	if err != nil || !ok {
		t.Fatalf("get surviving anchor: ok=%v err=%v", ok, err)
	}
	if surviving.ProviderUserMessageID != "u1" || surviving.ProviderParentUUID != "a0" {
		t.Fatalf("surviving anchor ids = %q/%q, want u1/a0", surviving.ProviderUserMessageID, surviving.ProviderParentUUID)
	}
	for id, want := range map[string]string{"user:0": "u0", "user:1": "u1"} {
		item, found, err := app.store.GetThreadItem(thread.ID, id)
		if err != nil || !found {
			t.Fatalf("get item %s: found=%v err=%v", id, found, err)
		}
		if got := usermessage.ReadProviderItemID(item.Meta); got != want {
			t.Fatalf("item %s provider_item_id = %q, want %q", id, got, want)
		}
	}

	if err := rollbackToMessage(app, thread.ID, "user:1"); err != nil {
		t.Fatalf("second rollback: %v", err)
	}
	afterSecond, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread after second rollback: %v", err)
	}
	assertClaudeSliceKeepsSourceRows(t, workspace, afterSecond.SessionRef, sourceRows[:2])
}

// TestConversationRollbackInClaudeForkResolvesStoredID pins revert after
// fork: the fork's rows carry the source's provider ids, and the fork's
// slice keeps those uuids, so reverting inside the fork cuts at the stored
// id.
func TestConversationRollbackInClaudeForkResolvesStoredID(t *testing.T) {
	app := newTestApp(t)
	source, workspace := setupSliceIDsThread(t, app)
	sourceRows := readClaudeSessionRows(t, workspace, source.SessionRef)

	fork, err := app.ForkThreadFromMessage(t.Context(), source.ID, "user:2")
	if err != nil {
		t.Fatalf("fork from message: %v", err)
	}
	second := userItemBySummary(t, app, fork.ID, "second")
	if got := usermessage.ReadProviderItemID(second.Meta); got != "u1" {
		t.Fatalf("fork \"second\" provider_item_id = %q, want the source's u1", got)
	}

	if err := rollbackToMessage(app, fork.ID, second.ID); err != nil {
		t.Fatalf("rollback in fork: %v", err)
	}
	after, err := app.store.GetThread(fork.ID)
	if err != nil {
		t.Fatalf("get fork: %v", err)
	}
	if after.SessionRef == fork.SessionRef {
		t.Fatalf("rollback in fork kept session ref %q, want a new slice", after.SessionRef)
	}
	assertClaudeSliceKeepsSourceRows(t, workspace, after.SessionRef, sourceRows[:2])
}

// TestForkAfterClaudeRollbackResolvesStoredID pins fork after revert: the
// rollback leaves the surviving anchor's stored ids untouched and its
// slice keeps those uuids, so a fork from a surviving message cuts the
// rolled-back session at the stored id.
func TestForkAfterClaudeRollbackResolvesStoredID(t *testing.T) {
	app := newTestApp(t)
	source, workspace := setupSliceIDsThread(t, app)
	sourceRows := readClaudeSessionRows(t, workspace, source.SessionRef)

	if err := rollbackToMessage(app, source.ID, "user:2"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	fork, err := app.ForkThreadFromMessage(t.Context(), source.ID, "user:1")
	if err != nil {
		t.Fatalf("fork after rollback: %v", err)
	}
	assertClaudeSliceKeepsSourceRows(t, workspace, fork.SessionRef, sourceRows[:2])
	first := userItemBySummary(t, app, fork.ID, "first")
	if got := usermessage.ReadProviderItemID(first.Meta); got != "u0" {
		t.Fatalf("fork \"first\" provider_item_id = %q, want the source's u0", got)
	}
}

// TestConversationRollbackRefusesStaleIDsInsteadOfOrdinalWalk pins the
// no-ordinal-fallback rule: when both stored uuid copies (the item row's
// meta stamp and the anchor's) are absent from a session file that
// continues past the anchor, AO cannot tell which entry to cut at. The
// ordinal walk would miscount a CLI-merged queue batch, so the rollback
// fails and leaves the session and the timeline as they were.
func TestConversationRollbackRefusesStaleIDsInsteadOfOrdinalWalk(t *testing.T) {
	app := newTestApp(t)
	thread, workspace := setupSliceIDsThread(t, app)
	before, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}

	if err := app.store.UpdateMessageAnchorProviderIDs(thread.ID, "user:2", "uuid-not-in-file", "also-not-in-file"); err != nil {
		t.Fatalf("poison anchor uuid: %v", err)
	}
	if err := app.store.UpdateItemMeta(thread.ID, "user:2", `{"provider_item_id":"uuid-not-in-file"}`); err != nil {
		t.Fatalf("poison item meta uuid: %v", err)
	}

	err = rollbackToMessage(app, thread.ID, "user:2")
	if err == nil {
		t.Fatal("rollback with stale uuids succeeded; want a refusal rather than an ordinal guess")
	}
	if !strings.Contains(err.Error(), "uuid-not-in-file") {
		t.Fatalf("refusal does not name the missing uuid: %v", err)
	}
	after, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread after refusal: %v", err)
	}
	if after.SessionRef != before.SessionRef {
		t.Fatalf("session ref changed on a refused rollback: %q -> %q", before.SessionRef, after.SessionRef)
	}
	assertClaudeSessionText(t, workspace, after.SessionRef, []string{"first", "second", "third"}, nil)
	if _, found, err := app.store.GetThreadItem(thread.ID, "user:2"); err != nil || !found {
		t.Fatalf("refused rollback removed user:2 (found=%v err=%v)", found, err)
	}
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
