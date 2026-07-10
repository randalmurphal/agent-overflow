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

func buildCodexExecResultMeta(t *testing.T, result, processID, command string) json.RawMessage {
	t.Helper()
	meta := map[string]any{
		"result": result,
	}
	if processID != "" {
		meta["process_id"] = processID
	}
	if command != "" {
		meta["command"] = command
		meta["input"] = map[string]any{
			"command": command,
		}
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal codex exec result meta: %v", err)
	}
	return encoded
}

func seedOpenTurn(t *testing.T, router *Router, st *store.Store, threadID string, turnIndex int) {
	t.Helper()
	router.setOpenTurn(threadID, turnIndex)
	startedAt := time.Now().UnixMilli()
	router.setOpenRoundSnapshot(ActiveTurnSnapshot{
		ThreadID:  threadID,
		TurnID:    threadID + ":" + strconv.Itoa(turnIndex) + ":round",
		TurnIndex: turnIndex,
		StartedAt: startedAt,
	})
	if err := st.InsertTurn(store.Turn{
		TurnID:    threadID + ":" + strconv.Itoa(turnIndex),
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		StartedAt: startedAt,
	}); err != nil {
		t.Fatalf("insert turn row: %v", err)
	}
}

func markCodexExecRunning(t *testing.T, router *Router, threadID, turnID, itemID, processID, command string) {
	t.Helper()
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventCodexExecResult, ThreadID: threadID, ItemID: itemID,
		ItemType: "commandExecution", TurnID: turnID,
		Meta:      buildCodexExecResultMeta(t, codexExecResultRunning, processID, command),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("codex exec running result: %v", err)
	}
}

