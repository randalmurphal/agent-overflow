package workflowapp

import (
	"errors"
	"fmt"
	"strings"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/engine"
)

func (s *Service) discardTree(root store.WorkItem) (DiscardResult, error) {
	result := DiscardResult{
		Members: make([]string, 0), Cancelled: make([]string, 0),
		RemovedWorktrees: make([]string, 0), DeletedBranches: make([]string, 0),
	}
	members, err := s.RunTree(root.ID)
	if err != nil {
		return result, err
	}
	result.Cancelled, err = s.CancelTreeMembers(members)
	if err != nil {
		return result, err
	}
	members, err = s.RunTree(root.ID)
	if err != nil {
		return result, err
	}
	for _, member := range members {
		result.Members = append(result.Members, member.ID)
		if engine.State(member.State) == engine.StateRunning {
			return result, fmt.Errorf(
				"workflow discard %s: run %s is still in flight; cancel it before discarding the tree",
				root.ID, member.ID,
			)
		}
	}
	database, err := s.store()
	if err != nil {
		return result, err
	}
	project, err := database.GetProject(root.ProjectID)
	if err != nil {
		return result, err
	}
	targets, err := s.treeWorktrees(members, "", project.Path)
	if err != nil {
		return result, err
	}
	if len(targets) == 0 {
		return result, nil
	}
	registry, err := s.ReadWorktrees(project.Path, fmt.Sprintf("workflow discard %s", root.ID))
	if err != nil {
		return result, err
	}
	if !registry.Present {
		return result, s.ClearTreeWorkspaces(members)
	}
	var errs []error
	for _, target := range targets {
		removed, err := s.removeDiscardedWorktree(project.Path, registry, target)
		if err != nil {
			errs = append(errs, err)
		}
		if removed.path != "" {
			result.RemovedWorktrees = append(result.RemovedWorktrees, removed.path)
		}
		if removed.branch != "" {
			result.DeletedBranches = append(result.DeletedBranches, removed.branch)
		}
	}
	if joined := errors.Join(errs...); joined != nil {
		return result, fmt.Errorf("workflow discard %s: %w", root.ID, joined)
	}
	return result, s.ClearTreeWorkspaces(members)
}

// CancelTreeMembers settles every running or parked member before checkout
// cleanup. Project deletion reuses this exact rule.
func (s *Service) CancelTreeMembers(members []store.WorkItem) ([]string, error) {
	stopped := make([]string, 0)
	live := make([]store.WorkItem, 0)
	for _, member := range members {
		if discardStops(member) {
			live = append(live, member)
		}
	}
	if len(live) == 0 {
		return stopped, nil
	}
	if s.deps.CancelRun == nil {
		return stopped, errors.New("workflow discard: engine unavailable")
	}
	cancelled := make(map[string]bool, len(live))
	var errs []error
	for _, member := range live {
		if cancelled[member.ParentItemID] {
			cancelled[member.ID] = true
			stopped = append(stopped, member.ID)
			continue
		}
		if err := s.deps.CancelRun(member.ID); err != nil {
			errs = append(errs, fmt.Errorf("cancel run %s: %w", member.ID, err))
			continue
		}
		cancelled[member.ID] = true
		stopped = append(stopped, member.ID)
	}
	return stopped, errors.Join(errs...)
}

func discardStops(member store.WorkItem) bool {
	switch engine.State(member.State) {
	case engine.StateRunning:
		return true
	case engine.StateNeedsHuman:
		return engine.Reason(member.Reason) != engine.ReasonDisposition
	default:
		return false
	}
}

type discardRemoval struct{ path, branch string }

func (s *Service) removeDiscardedWorktree(projectPath string, registry WorktreeRegistry, target WorktreeTarget) (discardRemoval, error) {
	if gitops.SameFilesystemPath(projectPath, target.Path) {
		return discardRemoval{}, nil
	}
	branch, registered := RegisteredWorktreeBranch(registry, target)
	removal := discardRemoval{}
	client, err := s.git()
	if err != nil {
		return removal, err
	}
	if registered {
		if err := client.RemoveWorktreeForce(projectPath, target.Path, true); err != nil {
			return removal, fmt.Errorf("remove worktree %q: %w", target.Path, err)
		}
		removal.path = target.Path
		if s.deps.InvalidateWorkspace != nil {
			s.deps.InvalidateWorkspace(target.Path)
		}
	}
	if branch == "" {
		return removal, nil
	}
	if err := client.DeleteBranch(projectPath, branch, true); err != nil {
		return removal, fmt.Errorf("delete branch %q: %w", branch, err)
	}
	removal.branch = branch
	return removal, nil
}

// ClearTreeWorkspaces removes stale checkout pointers after destructive
// cleanup. Project deletion does not call it because those rows are deleted.
func (s *Service) ClearTreeWorkspaces(members []store.WorkItem) error {
	database, err := s.store()
	if err != nil {
		return err
	}
	var errs []error
	for _, member := range members {
		if member.WorktreePath != "" {
			if err := database.UpdateWorkItemWorkspace(member.ID, "", "", member.BaseBranch); err != nil {
				errs = append(errs, err)
			}
		}
		units, err := database.ListWorkItemUnits(member.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("list fan-out units of %s: %w", member.ID, err))
			continue
		}
		for _, unit := range units {
			if strings.TrimSpace(unit.WorktreePath) == "" {
				continue
			}
			if err := database.AttachWorkItemUnitWorkspace(
				unit.ItemID, unit.PhaseID, unit.Attempt, unit.UnitID, unit.Branch, "",
			); err != nil {
				errs = append(errs, fmt.Errorf("clear unit %q worktree path: %w", unit.UnitID, err))
			}
		}
	}
	return errors.Join(errs...)
}
