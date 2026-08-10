package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/aocli"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/profile"
	"agent-overflow/internal/workflow/scheduler"
)

// The methods the `ao` CLI calls. Each one takes its project from the caller's
// scope rather than from an argument: the credential is what says which project
// this session may touch, and an argument would be the caller's claim about it.

// WorkflowAgentStartInput is `ao run start`. Scope is optional — omitted, the
// workflow id resolves by §8 precedence (project scope wins over shared),
// exactly as a call phase's static target does.
type WorkflowAgentStartInput struct {
	WorkflowID string          `json:"workflowId"`
	Scope      string          `json:"scope,omitempty"`
	Goal       string          `json:"goal,omitempty"`
	Seeds      json.RawMessage `json:"seeds,omitempty"`
	BaseBranch string          `json:"baseBranch,omitempty"`
	StepMode   bool            `json:"stepMode,omitempty"`
}

// WorkflowAgentStartResult is what `ao run start` prints. Skipped marks a
// surface-and-skip replay: the phase asked for something it had already done,
// so nothing new started and ItemID names the original run.
type WorkflowAgentStartResult struct {
	ItemID         string `json:"itemId"`
	WorkflowID     string `json:"workflowId"`
	WorkflowScope  string `json:"workflowScope"`
	State          string `json:"state"`
	Skipped        bool   `json:"skipped,omitempty"`
	BoundThreadID  string `json:"boundThreadId,omitempty"`
	BindingWarning string `json:"bindingWarning,omitempty"`
}

// WorkflowAgentRunView is the compact run projection `ao run status` and
// `ao run list` render. It deliberately excludes envelopes, snapshots, and
// worktree paths: an agent asking "where is this run" does not need a run's
// whole history crossing into its context.
type WorkflowAgentRunView struct {
	ItemID              string `json:"itemId"`
	WorkflowID          string `json:"workflowId"`
	Goal                string `json:"goal"`
	State               string `json:"state"`
	Reason              string `json:"reason,omitempty"`
	CurrentPhaseID      string `json:"currentPhaseId,omitempty"`
	CurrentPhaseOrdinal int    `json:"currentPhaseOrdinal,omitempty"`
	PhaseCount          int    `json:"phaseCount,omitempty"`
	ParentItemID        string `json:"parentItemId,omitempty"`
	Resting             bool   `json:"resting"`
	StartedAt           int64  `json:"startedAt,omitempty"`
	EndedAt             int64  `json:"endedAt,omitempty"`
	// Seeds is what the run was started with, populated only by the single-run
	// reads. It is the run's own frozen input, so a caller reconstructing what a
	// wave was asked for has it without a second surface; the listing projection
	// does not carry it at all, because a summary row blanks the column.
	Seeds json.RawMessage `json:"seeds,omitempty"`
	// FailedUnits names the units `run retry-unit` takes — the attempt's join
	// among them when it is what failed — populated only by
	// `run status` on a run resting needs-human(unit-failed). The reason already
	// says a fan-out needs repair; without the ids the caller has to find them in
	// the app, which is the one thing an agent holding a CLI cannot do. It is
	// deliberately absent from `run list` — one extra query per run is a fan-out
	// of its own, and a list is for locating a run, not for repairing one.
	FailedUnits []WorkflowAgentFailedUnit `json:"failedUnits,omitempty"`
	// Phases is the run's per-attempt provenance, populated only by `run status`
	// for the same reason FailedUnits is: it costs one extra query per run, and a
	// list is for locating a run rather than reading one.
	Phases []WorkflowAgentPhaseAttempt `json:"phases,omitempty"`
	// Budget is the ceiling in force and the tree's spend against it, absent on
	// a run that has none. Populated by the single-run reads only: resolving it
	// costs a root lookup, a profile read, and — for a run that HAS a ceiling —
	// the same tree-spend aggregate the engine's check runs, which is a per-run
	// fan-out a listing must not pay.
	Budget *WorkflowAgentRunBudget `json:"budget,omitempty"`
	// PendingGuidance counts the `run guide` entries waiting for this run's next
	// fresh phase entry. It is a COUNT here and the entries themselves are on
	// `run inspect`: the number is what a reader of a run's state needs — an
	// operator about to leave a fourth steer, or one wondering why the run has
	// not turned yet — while the text of what somebody else left is a read of
	// its own. Populated by the single-run reads only, like the two fields above.
	PendingGuidance int `json:"pendingGuidance,omitempty"`
}

