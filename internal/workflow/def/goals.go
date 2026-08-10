package def

import (
	"fmt"
	"strings"
)

// A run carries a GOAL (what it was started to achieve, typed by whoever
// started it) and its definition carries NON-GOALS (what the workflow
// deliberately does not do). The two are not the same axis and are deliberately
// not merged: a goal is per invocation and lives on the run row, while a
// non-goal is a property of the definition and freezes with the snapshot.
//
// Non-goals exist because scope growth is what a long campaign fails at. An
// element sees its own slice and infers the rest; without a stated boundary
// each wave's plan re-forms an opinion of what "done" means, and the opinion
// drifts outward. Stating the boundary once, in the definition, puts it in
// every element's prompt for the life of the run.

const (
	// MaxNonGoals bounds the list. It is a boundary statement, not a
	// specification: a dozen "we are not doing this" lines is already more than
	// an element will hold in mind while working, and a longer list is a
	// definition that wants splitting rather than a prompt that wants more
	// bytes.
	MaxNonGoals = 12
	// MaxNonGoalRunes bounds one entry. A non-goal is one sentence naming
	// something not to do; anything needing a paragraph belongs in the prompt
	// file that explains the work.
	MaxNonGoalRunes = 500
)

// validateNonGoals holds the authored list to its bounds. Every rule is a
// finding rather than a silent trim: a non-goal that was quietly dropped is
// exactly the boundary an element then crosses.
func validateNonGoals(workflow Workflow, element string) []Finding {
	if len(workflow.NonGoals) == 0 {
		return nil
	}
	var findings []Finding
	add := func(message string) {
		findings = append(findings, finding("workflow.non-goals", element, message))
	}
	if len(workflow.NonGoals) > MaxNonGoals {
		add(fmt.Sprintf("%d non-goals exceed the maximum of %d", len(workflow.NonGoals), MaxNonGoals))
	}
	for index, entry := range workflow.NonGoals {
		switch {
		case strings.TrimSpace(entry) == "":
			add(fmt.Sprintf("non-goal %d is blank", index))
		case len([]rune(entry)) > MaxNonGoalRunes:
			add(fmt.Sprintf("non-goal %d is %d characters; the maximum is %d",
				index, len([]rune(entry)), MaxNonGoalRunes))
		}
	}
	return findings
}
