package def

import (
	"fmt"
	"strings"
)

// Unit roles name the three places a unit definition can appear, so a finding
// says which one it means without the reader counting fields.
const (
	unitRoleStatic   = "fan-out unit"
	unitRoleTemplate = "unit template"
	unitRoleJoin     = "join"
)

// validateFanOut checks one phase's fan-out authoring: that the shape and the
// declared form agree, that exactly one form is used, that a static list is
// inside the project's fan-out ceiling, that every unit carries a runnable
// binding, and that a join exists. Variable-level checks on `over:` live with
// the other reference resolution in validate_vars.go.
//
// maxWidth is the project's absolute ceiling (EffectiveMaxFanOutWidth); it is
// passed as the resolved number rather than as Bindings because the ceiling is
// the only binding fact this function needs and it never varies per phase.
func validateFanOut(workflow Workflow, phase Phase, phaseElement string, maxWidth int) []Finding {
	var findings []Finding
	add := func(code, element, message string) {
		findings = append(findings, finding(code, element, message))
	}
	dynamic := phase.DynamicFanOut()
	if phase.EffectiveShape() != ShapeFanOut {
		if len(phase.FanOut) > 0 || dynamic || phase.Join != nil {
			add("phase.fan-out", phaseElement, "fan_out, over/as/unit, and join require shape: fan-out")
		}
		return findings
	}
	switch {
	case len(phase.FanOut) > 0 && dynamic:
		add("phase.fan-out", phaseElement, "a fan-out declares either a static fan_out list or a dynamic over/as/unit template, not both")
	case len(phase.FanOut) == 0 && !dynamic:
		add("phase.fan-out", phaseElement, "fan-out shape requires a static fan_out list or a dynamic over/as/unit template")
	}
	// The ceiling is a refusal, not the capacity throttle fanOutWidthReports
	// describes: a list this wide never starts, so it is a Finding and the
	// workflow is invalid. A dynamic width has nothing static to check here; the
	// engine refuses it at expansion, where the number first exists.
	if len(phase.FanOut) > maxWidth {
		add("fan-out.max-width", phaseElement, fmt.Sprintf(
			"%d units exceed the project maximum fan-out width of %d; raise max_fan_out_width in the project profile or fan out over fewer units",
			len(phase.FanOut), maxWidth,
		))
	}
	if phase.Join == nil {
		add("phase.fan-out", phaseElement, "fan-out shape requires a join")
	}
	findings = append(findings, fanOutPhaseFieldFindings(phase, phaseElement)...)
	if dynamic {
		if strings.TrimSpace(phase.Over) == "" {
			add("phase.fan-out", phaseElement, "a dynamic fan-out requires over: an array-typed variable reference")
		}
		if phase.Unit == nil {
			add("phase.fan-out", phaseElement, "a dynamic fan-out requires one unit template")
		}
		findings = append(findings, validateElementBinding(workflow, phase, phaseElement)...)
	}
	unitIDs := make(map[string]bool, len(phase.FanOut))
	for unitIndex, unit := range phase.FanOut {
		unitElement := fmt.Sprintf("%s %s %q", phaseElement, unitRoleStatic, unit.ID)
		if !idPattern.MatchString(unit.ID) {
			add("phase.fan-out-unit", phaseElement, fmt.Sprintf("fan-out unit %d id must match [a-z0-9-]+", unitIndex))
		} else if unitIDs[unit.ID] {
			add("phase.fan-out-unit", unitElement, "unit id is duplicated")
		}
		unitIDs[unit.ID] = true
		findings = append(findings, validateUnitDefinition(unit, unitElement, unitRoleStatic)...)
	}
	if phase.Unit != nil {
		unitElement := fmt.Sprintf("%s %s %q", phaseElement, unitRoleTemplate, phase.Unit.ID)
		if !idPattern.MatchString(phase.Unit.ID) {
			add("phase.fan-out-unit", phaseElement, "unit template id must match [a-z0-9-]+")
		}
		findings = append(findings, validateUnitDefinition(*phase.Unit, unitElement, unitRoleTemplate)...)
	}
	if phase.Join != nil {
		joinElement := phaseElement + " " + unitRoleJoin
		if !idPattern.MatchString(phase.Join.ID) {
			add("phase.fan-out-unit", joinElement, "join id must match [a-z0-9-]+")
		}
		findings = append(findings, validateUnitDefinition(*phase.Join, joinElement, unitRoleJoin)...)
	}
	return findings
}

