package def

import (
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"testing"

	"agent-overflow/internal/providerschema"
)

func toolPhase(outputs map[string]Variable) Phase {
	return Phase{ID: "build", Driver: DriverTool, Check: "go-build", Outputs: outputs}
}

func TestPhaseOutputsAddsSystemOutputsOnlyForToolPhases(t *testing.T) {
	authored := map[string]Variable{"report": {Schema: JSONSchema{Type: "string"}}}

	agent := Phase{ID: "review", Driver: DriverAgent, Outputs: authored}
	if got := PhaseOutputs(agent); !reflect.DeepEqual(got, authored) {
		t.Fatalf("agent phase outputs = %v, want the authored map unchanged", got)
	}

	merged := PhaseOutputs(toolPhase(authored))
	names := make([]string, 0, len(merged))
	for name := range merged {
		names = append(names, name)
	}
	sort.Strings(names)
	if want := []string{ToolOutputExitCode, ToolOutputPassed, "report"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("tool phase outputs = %v, want %v", names, want)
	}
	if got := merged[ToolOutputPassed].Schema.Type; got != "boolean" {
		t.Fatalf("passed type = %q, want boolean", got)
	}
	if got := merged[ToolOutputExitCode].Schema.Type; got != "number" {
		t.Fatalf("exit-code type = %q, want number", got)
	}
	if len(authored) != 1 {
		t.Fatalf("merging mutated the authored map: %v", authored)
	}

	// A definition frozen before the reserved-name rule existed must still
	// execute against the system's types, never the author's.
	conflicting := PhaseOutputs(toolPhase(map[string]Variable{
		ToolOutputPassed: {Schema: JSONSchema{Type: "string"}},
	}))
	if got := conflicting[ToolOutputPassed].Schema.Type; got != "boolean" {
		t.Fatalf("redeclared passed type = %q, want the system boolean", got)
	}
}

func TestEnvelopeSchemaForToolPhaseIsProviderLegal(t *testing.T) {
	schema, err := EnvelopeSchema(toolPhase(map[string]Variable{
		"report": {Schema: JSONSchema{Type: "string"}, Optional: true},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if violations := providerschema.Validate(schema); len(violations) > 0 {
		t.Fatalf("tool phase envelope schema violates provider strict mode: %v\n%s", violations, schema)
	}
	var decoded struct {
		Properties struct {
			Outputs struct {
				Properties map[string]json.RawMessage `json:"properties"`
				Required   []string                   `json:"required"`
			} `json:"outputs"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schema, &decoded); err != nil {
		t.Fatal(err)
	}
	if want := []string{ToolOutputExitCode, ToolOutputPassed, "report"}; !reflect.DeepEqual(decoded.Properties.Outputs.Required, want) {
		t.Fatalf("required outputs = %v, want %v", decoded.Properties.Outputs.Required, want)
	}
	if len(decoded.Properties.Outputs.Properties) != 3 {
		t.Fatalf("output properties = %v", decoded.Properties.Outputs.Properties)
	}
}

func TestValidateEnvelopeAcceptsSynthesizedToolResultAndRejectsMissingAuthoredOutputs(t *testing.T) {
	synthesized := []byte(`{"status":"done","outputs":{"passed":false,"exit-code":2},"question":null,"reason":null}`)

	if err := ValidateEnvelope(toolPhase(nil), synthesized); err != nil {
		t.Fatalf("synthesized envelope rejected: %v", err)
	}

	// The exit status cannot invent an authored output, so a command that
	// declares one must write its own envelope.
	err := ValidateEnvelope(toolPhase(map[string]Variable{
		"report": {Schema: JSONSchema{Type: "string"}},
	}), synthesized)
	var validation *EnvelopeValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("declared output missing from synthesized envelope: err = %v", err)
	}
	if len(validation.Findings) != 1 || validation.Findings[0].Path != "$.outputs.report" {
		t.Fatalf("findings = %+v", validation.Findings)
	}

	// A written envelope may fill the same outputs, and the system outputs
	// remain part of the contract it has to satisfy.
	written := []byte(`{"status":"done","outputs":{"passed":true,"exit-code":0,"report":"green"},"question":null,"reason":null}`)
	if err := ValidateEnvelope(toolPhase(map[string]Variable{
		"report": {Schema: JSONSchema{Type: "string"}},
	}), written); err != nil {
		t.Fatalf("written envelope rejected: %v", err)
	}
	missingSystem := []byte(`{"status":"done","outputs":{"report":"green"},"question":null,"reason":null}`)
	if err := ValidateEnvelope(toolPhase(map[string]Variable{
		"report": {Schema: JSONSchema{Type: "string"}},
	}), missingSystem); err == nil {
		t.Fatal("written envelope omitted the system outputs and passed validation")
	}
}

func TestValidateRejectsReservedToolOutputsAndDoubleBindings(t *testing.T) {
	workflow := Workflow{ID: "flow", Name: "Flow", Phases: []Phase{{
		ID: "build", Driver: DriverTool, Check: "go-build", Command: "notify",
		Outputs: map[string]Variable{
			ToolOutputPassed: {Schema: JSONSchema{Type: "boolean"}},
			"report":         {Schema: JSONSchema{Type: "string"}},
		},
		Gate: Gate{Routes: []Route{{To: "done"}}},
	}}}
	result := Validate(ResolvedWorkflow{Workflow: workflow}, nil, nil)
	codes := map[string]bool{}
	for _, finding := range result.Findings {
		codes[finding.Code] = true
	}
	if !codes["output.reserved"] {
		t.Fatalf("redeclared system output was accepted: %+v", result.Findings)
	}
	if !codes["phase.tool"] {
		t.Fatalf("phase binding both a check and a command was accepted: %+v", result.Findings)
	}
}

// A gate routes on the check result without the author declaring it, which is
// the whole point of the implicit outputs: variable resolution has to see them.
func TestToolPhaseSystemOutputsResolveAsVariables(t *testing.T) {
	workflow := Workflow{ID: "flow", Name: "Flow", Phases: []Phase{
		{
			ID: "build", Driver: DriverTool, Check: "go-build",
			Gate: Gate{Routes: []Route{
				{When: &Predicate{Eq: &Comparison{Ref: "build.passed", Value: false}}, To: "diagnose"},
				{When: &Predicate{Gt: &Comparison{Ref: "build.exit-code", Value: 1}}, To: "diagnose"},
				{To: "done"},
			}},
		},
		{
			ID: "diagnose", Driver: DriverAgent, Provider: "claude", Model: "opus",
			Prompt: "diagnose.md",
			Inputs: map[string]Variable{"build.exit-code": {Schema: JSONSchema{Type: "number"}}},
			Gate:   Gate{Routes: []Route{{To: "failed"}}},
		},
	}}
	result := Validate(ResolvedWorkflow{Workflow: workflow, Path: "flow.yaml"}, nil, nil)
	for _, finding := range result.Findings {
		switch finding.Code {
		case "predicate.ref", "variable.unresolved", "predicate.type", "variable.type":
			t.Fatalf("system tool output did not resolve: %+v", finding)
		}
	}
}
