package triage

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

func createCodexBackgroundTestThread(t *testing.T, st *store.Store, id string) {
	t.Helper()
	createTestThread(t, st, id)
	if err := st.UpdateProvider(id, "codex"); err != nil {
		t.Fatalf("set provider: %v", err)
	}
}

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

func TestCodexUnifiedExecStartIsVisibleBeforeYield(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
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
	if len(live) != 1 {
		t.Fatalf("pre-yield tray item count = %d, want 1", len(live))
	}
	if live[0].ID != "cmd-live" || live[0].IsBackground || live[0].Status != statusRunning {
		t.Fatalf("unexpected pre-yield running item: %+v", live[0])
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
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startMeta := buildUnifiedExecStartMeta(t, "pid-quick", "echo ok")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-quick",
		ItemType: "commandExecution", TurnID: "turn-0", Meta: startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}
	live := router.ListLiveCodexBackgroundTasks("t1", time.Now().UnixMilli(), 0)
	if len(live) != 1 || live[0].ID != "cmd-quick" || live[0].IsBackground {
		t.Fatalf("quick command should be visible as pending running item before completion: %+v", live)
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
	live = router.ListLiveCodexBackgroundTasks("t1", time.Now().UnixMilli(), 0)
	if len(live) != 0 {
		t.Fatalf("quick command should leave running tray after completion: %+v", live)
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
	createCodexBackgroundTestThread(t, st, "t1")
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

	completion, found, err := st.GetThreadItem("t1", nextToolCompletionID("cmd-bg"))
	if err != nil || !found {
		t.Fatalf("command completion missing: found=%v err=%v", found, err)
	}
	data, err := st.GetPayloadData(completion.PayloadID)
	if err != nil {
		t.Fatalf("payload data: %v", err)
	}
	if len(data) != codexLiveCommandOutputMaxBytes {
		t.Fatalf("payload size = %d, want cap %d", len(data), codexLiveCommandOutputMaxBytes)
	}
}

func TestCodexUnifiedExecBackgroundCompletionStaysOutOfTimeline(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
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
	createCodexBackgroundTestThread(t, st, "t1")
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
		t.Fatalf("completion wait should keep one carrier row, got %+v", waits)
	}
	if waits[0].Status != statusCompleted {
		t.Fatalf("wait carrier status = %q, want completed", waits[0].Status)
	}
	completion, found, err := st.GetThreadItem("t1", nextToolCompletionID("cmd-bg"))
	if err != nil || !found {
		t.Fatalf("command completion missing: found=%v err=%v", found, err)
	}
	completionMeta := decodeItemMetaMap(t, completion.Meta)
	if completionMeta["wait_carrier_id"] != waits[0].ID {
		t.Fatalf("completion wait_carrier_id = %v, want %s", completionMeta["wait_carrier_id"], waits[0].ID)
	}
	if completion.PayloadKind != "command_output" {
		t.Fatalf("completion payload kind = %q, want command_output", completion.PayloadKind)
	}
	data, err := st.GetPayloadData(completion.PayloadID)
	if err != nil {
		t.Fatalf("completion payload data: %v", err)
	}
	if string(data) != "done\n" {
		t.Fatalf("completion payload = %q, want done newline", string(data))
	}
	live := router.ListLiveCodexBackgroundTasks("t1", time.Now().UnixMilli(), 0)
	if len(live) != 0 {
		t.Fatalf("tray should clear after explicit wait attaches output, got %+v", live)
	}
}

func TestCodexTerminalInteractionWhileRunningAttachesCompletionBeforeNextText(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
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
	if waits[0].PayloadID != "" {
		t.Fatalf("wait row should stay a marker; got payload %q", waits[0].PayloadID)
	}
	if waits[0].Status != statusCompleted {
		t.Fatalf("wait carrier status = %q, want completed", waits[0].Status)
	}
	completion, found, err := st.GetThreadItem("t1", nextToolCompletionID("cmd-bg"))
	if err != nil || !found {
		t.Fatalf("command completion missing: found=%v err=%v", found, err)
	}
	completionMeta := decodeItemMetaMap(t, completion.Meta)
	if completionMeta["wait_carrier_id"] != waits[0].ID {
		t.Fatalf("completion wait_carrier_id = %v, want %s", completionMeta["wait_carrier_id"], waits[0].ID)
	}
	if completion.PayloadKind != "command_output" {
		t.Fatalf("completion payload kind = %q, want command_output", completion.PayloadKind)
	}
	data, err := st.GetPayloadData(completion.PayloadID)
	if err != nil {
		t.Fatalf("completion payload data: %v", err)
	}
	if string(data) != "late\n" {
		t.Fatalf("completion payload = %q, want late newline", string(data))
	}
	live := router.ListLiveCodexBackgroundTasks("t1", time.Now().UnixMilli(), 0)
	if len(live) != 0 {
		t.Fatalf("tray should clear after pending wait attaches output, got %+v", live)
	}
}

func TestCodexTerminalInteractionDoesNotAttachAfterModelMovesOn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
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
	if waits[0].Status != statusCompleted {
		t.Fatalf("stale wait status = %q, want completed", waits[0].Status)
	}
}