// WorkflowAgentFailedUnit is one unit of a parked fan-out that is resting
// failed. The attempt count rides along because "this unit has already been
// retried twice" is what decides between retrying it again and reading it.
type WorkflowAgentFailedUnit struct {
	UnitID      string `json:"unitId"`
	UnitAttempt int    `json:"unitAttempt"`
}

// WorkflowAgentRunOutputs is `ao run output`: the run's declared outputs plus
// the artifact file names it produced.
type WorkflowAgentRunOutputs struct {
	ItemID    string         `json:"itemId"`
	State     string         `json:"state"`
	Reason    string         `json:"reason,omitempty"`
	Resting   bool           `json:"resting"`
	Outputs   map[string]any `json:"outputs"`
	Artifacts []string       `json:"artifacts"`
}

// WorkflowAgentScheduleInput is `ao schedule`.
type WorkflowAgentScheduleInput struct {
	WorkflowID string          `json:"workflowId"`
	Scope      string          `json:"scope,omitempty"`
	Name       string          `json:"name,omitempty"`
	Cron       string          `json:"cron"`
	Seeds      json.RawMessage `json:"seeds,omitempty"`
}

// WorkflowAgentScheduleResult names the automation, skipped on a replay.
type WorkflowAgentScheduleResult struct {
	AutomationID string `json:"automationId"`
	Name         string `json:"name"`
	Cron         string `json:"cron"`
	Skipped      bool   `json:"skipped,omitempty"`
}

// WorkflowAgentNotesResult is `ao notes set`.
type WorkflowAgentNotesResult struct {
	AutomationID string `json:"automationId"`
	Skipped      bool   `json:"skipped,omitempty"`
}

// workflowRunResting reports whether a run has stopped doing work — the
// condition `ao run wait` returns on. `needs-human` counts: a run parked on a
// gate or a question is waiting for someone, and blocking on it forever would
// strand the caller.
func workflowRunResting(state string) bool {
	return engine.State(state) != engine.StateRunning
}

func workflowAgentRunView(item store.WorkItem) WorkflowAgentRunView {
	return WorkflowAgentRunView{
		ItemID: item.ID, WorkflowID: item.WorkflowID, Goal: item.Goal,
		State: item.State, Reason: item.Reason,
		CurrentPhaseID: item.CurrentPhaseID, CurrentPhaseOrdinal: item.CurrentPhaseOrdinal,
		PhaseCount: item.PhaseCount, ParentItemID: item.ParentItemID,
		Resting: workflowRunResting(item.State), StartedAt: item.StartedAt, EndedAt: item.EndedAt,
	}
}

