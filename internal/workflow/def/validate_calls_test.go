package def

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// mapCalls resolves call targets from an in-test definition set, standing in for
// the scoped directory resolution the CLI and app supply.
type mapCalls map[string]Workflow

func (m mapCalls) ResolveCall(id string) (ResolvedWorkflow, error) {
	workflow, ok := m[id]
	if !ok {
		return ResolvedWorkflow{}, fmt.Errorf("workflow id %q was not found", id)
	}
	return ResolvedWorkflow{Workflow: workflow, Scope: ScopeShared}, nil
}

func callsFor(workflows ...Workflow) mapCalls {
	resolver := make(mapCalls, len(workflows))
	for _, workflow := range workflows {
		resolver[workflow.ID] = workflow
	}
	return resolver
}

func schema(kind string) JSONSchema { return JSONSchema{Type: kind} }

// toolPhase is the cheapest fully-valid phase: a tool driver bound to a check,
// so a synthetic fixture needs no prompt files on disk.
func checkPhase(id string, outputs map[string]Variable, routes ...Route) Phase {
	return Phase{ID: id, Driver: DriverTool, Check: "test", Outputs: outputs, Gate: Gate{Routes: routes}}
}

func callPhase(id, target string, args map[string]string, routes ...Route) Phase {
	return Phase{ID: id, Shape: ShapeCall, Call: target, Args: args, Gate: Gate{Routes: routes}}
}

func resolvedFor(workflow Workflow) ResolvedWorkflow {
	return ResolvedWorkflow{Workflow: workflow, Scope: ScopeShared}
}

// auditChild is the standard callee: one required input, one optional input, and
// two declared outputs typed by the phase that produces them.
func auditChild() Workflow {
	return Workflow{
		ID: "child-audit", Name: "Child audit",
		Inputs: map[string]Variable{
			"subject": {Schema: schema("string")},
			"depth":   {Schema: schema("number"), Optional: true},
		},
		Outputs: map[string]WorkflowOutput{
			"approved": {From: "inspect.ok"},
			"summary":  {From: "inspect.notes"},
		},
		Phases: []Phase{checkPhase("inspect", map[string]Variable{
			"ok":    {Schema: schema("boolean")},
			"notes": {Schema: schema("string")},
		}, Route{To: "done"})},
	}
}

// callerOf builds a caller whose first phase produces the argument, whose second
// phase is the call, and whose third phase consumes the call's envelope.
func callerOf(target string, args map[string]string, consumes map[string]Variable) Workflow {
	return Workflow{
		ID: "caller", Name: "Caller",
		Phases: []Phase{
			checkPhase("prepare", map[string]Variable{"target": {Schema: schema("string")}}, Route{To: "audit"}),
			callPhase("audit", target, args, Route{To: "report"}),
			Phase{
				ID: "report", Driver: DriverTool, Check: "test", Inputs: consumes,
				Gate: Gate{Routes: []Route{{To: "done"}}},
			},
		},
	}
}

func findingsWithCode(result ValidationResult, code string) []Finding {
	var matches []Finding
	for _, item := range result.Findings {
		if item.Code == code {
			matches = append(matches, item)
		}
	}
	return matches
}

func requireFinding(t *testing.T, result ValidationResult, code, contains string) Finding {
	t.Helper()
	for _, item := range findingsWithCode(result, code) {
		if contains == "" || strings.Contains(item.Message, contains) {
			return item
		}
	}
	t.Fatalf("missing %q finding containing %q; findings:\n%s", code, contains, formatFindings(result.Findings))
	return Finding{}
}

func requireValid(t *testing.T, result ValidationResult) {
	t.Helper()
	if !result.Valid() {
		t.Fatalf("expected a valid definition; findings:\n%s", formatFindings(result.Findings))
	}
}

