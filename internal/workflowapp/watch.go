package workflowapp

import (
	"context"
	"fmt"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/wake"
	"agent-overflow/internal/workflowwatch"
)

const maxWorkflowWatchHold = 25 * time.Second

// WatchRun long-polls the injected transition ring while reading current state
// and persisted causes from SQLite.
func (s *Service) WatchRun(ctx context.Context, input WatchInput) (WatchResult, error) {
	scope, err := requireCallerScope(ctx)
	if err != nil {
		return WatchResult{}, err
	}
	item, err := s.scopedRun(scope, input.ItemID, "workflow run watch", true)
	if err != nil {
		return WatchResult{}, err
	}
	hold := workflowWatchHold(input.WaitMillis)
	deadline := s.deps.Now().Add(hold)
	expiry := time.NewTimer(hold)
	defer expiry.Stop()
	for {
		changed := s.watch.Wait()
		watched, err := s.watchedRuns(item, input.Tree)
		if err != nil {
			return WatchResult{}, err
		}
		transitions, head, gap := s.watch.Since(input.Cursor, func(itemID string) bool {
			return watched[itemID]
		})
		current, err := s.watchRunState(item.ID)
		if err != nil {
			return WatchResult{}, err
		}
		if input.Cursor == 0 || gap || len(transitions) > 0 || current.Resting || !s.deps.Now().Before(deadline) {
			projected, err := s.watchTransitions(transitions)
			if err != nil {
				return WatchResult{}, err
			}
			return WatchResult{
				ItemID: item.ID, Cursor: head, Run: current, Gap: gap, Transitions: projected,
			}, nil
		}
		select {
		case <-changed:
		case <-expiry.C:
		case <-ctx.Done():
			return WatchResult{
				ItemID: item.ID, Cursor: head, Run: current, Transitions: []Transition{},
			}, nil
		}
	}
}

func workflowWatchHold(requested int64) time.Duration {
	if requested <= 0 || time.Duration(requested)*time.Millisecond > maxWorkflowWatchHold {
		return maxWorkflowWatchHold
	}
	return time.Duration(requested) * time.Millisecond
}

func (s *Service) watchedRuns(item store.WorkItem, tree bool) (map[string]bool, error) {
	if !tree {
		return map[string]bool{item.ID: true}, nil
	}
	members, err := s.RunTreeNodes(item.ID)
	if err != nil {
		return nil, err
	}
	watched := make(map[string]bool, len(members))
	for _, member := range members {
		watched[member.ID] = true
	}
	return watched, nil
}

func (s *Service) watchRunState(itemID string) (WatchRunState, error) {
	database, err := s.store()
	if err != nil {
		return WatchRunState{}, err
	}
	summary, err := database.GetWorkItemSummary(itemID)
	if err != nil {
		return WatchRunState{}, err
	}
	state := WatchRunState{
		ItemID: summary.ID, WorkflowID: summary.WorkflowID, Goal: summary.Goal,
		State: summary.State, Reason: summary.Reason, PhaseID: summary.CurrentPhaseID,
		Resting: workflowRunResting(summary.State),
	}
	if !state.Resting {
		return state, nil
	}
	gateDecision, gateLabel := "", ""
	if engine.Reason(summary.Reason) == engine.ReasonGate {
		gateDecision, gateLabel = s.GateDecision(summary.ID)
	}
	state.Repair = wake.RepairSentence(summary.ID, summary.State, summary.Reason, gateDecision, gateLabel)
	return state, nil
}

func (s *Service) watchTransitions(recorded []workflowwatch.Transition) ([]Transition, error) {
	causes := make(map[string]map[string]string, 1)
	projected := make([]Transition, 0, len(recorded))
	for _, entry := range recorded {
		transition := Transition{
			Seq: entry.Seq, At: entry.At, ItemID: entry.ItemID,
			PhaseID: entry.PhaseID, Attempt: entry.Attempt,
			From: entry.From, To: entry.To, Reason: entry.Reason,
			Resting: workflowRunResting(entry.To),
		}
		if transition.Resting && entry.PhaseID != "" {
			byAttempt, loaded := causes[entry.ItemID]
			if !loaded {
				var err error
				byAttempt, err = s.parkCauses(entry.ItemID)
				if err != nil {
					return nil, err
				}
				causes[entry.ItemID] = byAttempt
			}
			transition.Cause = byAttempt[workflowAttemptKey(entry.PhaseID, entry.Attempt)]
		}
		projected = append(projected, transition)
	}
	return projected, nil
}

func (s *Service) parkCauses(itemID string) (map[string]string, error) {
	database, err := s.store()
	if err != nil {
		return nil, err
	}
	rows, err := database.ListWorkItemPhaseProvenance(itemID)
	if err != nil {
		return nil, err
	}
	causes := make(map[string]string, len(rows))
	for _, row := range rows {
		if row.ParkCause != "" {
			causes[workflowAttemptKey(row.PhaseID, row.Attempt)] = row.ParkCause
		}
	}
	return causes, nil
}

func workflowAttemptKey(phaseID string, attempt int) string {
	return fmt.Sprintf("%s/%d", phaseID, attempt)
}
