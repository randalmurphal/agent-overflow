package def

import (
	"strings"
	"testing"
)

// The reserved `budget` read: the ceiling in force for the run tree and what it
// has spent, supplied by the engine so a prompt can quote a number no element
// had to compute.

func TestBudgetResolvesAsAPromptReference(t *testing.T) {
	resolved := writableValidFixture(t)
	writeSibling(t, resolved, "plan.md", "Budget: {{budget}}\n\n{{goal}}\n")

	result := Validate(resolved, validBindings(), nil)
	if !result.Valid() {
		t.Fatalf("budget did not resolve as a prompt reference:\n%s", formatFindings(result.Findings))
	}
}

// A unit and a join read the phase's declarations, so the reserved name reaches
// them without any per-unit declaration — the rule `history.<phase>`, `units`,
// and `call-depth` already follow.
func TestBudgetReachesUnitsAndJoins(t *testing.T) {
	phase := Phase{ID: "wave", Inputs: map[string]Variable{"goal": {Schema: JSONSchema{Type: "string"}}}}
	for name, declarations := range map[string]map[string]Variable{
		"phase": PhaseDeclarations(phase),
		"unit":  UnitDeclarations(phase, nil),
		"join":  JoinDeclarations(phase),
	} {
		if errors := ValidateTemplate("spend {{budget}}", declarations); len(errors) != 0 {
			t.Errorf("%s declarations reject {{budget}}: %v", name, errors)
		}
		if got := declarations[BudgetVariable].Schema.Type; got != "object" {
			t.Errorf("%s budget type = %q, want object", name, got)
		}
		// Optional is what makes absence renderable: most runs have no ceiling,
		// and a required declaration would fail interpolation on every one of them.
		if !declarations[BudgetVariable].Optional {
			t.Errorf("%s budget is required; a run with no ceiling binds nothing", name)
		}
	}
}

// The engine composes the object, so its fields are not an authored contract: a
// path into it must not validate, exactly as `{{history.review.outputs}}` does
// not. `{{budget}}` — the whole object — is the supported form.
func TestBudgetHasNoAddressableFields(t *testing.T) {
	declarations := PhaseDeclarations(Phase{ID: "wave"})
	for _, path := range []string{"budget.remaining", "budget.ceiling", "budget.estimated"} {
		if errors := ValidateTemplate("{{"+path+"}}", declarations); len(errors) == 0 {
			t.Errorf("template reference %q validated; the entry shape is the engine's", path)
		}
	}
}

// The reservation is a REFUSAL, not a silent shadow. A seed named `budget`
// would be replaced by the engine's own binding at every phase entry, so the
// author has to learn that what they wrote is read by nothing.
func TestBudgetIsRefusedAtEveryDeclarationSite(t *testing.T) {
	t.Run("workflow input", func(t *testing.T) {
		resolved := writableValidFixture(t)
		resolved.Workflow.Inputs[BudgetVariable] = Variable{Schema: JSONSchema{Type: "object"}}
		result := Validate(resolved, validBindings(), nil)
		if !hasFinding(result.Findings, "input.reserved", `input "budget"`) {
			t.Fatalf("a workflow input named %q was accepted:\n%s", BudgetVariable, formatFindings(result.Findings))
		}
	})

	t.Run("phase input", func(t *testing.T) {
		resolved := writableValidFixture(t)
		resolved.Workflow.Phases[0].Inputs[BudgetVariable] = Variable{Schema: JSONSchema{Type: "object"}}
		result := Validate(resolved, validBindings(), nil)
		if !hasFinding(result.Findings, "input.reserved", `phase "plan" input "budget"`) {
			t.Fatalf("a phase input named %q was accepted:\n%s", BudgetVariable, formatFindings(result.Findings))
		}
	})
}

// A gate may not route on spend. The read exists so a prompt can adapt, not so
// a definition can do arithmetic in a predicate — and the refusal names the
// alternative, because a definition that reached for it wants somewhere to go.
func TestBudgetIsRefusedInAGatePredicate(t *testing.T) {
	resolved := writableValidFixture(t)
	resolved.Workflow.Phases[0].Gate.Routes[0].When = &Predicate{Exists: BudgetVariable}

	result := Validate(resolved, validBindings(), nil)
	if !hasFindingMessage(result.Findings, "predicate.ref", "prompt-surface only") {
		t.Fatalf("a predicate over %q was accepted:\n%s", BudgetVariable, formatFindings(result.Findings))
	}
	if !strings.Contains(formatFindings(result.Findings), "declare that as an output") {
		t.Fatalf("the refusal names no alternative:\n%s", formatFindings(result.Findings))
	}
}