func TestCallPhaseFixtureParsesAndValidates(t *testing.T) {
	callerPath, err := filepath.Abs("testdata/calls/caller.yaml")
	if err != nil {
		t.Fatal(err)
	}
	caller, err := ParseFile(callerPath)
	if err != nil {
		t.Fatal(err)
	}
	child, err := ParseFile(filepath.Join(filepath.Dir(callerPath), "child.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	phase := caller.Phases[1]
	if phase.EffectiveShape() != ShapeCall || phase.CallTarget() != "child-audit" || !phase.IsCall() {
		t.Fatalf("call phase parsed as %+v", phase)
	}
	if phase.Args["subject"] != "prepare.target" || phase.MaxDepth != 2 {
		t.Fatalf("call args/max_depth parsed as %v / %d", phase.Args, phase.MaxDepth)
	}
	resolved := ResolvedWorkflow{Workflow: caller, Scope: ScopeShared, Path: callerPath}
	requireValid(t, Validate(resolved, validBindings(), callsFor(child)))
}

func TestCallPhaseRefusesFieldsThatConfigureWork(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Phase)
		message string
	}{
		{"driver", func(p *Phase) { p.Driver = DriverAgent }, "driver is not valid on a call phase"},
		{"turn", func(p *Phase) { p.Provider = "codex"; p.Model = "m"; p.Prompt = "p.md" }, "provider/model/prompt is not valid on a call phase"},
		{"command", func(p *Phase) { p.Command = "report" }, "check/command/commands is not valid on a call phase"},
		{"check", func(p *Phase) { p.Check = "test" }, "check/command/commands is not valid on a call phase"},
		{"resources", func(p *Phase) { p.Resources = []string{"builder"} }, "resources is not valid on a call phase"},
		{"grants", func(p *Phase) { p.Capabilities = []string{"start-run"}; p.MCP = []string{"srv"} }, "capabilities/mcp is not valid on a call phase"},
		{"access", func(p *Phase) { p.Access = AccessWrite }, "access is not valid on a call phase"},
		{"watchdog", func(p *Phase) { p.Watchdog = "5m" }, "watchdog is not valid on a call phase"},
		{"inputs", func(p *Phase) { p.Inputs = map[string]Variable{"prepare.target": {Schema: schema("string")}} }, "inputs is not valid on a call phase"},
		{"outputs", func(p *Phase) { p.Outputs = map[string]Variable{"ok": {Schema: schema("boolean")}} }, "outputs is not valid on a call phase"},
		{"fan-out", func(p *Phase) {
			p.FanOut = []Unit{{ID: "u", Provider: "codex", Model: "m", Prompt: "u.md"}}
			p.Join = &Unit{ID: "j", Provider: "codex", Model: "m", Prompt: "j.md"}
		}, "fan_out/over/as/unit/join is not valid on a call phase"},
		{"dynamic fan-out", func(p *Phase) { p.Over = "prepare.target"; p.As = "item" }, "fan_out/over/as/unit/join is not valid on a call phase"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caller := callerOf("child-audit", map[string]string{"subject": "prepare.target"}, nil)
			tc.mutate(&caller.Phases[1])
			result := Validate(resolvedFor(caller), validBindings(), callsFor(auditChild()))
			requireFinding(t, result, "phase.call", tc.message)
		})
	}
}

func TestCallFieldsRequireCallShape(t *testing.T) {
	caller := callerOf("child-audit", nil, nil)
	caller.Phases[1] = checkPhase("audit", nil, Route{To: "report"})
	caller.Phases[1].Call = "child-audit"
	caller.Phases[1].MaxDepth = 3
	result := Validate(resolvedFor(caller), validBindings(), callsFor(auditChild()))
	requireFinding(t, result, "phase.call", "require shape: call")
}

func TestCallShapeRequiresStaticTarget(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		caller := callerOf("", nil, nil)
		result := Validate(resolvedFor(caller), validBindings(), callsFor(auditChild()))
		requireFinding(t, result, "phase.call", "call shape requires call:")
	})
	t.Run("interpolated", func(t *testing.T) {
		caller := callerOf("{{prepare.target}}", nil, nil)
		result := Validate(resolvedFor(caller), validBindings(), callsFor(auditChild()))
		requireFinding(t, result, "phase.call", "never a variable")
	})
	t.Run("negative max depth", func(t *testing.T) {
		caller := callerOf("child-audit", map[string]string{"subject": "prepare.target"}, nil)
		caller.Phases[1].MaxDepth = -1
		result := Validate(resolvedFor(caller), validBindings(), callsFor(auditChild()))
		requireFinding(t, result, "phase.call", "max_depth must be >= 1")
	})
}

