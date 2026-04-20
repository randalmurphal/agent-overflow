package claude

import (
	"encoding/json"
	"fmt"
	"time"

	"agent-overflow/internal/provider"
)

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
		ToolUseID             string          `json:"tool_use_id"`
		ToolUseIDCamel        string          `json:"toolUseId"`
		Input                 json.RawMessage `json:"input"`
		PermissionSuggestions json.RawMessage `json:"permission_suggestions"`
	}
	if err := json.Unmarshal(reqRaw, &req); err != nil {
		return nil, nil
	}

	if req.Subtype != "can_use_tool" {
		return nil, nil
	}

	toolUseID := req.ToolUseID
	if toolUseID == "" {
		toolUseID = req.ToolUseIDCamel
	}

	approvalMeta, _ := json.Marshal(provider.ApprovalRequest{
		RequestID:             requestID,
		ThreadID:              threadID,
		ToolUseID:             toolUseID,
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
