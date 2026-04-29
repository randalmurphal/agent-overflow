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
// usage snapshot, and an `aborted: true` flag when the envelope shape
// indicates an interrupted turn.
//
// Interrupted detection: Claude does not expose a `"interrupted"`
// stop_reason. Interruption surfaces as `subtype=error_during_execution`
// + `errors[]` containing "aborted" or "interrupted" (see forge's
// sdkMessageParsing.ts reference and the Python SDK's SDKResultError
// shape, which uses `errors: string[]` — there is no `error`
// (singular) field on the wire).
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
	aborted := detectInterrupted(subtype, raw["errors"])
	if aborted {
		stopReason = "interrupted"
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
