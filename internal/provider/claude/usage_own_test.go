package claude

import (
	"testing"
)

// The thread's own usage is the flat, parent-only result.usage, not the
// subagent-inclusive modelUsage the ledger keeps: in the captured delegating
// turn (subagent_usage_inclusion_20260703.ndjson) the thread spent 23,369
// itself and 36,709 with its subagent.
func TestAResultsOwnUsageLeavesItsSubagentsOut(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"result","subtype":"success","stop_reason":"end_turn","total_cost_usd":0.082,` +
		`"usage":{"input_tokens":42,"output_tokens":1159,"cache_read_input_tokens":148080,"cache_creation_input_tokens":22168},` +
		`"modelUsage":{"claude-haiku-4-5":{"inputTokens":52,"outputTokens":1260,"cacheReadInputTokens":148080,"cacheCreationInputTokens":35397,"costUSD":0.082}}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatal(err)
	}
	meta := requireWireTurnComplete(t, events)
	if meta.Usage == nil || meta.Usage.Spend() != 36709 {
		t.Fatalf("usage %+v; want the subagent-inclusive 36,709", meta.Usage)
	}
	if meta.Usage.InputTokens != 52 || meta.Usage.OutputTokens != 1260 || meta.Usage.CacheCreationInputTokens != 35397 {
		t.Fatalf("aggregate must be subagent-inclusive modelUsage, got %+v", meta.Usage)
	}
	if len(meta.ModelUsage) != 1 {
		t.Fatalf("perModel: %+v", meta.ModelUsage)
	}
	if meta.OwnUsage == nil || meta.OwnUsage.Spend() != 23369 || meta.OwnUsage.TotalCostUSD != 0 {
		t.Fatalf("own usage %+v; want the parent-only 23,369 and no cost", meta.OwnUsage)
	}
}

// A result that accounts nothing (a replayed result's zero delta, an
// interrupt's empty modelUsage) has no own usage either; the thread's own
// live usage of an unaccounted turn goes with the next result that
// accounts it, whose cumulative delta covers that turn too.
func TestOwnUsageFollowsTheResultThatAccountsIt(t *testing.T) {
	parser := NewParser()
	own := func(line []byte) int64 {
		t.Helper()
		events, err := parser.ParseLine(testThread, line)
		if err != nil {
			t.Fatal(err)
		}
		meta := requireWireTurnComplete(t, events)
		if meta.OwnUsage == nil {
			return -1
		}
		return meta.OwnUsage.Spend()
	}
	// cumulativeResultLine's flat usage spends 10 + 50 + 40; the first
	// result accounts it.
	const flat = 100
	if got := own(cumulativeResultLine(0.02, 10, 42, 100, 50, 0.02)); got != flat {
		t.Fatalf("first result's own usage %d; want its flat %d", got, flat)
	}
	if got := own(cumulativeResultLine(0.02, 10, 42, 100, 50, 0.02)); got != -1 {
		t.Fatalf("a replayed result's own usage %d; want none", got)
	}
	// An interrupted turn: the parent spent 7 + 3 live, a subagent's
	// message is no part of it, and its result accounts nothing.
	for _, line := range []string{
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"m-int","model":"claude-haiku-4-5","usage":{"input_tokens":7,"output_tokens":3}}}}`,
		`{"type":"stream_event","parent_tool_use_id":"child","event":{"type":"message_start","message":{"id":"m-child","model":"claude-haiku-4-5","usage":{"input_tokens":500,"output_tokens":100}}}}`,
	} {
		if _, err := parser.ParseLine(testThread, []byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if got := own([]byte(ede2_1_170InterruptResultLine)); got != -1 {
		t.Fatalf("an interrupt's own usage %d; want none", got)
	}
	if got := own(cumulativeResultLine(0.03, 15, 60, 150, 50, 0.03)); got != flat+10 {
		t.Fatalf("the next result's own usage %d; want its flat %d and the interrupted turn's 10", got, flat)
	}
}
