// Package triage — context-window usage and compaction routing.
// decodeContextWindow / encodeContextWindow normalise the provider snapshots
// that drive the composer meter; generic token-spend accounting is deliberately
// excluded because it does not represent current context occupancy.

package triage

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/stringsx"

	"github.com/google/uuid"
)

const usageEmitMinInterval = 500 * time.Millisecond

type usageEmitThrottle struct {
	lastEmittedAt time.Time
	pending       *provider.UsageEvent
}

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
	if scope := eventParentID(evt); scope != "" {
		return r.persistSubagentCompaction(evt, scope)
	}

	// The boundary is both providers' success signal, so it closes the
	// live compacting window (compaction_status.go). Claude also sends
	// an explicit close frame ~20ms earlier, making this a no-op there;
	// for Codex this IS the close (its open was item/started).
	r.clearCompacting(evt.ThreadID)

	turnIndex, err := r.currentTurnIndex(evt.ThreadID)
	if err != nil {
		return fmt.Errorf("compaction turn index: %w", err)
	}
	item, payload, err := r.compactionRow(evt, turnIndex, "")
	if err != nil {
		return err
	}
	if err := r.persistItem(item, payload); err != nil {
		return err
	}
	r.resetUsageEmitThrottle(evt.ThreadID)
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

// persistSubagentCompaction writes a SUBAGENT's compaction as a row under
// its launch, on the launch's turn, and touches nothing else. A subagent
// compacts its OWN context, which is private to it — the same reason
// handleTokenUsage drops scoped usage — so the thread's compacting
// window, its usage-emit throttle and its context meter, all of which
// describe the main agent, stay exactly where they are. Today only the
// transcript backfill (subagent_transcript.go) delivers a scoped
// boundary; the rule lives in the handler so a live one would land the
// same way.
func (r *Router) persistSubagentCompaction(evt provider.ProviderEvent, scope string) error {
	turnIndex, err := r.turnIndexForScope(evt.ThreadID, scope)
	if err != nil {
		return fmt.Errorf("subagent compaction turn index: %w", err)
	}
	item, payload, err := r.compactionRow(evt, turnIndex, scope)
	if err != nil {
		return err
	}
	return r.persistItem(item, payload)
}

// compactionRow builds a boundary's timeline row and, when the meta
// committed a summary, the on-demand payload it routes to.
func (r *Router) compactionRow(evt provider.ProviderEvent, turnIndex int, scope string) (store.Item, *store.Payload, error) {
	itemID, err := r.compactionItemID(evt, turnIndex)
	if err != nil {
		return store.Item{}, nil, err
	}

	// The claudetui provider commits the compaction summarizer's summary onto
	// the boundary meta. Lift that (potentially multi-KB) summary into an
	// on-demand payload so items.meta stays a cheap {trigger} blob (Core
	// Principle 4: heavy payloads load on demand). The summarizer's reasoning
	// streamed live as its own compaction_reasoning row, so it is not here.
	// Headless claude and Codex carry no summary, so it is empty, restMeta is
	// evt.Meta byte-for-byte, and the row persists exactly as before.
	summary, restMeta := ExtractCompactionSummary(evt.Meta)

	now := eventTimestampMillis(evt)
	item := store.Item{
		ID:        itemID,
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		Kind:      "compaction",
		Role:      "system",
		Status:    statusCompleted,
		Summary:   stringsx.FirstNonEmptyTrimmed(evt.Content, "Context compacted"),
		ParentID:  scope,
		Meta:      string(restMeta),
		CreatedAt: now,
		UpdatedAt: now,
	}
	payload := BuildCompactionPayload(summary, now)
	if payload != nil {
		item.PayloadID = payload.ID
	}
	return item, payload, nil
}

func (r *Router) compactionItemID(evt provider.ProviderEvent, turnIndex int) (string, error) {
	if NormalizeProviderCompactionID(evt.ItemID) != "" {
		return CompactionItemID(turnIndex, evt.ItemID, 0), nil
	}
	return r.nextAvailableSequencedCompactionID(evt.ThreadID, turnIndex)
}

func (r *Router) nextAvailableSequencedCompactionID(threadID string, turnIndex int) (string, error) {
	for {
		itemID := CompactionItemID(turnIndex, "", r.nextCompactionSequence(threadID, turnIndex))
		if _, ok, err := r.store.GetThreadItem(threadID, itemID); err != nil {
			return "", fmt.Errorf("compaction id lookup %s: %w", itemID, err)
		} else if !ok {
			return itemID, nil
		}
	}
}

