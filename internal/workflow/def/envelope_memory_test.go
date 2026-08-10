package def

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/providerschema"
	"agent-overflow/internal/workflow/memory"
)

func memoryPhase() Phase {
	return Phase{ID: "implement", Outputs: map[string]Variable{
		"summary": {Schema: JSONSchema{Type: "string"}},
	}}
}

func envelopeWithMemory(entries string) string {
	return `{"status":"done","outputs":{"summary":"s"},"question":null,"reason":null,` +
		`"narrative":null,"memory":` + entries + `}`
}

// The field has to be in the generated schema, not merely tolerated by
// post-validation: the top-level object is closed, so a provider under it
// physically cannot emit a property the schema does not declare.
func TestEnvelopeSchemaDeclaresMemoryAndBothProvidersAcceptIt(t *testing.T) {
	encoded, err := EnvelopeSchema(memoryPhase())
	if err != nil {
		t.Fatal(err)
	}
	for _, violation := range providerschema.Validate(encoded) {
		t.Errorf("memory field broke a provider rule — %s", violation.Error())
	}
	var schema map[string]any
	if err := json.Unmarshal(encoded, &schema); err != nil {
		t.Fatal(err)
	}
	field, declared := schema["properties"].(map[string]any)[EnvelopeMemoryField].(map[string]any)
	if !declared {
		t.Fatal("generated schema does not declare the memory field")
	}
	types, ok := field["type"].([]any)
	if !ok || len(types) != 2 || types[0] != "array" || types[1] != "null" {
		t.Fatalf("memory type = %v, want [array null]", field["type"])
	}
	required, _ := schema["required"].([]any)
	found := false
	for _, name := range required {
		found = found || name == EnvelopeMemoryField
	}
	if !found {
		// Strict mode has no optional, only required-and-nullable: a field left
		// out of `required` is one the provider refuses the whole schema over.
		t.Fatalf("memory is not in required: %v", required)
	}
	kinds, _ := field["items"].(map[string]any)["properties"].(map[string]any)["kind"].(map[string]any)["enum"].([]any)
	if len(kinds) != len(memory.Kinds) {
		t.Fatalf("kind enum = %v, want the closed vocabulary %v", kinds, memory.Kinds)
	}
	// An output may still be named `memory`: outputs nest, so the two names
	// never meet.
	nested := Phase{ID: "p", Outputs: map[string]Variable{"memory": {Schema: JSONSchema{Type: "string"}}}}
	payload := `{"status":"done","outputs":{"memory":"a deliverable"},"question":null,"reason":null,` +
		`"narrative":null,"memory":[{"kind":"warning","text":"a lesson","files":null}]}`
	if err := ValidateEnvelope(nested, []byte(payload)); err != nil {
		t.Fatalf("an output named memory collided with the control field: %v", err)
	}
}

func TestValidateEnvelopeRefusesABadMemoryKind(t *testing.T) {
	err := ValidateEnvelope(memoryPhase(), []byte(envelopeWithMemory(
		`[{"kind":"insight","text":"a lesson","files":null}]`)))
	if err == nil || !strings.Contains(err.Error(), "$.memory[0].kind") {
		t.Fatalf("bad kind error = %v", err)
	}
	if !strings.Contains(err.Error(), memory.KindList()) {
		t.Fatalf("refusal does not name the vocabulary: %v", err)
	}
}

// Provenance is the system's answer to "who wrote this". An envelope that
// supplies one is refused rather than having it quietly overwritten: an element
// told it may state its own provenance would keep doing it.
func TestValidateEnvelopeRefusesAuthorSuppliedProvenance(t *testing.T) {
	for _, field := range []string{
		`"provenance":{"runId":"someone-elses-run"}`,
		`"at":1`,
		`"wave":9`,
	} {
		payload := envelopeWithMemory(`[{"kind":"warning","text":"x","files":null,` + field + `}]`)
		err := ValidateEnvelope(memoryPhase(), []byte(payload))
		if err == nil || !strings.Contains(err.Error(), "stamped by the system") {
			t.Fatalf("%s was accepted: %v", field, err)
		}
	}
}

