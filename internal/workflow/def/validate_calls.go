package def

import (
	"fmt"
	"sort"
	"strings"
)

// maxReportedChildFindings bounds how much of a child's own dry-run is quoted
// on the call edge. The child validates on its own and reports everything; the
// parent's finding exists to say which edge is broken, not to mirror a whole
// second workflow's diagnostics.
const maxReportedChildFindings = 3

// validateCall enforces the call phase's authoring shape. A call phase runs no
// work of its own — it invokes another workflow and adopts that run's declared
// outputs as its envelope — so every field that configures work is refused
// here rather than silently ignored at run time.
func validateCall(phase Phase, phaseElement string) []Finding {
	var findings []Finding
	add := func(code, message string) {
		findings = append(findings, finding(code, phaseElement, message))
	}
	if !phase.IsCall() {
		if phase.CallTarget() != "" || len(phase.Args) > 0 || phase.MaxDepth != 0 {
			add("phase.call", "call, args, and max_depth require shape: call")
		}
		return findings
	}
	target := phase.CallTarget()
	switch {
	case target == "":
		add("phase.call", "call shape requires call: the id of the workflow to invoke")
	case !idPattern.MatchString(target):
		add("phase.call", fmt.Sprintf("call target %q must match [a-z0-9-]+; a call names a static workflow id, never a variable", target))
	}
	if phase.MaxDepth < 0 {
		add("phase.call", "max_depth must be >= 1")
	}
	// One message per forbidden group, naming the fields the author actually
	// wrote, so a miswired call phase reads as one problem instead of a wall.
	forbidden := []struct {
		fields  []string
		present bool
		message string
	}{
		{[]string{"driver"}, phase.Driver != "",
			"a call phase runs no work of its own; the child workflow's phases carry the drivers"},
		{[]string{"provider", "model", "prompt"}, phase.Provider != "" || phase.Model != "" || phase.Prompt != "",
			"a call phase runs no turn of its own; the child workflow's phases declare their own provider, model, and prompt"},
		{[]string{"check", "command", "commands"}, phase.Check != "" || phase.Command != "" || len(phase.Commands) > 0,
			"a call phase runs no command of its own; bind commands in the child workflow's phases"},
		{[]string{"resources"}, len(phase.Resources) > 0,
			"a call phase holds no resources; the child's phases acquire what they need"},
		{[]string{"capabilities", "mcp"}, len(phase.Capabilities) > 0 || len(phase.MCP) > 0,
			"a call phase grants nothing; the child workflow's phases declare their own capabilities and mcp servers"},
		{[]string{"access"}, phase.Access != "",
			"a call phase touches no workspace; access is declared by the child workflow's phases, and the child executes in the caller's workspace"},
		{[]string{"watchdog"}, phase.Watchdog != "",
			"a call phase runs no turn to watch; the child's phases carry their own watchdogs"},
		{[]string{"inputs"}, len(phase.Inputs) > 0,
			"a call phase's inputs are its args:, which map the child workflow's declared inputs"},
		{[]string{"outputs"}, len(phase.Outputs) > 0,
			"a call phase's outputs are the child workflow's declared outputs:; remove the declaration"},
		{[]string{"fan_out", "over", "as", "unit", "join"}, len(phase.FanOut) > 0 || phase.DynamicFanOut() || phase.Join != nil,
			"a call phase has no units; fan out inside the child workflow instead"},
	}
	for _, group := range forbidden {
		if group.present {
			add("phase.call", fmt.Sprintf("%s is not valid on a call phase: %s", strings.Join(group.fields, "/"), group.message))
		}
	}
	return findings
}

// callValidation carries the traversal state of one dry-run across the call
// graph: which workflows have already been validated (so a diamond validates
// each one once) and which are on the current stack (so a cycle terminates and
// can be named in its finding).
type callValidation struct {
	results  map[string]ValidationResult
	visiting map[string]bool
	stack    []string
	// cycles collects unbounded-cycle findings for the whole traversal. Each
	// workflow is validated at most once per dry-run, so each closing edge
	// contributes at most one finding and no deduplication is needed.
	cycles []Finding
}

func newCallValidation(rootID string) *callValidation {
	return &callValidation{
		results:  make(map[string]ValidationResult),
		visiting: map[string]bool{rootID: true},
		stack:    []string{rootID},
	}
}

// cycleFrom renders the call cycle the supplied target closes, from the point
// the target first appears on the stack through the edge that closes it.
func (v *callValidation) cycleFrom(target string) string {
	for index, id := range v.stack {
		if id == target {
			return strings.Join(append(append([]string(nil), v.stack[index:]...), target), " -> ")
		}
	}
	return target + " -> " + target
}

