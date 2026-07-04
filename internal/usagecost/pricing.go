package usagecost

import "strings"

// Rate holds per-million-token pricing for a model family.
type Rate struct {
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64
}

// knownRates maps model slug prefixes to per-million-token pricing.
// matchRate resolves a full model slug to one of these families via
// exact match first, then progressive suffix trimming — see matchRate.
//
// Pricing decisions baked into this table:
//
//   - Claude cache-WRITE rates below are the 1-HOUR-TTL rate (2x the
//     input rate), not the 5-minute rate (1.25x). Claude Code pins 1h
//     cache in practice: the 2026-07-03 capture in
//     docs/references/fixtures/claude/multiturn_cost_cumulative_20260703.ndjson
//     shows 100% of cache-creation tokens landing in
//     ephemeral_1h_input_tokens (ephemeral_5m_input_tokens is 0
//     throughout). The usage_ledger only stores the total
//     cache_creation_input_tokens count, not a per-TTL breakdown, so
//     there is no way to price a 5m-tier request separately even on
//     the rare session that used one — pricing everything at the 1h
//     rate is the best available approximation.
//   - There is no 200k-context-tier rate. The ledger stores per-turn
//     token totals, not per-request context sizes, so a >200k-token
//     request can't be identified after the fact and priced at its
//     higher tier. This under-prices large-context requests; accepted
//     because the ledger schema cannot support better without storing
//     per-request context size.
//   - This table only prices usage_ledger rows with cost_source='none'
//     (Codex, claudetui). Claude's wire-reported cost_usd passes
//     through untouched and is never repriced here.
//   - OpenAI CacheWrite rates are 0: OpenAI does not bill cache writes.
var knownRates = map[string]Rate{
	"claude-fable": {Input: 10.00, Output: 50.00, CacheRead: 1.00, CacheWrite: 20.00},
	"claude-opus":  {Input: 5.00, Output: 25.00, CacheRead: 0.50, CacheWrite: 10.00},
	// Introductory Sonnet 5 pricing, in effect through 2026-08-31; from
	// 2026-09-01 it moves to the claude-sonnet family rates below —
	// delete this entry then.
	"claude-sonnet-5": {Input: 2.00, Output: 10.00, CacheRead: 0.20, CacheWrite: 4.00},
	"claude-sonnet":   {Input: 3.00, Output: 15.00, CacheRead: 0.30, CacheWrite: 6.00},
	"claude-haiku":    {Input: 1.00, Output: 5.00, CacheRead: 0.10, CacheWrite: 2.00},

	"gpt-5.2-codex": {Input: 1.75, Output: 14.00, CacheRead: 0.175},
	"gpt-5.1-codex": {Input: 1.75, Output: 14.00, CacheRead: 0.175},
	// "gpt-5-codex" is meant as the family-level fallback for
	// gpt-5.x-codex variants without an explicit per-version entry, but
	// it does NOT actually catch future dotted versions automatically:
	// matchRate's suffix trim cuts at the last "-" or "." character each
	// round, so "gpt-5.3-codex" resolves "gpt-5.3-codex" -> "gpt-5.3" ->
	// "gpt-5" — landing on the plain "gpt-5" family rate below, never
	// this entry. A new dotted -codex version needs its own explicit
	// entry (like gpt-5.2-codex / gpt-5.1-codex above) the day it ships.
	"gpt-5-codex": {Input: 1.75, Output: 14.00, CacheRead: 0.175},

	"gpt-5.5":      {Input: 5.00, Output: 30.00, CacheRead: 0.50},
	"gpt-5.4":      {Input: 2.50, Output: 15.00, CacheRead: 0.25},
	"gpt-5.4-mini": {Input: 0.75, Output: 4.50, CacheRead: 0.075},
	// "gpt-5" is the family-level fallback for GPT-5 variants without an
	// explicit per-version entry (for example a future gpt-5.6 until/if
	// we add a distinct entry).
	"gpt-5": {Input: 1.25, Output: 10.00, CacheRead: 0.125},

	"o3":      {Input: 1.75, Output: 14.00, CacheRead: 0.175},
	"o4-mini": {Input: 0.25, Output: 2.00, CacheRead: 0.025},
}

// Price computes the USD cost for a model's token usage from the
// hardcoded rate table above. ok is false when the model resolves to no
// known pricing family; callers should count those tokens as unpriced
// rather than treat the 0 result as a real price. Reasoning tokens are
// already included in output tokens on Codex, so there is no separate
// reasoning rate.
func Price(model string, inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens int64) (costUSD float64, ok bool) {
	rate, ok := matchRate(model)
	if !ok {
		return 0, false
	}

	const million = 1_000_000.0
	cost := float64(inputTokens)/million*rate.Input +
		float64(outputTokens)/million*rate.Output +
		float64(cacheReadTokens)/million*rate.CacheRead +
		float64(cacheWriteTokens)/million*rate.CacheWrite
	return cost, true
}

// matchRate finds pricing for a model slug: exact match first, then
// progressive prefix matching by trimming the trailing "-" or "."
// segment each round, until a known family prefix matches. This lets
// "claude-sonnet-4-6" resolve to "claude-sonnet" and "gpt-5.6" resolve
// to "gpt-5" with the same algorithm.
func matchRate(model string) (Rate, bool) {
	// Drop a trailing context-tier marker ("claude-sonnet-5[1m]" ->
	// "claude-sonnet-5") — the wire slug carries it for 1M-context
	// sessions, but long context bills at standard per-token rates, and
	// leaving it on would skip exact per-version entries.
	if open := strings.IndexByte(model, '['); open > 0 && strings.HasSuffix(model, "]") {
		model = model[:open]
	}

	// Try exact match first.
	if r, ok := knownRates[model]; ok {
		return r, true
	}

	// Progressive prefix matching: trim the trailing "-" or "." segment
	// each round until a known family prefix matches.
	candidate := model
	for {
		cut := strings.LastIndexAny(candidate, "-.")
		if cut <= 0 {
			break
		}
		candidate = candidate[:cut]
		if r, ok := knownRates[candidate]; ok {
			return r, true
		}
	}

	return Rate{}, false
}
