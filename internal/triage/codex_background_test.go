package triage

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// buildUnifiedExecStartMeta is the Meta blob the Codex parser emits for
// an `item/started` with source=unifiedExecStartup. Minimal shape —
// projector only reads the top-level enrichment keys added by
// enrichItemMeta (source, item_status, process_id).
func buildUnifiedExecStartMeta(t *testing.T, processID string) json.RawMessage {
	t.Helper()
	meta := map[string]any{
		"source":      "unifiedExecStartup",
		"item_status": "inProgress",
		"process_id":  processID,
		"item": map[string]any{
			"id":        "cmd-1",
			"type":      "commandExecution",
			"source":    "unifiedExecStartup",
			"status":    "inProgress",
			"processId": processID,
			"command":   "sleep 3600",
		},
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal unified-exec start meta: %v", err)
	}
	return encoded
}

// seedOpenTurn puts the router in the "turn in flight" state without
// needing to drive a full EventTurnStart. The projector's
// turn-correlation lookups depend on currentTurnIndex — which needs an
// open turn — so every test that drives item-level events needs a
// parent turn.
func seedOpenTurn(t *testing.T, router *Router, st *store.Store, threadID string, turnIndex int) {
	t.Helper()
	router.setOpenTurn(threadID, turnIndex)
	// Also persist a turns row so currentTurnIndex's store fallback
	// doesn't need to synthesize one.
	turnID := threadID + ":" + strconv.Itoa(turnIndex)
	if err := st.InsertTurn(store.Turn{
		TurnID:    turnID,
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("insert turns row: %v", err)
	}
}

// TestCodexBackgroundProjector_TextDeltaAfterYieldStampsBackground
// covers the primary lifecycle: a unifiedExec command starts, then the
// model emits a text delta (yield), which must stamp is_background=true
// on the launch row and re-emit the upsert.
func TestCodexBackgroundProjector_TextDeltaAfterYieldStampsBackground(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startMeta := buildUnifiedExecStartMeta(t, "pid-42")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-1",
		ItemType: "commandExecution", TurnID: "turn-0", Meta: startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}

	// Before any yield the launch row is NOT backgrounded.
	launch, found, err := st.GetThreadItem("t1", "cmd-1")
	if err != nil || !found {
		t.Fatalf("lookup launch: found=%v err=%v", found, err)
	}
	if launch.IsBackground {
		t.Fatal("launch IsBackground=true before any yield (heuristic leak)")
	}

	// Model yield: first assistant_text delta.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1",
		Content: "I'll let that run in the background ", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}

	stamped, found, err := st.GetThreadItem("t1", "cmd-1")
	if err != nil || !found {
		t.Fatalf("lookup stamped: found=%v err=%v", found, err)
	}
	if !stamped.IsBackground {
		t.Fatal("launch IsBackground still false after yield — projector missed the text delta")
	}
	if stamped.Status != statusRunning {
		t.Errorf("launch status=%q, want %q (background stays running until completion)", stamped.Status, statusRunning)
	}

	// Projector emits provider:item_upsert for the stamped row so the UI
	// reconciler sees the flag flip without waiting for the next event.
	sawStampEmission := false
	for _, e := range *emissions {
		if e.eventName != "provider:item_upsert" {
			continue
		}
		item, ok := e.data.(store.Item)
		if !ok || item.ID != "cmd-1" {
			continue
		}
		if item.IsBackground {
			sawStampEmission = true
			break
		}
	}
	if !sawStampEmission {
		t.Error("no provider:item_upsert with IsBackground=true emitted for cmd-1")
	}
}

