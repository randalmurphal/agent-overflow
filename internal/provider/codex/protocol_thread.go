package codex

import (
	"encoding/json"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// classifyThreadNotification handles `thread/*` methods — the name-change,
// compaction, and lifecycle no-ops.
func classifyThreadNotification(threadID, method string, params json.RawMessage, now time.Time) ([]provider.ProviderEvent, bool) {
	switch method {
	case "thread/name/updated":
		name := readTopLevelString(params, "threadName")
		if name == "" {
			name = readTopLevelString(params, "name")
		}
		meta, _ := json.Marshal(map[string]string{"newTitle": name})
		return []provider.ProviderEvent{{
			Kind:      provider.EventThreadRenamed,
			ThreadID:  threadID,
			Content:   name,
			Meta:      meta,
			Timestamp: now,
		}}, true

	case "thread/compacted":
		return []provider.ProviderEvent{{
			Kind:      provider.EventCompactBoundary,
			ThreadID:  threadID,
			ItemID:    readTopLevelString(params, "compactionId"),
			Meta:      params,
			Timestamp: now,
		}}, true

	case "thread/tokenUsage/updated":
		meta := normalizeThreadTokenUsage(params)
		if len(meta) == 0 {
			return nil, true
		}
		return []provider.ProviderEvent{{
			Kind:      provider.EventTokenUsage,
			ThreadID:  threadID,
			Meta:      meta,
			Timestamp: now,
		}}, true

	case "thread/settings/updated":
		// Config reconciliation only — Session.reconcileThreadSettings
		// folds this into the observed-settings snapshot before the
		// classifier runs. Recognized here (rather than falling through)
		// so the method counts as consumed: the opt-out list in
		// notification_catalog.go is the complement of what we consume,
		// and an unrecognized method would be unsubscribed at initialize.
		return nil, true

	case "thread/started",
		"thread/status/changed",
		"thread/archived",
		"thread/unarchived",
		"thread/closed":
		return nil, true
	}
	return nil, false
}

type codexThreadTokenUsageNotification struct {
	TokenUsage codexThreadTokenUsage `json:"tokenUsage"`
}

type codexThreadTokenUsage struct {
	Last               codexTokenBreakdown `json:"last"`
	Total              codexTokenBreakdown `json:"total"`
	ModelContextWindow int                 `json:"modelContextWindow"`
}

type codexTokenBreakdown struct {
	TotalTokens int `json:"totalTokens"`
}

func normalizeThreadTokenUsage(params json.RawMessage) json.RawMessage {
	var payload codexThreadTokenUsageNotification
	if json.Unmarshal(params, &payload) != nil {
		return nil
	}
	used := payload.TokenUsage.Last.TotalTokens
	maxTokens := payload.TokenUsage.ModelContextWindow
	if used == 0 && maxTokens == 0 {
		return nil
	}
	window := provider.ContextWindow{
		UsedTokens: used,
		MaxTokens:  maxTokens,
	}
	// Sentinel: Codex's `fill_to_context_window` pegs `total.totalTokens`
	// to exactly `modelContextWindow` when the model returned
	// `ContextWindowExceeded` (see codex-rs/protocol/src/protocol.rs:2040
	// — `total_token_usage.total_tokens = context_window`, while
	// `last_token_usage.total_tokens` becomes `(window - previous_total).max(0)`,
	// a delta that is rarely == window). The meter renders this as a
	// distinct "exceeded" state. UsedPercentage is intentionally NOT set
	// here; persistAndEmitContextWindow owns the provider-aware formula.
	if maxTokens > 0 && payload.TokenUsage.Total.TotalTokens == maxTokens {
		window.Exceeded = true
	}
	meta, err := json.Marshal(window)
	if err != nil {
		return nil
	}
	return meta
}

// classifyAccountNotification handles `account/*` and `model/*` methods —
// rate-limit refreshes, model reroute signals, and the login/account
// no-ops.
func classifyAccountNotification(threadID, method string, params json.RawMessage, now time.Time) ([]provider.ProviderEvent, bool) {
	switch method {
	case "account/rateLimits/updated":
		meta := normalizeRateLimitsMeta(params, now)
		return []provider.ProviderEvent{{
			Kind:      provider.EventRateLimits,
			ThreadID:  threadID,
			Meta:      meta,
			Timestamp: now,
		}}, true

	case "model/rerouted":
		toModel := readTopLevelString(params, "toModel")
		meta, _ := json.Marshal(map[string]string{"newModel": toModel})
		return []provider.ProviderEvent{{
			Kind:      provider.EventModelRerouted,
			ThreadID:  threadID,
			Content:   toModel,
			Meta:      meta,
			Timestamp: now,
		}}, true

	case "model/verification":
		return []provider.ProviderEvent{codexNotificationEvent(threadID, "model_verification", "Model verification warning", params, now)}, true

	case "model/safetyBuffering/updated":
		return classifySafetyBuffering(threadID, params, now), true

	case "account/updated", "account/login/completed":
		return nil, true
	}
	return nil, false
}

// classifySafetyBuffering turns `model/safetyBuffering/updated` into the
// one user-facing fact it carries: the model's response is being held
// while OpenAI reviews it. Without this the UI is indistinguishable from
// a hung app during the hold (Core Principle 5 — errors and stalls are
// user-facing state, not log entries).
//
// Only the showBufferingUi=true edge produces a row. The false edge is the
// hold ending, which needs no announcement, and emitting on both would
// double every occurrence in the transcript. It still counts as handled so
// the method stays subscribed (see notification_catalog.go).
//
// Wire (`v2::ModelSafetyBufferingUpdatedNotification`): `{threadId, turnId,
// model, useCases[], reasons[], showBufferingUi, fasterModel?}`. `reasons`
// is server-authored free text rather than a closed enum
// (codex-rs/core/src/session/turn.rs passes the response's list straight
// through), so it is appended verbatim rather than mapped to our own copy.
// The summary has to be self-contained: triage persists `kind` and `title`
// for notification rows and drops the rest of the meta.
func classifySafetyBuffering(threadID string, params json.RawMessage, now time.Time) []provider.ProviderEvent {
	fields := decodeTopLevel(params)
	if !readRawBool(fields, "showBufferingUi") {
		return nil
	}
	summary := "OpenAI is reviewing this turn — the response is buffered"
	if reasons := readRawStringArray(fields, "reasons"); len(reasons) > 0 {
		summary += " (" + strings.Join(reasons, ", ") + ")"
	}
	event := codexNotificationEvent(threadID, "safety_buffering", summary, params, now)
	event.TurnID = readRawString(fields, "turnId")
	return []provider.ProviderEvent{event}
}
