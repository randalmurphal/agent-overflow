package codex

import (
	"encoding/json"
	"fmt"
	"testing"

	"agent-overflow/internal/provider"
)

// wireTotals is the cumulative `tokenUsage.total` breakdown a fixture
// notification carries. Named fields (rather than a run of positional
// ints) keep the call sites readable now that the breakdown has six
// components; a field left unset is the wire omitting that key.
type wireTotals struct {
	total      int
	input      int
	cached     int
	cacheWrite int
	output     int
	reasoning  int
}

// tokenUsageParams renders one `thread/tokenUsage/updated` params payload.
// The JSON is written out by hand rather than marshalled from
// codexWireTokenBreakdown so the fixtures assert the real wire key
// spelling instead of echoing the struct tags back at themselves.
func tokenUsageParams(total wireTotals, window int) json.RawMessage {
	return fmt.Appendf(nil,
		`{"threadId":"t","tokenUsage":{"total":{"totalTokens":%d,"inputTokens":%d,"cachedInputTokens":%d,`+
			`"cacheWriteInputTokens":%d,"outputTokens":%d,"reasoningOutputTokens":%d},`+
			`"last":{"totalTokens":1},"modelContextWindow":%d}}`,
		total.total, total.input, total.cached, total.cacheWrite, total.output, total.reasoning, window)
}

// TestUsageAccounting_FreshThreadTurnDeltas — the core cumulative→delta
// path: two turns on a fresh thread, several observations per turn, the
// settle yields only each turn's growth. Wire inputTokens includes
// cachedInputTokens; the normalized delta separates non-cached input from
// cache reads.
func TestUsageAccounting_FreshThreadTurnDeltas(t *testing.T) {
	a := newUsageAccounting(false)
	a.onTurnStart()
	a.observe(tokenUsageParams(wireTotals{total: 5000, input: 4000, cached: 3000, output: 1000, reasoning: 200}, 258400))
	a.observe(tokenUsageParams(wireTotals{total: 11839, input: 9000, cached: 8390, output: 2839, reasoning: 400}, 258400))

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
	a.observe(tokenUsageParams(wireTotals{total: 20000, input: 15000, cached: 13000, output: 5000, reasoning: 700}, 258400))
	turn2 := a.settleTurn()
	if turn2.InputTokens != (15000-13000)-(9000-8390) || turn2.CacheReadInputTokens != 13000-8390 {
		t.Fatalf("turn2 input split: %+v", turn2)
	}
	if turn2.OutputTokens != 5000-2839 || turn2.ReasoningOutputTokens != 300 {
		t.Fatalf("turn2 output: %+v", turn2)
	}
}

// TestUsageAccounting_CacheWriteDeltasAcrossTurns — cacheWriteInputTokens
// is a cumulative component like every other one: each turn accounts only
// its own growth, and the class stays on its own axis (it is neither
// folded into non-cached input nor into cache reads).
func TestUsageAccounting_CacheWriteDeltasAcrossTurns(t *testing.T) {
	a := newUsageAccounting(false)
	a.onTurnStart()
	a.observe(tokenUsageParams(wireTotals{total: 5000, input: 4000, cached: 3000, cacheWrite: 1200, output: 1000}, 258400))
	turn1 := a.settleTurn()
	if turn1.CacheCreationInputTokens != 1200 {
		t.Fatalf("turn1 cache creation: %+v", turn1)
	}
	if turn1.InputTokens != 1000 || turn1.CacheReadInputTokens != 3000 {
		t.Fatalf("turn1 must not fold cache writes into the input split: %+v", turn1)
	}

	a.onTurnStart()
	a.observe(tokenUsageParams(wireTotals{total: 9000, input: 7000, cached: 5500, cacheWrite: 1900, output: 2000}, 258400))
	turn2 := a.settleTurn()
	if turn2.CacheCreationInputTokens != 1900-1200 {
		t.Fatalf("turn2 cache creation delta: %+v", turn2)
	}
	if turn2.InputTokens != (7000-5500)-(4000-3000) || turn2.CacheReadInputTokens != 5500-3000 {
		t.Fatalf("turn2 input split: %+v", turn2)
	}
}

