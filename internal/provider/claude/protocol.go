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
		return nil, fmt.Errorf("missing or invalid type field")
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
	default:
		// Unknown type (e.g. rate_limit_event) — skip gracefully.
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

	// Explicitly skipped subtypes — no action, no error.
	case "compact_boundary",
		"api_retry",
		"hook_started", "hook_progress", "hook_response",
		"tool_progress",
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

	var events []provider.ProviderEvent

	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			events = append(events, provider.ProviderEvent{
				Kind:      provider.EventTextDelta,
				ThreadID:  threadID,
				ItemID:    msg.ID,
				Content:   block.Text,
				Role:      "assistant",
				Timestamp: now,
			})

		case "tool_use":
			meta, _ := json.Marshal(map[string]any{
				"toolName": block.Name,
				"input":    block.Input,
			})
			events = append(events, provider.ProviderEvent{
				Kind:      provider.EventToolStart,
				ThreadID:  threadID,
				ItemID:    block.ID,
				ItemType:  block.Name,
				Meta:      meta,
				Timestamp: now,
			})

		case "thinking":
			events = append(events, provider.ProviderEvent{
				Kind:      provider.EventThinking,
				ThreadID:  threadID,
				ItemID:    msg.ID,
				Content:   block.Thinking,
				Timestamp: now,
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
			Kind:      provider.EventTokenUsage,
			ThreadID:  threadID,
			Meta:      usageMeta,
			Timestamp: now,
		})
	}

	return events, nil
}

func parseResult(threadID string, raw map[string]json.RawMessage, now time.Time, line []byte) ([]provider.ProviderEvent, error) {
	var result struct {
		IsError   bool   `json:"is_error"`
		Error     string `json:"error,omitempty"`
		SessionID string `json:"session_id,omitempty"`
	}

	data, _ := json.Marshal(raw)
	_ = json.Unmarshal(data, &result)

	if result.IsError {
		return []provider.ProviderEvent{{
			Kind:      provider.EventError,
			ThreadID:  threadID,
			Content:   result.Error,
			Timestamp: now,
			Raw:       line,
		}}, nil
	}

	return []provider.ProviderEvent{{
		Kind:      provider.EventTurnComplete,
		ThreadID:  threadID,
		Timestamp: now,
		Raw:       line,
	}}, nil
}

func parseStreamEvent(threadID string, raw map[string]json.RawMessage, now time.Time) ([]provider.ProviderEvent, error) {
	var evt struct {
		Event string `json:"event"`
		Data  struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta,omitempty"`
		} `json:"data,omitempty"`
	}

	data, _ := json.Marshal(raw)
	_ = json.Unmarshal(data, &evt)

	if evt.Data.Delta.Text != "" {
		return []provider.ProviderEvent{{
			Kind:      provider.EventTextDelta,
			ThreadID:  threadID,
			Content:   evt.Data.Delta.Text,
			Role:      "assistant",
			Timestamp: now,
		}}, nil
	}

	return nil, nil
}

// parseControlRequest handles the wire format:
// {"type":"control_request","request_id":"req_1_abc","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}
func parseControlRequest(threadID string, raw map[string]json.RawMessage, now time.Time, line []byte) ([]provider.ProviderEvent, error) {
	var msg struct {
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype  string          `json:"subtype"`
			ToolName string          `json:"tool_name"`
			Input    json.RawMessage `json:"input"`
		} `json:"request"`
	}

	data, _ := json.Marshal(raw)
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, nil
	}

	if msg.Request.Subtype != "can_use_tool" {
		return nil, nil
	}

	approvalMeta, _ := json.Marshal(provider.ApprovalRequest{
		RequestID:   msg.RequestID,
		ThreadID:    threadID,
		ToolName:    msg.Request.ToolName,
		Description: fmt.Sprintf("Allow %s?", msg.Request.ToolName),
		Input:       msg.Request.Input,
		Title:       msg.Request.ToolName,
	})

	return []provider.ProviderEvent{{
		Kind:      provider.EventApprovalRequest,
		ThreadID:  threadID,
		ItemID:    msg.RequestID,
		Meta:      approvalMeta,
		Timestamp: now,
		Raw:       line,
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
