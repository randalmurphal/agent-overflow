package def

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// A unit is held to the phase rules for envelope generation, so the two
// generators must produce byte-identical schemas for identical declarations.
// Anything else means a unit could accept a payload a phase would reject.
func TestUnitEnvelopeSchemaIsTheSameGeneratorAsAPhase(t *testing.T) {
	outputs := map[string]Variable{
		"summary": {Schema: JSONSchema{Type: "string"}},
		"score":   {Schema: JSONSchema{Type: "number"}, Optional: true},
	}
	unitSchema, err := UnitEnvelope(Unit{ID: "port-0", Provider: "claude", Model: "sonnet", Prompt: "u.md", Outputs: outputs}).Schema()
	if err != nil {
		t.Fatal(err)
	}
	phaseSchema, err := PhaseEnvelope(Phase{ID: "port", Driver: DriverAgent, Outputs: outputs}).Schema()
	if err != nil {
		t.Fatal(err)
	}
	if string(unitSchema) != string(phaseSchema) {
		t.Fatalf("unit schema\n%s\ndiffers from phase schema\n%s", unitSchema, phaseSchema)
	}
}

// A unit that declares nothing still answers a control envelope: the run has to
// learn done/question/stuck from it. `outputs` stays a declared property with no
// members, so the provider must emit it and can only emit it empty or null.
func TestUnitWithoutOutputsGetsControlOnlyEnvelope(t *testing.T) {
	contract := UnitEnvelope(Unit{ID: "port-0", Provider: "claude", Model: "sonnet", Prompt: "u.md"})
	if len(contract.Outputs()) != 0 {
		t.Fatalf("control-only unit declared outputs %v", contract.Outputs())
	}
	encoded, err := contract.Schema()
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Outputs struct {
				Properties map[string]any `json:"properties"`
				Required   []string       `json:"required"`
			} `json:"outputs"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(encoded, &schema); err != nil {
		t.Fatal(err)
	}
	if len(schema.Properties.Outputs.Properties) != 0 || len(schema.Properties.Outputs.Required) != 0 {
		t.Fatalf("control-only envelope carries output members: %s", encoded)
	}
	if !reflect.DeepEqual(schema.Required, []string{"outputs", "question", "reason", "status"}) {
		t.Fatalf("control envelope required = %v", schema.Required)
	}
	for _, tc := range []struct {
		name    string
		payload string
		wantErr bool
	}{
		{"done with empty outputs", `{"status":"done","outputs":{},"question":null,"reason":null}`, false},
		{"stuck", `{"status":"stuck","outputs":null,"question":null,"reason":"cannot build"}`, false},
		{"question", `{"status":"question","outputs":null,"question":"which one?","reason":null}`, false},
		{"done with null outputs", `{"status":"done","outputs":null,"question":null,"reason":null}`, true},
		{"undeclared output", `{"status":"done","outputs":{"summary":"x"},"question":null,"reason":null}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := contract.Validate([]byte(tc.payload))
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate(%s) = %v, wantErr %v", tc.payload, err, tc.wantErr)
			}
		})
	}
}

// The tool driver's implicit outputs follow the binding, not the element kind: a
// command unit reports `passed`/`exit-code` for exactly the reason a `driver:
// tool` phase does, and an agent unit must not be asked for them.
func TestUnitOutputsMergeToolOutputsForCommandUnitsOnly(t *testing.T) {
	authored := map[string]Variable{"report": {Schema: JSONSchema{Type: "string"}}}
	command := UnitOutputs(Unit{ID: "check", Command: "build", Outputs: authored})
	for _, name := range []string{"report", ToolOutputPassed, ToolOutputExitCode} {
		if _, ok := command[name]; !ok {
			t.Fatalf("command unit contract is missing %q: %v", name, command)
		}
	}
	agent := UnitOutputs(Unit{ID: "port-0", Provider: "claude", Model: "sonnet", Prompt: "u.md", Outputs: authored})
	if len(agent) != 1 {
		t.Fatalf("agent unit contract = %v, want only the authored output", agent)
	}
	// The merge must not write through to the authored map, or a second read
	// would see the implicit outputs as authored ones — which validation rejects.
	if len(authored) != 1 {
		t.Fatalf("UnitOutputs mutated the authored declaration: %v", authored)
	}
}

