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

func intPtr(value int) *int { return &value }
