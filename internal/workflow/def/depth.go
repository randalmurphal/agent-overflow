package def

// CallDepthVariable is the reserved name every element of a run reads that
// run's CALL DEPTH under: 0 for the run a human or an automation started, 1 for
// a run one `call:` edge below it, and so on.
//
// It exists because a recursive campaign — one workflow whose last phase calls
// itself for the next wave (§3a) — otherwise has to compute its own wave
// ordinal, by seeding a number at the root and having each wave's planner hand
// its successor that number plus one through the self-call's `args:`. That is
// model arithmetic carrying a fact the engine already holds on the run row
// (`store.WorkItem.CallDepth`, the same number campaign memory stamps a note's
// `wave` provenance from), and a live forty-wave campaign desynced it: one wave
// mis-stated its own number and every wave after it inherited the error.
//
// It does NOT replace argument threading. A campaign that forwards richer
// context — a coverage ledger, carried notes — keeps doing so; what this makes
// unfalsifiable is the ORDINAL, which no element can now report wrongly because
// no element computes it.
//
// The name is COMPOUND, the way `job-notes` is, and deliberately not the bare
// `depth`. A reserved name is taken out of the namespace authored inputs share,
// so reserving a word authors already use for something else converts working
// definitions into refusals — and `depth` is such a word: it reads as "how deep
// should this audit go", which is exactly what one of this package's own test
// fixtures declares it as. `call-depth` names the one thing it is and matches
// the store column it is read from.
const CallDepthVariable = "call-depth"

// callDepthDeclaration is the shape `{{call-depth}}` renders and a predicate
// compares against. It is number-typed and never optional: every run has a
// depth, and a root's is 0 rather than absent — which is also why it is legal in
// a gate predicate where the reserved budget read is not. A wave ordinal is a
// fact about the run's position, not a measurement of what it has consumed.
func callDepthDeclaration() Variable {
	return Variable{Schema: JSONSchema{
		Type: "number",
		Description: "How deep this run sits in its call tree: 0 for the run that was started directly, " +
			"1 for a run one call edge below it, and so on. Supplied by the engine.",
	}}
}

// reservedInputName reports whether a name belongs to the engine rather than to
// the author. A declaration under one of these names is refused rather than
// shadowed, for the reason the `history` reservation exists: two things reachable
// under one name means one of them silently wins at every lookup.
//
// `units` is deliberately not a member. It is reserved inside a JOIN's context
// alone — no phase, unit, or gate outside one can see it — so a phase input of
// that name is a legitimate declaration everywhere except the join that shadows
// it, and JoinDeclarations binding it last is the whole rule.
func reservedInputName(name string) bool {
	return name == CallDepthVariable || name == BudgetVariable
}

// reservedDeclaration is the shape one reserved name renders, and the single
// place a name maps to it — so a reference resolving and the declaration set a
// prompt is validated against can never describe the reserved read differently.
func reservedDeclaration(name string) (Variable, bool) {
	switch name {
	case CallDepthVariable:
		return callDepthDeclaration(), true
	case BudgetVariable:
		return budgetDeclaration(), true
	default:
		return Variable{}, false
	}
}

// reservedInputMessage is the one sentence every site refusing a reserved name
// prints, so the workflow-input refusal and the phase-input refusal cannot drift
// into two explanations of one rule.
func reservedInputMessage(name string) string {
	return "the engine always supplies the reserved input " + quoted(name) + "; remove the declaration"
}

// reservedPhaseIDMessage refuses a PHASE named for a reserved read. The
// reservation has to hold at both ends of the namespace, exactly as `history`'s
// does: a phase named `call-depth` produces `call-depth.<output>` references,
// while the engine binds the reserved read under the bare `call-depth` — so one
// of the two silently wins at every lookup, and it is the reserved read, which
// is bound last and would overwrite the phase's whole output object with a
// number.
func reservedPhaseIDMessage(name string) string {
	return quoted(name) + " is reserved: the engine binds it for every element, so this phase's " +
		quoted(name+".<output>") + " references would resolve to the reserved value instead"
}

// bindReservedDeclarations adds the reserved reads to a declaration set. It is
// called LAST by every builder, so an authored name can never displace what the
// engine binds — the same ordering rule JoinDeclarations applies to `units` and
// the engine applies to `history.<phase>`.
func bindReservedDeclarations(declarations map[string]Variable) map[string]Variable {
	declarations[CallDepthVariable] = callDepthDeclaration()
	declarations[BudgetVariable] = budgetDeclaration()
	return declarations
}

// PhaseDeclarations returns what a phase's own prompt or command template may
// reference: its declared `inputs:`, plus the reserved reads. It is the phase
// counterpart of UnitDeclarations and JoinDeclarations, and it exists so
// validation and the runtime prompt builder ask one function rather than both
// reading `phase.Inputs` and disagreeing the moment a reserved name is added.
func PhaseDeclarations(phase Phase) map[string]Variable {
	declarations := make(map[string]Variable, len(phase.Inputs)+len(reservedInputNames))
	for name, variable := range phase.Inputs {
		declarations[name] = variable
	}
	return bindReservedDeclarations(declarations)
}

// reservedInputNames is the reserved set as a list, for the messages and sizing
// that need to name or count it. reservedInputName stays the predicate so there
// is no second answer to "is this reserved".
var reservedInputNames = []string{CallDepthVariable, BudgetVariable}
