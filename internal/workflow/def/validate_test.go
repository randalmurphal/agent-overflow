package def

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testBindings struct {
	checks, capacities, commands map[string]bool
}

func (b testBindings) HasCheck(name string) bool    { return b.checks[name] }
func (b testBindings) HasCapacity(name string) bool { return b.capacities[name] }
func (b testBindings) HasCommand(name string) bool  { return b.commands[name] }

func validResolved(t *testing.T) ResolvedWorkflow {
	t.Helper()
	path, err := filepath.Abs("testdata/valid/workflow.yaml")
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return ResolvedWorkflow{Workflow: workflow, Scope: ScopeShared, Path: path}
}

func validBindings() Bindings {
	return testBindings{
		checks:     map[string]bool{"test": true},
		capacities: map[string]bool{"builder": true},
		commands:   map[string]bool{"report": true},
	}
}

func TestValidateValidFixture(t *testing.T) {
	result := Validate(validResolved(t), validBindings())
	if !result.Valid() {
		t.Fatalf("valid fixture findings:\n%s", formatFindings(result.Findings))
	}
	if result.BindingStatus != BindingsChecked {
		t.Fatalf("binding status = %q", result.BindingStatus)
	}
	unchecked := Validate(validResolved(t), nil)
	if !unchecked.Valid() || unchecked.BindingStatus != BindingsUnchecked {
		t.Fatalf("nil bindings should be explicit unchecked success: %+v", unchecked)
	}
}

func TestValidateGoldenErrorsNameOffendingElement(t *testing.T) {
	cases := []struct {
		name        string
		mutate      func(*ResolvedWorkflow)
		bindings    Bindings
		wantCode    string
		wantElement string
	}{
		{"workflow id", func(r *ResolvedWorkflow) { r.Workflow.ID = "Bad" }, validBindings(), "workflow.id", "workflow \"Bad\""},
		{"namespace collision", func(r *ResolvedWorkflow) { r.Workflow.Inputs["plan"] = Variable{Schema: JSONSchema{Type: "string"}} }, validBindings(), "namespace.collision", "phase \"plan\""},
		{"unresolved input", func(r *ResolvedWorkflow) {
			r.Workflow.Phases[1].Inputs = map[string]Variable{"missing": {Schema: JSONSchema{Type: "string"}}}
		}, validBindings(), "variable.unresolved", "input \"missing\""},
		{"producer dominance", func(r *ResolvedWorkflow) {
			r.Workflow.Phases[0].Inputs["implement.changed"] = Variable{Schema: JSONSchema{Type: "boolean"}}
		}, validBindings(), "variable.dominance", "phase \"plan\" input \"implement.changed\""},
		{"optional producer", func(r *ResolvedWorkflow) {
			r.Workflow.Phases[0].Outputs["approach"] = Variable{Schema: JSONSchema{Type: "string"}, Optional: true}
		}, validBindings(), "variable.optionality", "phase \"implement\" input \"plan.approach\""},
		{"dotted optional", func(r *ResolvedWorkflow) {
			r.Workflow.Inputs["context"] = Variable{Schema: JSONSchema{Type: "object", Properties: map[string]JSONSchema{"ticket": {Type: "string"}}}}
			r.Workflow.Phases[0].Inputs["context.ticket"] = Variable{Schema: JSONSchema{Type: "string"}}
		}, validBindings(), "variable.optionality", "input \"context.ticket\""},
		{"type mismatch", func(r *ResolvedWorkflow) {
			r.Workflow.Phases[1].Inputs["plan.approach"] = Variable{Schema: JSONSchema{Type: "boolean"}}
		}, validBindings(), "variable.type", "input \"plan.approach\""},
		{"enum narrowing", func(r *ResolvedWorkflow) {
			r.Workflow.Phases[0].Outputs["approach"] = Variable{Schema: JSONSchema{Type: "string"}}
			r.Workflow.Phases[1].Inputs["plan.approach"] = Variable{Schema: JSONSchema{Type: "string", Enum: []any{"safe"}}}
		}, validBindings(), "variable.type", "input \"plan.approach\""},
		{"object shape", func(r *ResolvedWorkflow) {
			r.Workflow.Phases[0].Outputs["approach"] = Variable{Schema: JSONSchema{Type: "object", Properties: map[string]JSONSchema{"summary": {Type: "string"}}}}
			r.Workflow.Phases[1].Inputs["plan.approach"] = Variable{Schema: JSONSchema{Type: "object", Properties: map[string]JSONSchema{"summary": {Type: "string"}}, Required: []string{"summary"}}}
		}, validBindings(), "variable.type", "input \"plan.approach\""},
		{"missing target", func(r *ResolvedWorkflow) { r.Workflow.Phases[0].Gate.Routes[0].To = "absent" }, validBindings(), "gate.target", "route 0"},
		{"unreachable", func(r *ResolvedWorkflow) { r.Workflow.Phases[0].Gate.Routes[0].To = "done" }, validBindings(), "graph.unreachable", "phase \"implement\""},
		{"loop max", func(r *ResolvedWorkflow) { r.Workflow.Phases[2].Gate.Routes[1].Max = 0 }, validBindings(), "gate.loop-max", "phase \"review\" route 1"},
		{"loop ancestor", func(r *ResolvedWorkflow) {
			r.Workflow.Phases[0].Gate.Routes = []Route{{Loop: "review", Max: 1}}
		}, validBindings(), "gate.loop-ancestor", "phase \"plan\" route 0"},
		{"unbounded cycle", func(r *ResolvedWorkflow) {
			r.Workflow.Phases[2].Gate.Routes[1] = Route{To: "implement"}
		}, validBindings(), "gate.unbounded-cycle", "phase \"review\" route 1"},
		{"predicate ref", func(r *ResolvedWorkflow) {
			r.Workflow.Phases[2].Gate.Routes[0].When.Eq.Ref = "missing.value"
		}, validBindings(), "predicate.ref", "phase \"review\" route 0 predicate"},
		{"predicate type", func(r *ResolvedWorkflow) {
			r.Workflow.Phases[2].Gate.Routes[0].When.Eq.Value = "yes"
		}, validBindings(), "predicate.type", "phase \"review\" route 0 predicate"},
		{"predicate operator", func(r *ResolvedWorkflow) {
			r.Workflow.Phases[2].Gate.Routes[0].When.Exists = "review.ok"
		}, validBindings(), "predicate.operator", "phase \"review\" route 0 predicate"},
		{"human approve", func(r *ResolvedWorkflow) {
			r.Workflow.Phases[2].Gate.Routes[0] = Route{Human: &HumanRoute{Reject: &LoopTarget{Loop: "implement", Max: 1}}}
		}, validBindings(), "gate.human", "phase \"review\" route 0"},
		{"prompt missing", func(r *ResolvedWorkflow) { r.Workflow.Phases[0].Prompt = "missing.md" }, validBindings(), "prompt.file", "prompt file \"missing.md\""},
		{"prompt template", func(r *ResolvedWorkflow) { r.Workflow.Phases[0].Prompt = "invalid-template.md" }, validBindings(), "prompt.template", "prompt file \"invalid-template.md\""},
		{"check binding", func(r *ResolvedWorkflow) {}, testBindings{capacities: map[string]bool{"builder": true}, commands: map[string]bool{"report": true}}, "binding.check", "phase \"review\""},
		{"capacity binding", func(r *ResolvedWorkflow) {}, testBindings{checks: map[string]bool{"test": true}, commands: map[string]bool{"report": true}}, "binding.capacity", "phase \"implement\""},
		{"command binding", func(r *ResolvedWorkflow) {}, testBindings{checks: map[string]bool{"test": true}, capacities: map[string]bool{"builder": true}}, "binding.command", "phase \"implement\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolved := validResolved(t)
			tc.mutate(&resolved)
			result := Validate(resolved, tc.bindings)
			for _, finding := range result.Findings {
				if finding.Code == tc.wantCode && strings.Contains(finding.Element, tc.wantElement) {
					return
				}
			}
			t.Fatalf("missing code %q naming %q; findings:\n%s", tc.wantCode, tc.wantElement, formatFindings(result.Findings))
		})
	}
}

