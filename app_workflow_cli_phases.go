package main

import (
	"encoding/json"
	"fmt"

	"agent-overflow/internal/workflow/def"
)

// The per-attempt provenance `ao run status` carries (D38). A campaign agent
// reading a parked run has two questions the run row cannot answer: which
// attempt produced the outputs the gate consumed, and what each element
// actually ran with. Both are already persisted — the attempt rows carry
// status and gate trace, and the thread the attempt ran on carries the
// resolved provider/model/effort — so this is a projection, not new state.

// WorkflowAgentPhaseAttempt is one attempt of one phase. It is deliberately
// narrower than the phase row: no envelopes, no predicate trace, no narrative
// path. The decision fields are the gate's OUTCOME — which way the run went and
// which loop budgets it had spent — because that is what a reader deciding
// between `run resolve`, `run resume --phase`, and `run rerun` needs; the
// predicates that produced it are a debugging read, and they live in the app.
type WorkflowAgentPhaseAttempt struct {
	PhaseID string `json:"phaseId"`
	Attempt int    `json:"attempt"`
	Status  string `json:"status"`
	// Provider, Model, and Effort are the settings the attempt's thread was
	// created with, empty for an attempt that has no thread — a tool-driver
	// phase runs a command, not a provider session.
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Effort   string `json:"effort,omitempty"`
	// Decision, DecisionTarget, and ExhaustedLoops are absent until the attempt's
	// gate has been evaluated and persisted.
	Decision       string   `json:"decision,omitempty"`
	DecisionTarget string   `json:"decisionTarget,omitempty"`
	ExhaustedLoops []string `json:"exhaustedLoops,omitempty"`
}

// workflowAgentPhaseAttempts projects a run's phase history for the single-run
// status read. A gate trace that no longer decodes fails the read rather than
// reporting an attempt with no decision: "this attempt reached no gate" and
// "this attempt's record is corrupt" are different answers, and only one of
// them is a state a run can legitimately be in.
func (a *App) workflowAgentPhaseAttempts(itemID string) ([]WorkflowAgentPhaseAttempt, error) {
	rows, err := a.store.ListWorkItemPhaseProvenance(itemID)
	if err != nil {
		return nil, err
	}
	attempts := make([]WorkflowAgentPhaseAttempt, 0, len(rows))
	for _, row := range rows {
		attempt := WorkflowAgentPhaseAttempt{
			PhaseID: row.PhaseID, Attempt: row.Attempt, Status: row.Status,
			Provider: row.Provider, Model: row.Model, Effort: row.Effort,
		}
		if len(row.GateTrace) > 0 {
			var trace def.GateTrace
			if err := json.Unmarshal(row.GateTrace, &trace); err != nil {
				return nil, fmt.Errorf(
					"workflow run status %s: gate trace for %s attempt %d is unreadable: %w",
					itemID, row.PhaseID, row.Attempt, err)
			}
			attempt.Decision = string(trace.Decision.Kind)
			attempt.DecisionTarget = trace.Decision.Target
			attempt.ExhaustedLoops = trace.ExhaustedLoops
		}
		attempts = append(attempts, attempt)
	}
	return attempts, nil
}
