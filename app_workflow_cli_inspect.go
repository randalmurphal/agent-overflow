package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"agent-overflow/internal/store"
)

// `agent-overflow run inspect` — the whole picture of one run in one call.
//
// Everything here was already persisted and already readable; what was missing
// was a read that returns it together. An agent supervising a campaign asked
// "which worktree, which branch, which seeds, which children, what did the
// latest attempt output" before every gate decision, and answered it with raw
// SQL against the production database because no verb exposed any of it. The
// projection stays narrow for the same reason `run status` does — an agent's
// context window pays for every byte — so envelope outputs arrive as a bounded
// digest until a caller names the attempt it actually wants.

// WorkflowAgentInspectInput is `run inspect`. PhaseID selects a single attempt
// to read whole; Attempt narrows that to one try and is meaningless without it.
type WorkflowAgentInspectInput struct {
	ItemID  string `json:"itemId"`
	PhaseID string `json:"phaseId,omitempty"`
	Attempt int    `json:"attempt,omitempty"`
}

// WorkflowAgentRunInspection is what `run inspect` returns. Run is exactly the
// `run status` document, so a reader that already parses one parses this; the
// rest is what a run record carries and that projection deliberately omits.
type WorkflowAgentRunInspection struct {
	Run WorkflowAgentRunView `json:"run"`
	// WorktreePath, Branch, and BaseBranch are where the run's work actually
	// happens. Nothing else on the CLI surface names them, and a supervising
	// agent cannot inspect a diff, a log, or a commit without them.
	WorktreePath string `json:"worktreePath,omitempty"`
	Branch       string `json:"branch,omitempty"`
	BaseBranch   string `json:"baseBranch,omitempty"`
	// Children are the runs this run called, oldest first. They are NOT bounded:
	// a campaign's waves are its children, so eliding them would truncate the
	// answer to the question that is being asked — and one row per child is the
	// same cost `run list` already pays per run.
	Children []WorkflowAgentChildRun `json:"children"`
	// Guidance is what `run guide` left and the run has not reached a phase entry
	// to consume yet, oldest first. It is RUN-level rather than phase-level
	// because the slot is: an entry is delivered at whichever phase the run
	// enters next, not at one it is resting in. Absent when nothing is pending.
	Guidance []WorkflowAgentGuidanceEntry `json:"guidance,omitempty"`
	// Phase is present only when the caller named one, and carries that attempt
	// read whole.
	Phase *WorkflowAgentPhaseDetail `json:"phase,omitempty"`
}

// WorkflowAgentChildRun is one run this run called. The parent coordinate rides
// along because it is what tells two children apart: a call phase re-entered by
// a loop makes one child per attempt, and a fan-out makes one per unit.
type WorkflowAgentChildRun struct {
	ItemID        string `json:"itemId"`
	WorkflowID    string `json:"workflowId,omitempty"`
	Goal          string `json:"goal,omitempty"`
	State         string `json:"state"`
	Reason        string `json:"reason,omitempty"`
	ParentPhaseID string `json:"parentPhaseId,omitempty"`
	ParentUnitID  string `json:"parentUnitId,omitempty"`
	ParentAttempt int    `json:"parentAttempt,omitempty"`
}

// WorkflowAgentPhaseDetail is one phase attempt read whole: the outputs its
// envelope declared, how its gate decided, and the units it expanded. Outputs
// are the full values rather than the digest — naming an attempt is how a caller
// says the digest was not enough — bounded only by the envelope size cap every
// envelope was accepted under.
type WorkflowAgentPhaseDetail struct {
	PhaseID string `json:"phaseId"`
	Attempt int    `json:"attempt"`
	Status  string `json:"status"`
	// Provider, Model, and Effort mirror the attempt line's, so a drill-down is
	// readable without pairing it back up with the attempt it came from.
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Effort   string `json:"effort,omitempty"`
	// Cause mirrors the attempt line's: why the ENGINE parked this attempt.
	Cause string `json:"cause,omitempty"`
	// Outputs is empty for an attempt that produced no envelope, and for one
	// that rested on a question or a stuck reason: neither declares outputs.
	Outputs        map[string]json.RawMessage `json:"outputs,omitempty"`
	Decision       string                     `json:"decision,omitempty"`
	DecisionTarget string                     `json:"decisionTarget,omitempty"`
	ExhaustedLoops []string                   `json:"exhaustedLoops,omitempty"`
	// Units is empty for an attempt that is not a fan-out.
	Units []WorkflowAgentUnitView `json:"units"`
}

