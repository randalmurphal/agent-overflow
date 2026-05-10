// Package claude — parser for `control_request` NDJSON lines (CanUseTool
// approval requests and the exit_plan_mode signal). The control_request
// envelope is RPC-shaped, so this file also shapes the matching response
// structures the session uses to reply.

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

	if req.ToolName == "AskUserQuestion" {
		questions := parseAskUserQuestions(req.Input)
		if len(questions) > 0 {
			meta, _ := json.Marshal(provider.UserInputRequest{
				RequestID: requestID,
				ThreadID:  threadID,
				ToolUseID: toolUseID,
				ToolName:  req.ToolName,
				Title:     "User Input Required",
				Questions: questions,
			})
			return []provider.ProviderEvent{{
				Kind:      provider.EventUserInputRequest,
				ThreadID:  threadID,
				ItemID:    requestID,
				Meta:      meta,
				Timestamp: now,
				Raw:       line,
			}}, nil
		}
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

func parseAskUserQuestions(input json.RawMessage) []provider.UserInputQuestion {
	var payload struct {
		Questions *[]provider.UserInputQuestion `json:"questions"`
	}
	if json.Unmarshal(input, &payload) != nil {
		return nil
	}
	if payload.Questions == nil {
		return nil
	}
	return provider.NormalizeUserInputQuestions(*payload.Questions)
}

// parseRateLimitEvent handles Claude's rate_limit_event message type.
//
// Wire shape (docs/references/claude-wire.md):
//
//	{"type":"rate_limit_event",
//	 "rate_limit_info":{"status":"allowed_warning","resetsAt":1776981600,
//	   "rateLimitType":"five_hour"|"seven_day",
//	   "utilization":0.51,"isUsingOverage":false}}
//
// Each event carries a single window. UsedPercent is sourced from the
// wire's `utilization` field (0.0–1.0) so partial fills render
// correctly; WindowMins is derived from rateLimitType so the UI can
// key off it without re-deriving from the string. `utilization` is
// occasionally omitted (e.g. some "allowed" events with overage
// metadata) — Go's zero-value gives UsedPercent=0 in that case, which
// is the right empty-ring rendering.
func parseRateLimitEvent(threadID string, raw map[string]json.RawMessage, now time.Time) ([]provider.ProviderEvent, error) {
	infoRaw, ok := raw["rate_limit_info"]
	if !ok {
		return nil, nil
	}

	var info struct {
		ResetsAt      int64   `json:"resetsAt"`
		RateLimitType string  `json:"rateLimitType"`
		Utilization   float64 `json:"utilization"`
	}
	if json.Unmarshal(infoRaw, &info) != nil {
		return nil, nil
	}

	entry := provider.RateLimitEntry{
		LimitID:     info.RateLimitType,
		LimitName:   info.RateLimitType,
		UsedPercent: info.Utilization * 100,
		WindowMins:  windowMinsForRateLimitType(info.RateLimitType),
		ResetsAt:    info.ResetsAt,
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

// windowMinsForRateLimitType maps Claude's `rateLimitType` enum onto
// the window length so RateLimitEntry.WindowMins matches the Codex
// shape (where the wire carries `windowDurationMins` directly).
// Returns 0 for unknown types — callers treat 0 as "don't render".
func windowMinsForRateLimitType(rateLimitType string) int {
	switch rateLimitType {
	case "five_hour":
		return 300
	case "seven_day":
		return 10080
	default:
		return 0
	}
}
