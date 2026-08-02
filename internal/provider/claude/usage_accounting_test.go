package claude

import (
	"fmt"
	"math"
	"testing"

	"agent-overflow/internal/provider"
)

// The cumulative envelopes below are condensed from the live 3-turn capture
// docs/references/fixtures/claude/multiturn_cost_cumulative_20260703.ndjson
// (claude 2.x, haiku): total_cost_usd and modelUsage grow monotonically
// across turns while flat usage stays per-turn.
func cumulativeResultLine(totalCost float64, in, out, cr, cc int, modelCost float64) []byte {
	return fmt.Appendf(nil,
		`{"type":"result","subtype":"success","stop_reason":"end_turn","total_cost_usd":%g,`+
			`"usage":{"input_tokens":10,"output_tokens":40,"cache_read_input_tokens":100,"cache_creation_input_tokens":50},`+
			`"modelUsage":{"claude-haiku-4-5":{"inputTokens":%d,"outputTokens":%d,"cacheReadInputTokens":%d,"cacheCreationInputTokens":%d,"costUSD":%g}}}`,
		totalCost, in, out, cr, cc, modelCost)
}

func requireUsage(t *testing.T, events []provider.ProviderEvent) (*provider.TokenUsage, []provider.ModelTokenUsage) {
	t.Helper()
	meta := requireWireTurnComplete(t, events)
	return meta.Usage, meta.ModelUsage
}

func approxEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestParseResult_ModelUsageCumulativeToDelta is the double-count
// regression guard: modelUsage/total_cost_usd are session-cumulative on
// the wire (spike 2026-07-03), so each result must yield only the delta.
func TestParseResult_ModelUsageCumulativeToDelta(t *testing.T) {
	parser := NewParser()

	turns := []struct {
		line     []byte
		wantIn   int
		wantOut  int
		wantCost float64
	}{
		{cumulativeResultLine(0.0216, 10, 42, 16491, 9884, 0.0216), 10, 42, 0.0216},
		{cumulativeResultLine(0.0252, 20, 75, 42866, 10291, 0.0252), 10, 33, 0.0036},
		{cumulativeResultLine(0.0281, 30, 114, 69648, 10305, 0.0281), 10, 39, 0.0029},
	}
	for i, turn := range turns {
		events, err := parser.ParseLine(testThread, turn.line)
		if err != nil {
			t.Fatalf("turn %d: %v", i+1, err)
		}
		usage, perModel := requireUsage(t, events)
		if usage == nil {
			t.Fatalf("turn %d: no usage payload", i+1)
		}
		if usage.InputTokens != turn.wantIn || usage.OutputTokens != turn.wantOut {
			t.Fatalf("turn %d: in/out = %d/%d, want %d/%d",
				i+1, usage.InputTokens, usage.OutputTokens, turn.wantIn, turn.wantOut)
		}
		if !approxEqual(usage.TotalCostUSD, turn.wantCost) {
			t.Fatalf("turn %d: cost = %g, want %g", i+1, usage.TotalCostUSD, turn.wantCost)
		}
		if len(perModel) != 1 || perModel[0].Model != "claude-haiku-4-5" {
			t.Fatalf("turn %d: perModel = %+v, want single claude-haiku-4-5 entry", i+1, perModel)
		}
		if perModel[0].InputTokens != turn.wantIn || !approxEqual(perModel[0].TotalCostUSD, turn.wantCost) {
			t.Fatalf("turn %d: perModel delta = %+v", i+1, perModel[0])
		}
	}
}

