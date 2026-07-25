package def

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fanOutFixture writes the prompt files a fan-out workflow references so
// validation exercises the real prompt/template path instead of reporting a
// missing file for every unit.
func fanOutFixture(t *testing.T, workflow Workflow, prompts map[string]string) ResolvedWorkflow {
	t.Helper()
	dir := t.TempDir()
	for name, body := range prompts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return ResolvedWorkflow{Workflow: workflow, Scope: ScopeShared, Path: filepath.Join(dir, "workflow.yaml")}
}

func sectionsSchema() JSONSchema {
	return JSONSchema{Type: "array", Items: &JSONSchema{
		Type:       "object",
		Properties: map[string]JSONSchema{"path": {Type: "string"}},
		Required:   []string{"path"},
	}}
}

// dynamicFanOutWorkflow is the authoring shape under test: a plan phase emits
// an array, the port phase stamps one unit per element.
func dynamicFanOutWorkflow() Workflow {
	return Workflow{
		ID: "port", Name: "Port",
		Inputs: map[string]Variable{"goal": {Schema: JSONSchema{Type: "string"}}},
		Phases: []Phase{
			{
				ID: "plan", Driver: DriverAgent, Provider: "claude", Model: "sonnet", Prompt: "plan.md",
				Inputs:  map[string]Variable{"goal": {Schema: JSONSchema{Type: "string"}}},
				Outputs: map[string]Variable{"sections": {Schema: sectionsSchema()}},
				Gate:    Gate{Routes: []Route{{To: "port"}}},
			},
			{
				// No driver/provider/model/prompt/access: a fan-out phase runs no
				// work of its own, and declaring any of them is a finding.
				ID:    "port",
				Shape: ShapeFanOut, Over: "plan.sections", As: "section",
				Unit:    &Unit{ID: "port-section", Provider: "claude", Model: "sonnet", Prompt: "unit.md", Access: AccessWrite},
				Join:    &Unit{ID: "merge", Provider: "claude", Model: "sonnet", Prompt: "join.md"},
				Inputs:  map[string]Variable{"plan.sections": {Schema: sectionsSchema()}},
				Outputs: map[string]Variable{"merged": {Schema: JSONSchema{Type: "boolean"}}},
				Gate:    Gate{Routes: []Route{{To: "done"}}},
			},
		},
	}
}

func staticFanOutWorkflow() Workflow {
	workflow := dynamicFanOutWorkflow()
	phase := &workflow.Phases[1]
	phase.Over, phase.As, phase.Unit = "", "", nil
	phase.FanOut = []Unit{
		{ID: "alpha", Provider: "claude", Model: "sonnet", Prompt: "unit.md"},
		{ID: "beta", Provider: "codex", Model: "gpt-5.6-sol", Prompt: "unit.md"},
	}
	return workflow
}

func fanOutPrompts() map[string]string {
	return map[string]string{
		"plan.md": "plan {{goal}}", "unit.md": "port {{section.path}}", "join.md": "merge",
	}
}

