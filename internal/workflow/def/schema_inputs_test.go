package def

import (
	"strings"
	"testing"
)

func TestValidateInputsReusesSchemaValidation(t *testing.T) {
	workflow := Workflow{ID: "ship", Inputs: map[string]Variable{
		"ticket": {Schema: JSONSchema{Type: "string", MinLength: intPtr(3)}},
		"count":  {Schema: JSONSchema{Type: "number"}, Optional: true},
	}}

	errors := ValidateInputs(workflow, map[string]any{"ticket": "x", "extra": true})
	joined := strings.Join(errors, "\n")
	for _, want := range []string{"$.seeds.ticket must contain at least 3 characters", "$.seeds.extra is not declared"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("ValidateInputs errors = %q, want %q", joined, want)
		}
	}
	if errors := ValidateInputs(workflow, map[string]any{"ticket": "AO-123", "count": float64(2)}); len(errors) != 0 {
		t.Fatalf("valid inputs rejected: %v", errors)
	}
	if errors := ValidateInputs(workflow, nil); len(errors) != 1 || !strings.Contains(errors[0], "ticket is required") {
		t.Fatalf("missing required input errors = %v", errors)
	}
}

// One refusal, one text — both halves. A run start that omits four required
// seeds prints four lines, and printing them in a different order on every
// identical attempt makes a caller diffing two refusals read a reshuffle as a
// change in what the workflow wants.
func TestValidateInputsIsOrderedForMissingAndSuppliedAlike(t *testing.T) {
	workflow := Workflow{ID: "ship", Inputs: map[string]Variable{
		"ticket":   {Schema: JSONSchema{Type: "string"}},
		"assignee": {Schema: JSONSchema{Type: "string"}},
		"release":  {Schema: JSONSchema{Type: "string"}},
		"branch":   {Schema: JSONSchema{Type: "string"}},
		"count":    {Schema: JSONSchema{Type: "number"}, Optional: true},
	}}
	// Nothing supplied is declared, so every supplied key is refused too: the two
	// loops run over different maps and both have to be ordered.
	supplied := map[string]any{"zulu": 1, "alpha": 2, "mike": 3}
	want := []string{
		"$.seeds.assignee is required",
		"$.seeds.branch is required",
		"$.seeds.release is required",
		"$.seeds.ticket is required",
		`$.seeds.alpha is not declared by workflow "ship"`,
		`$.seeds.mike is not declared by workflow "ship"`,
		`$.seeds.zulu is not declared by workflow "ship"`,
	}
	// Repeated, because map iteration order is randomized per range: one pass
	// could be sorted by luck, twenty could not.
	for attempt := 0; attempt < 20; attempt++ {
		got := ValidateInputs(workflow, supplied)
		if len(got) != len(want) {
			t.Fatalf("errors = %v, want %v", got, want)
		}
		for index, line := range want {
			if got[index] != line {
				t.Fatalf("errors[%d] = %q, want %q (whole answer %v)", index, got[index], line, got)
			}
		}
	}
}

// ValidateInput is the per-key half `ValidateInputs` is built from, and the one
// an amendment is judged by. It has to agree with the whole-object form value
// for value — a seed accepted at start and refused later, or the reverse, would
// mean two definitions of what the workflow declares.
func TestValidateInputJudgesOneKeyExactlyAsTheWholeObjectDoes(t *testing.T) {
	workflow := Workflow{ID: "ship", Inputs: map[string]Variable{
		"ticket": {Schema: JSONSchema{Type: "string", MinLength: intPtr(3)}},
		"count":  {Schema: JSONSchema{Type: "number"}, Optional: true},
	}}

	for name, value := range map[string]any{
		"ticket": "x", "count": "two", "extra": true,
	} {
		single := strings.Join(ValidateInput(workflow, name, value), "\n")
		whole := strings.Join(ValidateInputs(workflow, map[string]any{"ticket": "AO-123", name: value}), "\n")
		if name == "ticket" {
			whole = strings.Join(ValidateInputs(workflow, map[string]any{"ticket": value}), "\n")
		}
		if single == "" {
			t.Fatalf("ValidateInput(%q, %v) found nothing", name, value)
		}
		if single != whole {
			t.Fatalf("ValidateInput(%q) = %q but ValidateInputs said %q", name, single, whole)
		}
	}

	// An undeclared key names the workflow, because the caller's fix is a name
	// they have to be able to read off the refusal.
	undeclared := ValidateInput(workflow, "tickett", "AO-123")
	if len(undeclared) != 1 || !strings.Contains(undeclared[0], `is not declared by workflow "ship"`) {
		t.Fatalf("undeclared key errors = %v", undeclared)
	}
	// An optional input is still typechecked when it IS supplied: optional says
	// it may be absent, not that anything goes.
	if errors := ValidateInput(workflow, "count", float64(2)); len(errors) != 0 {
		t.Fatalf("valid optional input rejected: %v", errors)
	}
	if errors := ValidateInput(workflow, "count", "two"); len(errors) == 0 {
		t.Fatal("an optional input accepted a value of the wrong type")
	}
}

func intPtr(value int) *int { return &value }
