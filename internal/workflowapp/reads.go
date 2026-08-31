package workflowapp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"agent-overflow/internal/store"
	"agent-overflow/internal/untrustedtext"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflowhost"
)

const (
	maxGuidanceEntryRunes = 400
)

func workflowRunResting(state string) bool { return engine.State(state) != engine.StateRunning }

func workflowAgentRunView(item store.WorkItem) RunView {
	return RunView{
		ItemID: item.ID, WorkflowID: item.WorkflowID, Goal: item.Goal,
		State: item.State, Reason: item.Reason,
		CurrentPhaseID: item.CurrentPhaseID, CurrentPhaseOrdinal: item.CurrentPhaseOrdinal,
		PhaseCount: item.PhaseCount, ParentItemID: item.ParentItemID,
		Resting: workflowRunResting(item.State), StartedAt: item.StartedAt, EndedAt: item.EndedAt,
	}
}

// RunStatus returns the persisted single-run projection authorized by the
// caller scope carried on ctx.
func (s *Service) RunStatus(ctx context.Context, itemID string) (RunView, error) {
	scope, err := requireCallerScope(ctx)
	if err != nil {
		return RunView{}, err
	}
	item, err := s.scopedRun(scope, itemID, "workflow run status", true)
	if err != nil {
		return RunView{}, err
	}
	return s.runDetail(ctx, item)
}

func (s *Service) runDetail(ctx context.Context, summary store.WorkItem) (RunView, error) {
	database, err := s.store()
	if err != nil {
		return RunView{}, err
	}
	item, err := database.GetWorkItem(summary.ID)
	if err != nil {
		return RunView{}, err
	}
	view := workflowAgentRunView(summary)
	view.Seeds = item.Seeds
	if s.deps.RunBudget != nil {
		view.Budget, err = s.deps.RunBudget(ctx, item)
		if err != nil {
			return RunView{}, err
		}
	}
	view.Phases, err = s.PhaseAttempts(summary.ID)
	if err != nil {
		return RunView{}, err
	}
	pending, err := s.pendingGuidance(item.ID)
	if err != nil {
		return RunView{}, err
	}
	view.PendingGuidance = len(pending)
	if engine.State(summary.State) == engine.StateNeedsHuman && engine.Reason(summary.Reason) == engine.ReasonUnitFailed {
		units, err := s.FailedUnits(summary.ID)
		if err != nil {
			return RunView{}, err
		}
		for _, unit := range units {
			view.FailedUnits = append(view.FailedUnits, FailedUnit{
				UnitID: unit.UnitID, UnitAttempt: unit.UnitAttempt, Note: strings.TrimSpace(unit.Feedback),
			})
		}
	}
	return view, nil
}

// ListRuns returns compact project-scoped summaries without per-run fan-out.
func (s *Service) ListRuns(ctx context.Context, activeOnly bool) ([]RunView, error) {
	scope, err := requireCallerScope(ctx)
	if err != nil {
		return nil, err
	}
	if scope.IsPhase() && !scope.HasGrant(string(def.GrantIntrospect)) {
		return nil, fmt.Errorf("workflow run list: this phase does not hold the %q grant", def.GrantIntrospect)
	}
	database, err := s.store()
	if err != nil {
		return nil, err
	}
	filter := store.WorkItemListFilter{ProjectID: scope.ProjectID}
	if activeOnly {
		filter.States = []string{string(engine.StateRunning), string(engine.StateNeedsHuman)}
	}
	items, err := database.ListWorkItemSummaries(filter)
	if err != nil {
		return nil, err
	}
	views := make([]RunView, 0, len(items))
	for _, item := range items {
		views = append(views, workflowAgentRunView(item))
	}
	return views, nil
}

