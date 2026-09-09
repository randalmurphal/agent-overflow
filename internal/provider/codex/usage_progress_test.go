package codex

import (
	"agent-overflow/internal/provider"
	"testing"
)

func TestUsageProgressUsesResumeBaselineAndSettlesOnce(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{threadID: "t", usageAcct: newUsageAccounting(true), turnConfig: sessionTurnConfig{model: "gpt-5.2-codex"},
		onEvent: func(e provider.ProviderEvent) { events = append(events, e) }}
	s.foldNotificationOntoParent("thread/tokenUsage/updated", tokenUsageParams(wireTotals{total: 100, input: 80, cached: 60, output: 20}, 258400))
	if len(events) != 0 {
		t.Fatal("resume seed reported as new spend")
	}
	s.usageAcct.onTurnStart()
	s.foldNotificationOntoParent("thread/tokenUsage/updated", tokenUsageParams(wireTotals{total: 150, input: 110, cached: 70, output: 40}, 258400))
	if len(events) != 1 {
		t.Fatalf("progress events: %d", len(events))
	}
	p := events[0].UsageProgress
	if p.Scope == "" || p.ModelUsage[0].InputTokens != 20 || p.ModelUsage[0].CacheReadInputTokens != 10 || p.ModelUsage[0].OutputTokens != 20 {
		t.Fatalf("delta: %+v", p)
	}
	meta := &provider.WireTurnCompleteMeta{Aborted: true}
	s.attachTurnUsage(meta)
	if meta.UsageScope != p.Scope || meta.Usage.OutputTokens != 20 {
		t.Fatalf("interrupt settlement: %+v", meta)
	}
	s.emitUsageProgress()
	if len(events) != 1 {
		t.Fatal("closed turn republished usage")
	}
	s.usageAcct.onTurnStart()
	s.foldNotificationOntoParent("thread/tokenUsage/updated", tokenUsageParams(wireTotals{total: 160, input: 110, cached: 70, output: 50}, 258400))
	p2 := events[1].UsageProgress
	if p2.Scope != p.Scope || p2.Segment == p.Segment || p2.ModelUsage[0].OutputTokens != 10 {
		t.Fatalf("next turn: %+v", p2)
	}
}

func TestUsageProgressUnseededResumeAndDestroyedCounters(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{threadID: "t", usageAcct: newUsageAccounting(true), turnConfig: sessionTurnConfig{model: "gpt-5.2-codex"},
		onEvent: func(e provider.ProviderEvent) { events = append(events, e) }}
	s.usageAcct.onTurnStart()
	s.foldNotificationOntoParent("thread/tokenUsage/updated", tokenUsageParams(wireTotals{total: 100, input: 80, output: 20}, 258400))
	if len(events) != 0 {
		t.Fatal("unseeded history was reported as new")
	}
	s.attachTurnUsage(&provider.WireTurnCompleteMeta{})
	s.usageAcct.onTurnStart()
	s.foldNotificationOntoParent("thread/tokenUsage/updated", tokenUsageParams(wireTotals{total: 110, input: 80, output: 30}, 258400))
	oldScope := events[0].UsageProgress.Scope
	s.foldNotificationOntoParent("thread/tokenUsage/updated", tokenUsageParams(wireTotals{total: 258400}, 258400))
	if len(events) != 1 {
		t.Fatal("context limit sentinel is not spending")
	}
	meta := &provider.WireTurnCompleteMeta{}
	s.attachTurnUsage(meta)
	if meta.UsageScope == oldScope || meta.Usage != nil {
		t.Fatalf("destroyed baseline reused: %+v", meta)
	}
}