func TestCallGraphReportsEdgesItCannotValidate(t *testing.T) {
	t.Run("unresolvable target", func(t *testing.T) {
		caller := callerOf("missing-child", nil, nil)
		result := Validate(resolvedFor(caller), validBindings(), callsFor(auditChild()))
		requireFinding(t, result, "call.target", `call target "missing-child" does not resolve`)
	})
	t.Run("no resolver", func(t *testing.T) {
		caller := callerOf("child-audit", map[string]string{"subject": "prepare.target"}, nil)
		result := Validate(resolvedFor(caller), validBindings(), nil)
		requireFinding(t, result, "call.unresolved", "no workflow resolver was supplied")
	})
	t.Run("invalid child", func(t *testing.T) {
		child := auditChild()
		child.Phases[0].Gate.Routes = []Route{{To: "nowhere"}}
		caller := callerOf("child-audit", map[string]string{"subject": "prepare.target"}, nil)
		result := Validate(resolvedFor(caller), validBindings(), callsFor(child))
		finding := requireFinding(t, result, "call.child-invalid", `child workflow "child-audit" fails validation`)
		if !strings.Contains(finding.Message, `forward target "nowhere" does not exist`) {
			t.Fatalf("child finding should quote the child's own diagnostics: %s", finding.Message)
		}
	})
}

func TestCallCyclesRequireDeclaredMaxDepth(t *testing.T) {
	// caller -> child-audit -> caller: the edge that closes the cycle is the
	// child's, so that is the edge the finding names.
	recursiveChild := func() Workflow {
		child := auditChild()
		child.Phases = []Phase{
			checkPhase("inspect", map[string]Variable{
				"ok":    {Schema: schema("boolean")},
				"notes": {Schema: schema("string")},
			}, Route{To: "again"}),
			callPhase("again", "caller", nil, Route{To: "done"}),
		}
		return child
	}
	t.Run("unbounded", func(t *testing.T) {
		child := recursiveChild()
		caller := callerOf("child-audit", map[string]string{"subject": "prepare.target"}, nil)
		result := Validate(resolvedFor(caller), validBindings(), callsFor(child, caller))
		finding := requireFinding(t, result, "call.unbounded-cycle", "declare max_depth on this call edge")
		if !strings.Contains(finding.Message, "caller -> child-audit -> caller") {
			t.Fatalf("cycle finding must name the cycle: %s", finding.Message)
		}
		if !strings.Contains(finding.Element, `workflow "child-audit" phase "again"`) {
			t.Fatalf("cycle finding must name the closing edge: %s", finding.Element)
		}
	})
	t.Run("bounded", func(t *testing.T) {
		child := recursiveChild()
		child.Phases[1].MaxDepth = 3
		caller := callerOf("child-audit", map[string]string{"subject": "prepare.target"}, nil)
		requireValid(t, Validate(resolvedFor(caller), validBindings(), callsFor(child, caller)))
	})
	t.Run("self call unbounded", func(t *testing.T) {
		caller := callerOf("caller", nil, nil)
		result := Validate(resolvedFor(caller), validBindings(), callsFor(caller))
		finding := requireFinding(t, result, "call.unbounded-cycle", "caller -> caller")
		if !strings.Contains(finding.Element, `phase "audit"`) {
			t.Fatalf("self-call finding must name its own phase: %s", finding.Element)
		}
	})
	t.Run("self call bounded", func(t *testing.T) {
		caller := callerOf("caller", nil, nil)
		caller.Phases[1].MaxDepth = 2
		requireValid(t, Validate(resolvedFor(caller), validBindings(), callsFor(caller)))
	})
	t.Run("diamond is not a cycle", func(t *testing.T) {
		// Two edges reaching one child must not read as recursion.
		caller := callerOf("child-audit", map[string]string{"subject": "prepare.target"}, nil)
		caller.Phases = append(caller.Phases[:2], append([]Phase{
			callPhase("audit-again", "child-audit", map[string]string{"subject": "prepare.target"}, Route{To: "report"}),
		}, caller.Phases[2:]...)...)
		caller.Phases[1].Gate.Routes = []Route{{To: "audit-again"}}
		requireValid(t, Validate(resolvedFor(caller), validBindings(), callsFor(auditChild())))
	})
}

