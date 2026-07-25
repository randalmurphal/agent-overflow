package def

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Validate performs a complete dry-run over a resolved workflow, including the
// workflows it reaches through call edges. `calls` resolves those targets under
// §8 scoping; a definition with call phases cannot be dry-run without one, and
// says so rather than passing unchecked.
func Validate(resolved ResolvedWorkflow, bindings Bindings, calls CallResolver) ValidationResult {
	state := newCallValidation(resolved.Workflow.ID)
	result := validateWorkflow(resolved, bindings, calls, state)
	// An unbounded cycle is a property of the call graph rather than of any one
	// definition — the edge that closes it can sit several levels down, and the
	// workflow holding that edge is perfectly valid read on its own. Collecting
	// cycles during the traversal and reporting them on the graph's own result
	// means a caller always sees `call.unbounded-cycle`, whatever it entered at.
	result.Findings = append(result.Findings, state.cycles...)
	return result
}

// validateWorkflow is the per-definition dry-run. Call-graph traversal state is
// threaded through so a child validates exactly once per dry-run and a cycle
// terminates instead of recursing forever.
func validateWorkflow(resolved ResolvedWorkflow, bindings Bindings, calls CallResolver, state *callValidation) ValidationResult {
	workflow := resolved.Workflow
	status := BindingsChecked
	if bindings == nil {
		status = BindingsUnchecked
	}
	result := ValidationResult{BindingStatus: status}
	add := func(findings ...Finding) { result.Findings = append(result.Findings, findings...) }
	maxFanOutWidth := EffectiveMaxFanOutWidth(bindings)
	element := "workflow " + quoted(workflow.ID)
	if !idPattern.MatchString(workflow.ID) {
		add(finding("workflow.id", element, "id must match [a-z0-9-]+"))
	}
	if strings.TrimSpace(workflow.Name) == "" {
		add(finding("workflow.name", element, "name is required"))
	}
	if len(workflow.Phases) == 0 {
		add(finding("workflow.phases", element, "at least one phase is required"))
	}
	if workflow.Cleanup != "" && workflow.Cleanup != CleanupManual && workflow.Cleanup != CleanupAuto {
		add(finding("workflow.cleanup", element, fmt.Sprintf("cleanup must be %q or %q", CleanupManual, CleanupAuto)))
	}
	phaseIndex := make(map[string]int, len(workflow.Phases))
	for name, input := range workflow.Inputs {
		inputElement := fmt.Sprintf("%s input %q", element, name)
		if !idPattern.MatchString(name) {
			add(finding("input.name", inputElement, "name must match [a-z0-9-]+"))
		}
		add(validateSchemaDefinition(input.Schema, inputElement)...)
	}
	for i, phase := range workflow.Phases {
		phaseElement := fmt.Sprintf("%s phase %q", element, phase.ID)
		if !idPattern.MatchString(phase.ID) {
			add(finding("phase.id", phaseElement, "id must match [a-z0-9-]+"))
		}
		if phase.ID == "done" || phase.ID == "failed" {
			add(finding("phase.id", phaseElement, "done and failed are reserved terminal names"))
		}
		if _, collision := workflow.Inputs[phase.ID]; collision {
			add(finding("namespace.collision", phaseElement, fmt.Sprintf("phase id collides with workflow input %q", phase.ID)))
		}
		if prior, duplicate := phaseIndex[phase.ID]; duplicate {
			add(finding("phase.duplicate", phaseElement, fmt.Sprintf("id duplicates phase at index %d", prior)))
		} else {
			phaseIndex[phase.ID] = i
		}
		shape := phase.EffectiveShape()
		if shape != ShapeSingle && shape != ShapeFanOut && shape != ShapeCall {
			add(finding("phase.shape", phaseElement, "shape must be single, fan-out, or call"))
		}
		add(validateCall(phase, phaseElement)...)
		if shape == ShapeCall {
			// Everything below configures work of the phase's own, which a call
			// phase never does; validateCall refused each field it declared, and
			// its contract is checked against the child in validateCallGraph.
			continue
		}
		if shape != ShapeFanOut {
			// A fan-out phase runs no turn or command of its own either — its
			// units and join do — so validateFanOut refuses those fields instead
			// of checking them here.
			add(validatePhaseExecution(phase, phaseElement)...)
		}
		add(validateGrants(phase, phaseElement)...)
		add(validateFanOut(workflow, phase, phaseElement, maxFanOutWidth)...)
		if phase.Watchdog != "" {
			duration, err := time.ParseDuration(phase.Watchdog)
			if err != nil {
				add(finding("phase.watchdog", phaseElement, "watchdog must be a time.ParseDuration-compatible string"))
			} else if duration <= 0 {
				add(finding("phase.watchdog", phaseElement, "watchdog must be greater than 0"))
			}
		}
		for name, output := range phase.Outputs {
			outputElement := fmt.Sprintf("%s output %q", phaseElement, name)
			if !idPattern.MatchString(name) {
				add(finding("output.name", outputElement, "name must match [a-z0-9-]+"))
			}
			if PhaseProducesToolEnvelope(phase) && ReservedToolOutput(name) {
				add(finding("output.reserved", outputElement, "the tool driver always supplies this output; remove the declaration"))
			}
			add(validateSchemaDefinition(output.Schema, outputElement)...)
		}
	}
	graph := buildGraph(workflow, phaseIndex, &result.Findings)
	// Every reference check below runs against the effective workflow: a call
	// phase's downstream surface is the child's declared outputs, so consumers
	// of `call-phase.output` resolve and type-check against the child's real
	// contract rather than against a phase that declares nothing.
	callFindings, effective := validateCallGraph(workflow, phaseIndex, graph, bindings, calls, state)
	add(callFindings...)
	add(validateVariables(effective, phaseIndex, graph)...)
	add(validateWorkflowOutputs(effective, phaseIndex)...)
	add(validatePrompts(resolved)...)
	add(validateBindings(workflow, bindings)...)
	result.Reports = fanOutWidthReports(workflow, bindings)
	sort.SliceStable(result.Findings, func(i, j int) bool {
		if result.Findings[i].Element == result.Findings[j].Element {
			return result.Findings[i].Code < result.Findings[j].Code
		}
		return result.Findings[i].Element < result.Findings[j].Element
	})
	return result
}

