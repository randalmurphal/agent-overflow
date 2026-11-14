package workflowhost

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

// A Claude init starts an attempt's turn unless AO's own command opened it.
func TestWorkflowProviderTurnStartedSkipsInternalCommandInit(t *testing.T) {
	initWith := func(opener provider.TurnOpener) provider.ProviderEvent {
		meta, err := json.Marshal(provider.SessionInfo{TurnOpener: opener})
		if err != nil {
			t.Fatal(err)
		}
		return provider.ProviderEvent{Kind: provider.EventInit, Meta: meta}
	}
	claude := string(provider.Claude)
	cases := []struct {
		name  string
		event provider.ProviderEvent
		want  bool
	}{
		{"command opener", initWith(provider.TurnOpenerCommand), true},
		{"no opener", initWith(""), true},
		{"no meta", provider.ProviderEvent{Kind: provider.EventInit}, true},
		{"internal command", initWith(provider.TurnOpenerInternalCommand), false},
	}
	for _, tc := range cases {
		if got := workflowProviderTurnStarted(claude, tc.event); got != tc.want {
			t.Errorf("%s: workflowProviderTurnStarted = %v, want %v", tc.name, got, tc.want)
		}
	}
	if workflowProviderTurnStarted(string(provider.Codex), initWith(provider.TurnOpenerCommand)) {
		t.Error("a Codex init started a workflow turn")
	}
}
