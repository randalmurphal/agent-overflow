package def

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"agent-overflow/internal/providerschema"
)

func envelopePhase() Phase {
	return Phase{ID: "review", Outputs: map[string]Variable{
		"count": {Schema: JSONSchema{Type: "number"}},
		"note":  {Schema: JSONSchema{Type: "string"}, Optional: true},
	}}
}

// A generated schema has to survive both CLIs' strict-mode validation or the
// phase fails before its first turn. providerschema owns those rules and the
// observed failures behind them; this pins the generator to them using the
// nastiest phase the authoring surface allows.
func TestEnvelopeSchemaObeysProviderStrictMode(t *testing.T) {
	closed := false
	phase := Phase{ID: "strict", Outputs: map[string]Variable{
		"body": {Schema: JSONSchema{Type: "string", Multiline: true}},
		"open": {Schema: JSONSchema{
			Type:       "object",
			Properties: map[string]JSONSchema{"a": {Type: "string"}, "b": {Type: "number"}},
			Required:   []string{"a"},
		}},
		"rows": {Schema: JSONSchema{Type: "array", Items: &JSONSchema{
			Type:                 "object",
			Properties:           map[string]JSONSchema{"path": {Type: "string"}},
			Required:             []string{"path"},
			AdditionalProperties: &closed,
		}}},
	}}
	encoded, err := EnvelopeSchema(phase)
	if err != nil {
		t.Fatal(err)
	}
	for _, violation := range providerschema.Validate(encoded) {
		t.Errorf("generated envelope schema breaks a provider rule — %s", violation.Error())
	}
}

// An object property the author left out of `required` still has to reach the
// wire as required-but-nullable, since strict mode has no other way to say
// "optional". ValidateEnvelope reads that null back as absent.
func TestEnvelopeSchemaWidensOptionalObjectProperty(t *testing.T) {
	phase := Phase{ID: "widen", Outputs: map[string]Variable{
		"meta": {Schema: JSONSchema{
			Type:       "object",
			Properties: map[string]JSONSchema{"kept": {Type: "string"}, "loose": {Type: "number"}},
			Required:   []string{"kept"},
		}},
	}}
	encoded, err := EnvelopeSchema(phase)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(encoded, &schema); err != nil {
		t.Fatal(err)
	}
	meta := schema["properties"].(map[string]any)["outputs"].(map[string]any)["properties"].(map[string]any)["meta"].(map[string]any)
	properties := meta["properties"].(map[string]any)
	if got := properties["kept"].(map[string]any)["type"]; got != "string" {
		t.Errorf("required property type = %v, want string", got)
	}
	loose := properties["loose"].(map[string]any)["type"].([]any)
	if len(loose) != 2 || loose[0] != "number" || loose[1] != "null" {
		t.Errorf("optional property type = %v, want [number null]", loose)
	}
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

func TestValidateEnvelopeRejectsNullRequiredNestedProperty(t *testing.T) {
	closed := false
	phase := Phase{ID: "object", Outputs: map[string]Variable{
		"result": {Schema: JSONSchema{
			Type:                 "object",
			Properties:           map[string]JSONSchema{"detail": {Type: "string"}},
			Required:             []string{"detail"},
			AdditionalProperties: &closed,
		}},
	}}
	payload := `{"status":"done","outputs":{"result":{"detail":null}},"question":null,"reason":null}`
	if err := ValidateEnvelope(phase, []byte(payload)); err == nil || !strings.Contains(err.Error(), "is required") {
		t.Fatalf("nested required null error = %v", err)
	}
}

// A property the author left out of `required` may answer null. Strict-mode
// providers must emit every declared key (EnvelopeSchema widens those keys to
// accept null for exactly that reason), so null is how they report an absent
// optional value.
func TestValidateEnvelopeAcceptsNullOptionalNestedProperty(t *testing.T) {
	closed := false
	phase := Phase{ID: "object", Outputs: map[string]Variable{
		"result": {Schema: JSONSchema{
			Type: "object",
			Properties: map[string]JSONSchema{
				"detail": {Type: "string"},
				"note":   {Type: "string"},
			},
			Required:             []string{"detail"},
			AdditionalProperties: &closed,
		}},
	}}
	payload := `{"status":"done","outputs":{"result":{"detail":"ok","note":null}},"question":null,"reason":null}`
	if err := ValidateEnvelope(phase, []byte(payload)); err != nil {
		t.Fatalf("nested optional null error = %v", err)
	}
}