func finding(code, element, message string) Finding {
	return Finding{Code: code, Element: element, Message: message}
}

// validatePhaseExecution checks the fields that configure a phase's own turn or
// command. Only a single-shape phase runs one: a call phase delegates to its
// child workflow and a fan-out delegates to its units and its join, and both
// refuse these fields outright (validateCall / validateFanOut) rather than
// checking a declaration that would never execute.
func validatePhaseExecution(phase Phase, phaseElement string) []Finding {
	var findings []Finding
	add := func(code, message string) {
		findings = append(findings, finding(code, phaseElement, message))
	}
	if phase.Driver != DriverAgent && phase.Driver != DriverTool {
		add("phase.driver", "driver must be agent or tool")
	}
	if phase.Access != "" && phase.Access != AccessReadOnly && phase.Access != AccessWrite {
		add("phase.access", "access must be read-only or write")
	}
	if phase.Driver == DriverAgent && phase.Prompt == "" {
		add("phase.prompt", "agent driver requires a prompt file")
	}
	if phase.Driver == DriverAgent && (strings.TrimSpace(phase.Provider) == "" || strings.TrimSpace(phase.Model) == "") {
		add("phase.model", "agent driver requires provider and model")
	}
	if phase.Driver == DriverTool && phase.Check == "" && phase.Command == "" {
		add("phase.tool", "tool driver requires a check or command binding")
	}
	// One phase, one command. Two bindings have no defined precedence, and
	// picking one at run time would make the phase's behaviour depend on
	// resolution order rather than on the definition.
	if phase.Driver == DriverTool && phase.Check != "" && phase.Command != "" {
		add("phase.tool", "tool driver accepts a check or a command binding, not both")
	}
	return findings
}

func quoted(value string) string { return fmt.Sprintf("%q", value) }

func validateWorkflowOutputs(workflow Workflow, phaseIndex map[string]int) []Finding {
	var findings []Finding
	for name, output := range workflow.Outputs {
		element := fmt.Sprintf("workflow %q output %q", workflow.ID, name)
		if !idPattern.MatchString(name) {
			findings = append(findings, finding("workflow-output.name", element, "name must match [a-z0-9-]+"))
		}
		parts := strings.Split(output.From, ".")
		resolved, producer, ok := resolveReference(workflow, phaseIndex, output.From)
		if !ok || producer < 0 || len(parts) < 2 {
			findings = append(findings, finding("workflow-output.ref", element, fmt.Sprintf("source %q does not resolve", output.From)))
		} else if output.Artifact {
			if resolved.Schema.Type != "string" {
				findings = append(findings, finding("workflow-output.artifact-type", element, "artifact source must resolve to a string path"))
			}
		}
	}
	return findings
}

