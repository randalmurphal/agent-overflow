package def

import (
	"strings"
	"testing"
)

func TestInterpolateSinglePassAndOptionalAbsence(t *testing.T) {
	declarations := map[string]Variable{
		"goal":    {Schema: JSONSchema{Type: "string"}},
		"context": {Schema: JSONSchema{Type: "string"}, Optional: true},
	}
	got, err := Interpolate("Goal={{goal}} Context={{context}}", declarations, map[string]any{"goal": "{{context}}"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "Goal={{context}} Context=(not provided)" {
		t.Fatalf("single-pass interpolation = %q", got)
	}
}

func TestInterpolateRejectsMalformedUndeclaredAndMissingRequired(t *testing.T) {
	decl := map[string]Variable{"goal": {Schema: JSONSchema{Type: "string"}}}
	for _, input := range []string{"{{unknown}}", "{{ goal | upper }}", "{{goal"} {
		if _, err := Interpolate(input, decl, nil); err == nil {
			t.Errorf("Interpolate(%q) unexpectedly succeeded", input)
		}
	}
	_, err := Interpolate("{{goal}}", decl, nil)
	if err == nil || !strings.Contains(err.Error(), "required variable") {
		t.Fatalf("missing-required error = %v", err)
	}
}
