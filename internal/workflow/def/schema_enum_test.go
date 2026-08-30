package def

import (
	"strings"
	"testing"
)

// The duplicate-enum rule: a repeated enum value is refused at validation
// (schema.enum), because no supplied input can ever mean the second
// occurrence and value-keyed renderers (the intake form's <select>) would
// have to repair the repeat at runtime.
func TestValidateSchemaDefinitionRefusesDuplicateEnumValues(t *testing.T) {
	cases := []struct {
		name       string
		schema     JSONSchema
		duplicates int
	}{
		{"distinct strings", JSONSchema{Type: "string", Enum: []any{"safe", "bold"}}, 0},
		{"repeated string", JSONSchema{Type: "string", Enum: []any{"safe", "bold", "safe"}}, 1},
		{"triple repeat counts each extra", JSONSchema{Type: "string", Enum: []any{"safe", "safe", "safe"}}, 2},
		// int and float64 spellings of one number are one value — the same
		// numberAsFloat64 normalization predicate comparison uses.
		{"numeric spellings collide", JSONSchema{Type: "number", Enum: []any{1, 1.0}}, 1},
		{"distinct numbers", JSONSchema{Type: "number", Enum: []any{1, 2}}, 0},
		{"repeated bool", JSONSchema{Type: "boolean", Enum: []any{true, true}}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings := validateSchemaDefinition(tc.schema, "input \"x\"")
			got := 0
			for _, f := range findings {
				if f.Code == "schema.enum" && strings.Contains(f.Message, "duplicate") {
					got++
				}
			}
			if got != tc.duplicates {
				t.Fatalf("duplicate findings = %d, want %d; findings: %+v", got, tc.duplicates, findings)
			}
		})
	}
}

// A type-mismatched value is refused once (the type finding) and never also
// blamed as a duplicate — and it cannot panic duplicate detection by being a
// non-comparable slice or map.
func TestDuplicateEnumDetectionSkipsTypeMismatchedValues(t *testing.T) {
	schema := JSONSchema{Type: "string", Enum: []any{
		[]any{"a"}, []any{"a"}, // non-comparable AND repeated: type findings only
		"safe", "safe", // still caught behind them
	}}
	findings := validateSchemaDefinition(schema, "input \"x\"")
	typeFindings, duplicateFindings := 0, 0
	for _, f := range findings {
		if f.Code != "schema.enum" {
			continue
		}
		if strings.Contains(f.Message, "duplicate") {
			duplicateFindings++
		} else {
			typeFindings++
		}
	}
	if typeFindings != 2 || duplicateFindings != 1 {
		t.Fatalf("type findings = %d (want 2), duplicate findings = %d (want 1); findings: %+v", typeFindings, duplicateFindings, findings)
	}
}
