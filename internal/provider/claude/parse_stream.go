// Package claude — parser for `stream_event` envelope NDJSON lines
// (incremental text/tool deltas streamed between assistant-message
// boundaries).

package claude

import (
	"bytes"
	"encoding/json"
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
			return p.emitContentBlockDelta(
				threadID,
				fast.ParentToolUseID,
				fast.Event.Index,
				fast.Event.Delta.Type,
				fast.Event.Delta.Text,
				fast.Event.Delta.Thinking,
				fast.Event.Delta.Signature,
				time.Now(),
			), true, nil
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
		return nil, false, nil
	}
	if env.Type != "stream_event" {
		return nil, false, nil
	}

	events, err := p.parseStreamEventPayload(
		threadID,
		env.ParentToolUseID,
		env.APIMessageID,
		env.Data,
		env.Event,
		env.AbandonedBlocks,
		true,
		time.Now(),
	)
	return events, true, err
}

// parseStreamEvent handles the `stream_event` envelope produced by the
// Claude CLI when `--include-partial-messages` is enabled. Unlike the
// assistant-message path, partial messages preserve content block
// boundaries, so we emit explicit start/stop events for text/thinking blocks
// and remember the block type by (parent_tool_use_id,index) so a later stop
// can settle the right streaming item.
func (p *Parser) parseStreamEvent(threadID string, raw map[string]json.RawMessage, now time.Time) ([]provider.ProviderEvent, error) {
	return p.parseStreamEventPayload(
		threadID,
		readRawString(raw["parent_tool_use_id"]),
		readRawString(raw["api_message_id"]),
		raw["data"],
		raw["event"],
		raw["abandoned_blocks"],
		false,
		now,
	)
}

func (p *Parser) emitContentBlockDelta(
	threadID, parentToolUseID string,
	index int,
	deltaType, text, thinking, signature string,
	now time.Time,
) []provider.ProviderEvent {
	switch deltaType {
	case "text_delta":
		if text == "" {
			return nil
		}
		return []provider.ProviderEvent{{
			Kind:            provider.EventTextDelta,
			ThreadID:        threadID,
			Content:         text,
			Role:            "assistant",
			ParentToolUseID: parentToolUseID,
			Timestamp:       now,
		}}
	case "thinking_delta":
		if thinking == "" {
			return nil
		}
		return []provider.ProviderEvent{{
			Kind:            provider.EventThinking,
			ThreadID:        threadID,
			Content:         thinking,
			ParentToolUseID: parentToolUseID,
			Timestamp:       now,
		}}
	case "signature_delta":
		// The signature is never content. It is kept until the block's
		// stop only to read its tag (settledBlockMeta).
		p.appendStreamSignature(parentToolUseID, index, signature)
		return nil
	default:
		return nil
	}
}

func (p *Parser) parseStreamEventPayload(
	threadID, parentToolUseID, envelopeAPIMessageID string,
	dataRaw, rawEvent, abandonedRaw json.RawMessage,
	updateTurnWindow bool,
	now time.Time,
) ([]provider.ProviderEvent, error) {
	eventRaw := dataRaw
	if len(eventRaw) == 0 {
		eventRaw = rawEvent
	}
	if len(eventRaw) == 0 {
		return nil, nil
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
		return nil, nil
	}

	eventType := inner.Type
	if eventType == "" {
		eventType = readRawString(rawEvent)
	}
	if eventType == "" {
		return nil, nil
	}

	if updateTurnWindow && p != nil && parentToolUseID == "" && len(rawEvent) > 0 {
		if len(dataRaw) == 0 {
			if inner.Type == "message_start" {
				p.betweenTurns = false
			}
		} else {
			var ev struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(rawEvent, &ev) == nil && ev.Type == "message_start" {
				p.betweenTurns = false
			}
		}
	}

	// API id of the response a stream_event belongs to: the envelope's
	// `api_message_id` when the CLI stamped one, else the id remembered from
	// the scope's `message_start`. Empty when neither is known.
	apiMessageIDFor := func() string {
		if envelopeAPIMessageID != "" {
			return envelopeAPIMessageID
		}
		if p == nil || p.streamMessageIDs == nil {
			return ""
		}
		return p.streamMessageIDs[parentToolUseID]
	}

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
		if json.Unmarshal(inner.Message, &msg) == nil {
			p.rememberStreamedMessageID(msg.ID)
			p.rememberStreamMessageID(parentToolUseID, firstNonEmpty(envelopeAPIMessageID, msg.ID))
		}
		p.startAdvisorMessage(parentToolUseID)
		return p.startUsageMessage(threadID, parentToolUseID, inner.Message, now), nil

	case "content_block_start":
		if inner.ContentBlock.Type == "" {
			return nil, nil
		}
		p.rememberStreamBlock(parentToolUseID, inner.Index, inner.ContentBlock.Type)
		// The streamed block opens with an empty signature that
		// signature_delta fills; a start that already carries one seeds it.
		p.appendStreamSignature(parentToolUseID, inner.Index, inner.ContentBlock.Signature)
		return []provider.ProviderEvent{{
			Kind:            provider.EventContentBlockStart,
			ThreadID:        threadID,
			Meta:            blockMeta(inner.Index, inner.ContentBlock.Type, apiMessageIDFor()),
			ParentToolUseID: parentToolUseID,
			Timestamp:       now,
		}}, nil

	case "content_block_stop":
		blockType, signature := p.takeStreamBlock(parentToolUseID, inner.Index)
		return []provider.ProviderEvent{{
			Kind:            provider.EventContentBlockStop,
			ThreadID:        threadID,
			Meta:            settledBlockMeta(inner.Index, blockType, apiMessageIDFor(), sessionimport.ThinkingIsNarration(signature)),
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
		if len(abandonedRaw) == 0 {
			return nil, nil
		}
		var abandoned struct {
			APIMessageID   string `json:"api_message_id"`
			FromBlockIndex int    `json:"from_block_index"`
		}
		if json.Unmarshal(abandonedRaw, &abandoned) != nil {
			return nil, nil
		}
		apiMessageID := firstNonEmpty(abandoned.APIMessageID, apiMessageIDFor())
		if apiMessageID == "" || abandoned.FromBlockIndex < 0 {
			return nil, nil
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
		}}, nil

	case "content_block_delta":
		if len(inner.Delta) == 0 {
			return nil, nil
		}
		var delta struct {
			Type      string `json:"type"`
			Text      string `json:"text,omitempty"`
			Thinking  string `json:"thinking,omitempty"`
			Signature string `json:"signature,omitempty"`
		}
		if json.Unmarshal(inner.Delta, &delta) != nil {
			return nil, nil
		}
		return p.emitContentBlockDelta(
			threadID,
			parentToolUseID,
			inner.Index,
			delta.Type,
			delta.Text,
			delta.Thinking,
			delta.Signature,
			now,
		), nil

	case "message_delta":
		// `message_delta` can emit two events from one envelope: an
		// EventTokenUsage from the `usage` snapshot (drives the
		// context meter) AND a soft EventTurnComplete from the inner
		// `delta.stop_reason` (drives the working indicator off when
		// the parent stops emitting, without waiting for the wire
		// `result` — docs/architecture/turn-lifecycle.md §Rounds and
		// turns). The two gates are independent; a malformed envelope
		// missing one field must still let the other fire.
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