func TestCallArgsAreCheckedAgainstChildInputs(t *testing.T) {
	cases := []struct {
		name     string
		args     map[string]string
		mutate   func(*Workflow)
		code     string
		contains string
	}{
		{
			name: "unknown input", args: map[string]string{"subject": "prepare.target", "extra": "prepare.target"},
			code: "call.arg", contains: `declares no input "extra"`,
		},
		{
			name: "missing required input", args: nil,
			code: "call.args", contains: `requires input "subject"`,
		},
		{
			name: "unresolvable reference", args: map[string]string{"subject": "prepare.missing"},
			code: "call.arg-ref", contains: `reference "prepare.missing" does not resolve`,
		},
		{
			name: "type mismatch", args: map[string]string{"subject": "prepare.count"},
			mutate: func(w *Workflow) {
				w.Phases[0].Outputs["count"] = Variable{Schema: schema("number")}
			},
			code: "call.arg-type", contains: `is "number" but child input "subject" is "string"`,
		},
		{
			name: "non-dominating producer", args: map[string]string{"subject": "late.target"},
			mutate: func(w *Workflow) {
				w.Phases[2].Outputs = map[string]Variable{"target": {Schema: schema("string")}}
				w.Phases[2].ID = "late"
				w.Phases[1].Gate.Routes = []Route{{To: "late"}}
			},
			code: "call.arg-dominance", contains: `producer phase "late" does not dominate phase "audit"`,
		},
		{
			name: "optional producer for required input", args: map[string]string{"subject": "prepare.target"},
			mutate: func(w *Workflow) {
				w.Phases[0].Outputs["target"] = Variable{Schema: schema("string"), Optional: true}
			},
			code: "call.arg-optionality", contains: `cannot satisfy required child input "subject"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caller := callerOf("child-audit", tc.args, nil)
			if tc.mutate != nil {
				tc.mutate(&caller)
			}
			result := Validate(resolvedFor(caller), validBindings(), callsFor(auditChild()))
			requireFinding(t, result, tc.code, tc.contains)
		})
	}
	t.Run("optional child input may be omitted", func(t *testing.T) {
		caller := callerOf("child-audit", map[string]string{"subject": "prepare.target"}, nil)
		requireValid(t, Validate(resolvedFor(caller), validBindings(), callsFor(auditChild())))
	})
	t.Run("workflow input is a legal argument source", func(t *testing.T) {
		caller := callerOf("child-audit", map[string]string{"subject": "goal"}, nil)
		caller.Inputs = map[string]Variable{"goal": {Schema: schema("string")}}
		requireValid(t, Validate(resolvedFor(caller), validBindings(), callsFor(auditChild())))
	})
}

func TestCallPhaseEnvelopeIsTypedByChildOutputs(t *testing.T) {
	args := map[string]string{"subject": "prepare.target"}
	t.Run("consumer typed from the child", func(t *testing.T) {
		caller := callerOf("child-audit", args, map[string]Variable{
			"audit.approved": {Schema: schema("boolean")},
			"audit.summary":  {Schema: schema("string")},
		})
		requireValid(t, Validate(resolvedFor(caller), validBindings(), callsFor(auditChild())))
	})
	t.Run("wrong type is reported", func(t *testing.T) {
		caller := callerOf("child-audit", args, map[string]Variable{
			"audit.approved": {Schema: schema("string")},
		})
		result := Validate(resolvedFor(caller), validBindings(), callsFor(auditChild()))
		requireFinding(t, result, "variable.type", `declared type "string" does not match producer type "boolean"`)
	})
	t.Run("undeclared child output is unresolved", func(t *testing.T) {
		caller := callerOf("child-audit", args, map[string]Variable{
			"audit.invented": {Schema: schema("string")},
		})
		result := Validate(resolvedFor(caller), validBindings(), callsFor(auditChild()))
		requireFinding(t, result, "variable.unresolved", `reference "audit.invented" does not resolve`)
	})
	t.Run("gate predicate reads the child's outputs", func(t *testing.T) {
		caller := callerOf("child-audit", args, nil)
		caller.Phases[1].Gate.Routes = []Route{
			{When: &Predicate{Eq: &Comparison{Ref: "audit.approved", Value: true}}, To: "report"},
			{To: "report"},
		}
		requireValid(t, Validate(resolvedFor(caller), validBindings(), callsFor(auditChild())))
	})
	t.Run("workflow output may come from a call phase", func(t *testing.T) {
		caller := callerOf("child-audit", args, nil)
		caller.Outputs = map[string]WorkflowOutput{"verdict": {From: "audit.summary"}}
		requireValid(t, Validate(resolvedFor(caller), validBindings(), callsFor(auditChild())))
	})
	t.Run("caller definition is not mutated", func(t *testing.T) {
		caller := callerOf("child-audit", args, nil)
		Validate(resolvedFor(caller), validBindings(), callsFor(auditChild()))
		if len(caller.Phases[1].Outputs) != 0 {
			t.Fatalf("validation leaked child outputs onto the caller: %v", caller.Phases[1].Outputs)
		}
	})
}

func TestCallPhaseOutputsOmitsUnresolvableDeclarations(t *testing.T) {
	child := auditChild()
	child.Outputs["ghost"] = WorkflowOutput{From: "inspect.nothing"}
	outputs := CallPhaseOutputs(child)
	if _, present := outputs["ghost"]; present {
		t.Fatalf("unresolvable child output should not be typed: %v", outputs)
	}
	if outputs["approved"].Schema.Type != "boolean" || outputs["summary"].Schema.Type != "string" {
		t.Fatalf("call phase outputs = %v", outputs)
	}
	if CallPhaseOutputs(Workflow{ID: "empty"}) != nil {
		t.Fatal("a child with no declared outputs contributes no typed surface")
	}
}

func TestPropagatedWorkspaceNeedFollowsCallEdges(t *testing.T) {
	writer := func(id string) Workflow {
		return Workflow{ID: id, Name: id, Phases: []Phase{{
			ID: "edit", Driver: DriverAgent, Provider: "codex", Model: "m", Prompt: "e.md",
			Access: AccessWrite, Gate: Gate{Routes: []Route{{To: "done"}}},
		}}}
	}
	reader := func(id, target string) Workflow {
		phases := []Phase{checkPhase("look", nil, Route{To: "done"})}
		if target != "" {
			phases = []Phase{
				checkPhase("look", nil, Route{To: "call-out"}),
				callPhase("call-out", target, nil, Route{To: "done"}),
			}
		}
		return Workflow{ID: id, Name: id, Phases: phases}
	}
	t.Run("write need propagates through a chain", func(t *testing.T) {
		root := reader("root", "middle")
		calls := callsFor(reader("middle", "leaf"), writer("leaf"))
		need, err := PropagatedWorkspaceNeed(root, calls)
		if err != nil {
			t.Fatal(err)
		}
		if need != WorkspaceWorktree {
			t.Fatalf("propagated need = %q, want %q", need, WorkspaceWorktree)
		}
		if DeriveWorkspaceNeed(root) != WorkspaceProjectRoot {
			t.Fatal("the root's own graph must still derive project-root; propagation is the call-aware answer")
		}
	})
	t.Run("read-only chain stays at the project root", func(t *testing.T) {
		root := reader("root", "middle")
		need, err := PropagatedWorkspaceNeed(root, callsFor(reader("middle", "leaf"), reader("leaf", "")))
		if err != nil {
			t.Fatal(err)
		}
		if need != WorkspaceProjectRoot {
			t.Fatalf("propagated need = %q, want %q", need, WorkspaceProjectRoot)
		}
	})
	t.Run("recursion terminates", func(t *testing.T) {
		root := reader("root", "root")
		need, err := PropagatedWorkspaceNeed(root, callsFor(reader("root", "root")))
		if err != nil {
			t.Fatal(err)
		}
		if need != WorkspaceProjectRoot {
			t.Fatalf("propagated need = %q, want %q", need, WorkspaceProjectRoot)
		}
	})
	t.Run("unresolvable target is an error, never a guess", func(t *testing.T) {
		if _, err := PropagatedWorkspaceNeed(reader("root", "gone"), callsFor()); err == nil {
			t.Fatal("expected an error for an unresolvable call target")
		}
	})
	t.Run("missing resolver is an error when call phases exist", func(t *testing.T) {
		if _, err := PropagatedWorkspaceNeed(reader("root", "middle"), nil); err == nil {
			t.Fatal("expected an error when call edges cannot be followed")
		}
		need, err := PropagatedWorkspaceNeed(reader("root", ""), nil)
		if err != nil || need != WorkspaceProjectRoot {
			t.Fatalf("a call-free workflow needs no resolver: %q / %v", need, err)
		}
	})
}

func TestCallTargetsAreSortedAndDeduplicated(t *testing.T) {
	workflow := Workflow{ID: "root", Phases: []Phase{
		callPhase("second", "zeta", nil),
		callPhase("first", "alpha", nil),
		callPhase("again", "zeta", nil),
		checkPhase("work", nil),
	}}
	targets := CallTargets(workflow)
	if strings.Join(targets, ",") != "alpha,zeta" {
		t.Fatalf("call targets = %v", targets)
	}
}
