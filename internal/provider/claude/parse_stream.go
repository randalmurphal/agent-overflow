// Package claude — parser for `stream_event` envelope NDJSON lines
// (incremental text/tool deltas streamed between assistant-message
// boundaries).

package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/sessionimport"
)

// tryParseStreamEventLine is the zero-map fast path for `stream_event`
// NDJSON lines (the >90% streaming hot path). Returns handled=false when
// the line is not actually a `stream_event` envelope so ParseLine falls
// back to the general decoder.
func (p *Parser) tryParseStreamEventLine(threadID string, line []byte) ([]provider.ProviderEvent, bool, error) {
	// Ultra-hot single-unmarshal path: content_block_delta carrying `event`
	// directly (no `data` wrapper).
	if bytes.Contains(line, []byte(`"content_block_delta"`)) && !bytes.Contains(line, []byte(`"data"`)) {
		var fast struct {
			Type            string `json:"type"`
			ParentToolUseID string `json:"parent_tool_use_id"`
			Event           struct {
				Type  string `json:"type"`
				Index int    `json:"index"`
				Delta struct {
					Type      string `json:"type"`
					Text      string `json:"text"`
					Thinking  string `json:"thinking"`
					Signature string `json:"signature"`
				} `json:"delta"`
			} `json:"event"`
		}
		if err := json.Unmarshal(line, &fast); err == nil &&
			fast.Type == "stream_event" && fast.Event.Type == "content_block_delta" {
			now := time.Now()
			switch fast.Event.Delta.Type {
			case "text_delta":
				if fast.Event.Delta.Text == "" {
					return nil, true, nil
				}
				return []provider.ProviderEvent{{
					Kind:            provider.EventTextDelta,
					ThreadID:        threadID,
					Content:         fast.Event.Delta.Text,
					Role:            "assistant",
					ParentToolUseID: fast.ParentToolUseID,
					Timestamp:       now,
				}}, true, nil
			case "thinking_delta":
				if fast.Event.Delta.Thinking == "" {
					return nil, true, nil
				}
				return []provider.ProviderEvent{{
					Kind:            provider.EventThinking,
					ThreadID:        threadID,
					Content:         fast.Event.Delta.Thinking,
					ParentToolUseID: fast.ParentToolUseID,
					Timestamp:       now,
				}}, true, nil
			case "signature_delta":
				p.appendStreamSignature(fast.ParentToolUseID, fast.Event.Index, fast.Event.Delta.Signature)
				return nil, true, nil
			default:
				return nil, true, nil
			}
		}
	}

	var env struct {
		Type            string          `json:"type"`
		ParentToolUseID string          `json:"parent_tool_use_id"`
		APIMessageID    string          `json:"api_message_id"`
		Data            json.RawMessage `json:"data"`
		Event           json.RawMessage `json:"event"`
		AbandonedBlocks json.RawMessage `json:"abandoned_blocks"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return nil, true, fmt.Errorf("invalid JSON: %w", err)
	}
	if env.Type != "stream_event" {
		return nil, false, nil
	}

	now := time.Now()
	eventRaw := env.Data
	if len(eventRaw) == 0 {
		eventRaw = env.Event
	}
	if len(eventRaw) == 0 {
		return nil, true, nil
	}

	var inner struct {
		Type         string          `json:"type"`
		Index        int             `json:"index"`
		Message      json.RawMessage `json:"message"`
		ContentBlock struct {
			Type      string `json:"type"`
			Signature string `json:"signature"`
		} `json:"content_block"`
		Delta json.RawMessage `json:"delta"`
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(eventRaw, &inner) != nil {
		return nil, true, nil
	}

	eventType := inner.Type
	if eventType == "" {
		eventType = readRawString(env.Event)
	}
	if eventType == "" {
		return nil, true, nil
	}

	parentToolUseID := env.ParentToolUseID
	if p != nil && parentToolUseID == "" && len(env.Event) > 0 {
		if len(env.Data) == 0 {
			if inner.Type == "message_start" {
				p.betweenTurns = false
			}
		} else {
			var ev struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(env.Event, &ev) == nil && ev.Type == "message_start" {
				p.betweenTurns = false
			}
		}
	}

	apiMessageIDFor := func() string {
		if env.APIMessageID != "" {
			return env.APIMessageID
		}
		if p != nil {
			return p.streamMessageIDs[parentToolUseID]
		}
		return ""
	}

	switch eventType {
	case "message_start":
		var msg struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(inner.Message, &msg) == nil {
			p.rememberStreamedMessageID(msg.ID)
			p.rememberStreamMessageID(parentToolUseID, firstNonEmpty(env.APIMessageID, msg.ID))
		}
		p.startAdvisorMessage(parentToolUseID)
		return p.startUsageMessage(threadID, parentToolUseID, inner.Message, now), true, nil

	case "content_block_start":
		if inner.ContentBlock.Type == "" {
			return nil, true, nil
		}
		p.rememberStreamBlock(parentToolUseID, inner.Index, inner.ContentBlock.Type)
		p.appendStreamSignature(parentToolUseID, inner.Index, inner.ContentBlock.Signature)
		return []provider.ProviderEvent{{
			Kind:            provider.EventContentBlockStart,
			ThreadID:        threadID,
			Meta:            blockMeta(inner.Index, inner.ContentBlock.Type, apiMessageIDFor()),
			ParentToolUseID: parentToolUseID,
			Timestamp:       now,
		}}, true, nil

	case "content_block_stop":
		blockType, signature := p.takeStreamBlock(parentToolUseID, inner.Index)
		return []provider.ProviderEvent{{
			Kind:            provider.EventContentBlockStop,
			ThreadID:        threadID,
			Meta:            settledBlockMeta(inner.Index, blockType, apiMessageIDFor(), sessionimport.ThinkingIsNarration(signature)),
			ParentToolUseID: parentToolUseID,
			Timestamp:       now,
		}}, true, nil

	case "message_stop":
		defer p.forgetStreamMessageID(parentToolUseID)
		p.forgetStreamSignatures(parentToolUseID)
		if len(env.AbandonedBlocks) == 0 {
			return nil, true, nil
		}
		var abandoned struct {
			APIMessageID   string `json:"api_message_id"`
			FromBlockIndex int    `json:"from_block_index"`
		}
		if json.Unmarshal(env.AbandonedBlocks, &abandoned) != nil {
			return nil, true, nil
		}
		apiMessageID := firstNonEmpty(abandoned.APIMessageID, apiMessageIDFor())
		if apiMessageID == "" || abandoned.FromBlockIndex < 0 {
			return nil, true, nil
		}
		meta, _ := json.Marshal(provider.BlocksAbandonedMeta{
			APIMessageID:   apiMessageID,
			FromBlockIndex: abandoned.FromBlockIndex,
		})
		return []provider.ProviderEvent{{
			Kind:            provider.EventBlocksAbandoned,
			ThreadID:        threadID,
			Meta:            meta,
			ParentToolUseID: parentToolUseID,
			Timestamp:       now,
		}}, true, nil

	case "content_block_delta":
		if len(inner.Delta) == 0 {
			return nil, true, nil
		}
		var delta struct {
			Type      string `json:"type"`
			Text      string `json:"text,omitempty"`
			Thinking  string `json:"thinking,omitempty"`
			Signature string `json:"signature,omitempty"`
		}
		if json.Unmarshal(inner.Delta, &delta) != nil {
			return nil, true, nil
		}
		switch delta.Type {
		case "text_delta":
			if delta.Text == "" {
				return nil, true, nil
			}
			return []provider.ProviderEvent{{
				Kind:            provider.EventTextDelta,
				ThreadID:        threadID,
				Content:         delta.Text,
				Role:            "assistant",
				ParentToolUseID: parentToolUseID,
				Timestamp:       now,
			}}, true, nil
		case "thinking_delta":
			if delta.Thinking == "" {
				return nil, true, nil
			}
			return []provider.ProviderEvent{{
				Kind:            provider.EventThinking,
				ThreadID:        threadID,
				Content:         delta.Thinking,
				ParentToolUseID: parentToolUseID,
				Timestamp:       now,
			}}, true, nil
		case "signature_delta":
			p.appendStreamSignature(parentToolUseID, inner.Index, delta.Signature)
			return nil, true, nil
		default:
			return nil, true, nil
		}

	case "message_delta":
		var events []provider.ProviderEvent
		if len(inner.Usage) > 0 {
			var u assistantUsage
			if json.Unmarshal(inner.Usage, &u) == nil {
				if p != nil && parentToolUseID == "" {
					events = append(events, p.reportMessageUsage(threadID, p.usageProgress.activeMessage, "", &u, now)...)
				}
				events = appendContextUsageEvent(events, threadID, parentToolUseID, now, u)
				events = p.stampAdvisorModels(events, threadID, parentToolUseID, &u, now)
			}
		}
		if soft := p.buildSoftTurnComplete(threadID, parentToolUseID, inner.Delta, now); soft != nil {
			events = append(events, *soft)
		}
		return events, true, nil

	default:
		return nil, true, nil
	}
}

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
	case "message_start":
		// Record the message id so the coalesced `assistant` snapshot
		// carrying the same id is recognised as already-streamed and its
		// text/thinking blocks are dropped (anti-double-render). A snapshot
		// whose id was never seen here never streamed and is recovered as a
		// completed block — see parse_assistant.go and the streamedMessageIDs
		// field doc. No downstream event: message_start carries no content.
		var msg struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(eventObj["message"], &msg) == nil {
			p.rememberStreamedMessageID(msg.ID)
			p.rememberStreamMessageID(parentToolUseID, firstNonEmpty(readRawString(raw["api_message_id"]), msg.ID))
		}
		p.startAdvisorMessage(parentToolUseID)
		return p.startUsageMessage(threadID, parentToolUseID, eventObj["message"], now), nil

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
		// The streamed block opens with an empty signature that
		// signature_delta fills; a start that already carries one seeds it.
		p.appendStreamSignature(parentToolUseID, index, readRawString(block["signature"]))
		return []provider.ProviderEvent{{
			Kind:            provider.EventContentBlockStart,
			ThreadID:        threadID,
			Meta:            blockMeta(index, blockType, p.streamAPIMessageID(raw, parentToolUseID)),
			ParentToolUseID: parentToolUseID,
			Timestamp:       now,
		}}, nil

	case "content_block_stop":
		index, _ := readIntAtAnyKey(eventRaw, "index")
		blockType, signature := p.takeStreamBlock(parentToolUseID, index)
		return []provider.ProviderEvent{{
			Kind:            provider.EventContentBlockStop,
			ThreadID:        threadID,
			Meta:            settledBlockMeta(index, blockType, p.streamAPIMessageID(raw, parentToolUseID), sessionimport.ThinkingIsNarration(signature)),
			ParentToolUseID: parentToolUseID,
			Timestamp:       now,
		}}, nil

	case "message_stop":
		// The CLI closes a response it stopped reading (stalled or failed
		// stream) with a message_stop of its own whose envelope carries
		// `abandoned_blocks`: the response's blocks from that index on
		// were streamed but will never get an assistant message and are
		// not in the transcript (claude-wire.md §abandoned_blocks). The
		// API's own message_stop carries no such key and emits nothing.
		defer p.forgetStreamMessageID(parentToolUseID)
		p.forgetStreamSignatures(parentToolUseID)
		return p.blocksAbandonedEvent(threadID, raw, parentToolUseID, now), nil

	case "content_block_delta":
		deltaRaw, ok := eventObj["delta"]
		if !ok {
			return nil, nil
		}
		var delta struct {
			Type      string `json:"type"`
			Text      string `json:"text,omitempty"`
			Thinking  string `json:"thinking,omitempty"`
			Signature string `json:"signature,omitempty"`
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
		case "signature_delta":
			// The signature is never content. It is kept until the block's
			// stop only to read its tag (settledBlockMeta).
			index, _ := readIntAtAnyKey(eventRaw, "index")
			p.appendStreamSignature(parentToolUseID, index, delta.Signature)
			return nil, nil
		default:
			return nil, nil
		}
	case "message_delta":
		// `message_delta` can emit two events from one envelope: an
		// EventTokenUsage from the `usage` snapshot (drives the
		// context meter) AND a soft EventTurnComplete from the inner
		// `delta.stop_reason` (drives the working indicator off when
		// the parent stops emitting, without waiting for the wire
		// `result` — docs/architecture/turn-lifecycle.md §Rounds and
		// turns). The two gates are
		// independent; a malformed envelope missing one field
		// must still let the other fire.
		//
		// The top-level `usage` fields are the cumulative sum of every
		// parent `type:"message"` iteration in this SSE message. That
		// sum — NOT the last iteration's snapshot — is what the CLI's
		// own auto-compact trigger uses; verified across five
		// production compactions where `compactMetadata.preTokens`
		// matched top-level within 1-2% and was ~2x the last-iteration
		// snapshot on advisor turns. See
		// internal/provider/wirecaptures/claude/advisor_pretokens_correlation_20260523.summary.json
		// and docs/references/claude-wire.md §Context-window usage.
		var events []provider.ProviderEvent
		if usageRaw := eventObj["usage"]; len(usageRaw) > 0 {
			var u assistantUsage
			if json.Unmarshal(usageRaw, &u) == nil {
				if p != nil && parentToolUseID == "" {
					events = append(events, p.reportMessageUsage(threadID, p.usageProgress.activeMessage, "", &u, now)...)
				}
				events = appendContextUsageEvent(events, threadID, parentToolUseID, now, u)
				events = p.stampAdvisorModels(events, threadID, parentToolUseID, &u, now)
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

// blockMeta builds the {index, blockType, apiMessageId} JSON meta carried
// by content-block lifecycle events (content_block_start /
// content_block_stop) and by the snapshot-recovery EventContentBlockStop
// (parse_assistant.go). triage reads "blockType" from it via
// blockTypeForStop, and stamps "index" and "apiMessageId" onto the block's
// row at content_block_start so an abandoned_blocks message_stop can name
// the rows it discards; apiMessageId is omitted when unknown.
func blockMeta(index int, blockType, apiMessageID string) json.RawMessage {
	return settledBlockMeta(index, blockType, apiMessageID, false)
}

// settledBlockMeta is blockMeta for a block's stop. narration reports that
// a thinking block's signature tags it as prose for the user
// (sessionimport.ThinkingTag); the stop then carries
// provider.MetaNarrationKey, and an untagged stop is byte-identical to
// blockMeta.
func settledBlockMeta(index int, blockType, apiMessageID string, narration bool) json.RawMessage {
	fields := map[string]any{
		"index":     index,
		"blockType": blockType,
	}
	if apiMessageID != "" {
		fields[provider.MetaAPIMessageIDKey] = apiMessageID
	}
	if narration && blockType == "thinking" {
		fields[provider.MetaNarrationKey] = true
	}
	meta, _ := json.Marshal(fields)
	return meta
}

// blocksAbandonedEvent reads a message_stop envelope's `abandoned_blocks`
// into an EventBlocksAbandoned, or nil when the key is absent or does not
// name a response. The key is an envelope sibling of `event`, not a field
// of the inner event. ParentToolUseID passes through unchanged: the CLI
// emits the key only for the top-level stream today (its close events
// hardcode a null parent), and a scoped signal would mark that scope's
// rows the same way.
func (p *Parser) blocksAbandonedEvent(threadID string, raw map[string]json.RawMessage, parentToolUseID string, now time.Time) []provider.ProviderEvent {
	abandonedRaw := raw["abandoned_blocks"]
	if len(abandonedRaw) == 0 {
		return nil
	}
	var abandoned struct {
		APIMessageID   string `json:"api_message_id"`
		FromBlockIndex int    `json:"from_block_index"`
	}
	if json.Unmarshal(abandonedRaw, &abandoned) != nil {
		return nil
	}
	apiMessageID := firstNonEmpty(abandoned.APIMessageID, p.streamAPIMessageID(raw, parentToolUseID))
	if apiMessageID == "" || abandoned.FromBlockIndex < 0 {
		return nil
	}
	meta, _ := json.Marshal(provider.BlocksAbandonedMeta{
		APIMessageID:   apiMessageID,
		FromBlockIndex: abandoned.FromBlockIndex,
	})
	return []provider.ProviderEvent{{
		Kind:            provider.EventBlocksAbandoned,
		ThreadID:        threadID,
		Meta:            meta,
		ParentToolUseID: parentToolUseID,
		Timestamp:       now,
	}}
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

// IsSoftRoundCloseStopReason is the exported seam for the interactive (TUI)
// provider, which reconstructs a turn-closing `result` envelope on exactly this
// stop_reason set. Sharing the predicate keeps the two Claude transports from
// drifting on "the model is done with the user's request" — a stop_reason added
// to one would otherwise silently diverge the other. See isSoftRoundCloseStopReason.
func IsSoftRoundCloseStopReason(s string) bool {
	return isSoftRoundCloseStopReason(s)
}
