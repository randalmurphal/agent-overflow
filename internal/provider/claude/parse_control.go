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
//
// A SUBAGENT's ask reaches the client through this same envelope, with
// `request.agent_id` set and NO `parent_tool_use_id` anywhere on it
// (claude-wire.md §"Subagent approvals carry `agent_id`", spiked
// 2026-08-22 on 2.1.237). `agent_id` is the asking agent's TASK id — the
// same id the §E5 async ack calls `agentId` — so the parser's
// task_id ↔ tool_use_id map resolves it to the LAUNCH tool_use, which is
// what nests the approval row under the agent's card. This is why the
// function is a *Parser method: it is the only correlation state that can
// answer "which agent asked". An id this parser cannot resolve (it
// reconnected mid-agent) leaves the scope empty rather than guessing —
// triage falls back to the requested tool's own persisted row scope.
func (p *Parser) parseControlRequest(threadID string, raw map[string]json.RawMessage, now time.Time, line []byte) ([]provider.ProviderEvent, error) {
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
		AgentID               string          `json:"agent_id"`
		AgentIDCamel          string          `json:"agentId"`
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

	// Subagent scope: agent_id == the asking agent's task id.
	agentID := req.AgentID
	if agentID == "" {
		agentID = req.AgentIDCamel
	}
	parentToolUseID := p.taskToolUseRef(agentID).ToolUseID

	if req.ToolName == "AskUserQuestion" {
		questions := parseAskUserQuestions(req.Input)
		if len(questions) > 0 {
			meta, _ := json.Marshal(provider.UserInputRequest{
				RequestID:       requestID,
				ThreadID:        threadID,
				ToolUseID:       toolUseID,
				ParentToolUseID: parentToolUseID,
				ToolName:        req.ToolName,
				Title:           "User Input Required",
				Questions:       questions,
			})
			return []provider.ProviderEvent{{
				Kind:            provider.EventUserInputRequest,
				ThreadID:        threadID,
				ItemID:          requestID,
				Meta:            meta,
				ParentToolUseID: parentToolUseID,
				Timestamp:       now,
				Raw:             line,
			}}, nil
		}
	}

	approvalMeta, _ := json.Marshal(provider.ApprovalRequest{
		RequestID:             requestID,
		ThreadID:              threadID,
		ToolUseID:             toolUseID,
		ParentToolUseID:       parentToolUseID,
		ToolName:              req.ToolName,
		Description:           fmt.Sprintf("Allow %s?", req.ToolName),
		Input:                 req.Input,
		Title:                 req.ToolName,
		PermissionSuggestions: req.PermissionSuggestions,
	})

	return []provider.ProviderEvent{{
		Kind:            provider.EventApprovalRequest,
		ThreadID:        threadID,
		ItemID:          requestID,
		Meta:            approvalMeta,
		ParentToolUseID: parentToolUseID,
		Timestamp:       now,
		Raw:             line,
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
