package store

import (
	"math"
	"testing"
	"time"
)

func TestMigrationV96DropsPricingVersionAndRetiresSettledPending(t *testing.T) {
	db := migrateThrough(t, 95)
	mustExec(t, db, `INSERT INTO usage_ledger(created_at,thread_id,turn_id,provider,model,input_tokens,output_tokens,cache_read_input_tokens,cache_creation_input_tokens,reasoning_output_tokens,cost_usd,cost_source,pricing_version) VALUES(1,'t','settled','codex','gpt-6-astra',100,0,0,0,0,0,'none','2026-09-09')`)
	insertPending := `INSERT INTO usage_pending(thread_id,scope,segment,model,created_at,turn_id,provider,input_tokens,output_tokens,cache_read_input_tokens,cache_creation_input_tokens,reasoning_output_tokens) VALUES(?,?,?,?,?,?,?,?,0,0,0,0)`
	mustExec(t, db, insertPending, "t", "old-process", "1", "gpt-6-astra", 2, "settled", "codex", 200)
	mustExec(t, db, insertPending, "t", "live-process", "0", "gpt-6-astra", 3, "running", "codex", 300)
	mustExec(t, db, insertPending, "other", "p", "0", "gpt-6-astra", 4, "settled", "codex", 400)
	if err := applyMigration(db, migrationByVersion(t, 96)); err != nil {
		t.Fatal(err)
	}
	var columns int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('usage_ledger') WHERE name = 'pricing_version'`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 0 {
		t.Fatal("pricing_version column survived")
	}
	var settled, pending int
	if err := db.QueryRow(`SELECT SUM(CASE WHEN pending = 0 THEN input_tokens ELSE 0 END), SUM(CASE WHEN pending = 1 THEN input_tokens ELSE 0 END) FROM usage_records`).Scan(&settled, &pending); err != nil {
		t.Fatal(err)
	}
	if settled != 100 || pending != 700 {
		t.Fatalf("after migration: settled=%d pending=%d, want the running turn and the other thread's pending rows kept", settled, pending)
	}
}

// Rates are dated by UTC day, so cost queries keep rows of different days
// in separate groups even for one model and cost source.
func TestUsageDetailGroupsByUTCDayInEveryCostQuery(t *testing.T) {
	s := newTestStore(t)
	day1 := time.Date(2026, 9, 9, 23, 30, 0, 0, time.UTC).UnixMilli()
	day2 := time.Date(2026, 9, 10, 0, 30, 0, 0, time.UTC).UnixMilli()
	for _, at := range []int64{day1, day1 + 60_000, day2} {
		row := progressRow(100)
		row.CreatedAt = at
		if err := s.AppendUsage([]UsageLedgerRow{row}); err != nil {
			t.Fatal(err)
		}
	}
	wantDay1 := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC).UnixMilli()
	wantDay2 := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC).UnixMilli()
	assert := func(rows []UsageDetailRow, err error) {
		t.Helper()
		if err != nil || len(rows) != 2 {
			t.Fatalf("day grouping: %+v %v", rows, err)
		}
		byDay := map[int64]UsageDetailRow{rows[0].Day: rows[0], rows[1].Day: rows[1]}
		if byDay[wantDay1].Rows != 2 || byDay[wantDay1].OutputTokens != 200 || byDay[wantDay2].Rows != 1 || byDay[wantDay2].OutputTokens != 100 {
			t.Fatalf("merged days: %+v", rows)
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
