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
