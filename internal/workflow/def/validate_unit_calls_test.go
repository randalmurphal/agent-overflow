package def

import (
	"strings"
	"testing"
)

// callFanOutWorkflow is the campaign shape: a plan phase emits an array, and the
// fan-out stamps one *call* unit per element, each running a whole sub-workflow
// in its own sub-worktree before the join consolidates them.
func callFanOutWorkflow() Workflow {
	workflow := dynamicFanOutWorkflow()
	workflow.Phases[1].Unit = &Unit{
		ID: "port-section", Call: "child-audit", Args: map[string]string{"subject": "section.path"},
	}
	return workflow
}

// callFanOutPrompts drops the unit prompt: a call unit has none, so leaving it
// on disk would let a fixture pass for the wrong reason.
func callFanOutPrompts() map[string]string {
	return map[string]string{"plan.md": "plan {{goal}}", "join.md": "merge"}
}

func validateCallFanOut(t *testing.T, workflow Workflow, children ...Workflow) ValidationResult {
	t.Helper()
	if len(children) == 0 {
		children = []Workflow{auditChild()}
	}
	return Validate(fanOutFixture(t, workflow, callFanOutPrompts()), validBindings(), callsFor(children...))
}

func TestCallUnitValidates(t *testing.T) {
	requireValid(t, validateCallFanOut(t, callFanOutWorkflow()))
}

// The three unit bindings are mutually exclusive, and the matrix is the
// contract: exactly one of prompt / command / call, never two, never none.
func TestUnitBindingMatrix(t *testing.T) {
	agent := Unit{ID: "port-section", Provider: "claude", Model: "sonnet", Prompt: "unit.md"}
	for _, testCase := range []struct {
		name     string
		unit     Unit
		contains string
	}{
		{name: "agent only", unit: agent},
		{name: "command only", unit: Unit{ID: "port-section", Command: "report"}},
		{name: "call only", unit: Unit{ID: "port-section", Call: "child-audit", Args: map[string]string{"subject": "section.path"}}},
		{
			name:     "nothing bound",
			unit:     Unit{ID: "port-section"},
			contains: "an agent unit requires provider, model, and prompt; a tool unit requires a command; a call unit requires call",
		},
		{
			name:     "command and agent",
			unit:     Unit{ID: "port-section", Command: "report", Provider: "claude", Model: "sonnet", Prompt: "unit.md"},
			contains: "a unit declares a command, provider/model/effort/prompt, or call, not more than one",
		},
		{
			name:     "command and effort",
			unit:     Unit{ID: "port-section", Command: "report", Effort: string(EffortHigh)},
			contains: "a unit declares a command, provider/model/effort/prompt, or call, not more than one",
		},
		{
			name:     "call and agent",
			unit:     Unit{ID: "port-section", Call: "child-audit", Provider: "claude", Model: "sonnet", Prompt: "unit.md"},
			contains: "provider/model/effort/prompt is not valid on a call unit",
		},
		{
			name:     "call and effort",
			unit:     Unit{ID: "port-section", Call: "child-audit", Effort: string(EffortHigh)},
			contains: "provider/model/effort/prompt is not valid on a call unit",
		},
		{
			name:     "call and command",
			unit:     Unit{ID: "port-section", Call: "child-audit", Command: "report"},
			contains: "command is not valid on a call unit",
		},
		{
			name:     "call and access",
			unit:     Unit{ID: "port-section", Call: "child-audit", Access: AccessWrite},
			contains: "access is not valid on a call unit",
		},
		{
			name: "call and outputs",
			unit: Unit{ID: "port-section", Call: "child-audit", Outputs: map[string]Variable{
				"ok": {Schema: schema("boolean")},
			}},
			contains: "outputs is not valid on a call unit",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			workflow := dynamicFanOutWorkflow()
			unit := testCase.unit
			workflow.Phases[1].Unit = &unit
			prompts := callFanOutPrompts()
			if unit.Prompt != "" {
				prompts["unit.md"] = "port {{section.path}}"
			}
			if unit.Call != "" && unit.Args == nil {
				// The binding matrix is what is under test, not the argument map; supply
				// the child's required input so its absence cannot mask the finding.
				unit.Args = map[string]string{"subject": "section.path"}
				workflow.Phases[1].Unit = &unit
			}
			result := Validate(
				fanOutFixture(t, workflow, prompts), validBindings(), callsFor(auditChild()),
			)
			if testCase.contains == "" {
				requireValid(t, result)
				return
			}
			requireFinding(t, result, "phase.fan-out-unit", testCase.contains)
		})
	}
}