// TestCodexBackgroundProjector_ItemCompletedBeforeYieldLeavesNormal
// pins the "synchronous command" case: a unifiedExec that finishes
// before the model emits any text/reasoning must NOT be flagged
// backgrounded, and must NOT synthesize a sibling completion row — the
// launch row itself carries the terminal status.
func TestCodexBackgroundProjector_ItemCompletedBeforeYieldLeavesNormal(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startMeta := buildUnifiedExecStartMeta(t, "pid-quick")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-quick",
		ItemType: "commandExecution", TurnID: "turn-0", Meta: startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}

	completedMeta := buildItemCompletedMeta(t, "completed", "")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "cmd-quick",
		ItemType: "commandExecution", TurnID: "turn-0", Meta: completedMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool complete: %v", err)
	}

	launch, _, _ := st.GetThreadItem("t1", "cmd-quick")
	if launch.IsBackground {
		t.Fatal("synchronous unifiedExec flagged is_background — projector triggered on completion instead of yield")
	}

	siblings := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(siblings) != 0 {
		t.Fatalf("synchronous unifiedExec produced a sibling completion row (got %d)", len(siblings))
	}
}

// TestCodexBackgroundProjector_ParallelSiblingsDontTriggerYield
// pins the most surprising case: two unifiedExec starts in the same
// turn (parallel batch) must NOT mark each other as backgrounded. Only
// a real text/reasoning yield should flip them. A naive
// "second item_started means the first has backgrounded" heuristic
// would corrupt parallel tool calls.
func TestCodexBackgroundProjector_ParallelSiblingsDontTriggerYield(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startA := buildUnifiedExecStartMeta(t, "pid-A")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-A",
		ItemType: "commandExecution", TurnID: "turn-0", Meta: startA,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start A: %v", err)
	}
	startB := buildUnifiedExecStartMeta(t, "pid-B")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-B",
		ItemType: "commandExecution", TurnID: "turn-0", Meta: startB,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start B: %v", err)
	}

	for _, id := range []string{"cmd-A", "cmd-B"} {
		item, _, _ := st.GetThreadItem("t1", id)
		if item.IsBackground {
			t.Errorf("%s flagged is_background after a sibling item_start (heuristic leak)", id)
		}
	}
}

// TestCodexBackgroundProjector_TurnCompletedStampsBackgroundOnRemaining
// guards the catchall: a unifiedExec that was still inProgress when
// turn/completed fires without any preceding yield must still be
// flagged backgrounded. This is the edge case where the model finishes
// a turn without emitting text (tool_call-only turn or immediate
// tool-use with no narrative) and the command simply keeps running.
func TestCodexBackgroundProjector_TurnCompletedStampsBackgroundOnRemaining(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnID: "turn-0",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	startMeta := buildUnifiedExecStartMeta(t, "pid-silent")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-silent",
		ItemType: "commandExecution", TurnID: "turn-0", Meta: startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}

	// turn/completed without any preceding text/reasoning.
	completeMeta, _ := json.Marshal(map[string]any{"turn_status": "completed"})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1", TurnID: "turn-0",
		Meta: completeMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}

	stamped, _, _ := st.GetThreadItem("t1", "cmd-silent")
	if !stamped.IsBackground {
		t.Fatal("catchall did not stamp is_background on the silent unifiedExec row")
	}
	if stamped.Status != statusRunning {
		t.Errorf("silent unifiedExec status=%q, want %q (force-close must NOT flip a just-stamped bg row)", stamped.Status, statusRunning)
	}
}

// buildItemCompletedMeta produces the Meta blob for an item/completed
// EventToolComplete. Mirrors enrichItemMeta's output: `item_status` is
// what the projector reads to decide between completed/failed/killed
// on the sibling row.
func buildItemCompletedMeta(t *testing.T, status, source string) json.RawMessage {
	t.Helper()
	m := map[string]any{
		"item_status": status,
	}
	if source != "" {
		m["source"] = source
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal completed meta: %v", err)
	}
	return out
}

