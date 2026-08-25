package codex

import (
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestSessionBuffersPlanDeltaUntilCompletion(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	lines := []string{
		`{"jsonrpc":"2.0","method":"item/plan/delta","params":{"turnId":"turn-plan","itemId":"plan-1","delta":"# Plan\n\n"}}`,
		`{"jsonrpc":"2.0","method":"item/plan/delta","params":{"turnId":"turn-plan","itemId":"plan-1","delta":"- first\n- second"}}`,
		`{"jsonrpc":"2.0","method":"item/completed","params":{"turnId":"turn-plan","item":{"id":"plan-1","type":"plan"}}}`,
	}
	for _, line := range lines {
		if err := s.proc.WriteLine([]byte(line)); err != nil {
			t.Fatalf("write line: %v", err)
		}
	}

	var got provider.ProviderEvent
	select {
	case got = <-eventCh:
	case <-time.After(2 * time.Second):
		t.Fatal("no proposed plan event emitted")
	}
	if got.Kind != provider.EventProposedPlan {
		t.Fatalf("kind: got %q, want EventProposedPlan", got.Kind)
	}
	if got.ItemID != "plan-1" {
		t.Fatalf("itemID: got %q, want plan-1", got.ItemID)
	}
	if got.Content != "# Plan\n\n- first\n- second" {
		t.Fatalf("content: got %q", got.Content)
	}
}

func TestSessionPrefersCompletedPlanContentOverBufferedDelta(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	lines := []string{
		`{"jsonrpc":"2.0","method":"item/plan/delta","params":{"turnId":"turn-plan","itemId":"plan-1","delta":"# Draft plan"}}`,
		`{"jsonrpc":"2.0","method":"item/completed","params":{"turnId":"turn-plan","item":{"id":"plan-1","type":"plan","text":"# Final plan"}}}`,
	}
	for _, line := range lines {
		if err := s.proc.WriteLine([]byte(line)); err != nil {
			t.Fatalf("write line: %v", err)
		}
	}

	var got provider.ProviderEvent
	select {
	case got = <-eventCh:
	case <-time.After(2 * time.Second):
		t.Fatal("no proposed plan event emitted")
	}
	if got.Content != "# Final plan" {
		t.Fatalf("content = %q, want completed item text", got.Content)
	}
}
