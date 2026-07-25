package def

import (
	"fmt"
	"strings"
)

// DefaultProviderCapacity bounds concurrent agent turns per provider when the
// project profile declares no `provider:<name>` capacity. It lives beside the
// authoring format because two consumers must agree on it: the engine, which
// acquires the implicit resource before every agent phase and unit, and the
// dry-run, which reports a static fan-out wider than the capacity it will
// actually get.
const DefaultProviderCapacity = 2

// DefaultMaxFanOutWidth is the absolute ceiling on the units one fan-out phase
// attempt may expand to when the project profile declares none. It lives beside
// DefaultProviderCapacity for the same reason: the dry-run (which refuses a
// static list wider than it) and the engine (which refuses an expansion wider
// than it) must read one number.
//
// It is NOT a capacity. Capacity throttles work that all still runs; this is a
// refusal, so the number is chosen as "past here, the width is an accident
// rather than a plan". 32 sits comfortably above any hand-authored `fan_out:`
// list — the widest anywhere in the shipped starters and the spec's examples is
// single digits — and above the realistic dynamic case of one unit per section
// of a plan. Past it, a width is almost always a query that did not filter, and
// 32 units is already 32 sub-worktrees, 32 branches, and 32 provider sessions'
// worth of subscription spend for one phase. A project that genuinely wants 50
// writes 50 in its profile; nothing is unbounded.
const DefaultMaxFanOutWidth = 32

const providerResourcePrefix = "provider:"

// ProviderResource names the implicit resource every agent-driver phase and
// fan-out unit acquires on top of its declared resources.
func ProviderResource(provider string) string { return providerResourcePrefix + provider }

// IsProviderResource reports whether a resource name is in the reserved
// implicit-provider namespace. `provider:` with no name is not.
func IsProviderResource(name string) bool {
	return len(name) > len(providerResourcePrefix) && strings.HasPrefix(name, providerResourcePrefix)
}

// ExpandedUnit is one unit of one fan-out attempt: the definition to run, the
// id that identifies it for its whole lifetime, and the element binding a
// dynamic expansion stamped it with.
type ExpandedUnit struct {
	ID       string         `json:"id"`
	Index    int            `json:"index"`
	Unit     Unit           `json:"unit"`
	Bindings map[string]any `json:"bindings,omitempty"`
}

// ExpandUnits returns the units a fan-out phase attempt runs, in launch order.
//
// A static `fan_out:` list expands to itself. A dynamic `over:`/`as:`/`unit:`
// phase stamps one unit per element of the named array variable, binding the
// element under the `as` name for prompt and command interpolation. Unit count
// is therefore a runtime fact: the same definition can expand to zero units on
// one run and twenty on the next. Zero is legal — the attempt runs its join
// over no units.
//
// vars is the attempt's frozen variable context, so re-expanding a persisted
// attempt (a retry after a park, a rebuild after a crash) reproduces exactly
// the same ids and bindings.
func ExpandUnits(phase Phase, vars map[string]any) ([]ExpandedUnit, error) {
	if phase.EffectiveShape() != ShapeFanOut {
		return nil, fmt.Errorf("phase %q has shape %q, not %q", phase.ID, phase.EffectiveShape(), ShapeFanOut)
	}
	if !phase.DynamicFanOut() {
		units := make([]ExpandedUnit, 0, len(phase.FanOut))
		for index, unit := range phase.FanOut {
			units = append(units, ExpandedUnit{ID: unit.ID, Index: index, Unit: unit})
		}
		return units, nil
	}
	if phase.Unit == nil {
		return nil, fmt.Errorf("phase %q declares a dynamic fan-out with no unit template", phase.ID)
	}
	binding := strings.TrimSpace(phase.As)
	if binding == "" {
		return nil, fmt.Errorf("phase %q declares a dynamic fan-out with no element binding", phase.ID)
	}
	value, ok := LookupVariable(vars, phase.Over)
	if !ok {
		return nil, fmt.Errorf("phase %q fans out over %q, which is not in scope", phase.ID, phase.Over)
	}
	elements, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("phase %q fans out over %q, which is %T, not an array", phase.ID, phase.Over, value)
	}
	template := *phase.Unit
	prefix := strings.TrimSpace(template.ID)
	if prefix == "" {
		prefix = phase.ID
	}
	units := make([]ExpandedUnit, 0, len(elements))
	for index, element := range elements {
		stamped := template
		stamped.ID = fmt.Sprintf("%s-%d", prefix, index)
		units = append(units, ExpandedUnit{
			ID:       stamped.ID,
			Index:    index,
			Unit:     stamped,
			Bindings: map[string]any{binding: element},
		})
	}
	return units, nil
}

