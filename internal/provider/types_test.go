package provider

import (
	"encoding/json"
	"testing"
	"time"
)

func TestEventKindUniqueness(t *testing.T) {
	kinds := []EventKind{
		EventInit, EventTextDelta, EventToolStart, EventToolComplete,
		EventTurnStart, EventTurnComplete, EventApprovalRequest,
		EventApprovalResolved, EventSessionStatus, EventTokenUsage,
		EventError, EventBackgroundStart, EventBackgroundDelta,
		EventBackgroundComplete, EventDiff, EventCommandOutput, EventThinking,
	}

	seen := make(map[EventKind]bool, len(kinds))
	for _, k := range kinds {
		if k == "" {
			t.Errorf("EventKind has empty string value")
		}
		if seen[k] {
			t.Errorf("duplicate EventKind value: %q", k)
		}
		seen[k] = true
	}

	if len(seen) != 17 {
		t.Errorf("expected 17 unique EventKind values, got %d", len(seen))
	}
}

func TestItemKindUniqueness(t *testing.T) {
	kinds := []ItemKind{
		ItemText, ItemToolCall, ItemToolResult, ItemThinking,
		ItemDiff, ItemCommandExecution, ItemBackgroundStarted, ItemBackgroundDone,
	}

	seen := make(map[ItemKind]bool, len(kinds))
	for _, k := range kinds {
		if k == "" {
			t.Errorf("ItemKind has empty string value")
		}
		if seen[k] {
			t.Errorf("duplicate ItemKind value: %q", k)
		}
		seen[k] = true
	}

	if len(seen) != 8 {
		t.Errorf("expected 8 unique ItemKind values, got %d", len(seen))
	}
}

func TestProviderEventJSONRoundTrip(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	meta := json.RawMessage(`{"toolName":"bash","exitCode":0}`)

	original := ProviderEvent{
		Kind:      EventToolStart,
		ThreadID:  "thread-123",
		TurnID:    "turn-456",
		ItemID:    "item-789",
		ItemType:  "tool_use",
		Content:   "echo hello",
		Role:      "assistant",
		Meta:      meta,
		Timestamp: now,
		Raw:       json.RawMessage(`{"raw":"data"}`),
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded ProviderEvent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Kind != original.Kind {
		t.Errorf("Kind: got %q, want %q", decoded.Kind, original.Kind)
	}
	if decoded.ThreadID != original.ThreadID {
		t.Errorf("ThreadID: got %q, want %q", decoded.ThreadID, original.ThreadID)
	}
	if decoded.TurnID != original.TurnID {
		t.Errorf("TurnID: got %q, want %q", decoded.TurnID, original.TurnID)
	}
	if decoded.ItemID != original.ItemID {
		t.Errorf("ItemID: got %q, want %q", decoded.ItemID, original.ItemID)
	}
	if decoded.ItemType != original.ItemType {
		t.Errorf("ItemType: got %q, want %q", decoded.ItemType, original.ItemType)
	}
	if decoded.Content != original.Content {
		t.Errorf("Content: got %q, want %q", decoded.Content, original.Content)
	}
	if decoded.Role != original.Role {
		t.Errorf("Role: got %q, want %q", decoded.Role, original.Role)
	}
	if string(decoded.Meta) != string(original.Meta) {
		t.Errorf("Meta: got %s, want %s", decoded.Meta, original.Meta)
	}
	if !decoded.Timestamp.Equal(original.Timestamp) {
		t.Errorf("Timestamp: got %v, want %v", decoded.Timestamp, original.Timestamp)
	}

	// Raw is tagged json:"-" so it must NOT be marshaled
	if decoded.Raw != nil {
		t.Errorf("Raw should not be marshaled, got %s", decoded.Raw)
	}
}

func TestProviderEventOmitsEmptyFields(t *testing.T) {
	evt := ProviderEvent{
		Kind:      EventTextDelta,
		ThreadID:  "thread-1",
		Timestamp: time.Now(),
	}

	data, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}

	for _, field := range []string{"turnId", "itemId", "itemType", "content", "role", "meta"} {
		if _, ok := raw[field]; ok {
			t.Errorf("expected field %q to be omitted when empty, but it was present", field)
		}
	}

	// kind and threadId must always be present
	for _, field := range []string{"kind", "threadId"} {
		if _, ok := raw[field]; !ok {
			t.Errorf("expected field %q to be present, but it was missing", field)
		}
	}
}