// fanOutPhaseFieldFindings refuses every field that configures the phase's own
// execution. A fan-out phase runs no turn and no command: `startPhaseWork`
// expands it into units instead of starting a runner, `phaseResources` skips its
// provider bound because its units and join each take their own, and
// `PhaseProducesToolEnvelope` answers from the join. A phase-level provider,
// model, prompt, or command therefore executes nothing, and a phase-level
// `access` reads as "my units may write" while every unit stays at its own
// declaration — so each is a finding naming the per-unit field to move it to,
// rather than a line the author never learns was ignored.
func fanOutPhaseFieldFindings(phase Phase, phaseElement string) []Finding {
	// One message per forbidden group, naming the fields the author actually
	// wrote, so a miswired fan-out reads as one problem instead of a wall.
	forbidden := []struct {
		fields  []string
		present bool
		message string
	}{
		{[]string{"driver"}, phase.Driver != "",
			"a fan-out phase runs no work of its own; each unit and the join declares its driver by the binding it carries — command: for a tool one, provider/model/prompt for an agent one"},
		{[]string{"provider", "model", "prompt"}, phase.Provider != "" || phase.Model != "" || phase.Prompt != "",
			"a fan-out phase runs no turn of its own; declare provider, model, and prompt on each fan_out unit (or on the unit: template) and on the join"},
		{[]string{"check", "command", "commands"}, phase.Check != "" || phase.Command != "" || len(phase.Commands) > 0,
			"a fan-out phase runs no command of its own; declare command: on the unit or the join that runs it"},
		{[]string{"access"}, phase.Access != "",
			"a fan-out phase touches no workspace; declare access on each unit and on the join — a writing unit gets its own sub-worktree, and a phase-level declaration would never reach one"},
	}
	var findings []Finding
	for _, group := range forbidden {
		if group.present {
			findings = append(findings, finding("phase.fan-out", phaseElement,
				fmt.Sprintf("%s is not valid on a fan-out phase: %s", strings.Join(group.fields, "/"), group.message)))
		}
	}
	return findings
}

// validateElementBinding checks the `as:` name. It shares one namespace with
// workflow inputs and phase ids, because a unit's prompt resolves all three
// from one variable map — a colliding binding would silently shadow the value
// the author meant.
func validateElementBinding(workflow Workflow, phase Phase, phaseElement string) []Finding {
	var findings []Finding
	binding := strings.TrimSpace(phase.As)
	if binding == "" {
		return []Finding{finding("phase.fan-out", phaseElement, "a dynamic fan-out requires as: the element binding name")}
	}
	if !idPattern.MatchString(binding) {
		findings = append(findings, finding("phase.fan-out", phaseElement, "as must match [a-z0-9-]+"))
	}
	if _, collision := workflow.Inputs[binding]; collision {
		findings = append(findings, finding("namespace.collision", phaseElement, fmt.Sprintf("element binding %q collides with workflow input %q", binding, binding)))
	}
	for _, other := range workflow.Phases {
		if other.ID == binding {
			findings = append(findings, finding("namespace.collision", phaseElement, fmt.Sprintf("element binding %q collides with phase %q", binding, binding)))
			break
		}
	}
	return findings
}

// validateUnitDefinition enforces that a unit carries exactly one runnable
// binding and that whatever it declares as outputs is a contract a provider can
// be held to. A unit has no `driver:` field — the binding it declares is the
// discriminator behind Unit.EffectiveDriver, so a unit with both or neither
// would make the driver a guess.
func validateUnitDefinition(unit Unit, element, role string) []Finding {
	var findings []Finding
	if unit.Access != "" && unit.Access != AccessReadOnly && unit.Access != AccessWrite {
		findings = append(findings, finding("phase.access", element, "access must be read-only or write"))
	}
	command := strings.TrimSpace(unit.Command)
	agentFields := strings.TrimSpace(unit.Provider) != "" || strings.TrimSpace(unit.Model) != "" || unit.Prompt != ""
	switch {
	case command != "" && agentFields:
		findings = append(findings, finding("phase.fan-out-unit", element, "a unit declares a command or provider/model/prompt, not both"))
	case command == "" && (strings.TrimSpace(unit.Provider) == "" || strings.TrimSpace(unit.Model) == "" || unit.Prompt == ""):
		findings = append(findings, finding("phase.fan-out-unit", element, "an agent unit requires provider, model, and prompt; a tool unit requires a command"))
	}
	return append(findings, validateUnitOutputs(unit, element, role)...)
}