// TestUsageAccounting_CacheWriteResumeSeedIsHistory — the resume seed
// baselines cache writes with the rest of the breakdown, so the first
// post-resume turn accounts its own writes rather than the thread's.
func TestUsageAccounting_CacheWriteResumeSeedIsHistory(t *testing.T) {
	a := newUsageAccounting(true)
	a.observe(tokenUsageParams(wireTotals{total: 90000, input: 70000, cached: 60000, cacheWrite: 5000, output: 20000}, 258400))
	a.onTurnStart()
	a.observe(tokenUsageParams(wireTotals{total: 95000, input: 74000, cached: 64000, cacheWrite: 5300, output: 21000}, 258400))
	got := a.settleTurn()
	if got.CacheCreationInputTokens != 300 {
		t.Fatalf("resumed turn cache creation: %+v", got)
	}
}

// TestUsageAccounting_CacheWriteAbsentKeyIsZero — app-servers older than
// the field's introduction omit the key entirely; that reads as zero
// writes, not as a decode failure or a shifted component.
func TestUsageAccounting_CacheWriteAbsentKeyIsZero(t *testing.T) {
	a := newUsageAccounting(false)
	a.onTurnStart()
	a.observe(json.RawMessage(`{"threadId":"t","tokenUsage":{"total":` +
		`{"totalTokens":5000,"inputTokens":4000,"cachedInputTokens":3000,"outputTokens":1000},` +
		`"modelContextWindow":258400}}`))
	got := a.settleTurn()
	if got.CacheCreationInputTokens != 0 {
		t.Fatalf("absent key must read as zero: %+v", got)
	}
	if got.InputTokens != 1000 || got.CacheReadInputTokens != 3000 || got.OutputTokens != 1000 {
		t.Fatalf("rest of the breakdown must be unaffected: %+v", got)
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
	a.observe(tokenUsageParams(wireTotals{total: 90000, input: 70000, cached: 60000, output: 20000, reasoning: 3000}, 258400)) // historical seed
	a.onTurnStart()
	a.observe(tokenUsageParams(wireTotals{total: 95000, input: 74000, cached: 64000, output: 21000, reasoning: 3100}, 258400))
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
	a.observe(tokenUsageParams(wireTotals{total: 95000, input: 74000, cached: 64000, output: 21000, reasoning: 3100}, 258400)) // history + turn mixed
	if got := a.settleTurn(); !got.IsZero() {
		t.Fatalf("first resumed turn must skip accounting, got %+v", got)
	}
	a.onTurnStart()
	a.observe(tokenUsageParams(wireTotals{total: 97000, input: 75000, cached: 65000, output: 22000, reasoning: 3100}, 258400))
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
	a.observe(tokenUsageParams(wireTotals{total: 5000, input: 4000, cached: 3000, output: 1000}, 258400))
	a.observe(tokenUsageParams(wireTotals{total: 258400}, 258400)) // sentinel
	if got := a.settleTurn(); !got.IsZero() {
		t.Fatalf("sentinel turn must not account, got %+v", got)
	}
	// Post-sentinel growth (codex keeps add_assign-ing onto the pegged
	// total) accounts as a normal delta.
	a.onTurnStart()
	a.observe(tokenUsageParams(wireTotals{total: 260400, input: 1500, cached: 500, output: 500}, 258400))
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
	a.observe(tokenUsageParams(wireTotals{total: 5000, input: 4000, cached: 3000, output: 1000}, 258400))
	if got := a.settleTurn(); got.IsZero() {
		t.Fatalf("turn 1 should account")
	}
	a.onTurnStart()
	a.observe(tokenUsageParams(wireTotals{total: 2000, input: 1500, cached: 1000, output: 500}, 258400))
	if got := a.settleTurn(); !got.IsZero() {
		t.Fatalf("backwards cumulative must re-baseline, got %+v", got)
	}
}

// TestAttachTurnUsage_StampsAggregateAndModel — the session hook writes
// both the aggregate and the single-model attribution onto the wire meta.
func TestAttachTurnUsage_StampsAggregateAndModel(t *testing.T) {
	s := &Session{
		usageAcct: newUsageAccounting(false),
		turnConfig: sessionTurnConfig{
			model: "gpt-5.2-codex",
		},
	}
	s.usageAcct.onTurnStart()
	s.usageAcct.observe(tokenUsageParams(wireTotals{total: 5000, input: 4000, cached: 3000, output: 1000, reasoning: 200}, 258400))

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