// TestCodexBackgroundCompletion_SynthesizesSiblingRow verifies the
// end-of-life flow: a backgrounded unifiedExec command eventually
// closes via item/completed → the projector synthesizes a
// tool_completion sibling row, stable id, completion_of set to launch,
// is_background=true propagated. The launch row itself stays running
// (invariant 24 — its sibling carries the terminal state).
func TestCodexBackgroundCompletion_SynthesizesSiblingRow(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startMeta := buildUnifiedExecStartMeta(t, "pid-1")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-1",
		ItemType: "commandExecution", TurnID: "turn-0", Meta: startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}

	// Yield — stamps backgrounded.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1",
		Content: "letting that run...", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}

	// Close the text block so the sibling isn't queued (the completion
	// itself happens after the streaming block closes in this test).
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1",
		Meta: json.RawMessage(`{"blockType":"text"}`), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}

	completedMeta := buildItemCompletedMeta(t, "completed", "unifiedExecStartup")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "cmd-1",
		ItemType: "commandExecution", TurnID: "turn-0", Meta: completedMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool complete: %v", err)
	}

	siblings := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(siblings) != 1 {
		t.Fatalf("expected 1 sibling tool_completion, got %d", len(siblings))
	}
	sibling := siblings[0]
	if sibling.ID != nextToolCompletionID("cmd-1") {
		t.Errorf("sibling id=%q, want %q", sibling.ID, nextToolCompletionID("cmd-1"))
	}
	if sibling.CompletionOf != "cmd-1" {
		t.Errorf("sibling completion_of=%q, want cmd-1", sibling.CompletionOf)
	}
	if !sibling.IsBackground {
		t.Error("sibling IsBackground=false")
	}
	if sibling.Status != statusCompleted {
		t.Errorf("sibling status=%q, want %q", sibling.Status, statusCompleted)
	}
}

// TestCodexBackgroundCompletion_DefersDuringStreaming guards against
// mid-stream injection: a backgrounded item's item/completed that
// arrives while an assistant text block is streaming must queue behind
// the active block (via maybeDeferOrPersist) and only land after the
// block closes. Mirrors Claude's background task terminal behaviour.
func TestCodexBackgroundCompletion_DefersDuringStreaming(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startMeta := buildUnifiedExecStartMeta(t, "pid-defer")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-defer",
		ItemType: "commandExecution", TurnID: "turn-0", Meta: startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}

	// Yield 1: text delta stamps backgrounded AND opens a streaming text
	// block.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1",
		Content: "running this in the background ", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}

	// Item completes WHILE text is still streaming.
	completedMeta := buildItemCompletedMeta(t, "completed", "unifiedExecStartup")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "cmd-defer",
		ItemType: "commandExecution", TurnID: "turn-0", Meta: completedMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool complete: %v", err)
	}

	// Sibling must NOT yet be persisted — queued behind the active text.
	queued := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(queued) != 0 {
		t.Fatalf("sibling persisted mid-stream (should defer); got %d", len(queued))
	}
	router.mu.Lock()
	qlen := len(router.interruptQueue["t1"])
	router.mu.Unlock()
	if qlen == 0 {
		t.Fatal("interrupt queue is empty — completion was not deferred")
	}

	// Close the text block; drain fires; sibling lands.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1",
		Meta: json.RawMessage(`{"blockType":"text"}`), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}

	drained := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(drained) != 1 {
		t.Fatalf("expected 1 sibling after drain, got %d", len(drained))
	}
	if drained[0].CompletionOf != "cmd-defer" {
		t.Errorf("drained completion_of=%q, want cmd-defer", drained[0].CompletionOf)
	}
}

