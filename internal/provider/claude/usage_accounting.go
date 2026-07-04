package claude

import (
	"encoding/json"
	"sort"

	"agent-overflow/internal/provider"
)

// Per-turn usage accounting for `result` envelopes.
//
// Verified 2026-07-03 against live captures (fixtures
// docs/references/fixtures/claude/multiturn_cost_cumulative_20260703.ndjson
// and subagent_usage_inclusion_20260703.ndjson):
//
//   - `result.total_cost_usd` and `result.modelUsage` are SESSION-CUMULATIVE
//     across turns within one CLI process (in=10→20→30 across three turns).
//     Summing them per turn double-counts; per-turn truth is the delta
//     between consecutive snapshots.
//   - `result.modelUsage` INCLUDES Task-subagent (sidechain) usage and
//     carries a per-model `costUSD` computed by the CLI itself.
//   - Flat `result.usage` is genuinely per-turn but PARENT-ONLY — it
//     excludes subagent tokens, so it is ONLY the fallback when
//     `modelUsage` is absent from the envelope entirely (claudetui's
//     synthesized result envelopes). A present-but-all-zero-delta
//     `modelUsage` (e.g. a replayed/duplicated result for an
//     already-accounted turn) must NOT fall through to flat usage —
//     the flat object is still session-cumulative-derived per-turn
//     data and would double-count against the modelUsage path already
//     having advanced the snapshot.
//
// The parser therefore keeps a cumulative modelUsage snapshot per process
// and emits per-model deltas on each result. A fresh process (including
// `--resume`) restarts the CLI's cumulative counters at zero, so the
// zero-value snapshot is always the correct baseline. Interrupted turns
// carry an empty `modelUsage` and naturally produce no delta.
//
// Cost is wire-reported only. There is deliberately no client-side
// pricing table: when the CLI does not report cost (claudetui synthetic
// results carry no total_cost_usd / modelUsage), tokens persist and cost
// stays 0.

