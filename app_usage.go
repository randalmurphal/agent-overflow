package main

import (
	"fmt"
	"log"

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

	buckets = a.overlayProviderThreadCost(query, buckets)

	return buckets, nil
}

// overlayProviderThreadCost replaces a single thread's lifetime cost with the
// PROVIDER's own figure when one has been recorded (Codex >= 0.148; see
// app_codex_thread_cost.go), and labels the bucket so a surface can say whose
// number it is showing.
//
// It applies to exactly one query shape: one thread, no time bounds, no
// grouping, no model filter — the composer usage chip's lifetime query, and
// the only question a cumulative per-thread total actually answers. Every
// other shape keeps the rate-table arithmetic, because the provider figure
// cannot be decomposed into it:
//
//   - a day/week/month bucket would need the estimate split across time, and
//     the provider states one number for the thread's whole life;
//   - a model or provider bucket would need it split per model — the
//     `threadUsage.groups` breakdown exists but reports credits, not dollars,
//     and every token field on it is optional;
//   - a project or multi-thread aggregate would need it summed with rate-table
//     figures for the threads that have no estimate, which mixes two pricing
//     bases inside one total without any way to say so.
//
// ZERO buckets is a real case, not an early exit: a RESUMED provider thread
// has spend the backend knows about and no AO ledger rows at all until it
// settles a turn here (QueryUsage returns nothing for a thread with no rows).
// Reporting "no usage" beside a provider figure already on disk would hide the
// better answer, so the bucket is synthesized — with zero tokens and zero
// turns, which is the truth about the LEDGER, and the provider's dollars.
//
// Failure to read is not failure to answer: the ledger total is already in the
// bucket, so an error logs and leaves it.
func (a *App) overlayProviderThreadCost(query store.UsageQuery, buckets []store.UsageBucket) []store.UsageBucket {
	if query.ThreadID == "" || query.GroupBy != "" || query.Model != "" {
		return buckets
	}
	if query.FromMillis != 0 || query.ToMillis != 0 {
		return buckets
	}
	if len(buckets) > 1 {
		return buckets
	}
	// A row that describes a provider thread this AO thread no longer points
	// at answers `found=false` here: GetProviderThreadCost compares the stored
	// provider-thread identity against the thread's current SessionRef
	// (migration v68). So a rollback whose delete never landed costs the user
	// the rate-table figure that is already in the bucket, never another Codex
	// thread's total — with no process-lifetime marker in the way, which is
	// what makes it survive a restart. See forgetCodexThreadCost.
	cost, found, err := a.store.GetProviderThreadCost(query.ThreadID)
	if err != nil {
		log.Printf("usage stats: provider thread cost for %s: %v", query.ThreadID, err)
		return buckets
	}
	if !found {
		return buckets
	}
	if query.Provider != "" && query.Provider != cost.Provider {
		return buckets
	}
	if len(buckets) == 0 {
		buckets = append(buckets, store.UsageBucket{})
	}
	buckets[0].CostUSD = cost.CostUSD()
	// The provider priced the whole thread, so nothing in this bucket is
	// unpriced any more — a `≥` lower-bound marker over a complete figure
	// would be a worse answer than the one it replaced.
	buckets[0].UnpricedRows = 0
	buckets[0].CostSource = cost.CostSource
	return buckets
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
