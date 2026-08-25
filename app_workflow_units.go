package main

import (
	"errors"
	"fmt"
	"strings"

	"agent-overflow/internal/store"
)

// removeWorkflowUnitWorktrees removes every sub-worktree a run's fan-out units
// still hold, and clears the paths from their rows. It is the item-teardown
// counterpart of retireUnitWorktrees: that one runs when a join consumed its
// units, this one when the whole tree is going away.
//
// Branches are kept here too. Discarding a run removes checkouts, not history;
// the branches go with the repository's own branch hygiene, which is where a
// human can still find what a unit produced.
func (a *App) removeWorkflowUnitWorktrees(item store.WorkItem) error {
	units, err := a.store.ListWorkItemUnits(item.ID)
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
	project, err := a.store.GetProject(item.ProjectID)
	if err != nil {
		return fmt.Errorf("load project: %w", err)
	}
	var errs []error
	for _, unit := range units {
		if strings.TrimSpace(unit.WorktreePath) == "" {
			continue
		}
		if err := a.gitCore().RemoveWorktreeForce(project.Path, unit.WorktreePath, true); err != nil {
			errs = append(errs, fmt.Errorf("remove unit %q worktree %q: %w", unit.UnitID, unit.WorktreePath, err))
			continue
		}
		if err := a.store.AttachWorkItemUnitWorkspace(
			unit.ItemID, unit.PhaseID, unit.Attempt, unit.UnitID, unit.Branch, "",
		); err != nil {
			errs = append(errs, fmt.Errorf("clear unit %q worktree path: %w", unit.UnitID, err))
		}
	}
	return errors.Join(errs...)
}
