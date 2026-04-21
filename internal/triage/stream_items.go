package triage

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// streamingHighlightIntervalMs caps how often the streaming delta path
// re-renders the cumulative summary to HTML. goldmark+chroma on a 40 KB
// summary runs in ~0.5 ms; terminal-to-html on a 40 KB thinking block
// runs in ~0.15 ms. Unthrottled that is fine, but Claude can stream 40
// deltas per second and the render parses the FULL cumulative summary
// every time, so the total work grows quadratically in the length of
// the summary. At 50 ms we cap that burst to ~20 renders/sec per item
// and the user-visible lag is under one animation frame.
//
// Settle (content-block-stop, interrupt, turn-end) forces a final
// render regardless of the throttle, so the last visible state always
// reflects the completed summary.
const streamingHighlightIntervalMs int64 = 50

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
	// row. Every subsequent delta appends into the summary column via
	// AppendItemSummary (one UPDATE, no render inside the TX), then
	// optionally renders the cumulative summary and flushes the result
	// with UpdateItemHighlight. The render runs OUTSIDE the writer TX
	// so it does not block other thread writes, and the throttle caps
	// render frequency to streamingHighlightIntervalMs regardless of
	// provider delta rate.
	if !firstBlock {
		updated, err := r.store.AppendItemSummary(itemID, evt.Content, now)
		if err == nil {
			if r.shouldRenderHighlight(evt.ThreadID, itemID, now) {
				html := r.highlighter.RenderMarkdown(updated.Summary)
				if err := r.store.UpdateItemHighlight(itemID, html); err != nil {
					return fmt.Errorf("text delta highlight %s: %w", itemID, err)
				}
				updated.HighlightedContent = html
			}
			r.emitItemUpsert(updated)
			return r.emitInline(evt)
		}
		// An interrupt or settle has already committed a terminal status
		// for this row — drop the late delta rather than resurrect the
		// streaming state. The frontend already reflects the settled row
		// from the interrupt's own upsert; we only need to let the
		// passthrough emit fire so inline cards stay in sync.
		if errors.Is(err, store.ErrItemSettled) {
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
	// persistItem just rendered — reset the throttle window so the next
	// real delta doesn't race the render it just triggered.
	r.markHighlighted(evt.ThreadID, itemID, now)
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
	// AppendPayloadData — same O(N^2) → O(N) fix as handleTextDelta.
	payloadID := "thinking:" + itemID

	if !firstBlock {
		// Append-only path: extend summary + payload.data without reading
		// cumulative text into Go memory. The payload meta carries the
		// preview, which only reflects the first ~200 runes of the block;
		// we leave it alone here and let settleStreamingThinking rebuild
		// it from the final summary when the block closes.
		//
		// HighlightedContent uses the same throttled two-phase write as
		// text deltas. The renderer for thinking is ANSI (terminal→HTML)
		// because thinking often carries escape sequences; the payload
		// blob stays raw. Payload HTML is rendered on demand at
		// GetPayloadData time (see app_payloads.go).
		updated, err := r.store.AppendItemSummary(itemID, evt.Content, now)
		if err == nil {
			if err := r.store.AppendPayloadData(payloadID, []byte(evt.Content), updated.PayloadMeta, now); err != nil {
				return fmt.Errorf("thinking delta append payload %s: %w", payloadID, err)
			}
			if r.shouldRenderHighlight(evt.ThreadID, itemID, now) {
				html := r.highlighter.RenderANSI(updated.Summary)
				if err := r.store.UpdateItemHighlight(itemID, html); err != nil {
					return fmt.Errorf("thinking delta highlight %s: %w", itemID, err)
				}
				updated.HighlightedContent = html
			}
			r.emitItemUpsert(updated)
			return r.emitInline(evt)
		}
		// Settled row: interrupt or settle beat this delta to the row.
		// Drop the delta (and its payload append) to avoid clobbering
		// the terminal summary; see handleTextDelta for the same guard.
		if errors.Is(err, store.ErrItemSettled) {
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
	r.markHighlighted(evt.ThreadID, itemID, now)
	return r.emitInline(evt)
}

// highlightThrottleKey returns the map key for nextHighlightAt. Scoping
// by thread lets CleanupThread prune the entries for a torn-down
// thread by prefix, matching the activeTextBlocks / activeThinkingBlocks
// cleanup pattern.
func highlightThrottleKey(threadID, itemID string) string {
	return threadID + "|" + itemID
}

// shouldRenderHighlight returns true when enough wall-clock time has
// elapsed since the last render for this item that we should re-render
// on the current delta. Also updates the throttle bookkeeping so the
// NEXT caller sees the new floor.
func (r *Router) shouldRenderHighlight(threadID, itemID string, nowMs int64) bool {
	key := highlightThrottleKey(threadID, itemID)
	r.mu.Lock()
	defer r.mu.Unlock()
	next, ok := r.nextHighlightAt[key]
	if ok && nowMs < next {
		return false
	}
	r.nextHighlightAt[key] = nowMs + streamingHighlightIntervalMs
	return true
}

// markHighlighted records that the caller just rendered this item so
// shouldRenderHighlight won't fire again until the throttle elapses.
// Used by the first-delta path where persistItem's built-in render runs
// unconditionally.
func (r *Router) markHighlighted(threadID, itemID string, nowMs int64) {
	key := highlightThrottleKey(threadID, itemID)
	r.mu.Lock()
	r.nextHighlightAt[key] = nowMs + streamingHighlightIntervalMs
	r.mu.Unlock()
}

// forgetHighlighted drops the throttle entry for an item that has
// settled. Called from the settle paths so the map does not grow
// unboundedly across the life of a thread.
func (r *Router) forgetHighlighted(threadID, itemID string) {
	key := highlightThrottleKey(threadID, itemID)
	r.mu.Lock()
	delete(r.nextHighlightAt, key)
	r.mu.Unlock()
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
