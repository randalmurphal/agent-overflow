package claude

import (
	"encoding/json"
	"fmt"
	"time"

	"agent-overflow/internal/provider"
)

func (p *Parser) parseAssistant(threadID string, raw map[string]json.RawMessage, now time.Time, line []byte) ([]provider.ProviderEvent, error) {
	var msg struct {
		ID      string `json:"id"`
		Content []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text,omitempty"`
			ID        string          `json:"id,omitempty"`
			Name      string          `json:"name,omitempty"`
			Input     json.RawMessage `json:"input,omitempty"`
			Thinking  string          `json:"thinking,omitempty"`
			Signature string          `json:"signature,omitempty"`
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
	// Silence the unused-line warning. `line` is kept on the signature to
	// match the other parse* helpers so the top-level switch in ParseLine
	// stays uniform.
	_ = line

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

			isBackground := hasRunInBackground(block.Input)
			if isBackground {
				p.markBackground(block.ID)
			}

			meta := marshalToolMeta(block.Name, block.Input, isBackground)
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
			var meta json.RawMessage
			if block.Signature != "" {
				meta, _ = json.Marshal(map[string]any{
					"signature": block.Signature,
				})
			}
			events = append(events, provider.ProviderEvent{
				Kind:            provider.EventThinking,
				ThreadID:        threadID,
				ItemID:          msg.ID,
				Content:         block.Thinking,
				Meta:            meta,
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

// hasRunInBackground returns true when the tool input JSON contains
// `"run_in_background": true`. Malformed JSON is treated as absent —
// this is a best-effort hint, not a correctness-critical value.
func hasRunInBackground(input json.RawMessage) bool {
	if len(input) == 0 {
		return false
	}
	var parsed struct {
		RunInBackground bool `json:"run_in_background"`
	}
	if err := json.Unmarshal(input, &parsed); err != nil {
		return false
	}
	return parsed.RunInBackground
}

// marshalToolMeta builds the EventToolStart Meta payload. We omit
// `is_background` when false so pipelines downstream don't have to
// distinguish "explicitly foreground" from "unknown" — absence is the
// default.
func marshalToolMeta(toolName string, input json.RawMessage, isBackground bool) json.RawMessage {
	fields := map[string]any{
		"toolName": toolName,
		"input":    input,
	}
	if isBackground {
		fields["is_background"] = true
	}
	out, _ := json.Marshal(fields)
	return out
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
