package agy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/acp"
)

func TestParseAgyTranscriptModel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw        string
		wantModel  string
		wantEffort string
	}{
		{"gemini-4-hydrogen-tiered-high", "gemini-4-hydrogen", "high"},
		{"gemini-4-neon-xhigh", "gemini-4-neon", "xhigh"},
		{"claude-sonnet-4-6-medium", "claude-sonnet-4-6", "medium"},
		{"gpt-oss-120b-medium", "gpt-oss-120b-medium", ""},
		{"gemini-pro-orchestrator", "gemini-pro-orchestrator", ""},
		{"", "", ""},
	}
	for _, tc := range cases {
		gotModel, gotEffort := ParseAgyTranscriptModel(tc.raw)
		if gotModel != tc.wantModel || gotEffort != tc.wantEffort {
			t.Errorf("ParseAgyTranscriptModel(%q) = (%q, %q), want (%q, %q)",
				tc.raw, gotModel, gotEffort, tc.wantModel, tc.wantEffort)
		}
	}
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestWatchSubagentCorrelatesByInboxToolCallIDAndSettlesFromParentInbox(t *testing.T) {
	t.Parallel()
	appDataDir := t.TempDir()
	parentID := "4d5f2b49-97b2-49e7-a214-7aff00575443"
	child1 := "8d1d87ec-d2f0-472e-8626-2c83eefc2bc1"
	child2 := "7e836975-0d1f-4944-a41f-9940e16715d1"

	// Two parallel subagents launched simultaneously — exact toolCall.id in
	// each child's inbox uniquely identifies them without touching transcript.jsonl.
	writeJSONFile(t, filepath.Join(appDataDir, "brain", parentID, ".system_generated", "subagents", child1+".json"), map[string]any{
		"conversationId": child1,
		"state":          "SUBAGENT_STATE_ALIVE",
	})
	writeJSONFile(t, filepath.Join(appDataDir, "brain", parentID, ".system_generated", "subagents", child2+".json"), map[string]any{
		"conversationId": child2,
		"state":          "SUBAGENT_STATE_ALIVE",
	})
	writeJSONFile(t, filepath.Join(appDataDir, "brain", child1, ".system_generated", "messages", "launch-1.json"), map[string]any{
		"sender": parentID,
		"sourceMetadata": map[string]any{
			"tool": map[string]any{
				"conversationId": parentID,
				"toolCall":       map[string]any{"id": "call_sub_1", "name": "invoke_subagent"},
			},
		},
	})
	writeJSONFile(t, filepath.Join(appDataDir, "brain", child2, ".system_generated", "messages", "launch-2.json"), map[string]any{
		"sender": parentID,
		"sourceMetadata": map[string]any{
			"tool": map[string]any{
				"conversationId": parentID,
				"toolCall":       map[string]any{"id": "call_sub_2", "name": "invoke_subagent"},
			},
		},
	})

	flavor := NewFlavor(appDataDir)
	flavor.subagentPollInterval = 10 * time.Millisecond
	flavor.subagentDiscoveryTimeout = 2 * time.Second

	// Live ResolveSubagentLaunch maps child conversation UUID -> parent launch ID.
	if gotLaunch, ok := flavor.ResolveSubagentLaunch(context.Background(), parentID, child2); !ok || gotLaunch != "call_sub_2" {
		t.Fatalf("ResolveSubagentLaunch(child2) = (%q, %v), want (call_sub_2, true)", gotLaunch, ok)
	}

	var mu sync.Mutex
	var events []provider.ProviderEvent
	var discovered string
	done := make(chan struct{})
	go func() {
		defer close(done)
		flavor.WatchSubagent(context.Background(), acp.SubagentLaunchContext{
			SessionID:    parentID,
			ThreadID:     "t1",
			TurnID:       "turn-1",
			ToolCallID:   "call_sub_1",
			RawInput:     json.RawMessage(`{"TypeName":"research","Role":"Queue Auditor","Model":"inherit"}`),
			Started:      acp.NormalizedToolStart{SubagentType: "research", Description: "Queue Auditor"},
			ParentModel:  "gemini-4-neon",
			ParentEffort: "high",
			Emit: func(evt provider.ProviderEvent) {
				mu.Lock()
				events = append(events, evt)
				mu.Unlock()
			},
			OnChildDiscovered: func(id string) {
				mu.Lock()
				discovered = id
				mu.Unlock()
			},
		})
	}()

	// Wait for discovery callback, then deliver child1's report into parent inbox.
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		got := discovered
		mu.Unlock()
		if got == child1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for child discovery, got %q", got)
		}
		time.Sleep(10 * time.Millisecond)
	}

	writeJSONFile(t, filepath.Join(appDataDir, "brain", parentID, ".system_generated", "messages", "report-1.json"), map[string]any{
		"sender":   child1,
		"priority": "MESSAGE_PRIORITY_HIGH",
		"content":  "Found 7 queue bugs.",
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WatchSubagent did not settle after parent inbox report arrived")
	}

	mu.Lock()
	defer mu.Unlock()
	var sawIdentity, sawCompleted bool
	for _, ev := range events {
		if ev.Kind == provider.EventToolStart && ev.ItemID == "call_sub_1" {
			var m map[string]any
			_ = json.Unmarshal(ev.Meta, &m)
			if m["agent_id"] == child1 && m["subagent_model"] == "gemini-4-neon" && m["run_effort"] == "high" {
				sawIdentity = true
			}
		}
		if ev.Kind == provider.EventBackgroundTaskNotification && ev.ItemID == "call_sub_1" {
			var m map[string]any
			_ = json.Unmarshal(ev.Meta, &m)
			if m["status"] == "completed" && ev.Content == "Found 7 queue bugs." {
				sawCompleted = true
			}
		}
	}
	if !sawIdentity {
		t.Fatalf("missing identity meta stamp (agent_id + inherited parent model/effort); events=%+v", events)
	}
	if !sawCompleted {
		t.Fatalf("missing completed background task notification with inbox report; events=%+v", events)
	}
}