// WorkflowAgentUnitView is one fan-out unit (or the join) of the inspected
// attempt. Branch and worktree are on it for the same reason they are on the
// run: they are where that unit's work is, and nothing else names them.
type WorkflowAgentUnitView struct {
	UnitID      string `json:"unitId"`
	Kind        string `json:"kind"`
	Status      string `json:"status"`
	UnitAttempt int    `json:"unitAttempt"`
	// Note is the note the unit row carries: for a settled unit, how it ended;
	// for one a repair reopened, what that repair told its next try. It is here
	// because `failed` alone is ambiguous — a pause tears its in-flight units
	// down `failed` with an interrupted note, since there is no interrupted unit
	// status and `failed` is what the repair verbs recover — so a drill-down
	// without it reports the operator's own pause as a wave of agent failures.
	Note         string `json:"note,omitempty"`
	Branch       string `json:"branch,omitempty"`
	WorktreePath string `json:"worktreePath,omitempty"`
	ThreadID     string `json:"threadId,omitempty"`
}

// WorkflowAgentInspectRun is `agent-overflow run inspect`.
//
// LocalOnly: see WorkflowAgentRunStatus. It additionally names local worktree
// paths, which is a fact about this machine's filesystem.
func (a *App) WorkflowAgentInspectRun(ctx context.Context, input WorkflowAgentInspectInput) (WorkflowAgentRunInspection, error) {
	scope, err := requireCallerScope(ctx)
	if err != nil {
		return WorkflowAgentRunInspection{}, err
	}
	item, err := a.scopedRun(scope, input.ItemID, "workflow run inspect", true)
	if err != nil {
		return WorkflowAgentRunInspection{}, err
	}
	phaseID := strings.TrimSpace(input.PhaseID)
	if phaseID == "" && input.Attempt != 0 {
		return WorkflowAgentRunInspection{}, fmt.Errorf(
			"workflow run inspect %s: an attempt names an attempt OF a phase; supply the phase id too", item.ID)
	}
	view, err := a.workflowAgentRunDetail(ctx, item)
	if err != nil {
		return WorkflowAgentRunInspection{}, err
	}
	children, err := a.workflowAgentChildRuns(item.ID)
	if err != nil {
		return WorkflowAgentRunInspection{}, err
	}
	guidance, err := a.workflowInspectGuidance(item.ID)
	if err != nil {
		return WorkflowAgentRunInspection{}, err
	}
	inspection := WorkflowAgentRunInspection{
		Run: view, WorktreePath: item.WorktreePath, Branch: item.Branch, BaseBranch: item.BaseBranch,
		Children: children, Guidance: guidance,
	}
	timeline, err := a.store.ListWorkItemPhaseTimeline(item.ID)
	if err != nil {
		return WorkflowAgentRunInspection{}, err
	}
	if phaseID == "" {
		// The digest is what stands in for the outputs nobody asked for by name.
		// Computing it alongside a drill-down would print the same values twice.
		if err := workflowAttachLatestDigests(item.ID, inspection.Run.Phases, timeline); err != nil {
			return WorkflowAgentRunInspection{}, err
		}
		return inspection, nil
	}
	detail, err := a.workflowAgentPhaseDetail(item.ID, phaseID, input.Attempt, inspection.Run.Phases, timeline)
	if err != nil {
		return WorkflowAgentRunInspection{}, err
	}
	inspection.Phase = detail
	return inspection, nil
}

// workflowAgentChildRuns projects the runs one run called.
func (a *App) workflowAgentChildRuns(itemID string) ([]WorkflowAgentChildRun, error) {
	children, err := a.store.ListWorkItemChildren(itemID)
	if err != nil {
		return nil, err
	}
	views := make([]WorkflowAgentChildRun, 0, len(children))
	for _, child := range children {
		views = append(views, WorkflowAgentChildRun{
			ItemID: child.ID, WorkflowID: child.WorkflowID, Goal: child.Goal,
			State: child.State, Reason: child.Reason,
			ParentPhaseID: child.ParentPhaseID, ParentUnitID: child.ParentUnitID,
			ParentAttempt: child.ParentAttempt,
		})
	}
	return views, nil
}

