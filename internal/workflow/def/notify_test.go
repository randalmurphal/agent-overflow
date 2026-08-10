package def

import (
	"encoding/json"
	"strings"
	"testing"
)

// The `notify:` route decoration (K1): a gate route that wakes the run's bound
// thread without parking it.

func TestParseAcceptsNotifyOnARouteAndThePublishedSchemaAgrees(t *testing.T) {
	workflow, err := Parse(strings.NewReader(`
id: notify-flow
name: Notify
phases:
  - id: wave
    driver: agent
    provider: codex
    model: test-model
    prompt: wave.md
    gate:
      routes:
        - loop: wave
          max: 3
          notify: true
        - to: done
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	routes := workflow.Phases[0].Gate.Routes
	if !routes[0].Notify {
		t.Fatal("notify: true did not reach the loop route")
	}
	if routes[1].Notify {
		t.Fatal("an undecorated route came back decorated")
	}

	var published map[string]any
	if err := json.Unmarshal(AuthoringSchema(), &published); err != nil {
		t.Fatalf("published schema is invalid JSON: %v", err)
	}
	route := published["$defs"].(map[string]any)["route"].(map[string]any)["properties"].(map[string]any)
	if _, ok := route["notify"]; !ok {
		t.Fatal("published schema does not admit notify, so an authored route would be refused by the editor")
	}
}

func TestNotifyOnAHumanOrParkRouteIsAFinding(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		route Route
	}{
		{"human", Route{Human: &HumanRoute{Approve: "implement", Reject: &LoopTarget{Loop: "plan", Max: LiteralBound(1)}}, Notify: true}},
		{"park", Route{Park: "needs-a-human", Notify: true}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			resolved := validResolved(t)
			resolved.Workflow.Phases[0].Gate.Routes = []Route{testCase.route, {To: "implement"}}
			result := Validate(resolved, validBindings(), nil)
			if result.Valid() {
				t.Fatal("notify on a parking route validated; it promises a second wake for the one event")
			}
			if !hasFinding(result.Findings, "gate.notify", `phase "plan" route 0`) {
				t.Fatalf("missing gate.notify finding:\n%s", formatFindings(result.Findings))
			}
		})
	}
}

func TestNotifyOnATerminalRouteReportsRatherThanRefuses(t *testing.T) {
	resolved := validResolved(t)
	// The review phase's `to: done` route — the run rests there, and a resting
	// run already wakes its bound thread.
	resolved.Workflow.Phases[2].Gate.Routes[0].Notify = true
	result := Validate(resolved, validBindings(), nil)
	if !result.Valid() {
		t.Fatalf("notify on a terminal route made the definition invalid:\n%s", formatFindings(result.Findings))
	}
	if !hasFinding(result.Reports, "gate.notify-terminal", `phase "review" route 0`) {
		t.Fatalf("missing gate.notify-terminal report:\n%s", formatFindings(result.Reports))
	}
	if !strings.Contains(formatFindings(result.Reports), "resting wake") {
		t.Fatalf("report does not say why it is inert:\n%s", formatFindings(result.Reports))
	}
}

func TestNotifyReachesOnlyTheDecisionsARunContinuesThrough(t *testing.T) {
	// Every kind is exercised through one gate evaluation each, because the
	// decision is what the engine reads to decide whether a progress wake is
	// owed — and a frozen snapshot is decoded, never re-validated, so the
	// human/park cases really do arrive here decorated.
	for _, testCase := range []struct {
		name       string
		route      Route
		wantKind   DecisionKind
		wantNotify bool
	}{
		{"advance", Route{To: "next", Notify: true}, DecisionAdvance, true},
		{"loop", Route{Loop: "wave", Max: LiteralBound(3), Notify: true}, DecisionLoop, true},
		{"done", Route{To: "done", Notify: true}, DecisionDone, false},
		{"failed", Route{To: "failed", Notify: true}, DecisionFailed, false},
		{"human", Route{Human: &HumanRoute{Approve: "next"}, Notify: true}, DecisionHuman, false},
		{"park", Route{Park: "label", Notify: true}, DecisionPark, false},
		{"undecorated advance", Route{To: "next"}, DecisionAdvance, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			decision, trace, err := EvaluateGate(
				Phase{ID: "wave", Gate: Gate{Routes: []Route{testCase.route}}}, map[string]any{}, map[string]int{})
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			if decision.Kind != testCase.wantKind {
				t.Fatalf("decision kind = %q, want %q", decision.Kind, testCase.wantKind)
			}
			if decision.Notify != testCase.wantNotify {
				t.Fatalf("decision notify = %v, want %v", decision.Notify, testCase.wantNotify)
			}
			// The trace is the persisted record of what the gate decided, so the
			// flag has to survive into it — that is where a reader afterwards
			// learns a progress wake was owed.
			if trace.Decision.Notify != testCase.wantNotify {
				t.Fatalf("gate trace notify = %v, want %v", trace.Decision.Notify, testCase.wantNotify)
			}
			encoded, err := json.Marshal(trace)
			if err != nil {
				t.Fatal(err)
			}
			var decoded GateTrace
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.Decision.Notify != testCase.wantNotify {
				t.Fatalf("notify did not survive the gate trace round trip: %s", encoded)
			}
		})
	}
}
