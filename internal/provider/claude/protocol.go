package claude

import (
	"encoding/json"
	"fmt"
	"time"

	"agent-overflow/internal/provider"
)

// ParseLine parses a single NDJSON line from Claude CLI stdout
// and returns zero or more ProviderEvents.
func ParseLine(threadID string, line []byte) ([]provider.ProviderEvent, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}

	var msgType string
	if err := json.Unmarshal(raw["type"], &msgType); err != nil {
		return nil, fmt.Errorf("missing or invalid type field: %w", err)
	}

	now := time.Now()

	switch msgType {
	case "system":
		return parseSystem(threadID, raw, now, line)
	case "assistant":
		return parseAssistant(threadID, raw, now, line)
	case "user":
		// Echoed tool results. Skip — we track tool results
		// from the tool_complete flow.
		return nil, nil
	case "result":
		return parseResult(threadID, raw, now, line)
	case "stream_event":
		return parseStreamEvent(threadID, raw, now)
	case "control_request":
		return parseControlRequest(threadID, raw, now, line)
	case "rate_limit_event":
		return parseRateLimitEvent(threadID, raw, now)
	default:
		// Unknown type — skip gracefully.
		return nil, nil
	}
}

func parseSystem(threadID string, raw map[string]json.RawMessage, now time.Time, line []byte) ([]provider.ProviderEvent, error) {
	var subtype string
	if err := json.Unmarshal(raw["subtype"], &subtype); err != nil {
		return nil, nil // no subtype — skip
	}

	switch subtype {
	case "init":
		meta, _ := json.Marshal(extractSessionInfo(raw))
		return []provider.ProviderEvent{{
			Kind:      provider.EventInit,
			ThreadID:  threadID,
			Meta:      meta,
			Timestamp: now,
			Raw:       line,
		}}, nil

	case "session_state_changed":
		return []provider.ProviderEvent{{
			Kind:      provider.EventSessionStatus,
			ThreadID:  threadID,
			Content:   "session_state_changed",
			Meta:      raw["data"],
			Timestamp: now,
		}}, nil

	case "tool_progress":
		meta := extractToolProgressMeta(raw)
		itemID := ""
		if v, ok := raw["item_id"]; ok {
			json.Unmarshal(v, &itemID)
		}
		return []provider.ProviderEvent{{
			Kind:      provider.EventToolProgress,
			ThreadID:  threadID,
			ItemID:    itemID,
			Meta:      meta,
			Timestamp: now,
		}}, nil

	case "compact_boundary":
		meta := extractCompactBoundaryMeta(raw)
		return []provider.ProviderEvent{{
			Kind:      provider.EventCompactBoundary,
			ThreadID:  threadID,
			Meta:      meta,
			Timestamp: now,
		}}, nil

	case "api_retry":
		meta := raw["data"]
		return []provider.ProviderEvent{{
			Kind:      provider.EventSessionStatus,
			ThreadID:  threadID,
			Content:   "retrying",
			Meta:      meta,
			Timestamp: now,
		}}, nil

	// Explicitly skipped subtypes — no action, no error.
	case "hook_started", "hook_progress", "hook_response",
		"notification",
		"files_persisted",
		"tool_use_summary",
		"memory_recall",
		"local_command_output":
		return nil, nil

	default:
		// Unknown system subtype — skip.
		return nil, nil
	}
}

