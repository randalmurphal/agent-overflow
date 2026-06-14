package triage

import (
	"fmt"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// handleCompactionReasoning streams the claudetui compaction summarizer's
// reasoning as its own top-level `compaction_reasoning` row — the live "compact"
// tail that settles just above the `compaction` divider. It is dispatched only
// for EventThinking carrying provider.CompactionReasoningScope (see Handle), and
// reuses the thinking streaming machinery (active-block maps, tail-bounded
// persistence, async settle) under that reserved scope, diverging only in the
// row's kind and its top-level placement.
//
// Turn resolution is direct (currentTurnIndex), NOT turnIndexForEvent: the
// reserved scope is not a real subagent parent (turnIndexForScope would look it
// up as an item and miss). The summarizer streams in the compaction window — the
// prior turn just settled, the next has not opened — so the reasoning row shares
// the divider's turn, and because it is created during streaming (before the
// divider persists at PostCompact) it sorts just before the divider.
func (r *Router) handleCompactionReasoning(evt provider.ProviderEvent) error {
	if evt.Content == "" {
		return nil
	}
	turnIndex, err := r.currentTurnIndex(evt.ThreadID)
	if err != nil {
		return fmt.Errorf("compaction reasoning turn index: %w", err)
	}
	scope := provider.CompactionReasoningScope
	firstBlock, itemID := r.ensureThinkingBlockStarted(evt.ThreadID, turnIndex, scope, evt.ItemID)
	if firstBlock {
		defer r.drainInterruptQueueIfIdle(evt.ThreadID)
	}
	now := evt.Timestamp.UnixMilli()
	if now == 0 {
		now = time.Now().UnixMilli()
	}

	// Deterministic payload id keyed off the item, mirroring thinking, so later
	// deltas address the same blob without a store round-trip.
	payloadID := thinkingPayloadID(itemID)

	if firstBlock {
		item := store.Item{
			ID:        itemID,
			ThreadID:  evt.ThreadID,
			TurnIndex: turnIndex,
			Kind:      itemKindCompactionReasoning,
			Role:      "assistant",
			Status:    statusStreaming,
			Summary:   thinkingSummaryPreview(evt.Content),
			PayloadID: payloadID,
			// ParentID intentionally empty: the reserved scope isolates this
			// row's id + stream counters, but it renders top-level (above the
			// divider), never nested under an Agent card.
			CreatedAt: now,
			UpdatedAt: now,
		}
		payload := store.Payload{
			ID:        payloadID,
			Kind:      itemKindCompactionReasoning,
			Meta:      buildPayloadMeta(itemKindCompactionReasoning, evt),
			Data:      []byte(evt.Content),
			CreatedAt: now,
		}
		return r.emitStreamingBlockStart(item, &payload, evt.Content, now)
	}

	flushAfterEmit := r.stageThinkingPersistenceForEmit(evt.ThreadID, itemID, payloadID, evt.Content, now)
	r.emitItemDelta(ItemDeltaEvent{
		ThreadID:  evt.ThreadID,
		ItemID:    itemID,
		Kind:      itemKindCompactionReasoning,
		Delta:     evt.Content,
		UpdatedAt: now,
	})
	if flushAfterEmit {
		if err := r.flushStreamingItem(evt.ThreadID, itemID); err != nil {
			return fmt.Errorf("compaction reasoning flush %s: %w", itemID, err)
		}
	}
	// Mirror handleThinking's delta-path exit: emitInline is a no-op marker
	// today, but keeping it here means a future re-activation treats the
	// compaction-reasoning tail like the thinking tail rather than silently
	// skipping it.
	return r.emitInline(evt)
}

// settleCompactionReasoning finalizes the live reasoning row when the
// summarizer's thinking block stops (EventContentBlockStop carrying the reserved
// scope). It reuses the thinking settle — the block was registered in the
// thinking active-block maps under the reserved scope — flipping the row to
// completed. The committed compaction summary lands separately on the divider.
func (r *Router) settleCompactionReasoning(evt provider.ProviderEvent) error {
	turnIndex, err := r.currentTurnIndex(evt.ThreadID)
	if err != nil {
		return fmt.Errorf("compaction reasoning settle turn index: %w", err)
	}
	r.settleStreamingThinkingAsync(
		evt.ThreadID,
		turnIndex,
		provider.CompactionReasoningScope,
		evt.ItemID,
		statusCompleted,
		evt.Content,
		evt.ContentPresent,
	)
	return nil
}
