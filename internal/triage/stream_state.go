package triage

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/store"
)

type queuedPersistence struct {
	item    store.Item
	payload *store.Payload
}

func (r *Router) ensureTextBlockStarted(threadID string, turnIndex int, scope string) bool {
	key := scopeCounterKey(threadID, turnIndex, scope)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.activeTextBlocks[key] {
		return false
	}
	r.segmentIndexByScope[key] = r.segmentIndexByScope[key] + 1
	r.activeTextBlocks[key] = true
	r.streamingItemCounts[threadID] = r.streamingItemCounts[threadID] + 1
	return true
}

func (r *Router) ensureThinkingBlockStarted(threadID string, turnIndex int, scope string) bool {
	key := scopeCounterKey(threadID, turnIndex, scope)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.activeThinkingBlocks[key] {
		return false
	}
	r.blockIndexByScope[key] = r.blockIndexByScope[key] + 1
	r.activeThinkingBlocks[key] = true
	r.streamingItemCounts[threadID] = r.streamingItemCounts[threadID] + 1
	return true
}

func (r *Router) hasActiveStreamingItem(threadID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.streamingItemCounts[threadID] > 0
}

func (r *Router) maybeDeferOrPersist(threadID string, item store.Item, payload *store.Payload) error {
	if !r.hasActiveStreamingItem(threadID) {
		return r.persistItem(item, payload)
	}

	r.mu.Lock()
	r.interruptQueue[threadID] = append(r.interruptQueue[threadID], queuedPersistence{
		item:    item,
		payload: payload,
	})
	r.mu.Unlock()
	return nil
}

func (r *Router) drainInterruptQueueIfIdle(threadID string) {
	if r.hasActiveStreamingItem(threadID) {
		return
	}
	if err := r.drainInterruptQueue(threadID, false); err != nil {
		// drainInterruptQueue logs per-item failures itself; the idle
		// drain has no upstream caller to surface the aggregate to, so
		// swallow it here.
		return
	}
}

