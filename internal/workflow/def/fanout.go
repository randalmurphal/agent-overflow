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

// UnitDeclarations returns the variables a unit's prompt or command may
// reference: everything the phase declares as an input, plus the element
// binding a dynamic fan-out provides. elementSchema is the resolved item schema
// of the `over:` array; an unresolvable `over:` reference is reported by
// variable validation and contributes no declaration here.
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
