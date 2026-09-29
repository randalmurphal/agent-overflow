package claude

import (
	"fmt"
	"slices"
	"testing"

	"agent-overflow/internal/provider"
)

func TestUsageProgressMessageSnapshotsAndInterruptedSegments(t *testing.T) {
	p := NewParser()
	parse := func(line string) []provider.ProviderEvent {
		t.Helper()
		events, err := p.ParseLine(testThread, []byte(line))
		if err != nil {
			t.Fatal(err)
		}
		return events
	}
	progress := func(events []provider.ProviderEvent, output int) *provider.UsageProgress {
		t.Helper()
		for _, e := range events {
			if e.Kind == provider.EventUsageProgress {
				if len(e.UsageProgress.ModelUsage) != 1 || e.UsageProgress.ModelUsage[0].OutputTokens != output {
					t.Fatalf("progress: %+v", e.UsageProgress)
				}
				return e.UsageProgress
			}
		}
		t.Fatal("missing progress")
		return nil
	}
	start := `{"type":"stream_event","event":{"type":"message_start","message":{"id":"m1","model":"claude-haiku-4-5","usage":{"input_tokens":10,"output_tokens":1}}}}`
	first := progress(parse(start), 1)
	if first.Scope == "" {
		t.Fatal("missing accounting identity")
	}
	if len(parse(start)) != 0 {
		t.Fatal("duplicate message_start published twice")
	}
	delta := `{"type":"stream_event","event":{"type":"message_delta","usage":{"output_tokens":8}}}`
	latest := progress(parse(delta), 8)
	if latest.Scope != first.Scope || latest.ModelUsage[0].InputTokens != 10 {
		t.Fatalf("partial fields lost: %+v", latest)
	}
	snapshot := `{"type":"assistant","message":{"id":"m1","model":"claude-haiku-4-5","content":[],"usage":{"input_tokens":10,"output_tokens":8}}}`
	for _, e := range parse(snapshot) {
		if e.Kind == provider.EventUsageProgress {
			t.Fatal("assistant duplicate added usage")
		}
	}
	result := requireWireTurnComplete(t, parse(ede2_1_170InterruptResultLine))
	if result.UsageScope != first.Scope {
		t.Fatal("interrupt lost scope")
	}
	for _, e := range parse(snapshot) {
		if e.Kind == provider.EventUsageProgress {
			t.Fatal("late duplicate reopened settled segment")
		}
	}
	second := progress(parse(`{"type":"stream_event","event":{"type":"message_start","message":{"id":"m2","model":"claude-haiku-4-5","usage":{"input_tokens":20,"output_tokens":2}}}}`), 2)
	if second.Scope != first.Scope || second.Segment == first.Segment {
		t.Fatal("interrupt must begin a fresh segment in the same scope")
	}
	final := requireWireTurnComplete(t, parse(string(cumulativeResultLine(0.1, 30, 15, 0, 0, 0.1))))
	if final.UsageScope != first.Scope || final.Usage.OutputTokens != 15 {
		t.Fatalf("final accounting: %+v", final)
	}
}

func TestUsageProgressExcludesChildAndAdvisorMessages(t *testing.T) {
	p := NewParser()
	for _, line := range []string{
		`{"type":"stream_event","parent_tool_use_id":"child","event":{"type":"message_start","message":{"id":"m-child","model":"claude-haiku-4-5","usage":{"output_tokens":100}}}}`,
		`{"type":"assistant","parent_tool_use_id":"child","message":{"id":"m-child","model":"claude-haiku-4-5","usage":{"output_tokens":100},"content":[]}}`,
		`{"type":"assistant","message":{"id":"m-advisor","model":"claude-haiku-4-5","usage":{"output_tokens":100},"content":[{"type":"server_tool_use","id":"advisor","name":"advisor"}]}}`,
	} {
		events, err := p.ParseLine(testThread, []byte(line))
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Kind == provider.EventUsageProgress {
				t.Fatalf("scoped usage leaked: %s", line)
			}
		}
	}
}

