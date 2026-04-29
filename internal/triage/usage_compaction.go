// Package triage — context-window usage and compaction routing.
// decodeContextWindow / encodeContextWindow normalise the provider snapshots
// that drive the composer meter; generic token-spend accounting is deliberately
// excluded because it does not represent current context occupancy.

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
	var window provider.ContextWindow
	if json.Unmarshal(raw, &window) == nil {
		if window.UsedTokens != 0 || window.MaxTokens != 0 || window.UsedPercentage != 0 {
			if window.UsedPercentage == 0 && window.MaxTokens > 0 {
				window.UsedPercentage = float64(window.UsedTokens) / float64(window.MaxTokens) * 100
			}
			return window, true
		}
	}
	return provider.ContextWindow{}, false
}

func encodeContextWindow(window provider.ContextWindow) string {
	data, err := json.Marshal(map[string]any{
		"usedTokens":            window.UsedTokens,
		"maxTokens":             window.MaxTokens,
		"contextPercent":        window.UsedPercentage,
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
	if window, ok := decodeContextWindow(evt.Meta); ok {
		return r.persistAndEmitContextWindow(evt.ThreadID, window)
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

func (r *Router) persistAndEmitContextWindow(threadID string, window provider.ContextWindow) error {
	autoCompactPercent := 0
	if settings, err := r.store.GetThreadContextSettings(threadID); err == nil {
		if window.MaxTokens == 0 && settings.ContextWindow > 0 {
			window.MaxTokens = settings.ContextWindow
		}
		if window.MaxTokens > 0 {
			window.UsedPercentage = float64(window.UsedTokens) / float64(window.MaxTokens) * 100
		}
		autoCompactPercent = provider.AutoCompactPercentForContextTier(
			provider.ContextTierForModelWindow(settings.Provider, settings.Model, settings.ContextWindow),
			settings.AutoCompactStandardPercent,
			settings.AutoCompactExtendedPercent,
		)
	}
	if autoCompactPercent == 0 {
		autoCompactPercent = 90
	}
	window.AutoCompactPercent = autoCompactPercent
	if autoCompactPercent > 0 && window.MaxTokens > 0 {
		window.AutoCompactTokenLimit = window.MaxTokens * autoCompactPercent / 100
	}
	if err := r.store.UpdateLastTokenUsage(threadID, encodeContextWindow(window)); err != nil {
		return fmt.Errorf("token usage persist: %w", err)
	}
	r.emit("provider:usage", provider.UsageEvent{
		Action:                "usage",
		ThreadID:              threadID,
		UsedTokens:            window.UsedTokens,
		MaxTokens:             window.MaxTokens,
		ContextPercent:        window.UsedPercentage,
		AutoCompactPercent:    window.AutoCompactPercent,
		AutoCompactTokenLimit: window.AutoCompactTokenLimit,
	})
	return nil
}
