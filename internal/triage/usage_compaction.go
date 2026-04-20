package triage

import (
	"encoding/json"
	"fmt"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

func decodeContextWindow(raw json.RawMessage) (provider.ContextWindow, bool) {
	if len(raw) == 0 {
		return provider.ContextWindow{}, false
	}
	var window provider.ContextWindow
	if json.Unmarshal(raw, &window) == nil {
		if window.UsedTokens != 0 || window.MaxTokens != 0 || window.UsedPercentage != 0 || window.TotalProcessed != 0 {
			return window, true
		}
	}

	var usage provider.TokenUsage
	if json.Unmarshal(raw, &usage) == nil {
		used := usage.InputTokens + usage.OutputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens
		if used > 0 {
			return provider.ContextWindow{
				UsedTokens: used,
			}, true
		}
	}
	return provider.ContextWindow{}, false
}

func encodeContextWindow(window provider.ContextWindow) string {
	data, err := json.Marshal(map[string]any{
		"usedTokens":     window.UsedTokens,
		"maxTokens":      window.MaxTokens,
		"contextPercent": window.UsedPercentage,
		"totalProcessed": window.TotalProcessed,
	})
	if err != nil {
		return ""
	}
	return string(data)
}

func (r *Router) handleCompaction(evt provider.ProviderEvent) error {
	now := eventTimestampMillis(evt)
	turnIndex, err := r.currentTurnIndex(evt.ThreadID)
	if err != nil {
		return fmt.Errorf("compaction turn index: %w", err)
	}

	item := store.Item{
		ID:        nextCompactionID(turnIndex),
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		Kind:      "compaction",
		Role:      "system",
		Status:    statusCompleted,
		Summary:   firstNonEmptyString(strings.TrimSpace(evt.Content), "Context compacted"),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := r.persistItem(item, nil); err != nil {
		return err
	}
	if err := r.store.ClearLastTokenUsage(evt.ThreadID); err != nil {
		return fmt.Errorf("compaction clear usage: %w", err)
	}
	r.emit("provider:usage", provider.UsageEvent{
		Action:   "reset",
		ThreadID: evt.ThreadID,
	})
	return nil
}
