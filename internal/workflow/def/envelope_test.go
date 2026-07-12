package def

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func envelopePhase() Phase {
	return Phase{ID: "review", Outputs: map[string]Variable{
		"count": {Schema: JSONSchema{Type: "number"}},
		"note":  {Schema: JSONSchema{Type: "string"}, Optional: true},
	}}
}

func TestEnvelopeSchemaMakesOptionalEnumNullable(t *testing.T) {
	phase := Phase{ID: "enum", Outputs: map[string]Variable{
		"verdict": {Schema: JSONSchema{Type: "string", Enum: []any{"yes", "no"}}, Optional: true},
	}}
	encoded, err := EnvelopeSchema(phase)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(encoded, &schema); err != nil {
		t.Fatal(err)
	}
	properties := schema["properties"].(map[string]any)
	outputs := properties["outputs"].(map[string]any)
	verdict := outputs["properties"].(map[string]any)["verdict"].(map[string]any)
	enum := verdict["enum"].([]any)
	if len(enum) != 3 || enum[2] != nil {
		t.Fatalf("nullable enum = %v, want [yes no null]", enum)
	}
}

func TestEnvelopeSchemaUsesBindingShapeAndIsDeterministic(t *testing.T) {
	one, err := EnvelopeSchema(envelopePhase())
	if err != nil {
		t.Fatal(err)
	}
	two, err := EnvelopeSchema(envelopePhase())
	if err != nil {
		t.Fatal(err)
	}
	if string(one) != string(two) {
		t.Fatal("schema generation is not deterministic")
	}
	var schema map[string]any
	if err := json.Unmarshal(one, &schema); err != nil {
		t.Fatal(err)
	}
	if schema["additionalProperties"] != false {
		t.Fatal("top-level schema is not closed")
	}
	required := schema["required"].([]any)
	if len(required) != 4 {
		t.Fatalf("top-level required = %v", required)
	}
	properties := schema["properties"].(map[string]any)
	outputs := properties["outputs"].(map[string]any)
	if len(outputs["required"].([]any)) != 2 {
		t.Fatalf("output keys are not all required: %v", outputs["required"])
	}
	encoded := string(one)
	if strings.Contains(encoded, "oneOf") || strings.Contains(encoded, "if") || strings.Contains(encoded, "then") {
		t.Fatalf("provider-rejected conditional keyword in schema: %s", encoded)
	}
}

func TestValidateEnvelope(t *testing.T) {
	valid := `{"status":"done","outputs":{"count":2,"note":null},"question":null,"reason":null}`
	if err := ValidateEnvelope(envelopePhase(), []byte(valid)); err != nil {
		t.Fatalf("valid envelope: %v", err)
	}
	cases := []struct{ name, payload, want string }{
		{"missing required output", `{"status":"done","outputs":{"note":null},"question":null,"reason":null}`, "$.outputs.count"},
		{"wrong output type", `{"status":"done","outputs":{"count":"two","note":null},"question":null,"reason":null}`, "must be a finite number"},
		{"question conditional", `{"status":"question","outputs":null,"question":null,"reason":null}`, "non-empty string"},
		{"stuck conditional", `{"status":"stuck","outputs":null,"question":null,"reason":null}`, "non-empty string"},
		{"unknown property", `{"status":"done","outputs":{"count":2,"note":null},"question":null,"reason":null,"extra":1}`, "$.extra"},
		{"bad status", `{"status":"failed","outputs":null,"question":null,"reason":null}`, "done, question, or stuck"},
		{"null status", `{"status":null,"outputs":null,"question":null,"reason":null}`, "must be a string"},
		{"trailing JSON", `{"status":"done","outputs":{"count":2,"note":null},"question":null,"reason":null} {}`, "trailing JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateEnvelope(envelopePhase(), []byte(tc.payload))
			var validationErr *EnvelopeValidationError
			if !errors.As(err, &validationErr) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want typed error containing %q", err, tc.want)
			}
		})
	}
	if err := ValidateEnvelope(envelopePhase(), []byte(valid), 8); err == nil || !strings.Contains(err.Error(), "write large content to a file") {
		t.Fatalf("size cap error = %v", err)
	}
}

func TestValidateEnvelopeRejectsNullNestedProperty(t *testing.T) {
	closed := false
	phase := Phase{ID: "object", Outputs: map[string]Variable{
		"result": {Schema: JSONSchema{
			Type:                 "object",
			Properties:           map[string]JSONSchema{"detail": {Type: "string"}},
			AdditionalProperties: &closed,
		}},
	}}
	payload := `{"status":"done","outputs":{"result":{"detail":null}},"question":null,"reason":null}`
	if err := ValidateEnvelope(phase, []byte(payload)); err == nil || !strings.Contains(err.Error(), "must not be null") {
		t.Fatalf("nested null error = %v", err)
	}
}
