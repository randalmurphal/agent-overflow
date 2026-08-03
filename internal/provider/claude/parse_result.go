// Package claude — parser for `result`-type NDJSON lines. The `result`
// envelope is Claude's authoritative turn-complete signal (see
// docs/references/claude-wire.md §result). This file owns the mapping
// from the envelope into `EventTurnComplete`. Context-meter updates
// flow through `message_delta.usage` (top-level fields, the cumulative
// sum across parent iterations) in parse_stream.go. That cumulative
// value IS what the CLI's own auto-compact trigger uses
// (compactMetadata.preTokens) — verified across five production
// compactions; see
// docs/references/fixtures/claude/advisor_pretokens_correlation_20260523.summary.json.
// The `result` envelope carries the same cumulative usage in its flat
// `usage` and `modelUsage[parent_model]` fields, but emitting from
// here would duplicate the meter reading the trailing message_delta
// already pushed. The advisor's own per-call usage surfaces only via
// `result.modelUsage[advisor_model]` (and is filtered by
// parse_assistant.go advisorOnly when it arrives as a standalone
// assistant frame), never as a stray message_delta into the parent
// stream.

package claude

import (
	"encoding/json"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// parseResult converts a Claude `result` envelope into an
// `EventTurnComplete`. The payload carries the final
// assistant_message_id (tracked in-stream from the last `assistant`
// envelope), the observed `stop_reason`, the aggregated usage
// snapshot (cost/billing accounting only — not a context-meter
// signal), an `aborted: true` flag when the envelope shape indicates
// an interrupted turn, and an `error` string when the turn ended in a
// non-interrupted error. The four `error_*` subtypes (per
// SDKResultError: `error_during_execution`, `error_max_turns`,
// `error_max_budget_usd`, `error_max_structured_output_retries`) are
// the explicit error path; a `subtype=success` envelope with
// `is_error:true` covers the case where an `assistant.error` flagged
// the turn but the final summary type stayed `success`.
// `terminal_reason` is intentionally not carried into the normalized
// completion payload; the raw line remains available for
// replay/debug. (That exclusion is a decision, not an oversight — the
// hard-steer feature that would consume it is on hold. Do not "fix" it
// without reviving that scope.) `fast_mode_state` /
// `fast_mode_disabled_reason` ARE carried, but as live session state
// (WireTurnCompleteMeta.FastMode → frontend), never as turn history.
//
// Interrupted detection: Claude does not expose a `"interrupted"`
// stop_reason. The PRIMARY signal is ack correlation: the read loop
// flags the parser when the CLI acks OUR interrupt control_request
// (always before the result line — verified 6/6 on 2.1.170), and the
// next `error_during_execution` result is then the interrupt's
// termination. The `errors[]` string heuristic ("aborted"/
// "interrupted" substrings, the upstream Python SDK's approach) is
// kept as a fallback for CLI versions whose interrupt results still
// carry those marker strings — including interrupts we didn't
// originate. It does NOT rescue a lost/timed-out ack on 2.1.170:
// there the strings say "[ede_diagnostic] result_type=user ...", so
// an unacked interrupt degrades to the hard-error path (visible, not
// wedged). Interrupt wins over error: a user abort that surfaces
// through the same envelope still maps to `stop_reason="interrupted"`
// (no `meta.error`), so the working indicator clears as cancelled
// rather than as a hard failure.
//
// After emitting, the parser's lastAssistantMessageID and
// interruptAcked are cleared so neither leaks into the next turn's
// result.
func (p *Parser) parseResult(threadID string, raw map[string]json.RawMessage, now time.Time, line []byte) ([]provider.ProviderEvent, error) {
	// Extract this turn's usage as per-turn deltas (aggregate + per-model).
	// The wire's `modelUsage` / `total_cost_usd` are session-cumulative;
	// takeTurnUsage owns the snapshot subtraction — see
	// usage_accounting.go for the verified semantics.
	usage, modelUsage := p.takeTurnUsage(raw)

	subtype := readRawString(raw["subtype"])
	stopReason := readRawString(raw["stop_reason"])
	isError := readBoolValue(raw, "is_error", "isError")
	// Consume the ack on every result — a raced ack must not classify a
	// LATER turn. An acked interrupt that still produced a success
	// result (interrupt landed after the model finished) stays a
	// success.
	acked := p.takeInterruptAcked()
	aborted := detectInterrupted(subtype, raw["errors"]) || (acked && subtype == "error_during_execution")

	// Resolve error message: any error_* subtype, or a `success` envelope
	// flagged `is_error:true`. Interrupt always wins (the user explicitly
	// asked for the abort) — leave the error path alone in that case.
	var errorMessage string
	if !aborted && (isErrorSubtype(subtype) || (subtype == "success" && isError)) {
		errorMessage = firstNonEmpty(joinErrors(raw["errors"]), boundedProviderErrorMessage(readRawString(raw["result"])))
	}

	if aborted {
		stopReason = "interrupted"
	} else if errorMessage != "" || (subtype == "success" && isError) {
		stopReason = "error"
	}

	assistantMessageID := p.takeLastAssistantMessageID()
	// Drop the per-turn snapshot discriminator + recovery state at the
	// same turn boundary so a streamed id (or recovery ordinal) from this
	// turn can't affect the next (see streamedMessageIDs / recoveredBlockSeq).
	p.clearSnapshotRecoveryState()
	var usagePayload *provider.TokenUsage
	if !usage.IsZero() {
		usagePayload = &usage
	}

	// Fast-mode report: live session state the CLI restates on every
	// result, NOT turn accounting. Nil when the binary carried neither key
	// (2.1.105 has no reason field at all) — triage forwards a present
	// report to the frontend and persists nothing.
	fastMode, _ := extractFastModeStatus(raw)

	events := []provider.ProviderEvent{{
		Kind:             provider.EventTurnComplete,
		ThreadID:         threadID,
		StructuredOutput: raw["structured_output"],
		TurnComplete: &provider.WireTurnCompleteMeta{
			StopReason:         stopReason,
			AssistantMessageID: assistantMessageID,
			Usage:              usagePayload,
			ModelUsage:         modelUsage,
			Aborted:            aborted,
			ErrorMessage:       errorMessage,
			FastMode:           fastMode,
		},
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
	return boundedProviderErrorMessage(joined)
}

const maxJoinedErrorChars = 512

func boundedProviderErrorMessage(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > maxJoinedErrorChars {
		return string(r[:maxJoinedErrorChars]) + "..."
	}
	return s
}

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
// `errors[]` containing `"aborted"` or `"interrupted"` — the same
// heuristic the upstream Python SDK uses for interrupted results.
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
