package def

import (
	"strings"
	"testing"
)

func historyBinding(window int) Variable {
	return Variable{Schema: JSONSchema{Type: "array"}, Window: window}
}

func TestParseHistoryBindingDeclaration(t *testing.T) {
	workflow, err := ParseBytes([]byte(`
id: loop
name: Loop
phases:
  - id: review
    driver: agent
    provider: codex
    model: test-model
    prompt: review.md
    inputs:
      history.review:
        schema:
          type: array
        window: 6
      history.fix:
        schema:
          type: array
    gate:
      routes:
        - to: done
`))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	inputs := workflow.Phases[0].Inputs
	if got := inputs["history.review"].Window; got != 6 {
		t.Fatalf("window = %d, want 6", got)
	}
	if got := EffectiveHistoryWindow(inputs["history.review"]); got != 6 {
		t.Fatalf("effective window = %d, want 6", got)
	}
	if got := EffectiveHistoryWindow(inputs["history.fix"]); got != DefaultHistoryWindow {
		t.Fatalf("undeclared window = %d, want %d", got, DefaultHistoryWindow)
	}
	if target, ok := HistoryBinding("history.review"); !ok || target != "review" {
		t.Fatalf("HistoryBinding = %q/%t, want review/true", target, ok)
	}
	if _, ok := HistoryBinding("review.notes"); ok {
		t.Fatal("an ordinary phase-output reference must not read as a history binding")
	}
}

// A frozen snapshot is decoded and never re-validated, so the ceiling has to
// hold at resolution too — not only at the dry-run that refused to author it.
func TestEffectiveHistoryWindowCapsAFrozenOverAuthoredValue(t *testing.T) {
	if got := EffectiveHistoryWindow(historyBinding(MaxHistoryWindow * 4)); got != MaxHistoryWindow {
		t.Fatalf("effective window = %d, want %d", got, MaxHistoryWindow)
	}
}

func TestValidateAcceptsHistoryBindings(t *testing.T) {
	resolved := validResolved(t)
	// Its own attempts, and those of a phase that does not dominate it: a
	// history binding names a phase, so neither rule an output reference obeys
	// applies to it.
	resolved.Workflow.Phases[1].Inputs["history.implement"] = historyBinding(0)
	resolved.Workflow.Phases[1].Inputs["history.review"] = historyBinding(MaxHistoryWindow)
	result := Validate(resolved, validBindings(), nil)
	if !result.Valid() {
		t.Fatalf("history bindings should validate:\n%s", formatFindings(result.Findings))
	}
}

func TestValidateHistoryBindingFindings(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*ResolvedWorkflow)
		wantCode string
		wantIn   string
	}{
		{"unknown phase", func(r *ResolvedWorkflow) {
			r.Workflow.Phases[1].Inputs["history.nope"] = historyBinding(0)
		}, "history.unknown-phase", `no phase "nope"`},
		{"window over cap", func(r *ResolvedWorkflow) {
			r.Workflow.Phases[1].Inputs["history.review"] = historyBinding(MaxHistoryWindow + 1)
		}, "history.window", "must be between 1 and 50"},
		{"negative window", func(r *ResolvedWorkflow) {
			r.Workflow.Phases[1].Inputs["history.review"] = historyBinding(-1)
		}, "history.window", "must be between 1 and 50"},
		{"reserved entry shape", func(r *ResolvedWorkflow) {
			r.Workflow.Phases[1].Inputs["history.review"] = Variable{Schema: JSONSchema{
				Type: "array", Items: &JSONSchema{Type: "object"},
			}}
		}, "history.schema", "entry shape is reserved"},
		{"non-array schema", func(r *ResolvedWorkflow) {
			r.Workflow.Phases[1].Inputs["history.review"] = Variable{Schema: JSONSchema{Type: "string"}}
		}, "history.schema", "entry shape is reserved"},
		{"optional", func(r *ResolvedWorkflow) {
			binding := historyBinding(0)
			binding.Optional = true
			r.Workflow.Phases[1].Inputs["history.review"] = binding
		}, "history.optional", "always bound"},
		{"phase named history", func(r *ResolvedWorkflow) {
			r.Workflow.Phases[0].ID = HistoryReservedName
		}, "phase.id", "is reserved"},
		{"workflow input named history", func(r *ResolvedWorkflow) {
			r.Workflow.Inputs[HistoryReservedName] = Variable{Schema: JSONSchema{Type: "string"}}
		}, "input.name", "is reserved"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			resolved := validResolved(t)
			testCase.mutate(&resolved)
			result := Validate(resolved, validBindings(), nil)
			if !hasFindingMessage(result.Findings, testCase.wantCode, testCase.wantIn) {
				t.Fatalf("want %s containing %q, got:\n%s", testCase.wantCode, testCase.wantIn, formatFindings(result.Findings))
			}
		})
	}
}

