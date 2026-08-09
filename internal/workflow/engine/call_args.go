package engine

import (
	"fmt"
	"sort"
	"strings"

	"agent-overflow/internal/workflow/def"
)

// unresolvedArg is one declared argument the caller's variable context has no
// value for: the child input it seeds, and the reference that came up empty.
type unresolvedArg struct{ name, ref string }

// resolveCallArgs evaluates one call edge's argument map. Every argument is a
// reference into the caller's variable context — a phase's for a call phase, a
// unit's (the phase inputs plus its `as:` element binding) for a call unit.
//
// A reference that does not resolve is reported rather than refused here.
// Whether an argument may simply be absent is a fact about the CHILD's declared
// inputs, which only the resolved target states (requireResolvedArgs), so this
// stays the pure half and the call edge asks the question once.
func resolveCallArgs(declared map[string]string, vars map[string]any) (map[string]any, []unresolvedArg) {
	args := make(map[string]any, len(declared))
	if len(declared) == 0 {
		return args, nil
	}
	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}
	sort.Strings(names)
	var unresolved []unresolvedArg
	for _, name := range names {
		value, ok := def.LookupVariable(vars, declared[name])
		if !ok {
			unresolved = append(unresolved, unresolvedArg{name: name, ref: declared[name]})
			continue
		}
		args[name] = value
	}
	return args, unresolved
}

// requireResolvedArgs refuses the unresolved arguments the child cannot be
// started without.
//
// An argument seeding an input the child declares `optional:` is OMITTED
// instead. The child then sees an absent optional input — byte for byte the run
// it would have had if a human had started it directly without that seed — and
// a workflow whose optional input is, as declared, absent no longer kills the
// call edge that forwards it. A self-calling campaign forwarding its own
// optional seed is the shape this exists for: it must not die at its recursion
// point because the value it never had is still not there.
//
// An argument naming NO child input is refused alongside the required ones.
// There is no declaration to be optional, so nothing says the absence is legal,
// and `def` already rejects that argument statically (`call.arg`) for the same
// reason.
func requireResolvedArgs(child def.Workflow, unresolved []unresolvedArg) error {
	missing := make([]string, 0, len(unresolved))
	for _, arg := range unresolved {
		if input, declared := child.Inputs[arg.name]; declared && input.Optional {
			continue
		}
		missing = append(missing, fmt.Sprintf("%s (%s)", arg.name, arg.ref))
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("arguments do not resolve: %s", strings.Join(missing, ", "))
}
