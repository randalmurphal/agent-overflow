package workflowapp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
)

type dispositionAction string

const (
	dispositionMerge   dispositionAction = "merged"
	dispositionPR      dispositionAction = "pr"
	dispositionDiscard dispositionAction = "discarded"

	dispositionManual    = "manual"
	dispositionAutoPR    = "auto-pr"
	dispositionAutoMerge = "auto-merge"
)

func (s *Service) MergeItem(itemID string) (DispositionReceipt, error) {
	return s.runDisposition(itemID, dispositionMerge, dispositionManual)
}

func (s *Service) CreateItemPR(itemID string) (DispositionReceipt, error) {
	return s.runDisposition(itemID, dispositionPR, dispositionManual)
}

func (s *Service) DiscardItem(itemID string) (DispositionReceipt, error) {
	return s.runDisposition(itemID, dispositionDiscard, dispositionManual)
}

func (s *Service) runDisposition(itemID string, action dispositionAction, policy string) (DispositionReceipt, error) {
	s.dispositionMu.Lock()
	defer s.dispositionMu.Unlock()

	database, err := s.store()
	if err != nil {
		return DispositionReceipt{}, err
	}
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return DispositionReceipt{}, fmt.Errorf("workflow disposition: item id is required")
	}
	item, err := database.GetWorkItem(itemID)
	if err != nil {
		return DispositionReceipt{}, err
	}
	if err := validateDispositionState(item, action); err != nil {
		return DispositionReceipt{}, err
	}
	if len(item.Disposition) > 0 {
		if policy != dispositionManual {
			var receipt DispositionReceipt
			if err := json.Unmarshal(item.Disposition, &receipt); err != nil {
				return DispositionReceipt{}, fmt.Errorf("workflow disposition %s decode existing receipt: %w", itemID, err)
			}
			return receipt, nil
		}
		return DispositionReceipt{}, fmt.Errorf("workflow disposition %s: item already has a receipt", itemID)
	}

	receipt, dispositionErr := s.applyDisposition(item, action, policy)
	if dispositionErr != nil && receipt.Action == "" && item.State == string(engine.StateDone) {
		var parkErr error
		if s.deps.ParkDisposition == nil {
			parkErr = errors.New("workflow disposition engine unavailable")
		} else {
			parkErr = s.deps.ParkDisposition(item.ID)
		}
		return DispositionReceipt{}, errors.Join(dispositionErr, parkErr)
	}
	if dispositionErr == nil && receipt.Action != "" && item.State == string(engine.StateNeedsHuman) && item.Reason == string(engine.ReasonDisposition) {
		var resolveErr error
		if s.deps.ResolveDisposition == nil {
			resolveErr = errors.New("workflow disposition engine unavailable")
		} else {
			resolveErr = s.deps.ResolveDisposition(item.ID)
		}
		dispositionErr = errors.Join(dispositionErr, resolveErr)
	}
	return receipt, dispositionErr
}

func validateDispositionState(item store.WorkItem, action dispositionAction) error {
	if item.ParentItemID != "" {
		return fmt.Errorf(
			"workflow disposition %s: this run was called by %s; dispose the run that called it",
			item.ID, item.ParentItemID,
		)
	}
	state := engine.State(item.State)
	switch action {
	case dispositionMerge, dispositionPR:
		parkedDisposition := state == engine.StateNeedsHuman && item.Reason == string(engine.ReasonDisposition)
		if state != engine.StateDone && !parkedDisposition {
			return fmt.Errorf("workflow disposition %s: %s requires item state done or needs-human(disposition), got %s(%s)", item.ID, action, state, item.Reason)
		}
	case dispositionDiscard:
		if state != engine.StateDone && state != engine.StateFailed && state != engine.StateCancelled && state != engine.StateNeedsHuman {
			return fmt.Errorf("workflow disposition %s: discard is invalid from state %s", item.ID, state)
		}
	default:
		return fmt.Errorf("workflow disposition %s: unknown action %q", item.ID, action)
	}
	return nil
}

