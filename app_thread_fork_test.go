package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/checkpoint"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// TestForkThreadFromMessageRemapsClaudeUUIDs is the structural
// regression guard for the fork-time UUID remap. Without it, the
// fork's cloned `items.meta.provider_item_id` columns would continue to
// reference UUIDs from the SOURCE session — UUIDs that don't exist in the
// fork's JSONL.
//
// The remap is wired into the fork pipeline after `CloneThreadItems` so cloned
// rows pick up the fork's NEW UUIDs from the same
// `BuildForkLinesWithUUIDMap` rewrite that produced the JSONL. This test
// verifies the resulting stored UUIDs match what the JSONL actually contains
// by looking up each entry via its `forkedFrom.messageUuid` backpointer.
func TestForkThreadFromMessageRemapsClaudeUUIDs(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := initCheckpointRepo(t)
	const sessionID = "fork-remap-source"
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"fork-remap-source","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"fork-remap-source","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"fork-remap-source","message":{"role":"user","content":"second"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"fork-remap-source","message":{"role":"assistant","content":[{"type":"text","text":"reply 1"}]}}
{"type":"user","uuid":"u2","parentUuid":"a1","sessionId":"fork-remap-source","message":{"role":"user","content":"third"}}
{"type":"assistant","uuid":"a2","parentUuid":"u2","sessionId":"fork-remap-source","message":{"role":"assistant","content":[{"type":"text","text":"reply 2"}]}}
`)
	source := createCheckpointTestThread(t, app, "src-fork-remap", "claude", workspace)
	source.SessionRef = sessionID
	if err := app.store.UpdateThread(source); err != nil {
		t.Fatalf("update source: %v", err)
	}
	insertUserItemWithMeta(t, app.store, source.ID, "user:0", 0, "first", `{"provider_item_id":"u0"}`)
	insertUserItemWithMeta(t, app.store, source.ID, "user:1", 1, "second", `{"provider_item_id":"u1"}`)
	insertUserItemWithMeta(t, app.store, source.ID, "user:2", 2, "third", `{"provider_item_id":"u2"}`)
	saveSourceCheckpoint(t, app, source.ID, workspace, "chk-1", "user:1", 1, "u1", "a0")
	saveSourceCheckpoint(t, app, source.ID, workspace, "chk-2", "user:2", 2, "u2", "a1")

	fork, err := app.ForkThreadFromMessage(source.ID, "user:2")
	if err != nil {
		t.Fatalf("fork from message: %v", err)
	}

	oldToNew := readForkUUIDMap(t, workspace, fork.SessionRef)
	for _, oldUUID := range []string{"u0", "u1", "a0", "a1"} {
		if oldToNew[oldUUID] == "" {
			t.Fatalf("fork JSONL missing remap for %q: %v", oldUUID, oldToNew)
		}
	}
	// The dropped user (u2 / a2) MUST NOT appear in the fork JSONL.
	if oldToNew["u2"] != "" {
		t.Errorf("fork unexpectedly contains u2: mapped to %q", oldToNew["u2"])
	}

	items, err := app.store.ListItems(fork.ID)
	if err != nil {
		t.Fatalf("list fork items: %v", err)
	}
	// `CloneThreadItems` mints fresh UUIDs for every cloned row, so
	// the fork's items no longer match the source's "user:0" / "user:1"
	// IDs. Index by Summary instead — it's stable through cloning.
	bySummary := map[string]store.Item{}
	for _, it := range items {
		if it.Kind == "user_text" {
			bySummary[it.Summary] = it
		}
	}
	if got := usermessage.ReadProviderItemID(bySummary["first"].Meta); got != oldToNew["u0"] {
		t.Errorf("fork user:0 (\"first\") provider_item_id = %q, want fork-remapped %q", got, oldToNew["u0"])
	}
	if got := usermessage.ReadProviderItemID(bySummary["second"].Meta); got != oldToNew["u1"] {
		t.Errorf("fork user:1 (\"second\") provider_item_id = %q, want fork-remapped %q", got, oldToNew["u1"])
	}
	if _, ok := bySummary["third"]; ok {
		t.Errorf("fork unexpectedly contains \"third\" (we forked from it)")
	}

	cps, err := app.store.ListCheckpoints(fork.ID)
	if err != nil {
		t.Fatalf("list fork checkpoints: %v", err)
	}
	if len(cps) != 0 {
		t.Fatalf("fork checkpoints = %d, want 0", len(cps))
	}
}

// TestForkThreadAtTurnIndexRemapsClaudeUUIDs covers the turn-keyed
// `ForkThread` entry point. The slice anchor is computed from the
// `lookupTurnAnchorClaudeUUID` helper (the user_text at the first
// turn DROPPED from the fork). Otherwise the remap pipeline is the
// same as the message-keyed fork.
func TestForkThreadAtTurnIndexRemapsClaudeUUIDs(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := initCheckpointRepo(t)
	const sessionID = "fork-turn-remap"
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"fork-turn-remap","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"fork-turn-remap","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"fork-turn-remap","message":{"role":"user","content":"second"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"fork-turn-remap","message":{"role":"assistant","content":[{"type":"text","text":"reply 1"}]}}
{"type":"user","uuid":"u2","parentUuid":"a1","sessionId":"fork-turn-remap","message":{"role":"user","content":"third"}}
{"type":"assistant","uuid":"a2","parentUuid":"u2","sessionId":"fork-turn-remap","message":{"role":"assistant","content":[{"type":"text","text":"reply 2"}]}}
`)
	source := createCheckpointTestThread(t, app, "src-turn-remap", "claude", workspace)
	source.SessionRef = sessionID
	if err := app.store.UpdateThread(source); err != nil {
		t.Fatalf("update source: %v", err)
	}
	insertUserItemWithMeta(t, app.store, source.ID, "user:0", 0, "first", `{"provider_item_id":"u0"}`)
	insertUserItemWithMeta(t, app.store, source.ID, "user:1", 1, "second", `{"provider_item_id":"u1"}`)
	insertUserItemWithMeta(t, app.store, source.ID, "user:2", 2, "third", `{"provider_item_id":"u2"}`)

	atTurn := 1
	fork, err := app.ForkThread(source.ID, &atTurn)
	if err != nil {
		t.Fatalf("fork at turn: %v", err)
	}

	oldToNew := readForkUUIDMap(t, workspace, fork.SessionRef)
	for _, oldUUID := range []string{"u0", "u1", "a0", "a1"} {
		if oldToNew[oldUUID] == "" {
			t.Fatalf("fork JSONL missing remap for %q: %v", oldUUID, oldToNew)
		}
	}
	if oldToNew["u2"] != "" {
		t.Errorf("fork unexpectedly contains u2: mapped to %q", oldToNew["u2"])
	}

	items, err := app.store.ListItems(fork.ID)
	if err != nil {
		t.Fatalf("list fork items: %v", err)
	}
	bySummary := map[string]store.Item{}
	for _, it := range items {
		if it.Kind == "user_text" {
			bySummary[it.Summary] = it
		}
	}
	if got := usermessage.ReadProviderItemID(bySummary["first"].Meta); got != oldToNew["u0"] {
		t.Errorf("fork \"first\" provider_item_id = %q, want fork-remapped %q", got, oldToNew["u0"])
	}
	if got := usermessage.ReadProviderItemID(bySummary["second"].Meta); got != oldToNew["u1"] {
		t.Errorf("fork \"second\" provider_item_id = %q, want fork-remapped %q", got, oldToNew["u1"])
	}
}

