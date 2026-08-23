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

	case "thread/queue/changed":
		// Codex 0.148. Something OUTSIDE this app-server connection changed
		// this thread's external queue — in practice `codex queue --thread
		// <uuid> --message <text>` (codex-rs/cli/src/queue_cmd.rs), which
		// writes a row into `state_5.sqlite` that AO's own app-server then
		// picks up. See AGENTS.md §"Externally queued turns" for the full
		// mechanism and why AO cannot ignore it.
		//
		// `ThreadQueueChangedNotification` is `{threadId}` and NOTHING ELSE
		// (codex-rs/app-server-protocol/src/protocol/v2/thread.rs @
		// rust-v0.149.0) — there is no depth, no item id, no message text.
		//
		// So this event is the answer for a connection that cannot ask:
		// classification is pure and version-blind, and on an app-server
		// below 0.148 there is no `thread/queue/list` to ask with. The notice
		// therefore says only what the notification knows, and the injected
		// turn carries the content moments later.
		//
		// On a queue-native session the SESSION layer drops this event and
		// replaces it with an evidence-driven one built from a
		// `thread/queue/list` diffed against AO's own client ids — see
		// thread_queue.go — because there the change is more often AO's own
		// write than a foreign producer's.
		return []provider.ProviderEvent{{
			Kind:     provider.EventNotification,
			ThreadID: threadID,
			Content:  externalQueueNoticeText,
			Meta: mergeMetaKeys(params, map[string]any{
				"kind":   "external_queue",
				"title":  externalQueueNoticeText,
				"origin": ExternalTurnOriginQueue,
			}),
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

// externalQueueNoticeText is the transcript notice a `thread/queue/changed`
// raises. Deliberately not a warning kind: nothing is wrong, the user (or a
// script of theirs) queued work from another surface and the turn is about to
// appear on its own.
const externalQueueNoticeText = "A message was queued for this thread from outside Agent Overflow"

type codexThreadTokenUsageNotification struct {
	TokenUsage codexThreadTokenUsage `json:"tokenUsage"`
}

type codexThreadTokenUsage struct {
	Last               codexWireTokenBreakdown `json:"last"`
	Total              codexWireTokenBreakdown `json:"total"`
	ModelContextWindow int                     `json:"modelContextWindow"`
}

// childAgentTokenSpend reads a `thread/tokenUsage/updated` for a SPAWNED
// CHILD and returns the number an agent card shows: the child's TRUE
// CUMULATIVE spend, every token it ever caused to be processed, counted
// once.
//
//	all fresh input + all cache writes + all output
//
// Every term is a `total.*` cumulative the provider only ever grows, so
// the figure is MONOTONIC — it cannot go backwards when the child
// compacts its own context (user ruling 2026-08-23).
//
// Wire mapping: Codex's `inputTokens` INCLUDES `cachedInputTokens` but
// NOT `cacheWriteInputTokens` (see usage_accounting.go), so fresh input
// is `total.inputTokens - total.cachedInputTokens` and the cache writes
// are added separately; `outputTokens` already includes
// `reasoningOutputTokens`, so `total.outputTokens` is the whole
// generated side and must not have reasoning added on top.
//
// Deliberately NOT `total.totalTokens`, which is what this used to send.
// That figure re-counts the cached prompt every round: a real 42-minute
// child measured 4,570,684 there against 209,724 of actual spend — 22x,
// and it grows with round count rather than with work done.
//
// Deliberately NOT normalizeThreadTokenUsage's number either: that one
// is `last.totalTokens` because it answers "how full is the context
// window right now". A child's window is nobody's meter.
//
// Relation to Claude, since one card component renders both. Claude's
// `system/task_progress` `usage.total_tokens` is LATEST input plus all
// output — the 2.1.237 bundle's accumulator overwrites its input term
// each assistant message (`latestInputTokens = input + cache_creation +
// cache_read`) and only `+=`s output. That is the same quantity right up
// until a compaction, because summing each round's FRESH input is how the
// current context got its size; after one, Claude's dips and this does
// not. Claude cannot be given the same treatment: its envelope is
// `{total_tokens, tool_uses, duration_ms}` with no breakdown to
// accumulate, so it stays half-cumulative by force. On the same 42-minute
// child the two differ by 1.2% (209,724 here vs 207,202 Claude-shaped).
//
// ok=false when the frame carried nothing usable, so a malformed or empty
// notification leaves whatever the card already had rather than resetting
// its counter to zero.
func childAgentTokenSpend(params json.RawMessage) (int64, bool) {
	var payload codexThreadTokenUsageNotification
	if json.Unmarshal(params, &payload) != nil {
		return 0, false
	}
	total := payload.TokenUsage.Total
	// Clamped rather than trusted: the subtraction is only meaningful
	// because upstream nests cached inside input, and an inverted frame
	// would otherwise subtract real output out of the card.
	fresh := total.InputTokens - total.CachedInputTokens
	if fresh < 0 {
		fresh = 0
	}
	spend := fresh + total.CacheWriteInputTokens + total.OutputTokens
	if spend <= 0 {
		return 0, false
	}
	return int64(spend), true
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
