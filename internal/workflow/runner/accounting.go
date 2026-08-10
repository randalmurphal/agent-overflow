package runner

import (
	"strings"

	"agent-overflow/internal/untrustedtext"
	"agent-overflow/internal/workflow/def"
)

// The merge-join obligation, stated to the join that carries it.
//
// `accounts_for_units: true` makes the engine post-validate a `done` join
// envelope against the exact set of units it ran over (def's
// `join_accounting.go`). Everything else post-validation enforces is stated in
// this same block for the same reason — a rule the engine applies and nobody
// states is one the element discovers by being refused — and this one has a
// sharper edge than most: the schema cannot express "these two arrays partition
// this set", so the block is the only place the join can learn it before it
// answers.
//
// The ids are listed rather than described. The join reads them under the
// reserved `units` binding only if its author interpolated `{{units}}`, and the
// set it is JUDGED against is the one this block names — so naming it here is
// what makes the obligation satisfiable without the author having to remember
// to show it.
func writeUnitAccountingSection(prompt *strings.Builder, accounts bool, unitIDs []string) {
	if !accounts {
		return
	}
	prompt.WriteString("This element is the join of a fan-out and must account for every unit of it. ")
	prompt.WriteString("In a `done` envelope, each unit id below must appear exactly once — either in outputs.")
	prompt.WriteString(def.JoinMergedOutput)
	prompt.WriteString(" (an array of unit ids whose work you took) or in outputs.")
	prompt.WriteString(def.JoinBlockedOutput)
	prompt.WriteString(" (an array of {")
	prompt.WriteString(def.JoinBlockedUnitField + ", " + def.JoinBlockedReasonField)
	prompt.WriteString("} objects, the reason saying why you could not take it). ")
	prompt.WriteString("A unit missing from both lists, named twice, or named when it is not below is refused and sent back to you. ")
	prompt.WriteString("Never drop a unit to make the lists balance: blocking one with an honest reason is always correct, and silently omitting one loses work nobody will know was lost.\n")
	if len(unitIDs) == 0 {
		prompt.WriteString("This fan-out expanded to no units, so both lists must be empty arrays.\n")
		return
	}
	prompt.WriteString("The units you must account for:\n")
	for _, id := range unitIDs {
		prompt.WriteString("- ")
		prompt.WriteString(untrustedtext.Field(id))
		prompt.WriteString("\n")
	}
}
