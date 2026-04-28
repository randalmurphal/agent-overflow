// Package triage — token-usage and compaction routing. decodeContextWindow
// / encodeContextWindow normalise the wire shape of ContextWindow across
// providers; handleCompaction persists a compaction timeline entry and
// clears the thread's last token-usage snapshot.

package triage

import (
	"encoding/json"
	"fmt"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/stringsx"
)

func decodeContextWindow(raw json.RawMessage) (provider.ContextWindow, bool) {
	if len(raw) == 0 {
		return provider.ContextWindow{}, false
	}
	if window, ok := decodeCodexThreadTokenUsage(raw); ok {
		return window, true
	}
	var window provider.ContextWindow
	if json.Unmarshal(raw, &window) == nil {
		if window.UsedTokens != 0 || window.MaxTokens != 0 || window.UsedPercentage != 0 || window.TotalProcessed != 0 {
			if window.UsedPercentage == 0 && window.MaxTokens > 0 {
				window.UsedPercentage = float64(window.UsedTokens) / float64(window.MaxTokens) * 100
			}
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

func decodeCodexThreadTokenUsage(raw json.RawMessage) (provider.ContextWindow, bool) {
	var payload struct {
		TokenUsage         *codexThreadTokenUsage `json:"tokenUsage"`
		Total              *codexTokenBreakdown   `json:"total"`
		Last               *codexTokenBreakdown   `json:"last"`
		ModelContextWindow int                    `json:"modelContextWindow"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return provider.ContextWindow{}, false
	}
	usage := payload.TokenUsage
	if usage == nil && (payload.Total != nil || payload.ModelContextWindow > 0) {
		usage = &codexThreadTokenUsage{
			Total:              payload.Total,
			Last:               payload.Last,
			ModelContextWindow: payload.ModelContextWindow,
		}
	}
	if usage == nil || usage.Total == nil {
		return provider.ContextWindow{}, false
	}
	used := usage.Total.TotalTokens
	if used == 0 {
		used = usage.Total.InputTokens + usage.Total.OutputTokens + usage.Total.CachedInputTokens + usage.Total.ReasoningOutputTokens
	}
	if used == 0 && usage.ModelContextWindow == 0 {
		return provider.ContextWindow{}, false
	}
	window := provider.ContextWindow{
		UsedTokens: used,
		MaxTokens:  usage.ModelContextWindow,
	}
	if window.MaxTokens > 0 {
		window.UsedPercentage = float64(window.UsedTokens) / float64(window.MaxTokens) * 100
	}
	return window, true
}

type codexThreadTokenUsage struct {
	Last               *codexTokenBreakdown `json:"last"`
	Total              *codexTokenBreakdown `json:"total"`
	ModelContextWindow int                  `json:"modelContextWindow"`
}

type codexTokenBreakdown struct {
	CachedInputTokens     int `json:"cachedInputTokens"`
	InputTokens           int `json:"inputTokens"`
	OutputTokens          int `json:"outputTokens"`
	ReasoningOutputTokens int `json:"reasoningOutputTokens"`
	TotalTokens           int `json:"totalTokens"`
}

func encodeContextWindow(window provider.ContextWindow) string {
	data, err := json.Marshal(map[string]any{
		"usedTokens":            window.UsedTokens,
		"maxTokens":             window.MaxTokens,
		"contextPercent":        window.UsedPercentage,
		"totalProcessed":        window.TotalProcessed,
		"autoCompactPercent":    window.AutoCompactPercent,
		"autoCompactTokenLimit": window.AutoCompactTokenLimit,
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
		Summary:   stringsx.FirstNonEmptyTrimmed(evt.Content, "Context compacted"),
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