// RunOutput resolves declared outputs and artifact names for one authorized run.
func (s *Service) RunOutput(ctx context.Context, itemID string) (RunOutputs, error) {
	scope, err := requireCallerScope(ctx)
	if err != nil {
		return RunOutputs{}, err
	}
	item, err := s.scopedRun(scope, itemID, "workflow run output", true)
	if err != nil {
		return RunOutputs{}, err
	}
	database, err := s.store()
	if err != nil {
		return RunOutputs{}, err
	}
	full, err := database.GetWorkItem(item.ID)
	if err != nil {
		return RunOutputs{}, err
	}
	phases, err := database.ListWorkItemPhaseTimeline(item.ID)
	if err != nil {
		return RunOutputs{}, err
	}
	outputs, err := workflowNamedOutputs(full.Snapshot, phases)
	if err != nil {
		return RunOutputs{}, fmt.Errorf("workflow run output %s: %w", item.ID, err)
	}
	artifacts, err := workflowhost.ListArtifacts(s.dataRoot(), item.ID)
	if err != nil {
		return RunOutputs{}, err
	}
	names := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		names = append(names, artifact.Name)
	}
	return RunOutputs{
		ItemID: item.ID, State: item.State, Reason: item.Reason,
		Resting: workflowRunResting(item.State), Outputs: outputs, Artifacts: names,
	}, nil
}

func workflowNamedOutputs(payload json.RawMessage, phases []store.WorkItemPhaseTimeline) (map[string]any, error) {
	outputs := make(map[string]any)
	if len(payload) == 0 {
		return outputs, nil
	}
	var snapshot engine.Snapshot
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		return nil, err
	}
	vars := make(map[string]any)
	latestAttempts := make(map[string]int)
	for _, phase := range phases {
		if (phase.Status != "completed" && phase.Status != "failed") || len(phase.OutputEnvelope) == 0 || phase.Attempt < latestAttempts[phase.PhaseID] {
			continue
		}
		var envelope struct {
			Status  string         `json:"status"`
			Outputs map[string]any `json:"outputs"`
		}
		if err := json.Unmarshal(phase.OutputEnvelope, &envelope); err != nil {
			return nil, fmt.Errorf("decode phase %s attempt %d: %w", phase.PhaseID, phase.Attempt, err)
		}
		if envelope.Status != "done" {
			continue
		}
		latestAttempts[phase.PhaseID] = phase.Attempt
		for name, value := range envelope.Outputs {
			if value != nil {
				vars[phase.PhaseID+"."+name] = value
			}
		}
	}
	for name, declaration := range snapshot.Workflow.Outputs {
		if declaration.Artifact {
			continue
		}
		if value, ok := def.LookupVariable(vars, declaration.From); ok {
			outputs[name] = value
		}
	}
	return outputs, nil
}

// InspectRun returns a compact run view plus persisted coordinates and an
// optional full attempt drill-down.
func (s *Service) InspectRun(ctx context.Context, input InspectInput) (RunInspection, error) {
	scope, err := requireCallerScope(ctx)
	if err != nil {
		return RunInspection{}, err
	}
	item, err := s.scopedRun(scope, input.ItemID, "workflow run inspect", true)
	if err != nil {
		return RunInspection{}, err
	}
	phaseID := strings.TrimSpace(input.PhaseID)
	if phaseID == "" && input.Attempt != 0 {
		return RunInspection{}, fmt.Errorf(
			"workflow run inspect %s: an attempt names an attempt OF a phase; supply the phase id too", item.ID)
	}
	view, err := s.runDetail(ctx, item)
	if err != nil {
		return RunInspection{}, err
	}
	children, err := s.childRuns(item.ID)
	if err != nil {
		return RunInspection{}, err
	}
	guidance, err := s.inspectGuidance(item.ID)
	if err != nil {
		return RunInspection{}, err
	}
	inspection := RunInspection{
		Run: view, WorktreePath: item.WorktreePath, Branch: item.Branch,
		BaseBranch: item.BaseBranch, Children: children, Guidance: guidance,
	}
	database, err := s.store()
	if err != nil {
		return RunInspection{}, err
	}
	timeline, err := database.ListWorkItemPhaseTimeline(item.ID)
	if err != nil {
		return RunInspection{}, err
	}
	if phaseID == "" {
		if err := workflowAttachLatestDigests(item.ID, inspection.Run.Phases, timeline); err != nil {
			return RunInspection{}, err
		}
		return inspection, nil
	}
	inspection.Phase, err = s.phaseDetail(item.ID, phaseID, input.Attempt, inspection.Run.Phases, timeline)
	if err != nil {
		return RunInspection{}, err
	}
	return inspection, nil
}

