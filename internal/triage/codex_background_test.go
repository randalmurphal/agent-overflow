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

func buildUnifiedExecStartMeta(t *testing.T, processID, command string) json.RawMessage {
	t.Helper()
	meta := map[string]any{
		"source":      "unifiedExecStartup",
		"item_status": "inProgress",
		"process_id":  processID,
		"toolName":    "Bash",
		"command":     command,
		"input": map[string]any{
			"command": command,
		},
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal unified-exec start meta: %v", err)
	}
	return encoded
}

func buildUnifiedExecCompleteMeta(t *testing.T, status, processID, command string, exitCode int) json.RawMessage {
	t.Helper()
	meta := map[string]any{
		"source":      "unifiedExecStartup",
		"item_status": status,
		"process_id":  processID,
		"toolName":    "Bash",
		"command":     command,
		"exitCode":    exitCode,
		"exit_code":   exitCode,
		"input": map[string]any{
			"command": command,
		},
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal unified-exec complete meta: %v", err)
	}
	return encoded
}

func seedOpenTurn(t *testing.T, router *Router, st *store.Store, threadID string, turnIndex int) {
	t.Helper()
	router.setOpenTurn(threadID, turnIndex)
	if err := st.InsertTurn(store.Turn{
		TurnID:    threadID + ":" + strconv.Itoa(turnIndex),
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("insert turn row: %v", err)
	}
}

func TestCodexUnifiedExecStartIsTrayOnlyAfterYield(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-live",
		ItemType: "commandExecution", TurnID: "turn-0",
		Meta:      buildUnifiedExecStartMeta(t, "pid-live", "sleep 15"),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}

	if _, found, err := st.GetThreadItem("t1", "cmd-live"); err != nil || found {
		t.Fatalf("unified exec start should not persist timeline row: found=%v err=%v", found, err)
	}
	live := router.ListLiveCodexBackgroundTasks("t1", time.Now().UnixMilli(), 0)
	if len(live) != 0 {
		t.Fatalf("pre-yield tray item count = %d, want 0", len(live))
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", Content: "continuing",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}
	live = router.ListLiveCodexBackgroundTasks("t1", time.Now().UnixMilli(), 0)
	if len(live) != 1 {
		t.Fatalf("live tray item count = %d, want 1", len(live))
	}
	if live[0].ID != "cmd-live" || !live[0].IsBackground || live[0].Status != statusRunning {
		t.Fatalf("unexpected live item: %+v", live[0])
	}
	if countEvents(*emissions, "provider:background_tasks_changed") == 0 {
		t.Fatal("start did not emit provider:background_tasks_changed")
	}
}

func TestCodexUnifiedExecQuickCompletionPersistsNormalCommand(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startMeta := buildUnifiedExecStartMeta(t, "pid-quick", "echo ok")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-quick",
		ItemType: "commandExecution", TurnID: "turn-0", Meta: startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventCommandOutput, ThreadID: "t1", ItemID: "cmd-quick",
		ItemType: "commandExecution", TurnID: "turn-0", Content: "ok\n",
		Meta:    buildUnifiedExecCompleteMeta(t, "completed", "pid-quick", "echo ok", 0),
		Replace: true, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("command output: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "cmd-quick",
		ItemType: "commandExecution", TurnID: "turn-0",
		Meta:      buildUnifiedExecCompleteMeta(t, "completed", "pid-quick", "echo ok", 0),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool complete: %v", err)
	}

	row, found, err := st.GetThreadItem("t1", "cmd-quick")
	if err != nil || !found {
		t.Fatalf("quick command row missing: found=%v err=%v", found, err)
	}
	if row.IsBackground {
		t.Fatal("quick command was marked backgrounded")
	}
	if row.Status != statusCompleted || row.PayloadKind != "command_output" {
		t.Fatalf("quick row status/payload = %q/%q", row.Status, row.PayloadKind)
	}
	data, err := st.GetPayloadData(row.PayloadID)
	if err != nil {
		t.Fatalf("payload data: %v", err)
	}
	if string(data) != "ok\n" {
		t.Fatalf("payload = %q, want ok newline", string(data))
	}
	if countEvents(*emissions, codexBackgroundTasksChangedEventName) < 2 {
		t.Fatal("quick completion did not emit tray refresh for live tracker removal")
	}
}

func TestCodexLiveCommandOutputIsBounded(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-bg",
		ItemType: "commandExecution", TurnID: "turn-0",
		Meta:      buildUnifiedExecStartMeta(t, "pid-bg", "yes"),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", Content: "continuing",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}

	oversized := strings.Repeat("a", codexLiveCommandOutputMaxBytes+128)
	completeMeta := buildUnifiedExecCompleteMeta(t, "completed", "pid-bg", "yes", 0)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventCommandOutput, ThreadID: "t1", ItemID: "cmd-bg",
		ItemType: "commandExecution", TurnID: "turn-0", Content: oversized,
		Meta: completeMeta, Replace: true, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("command output: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "cmd-bg",
		ItemType: "commandExecution", TurnID: "turn-0", Meta: completeMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool complete: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTerminalInteraction, ThreadID: "t1",
		Meta:      json.RawMessage(`{"process_id":"pid-bg","stdin":""}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("terminal interaction: %v", err)
	}

	waits := findItemsByKind(t, st, "t1", string(provider.ItemTerminalInteraction))
	if len(waits) != 1 {
		t.Fatalf("wait rows = %d, want 1", len(waits))
	}
	data, err := st.GetPayloadData(waits[0].PayloadID)
	if err != nil {
		t.Fatalf("payload data: %v", err)
	}
	if len(data) != codexLiveCommandOutputMaxBytes {
		t.Fatalf("payload size = %d, want cap %d", len(data), codexLiveCommandOutputMaxBytes)
	}
}

func TestCodexUnifiedExecBackgroundCompletionStaysOutOfTimeline(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-bg",
		ItemType: "commandExecution", TurnID: "turn-0",
		Meta:      buildUnifiedExecStartMeta(t, "pid-bg", "sleep 15"),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", Content: "continuing",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventCommandOutput, ThreadID: "t1", ItemID: "cmd-bg",
		ItemType: "commandExecution", TurnID: "turn-0", Content: "done\n",
		Meta:    buildUnifiedExecCompleteMeta(t, "completed", "pid-bg", "sleep 15", 0),
		Replace: true, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("command output: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "cmd-bg",
		ItemType: "commandExecution", TurnID: "turn-0",
		Meta:      buildUnifiedExecCompleteMeta(t, "completed", "pid-bg", "sleep 15", 0),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool complete: %v", err)
	}

	if _, found, err := st.GetThreadItem("t1", "cmd-bg"); err != nil || found {
		t.Fatalf("background completion should not create timeline command row: found=%v err=%v", found, err)
	}
	if siblings := findItemsByKind(t, st, "t1", itemKindBackgroundDone); len(siblings) != 0 {
		t.Fatalf("background completion created transcript siblings: %+v", siblings)
	}
	live := router.ListLiveCodexBackgroundTasks("t1", time.Now().UnixMilli(), 0)
	if len(live) != 2 {
		t.Fatalf("live tray items = %d, want launch+completion", len(live))
	}
	if live[1].CompletionOf != "cmd-bg" || live[1].Status != statusCompleted {
		t.Fatalf("unexpected completion tray item: %+v", live[1])
	}
}

func TestCodexTerminalInteractionAttachesCompletedOutputAndClearsTray(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-bg",
		ItemType: "commandExecution", TurnID: "turn-0",
		Meta:      buildUnifiedExecStartMeta(t, "pid-bg", "sleep 1; echo done"),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", Content: "continuing",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}
	completeMeta := buildUnifiedExecCompleteMeta(t, "completed", "pid-bg", "sleep 1; echo done", 0)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventCommandOutput, ThreadID: "t1", ItemID: "cmd-bg",
		ItemType: "commandExecution", TurnID: "turn-0", Content: "done\n",
		Meta: completeMeta, Replace: true, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("command output: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "cmd-bg",
		ItemType: "commandExecution", TurnID: "turn-0", Meta: completeMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool complete: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTerminalInteraction, ThreadID: "t1",
		Meta:      json.RawMessage(`{"process_id":"pid-bg","stdin":""}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("terminal interaction: %v", err)
	}

	waits := findItemsByKind(t, st, "t1", string(provider.ItemTerminalInteraction))
	if len(waits) != 1 {
		t.Fatalf("wait rows = %d, want 1", len(waits))
	}
	wait := waits[0]
	if wait.PayloadKind != "command_output" {
		t.Fatalf("wait payload kind = %q, want command_output", wait.PayloadKind)
	}
	data, err := st.GetPayloadData(wait.PayloadID)
	if err != nil {
		t.Fatalf("wait payload data: %v", err)
	}
	if string(data) != "done\n" {
		t.Fatalf("wait payload = %q, want done newline", string(data))
	}
	live := router.ListLiveCodexBackgroundTasks("t1", time.Now().UnixMilli(), 0)
	if len(live) != 0 {
		t.Fatalf("tray should clear after explicit wait attaches output, got %+v", live)
	}
}

func TestCodexTerminalInteractionWhileRunningAttachesCompletionBeforeNextText(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-bg",
		ItemType: "commandExecution", TurnID: "turn-0",
		Meta:      buildUnifiedExecStartMeta(t, "pid-bg", "sleep 10"),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", Content: "continuing",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTerminalInteraction, ThreadID: "t1",
		Meta:      json.RawMessage(`{"process_id":"pid-bg","stdin":""}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("terminal interaction: %v", err)
	}
	completeMeta := buildUnifiedExecCompleteMeta(t, "completed", "pid-bg", "sleep 10", 0)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventCommandOutput, ThreadID: "t1", ItemID: "cmd-bg",
		ItemType: "commandExecution", TurnID: "turn-0", Content: "late\n",
		Meta: completeMeta, Replace: true, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("command output: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "cmd-bg",
		ItemType: "commandExecution", TurnID: "turn-0", Meta: completeMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool complete: %v", err)
	}

	waits := findItemsByKind(t, st, "t1", string(provider.ItemTerminalInteraction))
	if len(waits) != 1 {
		t.Fatalf("wait rows = %d, want 1", len(waits))
	}
	if waits[0].PayloadID == "" {
		t.Fatalf("wait row missing command payload")
	}
	if waits[0].PayloadKind != "command_output" {
		t.Fatalf("wait payload kind = %q, want command_output", waits[0].PayloadKind)
	}
	data, err := st.GetPayloadData(waits[0].PayloadID)
	if err != nil {
		t.Fatalf("wait payload data: %v", err)
	}
	if string(data) != "late\n" {
		t.Fatalf("wait payload = %q, want late newline", string(data))
	}
	live := router.ListLiveCodexBackgroundTasks("t1", time.Now().UnixMilli(), 0)
	if len(live) != 0 {
		t.Fatalf("tray should clear after pending wait attaches output, got %+v", live)
	}
}

func TestCodexTerminalInteractionDoesNotAttachAfterModelMovesOn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-bg",
		ItemType: "commandExecution", TurnID: "turn-0",
		Meta:      buildUnifiedExecStartMeta(t, "pid-bg", "sleep 10"),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", Content: "continuing",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTerminalInteraction, ThreadID: "t1",
		Meta:      json.RawMessage(`{"process_id":"pid-bg","stdin":""}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("terminal interaction: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", Content: "moved on",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("post-wait text delta: %v", err)
	}
	completeMeta := buildUnifiedExecCompleteMeta(t, "completed", "pid-bg", "sleep 10", 0)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventCommandOutput, ThreadID: "t1", ItemID: "cmd-bg",
		ItemType: "commandExecution", TurnID: "turn-0", Content: "late\n",
		Meta: completeMeta, Replace: true, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("command output: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "cmd-bg",
		ItemType: "commandExecution", TurnID: "turn-0", Meta: completeMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool complete: %v", err)
	}

	waits := findItemsByKind(t, st, "t1", string(provider.ItemTerminalInteraction))
	if len(waits) != 1 {
		t.Fatalf("wait rows = %d, want 1", len(waits))
	}
	if waits[0].PayloadID != "" {
		t.Fatalf("stale wait row was mutated with payload %q", waits[0].PayloadID)
	}
}

func TestCodexTerminalInteractionAttachesWhenProcessIDArrivesOnCompletion(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-bg",
		ItemType: "commandExecution", TurnID: "turn-0",
		Meta:      buildUnifiedExecStartMeta(t, "", "sleep 10"),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", Content: "continuing",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTerminalInteraction, ThreadID: "t1",
		Meta:      json.RawMessage(`{"process_id":"pid-late","stdin":""}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("terminal interaction: %v", err)
	}
	completeMeta := buildUnifiedExecCompleteMeta(t, "completed", "pid-late", "sleep 10", 0)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventCommandOutput, ThreadID: "t1", ItemID: "cmd-bg",
		ItemType: "commandExecution", TurnID: "turn-0", Content: "late pid\n",
		Meta: completeMeta, Replace: true, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("command output: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "cmd-bg",
		ItemType: "commandExecution", TurnID: "turn-0", Meta: completeMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool complete: %v", err)
	}

	waits := findItemsByKind(t, st, "t1", string(provider.ItemTerminalInteraction))
	if len(waits) != 1 {
		t.Fatalf("wait rows = %d, want 1", len(waits))
	}
	if waits[0].PayloadKind != "command_output" {
		t.Fatalf("wait payload kind = %q, want command_output", waits[0].PayloadKind)
	}
	data, err := st.GetPayloadData(waits[0].PayloadID)
	if err != nil {
		t.Fatalf("wait payload data: %v", err)
	}
	if string(data) != "late pid\n" {
		t.Fatalf("wait payload = %q, want late pid newline", string(data))
	}
}

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

func TestCodexSubagentRunningChildBackgroundsImmediately(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	spawnMeta := buildSpawnAgentMeta(t, "child-1", "running")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "spawn-1",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "spawn-1",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn complete: %v", err)
	}

	row, found, err := st.GetThreadItem("t1", "spawn-1")
	if err != nil || !found {
		t.Fatalf("spawn row missing: found=%v err=%v", found, err)
	}
	if !row.IsBackground || row.Status != statusRunning {
		t.Fatalf("spawn row did not become live background immediately: %+v", row)
	}
}

func TestCodexSubagentCompletionSignalsCreateTranscriptSibling(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

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
	if siblings[0].CompletionOf != "spawn-abc" {
		t.Fatalf("completion_of=%q, want spawn-abc", siblings[0].CompletionOf)
	}
}

func TestCodexBackgroundProjectorCleanupDropsLiveState(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-cleanup",
		ItemType: "commandExecution", TurnID: "turn-0",
		Meta:      buildUnifiedExecStartMeta(t, "pid-cleanup", "sleep 60"),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", Content: "continuing",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}
	if len(router.ListLiveCodexBackgroundTasks("t1", time.Now().UnixMilli(), 0)) != 1 {
		t.Fatal("expected live tracker before cleanup")
	}

	router.CleanupThread("t1")
	if len(router.ListLiveCodexBackgroundTasks("t1", time.Now().UnixMilli(), 0)) != 0 {
		t.Fatal("live tracker survived cleanup")
	}
}

func countEvents(emissions []emitted, name string) int {
	count := 0
	for _, e := range emissions {
		if e.eventName == name {
			count++
		}
	}
	return count
}