func TestApprovalRequestJSONRoundTrip(t *testing.T) {
	input := json.RawMessage(`{"command":"rm -rf /"}`)
	original := ApprovalRequest{
		RequestID:   "req-001",
		ThreadID:    "thread-123",
		TurnID:      "turn-456",
		ToolName:    "bash",
		Description: "Execute shell command",
		Input:       input,
		Title:       "Run dangerous command",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded ApprovalRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.RequestID != original.RequestID {
		t.Errorf("RequestID: got %q, want %q", decoded.RequestID, original.RequestID)
	}
	if decoded.ThreadID != original.ThreadID {
		t.Errorf("ThreadID: got %q, want %q", decoded.ThreadID, original.ThreadID)
	}
	if decoded.TurnID != original.TurnID {
		t.Errorf("TurnID: got %q, want %q", decoded.TurnID, original.TurnID)
	}
	if decoded.ToolName != original.ToolName {
		t.Errorf("ToolName: got %q, want %q", decoded.ToolName, original.ToolName)
	}
	if decoded.Description != original.Description {
		t.Errorf("Description: got %q, want %q", decoded.Description, original.Description)
	}
	if string(decoded.Input) != string(original.Input) {
		t.Errorf("Input: got %s, want %s", decoded.Input, original.Input)
	}
	if decoded.Title != original.Title {
		t.Errorf("Title: got %q, want %q", decoded.Title, original.Title)
	}
}

func TestApprovalResponseJSON(t *testing.T) {
	resp := ApprovalResponse{
		RequestID: "req-001",
		Decision:  "allow",
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded ApprovalResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.RequestID != resp.RequestID {
		t.Errorf("RequestID: got %q, want %q", decoded.RequestID, resp.RequestID)
	}
	if decoded.Decision != resp.Decision {
		t.Errorf("Decision: got %q, want %q", decoded.Decision, resp.Decision)
	}
}

func TestSessionInfoJSON(t *testing.T) {
	info := SessionInfo{
		SessionID: "session-abc",
		Model:     "claude-sonnet-4-20250514",
		CWD:       "/home/user/project",
		Tools:     []string{"bash", "editor", "browser"},
		Version:   "1.0.0",
	}

	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded SessionInfo
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.SessionID != info.SessionID {
		t.Errorf("SessionID: got %q, want %q", decoded.SessionID, info.SessionID)
	}
	if decoded.Model != info.Model {
		t.Errorf("Model: got %q, want %q", decoded.Model, info.Model)
	}
	if decoded.CWD != info.CWD {
		t.Errorf("CWD: got %q, want %q", decoded.CWD, info.CWD)
	}
	if len(decoded.Tools) != len(info.Tools) {
		t.Fatalf("Tools len: got %d, want %d", len(decoded.Tools), len(info.Tools))
	}
	for i, tool := range decoded.Tools {
		if tool != info.Tools[i] {
			t.Errorf("Tools[%d]: got %q, want %q", i, tool, info.Tools[i])
		}
	}
	if decoded.Version != info.Version {
		t.Errorf("Version: got %q, want %q", decoded.Version, info.Version)
	}
}

func TestSessionInfoOmitsEmptyOptionalFields(t *testing.T) {
	info := SessionInfo{
		SessionID: "session-1",
		Model:     "opus",
		CWD:       "/tmp",
	}

	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}

	for _, field := range []string{"tools", "version"} {
		if _, ok := raw[field]; ok {
			t.Errorf("expected field %q to be omitted when empty, but it was present", field)
		}
	}
}

func TestTokenUsageJSON(t *testing.T) {
	usage := TokenUsage{
		InputTokens:              1500,
		OutputTokens:             300,
		CacheReadInputTokens:     1000,
		CacheCreationInputTokens: 200,
		TotalCostUSD:             0.0042,
	}

	data, err := json.Marshal(usage)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded TokenUsage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.InputTokens != usage.InputTokens {
		t.Errorf("InputTokens: got %d, want %d", decoded.InputTokens, usage.InputTokens)
	}
	if decoded.OutputTokens != usage.OutputTokens {
		t.Errorf("OutputTokens: got %d, want %d", decoded.OutputTokens, usage.OutputTokens)
	}
	if decoded.CacheReadInputTokens != usage.CacheReadInputTokens {
		t.Errorf("CacheReadInputTokens: got %d, want %d", decoded.CacheReadInputTokens, usage.CacheReadInputTokens)
	}
	if decoded.CacheCreationInputTokens != usage.CacheCreationInputTokens {
		t.Errorf("CacheCreationInputTokens: got %d, want %d", decoded.CacheCreationInputTokens, usage.CacheCreationInputTokens)
	}
	if decoded.TotalCostUSD != usage.TotalCostUSD {
		t.Errorf("TotalCostUSD: got %f, want %f", decoded.TotalCostUSD, usage.TotalCostUSD)
	}
}

func TestTokenUsageOmitsZeroOptionals(t *testing.T) {
	usage := TokenUsage{
		InputTokens:  100,
		OutputTokens: 50,
	}

	data, err := json.Marshal(usage)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}

	for _, field := range []string{"cacheReadInputTokens", "cacheCreationInputTokens", "totalCostUsd"} {
		if _, ok := raw[field]; ok {
			t.Errorf("expected field %q to be omitted when zero, but it was present", field)
		}
	}
}

func TestProviderKindValues(t *testing.T) {
	if Claude != "claude" {
		t.Errorf("Claude: got %q, want %q", Claude, "claude")
	}
	if Codex != "codex" {
		t.Errorf("Codex: got %q, want %q", Codex, "codex")
	}
}
