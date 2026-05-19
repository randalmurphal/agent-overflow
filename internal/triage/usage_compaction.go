// Package triage — context-window usage and compaction routing.
// decodeContextWindow / encodeContextWindow normalise the provider snapshots
// that drive the composer meter; generic token-spend accounting is deliberately
// excluded because it does not represent current context occupancy.

package triage

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/stringsx"
)

const maxProviderCompactionIDLength = 420

func decodeContextWindow(raw json.RawMessage) (provider.ContextWindow, bool) {
	if len(raw) == 0 {
		return provider.ContextWindow{}, false
	}
	var window provider.ContextWindow
	if json.Unmarshal(raw, &window) == nil {
		// Require at least one real signal (tokens or percentage). An
		// `Exceeded`-only payload with no tokens is meaningless — the
		// sentinel only makes sense alongside a real `MaxTokens` reading,
		// and Codex always emits both together (`fill_to_context_window`
		// pegs both sides). Leave UsedPercentage as-is — the authoritative
		// value is computed in persistAndEmitContextWindow via the
		// provider-aware formula (Codex subtracts a 12000-token baseline;
		// Claude uses the plain ratio). Pre-computing here would force
		// decode callers to know the provider.
		if window.UsedTokens != 0 || window.MaxTokens != 0 || window.UsedPercentage != 0 {
			return window, true
		}
	}
	return provider.ContextWindow{}, false
}

func encodeContextWindow(window provider.ContextWindow) string {
	payload := map[string]any{
		"usedTokens":            window.UsedTokens,
		"maxTokens":             window.MaxTokens,
		"contextPercent":        window.UsedPercentage,
		"autoCompactPercent":    window.AutoCompactPercent,
		"autoCompactTokenLimit": window.AutoCompactTokenLimit,
	}
	if window.Exceeded {
		payload["exceeded"] = true
	}
	data, err := json.Marshal(payload)
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

	itemID, err := r.compactionItemID(evt, turnIndex)
	if err != nil {
		return err
	}
	item := store.Item{
		ID:        itemID,
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		Kind:      "compaction",
		Role:      "system",
		Status:    statusCompleted,
		Summary:   stringsx.FirstNonEmptyTrimmed(evt.Content, "Context compacted"),
		Meta:      string(evt.Meta),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := r.persistItem(item, nil); err != nil {
		return err
	}
	if window, ok := decodeContextWindow(evt.Meta); ok {
		return r.persistAndEmitContextWindow(evt.ThreadID, window)
	}
	// Compaction with no window in meta (Codex's typical case — the wire
	// notification carries the raw item-completed params, not a normalized
	// ContextWindow). Do NOT clear last_token_usage here. Codex emits a
	// fresh `thread/tokenUsage/updated` after `recompute_token_usage`
	// (`codex-rs/core/src/compact.rs:286`) which arrives shortly after the
	// compact event and overwrites the meter naturally. Clearing here
	// would flash a misleading 0% in the brief window between the two
	// notifications.
	return nil
}

func (r *Router) compactionItemID(evt provider.ProviderEvent, turnIndex int) (string, error) {
	if providerID := normalizeProviderCompactionID(evt.ItemID); providerID != "" {
		return providerCompactionID(turnIndex, providerID), nil
	}
	return r.nextAvailableSequencedCompactionID(evt.ThreadID, turnIndex)
}

func (r *Router) nextAvailableSequencedCompactionID(threadID string, turnIndex int) (string, error) {
	for {
		itemID := sequencedCompactionID(turnIndex, r.nextCompactionSequence(threadID, turnIndex))
		if _, ok, err := r.store.GetThreadItem(threadID, itemID); err != nil {
			return "", fmt.Errorf("compaction id lookup %s: %w", itemID, err)
		} else if !ok {
			return itemID, nil
		}
	}
}

func providerCompactionID(turnIndex int, providerID string) string {
	return fmt.Sprintf("compact:%d:provider:%s", turnIndex, providerID)
}

func sequencedCompactionID(turnIndex, seq int) string {
	return fmt.Sprintf("compact:%d:seq:%d", turnIndex, seq)
}

func normalizeProviderCompactionID(providerID string) string {
	trimmed := strings.TrimSpace(providerID)
	if trimmed == "" || strings.ContainsFunc(trimmed, unicode.IsControl) {
		return ""
	}
	if len(trimmed) <= maxProviderCompactionIDLength {
		return trimmed
	}
	hash := sha256.Sum256([]byte(trimmed))
	return fmt.Sprintf("sha256:%x", hash)
}

func (r *Router) persistAndEmitContextWindow(threadID string, window provider.ContextWindow) error {
	autoCompactPercent := 0
	providerName := ""
	if settings, err := r.store.GetThreadContextSettings(threadID); err == nil {
		providerName = settings.Provider
		if window.MaxTokens == 0 && settings.ContextWindow > 0 {
			window.MaxTokens = settings.ContextWindow
		}
		autoCompactPercent = provider.AutoCompactPercentForContextTier(
			provider.ContextTierForModelWindow(settings.Provider, settings.Model, settings.ContextWindow),
			settings.AutoCompactStandardPercent,
			settings.AutoCompactExtendedPercent,
		)
	}
	// Provider-aware percentage. For Codex this matches its TUI's
	// "X% context left" indicator (see provider.ComputeContextPercent
	// for the formula citation). Claude/etc. use the plain ratio.
	window.UsedPercentage = provider.ComputeContextPercent(provider.ProviderKind(providerName), window.UsedTokens, window.MaxTokens)
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
		Exceeded:              window.Exceeded,
	})
	return nil
}
