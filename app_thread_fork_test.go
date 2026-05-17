package main

import (
	"encoding/json"
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
// fork's cloned `items.meta.provider_item_id` and
// `thread_checkpoints.provider_user_message_id` columns would
// continue to reference UUIDs from the SOURCE session — UUIDs that
// don't exist in the fork's JSONL. A subsequent UUID-keyed revert
// inside the fork would then hit `ErrMessageNotFound` and fall back
// to the ordinal walk, defeating the structural fix for any
// fork-of-fork scenario.
//
// The remap is wired into the fork pipeline after `CloneThreadItems`
// + `copyForkCheckpoints` so cloned rows pick up the fork's NEW
// UUIDs from the same `BuildForkLinesWithUUIDMap` rewrite that produced the
// JSONL. This test verifies the resulting stored UUIDs match what
// the JSONL actually contains by looking up each entry via its
// `forkedFrom.messageUuid` backpointer.
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
	if len(cps) != 1 {
		t.Fatalf("fork checkpoints = %d, want 1 (only the user:1 checkpoint, since user:2 was dropped)", len(cps))
	}
	chk := cps[0]
	// `copyForkCheckpoints` remaps UserItemID to the cloned id — the
	// checkpoint is no longer keyed by "user:1". Identify it via the
	// linked user_text row instead.
	clonedUser1ID := bySummary["second"].ID
	if clonedUser1ID == "" {
		t.Fatalf("fork has no cloned user_text row for source user:1; cannot map checkpoint")
	}
	if chk.UserItemID != clonedUser1ID {
		t.Errorf("checkpoint UserItemID = %q, want fork-cloned id %q for source user:1", chk.UserItemID, clonedUser1ID)
	}
	if chk.ProviderUserMessageID != oldToNew["u1"] {
		t.Errorf("checkpoint ProviderUserMessageID = %q, want fork-remapped %q", chk.ProviderUserMessageID, oldToNew["u1"])
	}
	if chk.ProviderParentUUID != oldToNew["a0"] {
		t.Errorf("checkpoint ProviderParentUUID = %q, want fork-remapped %q", chk.ProviderParentUUID, oldToNew["a0"])
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

	// Sanity: fork1.checkpoint should already reference fork1 UUIDs
	// (proven by the standalone remap test above; we re-assert here
	// to scope failures cleanly).
	fork1Cps, err := app.store.ListCheckpoints(fork1.ID)
	if err != nil {
		t.Fatalf("list fork1 checkpoints: %v", err)
	}
	if len(fork1Cps) != 1 {
		t.Fatalf("fork1 checkpoints = %d, want 1", len(fork1Cps))
	}
	if fork1Cps[0].ProviderUserMessageID != fork1Map["u1"] {
		t.Fatalf("fork1 checkpoint ProviderUserMessageID = %q, want fork1-remapped %q", fork1Cps[0].ProviderUserMessageID, fork1Map["u1"])
	}

	// Now fork from fork1 at fork1's "second" message — that's the
	// clone of source user:1. CloneThreadItems mints fresh IDs, so
	// look up fork1's user_text id for the "second" summary first.
	fork1Items, err := app.store.ListItems(fork1.ID)
	if err != nil {
		t.Fatalf("list fork1 items: %v", err)
	}
	var fork1SecondID string
	for _, it := range fork1Items {
		if it.Kind == "user_text" && it.Summary == "second" {
			fork1SecondID = it.ID
			break
		}
	}
	if fork1SecondID == "" {
		t.Fatalf("fork1 missing \"second\" user_text row")
	}
	fork2, err := app.ForkThreadFromMessage(fork1.ID, fork1SecondID)
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

// saveSourceCheckpoint is a thin test helper for the fork tests: a
// checkpoint with the full provider-id surface (user UUID + parent
// UUID) that the remap needs to see. Captures the git ref first so
// the subsequent `copyForkCheckpoints` step (which CopyRef's the
// source ref onto a fork-scoped name) can resolve the source commit
// — without that, fork creation fails with
// `checkpoint: source ref "%s" is unavailable`.
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
