package def

// BudgetVariable is the reserved name every element of a run reads that run
// tree's BUDGET state under: the ceiling in force, what the tree has spent
// against it, what is left, and whether any of the spend figure was estimated
// from a rate table rather than reported by a provider.
//
// It exists because the ceiling is enforced somewhere an element cannot see.
// A per-item budget (§12) is checked by the engine before every phase attempt
// and, when it is crossed, parks the run — so an element that could have
// finished cheaply, or skipped an optional round, learns about the ceiling only
// by hitting it. Reading the same numbers the enforcement reads lets a prompt
// say "you have $4 of a $25 ceiling left, cut scope accordingly" without any
// element computing, carrying, or being told a number that could be wrong.
//
// It is a READ, and prompt-surface only:
//
//   - It cannot be written. Nothing an element emits changes a ceiling; raising
//     one is a human action on the run.
//   - It cannot be compared in a gate predicate (`budgetPredicateMessage`).
//     Routing on remaining budget is arithmetic-in-predicates by another name,
//     and the predicate vocabulary deliberately has none — a definition that
//     wants to branch on spend asks a model to decide and declares that as an
//     output.
//
// Absence is a real state: most runs have no ceiling at all. The declaration is
// therefore optional, and `{{budget}}` on a run with no budget renders the same
// "(not provided)" every absent optional input does, rather than a zero ceiling
// no one set.
const BudgetVariable = "budget"

// budgetDeclaration is the shape `{{budget}}` renders. The object is composed
// by the engine from the run tree's own record (`ResolveBudget`), so it declares
// no properties: the fields are the engine's to name, and an authored reference
// to one of them would be a contract this definition does not own — the same
// reason a `history.<phase>` binding may not declare `items:`.
func budgetDeclaration() Variable {
	return Variable{
		Optional: true,
		Schema: JSONSchema{
			Type: "object",
			Description: "This run tree's budget: {kind, ceiling, spent, remaining, estimated}. " +
				"kind is tokens, usd, or wall_clock; estimated is true when part of the spend " +
				"figure was priced from a rate table because the provider reported tokens only. " +
				"Absent when the run has no ceiling. Supplied by the engine.",
		},
	}
}

// budgetPredicateMessage is the refusal a gate predicate referencing the
// reserved budget read prints. It names the alternative, because a definition
// that reached for it wants to route on spend and needs somewhere to go.
func budgetPredicateMessage() string {
	return "the reserved input " + quoted(BudgetVariable) +
		" is prompt-surface only; a gate cannot route on spend — have the phase decide and declare that as an output"
}
