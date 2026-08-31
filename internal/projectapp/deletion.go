package projectapp

import (
	"fmt"
	"slices"

	"agent-overflow/internal/store"
)

// WorkflowFootprint is the persistent workflow work owned by one project.
// It intentionally contains no live runtime state: root reads it before and
// after destructive cleanup to detect work created during deletion.
type WorkflowFootprint struct {
	roots         []store.WorkItem
	runIDs        []string
	automationIDs []string
}

func (f WorkflowFootprint) HasWork() bool {
	return len(f.runIDs) > 0 || len(f.automationIDs) > 0
}

// SameAs compares identities rather than counts. A run or automation created
// during deletion must make the guarded delete retry even if another row
// disappeared in the same interval.
func (f WorkflowFootprint) SameAs(other WorkflowFootprint) bool {
	return slices.Equal(f.runIDs, other.runIDs) &&
		slices.Equal(f.automationIDs, other.automationIDs)
}

func (f WorkflowFootprint) Roots() []store.WorkItem { return f.roots }
func (f WorkflowFootprint) RunCount() int           { return len(f.runIDs) }
func (f WorkflowFootprint) AutomationCount() int    { return len(f.automationIDs) }

// WorkflowFootprint reads the rows that project deletion must clean up and
// later forget. Work item summaries avoid loading frozen workflow snapshots.
func (s *Service) WorkflowFootprint(projectID string) (WorkflowFootprint, error) {
	database, err := s.database("read project workflow footprint")
	if err != nil {
		return WorkflowFootprint{}, err
	}
	items, err := database.ListWorkItemSummaries(store.WorkItemListFilter{ProjectID: projectID})
	if err != nil {
		return WorkflowFootprint{}, fmt.Errorf("project %s workflow footprint: list runs: %w", projectID, err)
	}
	automations, err := database.ListAutomations(projectID)
	if err != nil {
		return WorkflowFootprint{}, fmt.Errorf("project %s workflow footprint: list automations: %w", projectID, err)
	}

	footprint := WorkflowFootprint{
		roots:         make([]store.WorkItem, 0, len(items)),
		runIDs:        make([]string, 0, len(items)),
		automationIDs: make([]string, 0, len(automations)),
	}
	owned := make(map[string]bool, len(items))
	for _, item := range items {
		owned[item.ID] = true
	}
	for _, item := range items {
		footprint.runIDs = append(footprint.runIDs, item.ID)
		// A missing or cross-project parent cannot walk to this item, so it is
		// another root of the project's deletion forest.
		if item.ParentItemID == "" || !owned[item.ParentItemID] {
			footprint.roots = append(footprint.roots, item)
		}
	}
	for _, automation := range automations {
		footprint.automationIDs = append(footprint.automationIDs, automation.ID)
	}
	slices.Sort(footprint.runIDs)
	slices.Sort(footprint.automationIDs)
	return footprint, nil
}