func TestUsageProgressBoundsMessageDeduplication(t *testing.T) {
	p := NewParser()
	for i := range usageProgressMessageLimit + 10 {
		line := fmt.Sprintf(`{"type":"stream_event","event":{"type":"message_start","message":{"id":"m%d","model":"claude-haiku-4-5","usage":{"output_tokens":1}}}}`, i)
		events, err := p.ParseLine(testThread, []byte(line))
		if err != nil {
			t.Fatal(err)
		}
		if i >= usageProgressMessageLimit && len(events) != 0 {
			t.Fatal("overflow should wait for final accounting")
		}
	}
	if len(p.usageProgress.messages) != usageProgressMessageLimit {
		t.Fatal("unbounded messages")
	}
	if _, err := p.ParseLine(testThread, cumulativeResultLine(1, 0, usageProgressMessageLimit+10, 0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	if len(p.usageProgress.messages) != 0 || len(p.usageProgress.totals) != 0 {
		t.Fatal("result retained active counters")
	}
}

func TestUsageAccountingCanonicalIdentityPreservesReportedModel(t *testing.T) {
	for _, tc := range []struct{ session, model, accounting string }{
		// A dated report of the session's model settles the rows live usage
		// recorded under the session's spelling.
		{"claude-haiku-4-5", "claude-haiku-4-5-20251001", "claude-haiku-4-5"},
		// The report names the session's model exactly as system/init did.
		{"claude-opus-4-7[1m]", "claude-opus-4-7[1m]", ""},
		// Another model, such as the auto-mode classifier, keeps its slug.
		{"claude-opus-4-7[1m]", "claude-haiku-4-5-20251001", "claude-haiku-4-5"},
		{"", "claude-opus-4-7[1m]", "claude-opus-4-7"},
	} {
		p := NewParser()
		p.SetModel(tc.session)
		result := p.accountingModelUsage(tc.model, provider.TokenUsage{OutputTokens: 5})
		if result.Model != tc.model || result.AccountingModel != tc.accounting {
			t.Fatalf("session %q, model %q: %+v, want accounting model %q", tc.session, tc.model, result, tc.accounting)
		}
	}
}

// TestUsageProgressUsesTheSessionsContextTier pins that a 1M session's live
// usage is named the way its final report will be, from the first message,
// and that settlement reconciles against that same name.
func TestUsageProgressUsesTheSessionsContextTier(t *testing.T) {
	p := NewParser()
	parse := func(line string) []provider.ProviderEvent {
		t.Helper()
		events, err := p.ParseLine(testThread, []byte(line))
		if err != nil {
			t.Fatal(err)
		}
		return events
	}
	parse(`{"type":"system","subtype":"init","session_id":"s1","model":"claude-opus-5-5[1m]","cwd":"/tmp","tools":[]}`)
	var models []string
	for _, e := range parse(`{"type":"stream_event","event":{"type":"message_start","message":{"id":"m1","model":"claude-opus-5-5","usage":{"input_tokens":10,"output_tokens":1}}}}`) {
		if e.Kind == provider.EventUsageProgress {
			for _, m := range e.UsageProgress.ModelUsage {
				models = append(models, m.Model)
			}
		}
	}
	if len(models) != 1 || models[0] != "claude-opus-5-5[1m]" {
		t.Fatalf("live usage models = %v, want [claude-opus-5-5[1m]]", models)
	}
	// A refusal fallback runs another model, which keeps its own name.
	models = nil
	for _, e := range parse(`{"type":"stream_event","event":{"type":"message_start","message":{"id":"m2","model":"claude-opus-4-8","usage":{"output_tokens":1}}}}`) {
		if e.Kind == provider.EventUsageProgress {
			for _, m := range e.UsageProgress.ModelUsage {
				models = append(models, m.Model)
			}
		}
	}
	if !slices.Equal(models, []string{"claude-opus-4-8", "claude-opus-5-5[1m]"}) {
		t.Fatalf("live usage models = %v, want the fallback under its own slug beside the session's", models)
	}
	final := requireWireTurnComplete(t, parse(`{"type":"result","subtype":"success","is_error":false,"total_cost_usd":0.1,`+
		`"modelUsage":{"claude-opus-5-5[1m]":{"inputTokens":10,"outputTokens":1,"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"costUSD":0.1}}}`))
	if len(final.ModelUsage) != 1 || final.ModelUsage[0].Model != "claude-opus-5-5[1m]" || final.ModelUsage[0].AccountingModel != "" {
		t.Fatalf("final model usage = %+v, want claude-opus-5-5[1m] settling its own live rows", final.ModelUsage)
	}
}
