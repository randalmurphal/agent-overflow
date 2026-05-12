package triage

import (
	"fmt"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

const thinkingPreviewRunes = 200

func (r *Router) handleTextDelta(evt provider.ProviderEvent) error {
	if evt.Content == "" {
		return nil
	}
	// Codex background projector: a text delta is a MODEL-PRODUCED event,
	// which is the wire-typed yield signal. Any inProgress unifiedExec
	// items for the thread get stamped is_background=true before the
	// delta persists. Safe to call before the block-start check — the
	// projector no-ops when there are no trackers. See invariant 25.
	r.observeCodexModelYield(evt.ThreadID)

	turnIndex, err := r.currentTurnIndex(evt.ThreadID)
	if err != nil {
		return fmt.Errorf("text delta turn index: %w", err)
	}
	scope := evt.ParentToolUseID
	firstBlock := r.ensureTextBlockStarted(evt.ThreadID, turnIndex, scope)
	if firstBlock {
		defer r.drainInterruptQueueIfIdle(evt.ThreadID)
	}
	itemID := textItemID(turnIndex, scope, r.segmentIndex(evt.ThreadID, turnIndex, scope))
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
			CreatedAt: now,
			UpdatedAt: now,
		}
		if err := r.persistItem(item, nil); err != nil {
			return err
		}
		return r.emitInline(evt)
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
	// projector — the model has moved on while any inProgress unifiedExec
	// command is still running. Mirrors the handleTextDelta yield hook.
	r.observeCodexModelYield(evt.ThreadID)

	turnIndex, err := r.currentTurnIndex(evt.ThreadID)
	if err != nil {
		return fmt.Errorf("thinking turn index: %w", err)
	}
	scope := evt.ParentToolUseID
	firstBlock := r.ensureThinkingBlockStarted(evt.ThreadID, turnIndex, scope)
	if firstBlock {
		defer r.drainInterruptQueueIfIdle(evt.ThreadID)
	}
	itemID := thinkingItemID(turnIndex, scope, r.blockIndex(evt.ThreadID, turnIndex, scope))
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
		if err := r.persistItem(item, &payload); err != nil {
			return err
		}
		return r.emitInline(evt)
	}
	r.emitItemDelta(ItemDeltaEvent{
		ThreadID:  evt.ThreadID,
		ItemID:    itemID,
		Kind:      itemKindThinking,
		Delta:     evt.Content,
		UpdatedAt: now,
	})
	if err := r.bufferThinkingPersistence(evt.ThreadID, itemID, payloadID, evt.Content, now); err != nil {
		return fmt.Errorf("thinking delta buffer %s: %w", itemID, err)
	}
	return r.emitInline(evt)
}

func scopeCounterKey(threadID string, turnIndex int, scope string) string {
	return fmt.Sprintf("%s|%d|%s", threadID, turnIndex, scope)
}

// thinkingSummaryPreview returns a tail-truncated preview of the
// thinking content. The frontend displays the END of the reasoning
// (a sliding 3-line tail), so the persisted summary keeps the tail
// too — reloaded threads then show the actual end of thinking
// without paying for a full payload fetch on mount.
func thinkingSummaryPreview(content string) string {
	runes := []rune(content)
	if len(runes) <= thinkingPreviewRunes {
		return content
	}
	return "..." + string(runes[len(runes)-thinkingPreviewRunes:])
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

func (r *Router) segmentIndex(threadID string, turnIndex int, scope string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.segmentIndexByScope[scopeCounterKey(threadID, turnIndex, scope)]
	if !ok {
		return 0
	}
	return value
}

func (r *Router) blockIndex(threadID string, turnIndex int, scope string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.blockIndexByScope[scopeCounterKey(threadID, turnIndex, scope)]
	if !ok {
		return 0
	}
	return value
}

func (r *Router) settleStreamingScope(threadID, scope string) error {
	turnIndex, err := r.currentTurnIndex(threadID)
	if err != nil {
		return err
	}
	if err := r.settleStreamingText(threadID, turnIndex, scope, statusCompleted); err != nil {
		return err
	}
	if err := r.settleStreamingThinking(threadID, turnIndex, scope, statusCompleted); err != nil {
		return err
	}
	return nil
}
