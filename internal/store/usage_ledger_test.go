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
	if b.SessionCount != 2 {
		t.Fatalf("session count = %d, want 2 (threads ta + tb; two turns on ta count once)", b.SessionCount)
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

// TestUsageLedger_WeekBucketingIsSundayStart pins the week-bucket
// contract: Sunday-start weeks keyed by the week's start date, correct
// across a year boundary. 2027-01-01 is a Friday (verified with
// `date -d 2027-01-01 +%A`), so its week began Sunday 2026-12-27 —
// that date is the bucket key. A Sunday row must key to its own date
// (Sunday IS the week start), the following Saturday joins the same
// bucket, and the next Sunday opens a new one.
func TestUsageLedger_WeekBucketingIsSundayStart(t *testing.T) {
	s := newTestStore(t)
	const (
		jan1_2027  = 1798761600000 // 2027-01-01T00:00:00Z (Friday)
		dec27_2026 = 1798329600000 // 2026-12-27T00:00:00Z (Sunday, week start)
		jan2_2027  = 1798848000000 // 2027-01-02T00:00:00Z (Saturday, week end)
		jan3_2027  = 1798934400000 // 2027-01-03T00:00:00Z (Sunday, NEXT week)
	)
	rows := []UsageLedgerRow{
		{CreatedAt: dec27_2026, ThreadID: "ta", ProjectID: "p1", TurnID: "turn-1",
			Provider: "claude", Model: "claude-haiku-4-5", InputTokens: 1, OutputTokens: 1},
		{CreatedAt: jan1_2027, ThreadID: "ta", ProjectID: "p1", TurnID: "turn-2",
			Provider: "claude", Model: "claude-haiku-4-5", InputTokens: 7, OutputTokens: 3},
		{CreatedAt: jan2_2027, ThreadID: "ta", ProjectID: "p1", TurnID: "turn-3",
			Provider: "claude", Model: "claude-haiku-4-5", InputTokens: 2, OutputTokens: 2},
		{CreatedAt: jan3_2027, ThreadID: "ta", ProjectID: "p1", TurnID: "turn-4",
			Provider: "claude", Model: "claude-haiku-4-5", InputTokens: 5, OutputTokens: 5},
	}
	if err := s.AppendUsage(rows); err != nil {
		t.Fatalf("append usage: %v", err)
	}

	buckets, err := s.QueryUsage(UsageQuery{GroupBy: "week"})
	if err != nil {
		t.Fatalf("week query: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("week buckets = %+v, want 2 (Sun 12-27..Sat 01-02, then Sun 01-03)", buckets)
	}
	if buckets[0].Bucket != "2026-12-27" || buckets[0].TurnCount != 3 {
		t.Fatalf("year-straddling week: %+v, want bucket 2026-12-27 holding Sun+Fri+Sat rows", buckets[0])
	}
	if buckets[1].Bucket != "2027-01-03" || buckets[1].TurnCount != 1 {
		t.Fatalf("next week: %+v, want bucket 2027-01-03 holding only the next Sunday's row", buckets[1])
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
	if byModel[0].SessionCount != 1 {
		t.Fatalf("haiku session count = %d, want 1 (both haiku turns are thread ta)", byModel[0].SessionCount)
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

func TestQueryWorkItemUsage(t *testing.T) {
	s := newTestStore(t)
	rows := []UsageLedgerRow{
		{CreatedAt: 1, ThreadID: "t1", WorkItemID: "item-1", Provider: "claude", Model: "m", InputTokens: 10, OutputTokens: 5, CacheReadInputTokens: 3, CacheCreationInputTokens: 2, ReasoningOutputTokens: 1, CostUSD: 0.25},
		{CreatedAt: 2, ThreadID: "t2", WorkItemID: "item-1", Provider: "codex", Model: "m", InputTokens: 20, OutputTokens: 7, CostUSD: 0.5},
		{CreatedAt: 3, ThreadID: "t3", WorkItemID: "item-2", Provider: "claude", Model: "m", InputTokens: 100, OutputTokens: 100, CostUSD: 10},
	}
	if err := s.AppendUsage(rows); err != nil {
		t.Fatalf("append: %v", err)
	}
	usage, err := s.QueryWorkItemUsage("item-1")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if usage.InputTokens != 30 || usage.OutputTokens != 12 ||
		usage.CacheReadInputTokens != 3 || usage.CacheCreationInputTokens != 2 ||
		usage.ReasoningOutputTokens != 1 || usage.TotalTokens != 47 || usage.CostUSD != 0.75 {
		t.Fatalf("usage = %#v", usage)
	}
	empty, err := s.QueryWorkItemUsage("missing")
	if err != nil {
		t.Fatalf("query missing: %v", err)
	}
	if empty.TotalTokens != 0 || empty.CostUSD != 0 {
		t.Fatalf("missing usage = %#v, want zero", empty)
	}
	if _, err := s.QueryWorkItemUsage(""); err == nil {
		t.Fatal("empty work item id must be rejected, '' marks unattributed rows")
	}
}

func TestQueryWorkItemCostsGroupsByProjectAndItem(t *testing.T) {
	s := newTestStore(t)
	if err := s.AppendUsage([]UsageLedgerRow{
		{CreatedAt: 1, ProjectID: "project-a", WorkItemID: "item-1", ThreadID: "t1", Provider: "claude", Model: "m", CostUSD: 0.25},
		{CreatedAt: 2, ProjectID: "project-a", WorkItemID: "item-1", ThreadID: "t2", Provider: "claude", Model: "m", CostUSD: 0.75},
		{CreatedAt: 3, ProjectID: "project-a", WorkItemID: "item-2", ThreadID: "t3", Provider: "claude", Model: "m", CostUSD: 2},
		{CreatedAt: 4, ProjectID: "project-b", WorkItemID: "item-1", ThreadID: "t4", Provider: "claude", Model: "m", CostUSD: 10},
		{CreatedAt: 5, ProjectID: "project-a", WorkItemID: "", ThreadID: "t5", Provider: "claude", Model: "m", CostUSD: 20},
	}); err != nil {
		t.Fatal(err)
	}
	costs, err := s.QueryWorkItemCosts("project-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(costs) != 2 || costs["item-1"] != 1 || costs["item-2"] != 2 {
		t.Fatalf("costs = %#v", costs)
	}
	empty, err := s.QueryWorkItemCosts("missing")
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("missing project costs = %#v", empty)
	}
	if _, err := s.QueryWorkItemCosts(""); err == nil {
		t.Fatal("empty project id must be rejected")
	}
}

func TestQueryWorkItemCostsUsesProjectWorkItemIndex(t *testing.T) {
	s := newTestStore(t)
	assertPlanUses(t, s.db, "idx_usage_ledger_project_work_item",
		"EXPLAIN QUERY PLAN "+queryWorkItemCostsSQL, "project-a")
}

func TestQueryWorkItemUsageDetail(t *testing.T) {
	s := newTestStore(t)
	if err := s.AppendUsage([]UsageLedgerRow{
		{CreatedAt: 1, ThreadID: "t1", WorkItemID: "item-1", TurnID: "turn-1", Provider: "codex", Model: "gpt-5.2-codex", InputTokens: 10, OutputTokens: 2, CostSource: "none"},
		{CreatedAt: 2, ThreadID: "t1", WorkItemID: "item-1", TurnID: "turn-2", Provider: "codex", Model: "gpt-5.2-codex", InputTokens: 20, CacheReadInputTokens: 4, CostSource: "none"},
		{CreatedAt: 3, ThreadID: "t2", WorkItemID: "item-1", TurnID: "turn-3", Provider: "claude", Model: "claude-opus-4-7", OutputTokens: 5, CostUSD: 0.25, CostSource: "wire"},
		{CreatedAt: 4, ThreadID: "t3", WorkItemID: "other", TurnID: "turn-4", Provider: "codex", Model: "gpt-5.2-codex", InputTokens: 999, CostSource: "none"},
	}); err != nil {
		t.Fatal(err)
	}
	details, err := s.QueryWorkItemUsageDetail("item-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(details) != 2 {
		t.Fatalf("details = %+v", details)
	}
	if details[0].Model != "claude-opus-4-7" || details[0].CostSource != "wire" || details[0].CostUSD != 0.25 || details[0].Rows != 1 {
		t.Fatalf("wire detail = %+v", details[0])
	}
	if details[1].Model != "gpt-5.2-codex" || details[1].CostSource != "none" || details[1].InputTokens != 30 || details[1].OutputTokens != 2 || details[1].CacheReadInputTokens != 4 || details[1].Rows != 2 {
		t.Fatalf("estimated detail = %+v", details[1])
	}
}

func TestQueryWorkItemTreeUsageAggregatesCalledRuns(t *testing.T) {
	s := newTestStore(t)
	// root -> child -> grandchild, plus a sibling tree that must not leak in.
	tree := []WorkItem{
		testWorkItem("root", "project-a", "running", 10),
		testCalledWorkItem("child", "root", "audit", 1, 1, 20),
		testCalledWorkItem("grandchild", "child", "audit", 1, 2, 30),
		testWorkItem("other-root", "project-a", "running", 40),
		testCalledWorkItem("other-child", "other-root", "audit", 1, 1, 50),
	}
	for _, item := range tree {
		if err := s.CreateWorkItem(item); err != nil {
			t.Fatalf("create %s: %v", item.ID, err)
		}
	}
	rows := []UsageLedgerRow{
		{CreatedAt: 1, ThreadID: "t1", WorkItemID: "root", Provider: "claude", Model: "m", InputTokens: 10, OutputTokens: 5, CostUSD: 0.25, CostSource: "wire"},
		{CreatedAt: 2, ThreadID: "t2", WorkItemID: "child", Provider: "claude", Model: "m", InputTokens: 20, OutputTokens: 7, CostUSD: 0.5, CostSource: "wire"},
		{CreatedAt: 3, ThreadID: "t3", WorkItemID: "grandchild", Provider: "codex", Model: "n", InputTokens: 100, CostSource: "none"},
		{CreatedAt: 4, ThreadID: "t4", WorkItemID: "other-child", Provider: "claude", Model: "m", InputTokens: 1000, CostUSD: 9, CostSource: "wire"},
	}
	if err := s.AppendUsage(rows); err != nil {
		t.Fatalf("append: %v", err)
	}

	usage, err := s.QueryWorkItemTreeUsage("root")
	if err != nil {
		t.Fatalf("query tree: %v", err)
	}
	if usage.InputTokens != 130 || usage.OutputTokens != 12 || usage.TotalTokens != 142 || usage.CostUSD != 0.75 {
		t.Fatalf("tree usage = %#v", usage)
	}
	// Enforcement happens against the root, but the same query answers for any
	// node: a subtree prices exactly its own runs.
	subtree, err := s.QueryWorkItemTreeUsage("child")
	if err != nil {
		t.Fatalf("query subtree: %v", err)
	}
	if subtree.TotalTokens != 127 || subtree.CostUSD != 0.5 {
		t.Fatalf("subtree usage = %#v", subtree)
	}
	leaf, err := s.QueryWorkItemTreeUsage("grandchild")
	if err != nil {
		t.Fatalf("query leaf: %v", err)
	}
	own, err := s.QueryWorkItemUsage("grandchild")
	if err != nil {
		t.Fatal(err)
	}
	if leaf != own {
		t.Fatalf("childless tree usage %#v differs from item usage %#v", leaf, own)
	}
	// Ledger rows outlive the runs they attribute (no foreign keys), and the walk
	// is anchored on the id rather than on a work_items lookup — so an id with no
	// run record still prices its own rows instead of silently reporting zero.
	if err := s.AppendUsage([]UsageLedgerRow{
		{CreatedAt: 5, ThreadID: "t5", WorkItemID: "ghost", Provider: "claude", Model: "m", InputTokens: 7, CostUSD: 0.1, CostSource: "wire"},
	}); err != nil {
		t.Fatal(err)
	}
	orphaned, err := s.QueryWorkItemTreeUsage("ghost")
	if err != nil {
		t.Fatalf("query orphaned: %v", err)
	}
	if orphaned.TotalTokens != 7 || orphaned.CostUSD != 0.1 {
		t.Fatalf("usage without a run record = %#v", orphaned)
	}

	detail, err := s.QueryWorkItemTreeUsageDetail("child")
	if err != nil {
		t.Fatalf("query tree detail: %v", err)
	}
	if len(detail) != 2 {
		t.Fatalf("tree detail groups = %#v", detail)
	}
	if detail[0].Model != "m" || detail[0].CostSource != "wire" || detail[0].InputTokens != 20 {
		t.Fatalf("wire group = %#v", detail[0])
	}
	if detail[1].Model != "n" || detail[1].CostSource != "none" || detail[1].InputTokens != 100 {
		t.Fatalf("unpriced group = %#v", detail[1])
	}
	if _, err := s.QueryWorkItemTreeUsage(""); err == nil {
		t.Fatal("empty root id must be rejected")
	}
	if _, err := s.QueryWorkItemTreeUsageDetail(""); err == nil {
		t.Fatal("empty root id must be rejected")
	}
}