func parseAssistant(threadID string, raw map[string]json.RawMessage, now time.Time, line []byte) ([]provider.ProviderEvent, error) {
	var msg struct {
		ID      string `json:"id"`
		Content []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text,omitempty"`
			ID       string          `json:"id,omitempty"`
			Name     string          `json:"name,omitempty"`
			Input    json.RawMessage `json:"input,omitempty"`
			Thinking string          `json:"thinking,omitempty"`
		} `json:"content"`
		Role  string `json:"role"`
		Usage *struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage,omitempty"`
	}

	// The message payload is under "message" key for assistant type.
	if rawMsg, ok := raw["message"]; ok {
		if err := json.Unmarshal(rawMsg, &msg); err != nil {
			return nil, fmt.Errorf("parse assistant message: %w", err)
		}
	} else {
		// Might be flat — try parsing raw directly.
		data, _ := json.Marshal(raw)
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, nil
		}
	}

	// Top-level parent_tool_use_id links subagent (Task-tool) child messages
	// to their parent Task tool use. It's not always present, and only a
	// string when it is.
	var parentToolUseID string
	if v, ok := raw["parent_tool_use_id"]; ok {
		_ = json.Unmarshal(v, &parentToolUseID)
	}

	var events []provider.ProviderEvent

	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			events = append(events, provider.ProviderEvent{
				Kind:            provider.EventTextDelta,
				ThreadID:        threadID,
				ItemID:          msg.ID,
				Content:         block.Text,
				Role:            "assistant",
				ParentToolUseID: parentToolUseID,
				Timestamp:       now,
			})

		case "tool_use":
			if block.Name == "ExitPlanMode" {
				planMarkdown := extractExitPlanModePlan(block.Input)
				if planMarkdown == "" {
					continue
				}
				events = append(events, provider.ProviderEvent{
					Kind:            provider.EventProposedPlan,
					ThreadID:        threadID,
					ItemID:          block.ID,
					ItemType:        block.Name,
					Content:         planMarkdown,
					ParentToolUseID: parentToolUseID,
					Timestamp:       now,
				})
				continue
			}

			meta, _ := json.Marshal(map[string]any{
				"toolName": block.Name,
				"input":    block.Input,
			})
			events = append(events, provider.ProviderEvent{
				Kind:            provider.EventToolStart,
				ThreadID:        threadID,
				ItemID:          block.ID,
				ItemType:        block.Name,
				Meta:            meta,
				ParentToolUseID: parentToolUseID,
				Timestamp:       now,
			})

		case "thinking":
			events = append(events, provider.ProviderEvent{
				Kind:            provider.EventThinking,
				ThreadID:        threadID,
				ItemID:          msg.ID,
				Content:         block.Thinking,
				ParentToolUseID: parentToolUseID,
				Timestamp:       now,
			})
		}
	}

	// Emit token usage if present.
	if msg.Usage != nil {
		usageMeta, _ := json.Marshal(provider.TokenUsage{
			InputTokens:              msg.Usage.InputTokens,
			OutputTokens:             msg.Usage.OutputTokens,
			CacheReadInputTokens:     msg.Usage.CacheReadInputTokens,
			CacheCreationInputTokens: msg.Usage.CacheCreationInputTokens,
		})
		events = append(events, provider.ProviderEvent{
			Kind:            provider.EventTokenUsage,
			ThreadID:        threadID,
			Meta:            usageMeta,
			ParentToolUseID: parentToolUseID,
			Timestamp:       now,
		})
	}

	return events, nil
}

func parseResult(threadID string, raw map[string]json.RawMessage, now time.Time, line []byte) ([]provider.ProviderEvent, error) {
	var isError bool
	if v, ok := raw["is_error"]; ok {
		_ = json.Unmarshal(v, &isError)
	}

	if isError {
		var errMsg string
		if v, ok := raw["error"]; ok {
			_ = json.Unmarshal(v, &errMsg)
		}
		return []provider.ProviderEvent{{
			Kind:      provider.EventError,
			ThreadID:  threadID,
			Content:   errMsg,
			Timestamp: now,
			Raw:       line,
		}}, nil
	}

	var events []provider.ProviderEvent

	// Extract usage/cost data from the result summary.
	usage := extractResultUsage(raw)
	if usage.InputTokens > 0 || usage.OutputTokens > 0 {
		usageMeta, _ := json.Marshal(usage)
		events = append(events, provider.ProviderEvent{
			Kind:      provider.EventTokenUsage,
			ThreadID:  threadID,
			Meta:      usageMeta,
			Timestamp: now,
		})
	}

	events = append(events, provider.ProviderEvent{
		Kind:      provider.EventTurnComplete,
		ThreadID:  threadID,
		Timestamp: now,
		Raw:       line,
	})

	return events, nil
}

// extractResultUsage parses token usage from a Claude result message.
// It checks both "usage" (flat format) and "modelUsage" (per-model format)
// and aggregates total_cost_usd when present.
func extractResultUsage(raw map[string]json.RawMessage) provider.TokenUsage {
	var usage provider.TokenUsage

	// Try flat "usage" object first.
	if v, ok := raw["usage"]; ok {
		var u struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		}
		if json.Unmarshal(v, &u) == nil {
			usage.InputTokens = u.InputTokens
			usage.OutputTokens = u.OutputTokens
			usage.CacheReadInputTokens = u.CacheReadInputTokens
			usage.CacheCreationInputTokens = u.CacheCreationInputTokens
		}
	}

	// Aggregate from "modelUsage" if flat usage was empty.
	if usage.InputTokens == 0 && usage.OutputTokens == 0 {
		if v, ok := raw["modelUsage"]; ok {
			var models map[string]struct {
				InputTokens              int     `json:"inputTokens"`
				OutputTokens             int     `json:"outputTokens"`
				CacheReadInputTokens     int     `json:"cacheReadInputTokens"`
				CacheCreationInputTokens int     `json:"cacheCreationInputTokens"`
				CostUSD                  float64 `json:"costUSD"`
			}
			if json.Unmarshal(v, &models) == nil {
				for _, m := range models {
					usage.InputTokens += m.InputTokens
					usage.OutputTokens += m.OutputTokens
					usage.CacheReadInputTokens += m.CacheReadInputTokens
					usage.CacheCreationInputTokens += m.CacheCreationInputTokens
					usage.TotalCostUSD += m.CostUSD
				}
			}
		}
	}

	// Override cost with explicit total_cost_usd if present.
	if v, ok := raw["total_cost_usd"]; ok {
		var cost float64
		if json.Unmarshal(v, &cost) == nil && cost > 0 {
			usage.TotalCostUSD = cost
		}
	}

	return usage
}

