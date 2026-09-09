package claude

import (
	"fmt"
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
	for _, model := range []string{"claude-haiku-4-5-20251001", "claude-opus-4-7[1m]"} {
		result := accountingModelUsage(model, provider.TokenUsage{OutputTokens: 5})
		if result.Model != model || result.AccountingModel != provider.NormalizeModelSlug(string(provider.Claude), model) {
			t.Fatalf("model identity: %+v", result)
		}
	}
}
