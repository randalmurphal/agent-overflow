package triage

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

func TestTurnCompleteMetaFromEventRequiresTypedPayload(t *testing.T) {
	legacyMeta, err := json.Marshal(map[string]any{
		"stop_reason":          "end_turn",
		"assistant_message_id": "legacy-msg",
	})
	if err != nil {
		t.Fatalf("marshal legacy meta: %v", err)
	}

	_, err = turnCompleteMetaFromEvent(provider.ProviderEvent{
		Kind: provider.EventTurnComplete,
		Meta: legacyMeta,
	})
	if err == nil {
		t.Fatal("expected legacy ProviderEvent.Meta payload to be rejected")
	}
	if !strings.Contains(err.Error(), "missing typed payload") &&
		!strings.Contains(err.Error(), "not supported") {
		t.Fatalf("error = %q, want typed-payload rejection", err.Error())
	}

	meta, err := turnCompleteMetaFromEvent(provider.ProviderEvent{
		Kind: provider.EventTurnComplete,
		TurnComplete: &provider.WireTurnCompleteMeta{
			StopReason:         "end_turn",
			AssistantMessageID: "typed-msg",
		},
		Meta: legacyMeta,
	})
	if err != nil {
		t.Fatalf("typed payload: %v", err)
	}
	if meta.AssistantMessageID != "typed-msg" {
		t.Fatalf("AssistantMessageID = %q, want typed-msg", meta.AssistantMessageID)
	}
}
