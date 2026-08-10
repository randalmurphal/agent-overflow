package def

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// A phase input bound straight to a workflow input, declaring no schema, takes
// the workflow input's — the duplication this removes was roughly forty percent
// of a real campaign's YAML.

// inheritanceFixture declares one input of each shape a phase can bind, so the
// edges are exercised against one document rather than five. It goes through the
// file on disk rather than ParseBytes because the whole rule is that inheritance
// is resolved AT parse: a test that assembled the Workflow in Go would be
// asserting against a shape no author can produce.
func inheritanceFixture(t *testing.T, phaseInputs string) ResolvedWorkflow {
	t.Helper()
	dir := t.TempDir()
	for _, prompt := range []string{"plan.md", "implement.md"} {
		if err := os.WriteFile(filepath.Join(dir, prompt), []byte("Work.\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "workflow.yaml")
	if err := os.WriteFile(path, []byte(`
id: inherit
name: Inherit
inputs:
  campaign-goal:
    schema:
      type: string
      multiline: true
      description: What the campaign is porting.
  mode:
    schema:
      type: string
      enum: [fast, thorough]
  context:
    schema:
      type: object
      properties:
        ticket:
          type: string
      required: [ticket]
phases:
  - id: plan
    driver: agent
    provider: codex
    model: test-model
    prompt: plan.md
    outputs:
      approach:
        schema:
          type: string
    gate:
      routes:
        - to: implement
  - id: implement
    driver: agent
    provider: codex
    model: test-model
    prompt: implement.md
    inputs:
`+phaseInputs+`
    gate:
      routes:
        - to: done
`), 0o600); err != nil {
		t.Fatal(err)
	}
	workflow, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	return ResolvedWorkflow{Workflow: workflow, Scope: ScopeShared, Path: path}
}

func inheritedInput(t *testing.T, resolved ResolvedWorkflow, name string) Variable {
	t.Helper()
	variable, ok := resolved.Workflow.Phases[1].Inputs[name]
	if !ok {
		t.Fatalf("phase declares no input %q", name)
	}
	return variable
}

func TestSchemalessPhaseInputInheritsTheWorkflowInputSchema(t *testing.T) {
	resolved := inheritanceFixture(t, `
      campaign-goal: {}
      context: {}
`)
	goal := inheritedInput(t, resolved, "campaign-goal")
	if !reflect.DeepEqual(goal.Schema, resolved.Workflow.Inputs["campaign-goal"].Schema) {
		t.Fatalf("campaign-goal schema = %+v, want the workflow input's %+v", goal.Schema, resolved.Workflow.Inputs["campaign-goal"].Schema)
	}
	// The WHOLE schema, not just its type: an inherited multiline string that
	// lost its description or its object's `required` would be a different
	// contract wearing the same name.
	if !goal.Schema.Multiline || goal.Schema.Description == "" {
		t.Fatalf("campaign-goal inherited a stripped schema: %+v", goal.Schema)
	}
	context := inheritedInput(t, resolved, "context")
	if len(context.Schema.Required) != 1 || context.Schema.Required[0] != "ticket" {
		t.Fatalf("context inherited a stripped schema: %+v", context.Schema)
	}
}

// The inherited definition has to VALIDATE, not merely parse: before the rule,
// a schema-less phase input was a `schema.type` finding and a `variable.type`
// mismatch against the very producer it was bound to.
func TestInheritedPhaseInputsValidate(t *testing.T) {
	resolved := inheritanceFixture(t, `
      campaign-goal: {}
      context: {}
`)
	if err := os.WriteFile(
		filepath.Join(filepath.Dir(resolved.Path), "implement.md"),
		[]byte("{{campaign-goal}} for {{context.ticket}}\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	result := Validate(resolved, nil, nil)
	if !result.Valid() {
		t.Fatalf("an inherited phase input did not validate:\n%s", formatFindings(result.Findings))
	}
	// And it reaches the prompt with the inherited schema, which is what makes
	// `{{context.ticket}}` — a path into an object the phase never re-declared —
	// resolve rather than read as an undeclared reference.
	if got := PhaseDeclarations(resolved.Workflow.Phases[1])["context"].Schema.Type; got != "object" {
		t.Fatalf("inherited declaration type = %q, want object", got)
	}
}

// An explicit schema wins and is NOT held to the workflow input's: narrowing is
// the reason to restate one. The ordinary producer/consumer check is what
// refuses a restatement that is not a narrowing.
func TestExplicitPhaseInputSchemaWinsAndMayNarrow(t *testing.T) {
	resolved := inheritanceFixture(t, `
      mode:
        schema:
          type: string
          enum: [thorough]
`)
	mode := inheritedInput(t, resolved, "mode")
	if len(mode.Schema.Enum) != 1 || mode.Schema.Enum[0] != "thorough" {
		t.Fatalf("mode schema = %+v, want the authored narrowing", mode.Schema)
	}
}

// A schema-less input bound to anything OTHER than a workflow input name keeps
// today's behaviour exactly: there is no unambiguous contract to copy, so the
// author is told the declaration is incomplete.
func TestSchemalessInputBoundElsewhereIsUnchanged(t *testing.T) {
	for name, binding := range map[string]string{
		"phase output":              "plan.approach: {}",
		"path into workflow input":  "context.ticket: {}",
		"undeclared workflow input": "nothing-declares-this: {}",
	} {
		t.Run(name, func(t *testing.T) {
			resolved := inheritanceFixture(t, "      "+binding+"\n")
			reference := binding[:len(binding)-len(": {}")]
			if got := inheritedInput(t, resolved, reference).Schema; !reflect.DeepEqual(got, JSONSchema{}) {
				t.Fatalf("%q inherited %+v; only an exact workflow input name inherits", reference, got)
			}
			result := Validate(resolved, nil, nil)
			if !hasFinding(result.Findings, "schema.type", `input "`+reference+`"`) {
				t.Fatalf("a schema-less %s was accepted:\n%s", name, formatFindings(result.Findings))
			}
		})
	}
}

// Inheritance is a copy taken at resolution, so a later edit to the workflow
// input cannot retroactively change a definition already in hand — which is
// what makes the frozen run snapshot carry one resolved contract.
func TestInheritanceIsACopyTakenAtResolution(t *testing.T) {
	resolved := inheritanceFixture(t, "      campaign-goal: {}\n")
	input := resolved.Workflow.Inputs["campaign-goal"]
	input.Schema.Type = "number"
	resolved.Workflow.Inputs["campaign-goal"] = input
	if got := inheritedInput(t, resolved, "campaign-goal").Schema.Type; got != "string" {
		t.Fatalf("inherited schema tracked a later edit: type = %q, want the resolved string", got)
	}
}

// ApplyInheritedInputSchemas is pure: a definition that inherits nothing is
// returned untouched, and one that does must not write through into the
// caller's phases.
func TestApplyInheritedInputSchemasDoesNotMutateItsInput(t *testing.T) {
	original := Workflow{
		ID: "inherit", Name: "Inherit",
		Inputs: map[string]Variable{"goal": {Schema: JSONSchema{Type: "string"}}},
		Phases: []Phase{{ID: "run", Inputs: map[string]Variable{"goal": {}}}},
	}
	resolvedWorkflow := ApplyInheritedInputSchemas(original)
	if got := original.Phases[0].Inputs["goal"].Schema; !reflect.DeepEqual(got, JSONSchema{}) {
		t.Fatalf("the caller's workflow was mutated: %+v", got)
	}
	if got := resolvedWorkflow.Phases[0].Inputs["goal"].Schema.Type; got != "string" {
		t.Fatalf("resolved schema type = %q, want string", got)
	}
}
