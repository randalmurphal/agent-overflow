package triage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
)

func TestAgyBackgroundSubagentLifecycleThroughTriage(t *testing.T) {
	router, st, bus := newTestRouter(t)
	createGeneratedImageThread(t, st, "t-agy-sub", "agy")

	now := time.Now()
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t-agy-sub",
		TurnID:    "turn-1",
		Timestamp: now,
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	// 1. Launch Agent tool_call marked background (as acp.Session emits for tracked agy invoke_subagent).
	launchStartMeta, _ := json.Marshal(map[string]any{
		"toolName":        "Agent",
		"subagent_launch": true,
		"description":     "Note Reader",
		"subagent_type":   "research",
		"is_background":   true,
		"provider":        "agy",
		"input": map[string]any{
			"subagent_type":     "research",
			"description":       "Note Reader",
			"prompt":            "Read note.txt",
			"model":             "flash",
			"run_in_background": true,
		},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t-agy-sub",
		TurnID:    "turn-1",
		ItemID:    "launch-1",
		ItemType:  "Agent",
		Meta:      launchStartMeta,
		Timestamp: now,
	}); err != nil {
		t.Fatalf("launch start: %v", err)
	}

	// 2. Immediate ACP tool_call_update(completed) carries is_background:true so the launch stays running.
	launchAckMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Agent",
		"is_background": true,
		"item_status":   "completed",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolComplete,
		ThreadID:  "t-agy-sub",
		TurnID:    "turn-1",
		ItemID:    "launch-1",
		ItemType:  "Agent",
		Meta:      launchAckMeta,
		Timestamp: now.Add(20 * time.Millisecond),
	}); err != nil {
		t.Fatalf("launch ack: %v", err)
	}

	launch, found, err := st.GetThreadItem("t-agy-sub", "launch-1")
	if err != nil || !found {
		t.Fatalf("launch row missing: found=%v err=%v", found, err)
	}
	if launch.Status != statusRunning || !launch.IsBackground {
		t.Fatalf("launch after ack = status:%q bg:%v, want running + bg=true", launch.Status, launch.IsBackground)
	}

	// 3. Out-of-band watcher stamps task_id, agent_id, subagent_model, run_effort.
	stampMeta, _ := json.Marshal(map[string]any{
		"task_id":        "child-conv-1",
		"agent_id":       "child-conv-1",
		"subagent_model": "gemini-4-hydrogen",
		"run_effort":     "high",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t-agy-sub",
		TurnID:    "turn-1",
		ItemID:    "launch-1",
		Meta:      stampMeta,
		Timestamp: now.Add(40 * time.Millisecond),
	}); err != nil {
		t.Fatalf("stamp meta: %v", err)
	}

	// 4. Child transcript items (thinking, Read tool call, text) + live SubagentProgress.
	thinkMeta, _ := json.Marshal(map[string]any{provider.MetaTranscriptSnapshotKey: true})
	if err := router.Handle(provider.ProviderEvent{
		Kind:            provider.EventThinking,
		ThreadID:        "t-agy-sub",
		TurnID:          "turn-1",
		ItemID:          "agy-sub:child-conv-1:think:1",
		ParentToolUseID: "launch-1",
		Content:         "Checking note.txt",
		Meta:            thinkMeta,
		Timestamp:       now.Add(50 * time.Millisecond),
	}); err != nil {
		t.Fatalf("child thinking: %v", err)
	}

	readStartMeta, _ := json.Marshal(map[string]any{
		"toolName": "Read",
		"input":    map[string]any{"file_path": "/tmp/note.txt"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:            provider.EventToolStart,
		ThreadID:        "t-agy-sub",
		TurnID:          "turn-1",
		ItemID:          "agy-sub:child-conv-1:tool:1:0",
		ItemType:        "Read",
		ParentToolUseID: "launch-1",
		Meta:            readStartMeta,
		Timestamp:       now.Add(60 * time.Millisecond),
	}); err != nil {
		t.Fatalf("child read start: %v", err)
	}
	readDoneMeta, _ := json.Marshal(map[string]any{
		"toolName":    "Read",
		"item_status": "completed",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:            provider.EventToolComplete,
		ThreadID:        "t-agy-sub",
		TurnID:          "turn-1",
		ItemID:          "agy-sub:child-conv-1:tool:1:0",
		ItemType:        "Read",
		ParentToolUseID: "launch-1",
		Content:         "1: SECRET_VALUE=42",
		ContentPresent:  true,
		Meta:            readDoneMeta,
		Timestamp:       now.Add(70 * time.Millisecond),
	}); err != nil {
		t.Fatalf("child read complete: %v", err)
	}

	progressMeta := provider.SubagentProgressMeta{
		TaskID:       "child-conv-1",
		ToolUses:     2,
		TotalTokens:  8634,
		DurationMs:   3100,
		Activity:     "Read: note.txt",
		LastToolName: "Read",
		AgentType:    "research",
	}
	progressJSON, _ := json.Marshal(progressMeta)
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventSubagentProgress,
		ThreadID:  "t-agy-sub",
		TurnID:    "turn-1",
		ItemID:    "launch-1",
		Meta:      progressJSON,
		Timestamp: now.Add(80 * time.Millisecond),
	}); err != nil {
		t.Fatalf("subagent progress: %v", err)
	}

	// 5. Settlement notification closes the card and writes complete:launch-1 with final counters + report preview.
	notifMeta, _ := json.Marshal(map[string]any{
		"task_id":     "child-conv-1",
		"tool_use_id": "launch-1",
		"status":      "completed",
		"source":      "agy_transcript",
		"usage":       progressMeta,
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventBackgroundTaskNotification,
		ThreadID:  "t-agy-sub",
		TurnID:    "turn-1",
		ItemID:    "launch-1",
		Content:   "SECRET_VALUE is 42.",
		Meta:      notifMeta,
		Timestamp: now.Add(100 * time.Millisecond),
	}); err != nil {
		t.Fatalf("background notification: %v", err)
	}

	// Verify launch row carries resolved subagent_model, run_effort, and task_id.
	launch, found, err = st.GetThreadItem("t-agy-sub", "launch-1")
	if err != nil || !found {
		t.Fatalf("launch lookup: found=%v err=%v", found, err)
	}
	var launchMeta map[string]any
	if err := json.Unmarshal([]byte(launch.Meta), &launchMeta); err != nil {
		t.Fatalf("unmarshal launch meta: %v", err)
	}
	if launchMeta["subagent_model"] != "gemini-4-hydrogen" ||
		launchMeta["run_effort"] != "high" ||
		launchMeta["task_id"] != "child-conv-1" {
		t.Fatalf("launch meta missing stamps: %+v", launchMeta)
	}

	// Verify child rows persisted under launch-1.
	allItems, err := st.ListItems("t-agy-sub")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var childCount int
	for _, it := range allItems {
		if it.ParentID == "launch-1" {
			childCount++
		}
	}
	if childCount < 2 {
		t.Fatalf("expected >= 2 child rows parented to launch-1, got %d", childCount)
	}

	// Verify completion sibling complete:launch-1 carries persisted subagentProgress and report preview.
	comp, found, err := st.GetThreadItem("t-agy-sub", ToolCompletionID("launch-1"))
	if err != nil || !found {
		t.Fatalf("completion sibling missing: found=%v err=%v", found, err)
	}
	if comp.Status != statusCompleted {
		t.Fatalf("completion status = %q, want completed", comp.Status)
	}
	if !strings.Contains(comp.PayloadMeta, "SECRET_VALUE is 42.") {
		t.Fatalf("completion payloadMeta missing report preview: %s", comp.PayloadMeta)
	}
	var compMeta struct {
		NotificationOutputLoaded bool                          `json:"notification_output_loaded"`
		SubagentProgress         provider.SubagentProgressMeta `json:"subagentProgress"`
	}
	if err := json.Unmarshal([]byte(comp.Meta), &compMeta); err != nil {
		t.Fatalf("unmarshal completion meta: %v", err)
	}
	if !compMeta.NotificationOutputLoaded {
		t.Fatalf("expected notification_output_loaded=true in %s", comp.Meta)
	}
	if compMeta.SubagentProgress.ToolUses != 2 || compMeta.SubagentProgress.TotalTokens != 8634 {
		t.Fatalf("completion subagentProgress = %+v, want toolUses=2 totalTokens=8634", compMeta.SubagentProgress)
	}

	// Verify live progress event was fanned out on the bus.
	var sawLiveProgress bool
	for _, emitted := range bus.snapshot() {
		if emitted.eventName == string(eventchan.ProviderSubagentProgress) {
			sawLiveProgress = true
			break
		}
	}
	if !sawLiveProgress {
		t.Fatal("expected provider:subagent_progress event on bus")
	}
}
