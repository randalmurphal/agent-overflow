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
	add(validateNonGoals(workflow, element)...)
	phaseIndex := make(map[string]int, len(workflow.Phases))
	for name, input := range workflow.Inputs {
		inputElement := fmt.Sprintf("%s input %q", element, name)
		if !idPattern.MatchString(name) {
			add(finding("input.name", inputElement, "name must match [a-z0-9-]+"))
		}
		if name == HistoryReservedName {
			add(finding("input.name", inputElement, fmt.Sprintf("%q is reserved for the %s<phase> input binding", HistoryReservedName, HistoryPrefix)))
		}
		if reservedInputName(name) {
			add(finding("input.reserved", inputElement, reservedInputMessage(name)))
		}
		add(validateVariableDeclaration(input, inputElement)...)
	}
	for i, phase := range workflow.Phases {
		phaseElement := fmt.Sprintf("%s phase %q", element, phase.ID)
		if !idPattern.MatchString(phase.ID) {
			add(finding("phase.id", phaseElement, "id must match [a-z0-9-]+"))
		}
		if phase.ID == "done" || phase.ID == "failed" {
			add(finding("phase.id", phaseElement, "done and failed are reserved terminal names"))
		}
		if phase.ID == HistoryReservedName {
			// A phase named `history` produces `history.<output>` references that
			// the reserved binding namespace already claims, so one of the two
			// would silently win at every lookup.
			add(finding("phase.id", phaseElement, fmt.Sprintf("%q is reserved for the %s<phase> input binding", HistoryReservedName, HistoryPrefix)))
		}
		if reservedInputName(phase.ID) {
			add(finding("phase.reserved", phaseElement, reservedPhaseIDMessage(phase.ID)))
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
			add(validateVariableDeclaration(output, outputElement)...)
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
	add(validateWorkflowOutputs(effective, phaseIndex, graph)...)
	add(validatePrompts(resolved)...)
	add(validateBindings(workflow, bindings)...)
	result.Reports = append(fanOutWidthReports(workflow, bindings), gateNotifyReports(workflow)...)
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
	// `effort:` tunes a model turn and is legal exactly where provider/model are.
	// A tool phase runs a command, so the tier would configure nothing — refused
	// rather than left as a line the author never learns was ignored.
	if strings.TrimSpace(phase.Effort) != "" {
		if phase.Driver != DriverAgent {
			add("phase.effort", "effort requires driver: agent; it pins the reasoning tier of a model turn, and a tool phase runs a command")
		} else {
			findings = append(findings, effortTierFindings(phase.Effort, phaseElement)...)
		}
	}
	// The mirror of the agent requirement above: a tool phase runs a command,
	// not a model turn, so the fields that configure one would be dead lines the
	// author never learns were ignored — refused for the same reason `effort:`
	// is, and for the same reason a call phase and a fan-out phase refuse them.
	if phase.Driver == DriverTool &&
		(strings.TrimSpace(phase.Provider) != "" || strings.TrimSpace(phase.Model) != "" || phase.Prompt != "") {
		add("phase.tool", "a tool phase runs a command, not a model turn; provider, model, and prompt belong on an agent phase")
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

func validatePrompts(resolved ResolvedWorkflow) []Finding {
	var findings []Finding
	base := filepath.Dir(resolved.Path)
	phases := make(map[string]Phase, len(resolved.Workflow.Phases))
	for _, phase := range resolved.Workflow.Phases {
		phases[phase.ID] = phase
	}
	for _, phase := range resolved.Workflow.Phases {
		findings = append(findings, validatePromptFile(
			fmt.Sprintf("workflow %q phase %q prompt file %q", resolved.Workflow.ID, phase.ID, phase.Prompt),
			base, phase.Prompt, PhaseDeclarations(phase))...)
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
		for routeIndex, route := range phase.Gate.Routes {
			if route.Prompt == "" {
				continue
			}
			// A route's override renders in the TARGET phase's context, because
			// that is the attempt it replaces the body of. Validating its template
			// against the source phase's inputs would accept references the round
			// cannot resolve and reject the ones it can. A target that does not
			// exist is `gate.target`'s finding; there is nothing to check against
			// here, so the file is still checked and the template is not.
			target, known := phases[route.Loop]
			element := fmt.Sprintf("workflow %q phase %q route %d prompt file %q",
				resolved.Workflow.ID, phase.ID, routeIndex, route.Prompt)
			if !known {
				// Checking the template against NO declarations is not "the template
				// is not checked" — it reports every reference in the file as
				// undeclared, one finding per token, burying the single real one
				// (`gate.target`, naming the phase this route cannot reach) that the
				// author has to fix first.
				_, fileFindings, _ := readAuthoredPrompt(element, base, route.Prompt)
				findings = append(findings, fileFindings...)
				continue
			}
			findings = append(findings, validatePromptFile(element, base, route.Prompt, PhaseDeclarations(target))...)
		}
	}
	return findings
}

func validateUnitPrompt(workflowID string, phase Phase, unit Unit, base string, declarations map[string]Variable, role string) []Finding {
	return validatePromptFile(
		fmt.Sprintf("workflow %q phase %q %s %q prompt file %q", workflowID, phase.ID, role, unit.ID, unit.Prompt),
		base, unit.Prompt, declarations)
}

// validatePromptFile is the one set of rules every authored prompt path is held
// to — a phase's, a unit's, a join's, and a loop route's override — so a new
// prompt site cannot arrive with its own idea of what "sibling-relative" means
// or forget to template-check what it read. `declarations` is what the body may
// reference, which is the one thing that differs per site.
func validatePromptFile(element, base, relative string, declarations map[string]Variable) []Finding {
	body, findings, ok := readAuthoredPrompt(element, base, relative)
	if !ok {
		return findings
	}
	for _, message := range ValidateTemplate(body, declarations) {
		findings = append(findings, finding("prompt.template", element, message))
	}
	return findings
}

// readAuthoredPrompt is the PATH half of validatePromptFile on its own: the
// sibling-relative rule, the confinement, and the bounded read, returning the
// body so the caller can decide whether a template check applies.
//
// It is split out for the one site where it does not: a loop route whose target
// phase does not resolve has no declaration set to check against, and checking
// against an empty one is not the same as not checking.
func readAuthoredPrompt(element, base, relative string) (string, []Finding, bool) {
	if relative == "" {
		return "", nil, false
	}
	clean := filepath.Clean(relative)
	if filepath.IsAbs(relative) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", []Finding{finding("prompt.path", element, "prompt must be a sibling-relative path inside the definition directory")}, false
	}
	path, err := confinedPath(base, relative)
	if err != nil {
		code := "prompt.path"
		if errors.Is(err, fs.ErrNotExist) {
			code = "prompt.file"
		}
		return "", []Finding{finding(code, element, err.Error())}, false
	}
	data, err := readLimitedFile(path, "prompt", MaxPromptBytes)
	if err != nil {
		return "", []Finding{finding("prompt.file", element, fmt.Sprintf("cannot read: %v", err))}, false
	}
	return string(data), nil, true
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
			unitElement := element + fmt.Sprintf(" fan-out unit %q", unit.ID)
			if unit.Command != "" && !bindings.HasCommand(unit.Command) {
				findings = append(findings, finding("binding.command", unitElement, fmt.Sprintf("command %q is not bindable", unit.Command)))
			}
			findings = append(findings, unitCapacityFindings(unit, unitElement, bindings)...)
		}
		if phase.Join != nil {
			if phase.Join.Command != "" && !bindings.HasCommand(phase.Join.Command) {
				findings = append(findings, finding("binding.command", element+" join", fmt.Sprintf("command %q is not bindable", phase.Join.Command)))
			}
			findings = append(findings, unitCapacityFindings(*phase.Join, element+" join", bindings)...)
		}
	}
	return findings
}

// unitCapacityFindings holds a unit's declared resources to the same
// bindability rule a phase's are held to. A unit acquires them itself, from the
// same project profile and the same semaphores, so a name the project never
// sized is refused here rather than parking the wave at its first admission.
func unitCapacityFindings(unit Unit, element string, bindings Bindings) []Finding {
	var findings []Finding
	for _, resource := range unit.Resources {
		if _, bound := bindings.Capacity(resource); !bound {
			findings = append(findings, finding("binding.capacity", element, fmt.Sprintf("resource capacity %q is not bindable", resource)))
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
