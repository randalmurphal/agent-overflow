package engine

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/workflow/def"
)

// A phase re-entered by a loop whose new attempt withdraws an optional
// output takes the gate route for the output being absent: the attempt
// being completed supplies its phase's values alone, so the earlier
// attempt's value never reaches the gate.
func TestAReEnteredPhaseGatesOnItsOwnAttemptsOutputs(t *testing.T) {
	work := agentPhase("work", nil, []def.Route{
		{When: &def.Predicate{Exists: "work.note"}, To: "review"},
		{To: "finish"},
	})
	work.Outputs = map[string]def.Variable{"note": {Schema: def.JSONSchema{Type: "string"}, Optional: true}}
	review := agentPhase("review", nil, []def.Route{{Loop: "work", Max: def.LiteralBound(1)}, {To: "finish"}})
	finish := agentPhase("finish", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{},
		map[string]def.Workflow{"reentry": {ID: "reentry", Phases: []def.Phase{work, review, finish}}}, []string{"project"}, nil)
	item := testItem("item", "project", "reentry", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	for _, envelope := range []string{
		`{"status":"done","outputs":{"note":"first"}}`, // work 1: noted, so review
		`{"status":"done","outputs":{"ok":true}}`,      // review 1: loops back to work
		`{"status":"done","outputs":{"note":null}}`,    // work 2: the note withdrawn
	} {
		h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: json.RawMessage(envelope)})
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.runner.lastStartFor(t, item.ID).Key; got.PhaseID != "finish" {
		t.Fatalf("work's second attempt routed to %s attempt %d, want finish: its own note is absent", got.PhaseID, got.Attempt)
	}
}
