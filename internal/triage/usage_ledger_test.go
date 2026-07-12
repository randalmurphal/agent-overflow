package triage

import (
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

func usageTurnCompleteMeta(models ...provider.ModelTokenUsage) *provider.WireTurnCompleteMeta {
	var agg provider.TokenUsage
	for _, m := range models {
		agg.Add(m.TokenUsage)
	}
	return &provider.WireTurnCompleteMeta{
		StopReason: "end_turn",
		Usage:      &agg,
		ModelUsage: models,
	}
}

// TestTurnCompleteAppendsUsageLedgerRows — a settled turn with per-model
// usage lands one ledger row per model, attributed to the thread's
// provider family and project.
func TestTurnCompleteAppendsUsageLedgerRows(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.UnixMilli(1_700_000_000_000),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: usageTurnCompleteMeta(
			provider.ModelTokenUsage{Model: "claude-haiku-4-5", TokenUsage: provider.TokenUsage{
				InputTokens: 10, OutputTokens: 42, TotalCostUSD: 0.02}},
			provider.ModelTokenUsage{Model: "claude-sonnet-4-6", TokenUsage: provider.TokenUsage{
				InputTokens: 5, OutputTokens: 500, CacheCreationInputTokens: 9000, TotalCostUSD: 0.05}},
		),
		Timestamp: time.UnixMilli(1_700_000_001_000),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}

	buckets, err := st.QueryUsage(store.UsageQuery{GroupBy: "model"})
	if err != nil {
		t.Fatalf("query usage: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("model buckets = %d, want 2: %+v", len(buckets), buckets)
	}
	if buckets[0].Bucket != "claude-haiku-4-5" || buckets[0].OutputTokens != 42 {
		t.Fatalf("haiku bucket: %+v", buckets[0])
	}
	if buckets[1].Bucket != "claude-sonnet-4-6" || buckets[1].CacheCreationInputTokens != 9000 {
		t.Fatalf("sonnet bucket: %+v", buckets[1])
	}

	lifetime, err := st.QueryUsage(store.UsageQuery{GroupBy: "provider"})
	if err != nil {
		t.Fatalf("provider query: %v", err)
	}
	if len(lifetime) != 1 || lifetime[0].Bucket != "claude" {
		t.Fatalf("provider attribution: %+v", lifetime)
	}
	if lifetime[0].TurnCount != 1 {
		t.Fatalf("multi-model turn must count once, got %d", lifetime[0].TurnCount)
	}
}

func TestWorkflowTurnUsageStampsLiveWorkItemAttribution(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "workflow-thread")
	router.SetUsageWorkItemResolver(func(threadID string) string {
		if threadID == "workflow-thread" {
			return "work-item-1"
		}
		return ""
	})

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "workflow-thread", TurnIndex: 0,
		Timestamp: time.UnixMilli(1_700_000_000_000),
	}); err != nil {
		t.Fatal(err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "workflow-thread",
		TurnComplete: usageTurnCompleteMeta(provider.ModelTokenUsage{
			Model:      "claude-haiku-4-5",
			TokenUsage: provider.TokenUsage{InputTokens: 7, OutputTokens: 3, TotalCostUSD: 0.02},
		}),
		Timestamp: time.UnixMilli(1_700_000_001_000),
	}); err != nil {
		t.Fatal(err)
	}
	usage, err := st.QueryWorkItemUsage("work-item-1")
	if err != nil {
		t.Fatal(err)
	}
	if usage.TotalTokens != 10 || usage.CostUSD != 0.02 {
		t.Fatalf("attributed usage = %+v", usage)
	}
}

