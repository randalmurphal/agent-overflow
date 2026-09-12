// Package usageledger owns the one pricing rule the usage ledger is read
// through: which rows the internal/usagecost rate table applies to, and what an
// unpriceable row does to a total.
package usageledger

import (
	"fmt"
	"math"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/usagecost"
)

// Spend combines provider-reported estimates with standard-rate estimates
// for token-only rows. All usage surfaces and workflow budgets share this
// rule; cumulative provider thread totals are overlaid separately.
type Spend struct {
	// WireUSD is what the providers themselves reported.
	WireUSD float64
	// EstimatedUSD is what the internal/usagecost rate table priced token-only
	// rows at, settled and pending alike, at the rate in effect on each
	// group's UTC day.
	EstimatedUSD float64
	// UnpricedRows counts rows whose model resolves to no rate at all. Their
	// tokens are real and counted everywhere tokens are; their dollars are
	// missing, so a total carrying them is a LOWER BOUND and every reader of
	// this struct has to say so rather than present it as complete.
	UnpricedRows int64
}

// TotalUSD is the composed cost: what was reported plus what was priced.
func (s Spend) TotalUSD() float64 { return s.WireUSD + s.EstimatedUSD }

// Estimated reports nonzero estimated spend or missing prices. Provider-reported
// amounts are estimates too, not settled invoices.
func (s Spend) Estimated() bool { return s.WireUSD != 0 || s.EstimatedUSD != 0 || s.UnpricedRows > 0 }

// Add folds one (model, cost_source, day) ledger group into the running
// total. An unrecognized cost_source is an error rather than silently missing
// cost. Pending groups are in-flight tokens: they are priced like settled
// token-only rows and replaced by the turn's final accounting at settlement.
func (s *Spend) Add(group store.UsageDetailRow) error {
	if group.CostUSD < 0 || math.IsNaN(group.CostUSD) || math.IsInf(group.CostUSD, 0) {
		return fmt.Errorf("usage ledger: invalid cost for model %q", group.Model)
	}
	switch group.CostSource {
	case "wire":
		s.WireUSD += group.CostUSD
	case "none", "pending":
		estimate, priced := usagecost.Price(
			group.Model, time.UnixMilli(group.Day).UTC(), group.InputTokens, group.OutputTokens,
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

// PriceGroups folds every group of one aggregation into one composed cost.
func PriceGroups(groups []store.UsageDetailRow) (Spend, error) {
	var spend Spend
	for _, group := range groups {
		if err := spend.Add(group); err != nil {
			return Spend{}, err
		}
	}
	return spend, nil
}
