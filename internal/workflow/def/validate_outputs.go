package def

import (
	"fmt"
	"sort"
	"strings"
)

// validateWorkflowOutputs checks the run's declared deliverables: that each
// names a resolvable phase output, that an artifact source is a path, and —
// for a REQUIRED one — that the phase producing it runs on every path that can
// reach `done`.
func validateWorkflowOutputs(workflow Workflow, phaseIndex map[string]int, graph workflowGraph) []Finding {
	var findings []Finding
	exits := doneExitPhases(workflow)
	names := make([]string, 0, len(workflow.Outputs))
	for name := range workflow.Outputs {
		names = append(names, name)
	}
	// Sorted so two runs of the same dry-run report the same findings in the same
	// order; the whole result is re-sorted by element, but the witness path a
	// finding quotes is chosen per output and must not vary between runs.
	sort.Strings(names)
	for _, name := range names {
		output := workflow.Outputs[name]
		element := fmt.Sprintf("workflow %q output %q", workflow.ID, name)
		if !idPattern.MatchString(name) {
			findings = append(findings, finding("workflow-output.name", element, "name must match [a-z0-9-]+"))
		}
		parts := strings.Split(output.From, ".")
		resolved, producer, ok := resolveReference(workflow, phaseIndex, output.From)
		if !ok || producer < 0 || len(parts) < 2 {
			findings = append(findings, finding("workflow-output.ref", element, fmt.Sprintf("source %q does not resolve", output.From)))
			continue
		}
		if output.Artifact && resolved.Schema.Type != "string" {
			findings = append(findings, finding("workflow-output.artifact-type", element, "artifact source must resolve to a string path"))
		}
		findings = append(findings, outputReachabilityFindings(workflow, graph, exits, element, output, producer)...)
	}
	return findings
}

// outputReachabilityFindings is the dry-run half of the completion contract a
// run is held to at the moment it finishes.
//
// A required workflow output is synthesized from the producing phase's outputs
// when the run completes, and a name with no value is a hard failure there
// (`childOutputEnvelope`, `internal/workflow/engine`). That strictness is right
// — a caller routes on these names — but it is checkable, and until it was
// checked the failure arrived at the worst possible moment: a campaign whose
// planner declared `complete` exited through a route that never entered the
// phase sourcing its per-wave handoff, and the tree died at the exact
// transition that meant it had succeeded.
//
// The rule is "on every path that reaches done", not "reachable": a producer
// that runs on one branch and not another is exactly the defect. The witness is
// one such path, printed, because "not on every path" is not something an author
// can check by reading a gate — the routes that skip the phase can be several
// phases apart.
func outputReachabilityFindings(
	workflow Workflow, graph workflowGraph, exits []bool,
	element string, output WorkflowOutput, producer int,
) []Finding {
	if output.Optional {
		// Declared absent-able: an unproduced value is the run the author asked
		// for, and the engine omits the name instead of failing.
		return nil
	}
	if producer >= len(graph.reachable) || !graph.reachable[producer] {
		// `graph.unreachable` already blames the phase. Reporting the output too
		// would name one mistake twice and point at the wrong line to fix.
		return nil
	}
	path, missed := completionPathAvoiding(workflow, graph, exits, producer)
	if !missed {
		return nil
	}
	return []Finding{finding("workflow.output-unreachable", element, fmt.Sprintf(
		"required output is sourced from phase %q, which is not on every path to done: %s. "+
			"Declare `optional: true` if this deliverable is genuinely absent on some completion path, "+
			"or route every path to done through %q — a required output the run did not produce fails the run as it completes.",
		workflow.Phases[producer].ID, path, workflow.Phases[producer].ID))}
}

// doneExitPhases marks every phase whose gate can end the run at `done`. A
// `failed` route is not a completion, and a `park:` route is a rest a human
// resumes from rather than a way out of the graph.
func doneExitPhases(workflow Workflow) []bool {
	exits := make([]bool, len(workflow.Phases))
	for index, phase := range workflow.Phases {
		for _, route := range phase.Gate.Routes {
			if route.To == "done" || (route.Human != nil && route.Human.Approve == "done") {
				exits[index] = true
				break
			}
		}
	}
	return exits
}

// completionPathAvoiding finds one path from the first phase to a `done` exit
// that never enters `avoid`, rendered as `a -> b -> done`. It walks the FORWARD
// edges only: loop routes re-enter a strict ancestor (`gate.loop-ancestor`), so
// a lap adds no phase a forward path did not already visit, and including them
// would only produce a longer witness for the same defect.
func completionPathAvoiding(workflow Workflow, graph workflowGraph, exits []bool, avoid int) (string, bool) {
	if len(workflow.Phases) == 0 || avoid == 0 {
		// The first phase runs on every path there is.
		return "", false
	}
	visited := make([]bool, len(workflow.Phases))
	var trail []int
	var walk func(node int) bool
	walk = func(node int) bool {
		if node == avoid || visited[node] {
			return false
		}
		visited[node] = true
		trail = append(trail, node)
		if exits[node] {
			return true
		}
		for _, next := range graph.forward[node] {
			if walk(next) {
				return true
			}
		}
		trail = trail[:len(trail)-1]
		return false
	}
	if !walk(0) {
		return "", false
	}
	names := make([]string, 0, len(trail)+1)
	for _, node := range trail {
		names = append(names, workflow.Phases[node].ID)
	}
	names = append(names, "done")
	return strings.Join(names, " -> "), true
}