// validateUnitOutputs applies the phase-output rules to a unit's own contract:
// the same name grammar, the same schema vocabulary, and the same refusal to
// redeclare an output the tool driver always supplies.
//
// A join declares none at all. Its envelope IS the phase's envelope, so the
// only contract it can answer is the phase's `outputs:` — a second declaration
// would name outputs the gate never reads.
func validateUnitOutputs(unit Unit, element, role string) []Finding {
	if role == unitRoleJoin {
		if len(unit.Outputs) > 0 {
			return []Finding{finding("phase.fan-out-unit", element,
				"a join answers the phase's outputs; remove its own outputs declaration")}
		}
		return nil
	}
	var findings []Finding
	for name, output := range unit.Outputs {
		outputElement := fmt.Sprintf("%s output %q", element, name)
		if !idPattern.MatchString(name) {
			findings = append(findings, finding("output.name", outputElement, "name must match [a-z0-9-]+"))
		}
		if unit.EffectiveDriver() == DriverTool && ReservedToolOutput(name) {
			findings = append(findings, finding("output.reserved", outputElement, "the tool driver always supplies this output; remove the declaration"))
		}
		findings = append(findings, validateSchemaDefinition(output.Schema, outputElement)...)
	}
	return findings
}

// fanOutWidthReports is the dry-run's informational half (spec §3): a static
// fan-out wider than the provider capacity it will bind to still runs, it just
// throttles, and you should learn that from validation instead of from watching
// units wait. A dynamic `over:` width is a runtime fact and is never reported.
//
// This is a different statement from the `fan-out.max-width` finding above and
// the two coexist: inside the ceiling but over capacity is a note about pacing,
// over the ceiling is a refusal to run at all. Reports are skipped entirely
// when no profile resolved, because "you will throttle to capacity N" is a
// claim only a real profile can make — the ceiling, which always has a default,
// is checked either way.
func fanOutWidthReports(workflow Workflow, bindings Bindings) []Finding {
	if bindings == nil {
		return nil
	}
	var reports []Finding
	for _, phase := range workflow.Phases {
		if phase.EffectiveShape() != ShapeFanOut || phase.DynamicFanOut() {
			continue
		}
		widths := make(map[string]int, 2)
		order := make([]string, 0, 2)
		for _, unit := range phase.FanOut {
			if unit.EffectiveDriver() != DriverAgent {
				continue
			}
			provider := strings.TrimSpace(unit.Provider)
			if provider == "" {
				continue
			}
			if _, seen := widths[provider]; !seen {
				order = append(order, provider)
			}
			widths[provider]++
		}
		for _, provider := range order {
			capacity := EffectiveProviderCapacity(bindings, provider)
			if widths[provider] <= capacity {
				continue
			}
			reports = append(reports, finding(
				"fan-out.width",
				fmt.Sprintf("workflow %q phase %q", workflow.ID, phase.ID),
				fmt.Sprintf(
					"%d units run on %q but %s capacity is %d; the run will throttle to capacity",
					widths[provider], provider, ProviderResource(provider), capacity,
				),
			))
		}
	}
	return reports
}

// EffectiveProviderCapacity is the bound an agent phase or unit actually gets
// for one provider: the project profile's declared capacity, or
// DefaultProviderCapacity when it declares none. A declared non-positive
// capacity is a profile finding of its own; the dry-run reports against the
// default rather than pretending the run has no bound.
func EffectiveProviderCapacity(bindings Bindings, provider string) int {
	if bindings != nil {
		if capacity, ok := bindings.Capacity(ProviderResource(provider)); ok && capacity > 0 {
			return capacity
		}
	}
	return DefaultProviderCapacity
}

// EffectiveMaxFanOutWidth is the absolute ceiling one fan-out phase attempt
// actually gets: the project profile's declared maximum, or
// DefaultMaxFanOutWidth when it declares none. A declared non-positive maximum
// is a profile finding of its own, so it resolves to the default here rather
// than to a ceiling that would refuse every fan-out.
//
// Nil bindings — a dry-run with no project resolved (`ao workflow validate`
// with no `--project`, a project with no profile.yaml) — get the default
// rather than a skip. Unset means the default everywhere else, and "no profile
// at all" is the most unset a project can be: the run-start path loads
// profile.Default() for exactly that project and enforces 32, so skipping here
// would let a definition validate clean and then be refused at its first
// expansion. The cost is that a project which raised its own ceiling sees a
// finding when validated without its profile; the message names the profile key
// to set, and erring toward the ceiling is the safe direction.
func EffectiveMaxFanOutWidth(bindings Bindings) int {
	if bindings != nil {
		if declared := bindings.DeclaredMaxFanOutWidth(); declared > 0 {
			return declared
		}
	}
	return DefaultMaxFanOutWidth
}
