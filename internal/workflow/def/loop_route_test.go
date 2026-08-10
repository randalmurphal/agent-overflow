package def

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The two per-round knobs a loop route carries: `session:` (continue the target
// phase's own session) and `prompt:` (render a different body for the one
// attempt the route creates).

// writableValidFixture is the valid fixture copied into a writable directory, so
// a test can rewrite its prompts or add the sibling files a route override
// names. Shared by every test that needs the fixture's graph plus a prompt of
// its own; the fixture's last route loops `review` → `implement`, which is the
// edge the loop-knob tests below are about.
func writableValidFixture(t *testing.T) ResolvedWorkflow {
	t.Helper()
	dir := t.TempDir()
	entries, err := os.ReadDir("testdata/valid")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		body, err := os.ReadFile(filepath.Join("testdata/valid", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, entry.Name()), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "workflow.yaml")
	workflow, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return ResolvedWorkflow{Workflow: workflow, Scope: ScopeShared, Path: path}
}

// loopRoute is the fixture's `review` → `implement` edge.
func loopRouteOf(resolved *ResolvedWorkflow) *Route {
	return &resolved.Workflow.Phases[2].Gate.Routes[1]
}

func writeSibling(t *testing.T, resolved ResolvedWorkflow, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(filepath.Dir(resolved.Path), name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestParseAcceptsLoopRouteKnobsAndThePublishedSchemaAgrees(t *testing.T) {
	workflow, err := Parse(strings.NewReader(`
id: loop-flow
name: Loop
phases:
  - id: implement
    driver: agent
    provider: codex
    model: test-model
    prompt: implement.md
    gate:
      routes:
        - to: review
  - id: review
    driver: agent
    provider: codex
    model: test-model
    prompt: review.md
    gate:
      routes:
        - loop: implement
          max: 3
          session: continue
          prompt: implement-fix.md
        - to: done
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	route := workflow.Phases[1].Gate.Routes[0]
	if route.Session != SessionContinue || route.Prompt != "implement-fix.md" {
		t.Fatalf("loop route knobs = %+v", route)
	}

	var published map[string]any
	if err := json.Unmarshal(AuthoringSchema(), &published); err != nil {
		t.Fatalf("published schema is invalid JSON: %v", err)
	}
	properties := published["$defs"].(map[string]any)["route"].(map[string]any)["properties"].(map[string]any)
	for _, name := range []string{"session", "prompt"} {
		if _, ok := properties[name]; !ok {
			t.Fatalf("published schema does not admit %q, so an authored route would be refused by the editor", name)
		}
	}
}

// The default is `fresh`, which is what every loop did before the knob existed —
// including a route that says nothing and one that says so explicitly.
func TestEffectiveSessionDefaultsToFresh(t *testing.T) {
	for name, route := range map[string]Route{
		"unset":    {Loop: "implement"},
		"explicit": {Loop: "implement", Session: SessionFresh},
	} {
		if got := route.EffectiveSession(); got != SessionFresh {
			t.Fatalf("%s route session = %q, want fresh", name, got)
		}
	}
	if (Route{Loop: "implement", Session: SessionContinue}).EffectiveSession() != SessionContinue {
		t.Fatal("continue did not survive EffectiveSession")
	}
}

// Both knobs are refused outside a loop route. A forward, park, or human route
// enters a phase from OUTSIDE its cycle, so there is no previous round to
// continue and no per-round question to narrow — and a knob that silently did
// nothing is one the author only discovers by watching a run behave as if it
// were absent.
func TestLoopRouteKnobsAreRefusedOnEveryOtherRouteKind(t *testing.T) {
	for name, route := range map[string]Route{
		"forward": {To: "review"},
		"park":    {Park: "needs-a-human"},
		"human":   {Human: &HumanRoute{Approve: "review", Reject: &LoopTarget{Loop: "plan", Max: LiteralBound(1)}}},
	} {
		t.Run(name, func(t *testing.T) {
			resolved := writableValidFixture(t)
			writeSibling(t, resolved, "narrow.md", "narrower body")
			decorated := route
			decorated.Session = SessionContinue
			decorated.Prompt = "narrow.md"
			resolved.Workflow.Phases[0].Gate.Routes = []Route{decorated, {To: "implement"}}
			result := Validate(resolved, validBindings(), nil)
			if result.Valid() {
				t.Fatal("loop knobs validated on a non-loop route")
			}
			for _, code := range []string{"gate.session", "gate.prompt"} {
				if !hasFinding(result.Findings, code, `phase "plan" route 0`) {
					t.Fatalf("missing %s finding:\n%s", code, formatFindings(result.Findings))
				}
			}
		})
	}
}

// A loop target that runs no session of its own has nothing for either knob to
// name, and the finding says which of the three shapes it is rather than leaving
// the author to work it out.
func TestLoopRouteKnobsRequireATargetThatRunsItsOwnSession(t *testing.T) {
	for name, mutate := range map[string]func(*ResolvedWorkflow){
		"tool": func(r *ResolvedWorkflow) {
			r.Workflow.Phases[1].Driver = DriverTool
			r.Workflow.Phases[1].Check = "test"
			r.Workflow.Phases[1].Provider, r.Workflow.Phases[1].Model, r.Workflow.Phases[1].Prompt = "", "", ""
		},
		"fan-out": func(r *ResolvedWorkflow) {
			r.Workflow.Phases[1].Shape = ShapeFanOut
			r.Workflow.Phases[1].Driver, r.Workflow.Phases[1].Provider = "", ""
			r.Workflow.Phases[1].Model, r.Workflow.Phases[1].Prompt = "", ""
			r.Workflow.Phases[1].FanOut = []Unit{{ID: "lens", Provider: "codex", Model: "test-model", Prompt: "implement.md"}}
			r.Workflow.Phases[1].Join = &Unit{ID: "combine", Provider: "codex", Model: "test-model", Prompt: "implement.md"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			resolved := writableValidFixture(t)
			writeSibling(t, resolved, "narrow.md", "narrower body")
			mutate(&resolved)
			route := loopRouteOf(&resolved)
			route.Session = SessionContinue
			route.Prompt = "narrow.md"
			result := Validate(resolved, validBindings(), nil)
			if result.Valid() {
				t.Fatal("loop knobs validated against a target that runs no session")
			}
			for _, code := range []string{"gate.session", "gate.prompt"} {
				if !hasFinding(result.Findings, code, `phase "review" route 1`) {
					t.Fatalf("missing %s finding:\n%s", code, formatFindings(result.Findings))
				}
			}
		})
	}
}

func TestLoopRouteSessionValueIsClosed(t *testing.T) {
	resolved := writableValidFixture(t)
	loopRouteOf(&resolved).Session = "warm"
	result := Validate(resolved, validBindings(), nil)
	if result.Valid() {
		t.Fatal("an unknown session mode validated")
	}
	if !hasFinding(result.Findings, "gate.session", `phase "review" route 1`) {
		t.Fatalf("missing gate.session finding:\n%s", formatFindings(result.Findings))
	}
}

// A route override is a prompt file like any other: it is resolved and read at
// validation time, and a missing one is a finding rather than a run that
// discovers it at the fourth lap.
func TestLoopRoutePromptFileIsResolvedAtValidation(t *testing.T) {
	resolved := writableValidFixture(t)
	loopRouteOf(&resolved).Prompt = "missing-fix.md"
	result := Validate(resolved, validBindings(), nil)
	if result.Valid() {
		t.Fatal("a route naming a missing prompt file validated")
	}
	if !hasFinding(result.Findings, "prompt.file", `phase "review" route 1 prompt file "missing-fix.md"`) {
		t.Fatalf("missing prompt.file finding:\n%s", formatFindings(result.Findings))
	}
}

// The override renders in the TARGET phase's context, so its template is checked
// against that phase's inputs: `implement` declares `plan.approach`, and `review`
// does not.
func TestLoopRoutePromptTemplateIsCheckedAgainstTheTargetPhase(t *testing.T) {
	resolved := writableValidFixture(t)
	writeSibling(t, resolved, "fix.md", "revisit {{plan.approach}}")
	loopRouteOf(&resolved).Prompt = "fix.md"
	if result := Validate(resolved, validBindings(), nil); !result.Valid() {
		t.Fatalf("a target-phase reference was refused:\n%s", formatFindings(result.Findings))
	}

	writeSibling(t, resolved, "fix.md", "revisit {{review.notes}}")
	result := Validate(resolved, validBindings(), nil)
	if result.Valid() {
		t.Fatal("a reference the target phase cannot resolve validated")
	}
	if !hasFinding(result.Findings, "prompt.template", `phase "review" route 1 prompt file "fix.md"`) {
		t.Fatalf("missing prompt.template finding:\n%s", formatFindings(result.Findings))
	}
}

// An unresolvable loop target has NO declaration set, and checking the override
// against an empty one is not the same as not checking it: every reference in
// the file comes back undeclared, one finding per token, and the single real
// finding — `gate.target`, naming the phase the route cannot reach — is buried
// under them. The file itself is still resolved and read.
func TestLoopRoutePromptTemplateIsNotCheckedAgainstAnUnknownTarget(t *testing.T) {
	resolved := writableValidFixture(t)
	writeSibling(t, resolved, "fix.md", "revisit {{plan.approach}} for {{goal}} and {{review.notes}}")
	route := loopRouteOf(&resolved)
	route.Loop = "implememt" // the typo the gate.target finding is about
	route.Prompt = "fix.md"

	result := Validate(resolved, validBindings(), nil)
	if result.Valid() {
		t.Fatal("a route looping to a phase that does not exist validated")
	}
	if !hasFinding(result.Findings, "gate.target", `phase "review" route 1`) {
		t.Fatalf("missing the gate.target finding this case is about:\n%s", formatFindings(result.Findings))
	}
	for _, found := range result.Findings {
		if found.Code == "prompt.template" {
			t.Fatalf("the override's template was checked against no declarations at all:\n%s",
				formatFindings(result.Findings))
		}
	}

	// The path half still applies: an override naming a file that is not there is
	// a finding whether or not its target resolves.
	route.Prompt = "missing-fix.md"
	missing := Validate(resolved, validBindings(), nil)
	if !hasFinding(missing.Findings, "prompt.file", `phase "review" route 1 prompt file "missing-fix.md"`) {
		t.Fatalf("an unresolvable target also skipped the file check:\n%s", formatFindings(missing.Findings))
	}
}

// The override is inlined and frozen with the definition exactly as a phase's
// prompt is: a run renders the body it froze, and `--refresh-def` is what re-reads
// an edited file.
func TestInlinePromptsInlinesLoopRouteOverrides(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"phase.md": "phase body", "fix.md": "fix body",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	original := Workflow{ID: "flow", Phases: []Phase{{
		ID: "work", Prompt: "phase.md",
		Gate: Gate{Routes: []Route{{Loop: "work", Max: LiteralBound(1), Prompt: "fix.md"}, {To: "done"}}},
	}}}
	got, err := InlinePrompts(ResolvedWorkflow{Workflow: original, Path: filepath.Join(dir, "workflow.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	if got.Phases[0].Gate.Routes[0].Prompt != "fix body" {
		t.Fatalf("route override = %q, want the inlined body", got.Phases[0].Gate.Routes[0].Prompt)
	}
	if got.Phases[0].Gate.Routes[1].Prompt != "" {
		t.Fatalf("an undecorated route came back with a body: %q", got.Phases[0].Gate.Routes[1].Prompt)
	}
	if original.Phases[0].Gate.Routes[0].Prompt != "fix.md" {
		t.Fatalf("source workflow was mutated: %#v", original.Phases[0].Gate.Routes[0])
	}
}

func TestInlinePromptsRejectsAMissingRouteOverride(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "phase.md"), []byte("phase body"), 0o600); err != nil {
		t.Fatal(err)
	}
	original := Workflow{ID: "flow", Phases: []Phase{{
		ID: "work", Prompt: "phase.md",
		Gate: Gate{Routes: []Route{{Loop: "work", Max: LiteralBound(1), Prompt: "gone.md"}}},
	}}}
	_, err := InlinePrompts(ResolvedWorkflow{Workflow: original, Path: filepath.Join(dir, "workflow.yaml")})
	if err == nil || !strings.Contains(err.Error(), "route 0") {
		t.Fatalf("inline error = %v, want one naming the route", err)
	}
}

// The DECISION is what the engine reads, so the mode has to survive into it —
// and only the non-default does, because a `fresh` stamped on every loop trace
// would be bytes every run pays for to say what its absence already says.
func TestGateDecisionCarriesOnlyANonDefaultSession(t *testing.T) {
	for name, testCase := range map[string]struct {
		route Route
		want  RouteSession
	}{
		"continue":         {Route{Loop: "wave", Max: LiteralBound(3), Session: SessionContinue}, SessionContinue},
		"explicitly fresh": {Route{Loop: "wave", Max: LiteralBound(3), Session: SessionFresh}, ""},
		"unset":            {Route{Loop: "wave", Max: LiteralBound(3)}, ""},
		"forward":          {Route{To: "next", Session: SessionContinue}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			decision, trace, err := EvaluateGate(
				Phase{ID: "wave", Gate: Gate{Routes: []Route{testCase.route}}}, map[string]any{}, map[string]int{})
			if err != nil {
				t.Fatal(err)
			}
			if decision.Session != testCase.want {
				t.Fatalf("decision session = %q, want %q", decision.Session, testCase.want)
			}
			encoded, err := json.Marshal(trace)
			if err != nil {
				t.Fatal(err)
			}
			var decoded GateTrace
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.Decision.Session != testCase.want {
				t.Fatalf("session did not survive the gate trace round trip: %s", encoded)
			}
		})
	}
}
