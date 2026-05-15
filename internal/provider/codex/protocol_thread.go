package codex

import (
	"encoding/json"
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

	case "account/updated", "account/login/completed":
		return nil, true
	}
	return nil, false
}