func TestValidateEnvelopeAppliesTheNoteBoundsAndTheEnvelopeBound(t *testing.T) {
	long := strings.Repeat("x", memory.MaxTextBytes+1)
	err := ValidateEnvelope(memoryPhase(), []byte(envelopeWithMemory(
		`[{"kind":"warning","text":"`+long+`","files":null}]`)))
	if err == nil || !strings.Contains(err.Error(), "$.memory[0].text") {
		t.Fatalf("oversize text error = %v", err)
	}
	var entries []string
	for index := 0; index <= MaxEnvelopeMemoryNotes; index++ {
		entries = append(entries, `{"kind":"learning","text":"a lesson","files":null}`)
	}
	err = ValidateEnvelope(memoryPhase(), []byte(envelopeWithMemory("["+strings.Join(entries, ",")+"]")),
		DefaultEnvelopeSizeCap)
	if err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("over-count error = %v", err)
	}
}

// `memory` sits outside every branch rule for the same reason `narrative` does:
// a done, a question, and a stuck element all learned things.
func TestMemoryIsLegalOnEveryStatus(t *testing.T) {
	notes := `[{"kind":"learning","text":"a lesson","files":["a.go"]}]`
	for name, payload := range map[string]string{
		"done":     `{"status":"done","outputs":{"summary":"s"},"question":null,"reason":null,"memory":` + notes + `}`,
		"question": `{"status":"question","outputs":null,"question":"which?","reason":null,"memory":` + notes + `}`,
		"stuck":    `{"status":"stuck","outputs":null,"question":null,"reason":"no network","memory":` + notes + `}`,
		"null":     `{"status":"done","outputs":{"summary":"s"},"question":null,"reason":null,"memory":null}`,
		"absent":   `{"status":"done","outputs":{"summary":"s"},"question":null,"reason":null}`,
		"empty":    `{"status":"done","outputs":{"summary":"s"},"question":null,"reason":null,"memory":[]}`,
	} {
		if err := ValidateEnvelope(memoryPhase(), []byte(payload)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := ValidateEnvelope(memoryPhase(), []byte(envelopeWithMemory(`"not an array"`))); err == nil ||
		!strings.Contains(err.Error(), "$.memory") {
		t.Fatalf("a non-array memory was accepted: %v", err)
	}
}

func TestSplitEnvelopeMemoryStripsAtTheSameSeamAsNarrative(t *testing.T) {
	drafts, stripped := SplitEnvelopeMemory(json.RawMessage(envelopeWithMemory(
		`[{"kind":"handoff","text":"the state","files":["a.go"]}]`)))
	if len(drafts) != 1 || drafts[0].Kind != memory.KindHandoff || drafts[0].Files[0] != "a.go" {
		t.Fatalf("drafts = %+v", drafts)
	}
	if strings.Contains(string(stripped), "memory") || !strings.Contains(string(stripped), `"summary":"s"`) {
		t.Fatalf("stripped = %s", stripped)
	}
	// An envelope that carries none is returned byte for byte: this is a seam,
	// not a validator, and a payload a human will read must reach them as it
	// was written.
	for name, payload := range map[string]string{
		"no memory":     `{"status":"done","outputs":null,"question":null,"reason":null}`,
		"not an object": `"a bare string"`,
		"empty":         ``,
	} {
		drafts, stripped := SplitEnvelopeMemory(json.RawMessage(payload))
		if len(drafts) != 0 || string(stripped) != payload {
			t.Errorf("%s: SplitEnvelopeMemory(%q) = %v, %q", name, payload, drafts, stripped)
		}
	}
	// A malformed field still strips and yields nothing: post-validation already
	// reported it, and leaving it in place would hand the engine the prose slot
	// this exists to keep empty.
	drafts, stripped = SplitEnvelopeMemory(json.RawMessage(`{"status":"done","memory":7}`))
	if len(drafts) != 0 || strings.Contains(string(stripped), "memory") {
		t.Fatalf("malformed memory = %v, %s", drafts, stripped)
	}
	// An entry post-validation would have refused never reaches the log, even
	// from a frozen or hand-written envelope the schema never saw.
	drafts, _ = SplitEnvelopeMemory(json.RawMessage(envelopeWithMemory(
		`[{"kind":"bogus","text":"x"},{"kind":"warning","text":"kept"}]`)))
	if len(drafts) != 1 || drafts[0].Text != "kept" {
		t.Fatalf("drafts = %+v, want only the valid one", drafts)
	}
}
