package store

import (
	"fmt"
	"path/filepath"
	"testing"
)

func progressRow(output int) UsageLedgerRow {
	return UsageLedgerRow{ThreadID: "thread", TurnID: "turn", Provider: "claude", Model: "claude-haiku-4-5", CreatedAt: 100,
		ProjectID: "project", WorkItemID: "run", InputTokens: 10, OutputTokens: output, CacheReadInputTokens: 20}
}

func TestUsagePendingInterruptionAndReconciliation(t *testing.T) {
	s := newTestStore(t)
	put := func(segment string, output int, wantChanged bool) {
		t.Helper()
		changed, err := s.PutUsageProgress("process", segment, []UsageLedgerRow{progressRow(output)})
		if err != nil || changed != wantChanged {
			t.Fatalf("put: changed=%v err=%v", changed, err)
		}
	}
	assert := func(output, pending, turns int64) {
		t.Helper()
		buckets, _, err := s.ReadUsageStats(UsageQuery{ThreadID: "thread"})
		if err != nil || len(buckets) != 1 {
			t.Fatalf("read: %+v %v", buckets, err)
		}
		b := buckets[0]
		if b.OutputTokens != output || b.PendingRows != pending || b.TurnCount != turns {
			t.Fatalf("totals: %+v", b)
		}
	}
	put("0", 5, true)
	put("0", 5, false)
	put("0", 3, false)
	put("0", 8, true)
	assert(8, 1, 0)
	if err := s.AppendUsageAndReconcile("process", nil); err != nil {
		t.Fatal(err)
	}
	assert(8, 1, 0) // Empty interrupt result leaves known tokens intact.
	put("1", 12, true)
	assert(20, 2, 0)
	final := progressRow(25)
	final.InputTokens = 20
	final.CacheReadInputTokens = 40
	final.CostUSD = 0.1
	if err := s.AppendUsageAndReconcile("process", []UsageLedgerRow{final}); err != nil {
		t.Fatal(err)
	}
	assert(25, 0, 1)
	_, detail, err := s.ReadUsageStats(UsageQuery{ThreadID: "thread"})
	if err != nil || len(detail) != 1 || detail[0].CostSource != "wire" || detail[0].CostUSD != 0.1 {
		t.Fatalf("price: %+v %v", detail, err)
	}
}

