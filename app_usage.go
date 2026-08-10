package main

import (
	"fmt"

	"agent-overflow/internal/store"
)

// GetUsageStats aggregates the append-only usage ledger (per-turn,
// per-model token/cost rows written on turn settlement). The query
// selects time range, filters, and bucket dimension — "" (lifetime),
// "day" / "week" / "month" (calendar buckets shifted by
// TZOffsetMinutes), or "model" / "provider" / "thread" / "project".
//
// Claude's wire-reported cost_usd passes through untouched. Rows with
// no wire cost (Codex, claudetui — cost_source='none') are priced here,
// at query time, from the hardcoded internal/usagecost rate table. The
// estimate is never persisted back to usage_ledger: a future rate-table
// update reprices all history the next time this runs. Buckets with
// UnpricedRows > 0 carry a model the rate table doesn't recognize —
// the UI should label those buckets' CostUSD as a lower bound.
// Not a LocalOnlyMethods entry: read-only aggregate data, safe for
// remote clients.
func (a *App) GetUsageStats(query store.UsageQuery) ([]store.UsageBucket, error) {
	if a.store == nil {
		return nil, fmt.Errorf("usage stats: store unavailable")
	}

	// Wire clients can send any int for TZOffsetMinutes; clamp to the
	// real range of UTC offsets (UTC-14..UTC+14) so a bogus value can't
	// produce nonsense calendar buckets in usageBucketExpr's raw
	// arithmetic shift.
	query.TZOffsetMinutes = clampTZOffsetMinutes(query.TZOffsetMinutes)

	buckets, err := a.store.QueryUsage(query)
	if err != nil {
		return nil, fmt.Errorf("usage stats: %w", err)
	}

	details, err := a.store.QueryUsageDetail(query)
	if err != nil {
		return nil, fmt.Errorf("usage stats detail: %w", err)
	}

	byBucket := make(map[string]*store.UsageBucket, len(buckets))
	for i := range buckets {
		byBucket[buckets[i].Bucket] = &buckets[i]
	}

	// Composed through the one ledger pricing rule (app_usage_pricing.go), the
	// same one the workflow budget check enforces with — a bucket's dollars and
	// a run's dollars are the same arithmetic over the same rate table.
	spendByBucket := make(map[string]*ledgerSpend, len(buckets))
	for _, d := range details {
		if _, ok := byBucket[d.Bucket]; !ok {
			// QueryUsageDetail shares QueryUsage's filters and bucket
			// expression, so every detail group's bucket key must
			// already exist in the aggregate result.
			return nil, fmt.Errorf("usage stats: detail bucket %q has no matching aggregate bucket", d.Bucket)
		}
		spend, ok := spendByBucket[d.Bucket]
		if !ok {
			spend = &ledgerSpend{}
			spendByBucket[d.Bucket] = spend
		}
		if err := spend.add(d); err != nil {
			return nil, fmt.Errorf("usage stats: %w", err)
		}
	}
	for bucket, spend := range spendByBucket {
		byBucket[bucket].CostUSD = spend.TotalUSD()
		byBucket[bucket].UnpricedRows = spend.UnpricedRows
	}

	return buckets, nil
}

// minTZOffsetMinutes / maxTZOffsetMinutes bound UsageQuery.TZOffsetMinutes
// to the real range of UTC offsets (UTC-14 through UTC+14, the widest
// spread any IANA zone uses).
const (
	minTZOffsetMinutes = -14 * 60
	maxTZOffsetMinutes = 14 * 60
)

// clampTZOffsetMinutes bounds an untrusted offset to [-840, 840].
func clampTZOffsetMinutes(minutes int) int {
	if minutes < minTZOffsetMinutes {
		return minTZOffsetMinutes
	}
	if minutes > maxTZOffsetMinutes {
		return maxTZOffsetMinutes
	}
	return minutes
}
