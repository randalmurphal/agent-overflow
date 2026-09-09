package store

import (
	"agent-overflow/internal/usagecost"
	"math"
	"testing"
)

func TestMigrationV93PreservesUsageAndPinsPricing(t *testing.T) {
	db := migrateThrough(t, 92)
	mustExec(t, db, `INSERT INTO usage_ledger(created_at,thread_id,turn_id,provider,model,input_tokens,output_tokens,cache_read_input_tokens,cache_creation_input_tokens,reasoning_output_tokens,cost_usd,cost_source) VALUES(1,'t','turn','codex','gpt-6-astra',100,0,0,0,0,0,'none')`)
	mustExec(t, db, `INSERT INTO usage_pending(thread_id,scope,segment,model,created_at,turn_id,provider,input_tokens,output_tokens,cache_read_input_tokens,cache_creation_input_tokens,reasoning_output_tokens) VALUES('t','p','0','gpt-6-astra',2,'turn2','codex',200,0,0,0,0)`)
	if err := applyMigration(db, migrationByVersion(t, 93)); err != nil {
		t.Fatal(err)
	}
	var tokens int
	var version string
	if err := db.QueryRow(`SELECT input_tokens,pricing_version FROM usage_records WHERE pending=0`).Scan(&tokens, &version); err != nil {
		t.Fatal(err)
	}
	if tokens != 100 || version != "2026-09-09" {
		t.Fatalf("settled: %d %q", tokens, version)
	}
	if err := db.QueryRow(`SELECT input_tokens,pricing_version FROM usage_records WHERE pending=1`).Scan(&tokens, &version); err != nil {
		t.Fatal(err)
	}
	if tokens != 200 || version != "" {
		t.Fatalf("pending: %d %q", tokens, version)
	}
}

func TestPricingVersionsRemainSeparateInEveryCostQuery(t *testing.T) {
	s := newTestStore(t)
	for _, version := range []string{usagecost.CurrentVersion, "future-snapshot"} {
		row := progressRow(100)
		row.PricingVersion = version
		if err := s.AppendUsage([]UsageLedgerRow{row}); err != nil {
			t.Fatal(err)
		}
	}
	assert := func(rows []UsageDetailRow, err error) {
		t.Helper()
		if err != nil || len(rows) != 2 {
			t.Fatalf("version grouping: %+v %v", rows, err)
		}
		if rows[0].PricingVersion == rows[1].PricingVersion || rows[0].OutputTokens != 100 || rows[1].OutputTokens != 100 {
			t.Fatalf("merged pricing versions: %+v", rows)
		}
	}
	assert(s.QueryUsageDetail(UsageQuery{}))
	assert(s.QueryWorkItemUsageDetail("run"))
	assert(s.QueryWorkItemTreeUsageDetail("run"))
	groups, err := s.QueryWorkItemCosts("project")
	if err != nil {
		t.Fatal(err)
	}
	rows := make([]UsageDetailRow, 0, len(groups))
	for _, group := range groups {
		rows = append(rows, group.UsageDetailRow)
	}
	assert(rows, nil)
}

func TestInvalidUsageBatchIsAtomicAndKeepsPending(t *testing.T) {
	s := newTestStore(t)
	row := progressRow(10)
	if _, err := s.PutUsageProgress("p", "0", []UsageLedgerRow{row}); err != nil {
		t.Fatal(err)
	}
	for _, value := range []float64{-1, math.NaN(), math.Inf(1)} {
		invalid := row
		invalid.CostUSD = value
		if err := s.AppendUsageAndReconcile("p", []UsageLedgerRow{row, invalid}); err == nil {
			t.Fatal("invalid cost accepted")
		}
		buckets, _, err := s.ReadUsageStats(UsageQuery{})
		if err != nil || len(buckets) != 1 || buckets[0].PendingRows != 1 || buckets[0].OutputTokens != 10 || buckets[0].TurnCount != 0 {
			t.Fatalf("partial invalid batch: %+v %v", buckets, err)
		}
	}
}
