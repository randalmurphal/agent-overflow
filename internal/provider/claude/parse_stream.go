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
	default:
		return nil, nil
	}
}
