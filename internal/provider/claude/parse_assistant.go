// Package claude — parser for `assistant`-type NDJSON lines. The top-level
// parseAssistant dispatches each content block to a per-type helper
// (appendTextEvent / appendToolUseEvent / appendThinkingEvent /
// appendExitPlanModeEvent) so new block types can be added without growing
// the main function.

package claude

import (
	"encoding/json"
	"fmt"
	"time"

	"agent-overflow/internal/provider"
)

// assistantContentBlock is the subset of fields every block type on an
// `assistant.message.content` entry carries. The decoded blocks are fed
// into the appendX helpers below; each helper reads the fields relevant
// to its block.Type and ignores the rest.
type assistantContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
}

// assistantUsage mirrors the usage object Claude attaches to
// `assistant.message` lines. Split out so the usage branch of
// parseAssistant can rewrite it into a provider.TokenUsage without the
// rest of the message struct leaking into the helper.
type assistantUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

type assistantMessage struct {
	ID      string                  `json:"id"`
	Content []assistantContentBlock `json:"content"`
	Role    string                  `json:"role"`
	Usage   *assistantUsage         `json:"usage,omitempty"`
}

func (p *Parser) parseAssistant(threadID string, raw map[string]json.RawMessage, now time.Time, line []byte) ([]provider.ProviderEvent, error) {
	var msg assistantMessage

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

	// Track the final assistant message id at the session level so the
	// eventual `result` envelope (which does NOT carry this id) can
	// emit it on EventTurnComplete.Meta.assistant_message_id. Only
	// top-level assistant messages qualify — subagent Task messages
	// carry `parent_tool_use_id`, and the final-text label we want
	// here is the parent thread's. See
	// docs/references/claude-wire.md §assistant.
	if parentToolUseID == "" {
		p.setLastAssistantMessageID(msg.ID)
	}

	var events []provider.ProviderEvent

	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			// Text content is already emitted delta-by-delta via the
			// stream_event path (`parse_stream.go`) — `--include-partial-
			// messages` is always on for our session. Emitting again
			// from the coalesced assistant envelope would append the
			// full block on top of the streamed partials, doubling the
			// text in the cumulative summary. This manifested as a
			// user-visible rendering bug (mermaid diagrams appearing
			// twice) after the closure gate started actually rendering
			// their content.
		case "tool_use":
			events = p.appendToolUseEvent(events, threadID, parentToolUseID, now, block)
		case "thinking":
			// Same as text: streamed via stream_event thinking_delta.
		}
	}

	events = appendUsageEvent(events, threadID, parentToolUseID, now, msg.Usage, p.currentModel())
	return events, nil
}

// appendTextEvent emits a streaming text delta for a `text` block. The
// message-level id (msg.ID) is used as the item id so every text chunk in
// the same assistant message collapses onto one timeline row.
func appendTextEvent(
	events []provider.ProviderEvent,
	threadID, itemID, parentToolUseID string,
	now time.Time,
	block assistantContentBlock,
) []provider.ProviderEvent {
	return append(events, provider.ProviderEvent{
		Kind:            provider.EventTextDelta,
		ThreadID:        threadID,
		ItemID:          itemID,
		Content:         block.Text,
		Role:            "assistant",
		ParentToolUseID: parentToolUseID,
		Timestamp:       now,
	})
}

// appendToolUseEvent handles `tool_use` blocks. ExitPlanMode takes a
// different path (EventProposedPlan); every other tool call emits an
// EventToolStart and — when the input says so — registers the tool as
// backgrounded so the later user.tool_result echo can be suppressed.
func (p *Parser) appendToolUseEvent(
	events []provider.ProviderEvent,
	threadID, parentToolUseID string,
	now time.Time,
	block assistantContentBlock,
) []provider.ProviderEvent {
	if block.Name == "ExitPlanMode" {
		return appendExitPlanModeEvent(events, threadID, parentToolUseID, now, block)
	}

	isBackground := hasRunInBackground(block.Input)
	if isBackground {
		p.markBackground(block.ID)
	}

	meta := marshalToolMeta(block.Name, block.Input, isBackground)
	return append(events, provider.ProviderEvent{
		Kind:            provider.EventToolStart,
		ThreadID:        threadID,
		ItemID:          block.ID,
		ItemType:        block.Name,
		Meta:            meta,
		ParentToolUseID: parentToolUseID,
		Timestamp:       now,
	})
}

// appendExitPlanModeEvent converts an ExitPlanMode tool call into an
// EventProposedPlan. A missing plan body drops the event rather than emit
// an empty plan the frontend would render as "no content".
func appendExitPlanModeEvent(
	events []provider.ProviderEvent,
	threadID, parentToolUseID string,
	now time.Time,
	block assistantContentBlock,
) []provider.ProviderEvent {
	planMarkdown := extractExitPlanModePlan(block.Input)
	if planMarkdown == "" {
		return events
	}
	return append(events, provider.ProviderEvent{
		Kind:            provider.EventProposedPlan,
		ThreadID:        threadID,
		ItemID:          block.ID,
		ItemType:        block.Name,
		Content:         planMarkdown,
		ParentToolUseID: parentToolUseID,
		Timestamp:       now,
	})
}

// appendThinkingEvent emits a thinking block, optionally carrying the
// cryptographic signature Claude attaches when it's enabled. The
// signature is marshaled into meta rather than stamped onto the Content
// so the frontend can rely on Content being plain text.
func appendThinkingEvent(
	events []provider.ProviderEvent,
	threadID, itemID, parentToolUseID string,
	now time.Time,
	block assistantContentBlock,
) []provider.ProviderEvent {
	var meta json.RawMessage
	if block.Signature != "" {
		meta, _ = json.Marshal(map[string]any{
			"signature": block.Signature,
		})
	}
	return append(events, provider.ProviderEvent{
		Kind:            provider.EventThinking,
		ThreadID:        threadID,
		ItemID:          itemID,
		Content:         block.Thinking,
		Meta:            meta,
		ParentToolUseID: parentToolUseID,
		Timestamp:       now,
	})
}

// appendUsageEvent emits an EventTokenUsage when the assistant message
// carries a usage object. Nil usage (the common mid-stream case) drops
// through without touching the event slice. When `model` is non-empty
// we price the usage via provider.CalculateCost and stamp the result on
// TotalCostUSD before marshaling — the triage router trusts the meta
// verbatim, so the cost is attached at the provider boundary where the
// model is authoritatively known.
func appendUsageEvent(
	events []provider.ProviderEvent,
	threadID, parentToolUseID string,
	now time.Time,
	usage *assistantUsage,
	model string,
) []provider.ProviderEvent {
	if usage == nil {
		return events
	}
	tokenUsage := provider.TokenUsage{
		InputTokens:              usage.InputTokens,
		OutputTokens:             usage.OutputTokens,
		CacheReadInputTokens:     usage.CacheReadInputTokens,
		CacheCreationInputTokens: usage.CacheCreationInputTokens,
	}
	if model != "" {
		tokenUsage.TotalCostUSD = provider.CalculateCost(model, tokenUsage)
	}
	usageMeta, _ := json.Marshal(tokenUsage)
	return append(events, provider.ProviderEvent{
		Kind:            provider.EventTokenUsage,
		ThreadID:        threadID,
		Meta:            usageMeta,
		ParentToolUseID: parentToolUseID,
		Timestamp:       now,
	})
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