// TestCodexBackgroundCompletion_IdempotentOnDuplicateDelivery pins the
// stable-id upsert contract: a duplicate item/completed (e.g. reconnect
// replay) must produce a single sibling row, not a second one.
func TestCodexBackgroundCompletion_IdempotentOnDuplicateDelivery(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startMeta := buildUnifiedExecStartMeta(t, "pid-dup")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-dup",
		ItemType: "commandExecution", TurnID: "turn-0", Meta: startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1",
		Content: "...", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1",
		Meta: json.RawMessage(`{"blockType":"text"}`), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}

	completedMeta := buildItemCompletedMeta(t, "completed", "unifiedExecStartup")
	for i := 0; i < 2; i++ {
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "cmd-dup",
			ItemType: "commandExecution", TurnID: "turn-0", Meta: completedMeta,
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("tool complete (iteration %d): %v", i, err)
		}
	}

	siblings := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(siblings) != 1 {
		t.Fatalf("duplicate item/completed produced %d siblings, want 1 (stable-id upsert failed)", len(siblings))
	}
}

// TestCodexBackgroundCompletion_TailPlacement verifies the "lands at
// the latest turn" rule: a unifiedExec spawned in turn 0 that finishes
// during turn 3 must place its sibling at turn_index=3, NOT turn 0.
// Long-running backgrounded commands can span multiple turns; the
// completion row belongs where the timeline's write-head is at
// completion time, so users see it next to the current activity.
func TestCodexBackgroundCompletion_TailPlacement(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Turn 0: spawn + yield + complete the turn.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnID: "turn-0",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 start: %v", err)
	}
	startMeta := buildUnifiedExecStartMeta(t, "pid-long")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-long",
		ItemType: "commandExecution", TurnID: "turn-0", Meta: startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1",
		Content: "letting this run", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1",
		Meta: json.RawMessage(`{"blockType":"text"}`), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}
	completeMeta, _ := json.Marshal(map[string]any{"turn_status": "completed"})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1", TurnID: "turn-0",
		Meta: completeMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 complete: %v", err)
	}

	// Turns 1, 2, 3 — just open each and close it, advancing the
	// timeline. The key thing is that currentTurnIndex should be 3 when
	// the sibling lands. TurnIndex is stamped explicitly since there's
	// no item in each turn (LastTurnIndex reads from items.turn_index).
	for i := 1; i <= 3; i++ {
		turnID := "turn-" + strconv.Itoa(i)
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventTurnStart, ThreadID: "t1", TurnID: turnID,
			TurnIndex: i, Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("turn %d start: %v", i, err)
		}
		if i < 3 {
			if err := router.Handle(provider.ProviderEvent{
				Kind: provider.EventTurnComplete, ThreadID: "t1", TurnID: turnID,
				TurnIndex: i, Meta: completeMeta, Timestamp: time.Now(),
			}); err != nil {
				t.Fatalf("turn %d complete: %v", i, err)
			}
		}
	}

	// At this point turn 3 is open, cmd-long is still running-and-bg.
	// Deliver item/completed for cmd-long during turn 3.
	doneMeta := buildItemCompletedMeta(t, "completed", "unifiedExecStartup")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "cmd-long",
		ItemType: "commandExecution", TurnID: "turn-3", Meta: doneMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool complete: %v", err)
	}

	siblings := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(siblings) != 1 {
		t.Fatalf("expected 1 sibling, got %d", len(siblings))
	}
	if siblings[0].TurnIndex != 3 {
		t.Errorf("sibling turn_index=%d, want 3 (tail of timeline at completion time)", siblings[0].TurnIndex)
	}

	// Launch row still on turn 0 (original turn).
	launch, _, _ := st.GetThreadItem("t1", "cmd-long")
	if launch.TurnIndex != 0 {
		t.Errorf("launch turn_index=%d, want 0 (unchanged from launch turn)", launch.TurnIndex)
	}
}