func TestUsagePendingSurvivesReopenUntilItsTurnSettles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutUsageProgress("old-process", "0", []UsageLedgerRow{progressRow(7)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	b, _, err := s.ReadUsageStats(UsageQuery{ThreadID: "thread"})
	if err != nil || len(b) != 1 || b[0].OutputTokens != 7 || b[0].PendingRows != 1 {
		t.Fatalf("reopen: %+v %v", b, err)
	}
	if err := s.AppendUsageAndReconcile("new-process", []UsageLedgerRow{progressRow(5)}); err != nil {
		t.Fatal(err)
	}
	b, _, err = s.ReadUsageStats(UsageQuery{ThreadID: "thread"})
	if err != nil || len(b) != 1 || b[0].OutputTokens != 5 || b[0].PendingRows != 0 {
		t.Fatalf("settled after reopen: %+v %v", b, err)
	}
}

// A turn's final accounting replaces its reported tokens even when it is
// smaller; an invalid settlement rolls back and leaves the report intact.
func TestUsagePendingFinalAccountingWinsAndInvalidSettlementRollsBack(t *testing.T) {
	s := newTestStore(t)
	row := progressRow(10)
	if _, err := s.PutUsageProgress("p", "0", []UsageLedgerRow{row}); err != nil {
		t.Fatal(err)
	}
	smaller := row
	smaller.OutputTokens = 4
	invalid := row
	invalid.OutputTokens = -1
	if err := s.AppendUsageAndReconcile("p", []UsageLedgerRow{smaller, invalid}); err == nil {
		t.Fatal("accepted negative settlement")
	}
	b, _, err := s.ReadUsageStats(UsageQuery{ThreadID: "thread"})
	if err != nil || len(b) != 1 || b[0].OutputTokens != 10 || b[0].PendingRows != 1 {
		t.Fatalf("rolled back settlement changed the report: %+v %v", b, err)
	}
	if err := s.AppendUsageAndReconcile("p", []UsageLedgerRow{smaller}); err != nil {
		t.Fatal(err)
	}
	b, details, err := s.ReadUsageStats(UsageQuery{ThreadID: "thread"})
	if err != nil || len(b) != 1 || b[0].OutputTokens != 4 || b[0].PendingRows != 0 || len(details) != 1 {
		t.Fatalf("final accounting: %+v %+v %v", b, details, err)
	}
	bad := row
	bad.CostUSD = 1
	if _, err := s.PutUsageProgress("p", "1", []UsageLedgerRow{bad}); err == nil {
		t.Fatal("accepted fabricated interim cost")
	}
}

func TestUsagePendingReconciliationPagesAndQueryIndexes(t *testing.T) {
	s := newTestStore(t)
	assertPlanUses(t, s.db, "idx_usage_pending_project_work_item", "EXPLAIN QUERY PLAN "+queryWorkItemCostsSQL, "project")
	for i := range 140 {
		if _, err := s.PutUsageProgress("p", fmt.Sprint(i), []UsageLedgerRow{progressRow(1)}); err != nil {
			t.Fatal(err)
		}
	}
	row := progressRow(140)
	row.InputTokens *= 140
	row.CacheReadInputTokens *= 140
	if err := s.AppendUsageAndReconcile("p", []UsageLedgerRow{row}); err != nil {
		t.Fatal(err)
	}
	b, _, err := s.ReadUsageStats(UsageQuery{ProjectID: "project"})
	if err != nil || len(b) != 1 || b[0].OutputTokens != 140 || b[0].PendingRows != 0 {
		t.Fatalf("pages: %+v %v", b, err)
	}
}

func TestMigrationV92PreservesSettledUsage(t *testing.T) {
	db := migrateThrough(t, 91)
	mustExec(t, db, `INSERT INTO usage_ledger (created_at, thread_id, turn_id, provider, model, output_tokens) VALUES (100, 't', 'turn', 'claude', 'm', 42)`)
	if err := applyMigration(db, migrationByVersion(t, 92)); err != nil {
		t.Fatal(err)
	}
	var output, pending int
	if err := db.QueryRow(`SELECT output_tokens, pending FROM usage_records WHERE thread_id = 't'`).Scan(&output, &pending); err != nil {
		t.Fatal(err)
	}
	if output != 42 || pending != 0 {
		t.Fatalf("migrated = %d/%d", output, pending)
	}
	if _, err := db.Exec(`INSERT INTO usage_pending (thread_id,scope,segment,model,created_at,turn_id,provider,input_tokens,output_tokens,cache_read_input_tokens,cache_creation_input_tokens,reasoning_output_tokens) VALUES ('t','p','0','m',100,'turn','claude',0,-1,0,0,0)`); err == nil {
		t.Fatal("negative usage accepted")
	}
}

func TestUsagePendingSnapshotRestore(t *testing.T) {
	s := newTestStore(t)
	row := progressRow(7)
	if _, err := s.PutUsageProgress("scope", "0", []UsageLedgerRow{row}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "snapshot.db")
	if err := s.SnapshotTo(path); err != nil {
		t.Fatal(err)
	}
	row.OutputTokens = 10
	if err := s.AppendUsageAndReconcile("scope", []UsageLedgerRow{row}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RestoreFrom(path); err != nil {
		t.Fatal(err)
	}
	b, _, err := s.ReadUsageStats(UsageQuery{ThreadID: row.ThreadID})
	if err != nil || len(b) != 1 || b[0].OutputTokens != 7 || b[0].PendingRows != 1 {
		t.Fatalf("restored pending usage: %+v %v", b, err)
	}
}

// A settled turn's pending rows are retired even when a different provider
// process reported them, so an interrupted-and-resumed turn cannot leave
// tokens pending forever beside its final accounting.
func TestUsagePendingIsRetiredWhenItsTurnSettlesInAnotherScope(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.PutUsageProgress("old-process", "0", []UsageLedgerRow{progressRow(7)}); err != nil {
		t.Fatal(err)
	}
	other := progressRow(3)
	other.TurnID = "other-turn"
	if _, err := s.PutUsageProgress("old-process", "1", []UsageLedgerRow{other}); err != nil {
		t.Fatal(err)
	}
	final := progressRow(5)
	final.CostUSD = 0.2
	if err := s.AppendUsageAndReconcile("new-process", []UsageLedgerRow{final}); err != nil {
		t.Fatal(err)
	}
	b, _, err := s.ReadUsageStats(UsageQuery{ThreadID: "thread"})
	if err != nil || len(b) != 1 || b[0].OutputTokens != 8 || b[0].PendingRows != 1 || b[0].TurnCount != 1 {
		t.Fatalf("settled turn kept pending tokens: %+v %v", b, err)
	}
}