func TestCodexTerminalInteractionTurnCompleteSettlesPendingWait(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
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
	waits := findItemsByKind(t, st, "t1", string(provider.ItemTerminalInteraction))
	if len(waits) != 1 {
		t.Fatalf("wait rows = %d, want 1", len(waits))
	}
	if waits[0].Status != statusRunning {
		t.Fatalf("wait status before turn complete = %q, want running", waits[0].Status)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1", TurnID: "turn-0",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}
	wait, found, err := st.GetThreadItem("t1", waits[0].ID)
	if err != nil || !found {
		t.Fatalf("wait row missing: found=%v err=%v", found, err)
	}
	if wait.Status != statusCompleted {
		t.Fatalf("wait status after turn complete = %q, want completed", wait.Status)
	}
	if siblings := findItemsByKind(t, st, "t1", itemKindBackgroundDone); len(siblings) != 0 {
		t.Fatalf("turn-complete wait settlement should not create completion rows: %+v", siblings)
	}
}

func TestCodexTerminalInteractionDoesNotAttachAfterLaterToolStart(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
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

	webMeta, _ := json.Marshal(map[string]any{
		"toolName": "webSearch",
		"input": map[string]any{
			"query": "background terminal semantics",
		},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "web-1",
		ItemType: "webSearch", TurnID: "turn-0", Meta: webMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("web search start: %v", err)
	}

	completeMeta := buildUnifiedExecCompleteMeta(t, "failed", "pid-bg", "sleep 10", 1)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventCommandOutput, ThreadID: "t1", ItemID: "cmd-bg",
		ItemType: "commandExecution", TurnID: "turn-0", Content: "late failure\n",
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
	if waits[0].Status != statusCompleted {
		t.Fatalf("stale wait status = %q, want completed", waits[0].Status)
	}
	live := router.ListLiveCodexBackgroundTasks("t1", time.Now().UnixMilli(), 0)
	if len(live) != 2 || live[1].CompletionOf != "cmd-bg" || live[1].Status != statusErrored {
		t.Fatalf("completed background output should stay in tray until next wait, got %+v", live)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTerminalInteraction, ThreadID: "t1",
		Meta:      json.RawMessage(`{"process_id":"pid-bg","stdin":""}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("second terminal interaction: %v", err)
	}
	waits = findItemsByKind(t, st, "t1", string(provider.ItemTerminalInteraction))
	if len(waits) != 2 {
		t.Fatalf("wait rows after second poll = %d, want stale marker + completion carrier", len(waits))
	}
	if waits[0].PayloadID != "" || waits[1].PayloadID != "" {
		t.Fatalf("wait rows should stay markers, got %+v", waits)
	}
	completion, found, err := st.GetThreadItem("t1", nextToolCompletionID("cmd-bg"))
	if err != nil || !found {
		t.Fatalf("command completion missing: found=%v err=%v", found, err)
	}
	if completion.PayloadKind != "command_output" {
		t.Fatalf("completion payload kind = %q, want command_output", completion.PayloadKind)
	}
	completionMeta := decodeItemMetaMap(t, completion.Meta)
	if completionMeta["wait_carrier_id"] != waits[1].ID {
		t.Fatalf("completion wait_carrier_id = %v, want %s", completionMeta["wait_carrier_id"], waits[1].ID)
	}
	data, err := st.GetPayloadData(completion.PayloadID)
	if err != nil {
		t.Fatalf("completion payload data: %v", err)
	}
	if string(data) != "late failure\n" {
		t.Fatalf("completion payload = %q, want late failure newline", string(data))
	}
}

func TestCodexTerminalInteractionAttachesWhenProcessIDArrivesOnCompletion(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
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
	if waits[0].PayloadID != "" {
		t.Fatalf("wait row should stay a marker; got payload %q", waits[0].PayloadID)
	}
	completion, found, err := st.GetThreadItem("t1", nextToolCompletionID("cmd-bg"))
	if err != nil || !found {
		t.Fatalf("command completion missing: found=%v err=%v", found, err)
	}
	if completion.PayloadKind != "command_output" {
		t.Fatalf("completion payload kind = %q, want command_output", completion.PayloadKind)
	}
	data, err := st.GetPayloadData(completion.PayloadID)
	if err != nil {
		t.Fatalf("completion payload data: %v", err)
	}
	if string(data) != "late pid\n" {
		t.Fatalf("completion payload = %q, want late pid newline", string(data))
	}
}

func buildSpawnAgentMeta(t *testing.T, childID, childStatus string) json.RawMessage {
	t.Helper()
	return buildSpawnAgentMetaWithMessage(t, childID, childStatus, "")
}

func buildSpawnAgentMetaWithMessage(t *testing.T, childID, childStatus, message string) json.RawMessage {
	t.Helper()
	return buildCollabAgentMetaWithMessage(t, "collab_agent", "spawn_agent", childID, childStatus, message)
}

func buildSpawnAgentMetaForChildren(t *testing.T, childStatuses map[string]string) json.RawMessage {
	t.Helper()
	receiverThreadIDs := make([]string, 0, len(childStatuses))
	agentsStates := make(map[string]any, len(childStatuses))
	for childID, status := range childStatuses {
		receiverThreadIDs = append(receiverThreadIDs, childID)
		agentsStates[childID] = map[string]any{"status": status}
	}
	sort.Strings(receiverThreadIDs)
	m := map[string]any{
		"source":      "",
		"item_status": "completed",
		"toolName":    "collab_agent",
		"input": map[string]any{
			"tool":              "spawn_agent",
			"receiverThreadIds": receiverThreadIDs,
			"agentsStates":      agentsStates,
		},
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal multi-child spawn meta: %v", err)
	}
	return out
}

func buildWaitAgentMetaWithMessage(t *testing.T, childID, childStatus, message string) json.RawMessage {
	t.Helper()
	return buildCollabAgentMetaWithMessage(t, "wait_agent", "wait_agent", childID, childStatus, message)
}

func buildCollabAgentMetaWithMessage(t *testing.T, toolName, tool, childID, childStatus, message string) json.RawMessage {
	t.Helper()
	childState := map[string]any{"status": childStatus}
	if message != "" {
		childState["message"] = message
	}
	m := map[string]any{
		"source":      "",
		"item_status": "completed",
		"toolName":    toolName,
		"input": map[string]any{
			"tool":              tool,
			"receiverThreadIds": []string{childID},
			"agentsStates": map[string]any{
				childID: childState,
			},
		},
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal %s meta: %v", toolName, err)
	}
	return out
}

func TestCodexSubagentRunningChildBackgroundsImmediately(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
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

func TestCodexSpawnStartIsPendingOnly(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	spawnMeta := buildSpawnAgentMeta(t, "child-pending", "running")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "spawn-pending",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn start: %v", err)
	}

	if _, found, err := st.GetThreadItem("t1", "spawn-pending"); err != nil || found {
		t.Fatalf("spawn start should not persist timeline row: found=%v err=%v", found, err)
	}
}

func TestCodexSpawnPendingStartClearsAtTurnComplete(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	spawnMeta := buildSpawnAgentMeta(t, "child-rejected", "running")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "spawn-rejected",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn start: %v", err)
	}
	router.mu.Lock()
	before := len(router.codexBackground["t1"].spawnAgent)
	router.mu.Unlock()
	if before != 1 {
		t.Fatalf("pending spawn trackers before turn complete = %d, want 1", before)
	}

	router.observeCodexTurnComplete("t1")

	router.mu.Lock()
	after := len(router.codexBackground["t1"].spawnAgent)
	router.mu.Unlock()
	if after != 0 {
		t.Fatalf("pending spawn trackers after turn complete = %d, want 0", after)
	}
}

func TestCodexSpawnFailedCompletionWithoutReceiverPersistsErrored(t *testing.T) {
	for _, itemStatus := range []string{"failed", "errored"} {
		t.Run(itemStatus, func(t *testing.T) {
			router, st, _ := newTestRouter(t)
			createCodexBackgroundTestThread(t, st, "t1")
			seedOpenTurn(t, router, st, "t1", 0)

			itemID := "spawn-" + itemStatus
			spawnMeta, _ := json.Marshal(map[string]any{
				"item_status": itemStatus,
				"toolName":    "collab_agent",
				"input": map[string]any{
					"tool":   "spawn_agent",
					"prompt": "try to spawn beyond the agent thread limit",
				},
			})
			if err := router.Handle(provider.ProviderEvent{
				Kind: provider.EventToolStart, ThreadID: "t1", ItemID: itemID,
				ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
				Timestamp: time.Now(),
			}); err != nil {
				t.Fatalf("spawn start: %v", err)
			}
			if err := router.Handle(provider.ProviderEvent{
				Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: itemID,
				ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
				Timestamp: time.Now(),
			}); err != nil {
				t.Fatalf("spawn complete: %v", err)
			}

			row, found, err := st.GetThreadItem("t1", itemID)
			if err != nil || !found {
				t.Fatalf("failed spawn row missing: found=%v err=%v", found, err)
			}
			if row.Status != statusErrored || row.IsBackground {
				t.Fatalf("failed spawn row = %+v, want errored foreground row", row)
			}
			if !strings.Contains(row.Summary, "("+itemStatus+")") {
				t.Fatalf("summary = %q, want %s suffix", row.Summary, itemStatus)
			}
		})
	}
}

func TestCodexSubagentInactiveStatusHidesLiveBackgroundWithoutCompletion(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	spawnMeta := buildSpawnAgentMeta(t, "child-inactive", "running")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "spawn-inactive",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "spawn-inactive",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn complete: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventSubagentStatus, ThreadID: "t1", ItemID: "spawn-inactive",
		Meta:      json.RawMessage(`{"agent_path":"child-inactive","status":"completed"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("subagent inactive status: %v", err)
	}

	live, err := st.ListLiveBackgroundTasks("t1", 0)
	if err != nil {
		t.Fatalf("live background tasks: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("inactive unwaited subagent should not be live background, got %+v", live)
	}
	if _, found, err := st.GetThreadItem("t1", nextToolCompletionID("spawn-inactive")); err != nil || found {
		t.Fatalf("inactive status should not create completion sibling: found=%v err=%v", found, err)
	}
	row, found, err := st.GetThreadItem("t1", "spawn-inactive")
	if err != nil || !found {
		t.Fatalf("spawn row missing: found=%v err=%v", found, err)
	}
	meta := decodeItemMetaMap(t, row.Meta)
	if meta["live_background_active"] != false {
		t.Fatalf("live_background_active = %v, want false", meta["live_background_active"])
	}
}

func TestCodexSubagentStatusWaitsForAllChildrenBeforeHidingLiveBackground(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	spawnMeta := buildSpawnAgentMetaForChildren(t, map[string]string{
		"child-a": "running",
		"child-b": "running",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "spawn-multi-status",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "spawn-multi-status",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn complete: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventSubagentStatus, ThreadID: "t1", ItemID: "spawn-multi-status",
		Meta:      json.RawMessage(`{"agent_path":"child-a","status":"completed"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("first child status: %v", err)
	}
	live, err := st.ListLiveBackgroundTasks("t1", 0)
	if err != nil {
		t.Fatalf("live background tasks after first child: %v", err)
	}
	if len(live) != 1 || live[0].ID != "spawn-multi-status" {
		t.Fatalf("spawn should stay live while child-b runs, got %+v", live)
	}
	row, found, err := st.GetThreadItem("t1", "spawn-multi-status")
	if err != nil || !found {
		t.Fatalf("spawn row missing after first child: found=%v err=%v", found, err)
	}
	meta := decodeItemMetaMap(t, row.Meta)
	if meta["live_background_active"] == false {
		t.Fatalf("live_background_active should not be false until all children finish: %+v", meta)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventSubagentStatus, ThreadID: "t1", ItemID: "spawn-multi-status",
		Meta:      json.RawMessage(`{"agent_path":"child-b","status":"completed"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("second child status: %v", err)
	}
	live, err = st.ListLiveBackgroundTasks("t1", 0)
	if err != nil {
		t.Fatalf("live background tasks after second child: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("spawn should hide after all children finish, got %+v", live)
	}
}

func TestCodexSubagentWaitAfterInactiveStatusCreatesCompletion(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	spawnMeta := buildSpawnAgentMeta(t, "child-inactive-wait", "running")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "spawn-inactive-wait",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "spawn-inactive-wait",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn complete: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventSubagentStatus, ThreadID: "t1", ItemID: "spawn-inactive-wait",
		Meta:      json.RawMessage(`{"agent_path":"child-inactive-wait","status":"completed"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("subagent inactive status: %v", err)
	}

	waitMeta := buildWaitAgentMetaWithMessage(t, "child-inactive-wait", "completed", "Final after wait")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "wait-inactive",
		ItemType: "wait_agent", TurnID: "turn-0", Meta: waitMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("wait start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "wait-inactive",
		ItemType: "wait_agent", TurnID: "turn-0", Content: "Final after wait",
		Meta: waitMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("wait complete: %v", err)
	}

	completion, found, err := st.GetThreadItem("t1", nextToolCompletionID("spawn-inactive-wait"))
	if err != nil || !found {
		t.Fatalf("subagent completion missing after wait: found=%v err=%v", found, err)
	}
	if completion.Summary == "" || completion.CompletionOf != "spawn-inactive-wait" {
		t.Fatalf("unexpected completion: %+v", completion)
	}
}

func TestCodexSubagentWaitCompletionRehydratesPersistedLaunch(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	spawnMeta := buildSpawnAgentMeta(t, "child-rehydrate", "running")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "spawn-rehydrate",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "spawn-rehydrate",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn complete: %v", err)
	}

	router.mu.Lock()
	delete(router.codexBackground, "t1")
	router.mu.Unlock()

	waitMeta := buildWaitAgentMetaWithMessage(t, "child-rehydrate", "completed", "Recovered final output")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "wait-rehydrate",
		ItemType: "wait_agent", TurnID: "turn-0", Meta: waitMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("wait start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "wait-rehydrate",
		ItemType: "wait_agent", TurnID: "turn-0", Content: "Recovered final output",
		Meta: waitMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("wait complete: %v", err)
	}

	completion, found, err := st.GetThreadItem("t1", nextToolCompletionID("spawn-rehydrate"))
	if err != nil || !found {
		t.Fatalf("rehydrated subagent completion missing: found=%v err=%v", found, err)
	}
	if completion.CompletionOf != "spawn-rehydrate" {
		t.Fatalf("completion_of = %q, want spawn-rehydrate", completion.CompletionOf)
	}
}

func TestCodexSubagentCompletionSignalsCreateTranscriptSibling(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
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

func TestCodexSubagentNotificationRehydratesPersistedLaunch(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	spawnMeta := buildSpawnAgentMeta(t, "child-notify-rehydrate", "running")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "spawn-notify-rehydrate",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "spawn-notify-rehydrate",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn complete: %v", err)
	}

	router.mu.Lock()
	delete(router.codexBackground, "t1")
	router.mu.Unlock()

	notifyMeta, _ := json.Marshal(map[string]any{
		"agent_path": "child-notify-rehydrate",
		"status":     "completed",
		"message":    "Recovered notification output",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventSubagentNotification, ThreadID: "t1",
		Meta: notifyMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("subagent notification: %v", err)
	}

	completion, found, err := st.GetThreadItem("t1", nextToolCompletionID("spawn-notify-rehydrate"))
	if err != nil || !found {
		t.Fatalf("rehydrated notification sibling missing: found=%v err=%v", found, err)
	}
	if completion.CompletionOf != "spawn-notify-rehydrate" {
		t.Fatalf("completion_of = %q, want spawn-notify-rehydrate", completion.CompletionOf)
	}
	if completion.Summary == "" {
		t.Fatalf("completion summary should be populated: %+v", completion)
	}
}

func TestCodexSubagentNotificationAfterWaitTimeoutDoesNotAttachToWaitCarrier(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	spawnMeta := buildSpawnAgentMeta(t, "child-provider-1", "running")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "spawn-timeout",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "spawn-timeout",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn complete: %v", err)
	}

	waitMeta := buildWaitAgentMetaWithMessage(t, "child-provider-1", "running", "")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "wait-timeout",
		ItemType: "wait_agent", TurnID: "turn-0", Meta: waitMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("wait start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "wait-timeout",
		ItemType: "wait_agent", TurnID: "turn-0", Meta: waitMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("wait complete: %v", err)
	}

	notifyMeta, _ := json.Marshal(map[string]any{
		"agent_path": "/root/researcher",
		"status":     "completed",
		"message":    "done later",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventSubagentNotification, ThreadID: "t1", ItemID: "spawn-timeout",
		Meta: notifyMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("subagent notification: %v", err)
	}

	completion, found, err := st.GetThreadItem("t1", nextToolCompletionID("spawn-timeout"))
	if err != nil || !found {
		t.Fatalf("subagent sibling missing: found=%v err=%v", found, err)
	}
	meta := decodeItemMetaMap(t, completion.Meta)
	if _, ok := meta["wait_carrier_id"]; ok {
		t.Fatalf("wait_carrier_id = %v, want no stale timeout carrier", meta["wait_carrier_id"])
	}
}

func TestCodexSubagentWaitCompletionCarriesFinalOutputPayload(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	spawnMeta := buildSpawnAgentMeta(t, "child-wait", "running")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "spawn-wait",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "spawn-wait",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn complete: %v", err)
	}

	waitMeta := buildWaitAgentMetaWithMessage(t, "child-wait", "completed", "Final child output")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "wait-child",
		ItemType: "wait_agent", TurnID: "turn-0", Meta: waitMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("wait start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "wait-child",
		ItemType: "wait_agent", TurnID: "turn-0", Content: "Final child output",
		Meta: waitMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("wait complete: %v", err)
	}

	siblings := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	var subagentSibling *store.Item
	for i := range siblings {
		if siblings[i].CompletionOf == "spawn-wait" {
			subagentSibling = &siblings[i]
		}
	}
	if subagentSibling == nil {
		t.Fatalf("expected subagent sibling for spawn-wait, got %+v", siblings)
	}
	if subagentSibling.PayloadKind != payloadKindToolCallResult {
		t.Fatalf("payload kind = %q, want %s", subagentSibling.PayloadKind, payloadKindToolCallResult)
	}
	siblingMeta := decodeItemMetaMap(t, subagentSibling.Meta)
	if siblingMeta["wait_carrier_id"] != "wait-child" {
		t.Fatalf("wait_carrier_id = %v, want wait-child", siblingMeta["wait_carrier_id"])
	}
	waitRow, found, err := st.GetThreadItem("t1", nextToolCompletionID("wait-child"))
	if err != nil || !found {
		t.Fatalf("wait row missing: found=%v err=%v", found, err)
	}
	if subagentSibling.PayloadID != waitRow.PayloadID {
		t.Fatalf("sibling payload id = %q, want shared wait payload %q", subagentSibling.PayloadID, waitRow.PayloadID)
	}
	data, err := st.GetPayloadData(subagentSibling.PayloadID)
	if err != nil {
		t.Fatalf("payload data: %v", err)
	}
	if string(data) != "Final child output" {
		t.Fatalf("payload = %q, want final child output", string(data))
	}
}

func TestCodexSubagentDuplicateBlankCompletionPreservesPayload(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	spawnMeta := buildSpawnAgentMeta(t, "child-blank-dup", "running")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "spawn-blank-dup",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "spawn-blank-dup",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn complete: %v", err)
	}

	waitMeta := buildWaitAgentMetaWithMessage(t, "child-blank-dup", "completed", "Final child output")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "wait-blank-dup",
		ItemType: "wait_agent", TurnID: "turn-0", Meta: waitMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("wait start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "wait-blank-dup",
		ItemType: "wait_agent", TurnID: "turn-0", Content: "Final child output",
		Meta: waitMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("wait complete: %v", err)
	}

	completionID := nextToolCompletionID("spawn-blank-dup")
	before, found, err := st.GetThreadItem("t1", completionID)
	if err != nil || !found {
		t.Fatalf("subagent sibling missing before duplicate: found=%v err=%v", found, err)
	}
	if before.PayloadID == "" {
		t.Fatalf("subagent sibling missing payload before duplicate")
	}
	beforeMeta := decodeItemMetaMap(t, before.Meta)
	if beforeMeta["wait_carrier_id"] != "wait-blank-dup" {
		t.Fatalf("wait_carrier_id before duplicate = %v, want wait-blank-dup", beforeMeta["wait_carrier_id"])
	}

	if err := router.synthesizeCodexBackgroundCompletion(provider.ProviderEvent{
		ThreadID:  "t1",
		ItemID:    "spawn-blank-dup",
		Content:   "\n",
		Meta:      subagentStatusToItemStatusMeta("completed"),
		Timestamp: time.Now(),
	}, "spawn-blank-dup", codexBackgroundCompletionOptions{}); err != nil {
		t.Fatalf("duplicate blank completion: %v", err)
	}

	after, found, err := st.GetThreadItem("t1", completionID)
	if err != nil || !found {
		t.Fatalf("subagent sibling missing after duplicate: found=%v err=%v", found, err)
	}
	if after.PayloadID != before.PayloadID {
		t.Fatalf("payload id changed after duplicate: %q, want %q", after.PayloadID, before.PayloadID)
	}
	afterMeta := decodeItemMetaMap(t, after.Meta)
	if afterMeta["wait_carrier_id"] != "wait-blank-dup" {
		t.Fatalf("wait_carrier_id after duplicate = %v, want wait-blank-dup", afterMeta["wait_carrier_id"])
	}
	data, err := st.GetPayloadData(after.PayloadID)
	if err != nil {
		t.Fatalf("payload data after duplicate: %v", err)
	}
	if string(data) != "Final child output" {
		t.Fatalf("payload after duplicate = %q, want final child output", string(data))
	}
}

func TestCodexSubagentWaitCompletionPersistsImmediatelyWithActiveStream(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	spawnMeta := buildSpawnAgentMeta(t, "child-immediate", "running")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "spawn-immediate",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "spawn-immediate",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn complete: %v", err)
	}

	waitMeta := buildWaitAgentMetaWithMessage(t, "child-immediate", "completed", "Child finished during wait")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "wait-immediate",
		ItemType: "wait_agent", TurnID: "turn-0", Meta: waitMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("wait start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", TurnID: "turn-0",
		Content: "stream still open", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "wait-immediate",
		ItemType: "wait_agent", TurnID: "turn-0", Content: "Child finished during wait",
		Meta: waitMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("wait complete: %v", err)
	}

	subagentSibling, found, err := st.GetThreadItem("t1", nextToolCompletionID("spawn-immediate"))
	if err != nil || !found {
		t.Fatalf("subagent sibling should persist at wait completion even with active stream: found=%v err=%v", found, err)
	}
	siblingMeta := decodeItemMetaMap(t, subagentSibling.Meta)
	if siblingMeta["wait_carrier_id"] != "wait-immediate" {
		t.Fatalf("wait_carrier_id = %v, want wait-immediate", siblingMeta["wait_carrier_id"])
	}
	router.mu.Lock()
	activeStreams := router.streamingItemCounts["t1"]
	router.mu.Unlock()
	if activeStreams == 0 {
		t.Fatal("test setup did not leave an active stream")
	}
}

func TestCodexSubagentWaitCompletionReusesPayloadForOutOfOrderChildren(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	spawnMeta := buildCollabAgentMetaWithMessage(t, "collab_agent", "spawn_agent", "child-b", "running", "")
	var spawn map[string]any
	if err := json.Unmarshal(spawnMeta, &spawn); err != nil {
		t.Fatalf("spawn meta unmarshal: %v", err)
	}
	input := spawn["input"].(map[string]any)
	input["receiverThreadIds"] = []string{"child-b", "child-a"}
	agents := input["agentsStates"].(map[string]any)
	agents["child-a"] = map[string]any{"status": "running"}
	spawnMeta, _ = json.Marshal(spawn)

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "spawn-multi",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "spawn-multi",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn complete: %v", err)
	}

	waitMeta := buildCollabAgentMetaWithMessage(t, "wait_agent", "wait_agent", "child-a", "completed", "A done")
	var wait map[string]any
	if err := json.Unmarshal(waitMeta, &wait); err != nil {
		t.Fatalf("wait meta unmarshal: %v", err)
	}
	waitInput := wait["input"].(map[string]any)
	waitInput["receiverThreadIds"] = []string{"child-a", "child-b"}
	waitInput["agentsStates"] = map[string]any{
		"child-a": map[string]any{"status": "completed", "message": "A done"},
		"child-b": map[string]any{"status": "completed", "message": "B done"},
	}
	waitMeta, _ = json.Marshal(wait)
	content := "Agent 1 (completed):\nA done\n\nAgent 2 (completed):\nB done"

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "wait-multi",
		ItemType: "wait_agent", TurnID: "turn-0", Meta: waitMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("wait start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "wait-multi",
		ItemType: "wait_agent", TurnID: "turn-0", Content: content,
		Meta: waitMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("wait complete: %v", err)
	}

	waitRow, found, err := st.GetThreadItem("t1", nextToolCompletionID("wait-multi"))
	if err != nil || !found {
		t.Fatalf("wait row missing: found=%v err=%v", found, err)
	}
	subagentSibling, found, err := st.GetThreadItem("t1", nextToolCompletionID("spawn-multi"))
	if err != nil || !found {
		t.Fatalf("subagent sibling missing: found=%v err=%v", found, err)
	}
	if subagentSibling.PayloadID != waitRow.PayloadID {
		t.Fatalf("sibling payload id = %q, want shared wait payload %q", subagentSibling.PayloadID, waitRow.PayloadID)
	}
	siblingMeta := decodeItemMetaMap(t, subagentSibling.Meta)
	if siblingMeta["wait_carrier_id"] != "wait-multi" {
		t.Fatalf("wait_carrier_id = %v, want wait-multi", siblingMeta["wait_carrier_id"])
	}
	data, err := st.GetPayloadData(subagentSibling.PayloadID)
	if err != nil {
		t.Fatalf("payload data: %v", err)
	}
	if strings.Contains(string(data), "child-a") || strings.Contains(string(data), "child-b") {
		t.Fatalf("payload leaked child ids: %q", string(data))
	}
	if !strings.Contains(string(data), "Agent 1 (completed)") || !strings.Contains(string(data), "Agent 2 (completed)") {
		t.Fatalf("payload missing ordinal agent headers: %q", string(data))
	}
}

func TestCodexSubagentNotificationCarriesFinalOutputPayload(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	spawnMeta := buildSpawnAgentMeta(t, "child-notify", "running")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "spawn-notify",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "spawn-notify",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn complete: %v", err)
	}

	notifyMeta, _ := json.Marshal(map[string]any{
		"agent_path": "child-notify",
		"status":     "completed",
		"message":    "Done from notification",
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
	if siblings[0].PayloadKind != payloadKindToolCallResult {
		t.Fatalf("payload kind = %q, want %s", siblings[0].PayloadKind, payloadKindToolCallResult)
	}
	data, err := st.GetPayloadData(siblings[0].PayloadID)
	if err != nil {
		t.Fatalf("payload data: %v", err)
	}
	if string(data) != "Done from notification" {
		t.Fatalf("payload = %q, want notification final output", string(data))
	}
}

// TestCodexUnifiedExecBackgroundedOnUserInterrupt pins the truncation
// branch of observeCodexTurnComplete. A pre-yield unifiedExec PTY survives
// `Op::Interrupt` on the Codex side (`core/src/tasks/mod.rs:632-637` —
// `close_unified_exec_processes` is a separate Op `abort_all_tasks`
// doesn't invoke). Our triage must mirror that truth: stamp the tracker
// `backgrounded=true` so the tray's Stop-All button enables and the user
// can fire `thread/backgroundTerminals/clean` against it. Without the
// stamp the tracker stays `backgrounded=false`, the row renders as
// non-stoppable in the tray, and the user has no UI path to kill a
// process Codex says is still running.
func TestCodexUnifiedExecBackgroundedOnUserInterrupt(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	// Start a unifiedExec command — pre-yield (no text/reasoning delta
	// followed it). The tracker is registered with backgrounded=false.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-preyield",
		ItemType: "commandExecution", TurnID: "turn-0",
		Meta:      buildUnifiedExecStartMeta(t, "pid-preyield", "sleep 30"),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}

	// Pre-fix: Stop-All would be disabled because no tasks are
	// `IsBackground=true` yet.
	beforeInterrupt := router.ListLiveCodexBackgroundTasks("t1", time.Now().UnixMilli(), 0)
	if len(beforeInterrupt) != 1 {
		t.Fatalf("live tray = %d, want 1 launch row before interrupt", len(beforeInterrupt))
	}
	if beforeInterrupt[0].IsBackground {
		t.Fatalf("pre-yield tracker should be IsBackground=false before turn-close (no yield signal yet)")
	}

	// User-Esc → backend emits EventTurnComplete normalized from Codex's
	// status=interrupted wire shape.
	if err := router.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "t1",
		TurnID:       "turn-0",
		TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "interrupted", Aborted: true},
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("turn complete truncated: %v", err)
	}

	// Post-fix: the catchall ran on the truncated path, stamping the
	// pre-yield tracker `backgrounded=true`.
	afterInterrupt := router.ListLiveCodexBackgroundTasks("t1", time.Now().UnixMilli(), 0)
	if len(afterInterrupt) != 1 {
		t.Fatalf("live tray = %d, want 1 launch row after interrupt", len(afterInterrupt))
	}
	if !afterInterrupt[0].IsBackground {
		t.Fatalf("user-Esc on pre-yield unifiedExec must stamp IsBackground=true (Codex PTY survives Op::Interrupt; tray needs it stoppable). Got: %+v", afterInterrupt[0])
	}
	if afterInterrupt[0].Status != statusRunning {
		t.Errorf("backgrounded launch status = %q, want running (invariant 24)", afterInterrupt[0].Status)
	}
}

func TestCodexBackgroundProjectorCleanupDropsLiveState(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
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