// WorkflowAgentStartRun starts a run on behalf of an agent session. An
// interactive caller's run binds to its thread (D17); a granted phase's run is
// recorded in the effect ledger so re-entering the phase surfaces the prior
// start instead of firing a second one (§5).
//
// LocalOnly: it starts autonomous provider sessions, like every other entry to
// the workflow start path.
func (a *App) WorkflowAgentStartRun(ctx context.Context, input WorkflowAgentStartInput) (WorkflowAgentStartResult, error) {
	scope, err := requireCallerScope(ctx)
	if err != nil {
		return WorkflowAgentStartResult{}, err
	}
	resolved, err := a.resolveAgentWorkflow(scope, input.WorkflowID, input.Scope)
	if err != nil {
		return WorkflowAgentStartResult{}, err
	}
	seedValues, seeds, err := decodeWorkflowSeeds(input.Seeds)
	if err != nil {
		return WorkflowAgentStartResult{}, fmt.Errorf("start workflow run: %w", err)
	}
	goal := strings.TrimSpace(input.Goal)
	if goal == "" {
		// The overlay lists runs by goal; falling back to the workflow's own
		// name keeps an agent-started run identifiable without forcing every
		// caller to invent prose.
		goal = strings.TrimSpace(resolved.Workflow.Name)
	}
	baseBranch := strings.TrimSpace(input.BaseBranch)
	hash, err := effectPayloadHash(map[string]any{
		"workflow": resolved.Workflow.ID, "scope": string(resolved.Scope), "goal": goal,
		"seeds": seedValues, "baseBranch": baseBranch, "stepMode": input.StepMode,
	})
	if err != nil {
		return WorkflowAgentStartResult{}, err
	}
	prior, found, err := a.priorStartedRun(scope, hash)
	if err != nil {
		return WorkflowAgentStartResult{}, err
	}
	if found {
		return WorkflowAgentStartResult{
			ItemID: prior.ID, WorkflowID: prior.WorkflowID, WorkflowScope: prior.WorkflowScope,
			State: prior.State, Skipped: true,
		}, nil
	}

	sourceRef := ""
	if scope.IsPhase() {
		sourceRef = phaseSourceRef(scope, hash)
	}
	item, err := a.startWorkflowRun(
		scope.ProjectID, resolved.Workflow.ID, string(resolved.Scope), goal, seeds,
		(*profile.Budget)(nil), baseBranch, input.StepMode || resolved.Workflow.DefaultStepMode,
		workflowSourceAgent, sourceRef,
	)
	if err != nil {
		return WorkflowAgentStartResult{}, err
	}
	result := WorkflowAgentStartResult{
		ItemID: item.ID, WorkflowID: item.WorkflowID, WorkflowScope: item.WorkflowScope,
		State: item.State,
	}
	if warning := a.bindOriginThread(scope, item); warning != "" {
		result.BindingWarning = warning
	} else if !scope.IsPhase() {
		result.BoundThreadID = scope.ThreadID
	}
	if err := a.recordEffect(scope, workflowEffectStartRun, hash, workflowEffect{
		Args:   map[string]any{"workflow": item.WorkflowID, "goal": item.Goal},
		Result: map[string]any{"itemId": item.ID},
	}); err != nil {
		// The run is already going and its source ref already carries this key,
		// so re-entry still surfaces it. What is lost is the ledger's record of
		// what was asked for, which a human reads. Report rather than fail.
		log.Printf("workflow: run %s started but its effect record did not persist: %v", item.ID, err)
		result.BindingWarning = strings.TrimSpace(result.BindingWarning + " " +
			"the run started, but its effect record could not be written; check the app log")
	}
	return result, nil
}

// priorStartedRun answers the surface-and-skip question for a run start. It asks
// twice on purpose: the effect ledger is the fast path and the record a human
// reads, while the run's own source ref is the durable one — a crash between the
// run committing and its ledger entry landing must not license a second start.
// An interactive scope has neither, by design: a human-approved invocation
// replays when the human runs it again.
func (a *App) priorStartedRun(scope transport.CallerScope, hash string) (store.WorkItem, bool, error) {
	if !scope.IsPhase() {
		return store.WorkItem{}, false, nil
	}
	effect, found, err := a.priorEffect(scope, workflowEffectStartRun, hash)
	if err != nil {
		return store.WorkItem{}, false, err
	}
	if found {
		itemID, _ := effect.Result["itemId"].(string)
		if itemID == "" {
			return store.WorkItem{}, false, fmt.Errorf(
				"start workflow run: a prior effect is recorded for these arguments but names no run")
		}
		item, err := a.store.GetWorkItem(itemID)
		if err != nil {
			return store.WorkItem{}, false, fmt.Errorf("start workflow run: load prior run %s: %w", itemID, err)
		}
		return item, true, nil
	}
	return a.store.GetWorkItemBySourceRef(workflowSourceAgent, phaseSourceRef(scope, hash))
}