// workflowAgentPhaseDetail resolves the attempt a caller named. A zero attempt
// means the latest, which is what a reader deciding a parked gate wants; an
// attempt that does not exist is refused by name rather than answered with an
// empty document, because "that attempt produced nothing" and "you asked for an
// attempt this run never made" are different answers.
func (a *App) workflowAgentPhaseDetail(
	itemID, phaseID string, attempt int,
	attempts []WorkflowAgentPhaseAttempt, timeline []store.WorkItemPhaseTimeline,
) (*WorkflowAgentPhaseDetail, error) {
	selected, err := selectWorkflowAttempt(itemID, phaseID, attempt, attempts)
	if err != nil {
		return nil, err
	}
	detail := &WorkflowAgentPhaseDetail{
		PhaseID: selected.PhaseID, Attempt: selected.Attempt, Status: selected.Status,
		Provider: selected.Provider, Model: selected.Model, Effort: selected.Effort,
		Cause:    selected.Cause,
		Decision: selected.Decision, DecisionTarget: selected.DecisionTarget,
		ExhaustedLoops: selected.ExhaustedLoops,
	}
	for _, phase := range timeline {
		if phase.PhaseID != selected.PhaseID || phase.Attempt != selected.Attempt {
			continue
		}
		outputs, err := workflowAttemptOutputs(itemID, phase.PhaseID, phase.Attempt, phase.OutputEnvelope)
		if err != nil {
			return nil, err
		}
		detail.Outputs = outputs
		break
	}
	units, err := a.store.ListWorkItemPhaseUnits(itemID, selected.PhaseID, selected.Attempt)
	if err != nil {
		return nil, err
	}
	detail.Units = make([]WorkflowAgentUnitView, 0, len(units))
	for _, unit := range units {
		detail.Units = append(detail.Units, WorkflowAgentUnitView{
			UnitID: unit.UnitID, Kind: unit.Kind, Status: unit.Status, UnitAttempt: unit.UnitAttempt,
			Note:   strings.TrimSpace(unit.Feedback),
			Branch: unit.Branch, WorktreePath: unit.WorktreePath, ThreadID: unit.ThreadID,
		})
	}
	return detail, nil
}

// selectWorkflowAttempt picks the attempt a caller named out of a run's
// provenance. Both refusals name what the run actually has: an agent that
// mistyped a phase id learns the ids from the refusal instead of from a second
// command.
func selectWorkflowAttempt(
	itemID, phaseID string, attempt int, attempts []WorkflowAgentPhaseAttempt,
) (WorkflowAgentPhaseAttempt, error) {
	var selected WorkflowAgentPhaseAttempt
	found, phaseSeen := false, false
	for _, candidate := range attempts {
		if candidate.PhaseID != phaseID {
			continue
		}
		phaseSeen = true
		if attempt != 0 && candidate.Attempt != attempt {
			continue
		}
		if !found || candidate.Attempt > selected.Attempt {
			selected, found = candidate, true
		}
	}
	switch {
	case found:
		return selected, nil
	case phaseSeen:
		return WorkflowAgentPhaseAttempt{}, fmt.Errorf(
			"workflow run %s: phase %q has no attempt %d; it has %s",
			itemID, phaseID, attempt, describeWorkflowAttempts(phaseID, attempts))
	default:
		// A named attempt of a phase the run never entered is the same mistake as
		// naming the phase alone, so it gets the same answer: the phases it has.
		return WorkflowAgentPhaseAttempt{}, fmt.Errorf(
			"workflow run %s has no phase %q; it has %s", itemID, phaseID, describeWorkflowPhases(attempts))
	}
}

// describeWorkflowPhases lists the phase ids a run has attempted, so a refusal
// carries the answer to the question the caller was really asking.
func describeWorkflowPhases(attempts []WorkflowAgentPhaseAttempt) string {
	seen := make(map[string]struct{}, len(attempts))
	ids := make([]string, 0, len(attempts))
	for _, attempt := range attempts {
		if _, repeated := seen[attempt.PhaseID]; repeated {
			continue
		}
		seen[attempt.PhaseID] = struct{}{}
		ids = append(ids, attempt.PhaseID)
	}
	if len(ids) == 0 {
		return "no phase attempts at all"
	}
	sort.Strings(ids)
	return "phases " + strings.Join(ids, ", ")
}

func describeWorkflowAttempts(phaseID string, attempts []WorkflowAgentPhaseAttempt) string {
	numbers := make([]string, 0, len(attempts))
	for _, attempt := range attempts {
		if attempt.PhaseID == phaseID {
			numbers = append(numbers, fmt.Sprintf("%d", attempt.Attempt))
		}
	}
	if len(numbers) == 0 {
		return "none"
	}
	return "attempts " + strings.Join(numbers, ", ")
}