func TestDynamicFanOutAuthoringFormParsesAndValidates(t *testing.T) {
	yaml := `
id: port
name: Port
inputs:
  goal:
    schema:
      type: string
phases:
  - id: plan
    driver: agent
    provider: claude
    model: sonnet
    prompt: plan.md
    inputs:
      goal:
        schema:
          type: string
    outputs:
      sections:
        schema:
          type: array
          items:
            type: object
            properties:
              path:
                type: string
            required: [path]
    gate:
      routes:
        - to: port
  - id: port
    shape: fan-out
    over: plan.sections
    as: section
    unit:
      id: port-section
      provider: claude
      model: sonnet
      prompt: unit.md
      access: write
    join:
      id: merge
      provider: claude
      model: sonnet
      prompt: join.md
    gate:
      routes:
        - to: done
`
	workflow, err := ParseBytes([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	phase := workflow.Phases[1]
	if phase.Over != "plan.sections" || phase.As != "section" || phase.Unit == nil || phase.Unit.ID != "port-section" {
		t.Fatalf("dynamic fan-out did not round-trip: %+v", phase)
	}
	if !phase.DynamicFanOut() || phase.Unit.EffectiveAccess() != AccessWrite {
		t.Fatalf("dynamic phase form = %+v", phase)
	}
	resolved := fanOutFixture(t, workflow, fanOutPrompts())
	if result := Validate(resolved, validBindings(), nil); !result.Valid() {
		t.Fatalf("dynamic fan-out findings:\n%s", formatFindings(result.Findings))
	}
}

func TestStaticFanOutStaysValid(t *testing.T) {
	resolved := fanOutFixture(t, staticFanOutWorkflow(), fanOutPrompts())
	// The static form has no element binding, so its unit prompt may only read
	// phase inputs.
	resolved.Workflow.Phases[1].Inputs["plan.sections"] = Variable{Schema: sectionsSchema()}
	prompts := fanOutPrompts()
	prompts["unit.md"] = "port {{plan.sections}}"
	resolved = fanOutFixture(t, resolved.Workflow, prompts)
	if result := Validate(resolved, validBindings(), nil); !result.Valid() {
		t.Fatalf("static fan-out findings:\n%s", formatFindings(result.Findings))
	}
}

func TestFanOutAuthoringFindings(t *testing.T) {
	cases := []struct {
		name, code, element string
		mutate              func(*Workflow)
	}{
		{"both forms", "phase.fan-out", "phase \"port\"", func(w *Workflow) {
			w.Phases[1].FanOut = []Unit{{ID: "alpha", Provider: "claude", Model: "sonnet", Prompt: "unit.md"}}
		}},
		{"neither form", "phase.fan-out", "phase \"port\"", func(w *Workflow) {
			w.Phases[1].Over, w.Phases[1].As, w.Phases[1].Unit = "", "", nil
		}},
		{"missing join", "phase.fan-out", "phase \"port\"", func(w *Workflow) { w.Phases[1].Join = nil }},
		{"missing over", "phase.fan-out", "phase \"port\"", func(w *Workflow) { w.Phases[1].Over = "" }},
		{"missing template", "phase.fan-out", "phase \"port\"", func(w *Workflow) { w.Phases[1].Unit = nil }},
		{"missing binding", "phase.fan-out", "phase \"port\"", func(w *Workflow) { w.Phases[1].As = "" }},
		{"binding pattern", "phase.fan-out", "phase \"port\"", func(w *Workflow) { w.Phases[1].As = "Section" }},
		{"binding collides with input", "namespace.collision", "phase \"port\"", func(w *Workflow) { w.Phases[1].As = "goal" }},
		{"binding collides with phase", "namespace.collision", "phase \"port\"", func(w *Workflow) { w.Phases[1].As = "plan" }},
		{"fan-out fields without shape", "phase.fan-out", "phase \"port\"", func(w *Workflow) { w.Phases[1].Shape = ShapeSingle }},
		{"over does not resolve", "variable.unresolved", "phase \"port\" over \"plan.missing\"", func(w *Workflow) {
			w.Phases[1].Over = "plan.missing"
		}},
		{"over is not an array", "variable.type", "phase \"port\" over \"goal\"", func(w *Workflow) { w.Phases[1].Over = "goal" }},
		{"over is optional", "variable.optionality", "phase \"port\" over \"plan.sections\"", func(w *Workflow) {
			w.Phases[0].Outputs["sections"] = Variable{Schema: sectionsSchema(), Optional: true}
		}},
		{"over does not dominate", "variable.dominance", "phase \"port\" over \"port.merged\"", func(w *Workflow) {
			w.Phases[1].Outputs["merged"] = Variable{Schema: sectionsSchema()}
			w.Phases[1].Over = "port.merged"
		}},
		{"template id", "phase.fan-out-unit", "phase \"port\"", func(w *Workflow) { w.Phases[1].Unit.ID = "Bad Id" }},
		{"template declares two drivers", "phase.fan-out-unit", "unit template \"port-section\"", func(w *Workflow) {
			w.Phases[1].Unit.Command = "merge-branches"
		}},
		{"template declares no driver", "phase.fan-out-unit", "unit template \"port-section\"", func(w *Workflow) {
			w.Phases[1].Unit.Provider, w.Phases[1].Unit.Model, w.Phases[1].Unit.Prompt = "", "", ""
		}},
		{"join declares no driver", "phase.fan-out-unit", "phase \"port\" join", func(w *Workflow) {
			w.Phases[1].Join.Prompt = ""
		}},
		{"unit prompt reference", "prompt.template", "unit template \"port-section\"", func(w *Workflow) {
			w.Phases[1].Unit.Prompt = "undeclared.md"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workflow := dynamicFanOutWorkflow()
			tc.mutate(&workflow)
			prompts := fanOutPrompts()
			prompts["undeclared.md"] = "port {{nowhere}}"
			result := Validate(fanOutFixture(t, workflow, prompts), validBindings(), nil)
			if !hasFinding(result.Findings, tc.code, tc.element) {
				t.Fatalf("missing code %q naming %q; findings:\n%s", tc.code, tc.element, formatFindings(result.Findings))
			}
		})
	}
}

// A fan-out phase runs no work of its own: startPhaseWork expands it into units
// instead of starting a runner, phaseResources skips its provider bound, and
// PhaseProducesToolEnvelope answers from the join. Every field that would
// configure the phase's own execution is therefore refused rather than accepted
// and ignored — the trap this pins is an author writing `provider:` or
// `access: write` on the phase and never learning it reached no unit.
func TestFanOutRefusesPhaseLevelExecutionFields(t *testing.T) {
	cases := []struct {
		name   string
		names  string
		mutate func(*Phase)
	}{
		{"driver", "driver", func(p *Phase) { p.Driver = DriverAgent }},
		{"tool driver", "driver", func(p *Phase) { p.Driver = DriverTool }},
		{"provider", "provider/model/prompt", func(p *Phase) { p.Provider = "claude" }},
		{"model", "provider/model/prompt", func(p *Phase) { p.Model = "sonnet" }},
		{"prompt", "provider/model/prompt", func(p *Phase) { p.Prompt = "unit.md" }},
		{"check", "check/command/commands", func(p *Phase) { p.Check = "build-and-test" }},
		{"command", "check/command/commands", func(p *Phase) { p.Command = "merge-branches" }},
		{"commands", "check/command/commands", func(p *Phase) { p.Commands = []string{"merge-branches"} }},
		{"access", "access", func(p *Phase) { p.Access = AccessWrite }},
		{"read-only access", "access", func(p *Phase) { p.Access = AccessReadOnly }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workflow := dynamicFanOutWorkflow()
			tc.mutate(&workflow.Phases[1])
			result := Validate(fanOutFixture(t, workflow, fanOutPrompts()), validBindings(), nil)
			if !hasFinding(result.Findings, "phase.fan-out", "phase \"port\"") {
				t.Fatalf("phase-level %s was accepted on a fan-out; findings:\n%s", tc.name, formatFindings(result.Findings))
			}
			var message string
			for _, found := range result.Findings {
				if found.Code == "phase.fan-out" && strings.HasPrefix(found.Message, tc.names+" is not valid") {
					message = found.Message
				}
			}
			if message == "" {
				t.Fatalf("no refusal naming %q; findings:\n%s", tc.names, formatFindings(result.Findings))
			}
			// The message has to point at the per-unit mechanism, otherwise the
			// author only learns the field is wrong, not where it belongs.
			if !strings.Contains(message, "unit") || !strings.Contains(message, "join") {
				t.Fatalf("refusal does not name the per-unit mechanism: %q", message)
			}
		})
	}
}

// The refusal must not swallow the checks a single-shape phase still needs: a
// phase with the fan-out fields removed is an ordinary agent phase again, and
// forgetting its provider is still a finding.
func TestSingleShapePhaseStillRequiresItsOwnExecutionFields(t *testing.T) {
	workflow := dynamicFanOutWorkflow()
	phase := &workflow.Phases[1]
	phase.Shape, phase.Over, phase.As, phase.Unit, phase.Join = ShapeSingle, "", "", nil, nil
	phase.Driver = DriverAgent
	result := Validate(fanOutFixture(t, workflow, fanOutPrompts()), validBindings(), nil)
	for _, code := range []string{"phase.prompt", "phase.model"} {
		if !hasFinding(result.Findings, code, "phase \"port\"") {
			t.Fatalf("missing %q on a single-shape phase; findings:\n%s", code, formatFindings(result.Findings))
		}
	}
}

func TestStaticFanOutUnitFindings(t *testing.T) {
	workflow := staticFanOutWorkflow()
	workflow.Phases[1].FanOut = []Unit{
		{ID: "alpha", Provider: "claude", Model: "sonnet", Prompt: "unit.md"},
		{ID: "alpha", Command: "merge-branches"},
		{ID: "Bad", Provider: "claude", Model: "sonnet", Prompt: "unit.md"},
		{ID: "gamma", Command: "merge-branches", Provider: "claude"},
	}
	prompts := fanOutPrompts()
	prompts["unit.md"] = "port"
	result := Validate(fanOutFixture(t, workflow, prompts), validBindings(), nil)
	for _, want := range []struct{ code, element string }{
		{"phase.fan-out-unit", "fan-out unit \"alpha\""},
		{"phase.fan-out-unit", "phase \"port\""},
		{"phase.fan-out-unit", "fan-out unit \"gamma\""},
		{"binding.command", "fan-out unit \"alpha\""},
	} {
		if !hasFinding(result.Findings, want.code, want.element) {
			t.Fatalf("missing %q naming %q; findings:\n%s", want.code, want.element, formatFindings(result.Findings))
		}
	}
}

func TestExpandUnitsStampsOneUnitPerElement(t *testing.T) {
	phase := dynamicFanOutWorkflow().Phases[1]
	vars := map[string]any{"plan.sections": []any{
		map[string]any{"path": "a"}, map[string]any{"path": "b"},
	}}
	units, err := ExpandUnits(phase, vars)
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 2 {
		t.Fatalf("expanded %d units, want 2", len(units))
	}
	for index, unit := range units {
		wantID := []string{"port-section-0", "port-section-1"}[index]
		if unit.ID != wantID || unit.Index != index || unit.Unit.ID != wantID {
			t.Fatalf("unit %d = %+v, want id %q", index, unit, wantID)
		}
		if unit.Unit.Prompt != phase.Unit.Prompt || unit.Unit.Access != AccessWrite {
			t.Fatalf("unit %d did not inherit the template: %+v", index, unit.Unit)
		}
		element, ok := unit.Bindings["section"].(map[string]any)
		if !ok || element["path"] != []string{"a", "b"}[index] {
			t.Fatalf("unit %d element binding = %+v", index, unit.Bindings)
		}
	}
}

// A dynamic expansion is the only source of unit count, so re-expanding the
// same frozen variable context after a park or a crash must reproduce exactly
// the same ids — that is what makes a persisted unit row addressable.
func TestExpandUnitsIsDeterministicForOneVariableContext(t *testing.T) {
	phase := dynamicFanOutWorkflow().Phases[1]
	vars := map[string]any{"plan.sections": []any{"a", "b", "c"}}
	first, err := ExpandUnits(phase, vars)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ExpandUnits(phase, vars)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("expansion is not deterministic:\n%+v\n%+v", first, second)
	}
}