func (s *Service) applyDisposition(item store.WorkItem, action dispositionAction, policy string) (DispositionReceipt, error) {
	receipt := DispositionReceipt{Action: string(action), Policy: policy, At: s.deps.Now().UnixMilli()}
	cleanupAuto := false
	if action == dispositionDiscard {
		discarded, err := s.discardTree(item)
		if err != nil {
			return DispositionReceipt{}, err
		}
		receipt.Discarded = &discarded
	} else {
		project, err := s.validateDispositionWorktree(item)
		if err != nil {
			return DispositionReceipt{}, err
		}
		cleanupAuto, err = cleanupAutoFor(item)
		if err != nil {
			return DispositionReceipt{}, err
		}
		if err := s.landDisposition(&receipt, item, project, action); err != nil {
			return DispositionReceipt{}, err
		}
	}

	database, err := s.store()
	if err != nil {
		return receipt, err
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return receipt, fmt.Errorf("workflow disposition %s encode receipt: %w", item.ID, err)
	}
	if err := database.UpdateWorkItemDisposition(item.ID, encoded); err != nil {
		return receipt, err
	}
	var cleanupErr error
	if cleanupAuto {
		if err := s.removeUnitWorktrees(item); err != nil {
			cleanupErr = fmt.Errorf("workflow disposition %s landed but automatic cleanup failed: %w", item.ID, err)
		} else if s.deps.RemoveOtherWorktree == nil {
			cleanupErr = fmt.Errorf("workflow disposition %s landed but automatic cleanup failed: worktree remover unavailable", item.ID)
		} else if err := s.deps.RemoveOtherWorktree(item.ProjectID, item.WorktreePath); err != nil {
			cleanupErr = fmt.Errorf("workflow disposition %s landed but automatic cleanup failed: %w", item.ID, err)
		} else if err := database.UpdateWorkItemWorkspace(item.ID, "", "", ""); err != nil {
			cleanupErr = err
		}
	}
	if cleanupErr != nil {
		receipt.CleanupFailed = true
		updated, encodeErr := json.Marshal(receipt)
		persistErr := encodeErr
		if encodeErr == nil {
			persistErr = database.UpdateWorkItemDisposition(item.ID, updated)
		}
		if persistErr != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("persist cleanup failure receipt: %w", persistErr))
		}
		s.deps.Logf("workflow disposition %s: %v", item.ID, cleanupErr)
		if s.deps.EmitError != nil {
			s.deps.EmitError(engine.ErrorEvent{
				ItemID: item.ID, Error: "workflow disposition landed but automatic worktree cleanup failed",
			})
		}
	}
	s.emitDispositionState(item.ID)
	return receipt, nil
}

func (s *Service) emitDispositionState(itemID string) {
	if s.deps.EmitState == nil {
		return
	}
	database, err := s.store()
	if err != nil {
		s.deps.Logf("workflow disposition %s: reload before state emit: %v", itemID, err)
		return
	}
	current, err := database.GetWorkItem(itemID)
	if err != nil {
		s.deps.Logf("workflow disposition %s: reload before state emit: %v", itemID, err)
		return
	}
	s.deps.EmitState(engine.StateEvent{
		ItemID: current.ID, ProjectID: current.ProjectID,
		From: engine.State(current.State), To: engine.State(current.State), Reason: engine.Reason(current.Reason),
	})
}

func (s *Service) landDisposition(receipt *DispositionReceipt, item store.WorkItem, project store.Project, action dispositionAction) error {
	base, err := s.dispositionBase(item)
	if err != nil {
		return err
	}
	receipt.Base = base
	client, err := s.git()
	if err != nil {
		return err
	}
	switch action {
	case dispositionMerge:
		merged, err := client.MergeBranch(project.Path, base, item.Branch)
		if err != nil {
			return fmt.Errorf("workflow disposition %s refused: %w", item.ID, err)
		}
		receipt.Mode, receipt.SHA = merged.Mode, merged.SHA
		return nil
	case dispositionPR:
		sha, err := client.HeadSHA(item.WorktreePath)
		if err != nil {
			return fmt.Errorf("workflow disposition %s read PR head: %w", item.ID, err)
		}
		if err := client.PushUnattended(item.WorktreePath); err != nil {
			return fmt.Errorf("workflow disposition %s push: %w", item.ID, err)
		}
		prRef, err := client.CreatePR(
			item.WorktreePath, item.Goal,
			fmt.Sprintf("Created from Agent Overflow workflow %s.", item.WorkflowID), base, false,
		)
		if err != nil {
			return fmt.Errorf("workflow disposition %s create PR: %w", item.ID, err)
		}
		receipt.PRRef, receipt.SHA = prRef, sha
		return nil
	default:
		return fmt.Errorf("workflow disposition %s: %q does not land a branch", item.ID, action)
	}
}