// TestLateTurnPayloadAppendsUsageLedger — a soft round-close settles the
// turn with no usage; the trailing wire result folds late and must still
// append ledger rows (the turns row keeps first-write-wins separately).
func TestLateTurnPayloadAppendsUsageLedger(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.UnixMilli(1_700_000_000_000),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	// Soft close: no usage payload.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: &provider.SoftRoundCloseMeta{StopReason: "end_turn"},
		Timestamp:    time.UnixMilli(1_700_000_001_000),
	}); err != nil {
		t.Fatalf("soft close: %v", err)
	}
	// Trailing wire result routes to persistLateTurnPayload (settled turn).
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: usageTurnCompleteMeta(
			provider.ModelTokenUsage{Model: "claude-haiku-4-5", TokenUsage: provider.TokenUsage{
				InputTokens: 10, OutputTokens: 42, TotalCostUSD: 0.02}},
		),
		Timestamp: time.UnixMilli(1_700_000_002_000),
	}); err != nil {
		t.Fatalf("late fold: %v", err)
	}

	buckets, err := st.QueryUsage(store.UsageQuery{})
	if err != nil {
		t.Fatalf("query usage: %v", err)
	}
	if len(buckets) != 1 || buckets[0].OutputTokens != 42 {
		t.Fatalf("late fold must append ledger rows: %+v", buckets)
	}
	// store.QueryUsage never sets UnpricedRows itself (pricing lookups
	// against internal/usagecost only happen in GetUsageStats/app_usage.go);
	// this is a baseline check that the raw aggregate stays at its
	// always-0 default rather than picking up a stray value.
	if buckets[0].UnpricedRows != 0 {
		t.Fatalf("raw QueryUsage must not report unpriced rows: %+v", buckets[0])
	}
}

// TestUsageLedgerCascadeIsAdditive is the direct regression test for the
// package doc's load-bearing claim: "appending on every settle event is
// additive-correct because providers emit deltas." A turn settles
// normally (the first EventTurnComplete, claimTurnSettlement wins) with
// usage delta A; a SECOND EventTurnComplete for the SAME already-settled
// turn (no intervening EventTurnStart, so currentTurnIndex is unchanged)
// routes to persistLateTurnPayload carrying delta B. Both deltas must
// land as separate ledger rows that sum correctly, and the turn must
// still count once — the turns row settles on the first event only.
func TestUsageLedgerCascadeIsAdditive(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.UnixMilli(1_700_000_000_000),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	// First settle: delta A (out=42, cost 0.02).
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: usageTurnCompleteMeta(
			provider.ModelTokenUsage{Model: "claude-haiku-4-5", TokenUsage: provider.TokenUsage{
				InputTokens: 10, OutputTokens: 42, TotalCostUSD: 0.02}},
		),
		Timestamp: time.UnixMilli(1_700_000_001_000),
	}); err != nil {
		t.Fatalf("first settle: %v", err)
	}
	// Second EventTurnComplete for the same already-settled turn (no
	// EventTurnStart in between) — routes to persistLateTurnPayload,
	// carrying delta B (out=8, cost 0.01).
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: usageTurnCompleteMeta(
			provider.ModelTokenUsage{Model: "claude-haiku-4-5", TokenUsage: provider.TokenUsage{
				InputTokens: 2, OutputTokens: 8, TotalCostUSD: 0.01}},
		),
		Timestamp: time.UnixMilli(1_700_000_002_000),
	}); err != nil {
		t.Fatalf("late cascade fold: %v", err)
	}

	buckets, err := st.QueryUsage(store.UsageQuery{})
	if err != nil {
		t.Fatalf("query usage: %v", err)
	}
	if len(buckets) != 1 {
		t.Fatalf("lifetime buckets = %d, want 1: %+v", len(buckets), buckets)
	}
	b := buckets[0]
	if b.OutputTokens != 50 {
		t.Fatalf("lifetime output tokens = %d, want 50 (A=42 + B=8, additive cascade)", b.OutputTokens)
	}
	if b.InputTokens != 12 {
		t.Fatalf("lifetime input tokens = %d, want 12 (A=10 + B=2)", b.InputTokens)
	}
	if b.TurnCount != 1 {
		t.Fatalf("turn count = %d, want 1 (the cascade is one logical turn, settled once)", b.TurnCount)
	}
}

// TestSubagentTurnCompleteSkipsUsageLedger — a nested turn complete
// (ParentToolUseID set) must not append rows even if a future producer
// attaches usage.
func TestSubagentTurnCompleteSkipsUsageLedger(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.UnixMilli(1_700_000_000_000),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		ParentToolUseID: "toolu_parent",
		TurnComplete: usageTurnCompleteMeta(
			provider.ModelTokenUsage{Model: "claude-haiku-4-5", TokenUsage: provider.TokenUsage{
				InputTokens: 1, OutputTokens: 1}},
		),
		Timestamp: time.UnixMilli(1_700_000_001_000),
	}); err != nil {
		t.Fatalf("nested complete: %v", err)
	}

	buckets, err := st.QueryUsage(store.UsageQuery{})
	if err != nil {
		t.Fatalf("query usage: %v", err)
	}
	if len(buckets) != 0 {
		t.Fatalf("nested turn appended ledger rows: %+v", buckets)
	}
}