func TestCodexUnifiedExecStartWaitsForTypedTerminalInteraction(t *testing.T) {
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
	meta := decodeItemMetaMap(t, live[0].Meta)
	if meta["command"] != "sleep 15" {
		t.Fatalf("live command meta = %v, want full command", meta["command"])
	}
	if _, ok := meta["process_id"]; ok {
		t.Fatalf("live command meta leaked process_id: %+v", meta)
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
	if live[0].ID != "cmd-live" || live[0].IsBackground || live[0].Status != statusRunning {
		t.Fatalf("text delta should not background unified exec without typed wait: %+v", live[0])
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventThinking, ThreadID: "t1", Content: "thinking",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("thinking: %v", err)
	}
	live = router.ListLiveCodexBackgroundTasks("t1", time.Now().UnixMilli(), 0)
	if len(live) != 1 || live[0].IsBackground {
		t.Fatalf("reasoning should not background unified exec without typed wait: %+v", live)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1", TurnID: "turn-0",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}
	live = router.ListLiveCodexBackgroundTasks("t1", time.Now().UnixMilli(), 0)
	if len(live) != 1 || live[0].IsBackground {
		t.Fatalf("turn complete should not background unified exec without typed wait: %+v", live)
	}
	markCodexExecRunning(t, router, "t1", "turn-0", "cmd-live", "pid-live", "sleep 15")
	live = router.ListLiveCodexBackgroundTasks("t1", time.Now().UnixMilli(), 0)
	if len(live) != 1 || live[0].ID != "cmd-live" || live[0].IsBackground || live[0].Status != statusRunning {
		t.Fatalf("raw running result should not background unified exec: %+v", live)
	}
	seedOpenTurn(t, router, st, "t1", 1)
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTerminalInteraction,
		ThreadID:  "t1",
		TurnID:    "turn-1",
		Meta:      terminalInteractionMetaBlob(t, "pid-live", ""),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("typed terminal interaction: %v", err)
	}
	live = router.ListLiveCodexBackgroundTasks("t1", time.Now().UnixMilli(), 0)
	if len(live) != 1 || live[0].ID != "cmd-live" || !live[0].IsBackground || live[0].Status != statusRunning {
		t.Fatalf("typed terminal interaction should background unified exec: %+v", live)
	}
	if countEvents(emissions.snapshot(), "provider:background_tasks_changed") == 0 {
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
	if countEvents(emissions.snapshot(), codexBackgroundTasksChangedEventName) < 2 {
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
	completion, found, err := st.GetThreadItem("t1", "cmd-bg")
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

func TestCodexUnifiedExecBackgroundCompletionPersistsCommandRow(t *testing.T) {
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
	markCodexExecRunning(t, router, "t1", "turn-0", "cmd-bg", "pid-bg", "sleep 15")
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

	row, found, err := st.GetThreadItem("t1", "cmd-bg")
	if err != nil || !found {
		t.Fatalf("background command row missing: found=%v err=%v", found, err)
	}
	if row.Status != statusCompleted || row.PayloadKind != "command_output" {
		t.Fatalf("background command row status/payload = %+v", row)
	}
	if siblings := findItemsByKind(t, st, "t1", itemKindBackgroundDone); len(siblings) != 0 {
		t.Fatalf("background completion created sibling rows: %+v", siblings)
	}
	live := router.ListLiveCodexBackgroundTasks("t1", time.Now().UnixMilli(), 0)
	if len(live) != 0 {
		t.Fatalf("live tray should clear after typed command completion: %+v", live)
	}
}

func TestCodexUnifiedExecIdleCompletionAfterTurnCompleteClearsTransientStateWithoutHistory(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-late",
		ItemType: "commandExecution", TurnID: "t1:0",
		Meta:      buildUnifiedExecStartMeta(t, "pid-late", "sleep 10 && echo done"),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "t1",
		TurnID:       "t1:0",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}

	completeMeta := buildUnifiedExecCompleteMeta(t, "completed", "pid-late", "sleep 10 && echo done", 0)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventCommandOutput, ThreadID: "t1", ItemID: "cmd-late",
		ItemType: "commandExecution", TurnID: "t1:0", Content: "done\n",
		Meta: completeMeta, Replace: true, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("command output: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "cmd-late",
		ItemType: "commandExecution", TurnID: "t1:0", Meta: completeMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool complete: %v", err)
	}

	if _, found, err := st.GetThreadItem("t1", "cmd-late"); err != nil || found {
		t.Fatalf("idle late completion should not persist history row: found=%v err=%v", found, err)
	}
	if live := router.ListLiveCodexBackgroundTasks("t1", time.Now().UnixMilli(), 0); len(live) != 0 {
		t.Fatalf("idle late completion should clear live tray: %+v", live)
	}
	for _, item := range filterItemEventUpserts(emissions.snapshot()) {
		if item.ID == "cmd-late" {
			t.Fatalf("idle late completion emitted history upsert: %+v", item)
		}
	}
}

func TestCodexUnifiedExecIdleCompletionAfterInterruptedTurnClearsTransientStateWithoutHistory(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-interrupted-late",
		ItemType: "commandExecution", TurnID: "t1:0",
		Meta:      buildUnifiedExecStartMeta(t, "pid-interrupted-late", "sleep 10 && echo done"),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "t1",
		TurnID:       "t1:0",
		TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "interrupted", Aborted: true},
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}

	completeMeta := buildUnifiedExecCompleteMeta(t, "completed", "pid-interrupted-late", "sleep 10 && echo done", 0)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventCommandOutput, ThreadID: "t1", ItemID: "cmd-interrupted-late",
		ItemType: "commandExecution", TurnID: "t1:0", Content: "done\n",
		Meta: completeMeta, Replace: true, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("command output: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "cmd-interrupted-late",
		ItemType: "commandExecution", TurnID: "t1:0", Meta: completeMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool complete: %v", err)
	}

	if _, found, err := st.GetThreadItem("t1", "cmd-interrupted-late"); err != nil || found {
		t.Fatalf("interrupted late completion should not persist history row: found=%v err=%v", found, err)
	}
	if live := router.ListLiveCodexBackgroundTasks("t1", time.Now().UnixMilli(), 0); len(live) != 0 {
		t.Fatalf("interrupted late completion should clear live tray: %+v", live)
	}
	for _, item := range filterItemEventUpserts(emissions.snapshot()) {
		if item.ID == "cmd-interrupted-late" {
			t.Fatalf("interrupted late completion emitted history upsert: %+v", item)
		}
	}
}

func TestCodexUnifiedExecCompletionDuringLaterActiveTurnPersistsAtObservedTurn(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-later-active",
		ItemType: "commandExecution", TurnID: "t1:0",
		Meta:      buildUnifiedExecStartMeta(t, "pid-later-active", "sleep 10 && echo done"),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "t1",
		TurnID:       "t1:0",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}

	seedOpenTurn(t, router, st, "t1", 1)
	completeMeta := buildUnifiedExecCompleteMeta(t, "completed", "pid-later-active", "sleep 10 && echo done", 0)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventCommandOutput, ThreadID: "t1", ItemID: "cmd-later-active",
		ItemType: "commandExecution", TurnID: "t1:0", Content: "done\n",
		Meta: completeMeta, Replace: true, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("command output: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "cmd-later-active",
		ItemType: "commandExecution", TurnID: "t1:0", Meta: completeMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool complete: %v", err)
	}

	row, found, err := st.GetThreadItem("t1", "cmd-later-active")
	if err != nil || !found {
		t.Fatalf("later active completion row missing: found=%v err=%v", found, err)
	}
	if row.TurnIndex != 1 {
		t.Fatalf("later active completion turn index = %d, want observed active turn 1", row.TurnIndex)
	}
	if row.Status != statusCompleted || row.PayloadKind != "command_output" {
		t.Fatalf("later active completion status/payload = %+v", row)
	}
	foundUpsert := false
	for _, item := range filterItemEventUpserts(emissions.snapshot()) {
		if item.ID != "cmd-later-active" {
			continue
		}
		foundUpsert = true
		if item.TurnIndex != 1 || item.Status != statusCompleted || item.PayloadKind != "command_output" {
			t.Fatalf("later active completion upsert = %+v", item)
		}
	}
	if !foundUpsert {
		t.Fatalf("later active completion did not emit provider:item_event upsert; emissions=%+v", emissions.snapshot())
	}
}

