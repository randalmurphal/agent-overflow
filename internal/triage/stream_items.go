package triage

import (
	"fmt"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// thinkingPreviewRunes caps the persisted summary on thinking rows to
// the trailing N code points. The frontend mirrors this constant in
// `frontend/src/lib/stores/thread.svelte.ts` (`THINKING_TAIL_RUNES`)
// so the in-memory pane summary matches what the server writes at
// settle time — a divergence would cause the row to visibly shrink at
// the streaming → completed transition. Keep the two in sync.
//
// Sized to overflow the 3-line collapsed-view box (`max-h-[3lh]` at
// 12px italic with `leading-relaxed`) at realistic chat-pane widths so
// the CSS clip + tail scroll-pin in `ThinkingBlock.svelte` show a
// consistent 3 lines regardless of pane width. Below this, narrow
// content would only fill 1–2 lines on wide panes and the box would
// visibly collapse.
const thinkingPreviewRunes = 400

func (r *Router) handleTextDelta(evt provider.ProviderEvent) error {
	if evt.Content == "" {
		return nil
	}
	// Codex background projector: a text delta is a MODEL-PRODUCED event,
	// which is the wire-typed yield signal. Any inProgress unifiedExec
	// items for the thread get stamped is_background=true before the
	// delta persists. Safe to call before the block-start check — the
	// projector no-ops when there are no trackers. See invariant 25.
	r.observeCodexModelContent(evt.ThreadID)

	turnIndex, err := r.turnIndexForEvent(evt)
	if err != nil {
		return fmt.Errorf("text delta turn index: %w", err)
	}
	scope := eventParentID(evt)
	firstBlock, itemID := r.ensureTextBlockStarted(evt.ThreadID, turnIndex, scope, evt.ItemID)
	if firstBlock {
		defer r.drainInterruptQueueIfIdle(evt.ThreadID)
	}
	now := evt.Timestamp.UnixMilli()
	if now == 0 {
		now = time.Now().UnixMilli()
	}

	if firstBlock {
		item := store.Item{
			ID:        itemID,
			ThreadID:  evt.ThreadID,
			TurnIndex: turnIndex,
			Kind:      itemKindAssistantText,
			Role:      "assistant",
			Status:    statusStreaming,
			Summary:   evt.Content,
			ParentID:  eventParentID(evt),
			Meta:      withProviderItemMeta(validJSONObjectString(evt.Meta), evt.ItemID),
			CreatedAt: now,
			UpdatedAt: now,
		}
		return r.emitStreamingBlockStart(item, nil, evt.Content, now)
	}
	r.emitItemDelta(ItemDeltaEvent{
		ThreadID:  evt.ThreadID,
		ItemID:    itemID,
		Kind:      itemKindAssistantText,
		Delta:     evt.Content,
		UpdatedAt: now,
	})
	if err := r.bufferTextPersistence(evt.ThreadID, itemID, evt.Content, now); err != nil {
		return fmt.Errorf("text delta buffer %s: %w", itemID, err)
	}
	return r.emitInline(evt)
}

func (r *Router) handleThinking(evt provider.ProviderEvent) error {
	if evt.Content == "" {
		return nil
	}
	// Reasoning deltas count as a yield for the Codex background-terminal
	// projector, but Codex TUI keeps any active terminal wait status visible
	// while reasoning streams.
	r.observeCodexModelReasoning(evt.ThreadID)

	turnIndex, err := r.turnIndexForEvent(evt)
	if err != nil {
		return fmt.Errorf("thinking turn index: %w", err)
	}
	scope := eventParentID(evt)
	firstBlock, itemID := r.ensureThinkingBlockStarted(evt.ThreadID, turnIndex, scope, evt.ItemID)
	if firstBlock {
		defer r.drainInterruptQueueIfIdle(evt.ThreadID)
	}
	now := evt.Timestamp.UnixMilli()
	if now == 0 {
		now = time.Now().UnixMilli()
	}

	// Payload id is deterministic so subsequent deltas can address the
	// same blob without a Store round-trip. The live UI gets an ordered
	// provider:item_event delta immediately; SQLite receives buffered
	// appends by interval, threshold, or lifecycle boundary.
	payloadID := "thinking:" + itemID

	if firstBlock {
		metaEvt := evt
		item := store.Item{
			ID:        itemID,
			ThreadID:  evt.ThreadID,
			TurnIndex: turnIndex,
			Kind:      itemKindThinking,
			Role:      "assistant",
			Status:    statusStreaming,
			Summary:   thinkingSummaryPreview(evt.Content),
			PayloadID: payloadID,
			ParentID:  eventParentID(evt),
			Meta:      withProviderItemMeta(validJSONObjectString(evt.Meta), evt.ItemID),
			CreatedAt: now,
			UpdatedAt: now,
		}
		payload := store.Payload{
			ID:        payloadID,
			Kind:      itemKindThinking,
			Meta:      buildPayloadMeta(itemKindThinking, metaEvt),
			Data:      []byte(evt.Content),
			CreatedAt: now,
		}
		return r.emitStreamingBlockStart(item, &payload, evt.Content, now)
	}
	flushThinkingAfterEmit := r.stageThinkingPersistenceForEmit(evt.ThreadID, itemID, payloadID, evt.Content, now)
	r.emitItemDelta(ItemDeltaEvent{
		ThreadID:  evt.ThreadID,
		ItemID:    itemID,
		Kind:      itemKindThinking,
		Delta:     evt.Content,
		UpdatedAt: now,
	})
	if flushThinkingAfterEmit {
		if err := r.flushStreamingItem(evt.ThreadID, itemID); err != nil {
			return fmt.Errorf("thinking delta flush %s: %w", itemID, err)
		}
	}
	return r.emitInline(evt)
}

// emitStreamingBlockStart persists a new streaming text/thinking row (with
// its optional payload) for history / mid-stream restore, then emits row
// creation with an EMPTY summary and ships the first chunk as a delta.
//
// If the creation upsert carried the content, the frontend per-item smoother
// would seed it as already-revealed and it would snap in as one block with no
// animation — very visible when the first chunk is large (a whole
// assistant-message text block from parse_assistant, or the opening burst
// after a thinking block). Routing ALL content through deltas lets every
// chunk animate; the non-first delta path already does this, so this just
// makes the first chunk consistent.
//
// It emits the PERSISTED row (summary blanked), not the pre-persist struct:
// the store assigns item_index via nextItemIndexTx, so the pre-persist struct
// still carries item_index 0 and would mis-sort against a preceding row (e.g.
// a thinking block before the text) and feed a wrong position into the
// frontend reveal gate.
func (r *Router) emitStreamingBlockStart(
	item store.Item,
	payload *store.Payload,
	content string,
	now int64,
) error {
	persisted, err := r.persistItemQuietReturning(item, payload)
	if err != nil {
		return err
	}
	persisted.Summary = ""
	r.emitItemUpsert(persisted)
	r.emitItemDelta(ItemDeltaEvent{
		ThreadID:  persisted.ThreadID,
		ItemID:    persisted.ID,
		Kind:      persisted.Kind,
		Delta:     content,
		UpdatedAt: now,
	})
	return nil
}

func scopeCounterKey(threadID string, turnIndex int, scope string) string {
	return fmt.Sprintf("%s|%d|%s", threadID, turnIndex, scope)
}

// thinkingSummaryPreview seeds a new thinking row's items.summary with
// the tail of the first delta. Subsequent deltas land via
// `AppendItemSummaryTail`, which keeps the row tail-bounded at
// `thinkingPreviewRunes` characters across the whole streaming run.
// The frontend renders the END of the reasoning (a sliding 3-line tail
// clipped via CSS); a raw character slice with no ellipsis prefix is
// fine because the leading characters fall outside the visible window
// regardless.
func thinkingSummaryPreview(content string) string {
	runes := []rune(content)
	if len(runes) <= thinkingPreviewRunes {
		return content
	}
	return string(runes[len(runes)-thinkingPreviewRunes:])
}

func textItemID(turnIndex int, scope string, segmentIndex int) string {
	if scope == "" {
		return fmt.Sprintf("text:%d:%d", turnIndex, segmentIndex)
	}
	return fmt.Sprintf("text:%d:%s:%d", turnIndex, scope, segmentIndex)
}

func thinkingItemID(turnIndex int, scope string, blockIndex int) string {
	if scope == "" {
		return fmt.Sprintf("think:%d:%d", turnIndex, blockIndex)
	}
	return fmt.Sprintf("think:%d:%s:%d", turnIndex, scope, blockIndex)
}

// settleStreamingScope is the per-scope settle hook used by
// settleStreamingBeforeTimelineBoundary when a parent_tool_use_id
// scopes the in-flight blocks (Claude subagent / nested tool path).
// The actual SQLite write moves to async goroutines so the next
// provider event on the read-loop can flow without waiting on the
// settle's persist. Returns nil always; errors surface via the
// settle goroutine's log line.
func (r *Router) settleStreamingScope(threadID, scope string) error {
	turnIndex, err := r.turnIndexForScope(threadID, scope)
	if err != nil {
		return err
	}
	r.settleStreamingTextScopeAsync(threadID, turnIndex, scope, statusCompleted)
	r.settleStreamingThinkingScopeAsync(threadID, turnIndex, scope, statusCompleted)
	return nil
}