// drainInterruptQueue persists every queued item for the thread. The
// queue is handed off before iteration (cleared from the map under the
// lock), so an early return on persist failure would silently strand
// the remaining items. We log each failure and return the first error
// once the full queue has been attempted.
func (r *Router) drainInterruptQueue(threadID string, forceErrored bool) error {
	r.mu.Lock()
	queue := r.interruptQueue[threadID]
	delete(r.interruptQueue, threadID)
	r.mu.Unlock()

	var firstErr error
	for _, queued := range queue {
		item := queued.item
		if forceErrored {
			item.Status = statusErrored
			item.Summary = interruptedSummary(item.Summary)
			item.UpdatedAt = time.Now().UnixMilli()
		}
		if err := r.persistItem(item, queued.payload); err != nil {
			log.Printf("triage: drain persist failed for item %s on thread %s: %v", item.ID, threadID, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// settleTurnStreaming collects every active streaming scope in
// (threadID, turnIndex) and calls settleStreamingText /
// settleStreamingThinking on each. The prefix scan runs against the
// full activeTextBlocks / activeThinkingBlocks maps, but both maps are
// pruned aggressively — entries clear on content_block_stop (typical
// Claude path), clearOpenTurn (turn completion), and CleanupThread
// (session teardown). In steady state the map holds ~1-5 entries for
// the current in-flight turn on active threads, so a flat prefix scan
// beats the bookkeeping overhead of a nested map[threadIDTurn]map[scope]bool
// rekey. Revisit if profiling shows this becoming a hotspot.
func (r *Router) settleTurnStreaming(threadID string, turnIndex int, status string) error {
	prefix := fmt.Sprintf("%s|%d|", threadID, turnIndex)

	r.mu.Lock()
	textScopes := make([]string, 0)
	for key, active := range r.activeTextBlocks {
		if active && strings.HasPrefix(key, prefix) {
			textScopes = append(textScopes, strings.TrimPrefix(key, prefix))
		}
	}
	thinkingScopes := make([]string, 0)
	for key, active := range r.activeThinkingBlocks {
		if active && strings.HasPrefix(key, prefix) {
			thinkingScopes = append(thinkingScopes, strings.TrimPrefix(key, prefix))
		}
	}
	r.mu.Unlock()

	for _, scope := range textScopes {
		if err := r.settleStreamingText(threadID, turnIndex, scope, status); err != nil {
			return err
		}
	}
	for _, scope := range thinkingScopes {
		if err := r.settleStreamingThinking(threadID, turnIndex, scope, status); err != nil {
			return err
		}
	}
	return nil
}

func interruptedSummary(summary string) string {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return "Interrupted"
	}
	const suffix = " — interrupted"
	if strings.HasSuffix(summary, suffix) {
		return summary
	}
	return summary + suffix
}

func (r *Router) settleStreamingText(threadID string, turnIndex int, scope string, status string) error {
	key := scopeCounterKey(threadID, turnIndex, scope)
	r.mu.Lock()
	active := r.activeTextBlocks[key]
	index := r.segmentIndexByScope[key]
	if active {
		delete(r.activeTextBlocks, key)
		if count := r.streamingItemCounts[threadID]; count > 0 {
			r.streamingItemCounts[threadID] = count - 1
		}
	}
	r.mu.Unlock()
	if !active {
		return nil
	}

	// Defer the drain unconditionally once the counter has been
	// decremented: every early return below (row missing, row already
	// non-streaming) still represents a streaming slot that just
	// closed. Without this, a late settle that sees a non-streaming
	// row would leak whatever was queued behind the streaming lock.
	defer r.drainInterruptQueueIfIdle(threadID)

	itemID := textItemID(turnIndex, scope, index)
	if err := r.flushStreamingItem(threadID, itemID); err != nil {
		return err
	}
	item, found, err := r.store.GetThreadItem(threadID, itemID)
	if err != nil || !found {
		return err
	}
	if item.Status != statusStreaming {
		return nil
	}
	item.Status = status
	if status == statusErrored {
		item.Summary = interruptedSummary(item.Summary)
	}
	item.UpdatedAt = time.Now().UnixMilli()
	return r.persistItem(item, nil)
}

func (r *Router) settleStreamingThinking(threadID string, turnIndex int, scope string, status string) error {
	key := scopeCounterKey(threadID, turnIndex, scope)
	r.mu.Lock()
	active := r.activeThinkingBlocks[key]
	index := r.blockIndexByScope[key]
	if active {
		delete(r.activeThinkingBlocks, key)
		if count := r.streamingItemCounts[threadID]; count > 0 {
			r.streamingItemCounts[threadID] = count - 1
		}
	}
	r.mu.Unlock()
	if !active {
		return nil
	}

	// Same invariant as settleStreamingText: once we've decremented the
	// counter, the drain must fire regardless of what we find in the
	// store. A lookup miss or an already-non-streaming row would
	// otherwise leak whatever sat behind the streaming lock.
	defer r.drainInterruptQueueIfIdle(threadID)

	itemID := thinkingItemID(turnIndex, scope, index)
	if err := r.flushStreamingItem(threadID, itemID); err != nil {
		return err
	}
	item, found, err := r.store.GetThreadItem(threadID, itemID)
	if err != nil || !found {
		return err
	}
	if item.Status != statusStreaming {
		return nil
	}
	item.Status = status
	item.UpdatedAt = time.Now().UnixMilli()

	// Refresh the persisted summary from the full payload's tail. The
	// streaming-time summary is head-capped by AppendItemSummaryPreview
	// as deltas accumulate, but the UI shows the END of the thinking
	// (a sliding tail), so the settled summary should match what
	// reloaded threads will display without paying for a full payload
	// fetch on mount. The signature field set later by
	// persistThinkingSignature must survive the meta refresh, so we
	// only replace the summary-derived fields and merge.
	if item.PayloadID != "" && item.PayloadKind == "thinking" {
		data, err := r.store.GetPayloadData(item.PayloadID)
		if err != nil {
			return err
		}
		item.Summary = thinkingSummaryPreview(string(data))
		if status == statusErrored {
			item.Summary = interruptedSummary(item.Summary)
		}

		metaBase := buildThinkingPayloadMeta(item.Summary, len(data), "")
		merged := metaBase
		if item.PayloadMeta != "" && item.PayloadMeta != "{}" {
			if sig := metaNestedString(json.RawMessage(item.PayloadMeta), "signature"); sig != "" {
				var m map[string]any
				if jerr := json.Unmarshal([]byte(metaBase), &m); jerr == nil {
					m["signature"] = sig
					if refreshed, merr := json.Marshal(m); merr == nil {
						merged = string(refreshed)
					}
				}
			}
		}
		if err := r.store.UpdatePayloadMeta(item.PayloadID, merged); err != nil {
			return err
		}
	} else if status == statusErrored {
		// Defensive fallback: thinking items normally always have a
		// payload, but apply the interruption marker for any row that
		// somehow lacks one.
		item.Summary = interruptedSummary(item.Summary)
	}
	return r.persistItem(item, nil)
}

func nextErrorID(turnIndex int, scope string, seq int) string {
	if scope == "" {
		return fmt.Sprintf("error:%d:%d", turnIndex, seq)
	}
	return fmt.Sprintf("error:%d:%s:%d", turnIndex, scope, seq)
}

func (r *Router) nextErrorSequence(threadID string, turnIndex int, scope string) int {
	key := scopeCounterKey(threadID, turnIndex, scope)
	r.mu.Lock()
	defer r.mu.Unlock()
	seq := r.errorSeqByScope[key]
	r.errorSeqByScope[key] = seq + 1
	return seq
}

func nextCompactionID(turnIndex int) string {
	return fmt.Sprintf("compact:%d", turnIndex)
}

func nextToolCompletionID(launchID string) string {
	return "complete:" + launchID
}

// backgroundCompletionID returns the stable id for a backgrounded
// task's tool_completion sibling. Steady-state callers must resolve a
// launch tool_use_id before writing a sibling. The task_id fallback is
// defensive only, and keeps any accidental no-launch id distinct from a
// future real tool_use id.
func backgroundCompletionID(launchID, taskID string) string {
	if launchID != "" {
		return "complete:" + launchID
	}
	return "complete:by-task:" + taskID
}