// wireModelUsage is the per-model shape inside `result.modelUsage`.
// Field casing on the wire is camelCase (unlike the flat `usage` object).
type wireModelUsage struct {
	InputTokens              int     `json:"inputTokens"`
	OutputTokens             int     `json:"outputTokens"`
	CacheReadInputTokens     int     `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int     `json:"cacheCreationInputTokens"`
	CostUSD                  float64 `json:"costUSD"`
}

func (w wireModelUsage) toTokenUsage() provider.TokenUsage {
	return provider.TokenUsage{
		InputTokens:              w.InputTokens,
		OutputTokens:             w.OutputTokens,
		CacheReadInputTokens:     w.CacheReadInputTokens,
		CacheCreationInputTokens: w.CacheCreationInputTokens,
		TotalCostUSD:             w.CostUSD,
	}
}

// takeTurnUsage extracts this turn's usage from a `result` envelope as
// per-turn DELTAS: the aggregate across models plus the per-model split.
// It consumes and advances the parser's cumulative snapshot state — call
// exactly once per result envelope (parseResult is the single owner).
//
// Safe on a nil parser (package-level ParseLine helper): deltas compute
// against an empty snapshot and no state is retained.
func (p *Parser) takeTurnUsage(raw map[string]json.RawMessage) (provider.TokenUsage, []provider.ModelTokenUsage) {
	perModel, present := p.takeModelUsageDeltas(raw["modelUsage"])
	if present {
		var agg provider.TokenUsage
		for _, m := range perModel {
			agg.Add(m.TokenUsage)
		}
		// Keep the cumulative-cost tracker in sync even on the modelUsage
		// path so a later envelope that lacks modelUsage (mixed-version
		// weirdness) can't re-attribute already-counted cost.
		p.advanceAccountedCost(readRawFloat(raw["total_cost_usd"]))
		if len(perModel) == 0 {
			// modelUsage was present but every model's delta was zero (a
			// replayed/duplicated result for an already-accounted turn,
			// or every model genuinely produced nothing new). The flat
			// `usage` fallback below is ONLY for envelopes that omit
			// modelUsage entirely — falling through here would re-emit
			// the flat parent-only usage as a fresh delta and double
			// count. Report nothing; the accounted-cost tracker above
			// already advanced.
			return provider.TokenUsage{}, nil
		}
		return agg, perModel
	}
	return p.takeFlatUsageDelta(raw)
}

// takeModelUsageDeltas parses `result.modelUsage` (cumulative) and returns
// the per-model deltas against the parser's snapshot, advancing the
// snapshot to the new cumulative values. Zero-delta models are skipped
// from the returned slice, but `present` still reports true so callers
// can distinguish "modelUsage was here and simply had nothing new" from
// "modelUsage was absent" — only the latter should fall back to flat
// usage. Negative deltas (cumulative moved backwards — never observed on
// the wire) clamp to zero rather than corrupting downstream sums.
func (p *Parser) takeModelUsageDeltas(rawModelUsage json.RawMessage) (deltas []provider.ModelTokenUsage, present bool) {
	if len(rawModelUsage) == 0 {
		return nil, false
	}
	var models map[string]wireModelUsage
	if json.Unmarshal(rawModelUsage, &models) != nil || len(models) == 0 {
		return nil, false
	}

	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	sort.Strings(names)

	deltas = make([]provider.ModelTokenUsage, 0, len(models))
	for _, name := range names {
		cumulative := models[name].toTokenUsage()
		var prev provider.TokenUsage
		if p != nil {
			prev = p.usageTotalsByModel[name]
		}
		delta := subtractUsageClamped(cumulative, prev)
		if p != nil {
			if p.usageTotalsByModel == nil {
				p.usageTotalsByModel = make(map[string]provider.TokenUsage)
			}
			p.usageTotalsByModel[name] = cumulative
		}
		if delta.IsZero() {
			continue
		}
		deltas = append(deltas, provider.ModelTokenUsage{Model: name, TokenUsage: delta})
	}
	return deltas, true
}

// takeFlatUsageDelta is the fallback when `modelUsage` is absent —
// claudetui's synthesized result envelopes carry only a flat per-turn
// `usage` (and no cost). Flat usage is already per-turn on the wire so
// only the cumulative `total_cost_usd` needs delta treatment. The turn
// attributes to the parser's tracked model as a single entry.
func (p *Parser) takeFlatUsageDelta(raw map[string]json.RawMessage) (provider.TokenUsage, []provider.ModelTokenUsage) {
	var usage provider.TokenUsage
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
	usage.TotalCostUSD = p.advanceAccountedCost(readRawFloat(raw["total_cost_usd"]))

	if usage.IsZero() {
		return provider.TokenUsage{}, nil
	}
	return usage, []provider.ModelTokenUsage{{Model: p.currentModel(), TokenUsage: usage}}
}

// advanceAccountedCost moves the cumulative-cost tracker to the wire's
// session-cumulative `total_cost_usd` and returns the not-yet-attributed
// delta. A missing or zero wire value returns 0 and leaves the tracker
// alone (interrupted results report 0, which does not mean "reset").
func (p *Parser) advanceAccountedCost(cumulativeCostUSD float64) float64 {
	if cumulativeCostUSD <= 0 || p == nil {
		return 0
	}
	delta := cumulativeCostUSD - p.usageAccountedCostUSD
	if delta < 0 {
		delta = 0
	}
	p.usageAccountedCostUSD = cumulativeCostUSD
	return delta
}

// subtractUsageClamped returns cur - prev with every field clamped at 0.
func subtractUsageClamped(cur, prev provider.TokenUsage) provider.TokenUsage {
	return provider.TokenUsage{
		InputTokens:              max(cur.InputTokens-prev.InputTokens, 0),
		OutputTokens:             max(cur.OutputTokens-prev.OutputTokens, 0),
		CacheReadInputTokens:     max(cur.CacheReadInputTokens-prev.CacheReadInputTokens, 0),
		CacheCreationInputTokens: max(cur.CacheCreationInputTokens-prev.CacheCreationInputTokens, 0),
		TotalCostUSD:             max(cur.TotalCostUSD-prev.TotalCostUSD, 0),
	}
}

// readRawFloat decodes a JSON number, returning 0 for empty or non-number.
func readRawFloat(raw json.RawMessage) float64 {
	if len(raw) == 0 {
		return 0
	}
	var f float64
	if json.Unmarshal(raw, &f) != nil {
		return 0
	}
	return f
}
