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

// A unit and a join read the phase's declarations, so each reserved read
// reaches them without any per-unit declaration, the rule `history.<phase>`
// and `units` already follow.
func TestReservedReadsReachUnitsAndJoins(t *testing.T) {
	phase := Phase{ID: "wave", Inputs: map[string]Variable{"goal": {Schema: JSONSchema{Type: "string"}}}}
	for _, read := range []struct {
		name, schemaType string
		optional         bool
		why              string
	}{
		// Optional is what makes absence renderable: most runs have no
		// ceiling, and a required declaration would fail interpolation on
		// every one of them.
		{BudgetVariable, "object", true, "a run with no ceiling binds nothing"},
		{CallDepthVariable, "number", false, "every run has a depth and a root's is 0"},
	} {
		t.Run(read.name, func(t *testing.T) {
			for site, declarations := range map[string]map[string]Variable{
				"phase": PhaseDeclarations(phase),
				"unit":  UnitDeclarations(phase, nil),
				"join":  JoinDeclarations(phase),
			} {
				if errors := ValidateTemplate("read {{"+read.name+"}}", declarations); len(errors) != 0 {
					t.Errorf("%s declarations reject {{%s}}: %v", site, read.name, errors)
				}
				if got := declarations[read.name].Schema.Type; got != read.schemaType {
					t.Errorf("%s %s type = %q, want %s", site, read.name, got, read.schemaType)
				}
				if got := declarations[read.name].Optional; got != read.optional {
					t.Errorf("%s %s optional = %v, want %v: %s", site, read.name, got, read.optional, read.why)
				}
			}
		})
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

// The reservation is a REFUSAL, not a silent shadow. A seed named for a
// reserved read would be replaced by the engine's own binding at every phase
// entry, so the author has to learn that what they wrote is read by nothing.
func TestReservedReadsAreRefusedAsInputs(t *testing.T) {
	for _, read := range []struct{ name, schemaType string }{
		{BudgetVariable, "object"},
		{CallDepthVariable, "number"},
	} {
		t.Run(read.name+" workflow input", func(t *testing.T) {
			resolved := writableValidFixture(t)
			resolved.Workflow.Inputs[read.name] = Variable{Schema: JSONSchema{Type: read.schemaType}}
			result := Validate(resolved, validBindings(), nil)
			if !hasFinding(result.Findings, "input.reserved", `input "`+read.name+`"`) {
				t.Fatalf("a workflow input named %q was accepted:\n%s", read.name, formatFindings(result.Findings))
			}
		})
		t.Run(read.name+" phase input", func(t *testing.T) {
			resolved := writableValidFixture(t)
			resolved.Workflow.Phases[0].Inputs[read.name] = Variable{Schema: JSONSchema{Type: read.schemaType}}
			result := Validate(resolved, validBindings(), nil)
			if !hasFinding(result.Findings, "input.reserved", `phase "plan" input "`+read.name+`"`) {
				t.Fatalf("a phase input named %q was accepted:\n%s", read.name, formatFindings(result.Findings))
			}
		})
	}
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
