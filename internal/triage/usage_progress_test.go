package triage

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
)

func TestUsageProgressSurvivesInterruptThenReconcilesLateResult(t *testing.T) {
	r, s, emissions := newTestRouter(t)
	createTestThread(t, s, "t1")
	handle := func(e provider.ProviderEvent) {
		t.Helper()
		e.ThreadID = "t1"
		e.Timestamp = time.Now()
		if err := r.Handle(e); err != nil {
			t.Fatal(err)
		}
	}
	handle(provider.ProviderEvent{Kind: provider.EventTurnStart})
	progress := func(segment string, n int) {
		handle(provider.ProviderEvent{Kind: provider.EventUsageProgress, UsageProgress: &provider.UsageProgress{Scope: "process", Segment: segment,
			ModelUsage: []provider.ModelTokenUsage{{Model: "claude-haiku-4-5", TokenUsage: provider.TokenUsage{OutputTokens: n}}}}})
	}
	read := func(output, pending int64) {
		t.Helper()
		b, err := s.QueryUsage(store.UsageQuery{ThreadID: "t1"})
		if err != nil || len(b) != 1 || b[0].OutputTokens != output || b[0].PendingRows != pending {
			t.Fatalf("totals: %+v %v", b, err)
		}
	}
	progress("0", 5)
	progress("0", 8)
	read(8, 1)
	items, err := s.ListItems("t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("usage altered conversation history: %+v", items)
	}
	handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: &provider.WireTurnCompleteMeta{UsageScope: "process", Aborted: true}})
	read(8, 1)
	handle(provider.ProviderEvent{Kind: provider.EventTurnStart, TurnIndex: 1})
	progress("1", 12)
	read(20, 2)
	// Soft round-close precedes the accounting result in Claude.
	handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: &provider.SoftRoundCloseMeta{StopReason: "end_turn"}})
	final := usageTurnCompleteMeta(provider.ModelTokenUsage{Model: "claude-haiku-4-5", TokenUsage: provider.TokenUsage{OutputTokens: 25, TotalCostUSD: 0.1}})
	final.UsageScope = "process"
	handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: final})
	read(25, 0)
	usageEvents := filterEmissions(emissions.snapshot(), "provider:usage")
	if len(usageEvents) == 0 || usageEvents[len(usageEvents)-1].data.(provider.UsageEvent).Action != "progress" {
		t.Fatal("settled accounting did not invalidate live totals")
	}
}

func TestUsageProgressFailureIsVisible(t *testing.T) {
	r, s, emissions := newTestRouter(t)
	createTestThread(t, s, "t1")
	err := r.Handle(provider.ProviderEvent{Kind: provider.EventUsageProgress, ThreadID: "t1"})
	if err == nil {
		t.Fatal("missing payload accepted")
	}
	events := filterEmissions(emissions.snapshot(), "provider:usage")
	if len(events) != 1 || events[0].data.(provider.UsageEvent).Error == "" {
		t.Fatal("usage failure did not reach user-facing state")
	}
}