// `window:` configures a series of attempts. Every other declaration names one
// value, so a window there is a dead line rather than a tuning knob.
func TestValidateRefusesWindowOutsideAHistoryBinding(t *testing.T) {
	cases := map[string]func(*ResolvedWorkflow){
		"workflow input": func(r *ResolvedWorkflow) {
			input := r.Workflow.Inputs["goal"]
			input.Window = 3
			r.Workflow.Inputs["goal"] = input
		},
		"phase input": func(r *ResolvedWorkflow) {
			input := r.Workflow.Phases[1].Inputs["plan.approach"]
			input.Window = 3
			r.Workflow.Phases[1].Inputs["plan.approach"] = input
		},
		"phase output": func(r *ResolvedWorkflow) {
			output := r.Workflow.Phases[1].Outputs["changed"]
			output.Window = 3
			r.Workflow.Phases[1].Outputs["changed"] = output
		},
		"unit output": func(r *ResolvedWorkflow) {
			phase := &r.Workflow.Phases[1]
			phase.Shape = ShapeFanOut
			phase.Driver = ""
			phase.Provider, phase.Model, phase.Prompt, phase.Access = "", "", "", ""
			phase.Commands = nil
			phase.FanOut = []Unit{{
				ID: "worker", Provider: "codex", Model: "test-model", Prompt: "plan.md",
				Outputs: map[string]Variable{"note": {Schema: JSONSchema{Type: "string"}, Window: 3}},
			}}
			phase.Join = &Unit{ID: "join", Provider: "codex", Model: "test-model", Prompt: "plan.md"}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			resolved := validResolved(t)
			mutate(&resolved)
			result := Validate(resolved, validBindings(), nil)
			if !hasFindingMessage(result.Findings, "variable.window", "valid only on a history.") {
				t.Fatalf("want variable.window, got:\n%s", formatFindings(result.Findings))
			}
		})
	}
}

// The binding is a prompt surface, not a routing one: the reference grammar has
// no indexing, so `{{history.review}}` renders the whole series as JSON exactly
// as `{{units}}` does, and a path into it is refused.
func TestHistoryBindingInterpolatesAsAWholeSeries(t *testing.T) {
	declarations := map[string]Variable{"history.review": historyBinding(0)}
	if errs := ValidateTemplate("Prior rounds: {{history.review}}", declarations); len(errs) != 0 {
		t.Fatalf("ValidateTemplate: %v", errs)
	}
	if errs := ValidateTemplate("{{history.review.outputs}}", declarations); len(errs) == 0 {
		t.Fatal("a path into the series must not validate")
	}
	values := map[string]any{"history.review": []any{
		map[string]any{"attempt": 1, "status": "completed"},
	}}
	rendered, err := Interpolate("Prior rounds: {{history.review}}", declarations, values)
	if err != nil {
		t.Fatalf("Interpolate: %v", err)
	}
	if !strings.Contains(rendered, `"attempt":1`) || !strings.Contains(rendered, `"status":"completed"`) {
		t.Fatalf("rendered = %q", rendered)
	}
}