// buildSpawnAgentMeta is the Meta blob Codex's parser emits for an
// item/completed with type=collabAgentToolCall tool=spawnAgent. Mirrors
// the enrichItemMeta output: `input.agentsStates` carries the per-child
// state map and `input.receiverThreadIds` lists the child thread ids.
func buildSpawnAgentMeta(t *testing.T, childID, childStatus string) json.RawMessage {
	t.Helper()
	m := map[string]any{
		"source":      "",
		"item_status": "completed",
		"toolName":    "collab_agent",
		"input": map[string]any{
			"tool":              "spawn_agent",
			"receiverThreadIds": []string{childID},
			"agentsStates": map[string]any{
				childID: map[string]any{"status": childStatus},
			},
		},
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal spawn_agent meta: %v", err)
	}
	return out
}

// TestCodexSubagentRunningPastTurnEnd_Backgrounded covers the second
// Codex background case: a spawn_agent tool call whose child is still
// running when the parent's turn/completed fires must be flagged
// is_background=true. The spawn row itself is wire-level "completed"
// already — backgrounding is not about its own status, it's about the
// work still happening on the child thread.
func TestCodexSubagentRunningPastTurnEnd_Backgrounded(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnID: "turn-0",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	spawnStartMeta := buildSpawnAgentMeta(t, "child-1", "running")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "spawn-1",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnStartMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn start: %v", err)
	}
	// Wire: spawn_agent completes immediately; child is still running.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "spawn-1",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnStartMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn complete: %v", err)
	}

	// Before turn complete: spawn row should not yet be backgrounded
	// (the wire just says "spawn finished" and there's no yield signal
	// to act on — we wait for turn/completed).
	before, _, _ := st.GetThreadItem("t1", "spawn-1")
	if before.IsBackground {
		t.Fatal("spawn flagged is_background before turn close (premature)")
	}

	// turn/completed — catchall stamps is_background.
	turnCompleteMeta, _ := json.Marshal(map[string]any{"turn_status": "completed"})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1", TurnID: "turn-0",
		Meta: turnCompleteMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}

	after, _, _ := st.GetThreadItem("t1", "spawn-1")
	if !after.IsBackground {
		t.Error("spawn_agent with running child past turn end was not flagged is_background")
	}
	// Status reverts to running so ListLiveBackgroundTasks surfaces the
	// launch in the tray — wire-level "completed" is misleading while
	// the child thread is still active. See invariant 24 + the
	// tray-visibility note on stampCodexItemBackgrounded.
	if after.Status != statusRunning {
		t.Errorf("spawn_agent status = %q, want %q (invariant 24: bg rows stay running)", after.Status, statusRunning)
	}
}

func TestCodexSubagentModelYieldBackgroundsRunningSpawn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	spawnMeta := buildSpawnAgentMeta(t, "child-yield", "running")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "spawn-yield",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "spawn-yield",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn complete: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1",
		Content: "I'll continue while that agent works", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}

	after, ok, err := st.GetThreadItem("t1", "spawn-yield")
	if err != nil || !ok {
		t.Fatalf("lookup spawn row: ok=%v err=%v", ok, err)
	}
	if !after.IsBackground {
		t.Fatal("spawn_agent with running child was not backgrounded on model yield")
	}
	if after.Status != statusRunning {
		t.Fatalf("spawn status=%q, want running", after.Status)
	}
}