// resolveAgentWorkflow resolves the id the caller named, honouring an explicit
// scope and otherwise §8 precedence. Resolving here — rather than letting the
// start path do it — is what lets the effect hash and the run row agree on which
// definition was meant.
func (a *App) resolveAgentWorkflow(scope transport.CallerScope, workflowID, workflowScope string) (def.ResolvedWorkflow, error) {
	workflowID = strings.TrimSpace(workflowID)
	if workflowID == "" {
		return def.ResolvedWorkflow{}, fmt.Errorf("workflow id is required")
	}
	projectRow, err := a.store.GetProject(scope.ProjectID)
	if err != nil {
		return def.ResolvedWorkflow{}, err
	}
	declared := def.Scope(strings.TrimSpace(workflowScope))
	if declared != "" {
		if declared != def.ScopeProject && declared != def.ScopeShared {
			return def.ResolvedWorkflow{}, fmt.Errorf("scope must be project or shared")
		}
		return aocli.ResolveWorkflow(a.workflowDataRoot(), projectRow.Slug, workflowID, declared)
	}
	calls, err := aocli.NewCallResolver(a.workflowDataRoot(), projectRow.Slug)
	if err != nil {
		return def.ResolvedWorkflow{}, err
	}
	return calls.ResolveCall(workflowID)
}

// WorkflowAgentRunStatus is `ao run status` / the poll behind `ao run wait`.
//
// LocalOnly: the whole ao surface is reachable only through credentials minted
// for local provider processes.
func (a *App) WorkflowAgentRunStatus(ctx context.Context, itemID string) (WorkflowAgentRunView, error) {
	scope, err := requireCallerScope(ctx)
	if err != nil {
		return WorkflowAgentRunView{}, err
	}
	item, err := a.scopedRun(scope, itemID, "workflow run status", true)
	if err != nil {
		return WorkflowAgentRunView{}, err
	}
	return a.workflowAgentRunDetail(ctx, item)
}

// workflowAgentRunDetail is the single-run projection `run status` prints and
// `run inspect` builds on, so the two cannot disagree about what reading one run
// answers. `item` is the row scopedRun already loaded: the summary projection
// carries the phase progress GetWorkItem does not compute, and blanks the seeds
// column that only the full row has.
func (a *App) workflowAgentRunDetail(ctx context.Context, item store.WorkItem) (WorkflowAgentRunView, error) {
	summary, err := a.store.GetWorkItemSummary(item.ID)
	if err != nil {
		return WorkflowAgentRunView{}, err
	}
	view := workflowAgentRunView(summary)
	view.Seeds = item.Seeds
	budget, err := a.workflowRunBudget(ctx, item)
	if err != nil {
		return WorkflowAgentRunView{}, err
	}
	view.Budget = budget
	phases, err := a.workflowAgentPhaseAttempts(summary.ID)
	if err != nil {
		return WorkflowAgentRunView{}, err
	}
	view.Phases = phases
	pending, err := a.workflowPendingGuidance(item.ID)
	if err != nil {
		return WorkflowAgentRunView{}, err
	}
	view.PendingGuidance = len(pending)
	if engine.State(summary.State) == engine.StateNeedsHuman && engine.Reason(summary.Reason) == engine.ReasonUnitFailed {
		units, err := a.workflowFailedUnits(summary.ID)
		if err != nil {
			return WorkflowAgentRunView{}, err
		}
		for _, unit := range units {
			view.FailedUnits = append(view.FailedUnits,
				WorkflowAgentFailedUnit{UnitID: unit.UnitID, UnitAttempt: unit.UnitAttempt})
		}
	}
	return view, nil
}

