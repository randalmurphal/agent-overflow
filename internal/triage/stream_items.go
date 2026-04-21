package triage

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

func (r *Router) handleTextDelta(evt provider.ProviderEvent) error {
	if evt.Content == "" {
		return nil
	}
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

	// Hot path: first delta opens the block and UpsertItem creates the
	// row; every subsequent delta just appends into the summary column
	// via AppendItemSummaryAndHighlight — SQLite does the concat inside
	// one UPDATE and the render callback rewrites highlighted_content in
	// the same transaction, so total work stays linear in cumulative
	// text size instead of the former O(N^2) (GetThreadItem →
	// existing+delta in Go → UpsertItem re-read).
	if !firstBlock {
		updated, err := r.store.AppendItemSummaryAndHighlight(itemID, evt.Content, r.highlighter.RenderMarkdown, now)
		if err == nil {
			r.emitItemUpsert(updated)
			return r.emitInline(evt)
		}
		// Fall through to UpsertItem on ErrNoRows: a prior firstBlock
		// insert might have failed, leaving the counter bumped but no
		// row. Re-creating the row here is how the old code self-healed
		// via GetThreadItem/UpsertItem, and the delta data is small so
		// paying one UpsertItem round-trip is fine for the error path.
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("text delta append %s: %w", itemID, err)
		}
	}

	item := store.Item{
		ID:        itemID,
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		Kind:      "assistant_text",
		Role:      "assistant",
		Status:    "streaming",
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

func (r *Router) handleThinking(evt provider.ProviderEvent) error {
	if evt.Content == "" {
		return nil
	}
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
	// same blob without a Store round-trip. First delta inserts; later
	// deltas append inside SQLite via AppendItemSummary +
	// AppendPayloadData — same O(N^2) → O(N) fix as handleTextDelta. See
	// store.AppendItemSummary / store.AppendPayloadData.
	payloadID := "thinking:" + itemID

	if !firstBlock {
		// Append-only path: extend summary + payload.data without
		// reading cumulative text into Go memory. The payload meta
		// carries the preview, which only reflects the first ~200
		// runes of the block; we leave it alone here and let
		// settleStreamingThinking rebuild it from the final summary
		// when the block closes.
		//
		// The summary append rewrites the item's highlighted_content
		// from the cumulative summary using the ANSI renderer —
		// thinking can contain terminal escape sequences. The payload
		// blob stays raw; payload HTML is rendered on demand at
		// GetPayloadData-time (see app_payloads.go).
		updated, err := r.store.AppendItemSummaryAndHighlight(itemID, evt.Content, r.highlighter.RenderANSI, now)
		if err == nil {
			if err := r.store.AppendPayloadData(payloadID, []byte(evt.Content), updated.PayloadMeta, now); err != nil {
				return fmt.Errorf("thinking delta append payload %s: %w", payloadID, err)
			}
			r.emitItemUpsert(updated)
			return r.emitInline(evt)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("thinking delta append %s: %w", itemID, err)
		}
		// Fall through to full insert: a prior firstBlock persist may
		// have failed and left the counter bumped without a row.
	}

	metaEvt := evt
	metaEvt.Content = evt.Content
	item := store.Item{
		ID:        itemID,
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		Kind:      "thinking",
		Role:      "assistant",
		Status:    "streaming",
		Summary:   evt.Content,
		PayloadID: payloadID,
		ParentID:  eventParentID(evt),
		CreatedAt: now,
		UpdatedAt: now,
	}
	payload := store.Payload{
		ID:        payloadID,
		Kind:      "thinking",
		Meta:      buildPayloadMeta("thinking", metaEvt),
		Data:      []byte(evt.Content),
		CreatedAt: now,
	}
	if err := r.persistItem(item, &payload); err != nil {
		return err
	}
	return r.emitInline(evt)
}

func scopeCounterKey(threadID string, turnIndex int, scope string) string {
	return fmt.Sprintf("%s|%d|%s", threadID, turnIndex, scope)
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