func TestCodexSubagentCloseAgentDoesNotBecomeBackgroundedSpawn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	meta, err := json.Marshal(map[string]any{
		"item_status": "completed",
		"toolName":    "collab_agent",
		"input": map[string]any{
			"tool":              "close_agent",
			"receiverThreadIds": []string{"child-1"},
			"agentsStates": map[string]any{
				"child-1": map[string]any{"status": "running"},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal close_agent meta: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "close-1",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: meta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("close start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "close-1",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: meta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("close complete: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1", TurnID: "turn-0",
		Meta: json.RawMessage(`{"turn_status":"completed"}`), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}

	row, ok, err := st.GetThreadItem("t1", "close-1")
	if err != nil || !ok {
		t.Fatalf("lookup close row: ok=%v err=%v", ok, err)
	}
	if row.IsBackground {
		t.Fatal("close_agent was treated as a backgroundable spawn_agent")
	}
	if siblings := findItemsByKind(t, st, "t1", itemKindBackgroundDone); len(siblings) != 0 {
		t.Fatalf("close_agent produced background sibling rows: %+v", siblings)
	}
}

// TestCodexSubagentCompletion_SynthesizesSiblingAtTail covers the
// detached-child closure: a <subagent_notification> arrives for a
// running child → projector synthesizes a tool_completion sibling for
// the parent's spawn_agent row at the current turn's tail.
func TestCodexSubagentCompletion_SynthesizesSiblingAtTail(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnID: "turn-0",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 start: %v", err)
	}
	spawnMeta := buildSpawnAgentMeta(t, "child-abc", "running")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "spawn-abc",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "spawn-abc",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn complete: %v", err)
	}
	turnCompleteMeta, _ := json.Marshal(map[string]any{"turn_status": "completed"})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1", TurnID: "turn-0",
		Meta: turnCompleteMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 complete: %v", err)
	}

	// Advance to turn 1 — the child notification arrives during the
	// next user turn, so the sibling should land on turn 1, not 0.
	// TurnIndex stamped explicitly since no item was persisted in
	// turn 0 (LastTurnIndex reads from items.turn_index).
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnID: "turn-1",
		TurnIndex: 1, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 1 start: %v", err)
	}

	notifyMeta, _ := json.Marshal(map[string]any{
		"agent_path": "child-abc",
		"status":     "completed",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventSubagentNotification, ThreadID: "t1",
		Meta: notifyMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("subagent notification: %v", err)
	}

	siblings := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(siblings) != 1 {
		t.Fatalf("expected 1 subagent sibling, got %d", len(siblings))
	}
	sibling := siblings[0]
	if sibling.CompletionOf != "spawn-abc" {
		t.Errorf("sibling completion_of=%q, want spawn-abc", sibling.CompletionOf)
	}
	if sibling.TurnIndex != 1 {
		t.Errorf("sibling turn_index=%d, want 1 (tail of timeline)", sibling.TurnIndex)
	}
	if !sibling.IsBackground {
		t.Error("subagent sibling is_background=false")
	}
}

func TestCodexSubagentCompletion_UsesMappedLaunchIDForNamedAgentPath(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnID: "turn-0",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 start: %v", err)
	}
	spawnMeta := buildSpawnAgentMeta(t, "child-thread-1", "running")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "spawn-named",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "spawn-named",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn complete: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1", TurnID: "turn-0",
		Meta: json.RawMessage(`{"turn_status":"completed"}`), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 complete: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnID: "turn-1",
		TurnIndex: 1, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 1 start: %v", err)
	}

	notifyMeta, _ := json.Marshal(map[string]any{
		"agent_path": "/root/researcher",
		"status":     "completed",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventSubagentNotification, ThreadID: "t1",
		ItemID: "spawn-named", Meta: notifyMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("subagent notification: %v", err)
	}

	siblings := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(siblings) != 1 {
		t.Fatalf("expected 1 subagent sibling, got %d", len(siblings))
	}
	if siblings[0].CompletionOf != "spawn-named" {
		t.Fatalf("completion_of=%q, want spawn-named", siblings[0].CompletionOf)
	}
}

