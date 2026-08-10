package main

import (
	"fmt"

	"agent-overflow/internal/store"
	"agent-overflow/internal/usagecost"
)

// One pricing rule for the usage ledger.
//
// A ledger row carries either a wire-reported cost (Claude prices its own
// turns) or tokens alone (Codex reports no cost anywhere on its wire;
// claudetui's synthesized results carry none — both persist
// `cost_source='none'`). Every surface that reports dollars has to compose
// those two halves: the usage modal, a run's cost in the overlay, `run status`,
// and the workflow budget check the engine enforces with.
//
// They compose them HERE, once. A second place that priced token-only rows
// would drift from this one the first time a rate moved, and the number a
// budget is enforced against would stop being the number a human is shown.
// internal/usagecost owns the rate table; this owns the rule for which rows it
// applies to and what an unpriceable row does to a total.
type ledgerSpend struct {
	// WireUSD is what the providers themselves reported.
	WireUSD float64
	// EstimatedUSD is what the internal/usagecost rate table priced token-only
	// rows at. It is never persisted — a rate change reprices all history on the
	// next read.
	EstimatedUSD float64
	// UnpricedRows counts rows whose model resolves to no rate at all. Their
	// tokens are real and counted everywhere tokens are; their dollars are
	// missing, so a total carrying them is a LOWER BOUND and every reader of
	// this struct has to say so rather than present it as complete.
	UnpricedRows int64
}

// TotalUSD is the composed cost: what was reported plus what was priced.
func (s ledgerSpend) TotalUSD() float64 { return s.WireUSD + s.EstimatedUSD }

// Estimated reports whether TotalUSD is anything other than exactly what the
// providers reported — either because the rate table priced part of it, or
// because some rows could not be priced at all and the total is a lower bound.
// Both are the same caveat to a reader, and a total that silently OMITS rows
// must never present itself as exact.
func (s ledgerSpend) Estimated() bool { return s.EstimatedUSD != 0 || s.UnpricedRows > 0 }

// add folds one (model, cost_source) ledger group into the running total. An
// unrecognized cost_source is an error rather than a skipped group: the column
// is written by one code path with two legal values, so a third one is
// corruption that must not silently subtract from a cost total.
func (s *ledgerSpend) add(group store.UsageDetailRow) error {
	switch group.CostSource {
	case "wire":
		s.WireUSD += group.CostUSD
	case "none":
		estimate, priced := usagecost.Price(
			group.Model, group.InputTokens, group.OutputTokens,
			group.CacheReadInputTokens, group.CacheCreationInputTokens,
		)
		if !priced {
			s.UnpricedRows += group.Rows
			return nil
		}
		s.EstimatedUSD += estimate
	default:
		return fmt.Errorf("usage ledger: unexpected cost_source %q for model %q", group.CostSource, group.Model)
	}
	return nil
}

// priceUsageGroups folds every group of one aggregation into one composed cost.
func priceUsageGroups(groups []store.UsageDetailRow) (ledgerSpend, error) {
	var spend ledgerSpend
	for _, group := range groups {
		if err := spend.add(group); err != nil {
			return ledgerSpend{}, err
		}
	}
	return spend, nil
}