func TestWatchSubagentSettlesKilledFromSubagentRecord(t *testing.T) {
	t.Parallel()
	appDataDir := t.TempDir()
	parentID := "4d5f2b49-97b2-49e7-a214-7aff00575443"
	childID := "2893f594-c39d-4623-b8af-0d15a65a717e"

	recPath := filepath.Join(appDataDir, "brain", parentID, ".system_generated", "subagents", childID+".json")
	writeJSONFile(t, recPath, map[string]any{
		"conversationId": childID,
		"state":          "SUBAGENT_STATE_ALIVE",
	})
	writeJSONFile(t, filepath.Join(appDataDir, "brain", childID, ".system_generated", "messages", "launch.json"), map[string]any{
		"sender": parentID,
		"sourceMetadata": map[string]any{
			"tool": map[string]any{
				"conversationId": parentID,
				"toolCall":       map[string]any{"id": "call_kill_1", "name": "invoke_subagent"},
			},
		},
	})

	flavor := NewFlavor(appDataDir)
	flavor.subagentPollInterval = 10 * time.Millisecond
	flavor.subagentDiscoveryTimeout = 2 * time.Second

	var mu sync.Mutex
	var events []provider.ProviderEvent
	done := make(chan struct{})
	go func() {
		defer close(done)
		flavor.WatchSubagent(context.Background(), acp.SubagentLaunchContext{
			SessionID:  parentID,
			ThreadID:   "t1",
			TurnID:     "turn-1",
			ToolCallID: "call_kill_1",
			Started:    acp.NormalizedToolStart{SubagentType: "research"},
			Emit: func(evt provider.ProviderEvent) {
				mu.Lock()
				events = append(events, evt)
				mu.Unlock()
			},
		})
	}()

	time.Sleep(30 * time.Millisecond)
	writeJSONFile(t, recPath, map[string]any{
		"conversationId": childID,
		"state":          "SUBAGENT_STATE_KILLED",
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WatchSubagent did not settle on SUBAGENT_STATE_KILLED")
	}

	mu.Lock()
	defer mu.Unlock()
	for _, ev := range events {
		if ev.Kind == provider.EventBackgroundTaskNotification {
			var m map[string]any
			_ = json.Unmarshal(ev.Meta, &m)
			if m["status"] == "killed" {
				return
			}
		}
	}
	t.Fatalf("expected killed notification, got events=%+v", events)
}

func TestWatchSubagentDiscoveryTimeoutDoesNotEmitFakeFailure(t *testing.T) {
	t.Parallel()
	flavor := NewFlavor(t.TempDir())
	flavor.subagentPollInterval = 10 * time.Millisecond
	flavor.subagentDiscoveryTimeout = 40 * time.Millisecond

	var events []provider.ProviderEvent
	flavor.WatchSubagent(context.Background(), acp.SubagentLaunchContext{
		SessionID:  "4d5f2b49-97b2-49e7-a214-7aff00575443",
		ThreadID:   "t1",
		TurnID:     "turn-1",
		ToolCallID: "call_missing",
		Emit: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	})
	if len(events) != 0 {
		t.Fatalf("discovery timeout must not emit fake failed agent notification, got %+v", events)
	}
}