func TestCodexUnifiedExecYieldResultBackgroundsBeforeCompletion(t *testing.T) {
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
		Kind: provider.EventCodexExecResult, ThreadID: "t1", ItemID: "cmd-bg",
		ItemType: "commandExecution", TurnID: "turn-0",
		Meta:      buildCodexExecResultMeta(t, codexExecResultRunning, "pid-bg", "sleep 1; echo done"),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("codex exec result: %v", err)
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

	row, found, err := st.GetThreadItem("t1", "cmd-bg")
	if err != nil || !found {
		t.Fatalf("yielded command row missing: found=%v err=%v", found, err)
	}
	if row.Status != statusCompleted || row.PayloadKind != "command_output" {
		t.Fatalf("yielded command row status/payload = %+v", row)
	}
	if siblings := findItemsByKind(t, st, "t1", itemKindBackgroundDone); len(siblings) != 0 {
		t.Fatalf("yielded command created sibling rows: %+v", siblings)
	}
}

func TestCodexUnifiedExecLateRawYieldResultDoesNotCreateDuplicate(t *testing.T) {
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

	row, found, err := st.GetThreadItem("t1", "cmd-bg")
	if err != nil || !found {
		t.Fatalf("typed completion should persist command row immediately: found=%v err=%v", found, err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventCodexExecResult, ThreadID: "t1", ItemID: "cmd-bg",
		ItemType: "commandExecution", TurnID: "turn-0",
		Meta:      buildCodexExecResultMeta(t, codexExecResultRunning, "pid-bg", "sleep 1; echo done"),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("codex exec result: %v", err)
	}
	after, found, err := st.GetThreadItem("t1", "cmd-bg")
	if err != nil || !found {
		t.Fatalf("command row missing after late raw result: found=%v err=%v", found, err)
	}
	if after.ID != row.ID || after.ItemIndex != row.ItemIndex {
		t.Fatalf("late raw result rewrote command identity/index: before=%+v after=%+v", row, after)
	}
	if siblings := findItemsByKind(t, st, "t1", itemKindBackgroundDone); len(siblings) != 0 {
		t.Fatalf("late raw result created sibling rows: %+v", siblings)
	}
}

func TestCodexTerminalInteractionAfterCompletionDoesNotCreateDetachedWait(t *testing.T) {
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
	if len(waits) != 0 {
		t.Fatalf("post-completion poll should not create detached wait rows: %+v", waits)
	}
	completion, found, err := st.GetThreadItem("t1", "cmd-bg")
	if err != nil || !found {
		t.Fatalf("command completion missing: found=%v err=%v", found, err)
	}
	completionMeta := decodeItemMetaMap(t, completion.Meta)
	if _, ok := completionMeta["wait_carrier_id"]; ok {
		t.Fatalf("completion wait_carrier_id = %v, want original command row without wait carrier", completionMeta["wait_carrier_id"])
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
		t.Fatalf("tray should clear after typed command completion, got %+v", live)
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
	completion, found, err := st.GetThreadItem("t1", "cmd-bg")
	if err != nil || !found {
		t.Fatalf("command completion missing: found=%v err=%v", found, err)
	}
	completionMeta := decodeItemMetaMap(t, completion.Meta)
	if _, ok := completionMeta["wait_carrier_id"]; ok {
		t.Fatalf("completion wait_carrier_id = %v, want original command row without wait carrier", completionMeta["wait_carrier_id"])
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

func TestCodexTerminalInteractionKeepsWaitOpenAcrossLaterToolStart(t *testing.T) {
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
		t.Fatalf("wait row should stay a marker; got payload %q", waits[0].PayloadID)
	}
	if waits[0].Status != statusCompleted {
		t.Fatalf("wait status = %q, want completed", waits[0].Status)
	}
	completion, found, err := st.GetThreadItem("t1", "cmd-bg")
	if err != nil || !found {
		t.Fatalf("command completion missing: found=%v err=%v", found, err)
	}
	if completion.PayloadKind != "command_output" {
		t.Fatalf("completion payload kind = %q, want command_output", completion.PayloadKind)
	}
	completionMeta := decodeItemMetaMap(t, completion.Meta)
	if _, ok := completionMeta["wait_carrier_id"]; ok {
		t.Fatalf("completion wait_carrier_id = %v, want original command row without wait carrier", completionMeta["wait_carrier_id"])
	}
	data, err := st.GetPayloadData(completion.PayloadID)
	if err != nil {
		t.Fatalf("completion payload data: %v", err)
	}
	if string(data) != "late failure\n" {
		t.Fatalf("completion payload = %q, want late failure newline", string(data))
	}
	live := router.ListLiveCodexBackgroundTasks("t1", time.Now().UnixMilli(), 0)
	if len(live) != 0 {
		t.Fatalf("tray should clear when matching wait is flushed by completion, got %+v", live)
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
	completion, found, err := st.GetThreadItem("t1", "cmd-bg")
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

func buildWaitAgentMetaForChildren(t *testing.T, childStatuses map[string]string, childMessages map[string]string) json.RawMessage {
	t.Helper()
	receiverThreadIDs := make([]string, 0, len(childStatuses))
	agentsStates := make(map[string]any, len(childStatuses))
	for childID, status := range childStatuses {
		receiverThreadIDs = append(receiverThreadIDs, childID)
		childState := map[string]any{"status": status}
		if message := childMessages[childID]; message != "" {
			childState["message"] = message
		}
		agentsStates[childID] = childState
	}
	sort.Strings(receiverThreadIDs)
	m := map[string]any{
		"source":      "",
		"item_status": "completed",
		"toolName":    "wait_agent",
		"input": map[string]any{
			"tool":              "wait_agent",
			"receiverThreadIds": receiverThreadIDs,
			"agentsStates":      agentsStates,
		},
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal multi-child wait meta: %v", err)
	}
	return out
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
	if !row.IsBackground || row.Status != statusCompleted {
		t.Fatalf("spawn row did not settle as a backgrounded launch event: %+v", row)
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

func TestCodexSubagentInactiveStatusMarksLaunchInactiveWithoutTranscriptCompletion(t *testing.T) {
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
	if _, err := st.AppendItem(store.Item{
		ID:        "child-final",
		ThreadID:  "t1",
		TurnIndex: 0,
		Kind:      itemKindAssistantText,
		Role:      "assistant",
		Status:    statusCompleted,
		ParentID:  "spawn-inactive",
		Summary:   "Child final answer",
		CreatedAt: time.Now().UnixMilli(),
		UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("seed child final text: %v", err)
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
		t.Fatalf("inactive subagent should leave no live tray rows, got %+v", live)
	}
	if _, found, err := st.GetThreadItem("t1", nextToolCompletionID("spawn-inactive")); err != nil || found {
		t.Fatalf("direct child lifecycle must not create transcript completion: found=%v err=%v", found, err)
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

func TestCodexSubagentRunningStatusReactivatesCompletedChild(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	spawnMeta := buildSpawnAgentMeta(t, "child-followup", "running")
	for _, event := range []provider.ProviderEvent{
		{Kind: provider.EventToolStart, ThreadID: "t1", TurnID: "turn-0", ItemID: "spawn-followup", ItemType: "collab_agent", Meta: spawnMeta, Timestamp: time.Now()},
		{Kind: provider.EventToolComplete, ThreadID: "t1", TurnID: "turn-0", ItemID: "spawn-followup", ItemType: "collab_agent", Meta: spawnMeta, Timestamp: time.Now()},
		{Kind: provider.EventSubagentStatus, ThreadID: "t1", ItemID: "spawn-followup", Meta: json.RawMessage(`{"agent_path":"child-followup","status":"completed"}`), Timestamp: time.Now()},
	} {
		if err := router.Handle(event); err != nil {
			t.Fatalf("handle %s: %v", event.Kind, err)
		}
	}
	// A previous child turn may already have produced a visible completion.
	// Follow-up activity must still make the launch live again.
	now := time.Now().UnixMilli()
	if _, err := st.AppendItem(store.Item{
		ID:           nextToolCompletionID("spawn-followup"),
		ThreadID:     "t1",
		TurnIndex:    0,
		Kind:         itemKindBackgroundDone,
		Role:         "assistant",
		Status:       statusCompleted,
		CompletionOf: "spawn-followup",
		ToolName:     "collab_agent",
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("seed previous completion: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventSubagentStatus, ThreadID: "t1", ItemID: "spawn-followup",
		Meta: json.RawMessage(`{"agent_path":"child-followup","status":"running"}`), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("reactivate child: %v", err)
	}
	live, err := st.ListLiveCodexSubagentLaunches("t1")
	if err != nil || len(live) != 1 || live[0].ID != "spawn-followup" {
		t.Fatalf("reactivated live launches = %+v err=%v", live, err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventSubagentStatus, ThreadID: "t1", ItemID: "spawn-followup",
		Meta: json.RawMessage(`{"agent_path":"child-followup","status":"completed"}`), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("complete follow-up child: %v", err)
	}
	live, err = st.ListLiveCodexSubagentLaunches("t1")
	if err != nil || len(live) != 0 {
		t.Fatalf("completed follow-up remained live: %+v err=%v", live, err)
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
	live, err := st.ListLiveBackgroundTasks("t1", 0)
	if err != nil {
		t.Fatalf("live background tasks after second child: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("spawn should hide after all children finish without transcript completion, got %+v", live)
	}
	if _, found, err := st.GetThreadItem("t1", nextToolCompletionID("spawn-multi-status")); err != nil || found {
		t.Fatalf("direct child lifecycle must not create completion sibling: found=%v err=%v", found, err)
	}
}

func TestCodexSubagentStatusAggregatesMultiChildFailures(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	spawnMeta := buildSpawnAgentMetaForChildren(t, map[string]string{
		"child-a": "running",
		"child-b": "running",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "spawn-mixed-status",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "spawn-mixed-status",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn complete: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventSubagentStatus, ThreadID: "t1", ItemID: "spawn-mixed-status",
		Meta:      json.RawMessage(`{"agent_path":"child-a","status":"errored"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("first child status: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventSubagentStatus, ThreadID: "t1", ItemID: "spawn-mixed-status",
		Meta:      json.RawMessage(`{"agent_path":"child-b","status":"completed"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("second child status: %v", err)
	}

	launch, found, err := st.GetThreadItem("t1", "spawn-mixed-status")
	if err != nil || !found {
		t.Fatalf("spawn row missing: found=%v err=%v", found, err)
	}
	meta := decodeItemMetaMap(t, launch.Meta)
	if meta["codex_child_status"] != "errored" {
		t.Fatalf("codex_child_status = %v, want errored", meta["codex_child_status"])
	}
	completion, found, err := st.GetThreadItem("t1", nextToolCompletionID("spawn-mixed-status"))
	if err != nil || found {
		t.Fatalf("direct child lifecycle must not create completion row: found=%v item=%+v err=%v", found, completion, err)
	}
}

func TestCodexSubagentStatusDoesNotFlushBufferedChildTextToTranscriptCompletion(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	spawnMeta := buildSpawnAgentMeta(t, "child-buffered", "running")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "spawn-buffered",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "spawn-buffered",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn complete: %v", err)
	}
	for _, content := range []string{"first ", "second"} {
		if err := router.Handle(provider.ProviderEvent{
			Kind:            provider.EventTextDelta,
			ThreadID:        "t1",
			ParentToolUseID: "spawn-buffered",
			Content:         content,
			Timestamp:       time.Now(),
		}); err != nil {
			t.Fatalf("child text delta %q: %v", content, err)
		}
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventSubagentStatus, ThreadID: "t1", ItemID: "spawn-buffered",
		Meta:      json.RawMessage(`{"agent_path":"child-buffered","status":"completed"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("subagent status: %v", err)
	}

	completion, found, err := st.GetThreadItem("t1", nextToolCompletionID("spawn-buffered"))
	if err != nil || found {
		t.Fatalf("direct child lifecycle must not create completion row: found=%v item=%+v err=%v", found, completion, err)
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

func TestCodexSubagentWaitDoesNotMergePriorChildLifecycleStatus(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	spawnMeta := buildSpawnAgentMetaForChildren(t, map[string]string{
		"child-a": "running",
		"child-b": "running",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "spawn-mixed",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "spawn-mixed",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn complete: %v", err)
	}

	statusMeta, _ := json.Marshal(map[string]any{
		"agent_path": "child-a",
		"status":     "completed",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventSubagentStatus, ThreadID: "t1", ItemID: "spawn-mixed",
		Meta: statusMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("subagent status: %v", err)
	}

	waitMeta := buildWaitAgentMetaForChildren(t, map[string]string{
		"child-b": "completed",
	}, map[string]string{
		"child-b": "B done after wait",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "wait-mixed",
		ItemType: "wait_agent", TurnID: "turn-0", Meta: waitMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("wait start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "wait-mixed",
		ItemType: "wait_agent", TurnID: "turn-0", Meta: waitMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("wait complete: %v", err)
	}

	completion, found, err := st.GetThreadItem("t1", nextToolCompletionID("spawn-mixed"))
	if err != nil || found {
		t.Fatalf("lifecycle status must not complete wait sibling: found=%v item=%+v err=%v", found, completion, err)
	}
}

func TestCodexSubagentPriorLifecycleStatusDoesNotAttachToUnrelatedLaterWait(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	spawnMeta := buildSpawnAgentMeta(t, "child-franklin", "running")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "spawn-franklin",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "spawn-franklin",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn complete: %v", err)
	}

	timeoutWaitMeta := buildWaitAgentMetaWithMessage(t, "child-franklin", "running", "")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "wait-timeout",
		ItemType: "wait_agent", TurnID: "turn-0", Meta: timeoutWaitMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("timeout wait start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "wait-timeout",
		ItemType: "wait_agent", TurnID: "turn-0", Meta: timeoutWaitMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("timeout wait complete: %v", err)
	}

	statusMeta, _ := json.Marshal(map[string]any{
		"agent_path": "child-franklin",
		"status":     "completed",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventSubagentStatus, ThreadID: "t1", ItemID: "spawn-franklin",
		Meta: statusMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("subagent status: %v", err)
	}

	unrelatedWaitMeta := buildWaitAgentMetaWithMessage(t, "child-arendt", "completed", "Arendt done")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "wait-arendt",
		ItemType: "wait_agent", TurnID: "turn-0", Meta: unrelatedWaitMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("unrelated wait start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "wait-arendt",
		ItemType: "wait_agent", TurnID: "turn-0", Meta: unrelatedWaitMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("unrelated wait complete: %v", err)
	}

	if completion, found, err := st.GetThreadItem("t1", nextToolCompletionID("spawn-franklin")); err != nil || found {
		t.Fatalf("franklin completion = %+v found=%v err=%v, want no unrelated wait attachment", completion, found, err)
	}
}

func TestCodexSubagentNotificationAfterLifecycleStatusCarriesFinalOutput(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	spawnMeta := buildSpawnAgentMeta(t, "child-franklin", "running")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "spawn-franklin",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "spawn-franklin",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn complete: %v", err)
	}

	statusMeta, _ := json.Marshal(map[string]any{
		"agent_path": "child-franklin",
		"status":     "completed",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventSubagentStatus, ThreadID: "t1", ItemID: "spawn-franklin",
		Meta: statusMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("subagent status: %v", err)
	}
	if completion, found, err := st.GetThreadItem("t1", nextToolCompletionID("spawn-franklin")); err != nil || found {
		t.Fatalf("lifecycle status completion = %+v found=%v err=%v, want none", completion, found, err)
	}

	notifyMeta, _ := json.Marshal(map[string]any{
		"agent_path": "child-franklin",
		"status":     "completed",
		"message":    "Franklin final answer",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventSubagentNotification, ThreadID: "t1", ItemID: "spawn-franklin",
		Meta: notifyMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("subagent notification: %v", err)
	}

	completion, found, err := st.GetThreadItem("t1", nextToolCompletionID("spawn-franklin"))
	if err != nil || !found {
		t.Fatalf("notification completion missing: found=%v err=%v", found, err)
	}
	if completion.PayloadKind != payloadKindToolCallResult {
		t.Fatalf("payload kind = %q, want %s", completion.PayloadKind, payloadKindToolCallResult)
	}
	data, err := st.GetPayloadData(completion.PayloadID)
	if err != nil {
		t.Fatalf("payload data: %v", err)
	}
	if string(data) != "Franklin final answer" {
		t.Fatalf("payload = %q, want notification output", string(data))
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

	// The persisted wait rows must NOT keep the agent's final message in
	// meta.input.agentsStates — the shared payload asserted above owns
	// it. Status survives; the heavy message is removed by persist
	// shaping (itemmeta.TrimCollabAgentStateMessages), which rewrites
	// only the store rows — the sibling synthesis reads evt.Meta and
	// still sees the full message.
	carrierRow, found, err := st.GetThreadItem("t1", "wait-child")
	if err != nil || !found {
		t.Fatalf("wait carrier missing: found=%v err=%v", found, err)
	}
	for label, metaJSON := range map[string]string{"carrier": carrierRow.Meta, "completion": waitRow.Meta} {
		status, hasMessage := persistedAgentState(t, metaJSON, "child-wait")
		if hasMessage {
			t.Errorf("%s row kept the agentsStates message key; payload owns the content", label)
		}
		if status != "completed" {
			t.Errorf("%s row agentsStates status = %q, want completed", label, status)
		}
	}
}

// persistedAgentState decodes a persisted item meta and returns the
// agentsStates entry for childID: its status plus whether a message
// key survived. Key presence is the assertion target — the trim must
// DELETE the key, not null it.
func persistedAgentState(t *testing.T, metaJSON, childID string) (status string, hasMessage bool) {
	t.Helper()
	var decoded struct {
		Input struct {
			AgentsStates map[string]map[string]json.RawMessage `json:"agentsStates"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(metaJSON), &decoded); err != nil {
		t.Fatalf("decode persisted meta: %v", err)
	}
	entry, ok := decoded.Input.AgentsStates[childID]
	if !ok {
		t.Fatalf("agentsStates entry %s missing from persisted meta: %s", childID, metaJSON)
	}
	if err := json.Unmarshal(entry["status"], &status); err != nil {
		t.Fatalf("decode status for %s: %v", childID, err)
	}
	_, hasMessage = entry["message"]
	return status, hasMessage
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

func TestCodexSubagentNotificationWaitsForAllChildren(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	spawnMeta := buildSpawnAgentMetaForChildren(t, map[string]string{
		"child-a": "running",
		"child-b": "running",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "spawn-notify-multi",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "spawn-notify-multi",
		ItemType: "collab_agent", TurnID: "turn-0", Meta: spawnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("spawn complete: %v", err)
	}

	firstNotifyMeta, _ := json.Marshal(map[string]any{
		"agent_path": "child-a",
		"status":     "completed",
		"message":    "A done",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventSubagentNotification, ThreadID: "t1", ItemID: "spawn-notify-multi",
		Meta: firstNotifyMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("first subagent notification: %v", err)
	}
	if siblings := findItemsByKind(t, st, "t1", itemKindBackgroundDone); len(siblings) != 0 {
		t.Fatalf("expected no sibling after first child notification, got %d", len(siblings))
	}

	secondNotifyMeta, _ := json.Marshal(map[string]any{
		"agent_path": "child-b",
		"status":     "completed",
		"message":    "B done",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventSubagentNotification, ThreadID: "t1", ItemID: "spawn-notify-multi",
		Meta: secondNotifyMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("second subagent notification: %v", err)
	}
	completion, found, err := st.GetThreadItem("t1", nextToolCompletionID("spawn-notify-multi"))
	if err != nil || !found {
		t.Fatalf("multi notification sibling missing: found=%v err=%v", found, err)
	}
	if completion.CompletionOf != "spawn-notify-multi" {
		t.Fatalf("completion_of = %q, want spawn-notify-multi", completion.CompletionOf)
	}
	data, err := st.GetPayloadData(completion.PayloadID)
	if err != nil {
		t.Fatalf("payload data: %v", err)
	}
	if string(data) != "B done" {
		t.Fatalf("payload = %q, want final notification output", string(data))
	}
}

func TestCodexUnifiedExecTurnCompleteDoesNotBackgroundWithoutRawResult(t *testing.T) {
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

	beforeInterrupt := router.ListLiveCodexBackgroundTasks("t1", time.Now().UnixMilli(), 0)
	if len(beforeInterrupt) != 1 {
		t.Fatalf("live tray = %d, want 1 launch row before interrupt", len(beforeInterrupt))
	}
	if beforeInterrupt[0].IsBackground {
		t.Fatalf("pre-yield tracker should be IsBackground=false before turn-close (no yield signal yet)")
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "t1",
		TurnID:       "turn-0",
		TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "interrupted", Aborted: true},
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("turn complete truncated: %v", err)
	}

	afterInterrupt := router.ListLiveCodexBackgroundTasks("t1", time.Now().UnixMilli(), 0)
	if len(afterInterrupt) != 1 {
		t.Fatalf("live tray = %d, want 1 launch row after interrupt", len(afterInterrupt))
	}
	if afterInterrupt[0].IsBackground {
		t.Fatalf("turn complete without typed wait must not background unifiedExec: %+v", afterInterrupt[0])
	}
	if afterInterrupt[0].Status != statusRunning {
		t.Errorf("launch status = %q, want running", afterInterrupt[0].Status)
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

func TestClearLiveCodexBackgroundTasksDropsTransientTrayRows(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-clear",
		ItemType: "commandExecution", TurnID: "turn-0",
		Meta:      buildUnifiedExecStartMeta(t, "pid-clear", "sleep 60"),
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
	if count := router.CountLiveCodexBackgroundTasks("t1"); count != 1 {
		t.Fatalf("count before clear = %d, want 1", count)
	}
	if ids := router.ThreadIDsWithLiveCodexBackgroundTasks(); len(ids) != 1 || ids[0] != "t1" {
		t.Fatalf("live thread ids before clear = %v, want [t1]", ids)
	}

	router.ClearLiveCodexBackgroundTasks("t1")

	if count := router.CountLiveCodexBackgroundTasks("t1"); count != 0 {
		t.Fatalf("count after clear = %d, want 0", count)
	}
	if ids := router.ThreadIDsWithLiveCodexBackgroundTasks(); len(ids) != 0 {
		t.Fatalf("live thread ids after clear = %v, want empty", ids)
	}
	if live := router.ListLiveCodexBackgroundTasks("t1", time.Now().UnixMilli(), 0); len(live) != 0 {
		t.Fatalf("live tracker survived clear: %+v", live)
	}
}

func TestCodexUnifiedExecSubagentCommandDoesNotEnterMainBackgroundTray(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	parent := store.Item{
		ID:        "spawn-agent",
		ThreadID:  "t1",
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      itemKindToolCall,
		Role:      "assistant",
		Status:    statusCompleted,
		Summary:   "spawn_agent",
		ToolName:  "collab_agent",
		Meta:      `{"input":{"tool":"spawn_agent","receiverThreadIds":["child-1"]}}`,
		CreatedAt: time.Now().UnixMilli(),
		UpdatedAt: time.Now().UnixMilli(),
	}
	if err := st.InsertItem(parent); err != nil {
		t.Fatalf("seed spawn parent: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:            provider.EventToolStart,
		ThreadID:        "t1",
		ItemID:          "child-cmd",
		ItemType:        "commandExecution",
		TurnID:          "turn-0",
		ParentToolUseID: "spawn-agent",
		Meta:            buildUnifiedExecStartMeta(t, "pid-child", "sleep 60"),
		Timestamp:       time.Now(),
	}); err != nil {
		t.Fatalf("child command start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "parent continues",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}

	live := router.ListLiveCodexBackgroundTasks("t1", time.Now().UnixMilli(), 0)
	if len(live) != 0 {
		t.Fatalf("subagent command leaked into main tray: %+v", live)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:            provider.EventToolComplete,
		ThreadID:        "t1",
		ItemID:          "child-cmd",
		ItemType:        "commandExecution",
		TurnID:          "turn-0",
		ParentToolUseID: "spawn-agent",
		Content:         "done\n",
		Meta:            buildUnifiedExecCompleteMeta(t, "completed", "pid-child", "sleep 60", 0),
		Timestamp:       time.Now(),
	}); err != nil {
		t.Fatalf("child command complete: %v", err)
	}

	item, found, err := st.GetThreadItem("t1", "child-cmd")
	if err != nil || !found {
		t.Fatalf("child command persisted: found=%v err=%v", found, err)
	}
	if item.ParentID != "spawn-agent" {
		t.Fatalf("child command parent_id = %q, want spawn-agent", item.ParentID)
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
