package codex

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

// TestClassifySafetyBuffering_ShowEmitsUserFacingRow covers the state the
// notification exists to make visible: OpenAI held the model's response for
// review, which is why the turn went quiet.
//
// The summary carries the reasons because triage persists only `kind` and
// `title` for notification rows — anything left in meta never reaches the
// transcript.
func TestClassifySafetyBuffering_ShowEmitsUserFacingRow(t *testing.T) {
	t.Parallel()
	params := json.RawMessage(`{
		"threadId":"th","turnId":"turn-1","model":"gpt-5.6-sol",
		"useCases":["cyber"],"reasons":["policy_review","high_risk"],
		"showBufferingUi":true,"fasterModel":"gpt-5.6-luna"}`)

	events, handled := classifyNotification("th", "model/safetyBuffering/updated", params)
	if !handled {
		t.Fatal("model/safetyBuffering/updated was not claimed by any classifier")
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(events), events)
	}
	evt := events[0]
	if evt.Kind != provider.EventNotification {
		t.Errorf("kind = %q, want %q", evt.Kind, provider.EventNotification)
	}
	if evt.TurnID != "turn-1" {
		t.Errorf("turnId = %q, want turn-1", evt.TurnID)
	}
	if !strings.HasPrefix(evt.Content, "OpenAI held this response for review") {
		t.Errorf("summary does not explain the hold: %q", evt.Content)
	}
	if !strings.Contains(evt.Content, "policy_review") || !strings.Contains(evt.Content, "high_risk") {
		t.Errorf("summary dropped the server-supplied reasons: %q", evt.Content)
	}

	var meta struct {
		Kind        string   `json:"kind"`
		Reasons     []string `json:"reasons"`
		UseCases    []string `json:"useCases"`
		FasterModel string   `json:"fasterModel"`
	}
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("decode meta: %v (raw %s)", err, string(evt.Meta))
	}
	if meta.Kind != "safety_buffering" {
		t.Errorf("meta.kind = %q, want safety_buffering", meta.Kind)
	}
	// The structured wire fields stay on the event so a later live-banner
	// UI needs no parser change.
	if len(meta.Reasons) != 2 || len(meta.UseCases) != 1 || meta.FasterModel != "gpt-5.6-luna" {
		t.Errorf("meta lost wire fields: %+v", meta)
	}
}

// TestClassifySafetyBuffering_ClearIsHandledButSilent pins the other value.
// showBufferingUi=false says nothing is held, so it gets no row. It still
// has to count as handled, or the opt-out derivation would unsubscribe from
// the method and the show=true signal would stop arriving too.
func TestClassifySafetyBuffering_ClearIsHandledButSilent(t *testing.T) {
	t.Parallel()
	params := json.RawMessage(`{"threadId":"th","turnId":"turn-1","model":"gpt","reasons":[],"showBufferingUi":false}`)
	events, handled := classifyNotification("th", "model/safetyBuffering/updated", params)
	if !handled {
		t.Fatal("the clear edge must still be claimed")
	}
	if len(events) != 0 {
		t.Errorf("the clear edge must not produce a row, got %+v", events)
	}
}

// TestClassifySafetyBuffering_NoReasonsKeepsSummaryClean guards the copy
// when the server sends the hold with no explanation.
func TestClassifySafetyBuffering_NoReasonsKeepsSummaryClean(t *testing.T) {
	t.Parallel()
	params := json.RawMessage(`{"threadId":"th","turnId":"turn-1","showBufferingUi":true}`)
	events, _ := classifyNotification("th", "model/safetyBuffering/updated", params)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if strings.Contains(events[0].Content, "(") {
		t.Errorf("empty reasons left an empty parenthetical: %q", events[0].Content)
	}
}
