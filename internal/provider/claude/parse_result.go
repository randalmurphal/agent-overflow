// Package claude — parser for `result`-type NDJSON lines. The `result`
// envelope is Claude's authoritative turn-complete signal (see
// docs/references/claude-wire.md §result). This file owns the mapping
// from the envelope into `EventTurnComplete`; context-meter updates come
// only from top-level assistant/message_delta usage snapshots.

package claude

import (
	"encoding/json"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// parseResult converts a Claude `result` envelope into an
// `EventTurnComplete`. The turn-complete event's Meta carries the final
// assistant_message_id (tracked in-stream from the last `assistant`
// envelope), the observed `stop_reason`, the duration, total cost, the
// usage snapshot, an `aborted: true` flag when the envelope shape
// indicates an interrupted turn, and an `error` string when the turn
// ended in a non-interrupted error. The four `error_*` subtypes (per
// SDKResultError: `error_during_execution`, `error_max_turns`,
// `error_max_budget_usd`, `error_max_structured_output_retries`) are
// the explicit error path; a `subtype=success` envelope with
// `is_error:true` covers the case where an `assistant.error` flagged
// the turn but the final summary type stayed `success`. `terminal_reason`
// (12 enum values, see docs/references/claude-wire.md) is forwarded on
// meta for telemetry and forward-compat — triage does not branch on it.
//
// Interrupted detection: Claude does not expose a `"interrupted"`
// stop_reason. Interruption surfaces as `subtype=error_during_execution`
// + `errors[]` containing "aborted" or "interrupted" (see forge's
// sdkMessageParsing.ts reference and the Python SDK's SDKResultError
// shape, which uses `errors: string[]` — there is no `error`
// (singular) field on the wire). Interrupt wins over error: a user
// abort that surfaces through the same envelope still maps to
// `stop_reason="interrupted"` (no `meta.error`), so the working
// indicator clears as cancelled rather than as a hard failure.
//
// After emitting, the parser's lastAssistantMessageID is cleared so it
// cannot leak into the next turn's result.
func (p *Parser) parseResult(threadID string, raw map[string]json.RawMessage, now time.Time, line []byte) ([]provider.ProviderEvent, error) {
	model := p.currentModel()

	// Extract usage/cost data from the result summary. If the wire
	// didn't include a cost and we know the model, price the usage
	// here for turn-complete accounting only. Do not emit this as a
	// context-meter update; Claude's result.usage is cumulative across
	// API calls in the turn.
	usage := extractResultUsage(raw)
	if usage.InputTokens > 0 || usage.OutputTokens > 0 {
		if usage.TotalCostUSD == 0 && model != "" {
			usage.TotalCostUSD = provider.CalculateCost(model, usage)
		}
	}

	// Build the turn-complete meta. Every field is optional from the
	// wire side; we only include fields that were present so the
	// resulting JSON stays compact and triage doesn't have to test
	// zero-vs-unset.
	subtype := readRawString(raw["subtype"])
	stopReason := readRawString(raw["stop_reason"])
	isError := readBoolValue(raw, "is_error", "isError")
	aborted := detectInterrupted(subtype, raw["errors"])

	// Resolve error message: any error_* subtype, or a `success` envelope
	// flagged `is_error:true`. Interrupt always wins (the user explicitly
	// asked for the abort) — leave the error path alone in that case.
	var errorMessage string
	if !aborted && (isErrorSubtype(subtype) || (subtype == "success" && isError)) {
		errorMessage = joinErrors(raw["errors"])
	}

	if aborted {
		stopReason = "interrupted"
	} else if errorMessage != "" {
		stopReason = "error"
	}

	assistantMessageID := p.takeLastAssistantMessageID()

	metaFields := map[string]any{}
	if stopReason != "" {
		metaFields["stop_reason"] = stopReason
	}
	if assistantMessageID != "" {
		metaFields["assistant_message_id"] = assistantMessageID
	}
	if usage.InputTokens > 0 || usage.OutputTokens > 0 {
		metaFields["usage"] = usage
	}
	if duration, ok := readIntValue(raw, "duration_ms", "durationMs"); ok {
		metaFields["duration_ms"] = duration
	}
	if cost, ok := readFloatValue(raw, "total_cost_usd", "totalCostUsd"); ok {
		metaFields["total_cost_usd"] = cost
	}
	if aborted {
		metaFields["aborted"] = true
	}
	if errorMessage != "" {
		metaFields["error"] = errorMessage
	}
	if subtype != "" {
		metaFields["subtype"] = subtype
	}
	if terminalReason := readRawString(raw["terminal_reason"]); terminalReason != "" {
		metaFields["terminal_reason"] = terminalReason
	}

	var turnMeta json.RawMessage
	if len(metaFields) > 0 {
		if m, err := json.Marshal(metaFields); err == nil {
			turnMeta = m
		}
	}

	events := []provider.ProviderEvent{{
		Kind:      provider.EventTurnComplete,
		ThreadID:  threadID,
		Meta:      turnMeta,
		Timestamp: now,
		Raw:       line,
	}}

	return events, nil
}

// isErrorSubtype reports whether a `result` envelope's `subtype` is
// one of the four documented error subtypes from the Python agent SDK
// (SDKResultError). Keeping the list explicit (rather than a
// `strings.HasPrefix("error_")` check) makes a new SDK error subtype a
// visible parser change and keeps unrelated `error*` subtypes from
// silently rerouting through this branch.
func isErrorSubtype(subtype string) bool {
	switch subtype {
	case "error_during_execution",
		"error_max_turns",
		"error_max_budget_usd",
		"error_max_structured_output_retries":
		return true
	}
	return false
}

// joinErrors flattens the `errors[]` array on a `result` envelope into
// a single human-readable message. The wire shape is normally an array
// of strings, but tolerate an array of objects (just stringify the raw
// JSON entry) so a future SDK schema change doesn't blank out the
// error copy. Empty arrays return an empty string — callers should
// fall back to a generic message in that case.
//
// The result is capped at maxJoinedErrorChars so a malformed envelope
// with many or long entries can't produce a multi-KB summary string.
// Triage rows render this verbatim; an unbounded length distorts
// timeline layout even though Svelte autoescapes content.
func joinErrors(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var joined string
	var asStrings []string
	if json.Unmarshal(raw, &asStrings) == nil {
		joined = joinNonEmpty(asStrings, "; ")
	} else {
		var asAny []json.RawMessage
		if json.Unmarshal(raw, &asAny) == nil {
			out := make([]string, 0, len(asAny))
			for _, entry := range asAny {
				s := strings.TrimSpace(string(entry))
				if s == "" {
					continue
				}
				out = append(out, s)
			}
			joined = joinNonEmpty(out, "; ")
		}
	}
	if r := []rune(joined); len(r) > maxJoinedErrorChars {
		return string(r[:maxJoinedErrorChars]) + "..."
	}
	return joined
}

const maxJoinedErrorChars = 512

func joinNonEmpty(parts []string, sep string) string {
	out := make([]string, 0, len(parts))
	for _, s := range parts {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		out = append(out, s)
	}
	return strings.Join(out, sep)
}

// detectInterrupted tests whether a non-error `result` envelope is
// really an interrupted turn. The shape we key on is
// `subtype=error_during_execution` combined with any entry in
// `errors[]` containing `"aborted"` or `"interrupted"` — lifted from
// forge's sdkMessageParsing.ts reference so both adapters agree on
// the heuristic.
func detectInterrupted(subtype string, errorsRaw json.RawMessage) bool {
	if subtype != "error_during_execution" {
		return false
	}
	if len(errorsRaw) == 0 {
		return false
	}

	// `errors` is typically an array of strings; tolerate an array of
	// objects by stringifying each entry.
	var asStrings []string
	if json.Unmarshal(errorsRaw, &asStrings) == nil {
		for _, s := range asStrings {
			if looksInterrupted(s) {
				return true
			}
		}
		return false
	}

	var asAny []json.RawMessage
	if json.Unmarshal(errorsRaw, &asAny) == nil {
		for _, entry := range asAny {
			if looksInterrupted(string(entry)) {
				return true
			}
		}
	}
	return false
}

func looksInterrupted(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, "aborted") || strings.Contains(lower, "interrupted")
}

