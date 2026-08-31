package app

import (
	"math"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// almostEqualUSD checks whether two dollar amounts are within floating
// point rounding tolerance of each other.
func almostEqualUSD(a, b float64) bool {
	return math.Abs(a-b) < 0.001
}

// seedGetUsageStatsRows appends three ledger rows exercising every path
// GetUsageStats must merge: a wire-priced Claude row, a Codex row whose
// model the internal/usagecost rate table knows (cost_source='none' but
// priceable), and a Codex row whose model the rate table does NOT know
// (cost_source='none' and unpriced). The known-model estimate is
// computed by hand in the doc comment on each test so expectations
// don't depend on reading the rate table alongside the test.
//
// gpt-5.2-codex rate: $1.75/M input, $14/M output, $0.175/M cache read.
// 2M*$1.75 + 1M*$14 + 1M*$0.175 = 3.5 + 14 + 0.175 = 17.675.
func seedGetUsageStatsRows(t *testing.T, app *App) (day1, day2 int64) {
	t.Helper()
	day1 = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC).UnixMilli()
	day2 = time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC).UnixMilli()

	rows := []store.UsageLedgerRow{
		{
			CreatedAt: day1, ThreadID: "t1", ProjectID: "p1", TurnID: "turn-1",
			Provider: "claude", Model: "claude-sonnet-4-6",
			InputTokens: 100, OutputTokens: 50,
			CostUSD: 0.05, CostSource: "wire",
		},
		{
			CreatedAt: day1, ThreadID: "t2", ProjectID: "p2", TurnID: "turn-2",
			Provider: "codex", Model: "gpt-5.2-codex",
			InputTokens: 2_000_000, OutputTokens: 1_000_000, CacheReadInputTokens: 1_000_000,
			CostSource: "none",
		},
		{
			CreatedAt: day2, ThreadID: "t2", ProjectID: "p2", TurnID: "turn-3",
			Provider: "codex", Model: "totally-unknown-model",
			InputTokens: 100, OutputTokens: 50,
			CostSource: "none",
		},
	}
	if err := app.store.AppendUsage(rows); err != nil {
		t.Fatalf("seed usage rows: %v", err)
	}
	return day1, day2
}

// TestGetUsageStats_LifetimeMergesWireAndEstimatedCost proves the core
// merge: the lifetime bucket's CostUSD sums the wire-reported Claude
// cost AND the read-time gpt-5.2-codex estimate, while the row whose
// model has no rate-table entry contributes 0 dollars and is counted
// in UnpricedRows instead.
func TestGetUsageStats_LifetimeMergesWireAndEstimatedCost(t *testing.T) {
	app := newTestAppWithStore(t)
	seedGetUsageStatsRows(t, app)

	buckets, err := app.GetUsageStats(store.UsageQuery{})
	if err != nil {
		t.Fatalf("GetUsageStats: %v", err)
	}
	if len(buckets) != 1 {
		t.Fatalf("lifetime buckets = %d, want 1: %+v", len(buckets), buckets)
	}
	b := buckets[0]

	wantCost := 0.05 + 17.675
	if !almostEqualUSD(b.CostUSD, wantCost) {
		t.Errorf("lifetime CostUSD = %f, want %f (wire 0.05 + estimate 17.675, unknown-model row excluded)", b.CostUSD, wantCost)
	}
	if b.UnpricedRows != 1 {
		t.Errorf("lifetime UnpricedRows = %d, want 1 (the unknown-model row)", b.UnpricedRows)
	}
	if b.TurnCount != 3 {
		t.Errorf("lifetime TurnCount = %d, want 3", b.TurnCount)
	}
}

// TestGetUsageStats_GroupByModelPricesEachRowIndependently confirms
// per-model buckets each carry only their own row's price: the wire row
// keeps its wire cost, the priceable Codex row gets its table estimate,
// and the unknown-model row gets $0 plus an UnpricedRows count.
func TestGetUsageStats_GroupByModelPricesEachRowIndependently(t *testing.T) {
	app := newTestAppWithStore(t)
	seedGetUsageStatsRows(t, app)

	buckets, err := app.GetUsageStats(store.UsageQuery{GroupBy: "model"})
	if err != nil {
		t.Fatalf("GetUsageStats: %v", err)
	}
	if len(buckets) != 3 {
		t.Fatalf("model buckets = %d, want 3: %+v", len(buckets), buckets)
	}

	byBucket := make(map[string]store.UsageBucket, len(buckets))
	for _, b := range buckets {
		byBucket[b.Bucket] = b
	}

	sonnet := byBucket["claude-sonnet-4-6"]
	if !almostEqualUSD(sonnet.CostUSD, 0.05) || sonnet.UnpricedRows != 0 {
		t.Errorf("claude-sonnet-4-6 bucket: %+v, want CostUSD=0.05 UnpricedRows=0", sonnet)
	}

	codex := byBucket["gpt-5.2-codex"]
	if !almostEqualUSD(codex.CostUSD, 17.675) || codex.UnpricedRows != 0 {
		t.Errorf("gpt-5.2-codex bucket: %+v, want CostUSD=17.675 UnpricedRows=0", codex)
	}

	unknown := byBucket["totally-unknown-model"]
	if unknown.CostUSD != 0 || unknown.UnpricedRows != 1 {
		t.Errorf("totally-unknown-model bucket: %+v, want CostUSD=0 UnpricedRows=1", unknown)
	}
}

