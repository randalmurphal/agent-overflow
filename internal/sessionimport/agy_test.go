package sessionimport

import (
	"testing"

	"agent-overflow/internal/provider"
)

func TestAgyEventsToIRChainedFingerprintDetectsDivergenceOnTurnComplete(t *testing.T) {
	t.Parallel()
	convA := []provider.ProviderEvent{
		{Kind: provider.EventTurnStart},
		{Kind: provider.EventUserText, Role: "user", Content: "question alpha"},
		{Kind: provider.EventTextDelta, Role: "assistant", Content: "answer alpha"},
		{Kind: provider.EventTurnComplete},
	}
	convB := []provider.ProviderEvent{
		{Kind: provider.EventTurnStart},
		{Kind: provider.EventUserText, Role: "user", Content: "question beta"},
		{Kind: provider.EventTextDelta, Role: "assistant", Content: "answer beta"},
		{Kind: provider.EventTurnComplete},
	}
	irA := agyEventsToIR("sess-1", convA, 1700000000000)
	irB := agyEventsToIR("sess-1", convB, 1700000000000)
	lastA := irA[len(irA)-1].SourceUUID
	lastB := irB[len(irB)-1].SourceUUID
	if lastA == lastB {
		t.Fatalf("divergent conversations produced identical trailing turn_complete cursor %q", lastA)
	}
}
