package triage

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
)

// A thread that delegates to a native subagent tallies its own spend: the
// captured turn (subagent_usage_inclusion_20260703.ndjson), every line of it
// through the parser and the router, tallies the parent-only 23,369 while
// the billing ledger keeps the subagent-inclusive 36,709.
func TestADelegatingTurnTalliesItsOwnSpendAndBillsAll(t *testing.T) {
	r, s, _ := newTestRouter(t)
	createTestThread(t, s, "t1")
	p := claude.NewParser()
	defer p.Close()
	data, err := os.ReadFile(filepath.Join("..", "provider", "wirecaptures", "claude", "subagent_usage_inclusion_20260703.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Handle(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		events, err := p.ParseLine("t1", line)
		if err != nil {
			t.Fatalf("parse %.80s: %v", line, err)
		}
		for _, event := range events {
			if err := r.Handle(event); err != nil {
				t.Fatalf("handle %s: %v", event.Kind, err)
			}
		}
	}
	tallies, err := s.ThreadTallies([]string{"t1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := tallies["t1"].Tokens; got != 23369 {
		t.Fatalf("tallied %d tokens; want the thread's own 23,369", got)
	}
	billed, err := s.QueryUsage(store.UsageQuery{ThreadID: "t1"})
	if err != nil || len(billed) != 1 {
		t.Fatalf("ledger %+v, %v", billed, err)
	}
	if spend := billed[0].InputTokens + billed[0].CacheCreationInputTokens + billed[0].OutputTokens; spend != 36709 || billed[0].PendingRows != 0 {
		t.Fatalf("ledger spends %d (%+v); want the subagent-inclusive 36,709, settled", spend, billed[0])
	}
}
