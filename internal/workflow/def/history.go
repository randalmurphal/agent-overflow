package def

import (
	"fmt"
	"reflect"
	"strings"
)

// HistoryPrefix opens the reserved input-name namespace a phase declares its
// prior attempts under. `history.<phaseID>` names any phase of the workflow,
// including the declaring phase itself, and binds that phase's earlier attempts
// oldest-first.
//
// It exists because the ordinary `<phase>.<output>` reference resolves to the
// highest completed attempt alone: a phase re-entered by a loop is otherwise
// structurally blind to every round before the last one, and two adjudicators
// ruling opposite ways can oscillate a review/fix pair indefinitely with each
// round obeying the newest verdict and none of them able to see the pattern.
const HistoryPrefix = "history."

// HistoryReservedName is the name the prefix consumes. A phase or workflow
// input called `history` would produce `history.<output>` references that are
// indistinguishable from this binding, so the name is refused at both.
const HistoryReservedName = "history"

const (
	// DefaultHistoryWindow is how many prior attempts a binding carries when it
	// declares no `window:`. Ten covers every loop bound an author writes by
	// hand while staying far below the point where the entries dominate the
	// prompt they are attached to.
	DefaultHistoryWindow = 10

	// MaxHistoryWindow is the ceiling on an authored `window:`, refused at
	// validation rather than silently trimmed at run time: a window the engine
	// will not honour is a definition that does not do what it says.
	MaxHistoryWindow = 50

	// MaxHistoryBytes bounds one rendered binding. Entries are size-capped
	// individually (each is one envelope's outputs, under DefaultEnvelopeSizeCap),
	// but a window multiplies them, so the series needs a bound of its own.
	// Content past it is dropped whole entries at a time, oldest first, and each
	// dropped entry SAYS it was elided — a window that silently shortened would
	// be indistinguishable from a phase that never ran those rounds.
	MaxHistoryBytes = 32 * 1024
)

// HistoryBinding reports the phase whose attempts an input name binds, and
// whether the name is a history binding at all. `history.` with nothing after
// it is a binding naming no phase, which validation reports as an unknown
// phase rather than silently treating as an ordinary input.
func HistoryBinding(name string) (string, bool) {
	target, ok := strings.CutPrefix(name, HistoryPrefix)
	return target, ok
}

// EffectiveHistoryWindow resolves how many prior attempts a binding carries.
// Zero means undeclared; a value over MaxHistoryWindow is a validation finding,
// and the ceiling is applied here too so a frozen snapshot carrying one — those
// are decoded and never re-validated — cannot expand past the bound either.
func EffectiveHistoryWindow(declaration Variable) int {
	window := declaration.Window
	if window <= 0 {
		window = DefaultHistoryWindow
	}
	return min(window, MaxHistoryWindow)
}

// historyBindingSchema is the only schema a history binding may declare. The
// entry shape is reserved — the engine composes it from persisted attempt rows,
// not from anything the author controls — so an authored `items:` would be a
// contract this definition does not own, and any other type would describe a
// value the binding never takes.
var historyBindingSchema = JSONSchema{Type: "array"}

// historyBindingFindings validates one `history.<phase>` input declaration.
// The reference is checked against the workflow's phases rather than through
// resolveReference: this binding names a phase, not one of its outputs, and it
// has no dominance rule — reading a phase's own prior attempts, or those of a
// phase that has not run yet (an empty array), is exactly the point.
func historyBindingFindings(workflow Workflow, phaseIndex map[string]int, phase Phase, name, target string, declaration Variable) []Finding {
	element := fmt.Sprintf("workflow %q phase %q input %q", workflow.ID, phase.ID, name)
	var findings []Finding
	if _, ok := phaseIndex[target]; !ok {
		findings = append(findings, finding("history.unknown-phase", element, fmt.Sprintf("no phase %q is declared by this workflow", target)))
	}
	if !reflect.DeepEqual(declaration.Schema, historyBindingSchema) {
		findings = append(findings, finding("history.schema", element, "a history binding declares `schema: {type: array}` and nothing else; the entry shape is reserved"))
	}
	if declaration.Optional {
		findings = append(findings, finding("history.optional", element, "a history binding is always bound — an empty array before the first prior attempt — so `optional:` describes a state it never takes"))
	}
	if declaration.Window < 0 || declaration.Window > MaxHistoryWindow {
		findings = append(findings, finding("history.window", element, fmt.Sprintf("window is %d; it must be between 1 and %d (omit it for %d)", declaration.Window, MaxHistoryWindow, DefaultHistoryWindow)))
	}
	return findings
}