func TestExpandUnitsEmptyArrayRunsNoUnits(t *testing.T) {
	phase := dynamicFanOutWorkflow().Phases[1]
	units, err := ExpandUnits(phase, map[string]any{"plan.sections": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 0 {
		t.Fatalf("empty array expanded to %d units", len(units))
	}
}

func TestExpandUnitsStaticListKeepsAuthoredIdentity(t *testing.T) {
	phase := staticFanOutWorkflow().Phases[1]
	units, err := ExpandUnits(phase, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 2 || units[0].ID != "alpha" || units[1].ID != "beta" || units[1].Index != 1 {
		t.Fatalf("static expansion = %+v", units)
	}
	if units[0].Bindings != nil {
		t.Fatalf("static unit carries an element binding: %+v", units[0])
	}
}

func TestExpandUnitsRejectsUnusableContext(t *testing.T) {
	phase := dynamicFanOutWorkflow().Phases[1]
	cases := []struct {
		name  string
		phase func() Phase
		vars  map[string]any
	}{
		{"missing variable", func() Phase { return phase }, map[string]any{}},
		{"null variable", func() Phase { return phase }, map[string]any{"plan.sections": nil}},
		{"not an array", func() Phase { return phase }, map[string]any{"plan.sections": "one"}},
		{"no template", func() Phase {
			broken := phase
			broken.Unit = nil
			return broken
		}, map[string]any{"plan.sections": []any{"a"}}},
		{"no binding name", func() Phase {
			broken := phase
			broken.As = ""
			return broken
		}, map[string]any{"plan.sections": []any{"a"}}},
		{"not a fan-out", func() Phase {
			broken := phase
			broken.Shape = ShapeSingle
			return broken
		}, map[string]any{"plan.sections": []any{"a"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if units, err := ExpandUnits(tc.phase(), tc.vars); err == nil {
				t.Fatalf("expansion succeeded with %d units", len(units))
			}
		})
	}
}

func TestFanOutWidthIsReportedNotFailed(t *testing.T) {
	workflow := staticFanOutWorkflow()
	workflow.Phases[1].FanOut = []Unit{
		{ID: "alpha", Provider: "claude", Model: "sonnet", Prompt: "unit.md"},
		{ID: "beta", Provider: "claude", Model: "sonnet", Prompt: "unit.md"},
		{ID: "gamma", Provider: "claude", Model: "sonnet", Prompt: "unit.md"},
		{ID: "delta", Command: "report"},
	}
	prompts := fanOutPrompts()
	prompts["unit.md"] = "port"
	resolved := fanOutFixture(t, workflow, prompts)

	result := Validate(resolved, validBindings(), nil)
	if !result.Valid() {
		t.Fatalf("width report failed the workflow:\n%s", formatFindings(result.Findings))
	}
	if len(result.Reports) != 1 {
		t.Fatalf("reports = %s", formatFindings(result.Reports))
	}
	report := result.Reports[0]
	if report.Code != "fan-out.width" || !strings.Contains(report.Message, "provider:claude capacity is 2") ||
		!strings.Contains(report.Message, "3 units") || !strings.Contains(report.Element, "phase \"port\"") {
		t.Fatalf("width report = %+v", report)
	}

	wide := validBindings().(testBindings)
	wide.declared = map[string]int{ProviderResource("claude"): 3}
	if reports := Validate(resolved, wide, nil).Reports; len(reports) != 0 {
		t.Fatalf("declared capacity still reported: %s", formatFindings(reports))
	}
	if reports := Validate(resolved, nil, nil).Reports; len(reports) != 0 {
		t.Fatalf("unchecked bindings reported width: %s", formatFindings(reports))
	}
}

// A dynamic width is a runtime fact — the dry-run cannot know it and must not
// guess one from the definition.
func TestDynamicFanOutWidthIsNeverReported(t *testing.T) {
	resolved := fanOutFixture(t, dynamicFanOutWorkflow(), fanOutPrompts())
	narrow := validBindings().(testBindings)
	narrow.declared = map[string]int{ProviderResource("claude"): 1}
	result := Validate(resolved, narrow, nil)
	if !result.Valid() || len(result.Reports) != 0 {
		t.Fatalf("dynamic fan-out reported a static width: %s", formatFindings(result.Reports))
	}
}

func TestUnitEffectiveDriver(t *testing.T) {
	if got := (Unit{ID: "a", Command: "merge"}).EffectiveDriver(); got != DriverTool {
		t.Fatalf("command unit driver = %q", got)
	}
	if got := (Unit{ID: "a", Provider: "claude", Model: "m", Prompt: "p.md"}).EffectiveDriver(); got != DriverAgent {
		t.Fatalf("agent unit driver = %q", got)
	}
}

func TestDynamicTemplateDrivesWorkspaceNeed(t *testing.T) {
	workflow := dynamicFanOutWorkflow()
	if got := DeriveWorkspaceNeed(workflow); got != WorkspaceWorktree {
		t.Fatalf("writing template need = %q", got)
	}
	workflow.Phases[1].Unit.Access = AccessReadOnly
	if got := DeriveWorkspaceNeed(workflow); got != WorkspaceProjectRoot {
		t.Fatalf("read-only template need = %q", got)
	}
}

func TestInlinePromptsInlinesTheUnitTemplate(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"unit.md": "unit body", "join.md": "join body"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	workflow := Workflow{ID: "w", Name: "W", Phases: []Phase{{
		ID: "fan", Shape: ShapeFanOut, Over: "goal", As: "item",
		Unit: &Unit{ID: "u", Prompt: "unit.md"}, Join: &Unit{ID: "j", Prompt: "join.md"},
	}}}
	inlined, err := InlinePrompts(ResolvedWorkflow{Workflow: workflow, Path: filepath.Join(dir, "workflow.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	if inlined.Phases[0].Unit.Prompt != "unit body" || inlined.Phases[0].Join.Prompt != "join body" {
		t.Fatalf("inlined prompts = %+v", inlined.Phases[0])
	}
	if workflow.Phases[0].Unit.Prompt != "unit.md" {
		t.Fatalf("InlinePrompts mutated the authored template: %+v", workflow.Phases[0].Unit)
	}
}