// args: and max_depth: are call-edge fields. On a unit that runs its own work
// they configure nothing, so they are a finding rather than a silent no-op.
func TestUnitCallFieldsRequireCallBinding(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*Unit)
	}{
		{"args", func(u *Unit) { u.Args = map[string]string{"subject": "section.path"} }},
		{"max depth", func(u *Unit) { u.MaxDepth = 3 }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			workflow := dynamicFanOutWorkflow()
			unit := *workflow.Phases[1].Unit
			testCase.mutate(&unit)
			workflow.Phases[1].Unit = &unit
			result := Validate(
				fanOutFixture(t, workflow, fanOutPrompts()), validBindings(), callsFor(auditChild()),
			)
			requireFinding(t, result, "phase.fan-out-unit", "args and max_depth require call:")
		})
	}
}

func TestCallUnitTargetIsAStaticID(t *testing.T) {
	t.Run("interpolated", func(t *testing.T) {
		workflow := callFanOutWorkflow()
		workflow.Phases[1].Unit.Call = "{{section.path}}"
		result := validateCallFanOut(t, workflow)
		requireFinding(t, result, "phase.fan-out-unit", "never a variable")
	})
	t.Run("negative max depth", func(t *testing.T) {
		workflow := callFanOutWorkflow()
		workflow.Phases[1].Unit.MaxDepth = -1
		requireFinding(t, validateCallFanOut(t, workflow), "phase.fan-out-unit", "max_depth must be >= 1")
	})
	t.Run("unresolvable target", func(t *testing.T) {
		workflow := callFanOutWorkflow()
		workflow.Phases[1].Unit.Call = "missing-child"
		result := validateCallFanOut(t, workflow)
		finding := requireFinding(t, result, "call.target", `call target "missing-child" does not resolve`)
		if !strings.Contains(finding.Element, `fan-out unit "port-section"`) {
			t.Fatalf("a unit edge's finding must name the unit: %s", finding.Element)
		}
	})
	t.Run("no resolver", func(t *testing.T) {
		result := Validate(
			fanOutFixture(t, callFanOutWorkflow(), callFanOutPrompts()), validBindings(), nil,
		)
		requireFinding(t, result, "call.unresolved", "no workflow resolver was supplied")
	})
	t.Run("invalid child", func(t *testing.T) {
		child := auditChild()
		child.Phases[0].Gate.Routes = []Route{{To: "nowhere"}}
		result := validateCallFanOut(t, callFanOutWorkflow(), child)
		requireFinding(t, result, "call.child-invalid", `child workflow "child-audit" fails validation`)
	})
}

// A join's envelope IS the phase's, and every phase-level continuation is a
// continuation of the join's own session, so a call join is refused outright.
func TestCallJoinIsRefused(t *testing.T) {
	workflow := callFanOutWorkflow()
	workflow.Phases[1].Join = &Unit{ID: "merge", Call: "child-audit"}
	result := validateCallFanOut(t, workflow)
	requireFinding(t, result, "phase.fan-out-unit", "call a workflow from a unit instead")
}