// A join's envelope IS its phase's, so what produces the phase's envelope is the
// join's binding. A command join makes the phase report `passed`/`exit-code`
// exactly as a `driver: tool` phase does; without this the synthesized tool
// envelope would carry outputs the phase never declared and fail its own
// post-validation.
func TestPhaseProducesToolEnvelopeFollowsTheJoinForFanOut(t *testing.T) {
	fanOut := func(join Unit) Phase {
		return Phase{ID: "port", Driver: DriverAgent, Shape: ShapeFanOut,
			FanOut: []Unit{{ID: "alpha", Provider: "claude", Model: "sonnet", Prompt: "u.md"}},
			Join:   &join}
	}
	cases := []struct {
		name  string
		phase Phase
		want  bool
	}{
		{"command join", fanOut(Unit{ID: "merge", Command: "merge-branches"}), true},
		{"agent join", fanOut(Unit{ID: "merge", Provider: "claude", Model: "sonnet", Prompt: "j.md"}), false},
		{"single tool phase", Phase{ID: "build", Driver: DriverTool, Command: "build"}, true},
		{"single agent phase", Phase{ID: "plan", Driver: DriverAgent}, false},
		{"fan-out with no join", Phase{ID: "port", Driver: DriverAgent, Shape: ShapeFanOut}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PhaseProducesToolEnvelope(tc.phase); got != tc.want {
				t.Fatalf("PhaseProducesToolEnvelope = %v, want %v", got, tc.want)
			}
			outputs := PhaseOutputs(tc.phase)
			if _, ok := outputs[ToolOutputPassed]; ok != tc.want {
				t.Fatalf("PhaseOutputs implicit tool outputs present = %v, want %v", ok, tc.want)
			}
		})
	}
}

// A command join's phase contract and the envelope the tool driver synthesizes
// have to agree: the driver always supplies `passed`/`exit-code`, so the phase
// that reads its envelope must declare them.
func TestCommandJoinEnvelopeAcceptsTheSynthesizedToolResult(t *testing.T) {
	phase := Phase{ID: "port", Driver: DriverAgent, Shape: ShapeFanOut,
		Outputs: map[string]Variable{"merged": {Schema: JSONSchema{Type: "boolean"}}},
		FanOut:  []Unit{{ID: "alpha", Provider: "claude", Model: "sonnet", Prompt: "u.md"}},
		Join:    &Unit{ID: "merge", Command: "merge-branches"}}
	payload := `{"status":"done","outputs":{"merged":true,"passed":true,"exit-code":0},"question":null,"reason":null}`
	if err := PhaseEnvelope(phase).Validate([]byte(payload)); err != nil {
		t.Fatalf("command join envelope rejected by its own phase contract: %v", err)
	}
}

// UnitDefinition is what the recovery paths read a persisted unit row's contract
// through. It must answer without the attempt's variable context: re-expanding
// would mean decoding the whole input envelope to learn what the id implies.
func TestUnitDefinitionResolvesTheFrozenContract(t *testing.T) {
	dynamic := dynamicFanOutWorkflow().Phases[1]
	unit, ok := UnitDefinition(dynamic, "port-section-3", false)
	if !ok {
		t.Fatal("dynamic template did not resolve a stamped unit id")
	}
	if unit.ID != "port-section-3" || unit.Prompt != "unit.md" || unit.EffectiveAccess() != AccessWrite {
		t.Fatalf("stamped unit = %+v, want the template with the id restored", unit)
	}
	if join, ok := UnitDefinition(dynamic, "merge", true); !ok || join.Prompt != "join.md" {
		t.Fatalf("join resolution = %+v ok=%v", join, ok)
	}
	if _, ok := UnitDefinition(dynamic, "merge", false); ok {
		t.Fatal("the join resolved as a work unit")
	}
	if _, ok := UnitDefinition(dynamic, "other", true); ok {
		t.Fatal("an unrelated id resolved as the join")
	}

	static := staticFanOutWorkflow().Phases[1]
	beta, ok := UnitDefinition(static, "beta", false)
	if !ok || beta.Provider != "codex" {
		t.Fatalf("static unit = %+v ok=%v, want the authored beta", beta, ok)
	}
	if _, ok := UnitDefinition(static, "gamma", false); ok {
		t.Fatal("an id the static list does not name resolved")
	}
}

