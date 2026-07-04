package store

import (
	"testing"
)

func seedUsageRows(t *testing.T, s *Store) {
	t.Helper()
	// Two turns on thread A (claude, one multi-model), one on thread B
	// (codex, unpriced). Timestamps chosen so day bucketing splits at a
	// UTC boundary: 2026-07-01T23:30Z and 2026-07-02T01:00Z.
	const (
		jul1_2330 = 1782948600000 // 2026-07-01T23:30:00Z
		jul2_0100 = 1782954000000 // 2026-07-02T01:00:00Z
	)
	rows := []UsageLedgerRow{
		{
			CreatedAt: jul1_2330, ThreadID: "ta", ProjectID: "p1", TurnID: "turn-1",
			Provider: "claude", Model: "claude-haiku-4-5",
			InputTokens: 10, OutputTokens: 42, CacheReadInputTokens: 100,
			CacheCreationInputTokens: 50, CostUSD: 0.02,
		},
		{
			CreatedAt: jul2_0100, ThreadID: "ta", ProjectID: "p1", TurnID: "turn-2",
			Provider: "claude", Model: "claude-haiku-4-5",
			InputTokens: 5, OutputTokens: 20, CostUSD: 0.01,
		},
		{
			CreatedAt: jul2_0100, ThreadID: "ta", ProjectID: "p1", TurnID: "turn-2",
			Provider: "claude", Model: "claude-sonnet-4-6",
			InputTokens: 3, OutputTokens: 500, CacheCreationInputTokens: 9000,
			CostUSD: 0.05,
		},
		{
			CreatedAt: jul2_0100, ThreadID: "tb", ProjectID: "p2", TurnID: "turn-3",
			Provider: "codex", Model: "gpt-5.2-codex",
			InputTokens: 600, OutputTokens: 1000, CacheReadInputTokens: 8000,
			ReasoningOutputTokens: 400,
		},
	}
	if err := s.AppendUsage(rows); err != nil {
		t.Fatalf("append usage: %v", err)
	}
}

