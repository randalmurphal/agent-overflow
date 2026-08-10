package def

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The reserved `call-depth` read: a run's own position in its call tree,
// supplied by the engine so a recursive campaign cannot mis-state its own wave
// ordinal.

func TestCallDepthResolvesForPromptsAndPredicates(t *testing.T) {
	resolved := writableValidFixture(t)
	// A phase prompt, a predicate, and a phase-output reference in one fixture:
	// the read has to be a first-class reference, not only a template token.
	writeSibling(t, resolved, "plan.md", "Wave {{call-depth}}: {{goal}}\n")
	plan := &resolved.Workflow.Phases[0]
	plan.Gate.Routes[0].When = &Predicate{Gt: &Comparison{Ref: CallDepthVariable, Value: 0}}
	plan.Gate.Routes = append(plan.Gate.Routes, Route{To: "implement"})

	result := Validate(resolved, validBindings(), nil)
	if !result.Valid() {
		t.Fatalf("call-depth did not resolve as a prompt and predicate reference:\n%s", formatFindings(result.Findings))
	}
}

// A unit and a join read the phase's declarations, so the reserved name has to
// reach them without any per-unit declaration — the rule `history.<phase>` and
// `units` already follow.
func TestCallDepthReachesUnitsAndJoins(t *testing.T) {
	phase := Phase{ID: "wave", Inputs: map[string]Variable{"goal": {Schema: JSONSchema{Type: "string"}}}}
	for name, declarations := range map[string]map[string]Variable{
		"phase": PhaseDeclarations(phase),
		"unit":  UnitDeclarations(phase, nil),
		"join":  JoinDeclarations(phase),
	} {
		if errors := ValidateTemplate("wave {{call-depth}}", declarations); len(errors) != 0 {
			t.Errorf("%s declarations reject {{call-depth}}: %v", name, errors)
		}
		if got := declarations[CallDepthVariable].Schema.Type; got != "number" {
			t.Errorf("%s call-depth type = %q, want number", name, got)
		}
		if declarations[CallDepthVariable].Optional {
			t.Errorf("%s call-depth is optional; every run has a depth and a root's is 0", name)
		}
	}
}

// The reservation is a REFUSAL, not a silent shadow: an author who declares the
// name has to learn that what they wrote is read by nothing.
func TestCallDepthIsRefusedAtEveryDeclarationSite(t *testing.T) {
	t.Run("workflow input", func(t *testing.T) {
		resolved := writableValidFixture(t)
		resolved.Workflow.Inputs[CallDepthVariable] = Variable{Schema: JSONSchema{Type: "number"}}
		result := Validate(resolved, validBindings(), nil)
		if !hasFinding(result.Findings, "input.reserved", `input "call-depth"`) {
			t.Fatalf("a workflow input named %q was accepted:\n%s", CallDepthVariable, formatFindings(result.Findings))
		}
	})

	t.Run("phase input", func(t *testing.T) {
		resolved := writableValidFixture(t)
		resolved.Workflow.Phases[0].Inputs[CallDepthVariable] = Variable{Schema: JSONSchema{Type: "number"}}
		result := Validate(resolved, validBindings(), nil)
		if !hasFinding(result.Findings, "input.reserved", `phase "plan" input "call-depth"`) {
			t.Fatalf("a phase input named %q was accepted:\n%s", CallDepthVariable, formatFindings(result.Findings))
		}
	})

	// The other end of the namespace. A phase named for a reserved read produces
	// `call-depth.<output>` references, and the engine binds the bare name LAST —
	// so the reserved number would silently overwrite the phase's whole output
	// object, which is precisely the collision the `history` reservation refuses.
	t.Run("phase id", func(t *testing.T) {
		resolved := writableValidFixture(t)
		resolved.Workflow.Phases[0].ID = CallDepthVariable
		result := Validate(resolved, validBindings(), nil)
		if !hasFinding(result.Findings, "phase.reserved", `phase "call-depth"`) {
			t.Fatalf("a phase named %q was accepted:\n%s", CallDepthVariable, formatFindings(result.Findings))
		}
	})

	t.Run("fan-out element binding", func(t *testing.T) {
		resolved := writableValidFixture(t)
		resolved.Workflow.Phases[1] = Phase{
			ID: "implement", Shape: ShapeFanOut,
			Over: "plan.approach", As: CallDepthVariable,
			Unit: &Unit{ID: "lane", Provider: "codex", Model: "test-model", Prompt: "implement.md"},
			Join: &Unit{ID: "merge", Command: "report"},
			Outputs: map[string]Variable{
				"changed": {Schema: JSONSchema{Type: "boolean"}},
			},
			Gate: Gate{Routes: []Route{{To: "review"}}},
		}
		result := Validate(resolved, validBindings(), nil)
		if !hasFinding(result.Findings, "namespace.collision", `phase "implement"`) {
			t.Fatalf("an element binding named %q was accepted:\n%s", CallDepthVariable, formatFindings(result.Findings))
		}
	})
}

// The name is compound on purpose. The bare `depth` reads as "how deep should
// this audit go" and is a name authors do declare — this package's own call
// fixtures declare one — so reserving it would convert working definitions into
// refusals. This test is the evidence, not decoration: it fails the moment the
// reservation is narrowed to the bare word.
func TestBareDepthStaysAvailableToAuthors(t *testing.T) {
	if reservedInputName("depth") {
		t.Fatal("the bare `depth` is reserved; it is a name authors already use for something else")
	}
	resolved := writableValidFixture(t)
	resolved.Workflow.Inputs["depth"] = Variable{Schema: JSONSchema{Type: "number"}, Optional: true}
	result := Validate(resolved, validBindings(), nil)
	if !result.Valid() {
		t.Fatalf("an authored input named `depth` was refused:\n%s", formatFindings(result.Findings))
	}
}

// The published authoring schema and the Go decoder have to agree that the read
// needs no declaration: a definition using it must parse under both.
func TestCallDepthNeedsNoDeclarationToParse(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "wave.md"), []byte("Wave {{call-depth}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "workflow.yaml")
	if err := os.WriteFile(path, []byte(`
id: campaign
name: Campaign
phases:
  - id: wave
    driver: agent
    provider: codex
    model: test-model
    prompt: wave.md
    gate:
      routes:
        - to: done
`), 0o600); err != nil {
		t.Fatal(err)
	}
	workflow, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	result := Validate(ResolvedWorkflow{Workflow: workflow, Scope: ScopeShared, Path: path}, nil, nil)
	if !result.Valid() {
		t.Fatalf("an undeclared {{call-depth}} was refused:\n%s", formatFindings(result.Findings))
	}
	if !strings.Contains(string(AuthoringSchema()), CallDepthVariable) {
		t.Fatalf("the published authoring schema does not document %q", CallDepthVariable)
	}
}