// The join reads its units' results through one reserved binding. The reference
// grammar has no indexing, so `{{units}}` — the whole array as JSON — is the
// supported form, and a path into it is an undeclared reference.
func TestJoinPromptReferencesUnitsAsOneBinding(t *testing.T) {
	phase := dynamicFanOutWorkflow().Phases[1]
	declarations := JoinDeclarations(phase)
	if messages := ValidateTemplate("merge {{units}}", declarations); len(messages) > 0 {
		t.Fatalf("{{units}} did not validate in a join prompt: %v", messages)
	}
	if messages := ValidateTemplate("merge {{units.0.id}}", declarations); len(messages) == 0 {
		t.Fatal("a path into the units array validated; the grammar has no indexing")
	}
	// A work unit's declarations must NOT carry it: only the join consolidates.
	if messages := ValidateTemplate("port {{units}}", ResolveUnitDeclarations(dynamicFanOutWorkflow(), phase)); len(messages) == 0 {
		t.Fatal("a work unit's prompt could reference the join-only units binding")
	}

	rendered, err := Interpolate("merge {{units}}", declarations, map[string]any{
		UnitsVariable: []any{
			map[string]any{"id": "port-section-0", "index": 0, "status": "done", "outputs": map[string]any{"file": "a.go"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered, `"id":"port-section-0"`) || !strings.Contains(rendered, `"file":"a.go"`) {
		t.Fatalf("rendered join prompt = %q, want the unit results as JSON", rendered)
	}
}

// The reserved binding is bound last precisely so it cannot be shadowed: a phase
// input called `units` would otherwise silently replace the results the join
// exists to read.
func TestJoinDeclarationsCannotBeShadowedByAPhaseInput(t *testing.T) {
	phase := Phase{ID: "port", Inputs: map[string]Variable{UnitsVariable: {Schema: JSONSchema{Type: "string"}}}}
	declarations := JoinDeclarations(phase)
	if declarations[UnitsVariable].Schema.Type != "array" {
		t.Fatalf("units declaration = %+v, want the reserved array shape", declarations[UnitsVariable])
	}
}

// A join declaring its own outputs names a contract nothing reads: the gate
// reads the phase's outputs, and the join answers those.
func TestJoinOutputsAreAValidationFinding(t *testing.T) {
	workflow := dynamicFanOutWorkflow()
	workflow.Phases[1].Join.Outputs = map[string]Variable{"merged": {Schema: JSONSchema{Type: "boolean"}}}
	result := Validate(fanOutFixture(t, workflow, fanOutPrompts()), validBindings())
	if !hasFinding(result.Findings, "phase.fan-out-unit", `phase "port" join`) {
		t.Fatalf("join outputs accepted; findings:\n%s", formatFindings(result.Findings))
	}
}

// A fan-out phase runs no turn of its own, so a phase-level command would be a
// binding nothing executes — and would make "what produces this phase's
// envelope" ambiguous between the phase and its join.
func TestFanOutPhaseWithToolDriverIsAFinding(t *testing.T) {
	workflow := dynamicFanOutWorkflow()
	workflow.Phases[1].Driver = DriverTool
	workflow.Phases[1].Command = "merge-branches"
	result := Validate(fanOutFixture(t, workflow, fanOutPrompts()), validBindings())
	if !hasFinding(result.Findings, "phase.fan-out", `phase "port"`) {
		t.Fatalf("fan-out driver: tool accepted; findings:\n%s", formatFindings(result.Findings))
	}
}

// A unit's outputs go through the same name and schema rules a phase's do, and a
// command unit may not redeclare what the tool driver always supplies.
func TestUnitOutputFindingsMatchPhaseOutputRules(t *testing.T) {
	workflow := dynamicFanOutWorkflow()
	workflow.Phases[1].Unit.Outputs = map[string]Variable{
		"Bad Name": {Schema: JSONSchema{Type: "string"}},
		"untyped":  {Schema: JSONSchema{Type: "widget"}},
	}
	result := Validate(fanOutFixture(t, workflow, fanOutPrompts()), validBindings())
	for _, want := range []struct{ code, element string }{
		{"output.name", `unit template "port-section" output "Bad Name"`},
		{"schema.type", `unit template "port-section" output "untyped"`},
	} {
		if !hasFinding(result.Findings, want.code, want.element) {
			t.Fatalf("missing %q naming %q; findings:\n%s", want.code, want.element, formatFindings(result.Findings))
		}
	}

	command := staticFanOutWorkflow()
	command.Phases[1].FanOut[0] = Unit{ID: "alpha", Command: "build", Outputs: map[string]Variable{
		ToolOutputPassed: {Schema: JSONSchema{Type: "boolean"}},
	}}
	prompts := fanOutPrompts()
	prompts["unit.md"] = "port"
	result = Validate(fanOutFixture(t, command, prompts), validBindings())
	if !hasFinding(result.Findings, "output.reserved", `fan-out unit "alpha" output "passed"`) {
		t.Fatalf("command unit redeclared a reserved output; findings:\n%s", formatFindings(result.Findings))
	}
}

// A unit that declares outputs still validates end to end, which is what proves
// the authoring shape reaches the generated envelope rather than being parsed
// and dropped.
func TestUnitOutputsRoundTripFromYAML(t *testing.T) {
	workflow, err := ParseBytes([]byte(`
id: port
name: Port
phases:
  - id: port
    driver: agent
    shape: fan-out
    provider: claude
    model: sonnet
    prompt: port.md
    fan_out:
      - id: alpha
        provider: claude
        model: sonnet
        prompt: unit.md
        outputs:
          file:
            schema:
              type: string
    join:
      id: merge
      provider: claude
      model: sonnet
      prompt: join.md
    outputs:
      merged:
        schema:
          type: boolean
    gate:
      routes:
        - to: done
`))
	if err != nil {
		t.Fatal(err)
	}
	unit := workflow.Phases[0].FanOut[0]
	if _, ok := unit.Outputs["file"]; !ok {
		t.Fatalf("unit outputs did not round-trip: %+v", unit)
	}
	prompts := map[string]string{"port.md": "coordinate", "unit.md": "port", "join.md": "merge {{units}}"}
	if result := Validate(fanOutFixture(t, workflow, prompts), validBindings()); !result.Valid() {
		t.Fatalf("unit outputs findings:\n%s", formatFindings(result.Findings))
	}
	payload := `{"status":"done","outputs":{"file":"a.go"},"question":null,"reason":null}`
	if err := UnitEnvelope(unit).Validate([]byte(payload)); err != nil {
		t.Fatalf("unit envelope rejected its own declared output: %v", err)
	}
}
