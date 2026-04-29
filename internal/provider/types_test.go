package provider

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEventKindUniqueness(t *testing.T) {
	kinds := []EventKind{
		EventInit, EventTextDelta, EventToolStart, EventToolComplete,
		EventTurnStart, EventTurnComplete, EventApprovalRequest,
		EventApprovalResolved, EventUserInputRequest, EventUserInputResolved,
		EventSessionStatus, EventTokenUsage,
		EventError, EventPlanUpdate, EventNotification,
		EventCompactBoundary, EventRateLimits,
		EventModelRerouted, EventThreadRenamed, EventDiff,
		EventCommandOutput, EventThinking, EventProposedPlan,
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

	if len(seen) != 23 {
		t.Errorf("expected 23 unique EventKind values, got %d", len(seen))
	}
}

func TestItemKindUniqueness(t *testing.T) {
	kinds := []ItemKind{
		ItemUserText, ItemAssistantText, ItemThinking, ItemToolCall,
		ItemToolCompletion, ItemError, ItemCompaction,
		ItemTerminalInteraction,
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

func TestApprovalRequestStructuredFields(t *testing.T) {
	enabled := true
	req := ApprovalRequest{
		RequestID:   "req-002",
		ThreadID:    "t1",
		ToolName:    "permissions",
		Description: "Permission needed",
		Kind:        "permission",
		Permissions: &PermissionProfile{
			Network: &NetworkPermissions{Enabled: &enabled},
			FileSystem: &FileSystemPermissions{
				Read:  []string{"/tmp"},
				Write: []string{"/tmp/out"},
			},
		},
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded ApprovalRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Kind != "permission" {
		t.Errorf("Kind: got %q, want permission", decoded.Kind)
	}
	if decoded.Permissions == nil {
		t.Fatal("Permissions is nil")
	}
	if decoded.Permissions.Network == nil || decoded.Permissions.Network.Enabled == nil || !*decoded.Permissions.Network.Enabled {
		t.Error("Network.Enabled should be true")
	}
	if len(decoded.Permissions.FileSystem.Read) != 1 || decoded.Permissions.FileSystem.Read[0] != "/tmp" {
		t.Errorf("FileSystem.Read: got %v", decoded.Permissions.FileSystem.Read)
	}
}

func TestUserInputRequestJSONRoundTrip(t *testing.T) {
	req := UserInputRequest{
		RequestID: "req-questions",
		ThreadID:  "t1",
		ToolName:  "AskUserQuestion",
		Title:     "User Input Required",
		Questions: []UserInputQuestion{
			{
				ID:       "q1",
				Header:   "Choose framework",
				Question: "Which framework?",
				Options: []UserInputQuestionOption{
					{Label: "React", Description: "React.js"},
					{Label: "Vue", Description: "Vue.js"},
				},
			},
		},
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded UserInputRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded.Questions) != 1 {
		t.Fatalf("Questions len: got %d, want 1", len(decoded.Questions))
	}
	if decoded.Questions[0].ID != "q1" {
		t.Errorf("Question ID: got %q, want q1", decoded.Questions[0].ID)
	}
	if len(decoded.Questions[0].Options) != 2 {
		t.Errorf("Options len: got %d, want 2", len(decoded.Questions[0].Options))
	}
}

func TestApprovalResponseStructuredFields(t *testing.T) {
	resp := ApprovalResponse{
		RequestID:   "req-003",
		Decision:    "allow",
		Permissions: &PermissionProfile{Network: &NetworkPermissions{}},
		Scope:       "session",
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded ApprovalResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Scope != "session" {
		t.Errorf("Scope: got %q, want session", decoded.Scope)
	}
	if decoded.Permissions == nil {
		t.Error("Permissions is nil")
	}
}

func TestUserInputResponseJSON(t *testing.T) {
	resp := UserInputResponse{
		RequestID: "req-003",
		Decision:  "accept",
		Answers: map[string]UserInputAnswer{
			"q1": SingleUserInputAnswer("React"),
			"q2": UserInputAnswer{"turn", "session"},
		},
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded UserInputResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := decoded.Answers["q1"]; len(got) != 1 || got[0] != "React" {
		t.Errorf("Answers[q1]: got %v, want [React]", got)
	}
	if got := decoded.Answers["q2"]; len(got) != 2 || got[0] != "turn" || got[1] != "session" {
		t.Errorf("Answers[q2]: got %v, want [turn session]", got)
	}
}

func TestUserInputAnswerJSON(t *testing.T) {
	tests := []struct {
		name    string
		answer  UserInputAnswer
		want    string
		wantLen int
	}{
		{name: "single", answer: SingleUserInputAnswer("React"), want: `"React"`, wantLen: 1},
		{name: "multi", answer: UserInputAnswer{"turn", "session"}, want: `["turn","session"]`, wantLen: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.answer)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(data) != tt.want {
				t.Fatalf("marshal = %s, want %s", data, tt.want)
			}

			var decoded UserInputAnswer
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(decoded) != tt.wantLen {
				t.Fatalf("len(decoded) = %d, want %d", len(decoded), tt.wantLen)
			}
		})
	}
}

func TestUserInputAnswerRejectsInvalidJSON(t *testing.T) {
	var answer UserInputAnswer
	err := json.Unmarshal([]byte(`{"invalid":true}`), &answer)
	if err == nil {
		t.Fatal("expected error for invalid answer JSON")
	}
	if !strings.Contains(err.Error(), "user input answer") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestApprovalResponseOmitsStructuredFieldsWhenEmpty(t *testing.T) {
	resp := ApprovalResponse{
		RequestID: "req-004",
		Decision:  "deny",
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}

	for _, field := range []string{"answers", "permissions", "scope"} {
		if _, ok := raw[field]; ok {
			t.Errorf("expected field %q to be omitted when empty, but it was present", field)
		}
	}
}

func TestContextWindowJSON(t *testing.T) {
	cw := ContextWindow{
		UsedTokens:     50000,
		MaxTokens:      200000,
		UsedPercentage: 25.0,
	}

	data, err := json.Marshal(cw)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded ContextWindow
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.UsedTokens != 50000 {
		t.Errorf("UsedTokens: got %d, want 50000", decoded.UsedTokens)
	}
	if decoded.MaxTokens != 200000 {
		t.Errorf("MaxTokens: got %d, want 200000", decoded.MaxTokens)
	}
}

func TestRateLimitsSnapshotJSON(t *testing.T) {
	snap := RateLimitsSnapshot{
		Provider: "claude",
		Limits: []RateLimitEntry{
			{LimitID: "5h", LimitName: "5 Hour", UsedPercent: 42.5, WindowMins: 300, ResetsAt: 1700000000000},
		},
		UpdatedAt: 1700000000000,
	}

	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded RateLimitsSnapshot
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Provider != "claude" {
		t.Errorf("Provider: got %q, want claude", decoded.Provider)
	}
	if len(decoded.Limits) != 1 {
		t.Fatalf("Limits len: got %d, want 1", len(decoded.Limits))
	}
	if decoded.Limits[0].UsedPercent != 42.5 {
		t.Errorf("UsedPercent: got %f, want 42.5", decoded.Limits[0].UsedPercent)
	}
}

func TestProviderEventReplaceField(t *testing.T) {
	evt := ProviderEvent{
		Kind:      EventDiff,
		ThreadID:  "t1",
		Replace:   true,
		Timestamp: time.Now(),
	}

	data, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded ProviderEvent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if !decoded.Replace {
		t.Error("Replace should be true")
	}

	// When Replace is false, it should be omitted.
	evt.Replace = false
	data, err = json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	if _, ok := raw["replace"]; ok {
		t.Error("replace should be omitted when false")
	}
}