// TestCodexSubagentCompletion_WaitResolvesBackgroundedSpawn covers the
// wait-tool closure path: when the parent uses `wait` to block on
// children, the wait's item/completed carries agentsStates with each
// awaited child's terminal state. Any already-backgrounded spawn_agent
// whose receivers are now terminal gets a sibling completion row.
// Mirrors TestCodexSubagentCompletion_SynthesizesSiblingAtTail but via
// the explicit wait signal rather than the detached-notification path.
func TestCodexSubagentCompletion_WaitResolvesBackgroundedSpawn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnID: "turn-0",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 start: %v", err)
	}
	spawnMeta := buildSpawnAgentMeta(t, "child-W", "running")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "spawn-W",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "spawn-W",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn complete: %v", err)
	}
	turnCompleteMeta, _ := json.Marshal(map[string]any{"turn_status": "completed"})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1", TurnID: "turn-0",
		Meta: turnCompleteMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 complete: %v", err)
	}

	// Turn 1: parent calls wait and the wait completes with the child
	// in a terminal state. The wait's agentsStates reports the closure.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnID: "turn-1",
		TurnIndex: 1, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 1 start: %v", err)
	}
	waitStartMeta, _ := json.Marshal(map[string]any{
		"toolName":    "wait_agent",
		"item_status": "inProgress",
		"input": map[string]any{
			"tool":              "wait_agent",
			"receiverThreadIds": []string{"child-W"},
		},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "wait-1",
		ItemType: "wait_agent", TurnID: "turn-1", Meta: waitStartMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("wait start: %v", err)
	}
	waitCompleteMeta, _ := json.Marshal(map[string]any{
		"toolName":    "wait_agent",
		"item_status": "completed",
		"input": map[string]any{
			"tool":              "wait_agent",
			"receiverThreadIds": []string{"child-W"},
			"agentsStates": map[string]any{
				"child-W": map[string]any{"status": "completed"},
			},
		},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "wait-1",
		ItemType: "wait_agent", TurnID: "turn-1", Meta: waitCompleteMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("wait complete: %v", err)
	}

	siblings := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(siblings) != 1 {
		t.Fatalf("expected 1 sibling after wait closed the child, got %d", len(siblings))
	}
	if siblings[0].CompletionOf != "spawn-W" {
		t.Errorf("sibling completion_of = %q, want spawn-W", siblings[0].CompletionOf)
	}
	if siblings[0].TurnIndex != 1 {
		t.Errorf("sibling turn_index = %d, want 1", siblings[0].TurnIndex)
	}
}

// TestCodexBackgroundProjector_CleanupThreadDropsState guards the
// session-teardown invariant: CleanupThread must drop all projector
// state so a re-opened session doesn't inherit stale trackers. A
// regression here would cause the first yield in the new session to
// stamp a row that belongs to a process that died with the old
// session.
func TestCodexBackgroundProjector_CleanupThreadDropsState(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startMeta := buildUnifiedExecStartMeta(t, "pid-cleanup")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-cleanup",
		ItemType: "commandExecution", TurnID: "turn-0", Meta: startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}

	router.mu.Lock()
	trackerPre := router.codexBackground["t1"]
	router.mu.Unlock()
	if trackerPre == nil || len(trackerPre.unifiedExec) == 0 {
		t.Fatal("expected projector to track the unifiedExec item after handleToolStart")
	}

	router.CleanupThread("t1")

	router.mu.Lock()
	trackerPost, has := router.codexBackground["t1"]
	router.mu.Unlock()
	if has {
		t.Errorf("codexBackground[t1] still present after CleanupThread: %+v", trackerPost)
	}
}