// TestGetUsageStats_GroupByProviderMergesModelsWithinBucket confirms
// the provider bucket for codex merges its priced and unpriced model
// rows into one CostUSD/UnpricedRows pair, while claude (all wire) has
// no unpriced rows at all.
func TestGetUsageStats_GroupByProviderMergesModelsWithinBucket(t *testing.T) {
	app := newTestAppWithStore(t)
	seedGetUsageStatsRows(t, app)

	buckets, err := app.GetUsageStats(store.UsageQuery{GroupBy: "provider"})
	if err != nil {
		t.Fatalf("GetUsageStats: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("provider buckets = %d, want 2: %+v", len(buckets), buckets)
	}

	byBucket := make(map[string]store.UsageBucket, len(buckets))
	for _, b := range buckets {
		byBucket[b.Bucket] = b
	}

	claude := byBucket["claude"]
	if !almostEqualUSD(claude.CostUSD, 0.05) || claude.UnpricedRows != 0 {
		t.Errorf("claude bucket: %+v, want CostUSD=0.05 UnpricedRows=0", claude)
	}

	codex := byBucket["codex"]
	if !almostEqualUSD(codex.CostUSD, 17.675) || codex.UnpricedRows != 1 {
		t.Errorf("codex bucket: %+v, want CostUSD=17.675 (priced row only) UnpricedRows=1 (unknown-model row)", codex)
	}
}

// TestGetUsageStats_GroupByDaySplitsPricingAcrossCalendarBuckets proves
// pricing is applied per calendar bucket, not just per lifetime total:
// day1 holds the wire row plus the priceable Codex row (both priced),
// day2 holds only the unpriced unknown-model row.
func TestGetUsageStats_GroupByDaySplitsPricingAcrossCalendarBuckets(t *testing.T) {
	app := newTestAppWithStore(t)
	seedGetUsageStatsRows(t, app)

	buckets, err := app.GetUsageStats(store.UsageQuery{GroupBy: "day"})
	if err != nil {
		t.Fatalf("GetUsageStats: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("day buckets = %d, want 2: %+v", len(buckets), buckets)
	}

	if buckets[0].Bucket != "2026-07-01" {
		t.Fatalf("buckets[0] = %+v, want 2026-07-01 first (chronological order)", buckets[0])
	}
	wantDay1Cost := 0.05 + 17.675
	if !almostEqualUSD(buckets[0].CostUSD, wantDay1Cost) || buckets[0].UnpricedRows != 0 {
		t.Errorf("2026-07-01 bucket: %+v, want CostUSD=%f UnpricedRows=0", buckets[0], wantDay1Cost)
	}

	if buckets[1].Bucket != "2026-07-02" {
		t.Fatalf("buckets[1] = %+v, want 2026-07-02 second", buckets[1])
	}
	if buckets[1].CostUSD != 0 || buckets[1].UnpricedRows != 1 {
		t.Errorf("2026-07-02 bucket: %+v, want CostUSD=0 UnpricedRows=1 (only the unknown-model row)", buckets[1])
	}
}

// TestGetUsageStats_ClampsAbsurdTZOffset proves an out-of-range
// TZOffsetMinutes (wire clients can send any int) is clamped to the real
// UTC-14..UTC+14 range rather than producing nonsense calendar buckets.
// An offset of 100000 minutes clamps to +840 (UTC+14); day1's row
// (2026-07-01T12:00:00Z) shifted by +14h lands on 2026-07-02, matching
// what an explicit TZOffsetMinutes: 840 query produces.
func TestGetUsageStats_ClampsAbsurdTZOffset(t *testing.T) {
	app := newTestAppWithStore(t)
	seedGetUsageStatsRows(t, app)

	absurd, err := app.GetUsageStats(store.UsageQuery{GroupBy: "day", TZOffsetMinutes: 100_000})
	if err != nil {
		t.Fatalf("GetUsageStats absurd offset: %v", err)
	}
	clamped, err := app.GetUsageStats(store.UsageQuery{GroupBy: "day", TZOffsetMinutes: 840})
	if err != nil {
		t.Fatalf("GetUsageStats clamped offset: %v", err)
	}
	if len(absurd) != len(clamped) {
		t.Fatalf("absurd offset buckets = %d, want %d (same as explicit +840): %+v", len(absurd), len(clamped), absurd)
	}
	for i := range absurd {
		if absurd[i].Bucket != clamped[i].Bucket {
			t.Fatalf("absurd offset bucket[%d] = %q, want %q (clamped +840 behavior)", i, absurd[i].Bucket, clamped[i].Bucket)
		}
	}

	negAbsurd, err := app.GetUsageStats(store.UsageQuery{GroupBy: "day", TZOffsetMinutes: -100_000})
	if err != nil {
		t.Fatalf("GetUsageStats absurd negative offset: %v", err)
	}
	negClamped, err := app.GetUsageStats(store.UsageQuery{GroupBy: "day", TZOffsetMinutes: -840})
	if err != nil {
		t.Fatalf("GetUsageStats clamped negative offset: %v", err)
	}
	if len(negAbsurd) != len(negClamped) {
		t.Fatalf("absurd negative offset buckets = %d, want %d (same as explicit -840): %+v", len(negAbsurd), len(negClamped), negAbsurd)
	}
	for i := range negAbsurd {
		if negAbsurd[i].Bucket != negClamped[i].Bucket {
			t.Fatalf("absurd negative offset bucket[%d] = %q, want %q (clamped -840 behavior)", i, negAbsurd[i].Bucket, negClamped[i].Bucket)
		}
	}
}

// TestGetUsageStats_NilStoreReturnsError preserves the store-unavailable
// guard across the rewrite — a nil store must fail loudly, not panic on
// the QueryUsageDetail call added by the pricing merge.
func TestGetUsageStats_NilStoreReturnsError(t *testing.T) {
	app := &App{}
	if _, err := app.GetUsageStats(store.UsageQuery{}); err == nil {
		t.Fatal("GetUsageStats with nil store: want error, got nil")
	}
}