// ExtractCompactionSummary pulls the committed summary out of a compact-
// boundary's meta (the verbatim compactMetadata the parser passes through) and
// returns the meta with that key removed. That keeps items.meta a cheap
// {trigger} blob while the heavy summary routes to a payload. When no summary
// is present — headless claude, Codex, or a context-window snapshot — it
// returns the meta unchanged byte-for-byte so trigger persistence and
// context-window decoding are untouched.
func ExtractCompactionSummary(meta json.RawMessage) (summary string, rest json.RawMessage) {
	if len(meta) == 0 {
		return "", meta
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(meta, &obj) != nil || obj == nil {
		return "", meta
	}
	summary = jsonRawString(obj["summary"])
	if summary == "" {
		return "", meta
	}
	delete(obj, "summary")
	rebuilt, err := json.Marshal(obj)
	if err != nil {
		// Couldn't rebuild a trimmed meta; fall back to the original so the
		// trigger isn't lost. The summary still routes to its payload.
		return summary, meta
	}
	return summary, rebuilt
}

// BuildCompactionPayload builds the on-demand payload for a committed
// compaction summary: preview/size in meta, the raw summary text in data
// (same shape as a thinking payload — raw text, not a JSON wrapper). Returns
// nil for an empty summary, so headless/Codex boundaries persist with no
// payload exactly as before.
func BuildCompactionPayload(summary string, now int64) *store.Payload {
	if summary == "" {
		return nil
	}
	metaJSON, err := json.Marshal(ExtractCompactionMeta(summary))
	if err != nil {
		log.Printf("triage: marshal compaction payload meta: %v", err)
		metaJSON = []byte("{}")
	}
	return &store.Payload{
		ID:        uuid.NewString(),
		Kind:      "compaction",
		Meta:      string(metaJSON),
		Data:      []byte(summary),
		CreatedAt: now,
	}
}

// jsonRawString decodes a JSON string value, returning "" when the raw is
// empty or not a string.
func jsonRawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
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
	evt := provider.UsageEvent{
		Action:                "usage",
		ThreadID:              threadID,
		UsedTokens:            window.UsedTokens,
		MaxTokens:             window.MaxTokens,
		ContextPercent:        window.UsedPercentage,
		AutoCompactPercent:    window.AutoCompactPercent,
		AutoCompactTokenLimit: window.AutoCompactTokenLimit,
		Exceeded:              window.Exceeded,
	}
	r.throttledEmitUsage(threadID, evt)
	return nil
}

// throttledEmitUsage rate-limits provider:usage emissions to at most one
// per usageEmitMinInterval per thread. The context meter changes gradually;
// updating it faster than ~2Hz has no visible benefit but costs wire bytes
// and ring buffer slots. Caller must hold NO lock (emit may fan out).
func (r *Router) throttledEmitUsage(threadID string, evt provider.UsageEvent) {
	now := time.Now()
	r.mu.Lock()
	throttle := r.usageEmitThrottles[threadID]
	if throttle == nil {
		throttle = &usageEmitThrottle{}
		r.usageEmitThrottles[threadID] = throttle
	}
	if now.Sub(throttle.lastEmittedAt) >= usageEmitMinInterval {
		throttle.lastEmittedAt = now
		throttle.pending = nil
		r.mu.Unlock()
		r.emit("provider:usage", evt)
		return
	}
	throttle.pending = &evt
	r.mu.Unlock()
}

// takeUsageEmitPendingLocked drains the pending usage event for a thread
// under r.mu. Returns the event and true if there was a pending event,
// or a zero value and false otherwise. Caller must hold r.mu and emit
// the returned event AFTER releasing the lock.
func (r *Router) takeUsageEmitPendingLocked(threadID string) (provider.UsageEvent, bool) {
	throttle := r.usageEmitThrottles[threadID]
	if throttle == nil || throttle.pending == nil {
		return provider.UsageEvent{}, false
	}
	evt := *throttle.pending
	throttle.pending = nil
	throttle.lastEmittedAt = time.Now()
	return evt, true
}

// FlushUsageEmitThrottle emits any pending throttled usage event for
// a thread. Called at turn-complete to ensure the final context-meter
// reading reaches the frontend.
func (r *Router) FlushUsageEmitThrottle(threadID string) {
	r.mu.Lock()
	evt, ok := r.takeUsageEmitPendingLocked(threadID)
	r.mu.Unlock()
	if ok {
		r.emit("provider:usage", evt)
	}
}

// resetUsageEmitThrottle clears the throttle for a thread so the next
// usage event emits immediately. Used at compaction boundaries where the
// context meter reading jumps discontinuously.
func (r *Router) resetUsageEmitThrottle(threadID string) {
	r.mu.Lock()
	delete(r.usageEmitThrottles, threadID)
	r.mu.Unlock()
}