// UnitDefinition resolves the frozen definition behind one unit id of a fan-out
// phase, without needing the variable context an expansion requires. A static
// fan-out names its units directly; a dynamic one stamps ids onto a single
// template, so every unit of it shares that template's provider, access, and
// output contract — only the id differs, and it is restored here.
//
// It exists for the recovery paths that hold a persisted unit row and need the
// contract that row ran under: re-expanding would mean reloading and decoding
// the attempt's whole input envelope to learn something the id already implies.
func UnitDefinition(phase Phase, unitID string, join bool) (Unit, bool) {
	if join {
		if phase.Join == nil || phase.Join.ID != unitID {
			return Unit{}, false
		}
		return *phase.Join, true
	}
	if phase.DynamicFanOut() {
		if phase.Unit == nil || !stampedUnitID(phase, unitID) {
			return Unit{}, false
		}
		unit := *phase.Unit
		unit.ID = unitID
		return unit, true
	}
	for _, unit := range phase.FanOut {
		if unit.ID == unitID {
			return unit, true
		}
	}
	return Unit{}, false
}

// stampedUnitID reports whether an id is one ExpandUnits could have produced for
// this dynamic phase: the template prefix and a non-negative index. Without the
// check a dynamic phase would answer UnitDefinition for *any* id — including its
// join's, or a typo — by fabricating a unit from the template, and a caller
// holding a wrong id would run a real turn instead of failing.
func stampedUnitID(phase Phase, unitID string) bool {
	prefix := strings.TrimSpace(phase.Unit.ID)
	if prefix == "" {
		prefix = phase.ID
	}
	index, ok := strings.CutPrefix(unitID, prefix+"-")
	if !ok || index == "" {
		return false
	}
	for _, digit := range index {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

// UnitsVariable is the reserved name a join's variable context binds its units'
// results under. It is reserved inside a join only: no other phase, unit, or
// gate sees it. The engine binds the values and this package declares their
// shape, so the prompt validator and the runtime context cannot disagree about
// what `{{units}}` renders.
const UnitsVariable = "units"

// unitsDeclaration is the shape one entry of the reserved `units` binding has.
// `outputs` is an open object because each unit answers its own declared
// contract; the join reads it as JSON rather than by declared path.
func unitsDeclaration() Variable {
	return Variable{Schema: JSONSchema{
		Type:        "array",
		Description: "Results of the units this join consolidates.",
		Items: &JSONSchema{
			Type: "object",
			Properties: map[string]JSONSchema{
				"id":       {Type: "string"},
				"index":    {Type: "number"},
				"status":   {Type: "string"},
				"branch":   {Type: "string"},
				"worktree": {Type: "string"},
				"thread":   {Type: "string"},
				"outputs":  {Type: "object"},
			},
			Required: []string{"id", "index", "status"},
		},
	}}
}

// UnitDeclarations returns the variables a unit's prompt or command may
// reference: everything the phase declares as an input, plus the element
// binding a dynamic fan-out provides. element is the resolved item schema of
// the `over:` array; an unresolvable `over:` reference is reported by variable
// validation and contributes no declaration here.
func UnitDeclarations(phase Phase, element *Variable) map[string]Variable {
	declarations := make(map[string]Variable, len(phase.Inputs)+1)
	for name, variable := range phase.Inputs {
		declarations[name] = variable
	}
	if phase.DynamicFanOut() && element != nil && strings.TrimSpace(phase.As) != "" {
		declarations[strings.TrimSpace(phase.As)] = *element
	}
	return declarations
}

// ResolveUnitDeclarations is UnitDeclarations with the element schema resolved
// from the workflow itself. Validation and the runtime prompt builder both go
// through it, so a template that validated at authoring time cannot fail to
// interpolate at run time.
func ResolveUnitDeclarations(workflow Workflow, phase Phase) map[string]Variable {
	element, ok := overElement(workflow, phaseIndexes(workflow), phase)
	if !ok {
		return UnitDeclarations(phase, nil)
	}
	return UnitDeclarations(phase, &element)
}

// JoinDeclarations returns the variables a join's prompt or command may
// reference: the phase's declared inputs plus the reserved `units` results. The
// reserved name is bound last for the same reason the engine binds it last —
// the results a join exists to consolidate can never be shadowed.
func JoinDeclarations(phase Phase) map[string]Variable {
	declarations := make(map[string]Variable, len(phase.Inputs)+1)
	for name, variable := range phase.Inputs {
		declarations[name] = variable
	}
	declarations[UnitsVariable] = unitsDeclaration()
	return declarations
}

func phaseIndexes(workflow Workflow) map[string]int {
	indexes := make(map[string]int, len(workflow.Phases))
	for index, phase := range workflow.Phases {
		if _, duplicate := indexes[phase.ID]; !duplicate {
			indexes[phase.ID] = index
		}
	}
	return indexes
}