// A unit's arguments resolve against exactly what its prompt could reference:
// the phase's declared inputs plus the fan-out element binding.
func TestCallUnitArgsResolveAgainstUnitDeclarations(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		args     map[string]string
		mutate   func(*Workflow)
		code     string
		contains string
	}{
		{
			name: "element binding", args: map[string]string{"subject": "section.path"},
		},
		{
			name: "phase input", args: map[string]string{"subject": "plan.title"},
			mutate: func(w *Workflow) {
				w.Phases[0].Outputs["title"] = Variable{Schema: schema("string")}
				w.Phases[1].Inputs["plan.title"] = Variable{Schema: schema("string")}
			},
		},
		{
			name: "unknown child input",
			args: map[string]string{"subject": "section.path", "extra": "section.path"},
			code: "call.arg", contains: `declares no input "extra"`,
		},
		{
			name: "missing required child input", args: map[string]string{},
			code: "call.args", contains: `requires input "subject"`,
		},
		{
			name: "reference outside the unit's scope", args: map[string]string{"subject": "plan.sections"},
			code: "call.arg-type", contains: `is "array" but child input "subject" is "string"`,
		},
		{
			name: "undeclared reference", args: map[string]string{"subject": "section.missing"},
			code: "call.arg-ref", contains: "is not declared by phase inputs or the fan-out element binding",
		},
		{
			name: "phase output is not in a unit's scope", args: map[string]string{"subject": "plan.title"},
			mutate: func(w *Workflow) {
				// Declared by the producing phase but never taken as a phase input, so
				// no unit prompt could reference it either.
				w.Phases[0].Outputs["title"] = Variable{Schema: schema("string")}
			},
			code: "call.arg-ref", contains: `reference "plan.title"`,
		},
		{
			name: "type mismatch", args: map[string]string{"subject": "plan.count"},
			mutate: func(w *Workflow) {
				w.Phases[0].Outputs["count"] = Variable{Schema: schema("number")}
				w.Phases[1].Inputs["plan.count"] = Variable{Schema: schema("number")}
			},
			code: "call.arg-type", contains: `is "number" but child input "subject" is "string"`,
		},
		{
			name: "optional producer for a required input", args: map[string]string{"subject": "plan.title"},
			mutate: func(w *Workflow) {
				w.Phases[0].Outputs["title"] = Variable{Schema: schema("string"), Optional: true}
				w.Phases[1].Inputs["plan.title"] = Variable{Schema: schema("string"), Optional: true}
			},
			code: "call.arg-optionality", contains: `cannot satisfy required child input "subject"`,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			workflow := callFanOutWorkflow()
			workflow.Phases[1].Unit.Args = testCase.args
			if testCase.mutate != nil {
				testCase.mutate(&workflow)
			}
			result := validateCallFanOut(t, workflow)
			if testCase.code == "" {
				requireValid(t, result)
				return
			}
			finding := requireFinding(t, result, testCase.code, testCase.contains)
			if !strings.Contains(finding.Element, `fan-out unit "port-section"`) {
				t.Fatalf("a unit argument finding must name the unit: %s", finding.Element)
			}
		})
	}
}

