package codex

import (
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestSessionBuffersPlanDeltaUntilCompletion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		lines []string
		want  string
	}{
		{"deltas joined on completion", []string{
			`{"jsonrpc":"2.0","method":"item/plan/delta","params":{"turnId":"turn-plan","itemId":"plan-1","delta":"# Plan\n\n"}}`,
			`{"jsonrpc":"2.0","method":"item/plan/delta","params":{"turnId":"turn-plan","itemId":"plan-1","delta":"- first\n- second"}}`,
			`{"jsonrpc":"2.0","method":"item/completed","params":{"turnId":"turn-plan","item":{"id":"plan-1","type":"plan"}}}`,
		}, "# Plan\n\n- first\n- second"},
		{"completed text wins over buffered delta", []string{
			`{"jsonrpc":"2.0","method":"item/plan/delta","params":{"turnId":"turn-plan","itemId":"plan-1","delta":"# Draft plan"}}`,
			`{"jsonrpc":"2.0","method":"item/completed","params":{"turnId":"turn-plan","item":{"id":"plan-1","type":"plan","text":"# Final plan"}}}`,
		}, "# Final plan"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, eventCh := newTestCodexSession(t)
			for _, line := range tc.lines {
				feed(t, s, []byte(line))
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
			if got.Content != tc.want {
				t.Fatalf("content = %q, want %q", got.Content, tc.want)
			}
		})
	}
}