// extractResultUsage parses token usage from a Claude result message.
// It checks both "usage" (flat format) and "modelUsage" (per-model format)
// and aggregates total_cost_usd when present.
func extractResultUsage(raw map[string]json.RawMessage) provider.TokenUsage {
	var usage provider.TokenUsage

	// Try flat "usage" object first.
	if v, ok := raw["usage"]; ok {
		var u struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		}
		if json.Unmarshal(v, &u) == nil {
			usage.InputTokens = u.InputTokens
			usage.OutputTokens = u.OutputTokens
			usage.CacheReadInputTokens = u.CacheReadInputTokens
			usage.CacheCreationInputTokens = u.CacheCreationInputTokens
		}
	}

	// Aggregate from "modelUsage" if flat usage was empty.
	if usage.InputTokens == 0 && usage.OutputTokens == 0 {
		if v, ok := raw["modelUsage"]; ok {
			var models map[string]struct {
				InputTokens              int     `json:"inputTokens"`
				OutputTokens             int     `json:"outputTokens"`
				CacheReadInputTokens     int     `json:"cacheReadInputTokens"`
				CacheCreationInputTokens int     `json:"cacheCreationInputTokens"`
				CostUSD                  float64 `json:"costUSD"`
			}
			if json.Unmarshal(v, &models) == nil {
				for _, m := range models {
					usage.InputTokens += m.InputTokens
					usage.OutputTokens += m.OutputTokens
					usage.CacheReadInputTokens += m.CacheReadInputTokens
					usage.CacheCreationInputTokens += m.CacheCreationInputTokens
					usage.TotalCostUSD += m.CostUSD
				}
			}
		}
	}

	// Override cost with explicit total_cost_usd if present.
	if v, ok := raw["total_cost_usd"]; ok {
		var cost float64
		if json.Unmarshal(v, &cost) == nil && cost > 0 {
			usage.TotalCostUSD = cost
		}
	}

	return usage
}
