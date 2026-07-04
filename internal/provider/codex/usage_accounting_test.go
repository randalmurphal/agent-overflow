package codex

import (
	"encoding/json"
	"fmt"
	"testing"

	"agent-overflow/internal/provider"
)

func tokenUsageParams(totalTokens, input, cached, output, reasoning, window int) json.RawMessage {
	return fmt.Appendf(nil,
		`{"threadId":"t","tokenUsage":{"total":{"totalTokens":%d,"inputTokens":%d,"cachedInputTokens":%d,"outputTokens":%d,"reasoningOutputTokens":%d},`+
			`"last":{"totalTokens":1},"modelContextWindow":%d}}`,
		totalTokens, input, cached, output, reasoning, window)
}

// TestUsageAccounting_FreshThreadTurnDeltas — the core cumulative→delta
// path: two turns on a fresh thread, several observations per turn, the
// settle yields only each turn's growth. Wire inputTokens includes
// cachedInputTokens; the normalized delta separates non-cached input from
// cache reads.
func TestUsageAccounting_FreshThreadTurnDeltas(t *testing.T) {
	a := newUsageAccounting(false)
	a.onTurnStart()
	a.observe(tokenUsageParams(5000, 4000, 3000, 1000, 200, 258400))
	a.observe(tokenUsageParams(11839, 9000, 8390, 2839, 400, 258400))

	turn1 := a.settleTurn()
	if turn1.InputTokens != 9000-8390 || turn1.CacheReadInputTokens != 8390 {
		t.Fatalf("turn1 input split: %+v", turn1)
	}
	if turn1.OutputTokens != 2839 || turn1.ReasoningOutputTokens != 400 {
		t.Fatalf("turn1 output: %+v", turn1)
	}
	if turn1.TotalCostUSD != 0 {
		t.Fatalf("codex must not carry cost, got %g", turn1.TotalCostUSD)
	}

	a.onTurnStart()
	a.observe(tokenUsageParams(20000, 15000, 13000, 5000, 700, 258400))
	turn2 := a.settleTurn()
	if turn2.InputTokens != (15000-13000)-(9000-8390) || turn2.CacheReadInputTokens != 13000-8390 {
		t.Fatalf("turn2 input split: %+v", turn2)
	}
	if turn2.OutputTokens != 5000-2839 || turn2.ReasoningOutputTokens != 300 {
		t.Fatalf("turn2 output: %+v", turn2)
	}
}

// TestUsageAccounting_NoObservationsSettlesZero — a turn with no
// tokenUsage notifications (send failure, instant interrupt) accounts
// nothing.
func TestUsageAccounting_NoObservationsSettlesZero(t *testing.T) {
	a := newUsageAccounting(false)
	a.onTurnStart()
	if got := a.settleTurn(); !got.IsZero() {
		t.Fatalf("expected zero, got %+v", got)
	}
}

// TestUsageAccounting_ResumeSeedBeforeFirstTurn — the rollout-seeded
// cumulative arriving BEFORE the first turn is history, not this
// process's spend; the first turn accounts only its own growth.
func TestUsageAccounting_ResumeSeedBeforeFirstTurn(t *testing.T) {
	a := newUsageAccounting(true)
	a.observe(tokenUsageParams(90000, 70000, 60000, 20000, 3000, 258400)) // historical seed
	a.onTurnStart()
	a.observe(tokenUsageParams(95000, 74000, 64000, 21000, 3100, 258400))
	got := a.settleTurn()
	if got.OutputTokens != 1000 || got.CacheReadInputTokens != 4000 {
		t.Fatalf("resumed turn delta: %+v", got)
	}
}

