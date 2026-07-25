package providerschema

import (
	"strings"
	"testing"
)

func messages(violations []Violation) string {
	parts := make([]string, 0, len(violations))
	for _, violation := range violations {
		parts = append(parts, violation.Error())
	}
	return strings.Join(parts, "; ")
}

func TestValidateAcceptsGeneratedEnvelopeShape(t *testing.T) {
	schema := []byte(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"status": {"type": "string", "enum": ["done", "question", "stuck"]},
			"outputs": {
				"type": ["object", "null"],
				"additionalProperties": false,
				"properties": {
					"note": {"type": ["string", "null"], "maxLength": 20},
					"rows": {"type": "array", "minItems": 1, "items": {
						"type": "object",
						"additionalProperties": false,
						"properties": {"path": {"type": "string"}},
						"required": ["path"]
					}}
				},
				"required": ["note", "rows"]
			},
			"question": {"type": ["string", "null"]},
			"reason": {"type": ["string", "null"]}
		},
		"required": ["outputs", "question", "reason", "status"]
	}`)
	if violations := Validate(schema); len(violations) > 0 {
		t.Fatalf("Validate() = %s, want none", messages(violations))
	}
}

func TestValidateAcceptsDraft07Declaration(t *testing.T) {
	schema := []byte(`{"$schema": "` + Draft07 + `", "type": "string"}`)
	if violations := Validate(schema); len(violations) > 0 {
		t.Fatalf("Validate() = %s, want none", messages(violations))
	}
}

func TestValidateRejectsProviderBreakingSchemas(t *testing.T) {
	for name, testCase := range map[string]struct {
		schema []byte
		want   string
	}{
		"draft 2020-12 meta-schema": {
			schema: []byte(`{"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "string"}`),
			want:   "$schema",
		},
		"unknown keyword": {
			schema: []byte(`{"type": "string", "multiline": true}`),
			want:   `keyword "multiline"`,
		},
		"open object": {
			schema: []byte(`{"type": "object", "properties": {"a": {"type": "string"}}, "required": ["a"]}`),
			want:   "additionalProperties",
		},
		"partial required": {
			schema: []byte(`{"type": "object", "additionalProperties": false,
				"properties": {"a": {"type": "string"}, "b": {"type": "string"}}, "required": ["a"]}`),
			want: `property "b" must be listed in required`,
		},
		"open object nested in an array": {
			schema: []byte(`{"type": "array", "items": {"type": "object",
				"properties": {"a": {"type": "string"}}, "required": ["a"]}}`),
			want: "$.items: object must set additionalProperties",
		},
		"missing type": {
			schema: []byte(`{"description": "no type here"}`),
			want:   "must declare a type",
		},
		"malformed json": {
			schema: []byte(`{"type":`),
			want:   "invalid JSON",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := messages(Validate(testCase.schema))
			if !strings.Contains(got, testCase.want) {
				t.Fatalf("Validate() = %q, want it to mention %q", got, testCase.want)
			}
		})
	}
}

// A nullable object still has to satisfy the object rules: Codex reports the
// violation against the union's object branch (context=(... 'type','0',...)).
func TestValidateChecksNullableObjectBranch(t *testing.T) {
	schema := []byte(`{"type": ["object", "null"], "properties": {"a": {"type": "string"}}, "required": ["a"]}`)
	if got := messages(Validate(schema)); !strings.Contains(got, "additionalProperties") {
		t.Fatalf("Validate() = %q, want an additionalProperties violation", got)
	}
}