func parseStreamEvent(threadID string, raw map[string]json.RawMessage, now time.Time) ([]provider.ProviderEvent, error) {
	dataRaw, ok := raw["data"]
	if !ok {
		return nil, nil
	}

	var dataObj map[string]json.RawMessage
	if json.Unmarshal(dataRaw, &dataObj) != nil {
		return nil, nil
	}

	deltaRaw, ok := dataObj["delta"]
	if !ok {
		return nil, nil
	}

	var delta struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(deltaRaw, &delta) != nil {
		return nil, nil
	}

	if delta.Text != "" {
		return []provider.ProviderEvent{{
			Kind:      provider.EventTextDelta,
			ThreadID:  threadID,
			Content:   delta.Text,
			Role:      "assistant",
			Timestamp: now,
		}}, nil
	}

	return nil, nil
}

// parseControlRequest handles the wire format:
// {"type":"control_request","request_id":"req_1_abc","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"},"permission_suggestions":[...]}}
//
// `permission_suggestions` is preserved as opaque JSON so downstream code
// (UI / approval handling) can surface the Claude SDK suggestions.
func parseControlRequest(threadID string, raw map[string]json.RawMessage, now time.Time, line []byte) ([]provider.ProviderEvent, error) {
	var requestID string
	if v, ok := raw["request_id"]; ok {
		_ = json.Unmarshal(v, &requestID)
	}

	reqRaw, ok := raw["request"]
	if !ok {
		return nil, nil
	}

	var req struct {
		Subtype               string          `json:"subtype"`
		ToolName              string          `json:"tool_name"`
		Input                 json.RawMessage `json:"input"`
		PermissionSuggestions json.RawMessage `json:"permission_suggestions"`
	}
	if err := json.Unmarshal(reqRaw, &req); err != nil {
		return nil, nil
	}

	if req.Subtype != "can_use_tool" {
		return nil, nil
	}

	approvalMeta, _ := json.Marshal(provider.ApprovalRequest{
		RequestID:             requestID,
		ThreadID:              threadID,
		ToolName:              req.ToolName,
		Description:           fmt.Sprintf("Allow %s?", req.ToolName),
		Input:                 req.Input,
		Title:                 req.ToolName,
		PermissionSuggestions: req.PermissionSuggestions,
	})

	return []provider.ProviderEvent{{
		Kind:      provider.EventApprovalRequest,
		ThreadID:  threadID,
		ItemID:    requestID,
		Meta:      approvalMeta,
		Timestamp: now,
		Raw:       line,
	}}, nil
}

func extractExitPlanModePlan(input json.RawMessage) string {
	var payload struct {
		Plan string `json:"plan"`
	}
	if err := json.Unmarshal(input, &payload); err != nil {
		return ""
	}
	return payload.Plan
}

// parseRateLimitEvent handles Claude's rate_limit_event message type.
func parseRateLimitEvent(threadID string, raw map[string]json.RawMessage, now time.Time) ([]provider.ProviderEvent, error) {
	infoRaw, ok := raw["rate_limit_info"]
	if !ok {
		return nil, nil
	}

	var info struct {
		Status        string `json:"status"`
		ResetsAt      int64  `json:"resetsAt"`
		RateLimitType string `json:"rateLimitType"`
	}
	if json.Unmarshal(infoRaw, &info) != nil {
		return nil, nil
	}

	entry := provider.RateLimitEntry{
		LimitID:   info.RateLimitType,
		LimitName: info.RateLimitType,
		ResetsAt:  info.ResetsAt,
	}
	if info.Status != "allowed" {
		entry.UsedPercent = 100
	}

	snapshot := provider.RateLimitsSnapshot{
		Provider:  string(provider.Claude),
		Limits:    []provider.RateLimitEntry{entry},
		UpdatedAt: now.UnixMilli(),
	}
	meta, _ := json.Marshal(snapshot)

	return []provider.ProviderEvent{{
		Kind:      provider.EventRateLimits,
		ThreadID:  threadID,
		Meta:      meta,
		Timestamp: now,
	}}, nil
}

// extractSessionInfo reads fields from the init message top level.
func extractSessionInfo(raw map[string]json.RawMessage) provider.SessionInfo {
	var info provider.SessionInfo

	if v, ok := raw["session_id"]; ok {
		json.Unmarshal(v, &info.SessionID)
	}
	if v, ok := raw["model"]; ok {
		json.Unmarshal(v, &info.Model)
	}
	if v, ok := raw["cwd"]; ok {
		json.Unmarshal(v, &info.CWD)
	}
	if v, ok := raw["tools"]; ok {
		json.Unmarshal(v, &info.Tools)
	}
	if v, ok := raw["claude_code_version"]; ok {
		json.Unmarshal(v, &info.Version)
	}

	return info
}