// TestUsageAccounting_ResumeWithoutSeedSkipsFirstTurn — with no pre-turn
// observation the first turn's delta would swallow the thread's whole
// history; it must be skipped, and the second turn must be exact.
func TestUsageAccounting_ResumeWithoutSeedSkipsFirstTurn(t *testing.T) {
	a := newUsageAccounting(true)
	a.onTurnStart()
	a.observe(tokenUsageParams(95000, 74000, 64000, 21000, 3100, 258400)) // history + turn mixed
	if got := a.settleTurn(); !got.IsZero() {
		t.Fatalf("first resumed turn must skip accounting, got %+v", got)
	}
	a.onTurnStart()
	a.observe(tokenUsageParams(97000, 75000, 65000, 22000, 3100, 258400))
	got := a.settleTurn()
	if got.OutputTokens != 1000 || got.CacheReadInputTokens != 1000 || got.InputTokens != 0 {
		t.Fatalf("second resumed turn delta: %+v", got)
	}
}

// TestUsageAccounting_ExceededSentinelRebaselines — fill_to_context_window
// pegs total.totalTokens to the window with zeroed components, destroying
// the cumulative. The stretch across the sentinel is dropped instead of
// producing a garbage delta.
func TestUsageAccounting_ExceededSentinelRebaselines(t *testing.T) {
	a := newUsageAccounting(false)
	a.onTurnStart()
	a.observe(tokenUsageParams(5000, 4000, 3000, 1000, 0, 258400))
	a.observe(tokenUsageParams(258400, 0, 0, 0, 0, 258400)) // sentinel
	if got := a.settleTurn(); !got.IsZero() {
		t.Fatalf("sentinel turn must not account, got %+v", got)
	}
	// Post-sentinel growth (codex keeps add_assign-ing onto the pegged
	// total) accounts as a normal delta.
	a.onTurnStart()
	a.observe(tokenUsageParams(260400, 1500, 500, 500, 0, 258400))
	got := a.settleTurn()
	if got.InputTokens != 1000 || got.CacheReadInputTokens != 500 || got.OutputTokens != 500 {
		t.Fatalf("post-sentinel delta: %+v", got)
	}
}

// TestUsageAccounting_BackwardsCumulativeRebaselines — a total that moves
// backwards (unhealthy wire) re-baselines silently instead of clamping
// into a fake delta.
func TestUsageAccounting_BackwardsCumulativeRebaselines(t *testing.T) {
	a := newUsageAccounting(false)
	a.onTurnStart()
	a.observe(tokenUsageParams(5000, 4000, 3000, 1000, 0, 258400))
	if got := a.settleTurn(); got.IsZero() {
		t.Fatalf("turn 1 should account")
	}
	a.onTurnStart()
	a.observe(tokenUsageParams(2000, 1500, 1000, 500, 0, 258400))
	if got := a.settleTurn(); !got.IsZero() {
		t.Fatalf("backwards cumulative must re-baseline, got %+v", got)
	}
}

// TestAttachTurnUsage_StampsAggregateAndModel — the session hook writes
// both the aggregate and the single-model attribution onto the wire meta.
func TestAttachTurnUsage_StampsAggregateAndModel(t *testing.T) {
	s := &Session{model: "gpt-5.2-codex", usageAcct: newUsageAccounting(false)}
	s.usageAcct.onTurnStart()
	s.usageAcct.observe(tokenUsageParams(5000, 4000, 3000, 1000, 200, 258400))

	meta := &provider.WireTurnCompleteMeta{StopReason: "end_turn"}
	s.attachTurnUsage(meta)
	if meta.Usage == nil || meta.Usage.OutputTokens != 1000 {
		t.Fatalf("aggregate not attached: %+v", meta.Usage)
	}
	if len(meta.ModelUsage) != 1 || meta.ModelUsage[0].Model != "gpt-5.2-codex" {
		t.Fatalf("model attribution: %+v", meta.ModelUsage)
	}

	// A turn with no new usage attaches nothing.
	meta2 := &provider.WireTurnCompleteMeta{StopReason: "end_turn"}
	s.attachTurnUsage(meta2)
	if meta2.Usage != nil || meta2.ModelUsage != nil {
		t.Fatalf("empty turn must not attach usage: %+v %+v", meta2.Usage, meta2.ModelUsage)
	}
}