func TestValidateStructuralGoldenErrors(t *testing.T) {
	cases := []struct {
		name, code, element string
		mutate              func(*ResolvedWorkflow)
	}{
		{"duplicate phase", "phase.duplicate", "phase \"plan\"", func(r *ResolvedWorkflow) { r.Workflow.Phases[1].ID = "plan" }},
		{"reserved phase", "phase.id", "phase \"done\"", func(r *ResolvedWorkflow) { r.Workflow.Phases[0].ID = "done" }},
		{"route kind", "gate.route-kind", "phase \"plan\" route 0", func(r *ResolvedWorkflow) { r.Workflow.Phases[0].Gate.Routes[0].Park = "manual" }},
		{"schema bounds", "schema.length", "output \"approach\"", func(r *ResolvedWorkflow) {
			negative := -1
			r.Workflow.Phases[0].Outputs["approach"] = Variable{Schema: JSONSchema{Type: "string", MinLength: &negative}}
		}},
		{"workflow output source", "workflow-output.ref", "output \"changed\"", func(r *ResolvedWorkflow) { r.Workflow.Outputs["changed"] = WorkflowOutput{From: "goal"} }},
		{"fan-out structure", "phase.fan-out", "phase \"plan\"", func(r *ResolvedWorkflow) { r.Workflow.Phases[0].Shape = ShapeFanOut }},
		{"access", "phase.access", "phase \"plan\"", func(r *ResolvedWorkflow) { r.Workflow.Phases[0].Access = "root" }},
		{"driver", "phase.driver", "phase \"plan\"", func(r *ResolvedWorkflow) { r.Workflow.Phases[0].Driver = "script" }},
		{"agent model", "phase.model", "phase \"plan\"", func(r *ResolvedWorkflow) { r.Workflow.Phases[0].Model = "" }},
		{"shape", "phase.shape", "phase \"plan\"", func(r *ResolvedWorkflow) { r.Workflow.Phases[0].Shape = "nested" }},
		{"watchdog syntax", "phase.watchdog", "phase \"plan\"", func(r *ResolvedWorkflow) { r.Workflow.Phases[0].Watchdog = "eventually" }},
		{"watchdog positive", "phase.watchdog", "phase \"plan\"", func(r *ResolvedWorkflow) { r.Workflow.Phases[0].Watchdog = "0s" }},
		{"human reject feedback", "gate.feedback", "phase \"review\" route 0", func(r *ResolvedWorkflow) {
			r.Workflow.Phases[2].Gate.Routes[0] = Route{Human: &HumanRoute{
				Approve: "done",
				Reject:  &LoopTarget{Loop: "implement", Max: 1, Feedback: []string{"missing"}},
			}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolved := validResolved(t)
			tc.mutate(&resolved)
			result := Validate(resolved, validBindings())
			if !hasFinding(result.Findings, tc.code, tc.element) {
				t.Fatalf("missing code %q naming %q; findings:\n%s", tc.code, tc.element, formatFindings(result.Findings))
			}
		})
	}
}

func TestValidateCollectsUnresolvedAndInvalidInputSchema(t *testing.T) {
	resolved := validResolved(t)
	negative := -1
	resolved.Workflow.Phases[1].Inputs = map[string]Variable{
		"missing": {Schema: JSONSchema{Type: "string", MinLength: &negative}},
	}
	result := Validate(resolved, validBindings())
	if !hasFinding(result.Findings, "variable.unresolved", "input \"missing\"") ||
		!hasFinding(result.Findings, "schema.length", "input \"missing\"") {
		t.Fatalf("all-errors contract failed:\n%s", formatFindings(result.Findings))
	}
}

func TestValidateAllowsOptionalNonDominatingReferencesAndCombinators(t *testing.T) {
	resolved := validResolved(t)
	resolved.Workflow.Phases[0].Inputs["review.notes"] = Variable{Schema: JSONSchema{Type: "string"}, Optional: true}
	resolved.Workflow.Phases[0].Gate.Routes[0].When = &Predicate{All: []Predicate{
		{Exists: "review.notes"},
		{Not: &Predicate{Eq: &Comparison{Ref: "goal", Value: ""}}},
	}}
	result := Validate(resolved, validBindings())
	if hasFinding(result.Findings, "variable.dominance", "review.notes") ||
		hasFinding(result.Findings, "predicate.dominance", "predicate") {
		t.Fatalf("optional non-dominating reference was rejected:\n%s", formatFindings(result.Findings))
	}
}

func TestDeriveWorkspaceNeed(t *testing.T) {
	resolved := validResolved(t)
	if got := DeriveWorkspaceNeed(resolved.Workflow); got != WorkspaceWorktree {
		t.Fatalf("writing workflow need = %q", got)
	}
	for i := range resolved.Workflow.Phases {
		resolved.Workflow.Phases[i].Access = AccessReadOnly
		resolved.Workflow.Phases[i].FanOut = nil
		resolved.Workflow.Phases[i].Join = nil
	}
	if got := DeriveWorkspaceNeed(resolved.Workflow); got != WorkspaceProjectRoot {
		t.Fatalf("read-only workflow need = %q", got)
	}
	resolved.Workflow.Phases[0].FanOut = []Unit{{ID: "writer", Access: AccessWrite}}
	if got := DeriveWorkspaceNeed(resolved.Workflow); got != WorkspaceWorktree {
		t.Fatalf("parallel writer need = %q", got)
	}
}

func TestValidatePromptConfinementAndSize(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape.md")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	workflow := Workflow{
		ID: "secure", Name: "Secure",
		Phases: []Phase{{
			ID: "one", Driver: DriverAgent, Prompt: "escape.md",
			Gate: Gate{Routes: []Route{{To: "done"}}},
		}},
	}
	resolved := ResolvedWorkflow{Workflow: workflow, Scope: ScopeShared, Path: filepath.Join(root, "workflow.yaml")}
	result := Validate(resolved, nil)
	if !hasFinding(result.Findings, "prompt.path", "escape.md") {
		t.Fatalf("symlink escape was not rejected:\n%s", formatFindings(result.Findings))
	}
	large := filepath.Join(root, "large.md")
	if err := os.WriteFile(large, []byte(strings.Repeat("x", int(MaxPromptBytes)+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved.Workflow.Phases[0].Prompt = "large.md"
	result = Validate(resolved, nil)
	if !hasFinding(result.Findings, "prompt.file", "large.md") {
		t.Fatalf("oversized prompt was not rejected:\n%s", formatFindings(result.Findings))
	}
}

func hasFinding(findings []Finding, code, element string) bool {
	for _, finding := range findings {
		if finding.Code == code && strings.Contains(finding.Element, element) {
			return true
		}
	}
	return false
}

func formatFindings(findings []Finding) string {
	var result strings.Builder
	for _, finding := range findings {
		result.WriteString(finding.Code)
		result.WriteString(": ")
		result.WriteString(finding.Error())
		result.WriteByte('\n')
	}
	return result.String()
}
