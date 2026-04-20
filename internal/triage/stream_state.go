package triage

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/provider"
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
		// Queue drain is best-effort at this layer; the turn/error handlers
		// already surface persistence failures via their own return path.
		return
	}
}

func (r *Router) drainInterruptQueue(threadID string, forceErrored bool) error {
	r.mu.Lock()
	queue := r.interruptQueue[threadID]
	delete(r.interruptQueue, threadID)
	r.mu.Unlock()

	for _, queued := range queue {
		item := queued.item
		if forceErrored {
			item.Status = statusErrored
			item.Summary = interruptedSummary(item.Summary)
			item.UpdatedAt = time.Now().UnixMilli()
		}
		if err := r.persistItem(item, queued.payload); err != nil {
			return err
		}
	}
	return nil
}

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

	itemID := textItemID(turnIndex, scope, index)
	item, found, err := r.store.GetThreadItem(threadID, itemID)
	if err != nil || !found {
		return err
	}
	if item.Status != "streaming" {
		return nil
	}
	item.Status = status
	if status == statusErrored {
		item.Summary = interruptedSummary(item.Summary)
	}
	item.UpdatedAt = time.Now().UnixMilli()
	if err := r.persistItem(item, nil); err != nil {
		return err
	}
	r.drainInterruptQueueIfIdle(threadID)
	return nil
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

	itemID := thinkingItemID(turnIndex, scope, index)
	item, found, err := r.store.GetThreadItem(threadID, itemID)
	if err != nil || !found {
		return err
	}
	if item.Status != "streaming" {
		return nil
	}
	item.Status = status
	if status == statusErrored {
		item.Summary = interruptedSummary(item.Summary)
	}
	item.UpdatedAt = time.Now().UnixMilli()

	// Now that the block has closed, refresh the thinking-meta preview so
	// the card reflects the final summary rather than the first-delta
	// preview captured at block open. We only pay the rebuild once per
	// block (not per delta). The preview rebuilds from the current
	// summary but the signature field — set later by
	// persistThinkingSignature when EventContentBlockStop carries one —
	// must survive the refresh, so we reuse the existing payload meta
	// JSON and only replace the summary-derived fields.
	if item.PayloadID != "" && item.PayloadKind == "thinking" {
		metaBase := buildPayloadMeta("thinking", provider.ProviderEvent{Content: item.Summary})
		merged := metaBase
		if item.PayloadMeta != "" && item.PayloadMeta != "{}" {
			// Preserve the signature field set by persistThinkingSignature
			// — the only field we can't reconstruct from summary alone.
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
	}
	if err := r.persistItem(item, nil); err != nil {
		return err
	}
	r.drainInterruptQueueIfIdle(threadID)
	return nil
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