func validatePrompts(resolved ResolvedWorkflow) []Finding {
	var findings []Finding
	base := filepath.Dir(resolved.Path)
	for _, phase := range resolved.Workflow.Phases {
		if phase.Prompt != "" {
			element := fmt.Sprintf("workflow %q phase %q prompt file %q", resolved.Workflow.ID, phase.ID, phase.Prompt)
			clean := filepath.Clean(phase.Prompt)
			if filepath.IsAbs(phase.Prompt) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == ".." {
				findings = append(findings, finding("prompt.path", element, "prompt must be a sibling-relative path inside the definition directory"))
			} else {
				path, err := confinedPath(base, phase.Prompt)
				if err != nil {
					code := "prompt.path"
					if errors.Is(err, fs.ErrNotExist) {
						code = "prompt.file"
					}
					findings = append(findings, finding(code, element, err.Error()))
				} else {
					data, readErr := readLimitedFile(path, "prompt", MaxPromptBytes)
					if readErr != nil {
						findings = append(findings, finding("prompt.file", element, fmt.Sprintf("cannot read: %v", readErr)))
					} else {
						for _, message := range ValidateTemplate(string(data), phase.Inputs) {
							findings = append(findings, finding("prompt.template", element, message))
						}
					}
				}
			}
		}
		// A unit's prompt may read the phase's declared inputs and, in a dynamic
		// fan-out, the element binding. Resolving the element schema is what lets
		// `{{section.path}}` validate against the array's item schema instead of
		// reporting an undeclared reference.
		declarations := ResolveUnitDeclarations(resolved.Workflow, phase)
		role := unitRoleStatic
		if phase.DynamicFanOut() {
			role = unitRoleTemplate
		}
		for _, unit := range phase.UnitDefinitions() {
			findings = append(findings, validateUnitPrompt(resolved.Workflow.ID, phase, unit, base, declarations, role)...)
		}
		if phase.Join != nil {
			// The join runs after every unit rests: it never binds an element, and
			// it reads their results under the reserved `units` name instead.
			findings = append(findings, validateUnitPrompt(resolved.Workflow.ID, phase, *phase.Join, base, JoinDeclarations(phase), unitRoleJoin)...)
		}
	}
	return findings
}

func validateUnitPrompt(workflowID string, phase Phase, unit Unit, base string, declarations map[string]Variable, role string) []Finding {
	if unit.Prompt == "" {
		return nil
	}
	element := fmt.Sprintf("workflow %q phase %q %s %q prompt file %q", workflowID, phase.ID, role, unit.ID, unit.Prompt)
	clean := filepath.Clean(unit.Prompt)
	if filepath.IsAbs(unit.Prompt) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return []Finding{finding("prompt.path", element, "prompt must be a sibling-relative path inside the definition directory")}
	}
	path, err := confinedPath(base, unit.Prompt)
	if err != nil {
		code := "prompt.path"
		if errors.Is(err, fs.ErrNotExist) {
			code = "prompt.file"
		}
		return []Finding{finding(code, element, err.Error())}
	}
	data, err := readLimitedFile(path, "prompt", MaxPromptBytes)
	if err != nil {
		return []Finding{finding("prompt.file", element, fmt.Sprintf("cannot read: %v", err))}
	}
	var findings []Finding
	for _, message := range ValidateTemplate(string(data), declarations) {
		findings = append(findings, finding("prompt.template", element, message))
	}
	return findings
}

func validateBindings(workflow Workflow, bindings Bindings) []Finding {
	if bindings == nil {
		return nil
	}
	var findings []Finding
	for _, phase := range workflow.Phases {
		element := fmt.Sprintf("workflow %q phase %q", workflow.ID, phase.ID)
		if phase.Check != "" && !bindings.HasCheck(phase.Check) {
			findings = append(findings, finding("binding.check", element, fmt.Sprintf("check %q is not bindable", phase.Check)))
		}
		for _, resource := range phase.Resources {
			if _, bound := bindings.Capacity(resource); !bound {
				findings = append(findings, finding("binding.capacity", element, fmt.Sprintf("resource capacity %q is not bindable", resource)))
			}
		}
		commands := append(append([]string(nil), phase.Commands...), phase.Command)
		for _, command := range commands {
			if command != "" && !bindings.HasCommand(command) {
				findings = append(findings, finding("binding.command", element, fmt.Sprintf("command %q is not bindable", command)))
			}
		}
		for _, unit := range phase.UnitDefinitions() {
			if unit.Command != "" && !bindings.HasCommand(unit.Command) {
				findings = append(findings, finding("binding.command", element+fmt.Sprintf(" fan-out unit %q", unit.ID), fmt.Sprintf("command %q is not bindable", unit.Command)))
			}
		}
		if phase.Join != nil && phase.Join.Command != "" && !bindings.HasCommand(phase.Join.Command) {
			findings = append(findings, finding("binding.command", element+" join", fmt.Sprintf("command %q is not bindable", phase.Join.Command)))
		}
	}
	return findings
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