func TestUsageLedger_LifetimeAggregate(t *testing.T) {
	s := newTestStore(t)
	seedUsageRows(t, s)

	buckets, err := s.QueryUsage(UsageQuery{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(buckets) != 1 {
		t.Fatalf("lifetime buckets = %d, want 1", len(buckets))
	}
	b := buckets[0]
	if b.InputTokens != 618 || b.OutputTokens != 1562 {
		t.Fatalf("lifetime tokens: %+v", b)
	}
	if b.TurnCount != 3 {
		t.Fatalf("turn count = %d, want 3 (multi-model turn counts once)", b.TurnCount)
	}
	// QueryUsage no longer selects cost_usd at all — pricing (both the
	// wire-reported sum and the rate-table estimate for cost_source='none'
	// rows) is GetUsageStats' job (see app_usage_test.go). The raw
	// aggregate always reports the zero value for both fields.
	if b.UnpricedRows != 0 {
		t.Fatalf("raw QueryUsage must not flag unpriced rows itself, got %d", b.UnpricedRows)
	}
	if b.CostUSD != 0 {
		t.Fatalf("raw QueryUsage must not report cost, got %g (cost is GetUsageStats' job)", b.CostUSD)
	}
}

func TestUsageLedger_DayBucketingRespectsTimezone(t *testing.T) {
	s := newTestStore(t)
	seedUsageRows(t, s)

	// UTC: rows split across Jul 1 / Jul 2.
	utc, err := s.QueryUsage(UsageQuery{GroupBy: "day"})
	if err != nil {
		t.Fatalf("utc query: %v", err)
	}
	if len(utc) != 2 || utc[0].Bucket != "2026-07-01" || utc[1].Bucket != "2026-07-02" {
		t.Fatalf("utc day buckets: %+v", utc)
	}

	// UTC+2: 23:30Z on Jul 1 is already Jul 2 locally — one bucket.
	plus2, err := s.QueryUsage(UsageQuery{GroupBy: "day", TZOffsetMinutes: 120})
	if err != nil {
		t.Fatalf("tz query: %v", err)
	}
	if len(plus2) != 1 || plus2[0].Bucket != "2026-07-02" {
		t.Fatalf("utc+2 day buckets: %+v", plus2)
	}
}

// TestUsageLedger_WeekBucketingIsISOAcrossYearBoundary is the regression
// guard for ISO week bucketing: 2027-01-01T00:00:00Z falls in ISO week
// 2026-W53 (verified against `date -d 2027-01-01 +%G-W%V` => 2026-W53),
// not calendar-year week "2027-W00" — which is what the old non-ISO
// `%Y-W%W` format produced (wrong year AND non-ISO week numbering for
// the first partial week of January).
func TestUsageLedger_WeekBucketingIsISOAcrossYearBoundary(t *testing.T) {
	s := newTestStore(t)
	const jan1_2027 = 1798761600000 // 2027-01-01T00:00:00Z
	rows := []UsageLedgerRow{
		{
			CreatedAt: jan1_2027, ThreadID: "ta", ProjectID: "p1", TurnID: "turn-1",
			Provider: "claude", Model: "claude-haiku-4-5",
			InputTokens: 7, OutputTokens: 3,
		},
	}
	if err := s.AppendUsage(rows); err != nil {
		t.Fatalf("append usage: %v", err)
	}

	buckets, err := s.QueryUsage(UsageQuery{GroupBy: "week"})
	if err != nil {
		t.Fatalf("week query: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Bucket != "2026-W53" {
		t.Fatalf("week bucket = %+v, want a single 2026-W53 bucket (ISO week, not 2027-W00)", buckets)
	}
}

func TestUsageLedger_GroupByModelAndFilters(t *testing.T) {
	s := newTestStore(t)
	seedUsageRows(t, s)

	byModel, err := s.QueryUsage(UsageQuery{GroupBy: "model", Provider: "claude"})
	if err != nil {
		t.Fatalf("model query: %v", err)
	}
	if len(byModel) != 2 {
		t.Fatalf("claude models = %d, want 2: %+v", len(byModel), byModel)
	}
	if byModel[0].Bucket != "claude-haiku-4-5" || byModel[0].OutputTokens != 62 {
		t.Fatalf("haiku bucket: %+v", byModel[0])
	}
	if byModel[1].Bucket != "claude-sonnet-4-6" || byModel[1].CacheCreationInputTokens != 9000 {
		t.Fatalf("sonnet bucket: %+v", byModel[1])
	}

	byThread, err := s.QueryUsage(UsageQuery{GroupBy: "thread", ThreadID: "tb"})
	if err != nil {
		t.Fatalf("thread query: %v", err)
	}
	if len(byThread) != 1 || byThread[0].ReasoningOutputTokens != 400 {
		t.Fatalf("thread filter: %+v", byThread)
	}
}

// TestUsageLedgerDetail_LifetimeGroupsByModelAndCostSource — the two
// wire-priced haiku rows (turn-1, turn-2) fold into one (model,
// cost_source) group even though they came from separate turns; the
// sonnet and codex rows each stay their own group.
func TestUsageLedgerDetail_LifetimeGroupsByModelAndCostSource(t *testing.T) {
	s := newTestStore(t)
	seedUsageRows(t, s)

	details, err := s.QueryUsageDetail(UsageQuery{})
	if err != nil {
		t.Fatalf("detail query: %v", err)
	}
	if len(details) != 3 {
		t.Fatalf("detail groups = %d, want 3: %+v", len(details), details)
	}

	haiku := details[0]
	if haiku.Model != "claude-haiku-4-5" || haiku.CostSource != "wire" {
		t.Fatalf("haiku group: %+v", haiku)
	}
	if haiku.Rows != 2 || haiku.InputTokens != 15 || haiku.OutputTokens != 62 {
		t.Fatalf("haiku group did not fold both turns: %+v", haiku)
	}
	if haiku.CostUSD < 0.029 || haiku.CostUSD > 0.031 {
		t.Fatalf("haiku group cost = %g, want ~0.03 (0.02+0.01)", haiku.CostUSD)
	}

	sonnet := details[1]
	if sonnet.Model != "claude-sonnet-4-6" || sonnet.CostSource != "wire" || sonnet.Rows != 1 {
		t.Fatalf("sonnet group: %+v", sonnet)
	}
	if sonnet.CacheCreationInputTokens != 9000 {
		t.Fatalf("sonnet group cache creation: %+v", sonnet)
	}

	codex := details[2]
	if codex.Model != "gpt-5.2-codex" || codex.CostSource != "none" || codex.Rows != 1 {
		t.Fatalf("codex group: %+v", codex)
	}
	if codex.CostUSD != 0 {
		t.Fatalf("codex group cost = %g, want 0 (no wire cost)", codex.CostUSD)
	}
	if codex.ReasoningOutputTokens != 400 {
		t.Fatalf("codex group reasoning tokens: %+v", codex)
	}
}

// TestUsageLedgerDetail_DayBucketingMatchesQueryUsage — detail groups
// must land in the same day buckets QueryUsage produces for the same
// query, since GetUsageStats merges the two result sets by Bucket key.
func TestUsageLedgerDetail_DayBucketingMatchesQueryUsage(t *testing.T) {
	s := newTestStore(t)
	seedUsageRows(t, s)

	buckets, err := s.QueryUsage(UsageQuery{GroupBy: "day"})
	if err != nil {
		t.Fatalf("bucket query: %v", err)
	}
	details, err := s.QueryUsageDetail(UsageQuery{GroupBy: "day"})
	if err != nil {
		t.Fatalf("detail query: %v", err)
	}
	if len(details) != 4 {
		t.Fatalf("detail groups = %d, want 4: %+v", len(details), details)
	}

	bucketKeys := make(map[string]bool, len(buckets))
	for _, b := range buckets {
		bucketKeys[b.Bucket] = true
	}
	for _, d := range details {
		if !bucketKeys[d.Bucket] {
			t.Fatalf("detail bucket %q has no matching QueryUsage bucket: buckets=%+v", d.Bucket, buckets)
		}
	}

	if details[0].Bucket != "2026-07-01" || details[0].Model != "claude-haiku-4-5" || details[0].Rows != 1 {
		t.Fatalf("jul1 haiku group: %+v", details[0])
	}
	if details[1].Bucket != "2026-07-02" || details[1].Model != "claude-haiku-4-5" || details[1].Rows != 1 {
		t.Fatalf("jul2 haiku group: %+v", details[1])
	}
	if details[2].Bucket != "2026-07-02" || details[2].Model != "claude-sonnet-4-6" {
		t.Fatalf("jul2 sonnet group: %+v", details[2])
	}
	if details[3].Bucket != "2026-07-02" || details[3].Model != "gpt-5.2-codex" || details[3].CostSource != "none" {
		t.Fatalf("jul2 codex group: %+v", details[3])
	}
}

// TestUsageLedgerDetail_FiltersLikeQueryUsage — Provider/ThreadID
// filters apply identically to QueryUsageDetail as they do to
// QueryUsage (usageWhereFilters is shared).
func TestUsageLedgerDetail_FiltersLikeQueryUsage(t *testing.T) {
	s := newTestStore(t)
	seedUsageRows(t, s)

	details, err := s.QueryUsageDetail(UsageQuery{Provider: "codex"})
	if err != nil {
		t.Fatalf("provider filter query: %v", err)
	}
	if len(details) != 1 || details[0].Model != "gpt-5.2-codex" || details[0].CostSource != "none" {
		t.Fatalf("codex provider filter: %+v", details)
	}

	details, err = s.QueryUsageDetail(UsageQuery{ThreadID: "ta"})
	if err != nil {
		t.Fatalf("thread filter query: %v", err)
	}
	if len(details) != 2 {
		t.Fatalf("thread ta detail groups = %d, want 2 (haiku+sonnet, no codex): %+v", len(details), details)
	}
}

func TestUsageLedger_RejectsUnknownGroupBy(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.QueryUsage(UsageQuery{GroupBy: "hour"}); err == nil {
		t.Fatal("expected error for unsupported groupBy")
	}
}

// TestUsageLedger_SurvivesThreadDeletion is the lifetime-accounting
// guarantee: deleting a thread cascades turns/items away but must leave
// ledger rows intact.
func TestUsageLedger_SurvivesThreadDeletion(t *testing.T) {
	s := newTestStore(t)
	thr := makeThread("ta", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	seedUsageRows(t, s)

	if err := s.DeleteThread("ta"); err != nil {
		t.Fatalf("delete thread: %v", err)
	}
	buckets, err := s.QueryUsage(UsageQuery{ThreadID: "ta"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(buckets) != 1 || buckets[0].TurnCount != 2 {
		t.Fatalf("ledger rows lost on thread deletion: %+v", buckets)
	}
}