// TestForkOfForkRemapStaysCorrect verifies the remap chain holds
// across two consecutive forks. After fork1 from source and fork2
// from fork1, fork2's stored UUIDs MUST reference fork2's JSONL
// UUIDs (not fork1's, and definitely not source's). Without the
// per-fork remap, fork2's UUIDs would stale-reference fork1's, and
// a UUID-keyed revert inside fork2 would fall back to the ordinal
// walk — defeating the structural fix when users chain forks.
func TestForkOfForkRemapStaysCorrect(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := initCheckpointRepo(t)
	const sessionID = "fork-of-fork-source"
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"fork-of-fork-source","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"fork-of-fork-source","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"fork-of-fork-source","message":{"role":"user","content":"second"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"fork-of-fork-source","message":{"role":"assistant","content":[{"type":"text","text":"reply 1"}]}}
{"type":"user","uuid":"u2","parentUuid":"a1","sessionId":"fork-of-fork-source","message":{"role":"user","content":"third"}}
{"type":"assistant","uuid":"a2","parentUuid":"u2","sessionId":"fork-of-fork-source","message":{"role":"assistant","content":[{"type":"text","text":"reply 2"}]}}
`)
	source := createCheckpointTestThread(t, app, "src-of-fork", "claude", workspace)
	source.SessionRef = sessionID
	if err := app.store.UpdateThread(source); err != nil {
		t.Fatalf("update source: %v", err)
	}
	insertUserItemWithMeta(t, app.store, source.ID, "user:0", 0, "first", `{"provider_item_id":"u0"}`)
	insertUserItemWithMeta(t, app.store, source.ID, "user:1", 1, "second", `{"provider_item_id":"u1"}`)
	insertUserItemWithMeta(t, app.store, source.ID, "user:2", 2, "third", `{"provider_item_id":"u2"}`)
	saveSourceCheckpoint(t, app, source.ID, workspace, "chk-1", "user:1", 1, "u1", "a0")
	saveSourceCheckpoint(t, app, source.ID, workspace, "chk-2", "user:2", 2, "u2", "a1")

	fork1, err := app.ForkThreadFromMessage(source.ID, "user:2")
	if err != nil {
		t.Fatalf("fork1: %v", err)
	}
	fork1Map := readForkUUIDMap(t, workspace, fork1.SessionRef)

	fork1Cps, err := app.store.ListCheckpoints(fork1.ID)
	if err != nil {
		t.Fatalf("list fork1 checkpoints: %v", err)
	}
	if len(fork1Cps) != 0 {
		t.Fatalf("fork1 checkpoints = %d, want 0", len(fork1Cps))
	}

	atTurn := 0
	fork2, err := app.ForkThread(fork1.ID, &atTurn)
	if err != nil {
		t.Fatalf("fork2: %v", err)
	}
	fork2Map := readForkUUIDMap(t, workspace, fork2.SessionRef)

	// fork2.JSONL backpointers should point at FORK1's UUIDs (because
	// fork1 is the immediate source), not source's. Verify by
	// confirming fork2 cannot resolve "u0" via forkedFrom.messageUuid
	// (that's fork1's old reference, not the now-current one).
	if fork2Map["u0"] != "" {
		t.Errorf("fork2 JSONL references source's u0 directly — expected only fork1's UUID")
	}
	if fork2Map[fork1Map["u0"]] == "" {
		t.Fatalf("fork2 JSONL missing fork1→fork2 remap for u0 (fork1 UUID %q): %v", fork1Map["u0"], fork2Map)
	}

	// fork2 cloned "first" (source user:0) from fork1. Its
	// meta.provider_item_id was fork1's UUID at clone time; after the
	// fork2 remap it must be fork2's UUID — i.e. the fork1→fork2
	// remap of the fork1 UUID.
	items, err := app.store.ListItems(fork2.ID)
	if err != nil {
		t.Fatalf("list fork2 items: %v", err)
	}
	var fork2First store.Item
	for _, it := range items {
		if it.Kind == "user_text" && it.Summary == "first" {
			fork2First = it
			break
		}
	}
	if fork2First.ID == "" {
		t.Fatalf("fork2 missing \"first\" user_text row")
	}
	wantUUID := fork2Map[fork1Map["u0"]]
	if got := usermessage.ReadProviderItemID(fork2First.Meta); got != wantUUID {
		t.Errorf("fork2 \"first\" provider_item_id = %q, want fork2-remapped %q (chain: u0 → %q → %q)", got, wantUUID, fork1Map["u0"], wantUUID)
	}
}

// TestForkThreadFromMessageKeepsSharedTurnPrefix — forking a Claude thread
// at a queued flush message that shares its turn with the prompt that was
// running when it was enqueued must keep that turn's prefix (prompt + agent
// work before the queued message) in the fork, matching the session-file
// slice which cuts at the message uuid, not the turn boundary.
func TestForkThreadFromMessageKeepsSharedTurnPrefix(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := initCheckpointRepo(t)
	const sessionID = "fork-midturn-source"
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"fork-midturn-source","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"fork-midturn-source","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"fork-midturn-source","message":{"role":"user","content":"second"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"fork-midturn-source","message":{"role":"assistant","content":[{"type":"text","text":"reply 1"}]}}
{"type":"user","uuid":"uq","parentUuid":"a1","sessionId":"fork-midturn-source","message":{"role":"user","content":"queued"}}
{"type":"assistant","uuid":"aq","parentUuid":"uq","sessionId":"fork-midturn-source","message":{"role":"assistant","content":[{"type":"text","text":"queued reply"}]}}
`)
	source := createCheckpointTestThread(t, app, "src-midturn-fork", "claude", workspace)
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
	saveSourceCheckpoint(t, app, source.ID, workspace, "chk-q", "user:q", 1, "uq", "a1")

	fork, err := app.ForkThreadFromMessage(source.ID, "user:q")
	if err != nil {
		t.Fatalf("fork from message: %v", err)
	}

	items, err := app.store.ListItems(fork.ID)
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

	oldToNew := readForkUUIDMap(t, workspace, fork.SessionRef)
	for _, oldUUID := range []string{"u0", "a0", "u1", "a1"} {
		if oldToNew[oldUUID] == "" {
			t.Fatalf("fork JSONL missing remap for %q: %v", oldUUID, oldToNew)
		}
	}
	if oldToNew["uq"] != "" || oldToNew["aq"] != "" {
		t.Errorf("fork JSONL kept the queued message or its reply: %v", oldToNew)
	}
	bySummary := map[string]store.Item{}
	for _, it := range items {
		if it.Kind == "user_text" {
			bySummary[it.Summary] = it
		}
	}
	if gotID := usermessage.ReadProviderItemID(bySummary["second"].Meta); gotID != oldToNew["u1"] {
		t.Errorf("fork \"second\" provider_item_id = %q, want fork-remapped %q", gotID, oldToNew["u1"])
	}
}

