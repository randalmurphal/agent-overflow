package app

import (
	"fmt"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// threeTurnClaudeSession is a three-turn Claude transcript under sessionID.
func threeTurnClaudeSession(sessionID string) string {
	return `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"` + sessionID + `","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"` + sessionID + `","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"` + sessionID + `","message":{"role":"user","content":"second"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"` + sessionID + `","message":{"role":"assistant","content":[{"type":"text","text":"reply 1"}]}}
{"type":"user","uuid":"u2","parentUuid":"a1","sessionId":"` + sessionID + `","message":{"role":"user","content":"third"}}
{"type":"assistant","uuid":"a2","parentUuid":"u2","sessionId":"` + sessionID + `","message":{"role":"assistant","content":[{"type":"text","text":"reply 2"}]}}
`
}

// assertForkUserProviderIDs asserts each named user_text row of threadID
// carries the given provider_item_id.
func assertForkUserProviderIDs(t *testing.T, app *App, threadID string, want map[string]string) {
	t.Helper()
	for summary, id := range want {
		if got := usermessage.ReadProviderItemID(userItemBySummary(t, app, threadID, summary).Meta); got != id {
			t.Errorf("thread %s %q provider_item_id = %q, want the source's %q", threadID, summary, got, id)
		}
	}
}

// TestForkThreadFromMessageKeepsClaudeUUIDs pins the fork's id contract
// on the message-keyed path: the fork's slice holds the source's
// surviving rows under their original uuids and parents, so the provider
// ids the fork's rows carry from the source name entries in the fork's
// own transcript.
func TestForkThreadFromMessageKeepsClaudeUUIDs(t *testing.T) {
	app := newTestApp(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := initGitRepo(t)
	const sessionID = "fork-ids-source"
	writeClaudeProjectSession(t, home, workspace, sessionID, threeTurnClaudeSession(sessionID))
	source := createAppTestThread(t, app, "src-fork-ids", "claude", workspace)
	source.SessionRef = sessionID
	if err := app.store.UpdateThread(source); err != nil {
		t.Fatalf("update source: %v", err)
	}
	insertUserItemWithMeta(t, app.store, source.ID, "user:0", 0, "first", `{"provider_item_id":"u0"}`)
	insertUserItemWithMeta(t, app.store, source.ID, "user:1", 1, "second", `{"provider_item_id":"u1"}`)
	insertUserItemWithMeta(t, app.store, source.ID, "user:2", 2, "third", `{"provider_item_id":"u2"}`)
	seedMessageAnchor(t, app.store, source.ID, "user:1", 1, "u1", "a0")
	seedMessageAnchor(t, app.store, source.ID, "user:2", 2, "u2", "a1")
	sourceRows := readClaudeSessionRows(t, workspace, sessionID)

	fork, err := app.ForkThreadFromMessage(t.Context(), source.ID, "user:2")
	if err != nil {
		t.Fatalf("fork from message: %v", err)
	}

	assertClaudeSliceKeepsSourceRows(t, workspace, fork.SessionRef, sourceRows[:4])
	assertForkUserProviderIDs(t, app, fork.ID, map[string]string{"first": "u0", "second": "u1"})
	items, err := forkConversationItems(app.store, fork.ID)
	if err != nil {
		t.Fatalf("list fork items: %v", err)
	}
	for _, it := range items {
		if it.Summary == "third" {
			t.Errorf("fork unexpectedly contains \"third\" (we forked from it)")
		}
	}

	anchors, err := app.store.ListMessageAnchors(fork.ID)
	if err != nil {
		t.Fatalf("list fork anchors: %v", err)
	}
	if len(anchors) != 0 {
		t.Fatalf("fork message anchors = %d, want 0", len(anchors))
	}
}

// TestForkThreadAtTurnIndexKeepsClaudeUUIDs covers the turn-keyed
// `ForkThread` entry point. The slice anchor is computed from the
// `lookupTurnAnchorClaudeUUID` helper (the user_text at the first
// turn DROPPED from the fork); the id contract is the message-keyed one.
func TestForkThreadAtTurnIndexKeepsClaudeUUIDs(t *testing.T) {
	app := newTestApp(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := initGitRepo(t)
	const sessionID = "fork-turn-ids"
	writeClaudeProjectSession(t, home, workspace, sessionID, threeTurnClaudeSession(sessionID))
	source := createAppTestThread(t, app, "src-turn-ids", "claude", workspace)
	source.SessionRef = sessionID
	if err := app.store.UpdateThread(source); err != nil {
		t.Fatalf("update source: %v", err)
	}
	insertUserItemWithMeta(t, app.store, source.ID, "user:0", 0, "first", `{"provider_item_id":"u0"}`)
	insertUserItemWithMeta(t, app.store, source.ID, "user:1", 1, "second", `{"provider_item_id":"u1"}`)
	insertUserItemWithMeta(t, app.store, source.ID, "user:2", 2, "third", `{"provider_item_id":"u2"}`)
	sourceRows := readClaudeSessionRows(t, workspace, sessionID)

	atTurn := 1
	fork, err := app.ForkThread(t.Context(), source.ID, &atTurn)
	if err != nil {
		t.Fatalf("fork at turn: %v", err)
	}

	assertClaudeSliceKeepsSourceRows(t, workspace, fork.SessionRef, sourceRows[:4])
	assertForkUserProviderIDs(t, app, fork.ID, map[string]string{"first": "u0", "second": "u1"})
}

// TestForkOfForkKeepsClaudeUUIDs verifies the id contract across two
// consecutive forks: fork2, cut from fork1, still holds the source's
// uuids, so the provider ids inherited through both forks name entries in
// fork2's own transcript.
func TestForkOfForkKeepsClaudeUUIDs(t *testing.T) {
	app := newTestApp(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := initGitRepo(t)
	const sessionID = "fork-of-fork-source"
	writeClaudeProjectSession(t, home, workspace, sessionID, threeTurnClaudeSession(sessionID))
	source := createAppTestThread(t, app, "src-of-fork", "claude", workspace)
	source.SessionRef = sessionID
	if err := app.store.UpdateThread(source); err != nil {
		t.Fatalf("update source: %v", err)
	}
	insertUserItemWithMeta(t, app.store, source.ID, "user:0", 0, "first", `{"provider_item_id":"u0"}`)
	insertUserItemWithMeta(t, app.store, source.ID, "user:1", 1, "second", `{"provider_item_id":"u1"}`)
	insertUserItemWithMeta(t, app.store, source.ID, "user:2", 2, "third", `{"provider_item_id":"u2"}`)
	seedMessageAnchor(t, app.store, source.ID, "user:1", 1, "u1", "a0")
	seedMessageAnchor(t, app.store, source.ID, "user:2", 2, "u2", "a1")
	sourceRows := readClaudeSessionRows(t, workspace, sessionID)

	fork1, err := app.ForkThreadFromMessage(t.Context(), source.ID, "user:2")
	if err != nil {
		t.Fatalf("fork1: %v", err)
	}
	fork1Anchors, err := app.store.ListMessageAnchors(fork1.ID)
	if err != nil {
		t.Fatalf("list fork1 anchors: %v", err)
	}
	if len(fork1Anchors) != 0 {
		t.Fatalf("fork1 message anchors = %d, want 0", len(fork1Anchors))
	}

	atTurn := 0
	fork2, err := app.ForkThread(t.Context(), fork1.ID, &atTurn)
	if err != nil {
		t.Fatalf("fork2: %v", err)
	}
	if fork2.SessionRef == fork1.SessionRef || fork2.SessionRef == sessionID {
		t.Fatalf("fork2 session ref = %q, want its own slice", fork2.SessionRef)
	}
	assertClaudeSliceKeepsSourceRows(t, workspace, fork2.SessionRef, sourceRows[:2])
	assertForkUserProviderIDs(t, app, fork2.ID, map[string]string{"first": "u0"})
}

// TestForkThreadFromMessageKeepsSharedTurnPrefix — forking a Claude thread
// at a queued flush message that shares its turn with the prompt that was
// running when it was enqueued must keep that turn's prefix (prompt + agent
// work before the queued message) in the fork, matching the session-file
// slice which cuts at the message uuid, not the turn boundary.
func TestForkThreadFromMessageKeepsSharedTurnPrefix(t *testing.T) {
	app := newTestApp(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := initGitRepo(t)
	const sessionID = "fork-midturn-source"
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"fork-midturn-source","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"fork-midturn-source","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"fork-midturn-source","message":{"role":"user","content":"second"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"fork-midturn-source","message":{"role":"assistant","content":[{"type":"text","text":"reply 1"}]}}
{"type":"user","uuid":"uq","parentUuid":"a1","sessionId":"fork-midturn-source","message":{"role":"user","content":"queued"}}
{"type":"assistant","uuid":"aq","parentUuid":"uq","sessionId":"fork-midturn-source","message":{"role":"assistant","content":[{"type":"text","text":"queued reply"}]}}
`)
	source := createAppTestThread(t, app, "src-midturn-fork", "claude", workspace)
	source.SessionRef = sessionID
	if err := app.store.UpdateThread(source); err != nil {
		t.Fatalf("update source: %v", err)
	}
	insertUserItemWithMeta(t, app.store, source.ID, "user:0", 0, "first", `{"provider_item_id":"u0"}`)
	insertAssistantTextItem(t, app.store, source.ID, "a:0", 0, "reply 0")
	insertUserItemWithMeta(t, app.store, source.ID, "user:1", 1, "second", `{"provider_item_id":"u1"}`)
	insertAssistantTextItem(t, app.store, source.ID, "a:1", 1, "reply 1")
	insertUserItemWithMeta(t, app.store, source.ID, "user:q", 1, "queued", `{"provider_item_id":"uq"}`)
	insertAssistantTextItem(t, app.store, source.ID, "a:q", 1, "queued reply")
	seedMessageAnchor(t, app.store, source.ID, "user:q", 1, "uq", "a1")
	sourceRows := readClaudeSessionRows(t, workspace, sessionID)

	fork, err := app.ForkThreadFromMessage(t.Context(), source.ID, "user:q")
	if err != nil {
		t.Fatalf("fork from message: %v", err)
	}

	items, err := forkConversationItems(app.store, fork.ID)
	if err != nil {
		t.Fatalf("list fork items: %v", err)
	}
	var got []string
	for _, it := range items {
		got = append(got, it.Summary)
	}
	want := []string{"first", "reply 0", "second", "reply 1"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("fork items = %v, want %v", got, want)
	}

	assertClaudeSliceKeepsSourceRows(t, workspace, fork.SessionRef, sourceRows[:4])
	assertForkUserProviderIDs(t, app, fork.ID, map[string]string{"second": "u1"})
}

// TestForkThreadFromMessageMidTurnZeroKeepsPrefix — a queued message
// consumed during the very FIRST turn is a mid-turn-0 anchor: the fork must
// keep turn 0's prefix and slice the session, not start empty (the turn-0
// shortcut only applies to an anchor that OPENS turn 0).
func TestForkThreadFromMessageMidTurnZeroKeepsPrefix(t *testing.T) {
	app := newTestApp(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := initGitRepo(t)
	const sessionID = "fork-midturn0-source"
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"fork-midturn0-source","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"fork-midturn0-source","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"uq","parentUuid":"a0","sessionId":"fork-midturn0-source","message":{"role":"user","content":"queued"}}
{"type":"assistant","uuid":"aq","parentUuid":"uq","sessionId":"fork-midturn0-source","message":{"role":"assistant","content":[{"type":"text","text":"queued reply"}]}}
`)
	source := createAppTestThread(t, app, "src-midturn0-fork", "claude", workspace)
	source.SessionRef = sessionID
	if err := app.store.UpdateThread(source); err != nil {
		t.Fatalf("update source: %v", err)
	}
	insertUserItemWithMeta(t, app.store, source.ID, "user:0", 0, "first", `{"provider_item_id":"u0"}`)
	insertAssistantTextItem(t, app.store, source.ID, "a:0", 0, "reply 0")
	insertUserItemWithMeta(t, app.store, source.ID, "user:q", 0, "queued", `{"provider_item_id":"uq"}`)
	seedMessageAnchor(t, app.store, source.ID, "user:q", 0, "uq", "a0")
	sourceRows := readClaudeSessionRows(t, workspace, sessionID)

	fork, err := app.ForkThreadFromMessage(t.Context(), source.ID, "user:q")
	if err != nil {
		t.Fatalf("fork from message: %v", err)
	}

	items, err := forkConversationItems(app.store, fork.ID)
	if err != nil {
		t.Fatalf("list fork items: %v", err)
	}
	var got []string
	for _, it := range items {
		got = append(got, it.Summary)
	}
	want := []string{"first", "reply 0"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("fork items = %v, want %v", got, want)
	}
	if fork.SessionRef == "" {
		t.Fatal("mid-turn-0 fork must slice the session, not start fresh")
	}
	assertClaudeSliceKeepsSourceRows(t, workspace, fork.SessionRef, sourceRows[:2])
}

// TestLookupTurnAnchorClaudeUUIDPicksStampedUserItem locks the helper's
// happy path: a turn with a stamped user_text row returns that row's
// provider_item_id so the UUID-keyed fork-slice branch can be taken.
func TestLookupTurnAnchorClaudeUUIDPicksStampedUserItem(t *testing.T) {
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "anchor-happy", "claude", t.TempDir())
	insertUserItemWithMeta(t, app.store, thread.ID, "u:0", 0, "first", `{"provider_item_id":"u0"}`)
	insertUserItemWithMeta(t, app.store, thread.ID, "u:1", 1, "second", `{"provider_item_id":"u1"}`)

	if got := app.lookupTurnAnchorClaudeUUID(thread.ID, 1); got != "u1" {
		t.Errorf("lookupTurnAnchorClaudeUUID(turn=1) = %q, want u1", got)
	}
	if got := app.lookupTurnAnchorClaudeUUID(thread.ID, 0); got != "u0" {
		t.Errorf("lookupTurnAnchorClaudeUUID(turn=0) = %q, want u0", got)
	}
}

// TestLookupTurnAnchorClaudeUUIDSkipsWireOnlyUserItems covers the
// `IsWireOnlyUserItem` skip: a wire-only cascade-injected row at the
// requested turn must not be picked as the anchor — those rows aren't
// turn-boundary anchors and would mis-slice the fork.
func TestLookupTurnAnchorClaudeUUIDSkipsWireOnlyUserItems(t *testing.T) {
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "anchor-wireonly", "claude", t.TempDir())
	// AO-authored row that opens turn 1 (gets the anchor).
	insertUserItemWithMeta(t, app.store, thread.ID, "u:1-real", 1, "real prompt", `{"provider_item_id":"u1-real"}`)
	// Wire-only cascade row at the same turn (must be skipped).
	insertUserItemWithMeta(t, app.store, thread.ID, "u:1-wire", 1, "task notification", `{"provider_item_id":"u1-wire","wire_only":true}`)

	if got := app.lookupTurnAnchorClaudeUUID(thread.ID, 1); got != "u1-real" {
		t.Errorf("lookupTurnAnchorClaudeUUID(turn=1) = %q, want u1-real (wire-only must be skipped)", got)
	}
}

// TestLookupTurnAnchorClaudeUUIDReturnsEmptyForUnstamped locks the
// fallback signal: when no user_text row at the requested turn
// carries a stamped wire id, the helper returns "" so the caller can
// dispatch to the ordinal-walk fallback rather than passing an empty
// UUID into WriteForkFileForUserMessageUUID.
func TestLookupTurnAnchorClaudeUUIDReturnsEmptyForUnstamped(t *testing.T) {
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "anchor-unstamped", "claude", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "u:1", 1, "second")

	if got := app.lookupTurnAnchorClaudeUUID(thread.ID, 1); got != "" {
		t.Errorf("lookupTurnAnchorClaudeUUID(turn=1) = %q, want empty (no stamped row)", got)
	}
	if got := app.lookupTurnAnchorClaudeUUID(thread.ID, 99); got != "" {
		t.Errorf("lookupTurnAnchorClaudeUUID(turn=99) = %q, want empty (turn out of range)", got)
	}
}

// TestForkThreadFromMessageHeadHealedFirstPromptStartsFresh is the fork
// mirror of the R8-1 revert fix: a head-healed turn-0 prompt at a
// negative item_index opens turn 0, so a fork before it keeps nothing
// and starts a fresh provider session instead of attempting an empty
// slice.
func TestForkThreadFromMessageHeadHealedFirstPromptStartsFresh(t *testing.T) {
	app := newTestApp(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := initGitRepo(t)
	const sessionID = "fork-headheal-source"
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"fork-headheal-source","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"fork-headheal-source","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
`)
	source := createAppTestThread(t, app, "src-headheal-fork", "claude", workspace)
	source.SessionRef = sessionID
	if err := app.store.UpdateThread(source); err != nil {
		t.Fatalf("update source: %v", err)
	}
	now := time.Now().UnixMilli()
	if err := app.store.InsertItem(store.Item{
		ID: "user:0:flush:0", ThreadID: source.ID, TurnIndex: 0, ItemIndex: -1,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "first", Meta: `{"provider_item_id":"u0"}`,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("insert head-healed prompt: %v", err)
	}
	if err := app.store.InsertItem(store.Item{
		ID: "text:0:0", ThreadID: source.ID, TurnIndex: 0, ItemIndex: 0,
		Kind: "assistant_text", Role: "assistant", Status: "completed",
		Summary: "reply 0", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("insert response row: %v", err)
	}
	seedMessageAnchor(t, app.store, source.ID, "user:0:flush:0", 0, "u0", "")

	fork, err := app.ForkThreadFromMessage(t.Context(), source.ID, "user:0:flush:0")
	if err != nil {
		t.Fatalf("fork from head-healed first prompt: %v", err)
	}
	if fork.SessionRef != "" {
		t.Errorf("fork SessionRef = %q, want empty — forking before the turn-opening prompt keeps nothing", fork.SessionRef)
	}
	items, err := app.store.ListItems(fork.ID)
	if err != nil {
		t.Fatalf("list fork items: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("fork items = %+v, want none", items)
	}
}

// TestForkThreadFromMessageSynthesizesOnAnchorTurnDrift pins the drift
// posture after the anchor slimming: an anchor whose cached turn index
// drifted from its user item (R8-3 shapes) is
// DISCARDED and a fresh record is synthesized from the item row — the
// SQLite clone and the provider cut both derive from the item, so the
// two histories stay aligned and the fork succeeds.
func TestForkThreadFromMessageSynthesizesOnAnchorTurnDrift(t *testing.T) {
	app := newTestApp(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := t.TempDir()
	const sessionID = "fork-drift-source"
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"fork-drift-source","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"fork-drift-source","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"fork-drift-source","message":{"role":"user","content":"second"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"fork-drift-source","message":{"role":"assistant","content":[{"type":"text","text":"reply 1"}]}}
`)
	source := createAppTestThread(t, app, "src-drift-fork", "claude", workspace)
	source.SessionRef = sessionID
	if err := app.store.UpdateThread(source); err != nil {
		t.Fatalf("update source: %v", err)
	}
	insertUserItemWithMeta(t, app.store, source.ID, "user:0", 0, "first", `{"provider_item_id":"u0"}`)
	insertUserItemWithMeta(t, app.store, source.ID, "user:1", 1, "second", `{"provider_item_id":"u1"}`)
	// Anchor cached at the WRONG turn (0) for a turn-1 item.
	seedMessageAnchor(t, app.store, source.ID, "user:1", 0, "stale-uuid", "")

	fork, err := app.ForkThreadFromMessage(t.Context(), source.ID, "user:1")
	if err != nil {
		t.Fatalf("fork with drifted anchor turn: %v", err)
	}
	// The synthesized anchor keys the slice on the ITEM's provider id
	// (u1), so the fork keeps exactly turn 0.
	assertClaudeSessionText(t, workspace, fork.SessionRef, []string{"first"}, []string{"second"})
	items, err := forkConversationItems(app.store, fork.ID)
	if err != nil {
		t.Fatalf("list fork items: %v", err)
	}
	if len(items) != 1 || items[0].Summary != "first" {
		t.Fatalf("fork items = %+v, want only turn 0 user item", items)
	}
}