// TestParseResult_ModelUsagePreferredOverFlatUsage mirrors the subagent
// capture (subagent_usage_inclusion_20260703.ndjson): flat usage is
// parent-only while modelUsage includes Task-subagent tokens — the
// aggregate must come from modelUsage, not the flat object.
func TestParseResult_ModelUsagePreferredOverFlatUsage(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"result","subtype":"success","stop_reason":"end_turn","total_cost_usd":0.082,` +
		`"usage":{"input_tokens":42,"output_tokens":1159,"cache_read_input_tokens":148080,"cache_creation_input_tokens":22168},` +
		`"modelUsage":{"claude-haiku-4-5":{"inputTokens":52,"outputTokens":1260,"cacheReadInputTokens":148080,"cacheCreationInputTokens":35397,"costUSD":0.082}}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	usage, perModel := requireUsage(t, events)
	if usage.InputTokens != 52 || usage.OutputTokens != 1260 || usage.CacheCreationInputTokens != 35397 {
		t.Fatalf("aggregate must be subagent-inclusive modelUsage, got %+v", usage)
	}
	if len(perModel) != 1 {
		t.Fatalf("perModel: %+v", perModel)
	}
}

// TestParseResult_NewModelMidSessionCountsFromZero — a model that first
// appears on turn N (subagent using a different model) has no prior
// snapshot; its full cumulative value is that turn's delta.
func TestParseResult_NewModelMidSessionCountsFromZero(t *testing.T) {
	parser := NewParser()
	if _, err := parser.ParseLine(testThread, cumulativeResultLine(0.02, 10, 42, 100, 50, 0.02)); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	line := []byte(`{"type":"result","subtype":"success","stop_reason":"end_turn","total_cost_usd":0.05,` +
		`"usage":{"input_tokens":10,"output_tokens":10,"cache_read_input_tokens":0,"cache_creation_input_tokens":0},` +
		`"modelUsage":{` +
		`"claude-haiku-4-5":{"inputTokens":20,"outputTokens":80,"cacheReadInputTokens":200,"cacheCreationInputTokens":60,"costUSD":0.03},` +
		`"claude-sonnet-4-6":{"inputTokens":5,"outputTokens":500,"cacheReadInputTokens":0,"cacheCreationInputTokens":9000,"costUSD":0.02}}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	usage, perModel := requireUsage(t, events)
	if len(perModel) != 2 {
		t.Fatalf("perModel count = %d, want 2 (%+v)", len(perModel), perModel)
	}
	// Sorted by model name: haiku first, sonnet second.
	if perModel[0].Model != "claude-haiku-4-5" || perModel[0].InputTokens != 10 || perModel[0].OutputTokens != 38 {
		t.Fatalf("haiku delta: %+v", perModel[0])
	}
	if perModel[1].Model != "claude-sonnet-4-6" || perModel[1].InputTokens != 5 || perModel[1].CacheCreationInputTokens != 9000 {
		t.Fatalf("sonnet full-cumulative-as-delta: %+v", perModel[1])
	}
	if usage.OutputTokens != 38+500 {
		t.Fatalf("aggregate output = %d, want %d", usage.OutputTokens, 538)
	}
}

// TestParseResult_InterruptedResultLeavesSnapshotUntouched — interrupted
// turns carry empty modelUsage and total_cost_usd 0; they must produce no
// usage and must not disturb the cumulative baseline for the next turn.
func TestParseResult_InterruptedResultLeavesSnapshotUntouched(t *testing.T) {
	parser := NewParser()
	if _, err := parser.ParseLine(testThread, cumulativeResultLine(0.02, 10, 42, 100, 50, 0.02)); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	events, err := parser.ParseLine(testThread, []byte(ede2_1_170InterruptResultLine))
	if err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	usage, perModel := requireUsage(t, events)
	if usage != nil || perModel != nil {
		t.Fatalf("interrupted result produced usage: %v %v", usage, perModel)
	}
	// Turn 3 resumes cumulative growth; delta must be measured against
	// turn 1's snapshot, not zero.
	events, err = parser.ParseLine(testThread, cumulativeResultLine(0.03, 15, 60, 150, 50, 0.03))
	if err != nil {
		t.Fatalf("turn 3: %v", err)
	}
	usage, _ = requireUsage(t, events)
	if usage == nil || usage.InputTokens != 5 || usage.OutputTokens != 18 {
		t.Fatalf("post-interrupt delta: %+v", usage)
	}
	if !approxEqual(usage.TotalCostUSD, 0.01) {
		t.Fatalf("post-interrupt cost delta = %g, want 0.01", usage.TotalCostUSD)
	}
}

// TestParseResult_DuplicateModelUsageDoesNotFallThroughToFlat is the
// double-count regression guard for the flat-fallthrough hole: a
// replayed/duplicated result envelope for an already-accounted turn
// carries modelUsage with the SAME cumulative values as the prior turn
// (so every per-model delta is zero) alongside a non-zero flat `usage`
// and the same total_cost_usd. Before the fix, `takeTurnUsage` treated
// the empty per-model slice as "modelUsage absent" and fell through to
// takeFlatUsageDelta, re-emitting the flat parent-only usage as a fresh
// delta — a double count. modelUsage being PRESENT (even with an
// all-zero delta) must suppress the flat fallback entirely.
func TestParseResult_DuplicateModelUsageDoesNotFallThroughToFlat(t *testing.T) {
	parser := NewParser()

	// Turn 1: establishes the cumulative snapshot.
	line1 := cumulativeResultLine(0.0216, 10, 42, 16491, 9884, 0.0216)
	events, err := parser.ParseLine(testThread, line1)
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	usage, perModel := requireUsage(t, events)
	if usage == nil || usage.InputTokens != 10 || usage.OutputTokens != 42 {
		t.Fatalf("turn 1 usage: %+v", usage)
	}

	// Duplicate envelope: IDENTICAL modelUsage cumulative and identical
	// total_cost_usd (a replayed/duplicated result for the same settled
	// turn). cumulativeResultLine's flat `usage` object is non-zero and
	// fixed regardless of the modelUsage args, so this exercises the
	// exact double-count hole: a naive "empty perModel means modelUsage
	// was absent" check would fall through to that non-zero flat usage.
	dup := cumulativeResultLine(0.0216, 10, 42, 16491, 9884, 0.0216)
	events, err = parser.ParseLine(testThread, dup)
	if err != nil {
		t.Fatalf("duplicate: %v", err)
	}
	usage, perModel = requireUsage(t, events)
	if usage != nil || perModel != nil {
		t.Fatalf("duplicate result must yield nil usage/ModelUsage, got %v %v", usage, perModel)
	}

	// Turn 3: cumulative grows from turn 1's snapshot (the duplicate must
	// not have disturbed it) — same grown values as
	// TestParseResult_ModelUsageCumulativeToDelta's turn 2, same expected
	// delta.
	line3 := cumulativeResultLine(0.0252, 20, 75, 42866, 10291, 0.0252)
	events, err = parser.ParseLine(testThread, line3)
	if err != nil {
		t.Fatalf("turn 3: %v", err)
	}
	usage, perModel = requireUsage(t, events)
	if usage == nil || usage.InputTokens != 10 || usage.OutputTokens != 33 {
		t.Fatalf("turn 3 delta: %+v", usage)
	}
	if !approxEqual(usage.TotalCostUSD, 0.0036) {
		t.Fatalf("turn 3 cost delta = %g, want 0.0036", usage.TotalCostUSD)
	}
	if len(perModel) != 1 || perModel[0].Model != "claude-haiku-4-5" {
		t.Fatalf("turn 3 perModel: %+v", perModel)
	}
}

// TestParseResult_FlatUsageFallbackForSynthesizedResults — claudetui's
// synthesized result envelopes carry only a per-turn flat `usage` (no
// modelUsage, no total_cost_usd). Tokens must persist, attributed to the
// tracked model, with cost 0 (wire-reported cost only; no pricing table).
func TestParseResult_FlatUsageFallbackForSynthesizedResults(t *testing.T) {
	parser := NewParser()
	parser.SetModel("claude-opus-4-8")
	line := []byte(`{"type":"result","subtype":"success","stop_reason":"end_turn",` +
		`"usage":{"input_tokens":12,"output_tokens":300,"cache_read_input_tokens":5000,"cache_creation_input_tokens":700}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	usage, perModel := requireUsage(t, events)
	if usage == nil || usage.InputTokens != 12 || usage.OutputTokens != 300 {
		t.Fatalf("flat usage: %+v", usage)
	}
	if usage.TotalCostUSD != 0 {
		t.Fatalf("cost must stay 0 without a wire-reported cost, got %g", usage.TotalCostUSD)
	}
	if len(perModel) != 1 || perModel[0].Model != "claude-opus-4-8" {
		t.Fatalf("fallback attribution: %+v", perModel)
	}
}

// TestParseResult_AutoModeClassifierRowIsAccountedNotDropped uses the real
// `result` envelope captured from a `--permission-mode auto` turn on claude
// 2.1.219 (the 2026-08-02 Claude spike). Auto adds a row to `modelUsage` for
// the Haiku classifier that adjudicated the Bash call — on a thread whose
// selected model is Fable.
//
// Two properties matter and they pull in opposite directions:
//
//   - Cost must stay exact. The classifier's costUSD is real spend, so the
//     turn aggregate has to include it. Dropping the unfamiliar row (or
//     attributing the whole turn to the thread's model) would understate the
//     turn or misprice it.
//   - Attribution must stay honest. The classifier row is a DIFFERENT model
//     from the one the user picked, and the ledger records it as such rather
//     than folding it into the thread's model.
//
// The accounting is model-name-agnostic by construction — a cumulative map
// keyed by whatever the wire reports — so this needs no code, and that is
// exactly what the test pins. The consequence the UI has not yet caught up
// with is that a Fable thread now shows a Haiku row; labelling classifier
// rows as such is a tracked follow-up, not a behaviour this test asserts.
func TestParseResult_AutoModeClassifierRowIsAccountedNotDropped(t *testing.T) {
	parser := NewParser()
	parser.SetModel("claude-fable-5")
	line := []byte(`{"type":"result","subtype":"success","stop_reason":"end_turn","total_cost_usd":0.23152199999999998,` +
		`"usage":{"input_tokens":4,"output_tokens":112,"cache_read_input_tokens":39632,"cache_creation_input_tokens":9283},` +
		`"modelUsage":{` +
		`"claude-haiku-4-5-20251001":{"inputTokens":530,"outputTokens":12,"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"costUSD":0.00059},` +
		`"claude-fable-5":{"inputTokens":4,"outputTokens":112,"cacheReadInputTokens":39632,"cacheCreationInputTokens":9283,"costUSD":0.23093199999999997}}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse auto-mode result: %v", err)
	}
	usage, perModel := requireUsage(t, events)

	if len(perModel) != 2 {
		t.Fatalf("perModel count = %d, want the thread model plus the classifier row (%+v)", len(perModel), perModel)
	}
	// Sorted by model name: fable before haiku.
	if perModel[0].Model != "claude-fable-5" || perModel[0].OutputTokens != 112 {
		t.Errorf("thread-model row = %+v, want the fable turn", perModel[0])
	}
	if perModel[1].Model != "claude-haiku-4-5-20251001" || perModel[1].InputTokens != 530 || perModel[1].OutputTokens != 12 {
		t.Errorf("classifier row = %+v, want the haiku classifier usage", perModel[1])
	}
	if usage.InputTokens != 4+530 || usage.OutputTokens != 112+12 {
		t.Errorf("aggregate tokens = in %d / out %d, want in %d / out %d",
			usage.InputTokens, usage.OutputTokens, 4+530, 112+12)
	}
	if wantCost := 0.00059 + 0.23093199999999997; math.Abs(usage.TotalCostUSD-wantCost) > 1e-9 {
		t.Errorf("aggregate cost = %v, want %v (the classifier call is billed spend)", usage.TotalCostUSD, wantCost)
	}
}