// TestForkThreadFromMessageMidTurnZeroKeepsPrefix — a queued message
// consumed during the very FIRST turn is a mid-turn-0 anchor: the fork must
// keep turn 0's prefix and slice the session, not start empty (the turn-0
// shortcut only applies to an anchor that OPENS turn 0).
func TestForkThreadFromMessageMidTurnZeroKeepsPrefix(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := initCheckpointRepo(t)
	const sessionID = "fork-midturn0-source"
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"fork-midturn0-source","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"fork-midturn0-source","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"uq","parentUuid":"a0","sessionId":"fork-midturn0-source","message":{"role":"user","content":"queued"}}
{"type":"assistant","uuid":"aq","parentUuid":"uq","sessionId":"fork-midturn0-source","message":{"role":"assistant","content":[{"type":"text","text":"queued reply"}]}}
`)
	source := createCheckpointTestThread(t, app, "src-midturn0-fork", "claude", workspace)
	source.SessionRef = sessionID
	if err := app.store.UpdateThread(source); err != nil {
		t.Fatalf("update source: %v", err)
	}
	insertUserItemWithMeta(t, app.store, source.ID, "user:0", 0, "first", `{"provider_item_id":"u0"}`)
	insertAssistantTextItem(t, app.store, source.ID, "a:0", 0, "reply 0")
	insertUserItemWithMeta(t, app.store, source.ID, "user:q", 0, "queued", `{"provider_item_id":"uq"}`)
	saveSourceCheckpoint(t, app, source.ID, workspace, "chk-q0", "user:q", 0, "uq", "a0")

	fork, err := app.ForkThreadFromMessage(source.ID, "user:q")
	if err != nil {
		t.Fatalf("fork from message: %v", err)
	}

	items, err := app.store.ListItems(fork.ID)
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
	oldToNew := readForkUUIDMap(t, workspace, fork.SessionRef)
	if oldToNew["u0"] == "" || oldToNew["a0"] == "" {
		t.Fatalf("fork JSONL missing turn-0 prefix: %v", oldToNew)
	}
	if oldToNew["uq"] != "" {
		t.Errorf("fork JSONL kept the queued message: %v", oldToNew)
	}
}

// saveSourceCheckpoint is a thin test helper for the fork tests: a
// checkpoint with the full provider-id surface (user UUID + parent
// UUID) that message-keyed forks use to find their Claude slice point.
func saveSourceCheckpoint(t *testing.T, app *App, threadID, workspace, chkID, userItemID string, turnIndex int, providerUserUUID, providerParentUUID string) {
	t.Helper()
	ref := checkpoint.ThreadRefPrefix(threadID) + "message/" + chkID
	if err := app.checkpointStore().CaptureRef(t.Context(), workspace, ref); err != nil {
		t.Fatalf("capture checkpoint %s ref: %v", chkID, err)
	}
	if err := app.store.SaveCheckpoint(store.Checkpoint{
		ID:                    chkID,
		ThreadID:              threadID,
		UserItemID:            userItemID,
		TurnIndex:             turnIndex,
		ProviderUserMessageID: providerUserUUID,
		ProviderParentUUID:    providerParentUUID,
		RefName:               ref,
		CapturedAt:            time.Now().UnixMilli(),
		WorkspacePath:         workspace,
	}); err != nil {
		t.Fatalf("save checkpoint %s: %v", chkID, err)
	}
}

// readForkUUIDMap walks a forked JSONL and returns a
// {sourceUUID → forkUUID} map keyed off each entry's
// `forkedFrom.messageUuid` backpointer (added by `BuildForkLinesWithUUIDMap`).
// The map covers every transcript entry that survived the slice.
// Empty when the file is missing or unreadable — the caller should
// assert non-empty before keying into it.
func readForkUUIDMap(t *testing.T, workspace, forkSessionRef string) map[string]string {
	t.Helper()
	path, err := sessionfork.LocateSessionFile(forkSessionRef, workspace)
	if err != nil {
		t.Fatalf("locate fork session %q: %v", forkSessionRef, err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fork session %s: %v", path, err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		newUUID, _ := entry["uuid"].(string)
		forkedFrom, _ := entry["forkedFrom"].(map[string]any)
		if forkedFrom == nil {
			continue
		}
		oldUUID, _ := forkedFrom["messageUuid"].(string)
		if oldUUID == "" || newUUID == "" {
			continue
		}
		out[oldUUID] = newUUID
	}
	return out
}

// TestLookupTurnAnchorClaudeUUIDPicksStampedUserItem locks the helper's
// happy path: a turn with a stamped user_text row returns that row's
// provider_item_id so the UUID-keyed fork-slice branch can be taken.
func TestLookupTurnAnchorClaudeUUIDPicksStampedUserItem(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createCheckpointTestThread(t, app, "anchor-happy", "claude", t.TempDir())
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
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createCheckpointTestThread(t, app, "anchor-wireonly", "claude", t.TempDir())
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
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createCheckpointTestThread(t, app, "anchor-unstamped", "claude", t.TempDir())
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
	app, cleanup := newTestApp(t)
	defer cleanup()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := initCheckpointRepo(t)
	const sessionID = "fork-headheal-source"
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"fork-headheal-source","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"fork-headheal-source","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
`)
	source := createCheckpointTestThread(t, app, "src-headheal-fork", "claude", workspace)
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
	saveSourceCheckpoint(t, app, source.ID, workspace, "chk-headheal", "user:0:flush:0", 0, "u0", "")

	fork, err := app.ForkThreadFromMessage(source.ID, "user:0:flush:0")
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

// TestForkThreadFromMessageRejectsCheckpointTurnDrift pins R8-4
// (round 8): the SQLite clone cuts at the item's turn while the
// provider cut derives from the checkpoint row, so a checkpoint whose
// cached turn drifted from its user item must fail the fork loudly —
// the same guard the revert path applies — instead of silently
// splitting the two histories.
func TestForkThreadFromMessageRejectsCheckpointTurnDrift(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	workspace := initCheckpointRepo(t)
	source := createCheckpointTestThread(t, app, "src-drift-fork", "claude", workspace)
	now := time.Now().UnixMilli()
	if err := app.store.InsertItem(store.Item{
		ID: "user:1", ThreadID: source.ID, TurnIndex: 1, ItemIndex: 0,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "second prompt", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("insert prompt: %v", err)
	}
	saveSourceCheckpoint(t, app, source.ID, workspace, "chk-drift", "user:1", 0, "", "")

	if _, err := app.ForkThreadFromMessage(source.ID, "user:1"); err == nil ||
		!strings.Contains(err.Error(), "does not match user message turn index") {
		t.Fatalf("fork with drifted checkpoint turn = %v, want turn-mismatch error", err)
	}
}