// WorkflowAgentListRuns is `ao run list`, scoped to the caller's project.
//
// LocalOnly: see WorkflowAgentRunStatus.
func (a *App) WorkflowAgentListRuns(ctx context.Context, activeOnly bool) ([]WorkflowAgentRunView, error) {
	scope, err := requireCallerScope(ctx)
	if err != nil {
		return nil, err
	}
	// Listing is project-wide by definition, so unlike the per-run reads there
	// is no narrower answer for a phase without `introspect`: it is refused
	// outright rather than handed an empty list it would read as "no runs".
	if scope.IsPhase() && !scope.HasGrant(string(def.GrantIntrospect)) {
		return nil, fmt.Errorf("workflow run list: this phase does not hold the %q grant", def.GrantIntrospect)
	}
	filter := store.WorkItemListFilter{ProjectID: scope.ProjectID}
	if activeOnly {
		filter.States = []string{string(engine.StateRunning), string(engine.StateNeedsHuman)}
	}
	items, err := a.store.ListWorkItemSummaries(filter)
	if err != nil {
		return nil, err
	}
	views := make([]WorkflowAgentRunView, 0, len(items))
	for _, item := range items {
		views = append(views, workflowAgentRunView(item))
	}
	return views, nil
}

// WorkflowAgentRunOutput is `ao run output` — the "different context that did
// not start the run" path (D15).
//
// LocalOnly: see WorkflowAgentRunStatus.
func (a *App) WorkflowAgentRunOutput(ctx context.Context, itemID string) (WorkflowAgentRunOutputs, error) {
	scope, err := requireCallerScope(ctx)
	if err != nil {
		return WorkflowAgentRunOutputs{}, err
	}
	item, err := a.scopedRun(scope, itemID, "workflow run output", true)
	if err != nil {
		return WorkflowAgentRunOutputs{}, err
	}
	phases, err := a.store.ListWorkItemPhaseTimeline(item.ID)
	if err != nil {
		return WorkflowAgentRunOutputs{}, err
	}
	outputs, err := workflowNamedOutputs(item.Snapshot, phases)
	if err != nil {
		return WorkflowAgentRunOutputs{}, fmt.Errorf("workflow run output %s: %w", item.ID, err)
	}
	artifacts, err := listWorkflowArtifacts(a.workflowDataRoot(), item.ID)
	if err != nil {
		return WorkflowAgentRunOutputs{}, err
	}
	names := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		names = append(names, artifact.Name)
	}
	return WorkflowAgentRunOutputs{
		ItemID: item.ID, State: item.State, Reason: item.Reason,
		Resting: workflowRunResting(item.State), Outputs: outputs, Artifacts: names,
	}, nil
}

