package aocli

import (
	"fmt"
	"strings"
)

// How a run and its phase attempts read on a human line — the one place that
// decides what a caller sees without `--json`. The verbs themselves, and which
// of these lines each one prints, live in exec_run.go.

// runView mirrors only the fields the human lines print. The authoritative shape
// is the app's; `--json` forwards that one verbatim.
type runView struct {
	ItemID              string `json:"itemId"`
	WorkflowID          string `json:"workflowId"`
	WorkflowScope       string `json:"workflowScope"`
	Goal                string `json:"goal"`
	State               string `json:"state"`
	Reason              string `json:"reason"`
	CurrentPhaseID      string `json:"currentPhaseId"`
	CurrentPhaseOrdinal int    `json:"currentPhaseOrdinal"`
	PhaseCount          int    `json:"phaseCount"`
	ParentItemID        string `json:"parentItemId"`
	Resting             bool   `json:"resting"`
	Skipped             bool   `json:"skipped"`
	BoundThreadID       string `json:"boundThreadId"`
	BindingWarning      string `json:"bindingWarning"`
	// FailedUnits is present only on `run status` for a run parked on a failed
	// fan-out; the line prints the ids because they are the second argument of
	// `run retry-unit`.
	FailedUnits []runFailedUnit `json:"failedUnits"`
	// Phases is the run's per-attempt provenance, present on `run status` only.
	Phases []runPhaseAttempt `json:"phases"`
}

// runFailedUnit is the one field of a failed unit the human line prints. The
// app's document carries more (the unit's try count); `--json` forwards that
// one verbatim, as it does for every other field this CLI does not model.
type runFailedUnit struct {
	UnitID string `json:"unitId"`
}

// runPhaseAttempt is one attempt of one phase: what ran it and how its gate
// decided. It answers the two questions a run line cannot — which attempt
// produced the outputs a gate is parked on, and what model settings each
// element actually ran with — for a reader who has only this CLI.
type runPhaseAttempt struct {
	PhaseID        string   `json:"phaseId"`
	Attempt        int      `json:"attempt"`
	Status         string   `json:"status"`
	Provider       string   `json:"provider"`
	Model          string   `json:"model"`
	Effort         string   `json:"effort"`
	Decision       string   `json:"decision"`
	DecisionTarget string   `json:"decisionTarget"`
	ExhaustedLoops []string `json:"exhaustedLoops"`
}

func (p runPhaseAttempt) line() string {
	// Kind and target are one fact — where the gate sent the run — so they print
	// as one field rather than two a reader has to pair up.
	decision := p.Decision
	if decision != "" && p.DecisionTarget != "" {
		decision += "->" + p.DecisionTarget
	}
	return fields(
		"phase="+p.PhaseID,
		fmt.Sprintf("attempt=%d", p.Attempt),
		"status="+p.Status,
		optionalField("provider", p.Provider),
		optionalField("model", p.Model),
		optionalField("effort", p.Effort),
		optionalField("decision", decision),
		optionalField("exhausted-loops", strings.Join(p.ExhaustedLoops, ",")),
	)
}

func (v runView) line() string {
	phase := v.CurrentPhaseID
	if phase != "" && v.PhaseCount > 0 {
		phase = fmt.Sprintf("%s(%d/%d)", phase, v.CurrentPhaseOrdinal, v.PhaseCount)
	}
	units := make([]string, 0, len(v.FailedUnits))
	for _, unit := range v.FailedUnits {
		units = append(units, unit.UnitID)
	}
	return fields(
		"run="+v.ItemID,
		// Parent sits next to the run id because it is the same fact: which run
		// this is. A campaign's `run list` is otherwise a flat list of ids with
		// the tree that relates them invisible.
		optionalField("parent", v.ParentItemID),
		optionalField("workflow", v.WorkflowID),
		"state="+v.State,
		optionalField("reason", v.Reason),
		optionalField("phase", phase),
		optionalField("failed-units", strings.Join(units, ",")),
		skippedField(v.Skipped),
	)
}

// statusBlock is what `run status` prints: the run line, then one line per phase
// attempt. Only that verb carries them — the app populates the attempts on the
// single-run read alone, and the control verbs ask "where is it now", which the
// run line already answers.
func (v runView) statusBlock() string {
	var block strings.Builder
	block.WriteString(v.line())
	for _, phase := range v.Phases {
		block.WriteString("\n")
		block.WriteString(phase.line())
	}
	return block.String()
}

func skippedField(skipped bool) string {
	if !skipped {
		return ""
	}
	return "skipped=true"
}
