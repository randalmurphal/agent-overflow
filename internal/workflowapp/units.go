package workflowapp

import (
	"errors"
	"fmt"
	"strings"

	"agent-overflow/internal/store"
)

// FailedUnits returns the failed units of the run's current phase attempt.
func (s *Service) FailedUnits(itemID string) ([]store.WorkItemUnit, error) {
	database, err := s.store()
	if err != nil {
		return nil, err
	}
	phases, err := database.ListWorkItemPhases(itemID)
	if err != nil {
		return nil, fmt.Errorf("list phases of %s: %w", itemID, err)
	}
	current, ok := currentPhaseAttempt(phases)
	if !ok {
		return nil, nil
	}
	units, err := database.ListWorkItemPhaseUnits(itemID, current.PhaseID, current.Attempt)
	if err != nil {
		return nil, fmt.Errorf("list fan-out units of %s: %w", itemID, err)
	}
	failed := make([]store.WorkItemUnit, 0, len(units))
	for _, unit := range units {
		if unit.Status == store.WorkItemUnitFailed {
			failed = append(failed, unit)
		}
	}
	return failed, nil
}

// CurrentUnit resolves one unit of a run's current phase attempt.
func (s *Service) CurrentUnit(itemID, unitID string) (store.WorkItemUnit, error) {
	database, err := s.store()
	if err != nil {
		return store.WorkItemUnit{}, fmt.Errorf("workflow store unavailable")
	}
	phases, err := database.ListWorkItemPhases(itemID)
	if err != nil {
		return store.WorkItemUnit{}, err
	}
	current, ok := currentPhaseAttempt(phases)
	if !ok {
		return store.WorkItemUnit{}, fmt.Errorf("item %s has no phase attempt", itemID)
	}
	unit, found, err := database.GetWorkItemUnit(itemID, current.PhaseID, current.Attempt, unitID)
	if err != nil {
		return store.WorkItemUnit{}, err
	}
	if !found {
		return store.WorkItemUnit{}, fmt.Errorf(
			"unit %q is not part of attempt %s/%d of item %s", unitID, current.PhaseID, current.Attempt, itemID,
		)
	}
	return unit, nil
}

func (s *Service) UnitThreadUnderTakeover(itemID, unitID string) string {
	unit, err := s.CurrentUnit(itemID, unitID)
	if err != nil || unit.Status != store.WorkItemUnitTakenOver {
		return ""
	}
	return unit.ThreadID
}

func currentPhaseAttempt(phases []store.WorkItemPhase) (store.WorkItemPhase, bool) {
	for index := len(phases) - 1; index >= 0; index-- {
		if phases[index].Status == "running" {
			return phases[index], true
		}
	}
	if len(phases) == 0 {
		return store.WorkItemPhase{}, false
	}
	return phases[len(phases)-1], true
}

func (s *Service) removeUnitWorktrees(item store.WorkItem) error {
	database, err := s.store()
	if err != nil {
		return err
	}
	units, err := database.ListWorkItemUnits(item.ID)
	if err != nil {
		return fmt.Errorf("list fan-out units: %w", err)
	}
	held := false
	for _, unit := range units {
		if strings.TrimSpace(unit.WorktreePath) != "" {
			held = true
			break
		}
	}
	if !held {
		return nil
	}
	project, err := database.GetProject(item.ProjectID)
	if err != nil {
		return fmt.Errorf("load project: %w", err)
	}
	client, err := s.git()
	if err != nil {
		return err
	}
	var errs []error
	for _, unit := range units {
		if strings.TrimSpace(unit.WorktreePath) == "" {
			continue
		}
		if err := client.RemoveWorktreeForce(project.Path, unit.WorktreePath, true); err != nil {
			errs = append(errs, fmt.Errorf("remove unit %q worktree %q: %w", unit.UnitID, unit.WorktreePath, err))
			continue
		}
		if err := database.AttachWorkItemUnitWorkspace(
			unit.ItemID, unit.PhaseID, unit.Attempt, unit.UnitID, unit.Branch, "",
		); err != nil {
			errs = append(errs, fmt.Errorf("clear unit %q worktree path: %w", unit.UnitID, err))
		}
	}
	return errors.Join(errs...)
}