func (s *Service) dispositionBase(item store.WorkItem) (string, error) {
	if s.deps.ProjectProfile == nil {
		return "", errors.New("workflow disposition: project profile unavailable")
	}
	projectProfile, err := s.deps.ProjectProfile(item.ProjectID)
	if err != nil {
		return "", err
	}
	base := strings.TrimSpace(projectProfile.BaseBranch)
	if base == "" {
		base = strings.TrimSpace(item.BaseBranch)
	}
	if base == "" {
		return "", fmt.Errorf("workflow disposition %s: base branch is unavailable", item.ID)
	}
	return base, nil
}

func (s *Service) validateDispositionWorktree(item store.WorkItem) (store.Project, error) {
	database, err := s.store()
	if err != nil {
		return store.Project{}, err
	}
	project, err := database.GetProject(item.ProjectID)
	if err != nil {
		return store.Project{}, err
	}
	if strings.TrimSpace(item.WorktreePath) == "" || strings.TrimSpace(item.Branch) == "" {
		return store.Project{}, fmt.Errorf("workflow disposition %s: item branch or worktree is missing", item.ID)
	}
	info, err := os.Stat(item.WorktreePath)
	if err != nil {
		return store.Project{}, fmt.Errorf("workflow disposition %s: inspect worktree: %w", item.ID, err)
	}
	if !info.IsDir() {
		return store.Project{}, fmt.Errorf("workflow disposition %s: worktree is not a directory", item.ID)
	}
	client, err := s.git()
	if err != nil {
		return store.Project{}, err
	}
	worktrees, err := client.ListWorktrees(project.Path)
	if err != nil {
		return store.Project{}, err
	}
	found := false
	for _, worktree := range worktrees {
		if gitops.SameFilesystemPath(worktree.Path, item.WorktreePath) && worktree.Branch == item.Branch {
			found = true
			break
		}
	}
	if !found {
		return store.Project{}, fmt.Errorf("workflow disposition %s: worktree is not registered on branch %q", item.ID, item.Branch)
	}
	changes, err := client.CountWorkingTreeChanges(item.WorktreePath)
	if err != nil {
		return store.Project{}, fmt.Errorf("workflow disposition %s: inspect item worktree: %w", item.ID, err)
	}
	if changes > 0 {
		return store.Project{}, fmt.Errorf("workflow disposition %s: item worktree is dirty (%d changed files)", item.ID, changes)
	}
	return project, nil
}

func cleanupAutoFor(item store.WorkItem) (bool, error) {
	if len(item.Snapshot) == 0 || item.WorktreePath == "" {
		return false, nil
	}
	var snapshot engine.Snapshot
	if err := json.Unmarshal(item.Snapshot, &snapshot); err != nil {
		return false, fmt.Errorf("workflow disposition %s: decode cleanup policy: %w", item.ID, err)
	}
	return snapshot.Workflow.Cleanup == def.CleanupAuto, nil
}

// AutoDispose applies the live profile's automatic disposition, or does
// nothing when the item is ineligible or the profile is manual.
func (s *Service) AutoDispose(itemID string) error {
	database, err := s.store()
	if err != nil {
		return err
	}
	item, err := database.GetWorkItem(itemID)
	if err != nil {
		if s.deps.ParkDisposition != nil {
			err = errors.Join(err, s.deps.ParkDisposition(itemID))
		}
		return fmt.Errorf("load item: %w", err)
	}
	if item.State != string(engine.StateDone) || item.WorktreePath == "" || len(item.Disposition) > 0 || item.ParentItemID != "" {
		return nil
	}
	if s.deps.ProjectProfile == nil {
		return errors.New("load live workflow profile: project profile unavailable")
	}
	projectProfile, err := s.deps.ProjectProfile(item.ProjectID)
	if err != nil {
		failure := fmt.Errorf("load live workflow profile: %w", err)
		if s.deps.ParkDisposition != nil {
			failure = errors.Join(failure, s.deps.ParkDisposition(item.ID))
		}
		return failure
	}
	var action dispositionAction
	switch projectProfile.Disposition {
	case dispositionManual, "":
		return nil
	case dispositionAutoPR:
		action = dispositionPR
	case dispositionAutoMerge:
		action = dispositionMerge
	default:
		s.deps.Logf("workflow auto-disposition %s: unsupported policy %q", itemID, projectProfile.Disposition)
		return nil
	}
	_, err = s.runDisposition(itemID, action, projectProfile.Disposition)
	return err
}