// TestCodexBackgroundProjector_ReplayDoesNotDoubleStamp pins the
// reconnect replay path: when Codex re-emits item/started for an
// already-tracked item, the projector must not reset the backgrounded
// flag or overwrite the launching-turn correlation. The row's
// is_background=true state (if any) must survive the replay.
func TestCodexBackgroundProjector_ReplayDoesNotDoubleStamp(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startMeta := buildUnifiedExecStartMeta(t, "pid-replay")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-replay",
		ItemType: "commandExecution", TurnID: "turn-0", Meta: startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start 1: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1",
		Content: "yielding", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}

	// Reconnect replays item/started. The projector must see the item
	// as already-tracked and NOT reset its state. The launch row will
	// re-upsert (summary / toolName refresh), but is_background must
	// survive the replay.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-replay",
		ItemType: "commandExecution", TurnID: "turn-0", Meta: startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start 2 (replay): %v", err)
	}

	launch, _, _ := st.GetThreadItem("t1", "cmd-replay")
	if !launch.IsBackground {
		t.Fatal("replayed start reset is_background — projector state was clobbered")
	}
	// Snapshot after the replay settled — the persistToolCallLaunch
	// replay does re-upsert and emit an item_upsert, but the PROJECTOR
	// itself must not produce a fresh stamp emission on the second
	// yield because the tracker already reports backgrounded.
	stampEmissionsAfterReplay := countStampEmissions(*emissions, "cmd-replay")

	// Another yield: because the tracker already reports backgrounded,
	// no additional stamp emission should fire for cmd-replay.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1",
		Content: "more", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta 2: %v", err)
	}

	stampEmissionsAfterSecondYield := countStampEmissions(*emissions, "cmd-replay")
	extra := stampEmissionsAfterSecondYield - stampEmissionsAfterReplay
	if extra != 0 {
		t.Errorf("second yield produced %d additional stamp emissions (want 0 — idempotent)", extra)
	}
}

// countStampEmissions tallies provider:item_upsert emissions for the
// given id whose IsBackground field is true. Used to verify the
// projector emits exactly once per stamp (no thrash on replay).
func countStampEmissions(emissions []emitted, id string) int {
	count := 0
	for _, e := range emissions {
		if e.eventName != "provider:item_upsert" {
			continue
		}
		item, ok := e.data.(store.Item)
		if !ok {
			continue
		}
		if item.ID == id && item.IsBackground {
			count++
		}
	}
	return count
}

// TestCodexBackgroundCompletion_SummaryReflectsOutcome pins the summary
// format: a backgrounded command that completed should produce
// "<launch summary> -> done" on the sibling row; a failed one should
// produce "<launch summary> -> failed". This keeps the tray readable
// when the launch row and sibling both surface.
func TestCodexBackgroundCompletion_SummaryReflectsOutcome(t *testing.T) {
	tests := []struct {
		name       string
		itemStatus string
		want       string
	}{
		{"completed", "completed", "-> done"},
		{"failed", "failed", "-> failed"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			router, st, _ := newTestRouter(t)
			createTestThread(t, st, "t1")
			seedOpenTurn(t, router, st, "t1", 0)

			startMeta := buildUnifiedExecStartMeta(t, "pid-outcome")
			if err := router.Handle(provider.ProviderEvent{
				Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-outcome",
				ItemType: "commandExecution", TurnID: "turn-0", Meta: startMeta,
				Timestamp: time.Now(),
			}); err != nil {
				t.Fatalf("tool start: %v", err)
			}
			if err := router.Handle(provider.ProviderEvent{
				Kind: provider.EventTextDelta, ThreadID: "t1",
				Content: "running", Timestamp: time.Now(),
			}); err != nil {
				t.Fatalf("text delta: %v", err)
			}
			if err := router.Handle(provider.ProviderEvent{
				Kind: provider.EventContentBlockStop, ThreadID: "t1",
				Meta: json.RawMessage(`{"blockType":"text"}`), Timestamp: time.Now(),
			}); err != nil {
				t.Fatalf("content block stop: %v", err)
			}

			doneMeta := buildItemCompletedMeta(t, tc.itemStatus, "unifiedExecStartup")
			if err := router.Handle(provider.ProviderEvent{
				Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "cmd-outcome",
				ItemType: "commandExecution", TurnID: "turn-0", Meta: doneMeta,
				Timestamp: time.Now(),
			}); err != nil {
				t.Fatalf("tool complete: %v", err)
			}

			siblings := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
			if len(siblings) != 1 {
				t.Fatalf("expected 1 sibling, got %d", len(siblings))
			}
			if !strings.Contains(siblings[0].Summary, tc.want) {
				t.Errorf("sibling summary %q does not contain %q", siblings[0].Summary, tc.want)
			}
		})
	}
}