// validateCallGraph resolves every call edge of one workflow and returns both
// the findings and the *effective* workflow the rest of validation runs
// against: one whose call phases carry the outputs their child declares, so a
// parent consumer of `call-phase.output` type-checks against the child's real
// contract instead of an empty one.
func validateCallGraph(
	workflow Workflow, phaseIndex map[string]int, graph workflowGraph,
	bindings Bindings, calls CallResolver, state *callValidation,
) ([]Finding, Workflow) {
	effective := workflow
	// The caller's definition is never mutated: the phase slice is copied the
	// first time a resolved child contributes outputs, and not at all otherwise.
	copiedPhases := false
	var findings []Finding
	for index, phase := range workflow.Phases {
		if !phase.IsCall() {
			continue
		}
		target := phase.CallTarget()
		if target == "" || !idPattern.MatchString(target) {
			continue // validateCall already reported the target itself.
		}
		element := fmt.Sprintf("workflow %q phase %q", workflow.ID, phase.ID)
		if calls == nil {
			// A workflow with call edges cannot be dry-run without resolution: its
			// arguments, its child's validity, and its cycles are all facts about
			// definitions this validation cannot see. Reporting it beats calling a
			// call graph valid on the strength of never having looked at it.
			findings = append(findings, finding("call.unresolved", element,
				fmt.Sprintf("call target %q cannot be checked: no workflow resolver was supplied", target)))
			continue
		}
		resolved, err := calls.ResolveCall(target)
		if err != nil {
			findings = append(findings, finding("call.target", element,
				fmt.Sprintf("call target %q does not resolve: %v", target, err)))
			continue
		}
		child := resolved.Workflow
		if state.visiting[target] {
			// This edge closes a cycle. Recursion is allowed and bounded: the edge
			// that closes the cycle is the one that has to declare its ceiling.
			if phase.MaxDepth < 1 {
				state.cycles = append(state.cycles, finding("call.unbounded-cycle", element, fmt.Sprintf(
					"call to %q closes the call cycle %s; declare max_depth on this call edge",
					target, state.cycleFrom(target),
				)))
			}
		} else {
			childResult, validated := state.results[target]
			if !validated {
				state.visiting[target] = true
				state.stack = append(state.stack, target)
				childResult = validateWorkflow(resolved, bindings, calls, state)
				state.stack = state.stack[:len(state.stack)-1]
				delete(state.visiting, target)
				state.results[target] = childResult
			}
			if !childResult.Valid() {
				findings = append(findings, finding("call.child-invalid", element,
					fmt.Sprintf("child workflow %q fails validation: %s", target, summarizeFindings(childResult.Findings))))
			}
		}
		findings = append(findings, validateCallArgs(workflow, phaseIndex, graph, index, phase, child, element)...)
		if outputs := CallPhaseOutputs(child); len(outputs) > 0 {
			if !copiedPhases {
				effective.Phases = append([]Phase(nil), workflow.Phases...)
				copiedPhases = true
			}
			effective.Phases[index].Outputs = outputs
		}
	}
	return findings, effective
}

// validateCallArgs checks the argument map against the child's declared inputs:
// every required child input is supplied, every supplied name exists, and every
// reference resolves in the caller with a producer that dominates the call and a
// type the child accepts. Arguments are the only way a value crosses into a
// child run, so an unchecked one would fail mid-run instead of at the dry-run.
func validateCallArgs(
	workflow Workflow, phaseIndex map[string]int, graph workflowGraph,
	consumerIndex int, phase Phase, child Workflow, element string,
) []Finding {
	var findings []Finding
	names := make([]string, 0, len(phase.Args))
	for name := range phase.Args {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		ref := phase.Args[name]
		argElement := fmt.Sprintf("%s arg %q", element, name)
		input, declared := child.Inputs[name]
		if !declared {
			findings = append(findings, finding("call.arg", argElement,
				fmt.Sprintf("child workflow %q declares no input %q", child.ID, name)))
			continue
		}
		producer, producerIndex, ok := resolveReference(workflow, phaseIndex, ref)
		if !ok {
			findings = append(findings, finding("call.arg-ref", argElement,
				fmt.Sprintf("reference %q does not resolve", ref)))
			continue
		}
		if producerIndex >= 0 && (producerIndex == consumerIndex || !graph.dominators[consumerIndex][producerIndex]) {
			findings = append(findings, finding("call.arg-dominance", argElement,
				fmt.Sprintf("producer phase %q does not dominate phase %q", workflow.Phases[producerIndex].ID, phase.ID)))
		}
		if producer.Optional && !input.Optional {
			findings = append(findings, finding("call.arg-optionality", argElement,
				fmt.Sprintf("optional producer %q cannot satisfy required child input %q", ref, name)))
		}
		if !schemasCompatible(producer.Schema, input.Schema) {
			findings = append(findings, finding("call.arg-type", argElement,
				fmt.Sprintf("reference %q is %q but child input %q is %q", ref, producer.Schema.Type, name, input.Schema.Type)))
		}
	}
	required := make([]string, 0, len(child.Inputs))
	for name, input := range child.Inputs {
		if _, supplied := phase.Args[name]; !supplied && !input.Optional {
			required = append(required, name)
		}
	}
	sort.Strings(required)
	for _, name := range required {
		findings = append(findings, finding("call.args", element,
			fmt.Sprintf("child workflow %q requires input %q; add it to args", child.ID, name)))
	}
	return findings
}

func summarizeFindings(findings []Finding) string {
	shown := findings
	suffix := ""
	if len(shown) > maxReportedChildFindings {
		shown = shown[:maxReportedChildFindings]
		suffix = fmt.Sprintf(" (and %d more)", len(findings)-maxReportedChildFindings)
	}
	messages := make([]string, 0, len(shown))
	for _, item := range shown {
		messages = append(messages, item.Error())
	}
	return strings.Join(messages, "; ") + suffix
}
