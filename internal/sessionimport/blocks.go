package sessionimport

import (
	"encoding/json"
	"fmt"
	"strings"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// blocks.go — the streaming-block family: the assistant_text and thinking
// rows both providers express differently. Claude's reader hands over
// whole blocks as deltas; Codex's hands over settled blocks as content
// block stops, which is the vocabulary the live Codex adapter uses. Both
// land on the same rows.

// extendStreamBlock appends a delta to an open block. It resolves the row
// once and hands it down: an id that is open in activeText/activeThinking
// but absent from byID is a builder bug, and asking twice would only give
// two chances to report it.
func (b *builder) extendStreamBlock(evt importir.Event, itemID, kind string, now int64) error {
	r, ok := b.byID[itemID]
	if !ok {
		return fmt.Errorf("%s block %s vanished from the batch", kind, itemID)
	}
	return b.setStreamBlockContent(evt, r, kind, now, string(r.payload.Data)+evt.Content)
}

// resolveStreamBlock looks up an open block's row by id.
func (b *builder) resolveStreamBlock(itemID, kind string) (*row, error) {
	r, ok := b.byID[itemID]
	if !ok {
		return nil, fmt.Errorf("%s block %s vanished from the batch", kind, itemID)
	}
	return r, nil
}

// setStreamBlockContent rewrites one open block's content. Its two callers
// differ only in what "full" is: a delta APPENDS to what streamed, a
// settle REPLACES it — the same split doSettleStreamingText makes when the
// stop carries final content.
func (b *builder) setStreamBlockContent(evt importir.Event, r *row, kind string, now int64, full string) error {
	r.payload.Data = []byte(full)
	r.payload.Meta = triage.BuildPayloadMeta(kind, provider.ProviderEvent{Content: full})
	if kind == kindThinking {
		r.item.Summary = triage.ThinkingSummaryPreview(full)
	} else {
		r.item.Summary = full
	}
	r.item.UpdatedAt = now
	return b.markUnavailable(evt, r)
}

// closeStreams ends the open text/thinking blocks of a turn+scope, the
// way any timeline boundary settles them live. Every later event for the
// same scope then starts a fresh block.
func (b *builder) closeStreams(turnIndex int, scope string) {
	prefix := scopeKey(turnIndex, scope)
	for _, active := range []map[string]string{b.activeText, b.activeThinking} {
		for key := range active {
			if key == prefix || strings.HasPrefix(key, prefix+"|") {
				delete(active, key)
			}
		}
	}
}

func (b *builder) assistantText(evt importir.Event) error {
	return b.streamBlock(evt, kindAssistantText)
}

func (b *builder) thinking(evt importir.Event) error {
	return b.streamBlock(evt, kindThinking)
}

// streamBlock builds (or extends) one assistant_text / thinking row. A
// live session accumulates these from deltas and settles them completed;
// an import receives whole blocks, so the row is born completed with the
// same id, payload id, summary rule and meta the settled live row has.
func (b *builder) streamBlock(evt importir.Event, kind string) error {
	if evt.Content == "" {
		return nil
	}
	now, err := timestamp(evt)
	if err != nil {
		return err
	}
	scope := strings.TrimSpace(evt.ParentToolUseID)
	turnIndex := b.turns.currentFor(evt)
	active := b.activeText
	if kind == kindThinking {
		active = b.activeThinking
	}
	key := streamKey(turnIndex, scope, strings.TrimSpace(evt.ItemID))
	if itemID, open := active[key]; open {
		return b.extendStreamBlock(evt, itemID, kind, now)
	}

	// Block indices are 0-based per (turn, scope): triage's setOpenTurn
	// seeds its counters to -1 and pre-increments, so the first block of
	// a turn is 0. An import allocating from 1 would give every row a
	// different id than the live session that produced the same file.
	var itemID, payloadID string
	counter := scopeKey(turnIndex, scope)
	if kind == kindThinking {
		seq := b.blockSeq[counter]
		b.blockSeq[counter] = seq + 1
		itemID = triage.ThinkingItemID(turnIndex, scope, seq)
		payloadID = triage.ThinkingPayloadID(itemID)
	} else {
		seq := b.segmentSeq[counter]
		b.segmentSeq[counter] = seq + 1
		itemID = triage.TextItemID(turnIndex, scope, seq)
		payloadID = triage.AssistantTextPayloadID(b.thread.ID, itemID)
	}

	summary := evt.Content
	if kind == kindThinking {
		summary = triage.ThinkingSummaryPreview(evt.Content)
	}
	meta, err := mergeMetaKeys(b.providerMetaString(evt), map[string]string{
		"provider_item_id": strings.TrimSpace(evt.ItemID),
	})
	if err != nil {
		return fmt.Errorf("%s meta %s: %w", kind, itemID, err)
	}
	payloadEvt := evt.ProviderEvent
	payloadEvt.Meta = b.providerMeta(evt)
	if _, err := b.appendRow(evt, store.Item{
		ID:        itemID,
		TurnIndex: turnIndex,
		Kind:      kind,
		Role:      "assistant",
		Status:    statusCompleted,
		Summary:   summary,
		ParentID:  scope,
		Meta:      meta,
		CreatedAt: now,
		UpdatedAt: now,
	}, &store.Payload{
		ID:        payloadID,
		Kind:      kind,
		Meta:      triage.BuildPayloadMeta(kind, payloadEvt),
		Data:      []byte(evt.Content),
		CreatedAt: now,
	}, nil); err != nil {
		return err
	}
	active[key] = itemID
	return nil
}

// contentBlockStop builds the row a SETTLED whole block produces.
//
// This is not framing, and treating it as such is how a Codex import used
// to land with no assistant text and no thinking at all: the Codex reader
// speaks the same vocabulary the live Codex adapter does, and there an
// `agentMessage` / `reasoning` item completing arrives as a content-block
// stop carrying the whole text plus a `blockType` meta — never as a delta.
// Triage persists exactly that (blockTypeForStop → settleStreaming*Async,
// which falls through to persistOrUpdateCompleted*Item when no block is
// open), so the writer must too.
//
// A stop with content SETTLES rather than extends: live, the final content
// replaces the streamed text rather than appending to it. And the block is
// closed either way, so the next stop in the same turn starts its own row —
// which is what makes two consecutive agent messages two rows, as they are
// live.
func (b *builder) contentBlockStop(evt importir.Event) error {
	kind, typed := settledBlockKind(evt.Meta)
	if !typed {
		// Claude's wire omits the type and triage resolves it from the
		// block it has open. No import reader emits an unlabelled stop —
		// both readers carry whole blocks — so there is nothing to resolve
		// against and nothing to write.
		b.warn("import.untyped-block-stop",
			"skipped a content block that named no block type")
		return nil
	}
	if !evt.ContentPresent || evt.Content == "" {
		return nil
	}
	turnIndex := b.turns.currentFor(evt)
	scope := strings.TrimSpace(evt.ParentToolUseID)
	key := streamKey(turnIndex, scope, strings.TrimSpace(evt.ItemID))
	active := b.activeBlocks(kind)

	if itemID, open := active[key]; open {
		now, err := timestamp(evt)
		if err != nil {
			return err
		}
		r, err := b.resolveStreamBlock(itemID, kind)
		if err != nil {
			return err
		}
		delete(active, key)
		return b.setStreamBlockContent(evt, r, kind, now, evt.Content)
	}
	if err := b.streamBlock(evt, kind); err != nil {
		return err
	}
	delete(active, key)
	return nil
}

// settledBlockKind reads the `blockType` a settled block names. The key is
// the one the live Codex adapter writes, which is why the reader emits it
// verbatim rather than inventing an import-only spelling.
func settledBlockKind(raw json.RawMessage) (string, bool) {
	switch metaString(raw, blockTypeMetaKey) {
	case "text":
		return kindAssistantText, true
	case "thinking":
		return kindThinking, true
	default:
		return "", false
	}
}

func (b *builder) activeBlocks(kind string) map[string]string {
	if kind == kindThinking {
		return b.activeThinking
	}
	return b.activeText
}