// WorkflowAgentSchedule is `ao schedule`: it creates one enabled cron
// automation through the same validation the overlay's editor uses.
//
// LocalOnly: an automation is a standing instruction to start autonomous
// provider sessions, like every other automation mutation.
func (a *App) WorkflowAgentSchedule(ctx context.Context, input WorkflowAgentScheduleInput) (WorkflowAgentScheduleResult, error) {
	scope, err := requireCallerScope(ctx)
	if err != nil {
		return WorkflowAgentScheduleResult{}, err
	}
	resolved, err := a.resolveAgentWorkflow(scope, input.WorkflowID, input.Scope)
	if err != nil {
		return WorkflowAgentScheduleResult{}, err
	}
	cronExpr := strings.TrimSpace(input.Cron)
	if cronExpr == "" {
		return WorkflowAgentScheduleResult{}, fmt.Errorf("create automation: a cron expression is required")
	}
	seedValues, seeds, err := decodeWorkflowSeeds(input.Seeds)
	if err != nil {
		return WorkflowAgentScheduleResult{}, fmt.Errorf("create automation: %w", err)
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		name = strings.TrimSpace(resolved.Workflow.Name)
	}
	trigger, err := json.Marshal(scheduler.Trigger{Kind: scheduler.KindCron, Expr: cronExpr})
	if err != nil {
		return WorkflowAgentScheduleResult{}, fmt.Errorf("create automation: encode trigger: %w", err)
	}
	hash, err := effectPayloadHash(map[string]any{
		"workflow": resolved.Workflow.ID, "scope": string(resolved.Scope),
		"name": name, "cron": cronExpr, "seeds": seedValues,
	})
	if err != nil {
		return WorkflowAgentScheduleResult{}, err
	}
	if prior, found, err := a.priorEffect(scope, workflowEffectSchedule, hash); err != nil {
		return WorkflowAgentScheduleResult{}, err
	} else if found {
		automationID, _ := prior.Result["automationId"].(string)
		if automationID == "" {
			return WorkflowAgentScheduleResult{}, fmt.Errorf(
				"create automation: a prior effect is recorded for these arguments but names no automation")
		}
		return WorkflowAgentScheduleResult{AutomationID: automationID, Name: name, Cron: cronExpr, Skipped: true}, nil
	}
	view, err := a.WorkflowCreateAutomation(WorkflowAutomationInput{
		ProjectID: scope.ProjectID, WorkflowID: resolved.Workflow.ID,
		WorkflowScope: string(resolved.Scope), Name: name, Enabled: true,
		Trigger: trigger, Seeds: seeds,
	})
	if err != nil {
		return WorkflowAgentScheduleResult{}, err
	}
	if err := a.recordEffect(scope, workflowEffectSchedule, hash, workflowEffect{
		Args:   map[string]any{"workflow": view.WorkflowID, "cron": cronExpr},
		Result: map[string]any{"automationId": view.ID},
	}); err != nil {
		return WorkflowAgentScheduleResult{}, err
	}
	return WorkflowAgentScheduleResult{AutomationID: view.ID, Name: view.Name, Cron: cronExpr}, nil
}

// WorkflowAgentGetNotes is `ao notes get`.
//
// LocalOnly: see WorkflowAgentRunStatus.
func (a *App) WorkflowAgentGetNotes(ctx context.Context, automationID string) (string, error) {
	scope, err := requireCallerScope(ctx)
	if err != nil {
		return "", err
	}
	automation, err := a.scopedAutomation(scope, automationID, "workflow job notes")
	if err != nil {
		return "", err
	}
	return automation.Notes, nil
}

// WorkflowAgentSetNotes is `ao notes set` — the §11 continuity-notes rewrite a
// terminal phase performs. Rewriting the same notes twice is a skip, not a
// second write, so a re-entered phase does not churn the row.
//
// LocalOnly: it mutates local automation state.
func (a *App) WorkflowAgentSetNotes(ctx context.Context, automationID, notes string) (WorkflowAgentNotesResult, error) {
	scope, err := requireCallerScope(ctx)
	if err != nil {
		return WorkflowAgentNotesResult{}, err
	}
	automation, err := a.scopedAutomation(scope, automationID, "workflow job notes")
	if err != nil {
		return WorkflowAgentNotesResult{}, err
	}
	hash, err := effectPayloadHash(map[string]any{"automation": automation.ID, "notes": notes})
	if err != nil {
		return WorkflowAgentNotesResult{}, err
	}
	if _, found, err := a.priorEffect(scope, workflowEffectSetNotes, hash); err != nil {
		return WorkflowAgentNotesResult{}, err
	} else if found {
		return WorkflowAgentNotesResult{AutomationID: automation.ID, Skipped: true}, nil
	}
	if err := a.WorkflowSetJobNotes(automation.ID, notes); err != nil {
		return WorkflowAgentNotesResult{}, err
	}
	if err := a.recordEffect(scope, workflowEffectSetNotes, hash, workflowEffect{
		Args:   map[string]any{"automation": automation.ID},
		Result: map[string]any{"automationId": automation.ID},
	}); err != nil {
		return WorkflowAgentNotesResult{}, err
	}
	return WorkflowAgentNotesResult{AutomationID: automation.ID}, nil
}
