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
	if len(required) != len(envelopeControlFields) {
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

// The schema requires every control key because strict mode has no optional,
// but the keys it forces onto a provider are not a debt a hand-written envelope
// owes: post-validation reads an absent `question`/`reason` as the null a
// provider would have sent and an absent `outputs` as an empty one. Only
// `status` is literally required — a document without one is not an envelope.
// A merge join that wrote `{"status":"done","outputs":{...}}` and had it refused
// for missing null boilerplate is the incident this pins.
func TestValidateEnvelopeTreatsAbsentControlFieldsAsTheirNullMeaning(t *testing.T) {
	controlOnly := Phase{ID: "control"}
	oneOutput := Phase{ID: "one", Outputs: map[string]Variable{"count": {Schema: JSONSchema{Type: "number"}}}}
	for name, testCase := range map[string]struct {
		phase   Phase
		payload string
		want    string
	}{
		"outputs delivered, no question or reason": {envelopePhase(), `{"status":"done","outputs":{"count":2,"note":null}}`, ""},
		"nothing declared, nothing carried":        {controlOnly, `{"status":"done"}`, ""},
		"absent outputs names the deliverable":     {oneOutput, `{"status":"done"}`, "$.outputs.count: property is required"},
		"absent status":                            {envelopePhase(), `{"outputs":{"count":2,"note":null}}`, "$.status: property is required"},
		"absent question under a question":         {controlOnly, `{"status":"question"}`, "$.question: must be a non-empty string when status is question"},
		"absent reason under a stuck":              {controlOnly, `{"status":"stuck"}`, "$.reason: must be a non-empty string when status is stuck"},
	} {
		t.Run(name, func(t *testing.T) {
			err := ValidateEnvelope(testCase.phase, []byte(testCase.payload))
			if testCase.want == "" {
				if err != nil {
					t.Fatalf("ValidateEnvelope(%s) = %v, want it accepted", testCase.payload, err)
				}
				return
			}
			var validation *EnvelopeValidationError
			if !errors.As(err, &validation) || err.Error() != testCase.want {
				t.Fatalf("error = %v, want typed error %q", err, testCase.want)
			}
		})
	}
}

// Absence and an explicit null are one statement, so the two spellings of an
// envelope must validate identically — accepted together, or failing with the
// same findings. Anything less makes the boilerplate load-bearing again.
func TestValidateEnvelopeReadsAbsenceExactlyAsNull(t *testing.T) {
	message := func(err error) string {
		if err == nil {
			return ""
		}
		return err.Error()
	}
	for name, testCase := range map[string]struct {
		phase          Phase
		absent, nulled string
		fails          bool
	}{
		"done": {
			phase:  envelopePhase(),
			absent: `{"status":"done","outputs":{"count":2,"note":null}}`,
			nulled: `{"status":"done","outputs":{"count":2,"note":null},"question":null,"reason":null,"narrative":null}`,
		},
		"done with nothing to deliver": {
			phase:  Phase{ID: "control"},
			absent: `{"status":"done"}`,
			nulled: `{"status":"done","outputs":null,"question":null,"reason":null,"narrative":null}`,
		},
		"done still owing its outputs": {
			phase:  envelopePhase(),
			absent: `{"status":"done"}`,
			nulled: `{"status":"done","outputs":null,"question":null,"reason":null}`,
			fails:  true,
		},
		"question": {
			phase:  envelopePhase(),
			absent: `{"status":"question","question":"which branch?"}`,
			nulled: `{"status":"question","question":"which branch?","outputs":null,"reason":null,"narrative":null}`,
		},
		"question with none asked": {
			phase:  envelopePhase(),
			absent: `{"status":"question"}`,
			nulled: `{"status":"question","outputs":null,"question":null,"reason":null}`,
			fails:  true,
		},
		"stuck": {
			phase:  envelopePhase(),
			absent: `{"status":"stuck","reason":"no network"}`,
			nulled: `{"status":"stuck","reason":"no network","outputs":null,"question":null}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			absent := message(ValidateEnvelope(testCase.phase, []byte(testCase.absent)))
			nulled := message(ValidateEnvelope(testCase.phase, []byte(testCase.nulled)))
			if absent != nulled {
				t.Fatalf("absent = %q, explicit null = %q; the two are one statement", absent, nulled)
			}
			if (absent != "") != testCase.fails {
				t.Fatalf("ValidateEnvelope(%s) = %q, want fails = %v", testCase.absent, absent, testCase.fails)
			}
		})
	}
}

// `narrative` is the one control field with no branch rule: a done element, one
// that has to ask, and one that got stuck all did work worth an account.
func TestValidateEnvelopeAcceptsNarrativeOnEveryStatus(t *testing.T) {
	for name, payload := range map[string]string{
		"done":     `{"status":"done","outputs":{"count":2,"note":null},"question":null,"reason":null,"narrative":"I counted the callers."}`,
		"question": `{"status":"question","outputs":null,"question":"which branch?","reason":null,"narrative":"I got as far as the resolver."}`,
		"stuck":    `{"status":"stuck","outputs":null,"question":null,"reason":"no network","narrative":"I tried three mirrors."}`,
		"null":     `{"status":"done","outputs":{"count":2,"note":null},"question":null,"reason":null,"narrative":null}`,
		"absent":   `{"status":"done","outputs":{"count":2,"note":null},"question":null,"reason":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateEnvelope(envelopePhase(), []byte(payload)); err != nil {
				t.Fatalf("ValidateEnvelope(%s) = %v", payload, err)
			}
		})
	}
	notAString := `{"status":"done","outputs":{"count":2,"note":null},"question":null,"reason":null,"narrative":7}`
	if err := ValidateEnvelope(envelopePhase(), []byte(notAString)); err == nil ||
		!strings.Contains(err.Error(), "$.narrative: must be a string or null") {
		t.Fatalf("non-string narrative error = %v", err)
	}
}

// The generated schema must carry the field as required-but-nullable, which is
// the only optional strict mode has, and must still satisfy providerschema.
func TestEnvelopeSchemaCarriesTheNarrativeField(t *testing.T) {
	encoded, err := EnvelopeSchema(envelopePhase())
	if err != nil {
		t.Fatal(err)
	}
	for _, violation := range providerschema.Validate(encoded) {
		t.Errorf("narrative field broke a provider rule — %s", violation.Error())
	}
	var schema map[string]any
	if err := json.Unmarshal(encoded, &schema); err != nil {
		t.Fatal(err)
	}
	narrative, declared := schema["properties"].(map[string]any)[EnvelopeNarrativeField].(map[string]any)
	if !declared {
		t.Fatalf("schema declares no %q property: %s", EnvelopeNarrativeField, encoded)
	}
	types, ok := narrative["type"].([]any)
	if !ok || len(types) != 2 || types[0] != "string" || types[1] != "null" {
		t.Fatalf("narrative type = %v, want [string null]", narrative["type"])
	}
	// An output may still be named `narrative`: outputs nest, so the two names
	// live in different objects and can never collide.
	nested, err := EnvelopeSchema(Phase{ID: "nest", Outputs: map[string]Variable{
		EnvelopeNarrativeField: {Schema: JSONSchema{Type: "string"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	payload := `{"status":"done","outputs":{"narrative":"a deliverable"},"question":null,"reason":null,"narrative":"an account"}`
	if err := ValidateEnvelope(Phase{ID: "nest", Outputs: map[string]Variable{
		EnvelopeNarrativeField: {Schema: JSONSchema{Type: "string"}},
	}}, []byte(payload)); err != nil {
		t.Fatalf("an output named %q collided with the control field: %v (schema %s)", EnvelopeNarrativeField, err, nested)
	}
}

// The split is the seam that keeps prose out of the engine. It must strip the
// field whatever its value, and leave anything that is not an envelope object
// byte-for-byte alone so a failed payload reaches the human as it was written.
func TestSplitEnvelopeNarrative(t *testing.T) {
	narrative, stripped := SplitEnvelopeNarrative(json.RawMessage(
		`{"status":"done","outputs":{"count":2},"question":null,"reason":null,"narrative":"I counted them."}`,
	))
	if narrative != "I counted them." {
		t.Fatalf("narrative = %q", narrative)
	}
	if strings.Contains(string(stripped), "narrative") || !strings.Contains(string(stripped), `"count":2`) {
		t.Fatalf("stripped envelope = %s", stripped)
	}
	if err := ValidateEnvelope(envelopePhase(), []byte(
		`{"status":"done","outputs":{"count":2,"note":null},"question":null,"reason":null}`,
	)); err != nil {
		t.Fatalf("a stripped envelope must still validate: %v", err)
	}
	// A non-string narrative is still stripped: post-validation already reported
	// the type, and leaving it would hand the engine the slot it exists to empty.
	if text, stripped := SplitEnvelopeNarrative(json.RawMessage(`{"status":"done","narrative":7}`)); text != "" ||
		strings.Contains(string(stripped), "narrative") {
		t.Fatalf("non-string narrative = %q, stripped = %s", text, stripped)
	}
	for name, payload := range map[string]string{
		"no narrative":  `{"status":"done","outputs":null,"question":null,"reason":null}`,
		"not JSON":      `this command printed prose`,
		"not an object": `["status"]`,
		"empty":         ``,
	} {
		t.Run(name, func(t *testing.T) {
			text, stripped := SplitEnvelopeNarrative(json.RawMessage(payload))
			if text != "" || string(stripped) != payload {
				t.Fatalf("SplitEnvelopeNarrative(%q) = %q, %q, want it unchanged", payload, text, stripped)
			}
		})
	}
}

// EnvelopeAccount is what narrative recovery reads an envelope-shaped candidate
// with: a document carrying a top-level `status` is an envelope, and what can be
// salvaged from it is its account — never its raw JSON.
func TestEnvelopeAccount(t *testing.T) {
	for name, testCase := range map[string]struct {
		payload string
		want    string
		shaped  bool
	}{
		"narrative wins":  {`{"status":"done","narrative":"I read the callers.","reason":"x"}`, "I read the callers.", true},
		"reason fallback": {`{"status":"stuck","narrative":null,"reason":"no network"}`, "no network", true},
		"blank narrative": {`{"status":"stuck","narrative":"   ","reason":"no network"}`, "no network", true},
		"no account":      {`{"status":"done","outputs":{"count":2}}`, "", true},
		"prose":           {`I read the callers and found two`, "", false},
		"json prose":      {`{"finding":"the schema is wrong"}`, "", false},
		"empty":           {``, "", false},
	} {
		t.Run(name, func(t *testing.T) {
			got, shaped := EnvelopeAccount([]byte(testCase.payload))
			if got != testCase.want || shaped != testCase.shaped {
				t.Fatalf("EnvelopeAccount(%q) = %q, %v; want %q, %v", testCase.payload, got, shaped, testCase.want, testCase.shaped)
			}
		})
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