func (s *Service) childRuns(itemID string) ([]ChildRun, error) {
	database, err := s.store()
	if err != nil {
		return nil, err
	}
	children, err := database.ListWorkItemChildren(itemID)
	if err != nil {
		return nil, err
	}
	views := make([]ChildRun, 0, len(children))
	for _, child := range children {
		views = append(views, ChildRun{
			ItemID: child.ID, WorkflowID: child.WorkflowID, Goal: child.Goal,
			State: child.State, Reason: child.Reason, ParentPhaseID: child.ParentPhaseID,
			ParentUnitID: child.ParentUnitID, ParentAttempt: child.ParentAttempt,
		})
	}
	return views, nil
}

func (s *Service) phaseDetail(itemID, phaseID string, attempt int, attempts []PhaseAttempt, timeline []store.WorkItemPhaseTimeline) (*PhaseDetail, error) {
	selected, err := selectWorkflowAttempt(itemID, phaseID, attempt, attempts)
	if err != nil {
		return nil, err
	}
	detail := &PhaseDetail{
		PhaseID: selected.PhaseID, Attempt: selected.Attempt, Status: selected.Status,
		Provider: selected.Provider, Model: selected.Model, Effort: selected.Effort,
		Cause: selected.Cause, Decision: selected.Decision, DecisionTarget: selected.DecisionTarget,
		ExhaustedLoops: selected.ExhaustedLoops,
	}
	for _, phase := range timeline {
		if phase.PhaseID == selected.PhaseID && phase.Attempt == selected.Attempt {
			detail.Outputs, err = workflowAttemptOutputs(itemID, phase.PhaseID, phase.Attempt, phase.OutputEnvelope)
			if err != nil {
				return nil, err
			}
			break
		}
	}
	database, err := s.store()
	if err != nil {
		return nil, err
	}
	units, err := database.ListWorkItemPhaseUnits(itemID, selected.PhaseID, selected.Attempt)
	if err != nil {
		return nil, err
	}
	detail.Units = make([]UnitView, 0, len(units))
	for _, unit := range units {
		detail.Units = append(detail.Units, UnitView{
			UnitID: unit.UnitID, Kind: unit.Kind, Status: unit.Status, UnitAttempt: unit.UnitAttempt,
			Note: strings.TrimSpace(unit.Feedback), Branch: unit.Branch,
			WorktreePath: unit.WorktreePath, ThreadID: unit.ThreadID,
		})
	}
	return detail, nil
}

func (s *Service) pendingGuidance(itemID string) ([]engine.GuidanceEntry, error) {
	database, err := s.store()
	if err != nil {
		return nil, err
	}
	raw, err := database.WorkItemPendingGuidance(itemID)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var pending []engine.GuidanceEntry
	if err := json.Unmarshal(raw, &pending); err != nil {
		return nil, fmt.Errorf("workflow run %s: pending guidance is unreadable: %w", itemID, err)
	}
	return pending, nil
}

func (s *Service) inspectGuidance(itemID string) ([]GuidanceEntry, error) {
	pending, err := s.pendingGuidance(itemID)
	if err != nil || len(pending) == 0 {
		return nil, err
	}
	now := s.deps.Now().UnixMilli()
	entries := make([]GuidanceEntry, 0, len(pending))
	for _, entry := range pending {
		age := (now - entry.At) / 1000
		if age < 0 {
			age = 0
		}
		entries = append(entries, GuidanceEntry{
			Text: untrustedtext.Truncate(entry.Text, maxGuidanceEntryRunes), At: entry.At,
			AgeSeconds: age, By: string(entry.By), ByRun: entry.ByRun,
		})
	}
	return entries, nil
}