// A unit edge closes a call cycle exactly as a phase edge does — a campaign
// whose units call the campaign back would otherwise recurse with nothing but
// the engine's absolute bound behind it.
func TestCallUnitCyclesRequireDeclaredMaxDepth(t *testing.T) {
	// port -> child-audit -> port: the closing edge is the child's call phase.
	recursiveChild := func() Workflow {
		child := auditChild()
		child.Phases = []Phase{
			checkPhase("inspect", map[string]Variable{
				"ok":    {Schema: schema("boolean")},
				"notes": {Schema: schema("string")},
			}, Route{To: "again"}),
			callPhase("again", "port", map[string]string{"goal": "inspect.notes"}, Route{To: "done"}),
		}
		return child
	}
	t.Run("unbounded through a unit edge", func(t *testing.T) {
		result := validateCallFanOut(t, callFanOutWorkflow(), recursiveChild(), callFanOutWorkflow())
		finding := requireFinding(t, result, "call.unbounded-cycle", "declare max_depth on this call edge")
		if !strings.Contains(finding.Message, "port -> child-audit -> port") {
			t.Fatalf("cycle finding must name the cycle: %s", finding.Message)
		}
	})
	t.Run("bounded", func(t *testing.T) {
		child := recursiveChild()
		child.Phases[1].MaxDepth = 3
		requireValid(t, validateCallFanOut(t, callFanOutWorkflow(), child, callFanOutWorkflow()))
	})
	t.Run("self-calling unit is unbounded", func(t *testing.T) {
		// The campaign shape: each unit calls the campaign itself.
		workflow := callFanOutWorkflow()
		workflow.Phases[1].Unit.Call = "port"
		workflow.Phases[1].Unit.Args = map[string]string{"goal": "section.path"}
		result := Validate(
			fanOutFixture(t, workflow, callFanOutPrompts()), validBindings(), callsFor(workflow),
		)
		finding := requireFinding(t, result, "call.unbounded-cycle", "port -> port")
		if !strings.Contains(finding.Element, `fan-out unit "port-section"`) {
			t.Fatalf("the closing edge is the unit: %s", finding.Element)
		}
	})
	t.Run("self-calling unit bounded by its own max_depth", func(t *testing.T) {
		workflow := callFanOutWorkflow()
		workflow.Phases[1].Unit.Call = "port"
		workflow.Phases[1].Unit.Args = map[string]string{"goal": "section.path"}
		workflow.Phases[1].Unit.MaxDepth = 100
		requireValid(t, Validate(
			fanOutFixture(t, workflow, callFanOutPrompts()), validBindings(), callsFor(workflow),
		))
	})
}

// A unit call is a call edge for every derived answer, not only for validation:
// the workspace need has to follow it, or a fan-out of writing sub-workflows
// would run with no worktree to cut sub-worktrees from.
func TestUnitCallEdgesArePartOfTheCallGraph(t *testing.T) {
	workflow := callFanOutWorkflow()
	calls := callsFor(auditChild())
	targets := CallTargets(workflow)
	if strings.Join(targets, ",") != "child-audit" {
		t.Fatalf("call targets = %v", targets)
	}
	need, err := PropagatedWorkspaceNeed(workflow, calls)
	if err != nil {
		t.Fatal(err)
	}
	// The fan-out template writes nothing itself, so only the call edge can carry
	// a writing need here — and this child is read-only.
	if need != WorkspaceProjectRoot {
		t.Fatalf("read-only child propagates need %q", need)
	}
	writingChild := auditChild()
	writingChild.Phases[0] = Phase{
		ID: "inspect", Driver: DriverAgent, Provider: "codex", Model: "m", Prompt: "e.md",
		Access: AccessWrite, Outputs: map[string]Variable{
			"ok": {Schema: schema("boolean")}, "notes": {Schema: schema("string")},
		},
		Gate: Gate{Routes: []Route{{To: "done"}}},
	}
	need, err = PropagatedWorkspaceNeed(workflow, callsFor(writingChild))
	if err != nil {
		t.Fatal(err)
	}
	if need != WorkspaceWorktree {
		t.Fatalf("a unit calling a writing workflow needs a worktree, got %q", need)
	}
}

// A call unit holds no session, so it contributes nothing to the phase-level
// "does anything under this phase run an agent" question that grants depend on.
func TestGrantsFollowTheUnitsThatRunAgents(t *testing.T) {
	workflow := callFanOutWorkflow()
	workflow.Phases[1].Grants = []string{string(GrantStartRun)}
	// The join is an agent, so the phase still holds a session.
	requireValid(t, validateCallFanOut(t, workflow))

	workflow.Phases[1].Join = &Unit{ID: "merge", Command: "merge"}
	result := validateCallFanOut(t, workflow)
	requireFinding(t, result, "phase.grants", "")
}