func TestUsageThrottleTrailingTimerAndIndependentLanes(t *testing.T) {
	r, _, emissions := newTestRouter(t)
	for _, action := range []string{"usage", "progress"} {
		r.throttledEmitUsage("t", provider.UsageEvent{ThreadID: "t", Action: action, UsedTokens: 10})
		r.throttledEmitUsage("t", provider.UsageEvent{ThreadID: "t", Action: action, UsedTokens: 20})
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(emissions.snapshot()) < 4 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	events := emissions.snapshot()
	if len(events) != 4 {
		t.Fatalf("quiet provider lost trailing updates: %+v", events)
	}
	last := map[string]int{}
	for _, e := range events {
		u := e.data.(provider.UsageEvent)
		last[u.Action] = u.UsedTokens
	}
	if last["usage"] != 20 || last["progress"] != 20 {
		t.Fatalf("lanes overwrote each other: %+v", last)
	}
}

func TestUsageThrottleCleanupCancelsBothTimers(t *testing.T) {
	r, s, emissions := newTestRouter(t)
	createTestThread(t, s, "t1")
	for _, action := range []string{"usage", "progress"} {
		r.throttledEmitUsage("t1", provider.UsageEvent{ThreadID: "t1", Action: action, UsedTokens: 10})
		r.throttledEmitUsage("t1", provider.UsageEvent{ThreadID: "t1", Action: action, UsedTokens: 20})
	}
	r.CleanupThread("t1")
	count := len(filterEmissions(emissions.snapshot(), "provider:usage"))
	if count != 4 {
		t.Fatalf("cleanup failed to flush both lanes: %d", count)
	}
	time.Sleep(usageEmitMinInterval + 20*time.Millisecond)
	if after := len(filterEmissions(emissions.snapshot(), "provider:usage")); after != count {
		t.Fatalf("timer emitted after cleanup: %d -> %d", count, after)
	}
}

// These captures use dated API message models and undated CLI accounting keys.
// Feed only their accounting envelopes, stripping content and machine metadata.
func TestClaudeCapturedUsageReconcilesModelSpellings(t *testing.T) {
	for _, fixture := range []string{"multiturn_cost_cumulative_20260703.ndjson", "subagent_usage_inclusion_20260703.ndjson"} {
		t.Run(fixture, func(t *testing.T) {
			r, s, _ := newTestRouter(t)
			createTestThread(t, s, "t1")
			p := claude.NewParser()
			defer p.Close()
			data, err := os.ReadFile(filepath.Join("..", "..", "docs", "references", "fixtures", "claude", fixture))
			if err != nil {
				t.Fatal(err)
			}
			turn := 0
			expectedOutput := 0
			sawProgress := false
			if err := r.Handle(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: turn}); err != nil {
				t.Fatal(err)
			}
			for _, line := range bytes.Split(data, []byte("\n")) {
				if len(line) == 0 {
					continue
				}
				var frame map[string]json.RawMessage
				if err := json.Unmarshal(line, &frame); err != nil {
					t.Fatal(err)
				}
				var kind string
				if err := json.Unmarshal(frame["type"], &kind); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "assistant":
					var parent string
					if raw := frame["parent_tool_use_id"]; len(raw) > 0 {
						if err := json.Unmarshal(raw, &parent); err != nil {
							t.Fatal(err)
						}
					}
					if parent != "" {
						continue
					}
					var message map[string]json.RawMessage
					if err := json.Unmarshal(frame["message"], &message); err != nil {
						t.Fatal(err)
					}
					message["content"] = json.RawMessage(`[]`)
					encoded, err := json.Marshal(message)
					if err != nil {
						t.Fatal(err)
					}
					frame = map[string]json.RawMessage{"type": frame["type"], "message": encoded}
				case "result":
					var models map[string]struct {
						OutputTokens int `json:"outputTokens"`
					}
					if err := json.Unmarshal(frame["modelUsage"], &models); err != nil {
						t.Fatal(err)
					}
					expectedOutput = 0
					for _, model := range models {
						expectedOutput += model.OutputTokens
					}
				default:
					continue
				}
				encoded, err := json.Marshal(frame)
				if err != nil {
					t.Fatal(err)
				}
				events, err := p.ParseLine("t1", encoded)
				if err != nil {
					t.Fatal(err)
				}
				for _, event := range events {
					if event.Kind != provider.EventUsageProgress && event.Kind != provider.EventTurnComplete {
						continue
					}
					if event.Kind == provider.EventUsageProgress {
						sawProgress = true
					}
					if err := r.Handle(event); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "result" {
					b, err := s.QueryUsage(store.UsageQuery{ThreadID: "t1"})
					if err != nil || len(b) != 1 || b[0].PendingRows != 0 || b[0].OutputTokens != int64(expectedOutput) {
						t.Fatalf("capture settlement: %+v expected=%d err=%v", b, expectedOutput, err)
					}
					turn++
					if err := r.Handle(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: turn}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if !sawProgress {
				t.Fatal("capture produced no live usage")
			}
		})
	}
}
