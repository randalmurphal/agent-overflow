package def

import (
	"strings"
	"testing"
)

// A REQUIRED workflow output is synthesized when the run completes, and a name
// with no value fails the run right there. The dry-run's job is to say so
// before the run rather than at the moment it succeeds.

// branchingOutputFixture is a graph with two ways out: `plan` can end the run
// directly (the "complete" exit), or route through `wrap-up` first. Only
// `wrap-up` produces `handoff`, which is the incident shape exactly.
func branchingOutputFixture(t *testing.T, handoff WorkflowOutput) ResolvedWorkflow {
	t.Helper()
	resolved := writableValidFixture(t)
	writeSibling(t, resolved, "plan.md", "Plan {{goal}}\n")
	writeSibling(t, resolved, "wrap-up.md", "Wrap up\n")
	resolved.Workflow.Outputs = map[string]WorkflowOutput{
		"approach": {From: "plan.approach"},
		"handoff":  handoff,
	}
	resolved.Workflow.Phases = []Phase{
		{
			ID: "plan", Driver: DriverAgent, Provider: "codex", Model: "test-model", Prompt: "plan.md",
			Inputs:  map[string]Variable{"goal": {Schema: JSONSchema{Type: "string"}}},
			Outputs: map[string]Variable{"approach": {Schema: JSONSchema{Type: "string"}}, "complete": {Schema: JSONSchema{Type: "boolean"}}},
			Gate: Gate{Routes: []Route{
				{When: &Predicate{Eq: &Comparison{Ref: "plan.complete", Value: true}}, To: "done"},
				{To: "wrap-up"},
			}},
		},
		{
			ID: "wrap-up", Driver: DriverAgent, Provider: "codex", Model: "test-model", Prompt: "wrap-up.md",
			Outputs: map[string]Variable{"notes": {Schema: JSONSchema{Type: "string"}}},
			Gate:    Gate{Routes: []Route{{To: "done"}}},
		},
	}
	return resolved
}

func TestRequiredOutputOffACompletionPathIsRefusedWithTheWitnessPath(t *testing.T) {
	resolved := branchingOutputFixture(t, WorkflowOutput{From: "wrap-up.notes"})
	result := Validate(resolved, nil, nil)
	if !hasFinding(result.Findings, "workflow.output-unreachable", `output "handoff"`) {
		t.Fatalf("a required output off the completion path was accepted:\n%s", formatFindings(result.Findings))
	}
	var message string
	for _, finding := range result.Findings {
		if finding.Code == "workflow.output-unreachable" {
			message = finding.Message
		}
	}
	// The witness is the point: "not on every path" is not something an author
	// can see by reading one gate.
	if !strings.Contains(message, "plan -> done") {
		t.Fatalf("finding names no completion path that misses the producer: %q", message)
	}
	if !strings.Contains(message, `phase "wrap-up"`) {
		t.Fatalf("finding does not name the producing phase: %q", message)
	}
	// The output that IS produced on every path is not reported: only one
	// declaration is wrong here.
	if hasFinding(result.Findings, "workflow.output-unreachable", `output "approach"`) {
		t.Fatalf("an output produced on every path was reported:\n%s", formatFindings(result.Findings))
	}
}

func TestOptionalOutputOffACompletionPathIsAccepted(t *testing.T) {
	resolved := branchingOutputFixture(t, WorkflowOutput{From: "wrap-up.notes", Optional: true})
	result := Validate(resolved, nil, nil)
	if !result.Valid() {
		t.Fatalf("an optional output off the completion path was refused:\n%s", formatFindings(result.Findings))
	}
}

// D44 strictness is untouched where it was already right: a required output
// whose producer runs on every completion path stays exactly as strict.
func TestRequiredOutputOnEveryCompletionPathStaysClean(t *testing.T) {
	resolved := branchingOutputFixture(t, WorkflowOutput{From: "plan.approach"})
	result := Validate(resolved, nil, nil)
	if !result.Valid() {
		t.Fatalf("a required output produced on every path was refused:\n%s", formatFindings(result.Findings))
	}
	// And the shipped fixture, whose only output comes from a mid-graph phase
	// every path runs through, is still clean.
	if result := Validate(writableValidFixture(t), validBindings(), nil); !result.Valid() {
		t.Fatalf("the valid fixture regressed:\n%s", formatFindings(result.Findings))
	}
}

// A human `approve: done` is a completion path like any other, so a producer
// that runs only on the other branch is off it.
func TestHumanApproveToDoneCountsAsACompletionPath(t *testing.T) {
	resolved := branchingOutputFixture(t, WorkflowOutput{From: "wrap-up.notes"})
	writeSibling(t, resolved, "sign-off.md", "Sign off\n")
	resolved.Workflow.Phases[0].Gate.Routes = []Route{
		{When: &Predicate{Eq: &Comparison{Ref: "plan.complete", Value: true}}, To: "sign-off"},
		{To: "wrap-up"},
	}
	resolved.Workflow.Phases = append(resolved.Workflow.Phases, Phase{
		ID: "sign-off", Driver: DriverAgent, Provider: "codex", Model: "test-model", Prompt: "sign-off.md",
		Gate: Gate{Routes: []Route{
			{Human: &HumanRoute{Approve: "done", Reject: &LoopTarget{Loop: "plan", Max: LiteralBound(2)}}},
		}},
	})
	result := Validate(resolved, nil, nil)
	if !hasFinding(result.Findings, "workflow.output-unreachable", `output "handoff"`) {
		t.Fatalf("a human approve-to-done exit was not read as a completion path:\n%s", formatFindings(result.Findings))
	}
	var message string
	for _, finding := range result.Findings {
		if finding.Code == "workflow.output-unreachable" {
			message = finding.Message
		}
	}
	if !strings.Contains(message, "plan -> sign-off -> done") {
		t.Fatalf("witness path does not run through the human gate: %q", message)
	}
}

// An unreachable producer is already `graph.unreachable`'s to blame. Reporting
// the output too would name one mistake twice and point at the wrong line.
func TestUnreachableProducerIsNotAlsoReportedAsAnUnreachableOutput(t *testing.T) {
	resolved := branchingOutputFixture(t, WorkflowOutput{From: "wrap-up.notes"})
	resolved.Workflow.Phases[0].Gate.Routes = []Route{{To: "done"}}
	result := Validate(resolved, nil, nil)
	if !hasFinding(result.Findings, "graph.unreachable", `phase "wrap-up"`) {
		t.Fatalf("expected the unreachable phase to be reported:\n%s", formatFindings(result.Findings))
	}
	if hasFinding(result.Findings, "workflow.output-unreachable", `output "handoff"`) {
		t.Fatalf("an unreachable phase was blamed twice:\n%s", formatFindings(result.Findings))
	}
}

// The published authoring schema and the decoder have to agree that a workflow
// output may declare `optional:`.
func TestWorkflowOutputOptionalParsesAndIsPublished(t *testing.T) {
	workflow, err := ParseBytes([]byte(`
id: campaign
name: Campaign
outputs:
  handoff:
    from: wrap-up.notes
    optional: true
phases:
  - id: wrap-up
    driver: tool
    check: test
    outputs:
      notes:
        schema:
          type: string
    gate:
      routes:
        - to: done
`))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if !workflow.Outputs["handoff"].Optional {
		t.Fatal("optional: true did not decode onto the workflow output")
	}
	if !strings.Contains(string(AuthoringSchema()), "workflow.output-unreachable") {
		t.Fatal("the published authoring schema does not document the optional-output rule")
	}
}
