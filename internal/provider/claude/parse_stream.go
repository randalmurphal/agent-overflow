// Package claude — parser for `stream_event` envelope NDJSON lines
// (incremental text/tool deltas streamed between assistant-message
// boundaries).

package claude

import (
	"encoding/json"
	"time"

	"agent-overflow/internal/provider"
)

// parseStreamEvent handles the `stream_event` envelope produced by the
// Claude CLI when `--include-partial-messages` is enabled. Unlike the
// assistant-message path, partial messages preserve content block
// boundaries, so we emit explicit start/stop events for text/thinking blocks
// and remember the block type by (parent_tool_use_id,index) so a later stop
// can settle the right streaming item.
func (p *Parser) parseStreamEvent(threadID string, raw map[string]json.RawMessage, now time.Time) ([]provider.ProviderEvent, error) {
	eventRaw := raw["data"]
	if len(eventRaw) == 0 {
		eventRaw = raw["event"]
	}
	if len(eventRaw) == 0 {
		return nil, nil
	}

	var eventObj map[string]json.RawMessage
	if json.Unmarshal(eventRaw, &eventObj) != nil {
		return nil, nil
	}

	eventType := firstNonEmpty(readRawString(eventObj["type"]), readRawString(raw["event"]))
	if eventType == "" {
		return nil, nil
	}

	parentToolUseID := readRawString(raw["parent_tool_use_id"])

	switch eventType {
	case "content_block_start":
		index, _ := readIntAtAnyKey(eventRaw, "index")
		var block map[string]json.RawMessage
		if json.Unmarshal(eventObj["content_block"], &block) != nil {
			return nil, nil
		}
		blockType := readRawString(block["type"])
		if blockType == "" {
			return nil, nil
		}
		p.rememberStreamBlock(parentToolUseID, index, blockType)
		meta, _ := json.Marshal(map[string]any{
			"index":         index,
			"blockType":     blockType,
			"content_block": json.RawMessage(eventObj["content_block"]),
		})
		return []provider.ProviderEvent{{
			Kind:            provider.EventContentBlockStart,
			ThreadID:        threadID,
			Meta:            meta,
			ParentToolUseID: parentToolUseID,
			Timestamp:       now,
		}}, nil

	case "content_block_stop":
		index, _ := readIntAtAnyKey(eventRaw, "index")
		blockType := p.takeStreamBlock(parentToolUseID, index)
		meta, _ := json.Marshal(map[string]any{
			"index":     index,
			"blockType": blockType,
		})
		return []provider.ProviderEvent{{
			Kind:            provider.EventContentBlockStop,
			ThreadID:        threadID,
			Meta:            meta,
			ParentToolUseID: parentToolUseID,
			Timestamp:       now,
		}}, nil

	case "content_block_delta":
		deltaRaw, ok := eventObj["delta"]
		if !ok {
			return nil, nil
		}
		var delta struct {
			Type     string `json:"type"`
			Text     string `json:"text,omitempty"`
			Thinking string `json:"thinking,omitempty"`
		}
		if json.Unmarshal(deltaRaw, &delta) != nil {
			return nil, nil
		}
		switch delta.Type {
		case "text_delta":
			if delta.Text == "" {
				return nil, nil
			}
			return []provider.ProviderEvent{{
				Kind:            provider.EventTextDelta,
				ThreadID:        threadID,
				Content:         delta.Text,
				Role:            "assistant",
				ParentToolUseID: parentToolUseID,
				Timestamp:       now,
			}}, nil
		case "thinking_delta":
			if delta.Thinking == "" {
				return nil, nil
			}
			return []provider.ProviderEvent{{
				Kind:            provider.EventThinking,
				ThreadID:        threadID,
				Content:         delta.Thinking,
				ParentToolUseID: parentToolUseID,
				Timestamp:       now,
			}}, nil
		default:
			return nil, nil
		}
	case "message_delta":
		// `message_delta` is the only stream_event case that can emit
		// two events from one envelope: an EventTokenUsage from the
		// `usage` snapshot (drives the context meter) AND a soft
		// EventTurnComplete from the inner `delta.stop_reason` (drives
		// the working indicator off when the parent stops emitting,
		// without waiting for the wire `result` — see invariants.md §27).
		var events []provider.ProviderEvent
		if usageRaw := eventObj["usage"]; len(usageRaw) > 0 {
			var usage assistantUsage
			if json.Unmarshal(usageRaw, &usage) == nil {
				events = appendContextUsageEvent(events, threadID, parentToolUseID, now, usage)
			}
		}
		if soft := p.buildSoftTurnComplete(threadID, parentToolUseID, eventObj["delta"], now); soft != nil {
			events = append(events, *soft)
		}
		return events, nil
	default:
		return nil, nil
	}
}

// buildSoftTurnComplete inspects a top-level `message_delta` envelope
// and emits a soft EventTurnComplete when the inner
// stop_reason is one of the closed-set "model has stopped" values:
// `end_turn`, `stop_sequence`, `refusal`. Returns nil otherwise —
// stop_reasons like `tool_use` / `pause_turn` / `max_tokens` mean the
// model is NOT done (more text follows the tool_result, the model
// asked for more time, or the harness will auto-continue), and
// unknown / future SDK values fall through nil too. Under-firing on
// an unknown is the safer failure mode: the trailing wire `result`
// envelope still settles the turn correctly.
//
// Returns nil for malformed envelopes too — empty/missing `delta`,
// non-object `delta`, or `parent_tool_use_id != ""` (subagent
// messages have their own end_turn signals that must not close the
// parent's round). A well-formed CLI never produces these shapes for
// a parent message_delta, but the parser tolerates them.
//
// The typed payload carries `stop_reason` and the parser's peeked
// `assistant_message_id` (peek, not take — the trailing wire `result`
// envelope still calls takeLastAssistantMessageID for its own emission).
// Including the id on the soft event keeps the FIRST settle's persisted
// row populated; the trailing result may overwrite it via the late-payload
// last-non-empty fold.
func (p *Parser) buildSoftTurnComplete(threadID, parentToolUseID string, deltaRaw json.RawMessage, now time.Time) *provider.ProviderEvent {
	if parentToolUseID != "" {
		return nil
	}
	if len(deltaRaw) == 0 {
		return nil
	}
	var delta struct {
		StopReason string `json:"stop_reason"`
	}
	if json.Unmarshal(deltaRaw, &delta) != nil {
		return nil
	}
	if !isSoftRoundCloseStopReason(delta.StopReason) {
		return nil
	}
	return &provider.ProviderEvent{
		Kind:     provider.EventTurnComplete,
		ThreadID: threadID,
		TurnComplete: &provider.SoftRoundCloseMeta{
			StopReason:         delta.StopReason,
			AssistantMessageID: p.peekLastAssistantMessageID(),
		},
		Timestamp: now,
	}
}

// isSoftRoundCloseStopReason reports whether the parent's message-level
// stop_reason indicates the model has truly stopped emitting for this
// round. The closed set is documented in claude-wire.md §Soft round
// close. tool_use / pause_turn / max_tokens are deliberately excluded
// — the model is not done in those cases.
func isSoftRoundCloseStopReason(s string) bool {
	switch s {
	case "end_turn", "stop_sequence", "refusal":
		return true
	}
	return false
}
