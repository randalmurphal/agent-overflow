package triage

import (
	"encoding/json"
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
	if r.ensureTextBlockStarted(evt.ThreadID, turnIndex, scope) {
		defer r.drainInterruptQueueIfIdle(evt.ThreadID)
	}
	itemID := textItemID(turnIndex, scope, r.segmentIndex(evt.ThreadID, turnIndex, scope))
	now := evt.Timestamp.UnixMilli()
	if now == 0 {
		now = time.Now().UnixMilli()
	}
	existing, found, err := r.store.GetThreadItem(evt.ThreadID, itemID)
	if err != nil {
		return fmt.Errorf("text delta get item %s: %w", itemID, err)
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
	if found {
		item.Summary = existing.Summary + evt.Content
		item.PayloadID = existing.PayloadID
		item.CreatedAt = existing.CreatedAt
		item.UpdatedAt = now
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
	if r.ensureThinkingBlockStarted(evt.ThreadID, turnIndex, scope) {
		defer r.drainInterruptQueueIfIdle(evt.ThreadID)
	}
	itemID := thinkingItemID(turnIndex, scope, r.blockIndex(evt.ThreadID, turnIndex, scope))
	now := evt.Timestamp.UnixMilli()
	if now == 0 {
		now = time.Now().UnixMilli()
	}
	existing, found, err := r.store.GetThreadItem(evt.ThreadID, itemID)
	if err != nil {
		return fmt.Errorf("thinking get item %s: %w", itemID, err)
	}
	item := store.Item{
		ID:        itemID,
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		Kind:      "thinking",
		Role:      "assistant",
		Status:    "streaming",
		Summary:   evt.Content,
		ParentID:  eventParentID(evt),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if found {
		item.Summary = existing.Summary + evt.Content
		item.PayloadID = existing.PayloadID
		item.CreatedAt = existing.CreatedAt
		item.UpdatedAt = now
	}
	payloadID := item.PayloadID
	if payloadID == "" {
		payloadID = "thinking:" + itemID
	}
	metaEvt := evt
	metaEvt.Content = item.Summary
	payload := store.Payload{
		ID:        payloadID,
		Kind:      "thinking",
		Meta:      buildPayloadMeta("thinking", metaEvt),
		Data:      []byte(item.Summary),
		CreatedAt: now,
	}
	if err := r.persistItem(item, &payload); err != nil {
		return err
	}
	return r.emitInline(evt)
}

func parseTokenUsage(meta json.RawMessage) (provider.TokenUsage, bool) {
	if len(meta) == 0 {
		return provider.TokenUsage{}, false
	}

	var usage provider.TokenUsage
	if err := json.Unmarshal(meta, &usage); err != nil {
		return provider.TokenUsage{}, false
	}
	return usage, true
}

func (r *Router) lookupThreadModel(threadID string) (string, error) {
	thread, err := r.store.GetThread(threadID)
	if err != nil {
		return "", err
	}
	return thread.Model, nil
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
