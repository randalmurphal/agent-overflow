package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/profile"
)

type WorkflowDispositionReceipt struct {
	Action        string `json:"action"`
	Mode          string `json:"mode,omitempty"`
	SHA           string `json:"sha,omitempty"`
	PRRef         string `json:"prRef,omitempty"`
	Base          string `json:"base,omitempty"`
	CleanupFailed bool   `json:"cleanupFailed,omitempty"`
	// Discarded is set for a discard and records the tree it covered and the
	// checkouts and branches it destroyed (D23). Merge and PR leave the work
	// reachable; discard does not, so its receipt is the only account of it.
	Discarded *WorkflowDiscardResult `json:"discarded,omitempty"`
	Policy    string                 `json:"policy"`
	At        int64                  `json:"at"`
}

type workflowDispositionAction string

const (
	workflowDispositionMerge   workflowDispositionAction = "merged"
	workflowDispositionPR      workflowDispositionAction = "pr"
	workflowDispositionDiscard workflowDispositionAction = "discarded"
)

// WorkflowMergeItem cleanly lands a done item's branch on the live profile's
// base branch. Refusals park the run for human disposition.
func (a *App) WorkflowMergeItem(itemID string) (WorkflowDispositionReceipt, error) {
	return a.runWorkflowDisposition(itemID, workflowDispositionMerge, profile.DispositionManual)
}

// WorkflowCreateItemPR pushes a done item's branch and creates a PR/MR through
// the repository's existing forge integration.
func (a *App) WorkflowCreateItemPR(itemID string) (WorkflowDispositionReceipt, error) {
	return a.runWorkflowDisposition(itemID, workflowDispositionPR, profile.DispositionManual)
}

// WorkflowDiscardItem removes an eligible item's worktree through the existing
// guarded removal path and keeps the durable run record.
func (a *App) WorkflowDiscardItem(itemID string) (WorkflowDispositionReceipt, error) {
	return a.runWorkflowDisposition(itemID, workflowDispositionDiscard, profile.DispositionManual)
}

func (a *App) runWorkflowDisposition(itemID string, action workflowDispositionAction, policy profile.Disposition) (WorkflowDispositionReceipt, error) {
	a.workflowDispositionMu.Lock()
	defer a.workflowDispositionMu.Unlock()

	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return WorkflowDispositionReceipt{}, fmt.Errorf("workflow disposition: item id is required")
	}
	item, err := a.store.GetWorkItem(itemID)
	if err != nil {
		return WorkflowDispositionReceipt{}, err
	}
	if err := validateWorkflowDispositionState(item, action); err != nil {
		return WorkflowDispositionReceipt{}, err
	}
	if len(item.Disposition) > 0 {
		if policy != profile.DispositionManual {
			var receipt WorkflowDispositionReceipt
			if err := json.Unmarshal(item.Disposition, &receipt); err != nil {
				return WorkflowDispositionReceipt{}, fmt.Errorf("workflow disposition %s decode existing receipt: %w", itemID, err)
			}
			return receipt, nil
		}
		return WorkflowDispositionReceipt{}, fmt.Errorf("workflow disposition %s: item already has a receipt", itemID)
	}

	receipt, dispositionErr := a.applyWorkflowDisposition(item, action, policy)
	if dispositionErr != nil && receipt.Action == "" && item.State == string(engine.StateDone) {
		workflowEngine, engineErr := a.requireWorkflowEngine()
		if engineErr == nil {
			engineErr = workflowEngine.ParkDisposition(item.ID)
		}
		return WorkflowDispositionReceipt{}, errors.Join(dispositionErr, engineErr)
	}
	if dispositionErr == nil && receipt.Action != "" && item.State == string(engine.StateNeedsHuman) && item.Reason == string(engine.ReasonDisposition) {
		workflowEngine, engineErr := a.requireWorkflowEngine()
		if engineErr == nil {
			engineErr = workflowEngine.ResolveDisposition(item.ID)
		}
		dispositionErr = errors.Join(dispositionErr, engineErr)
	}
	return receipt, dispositionErr
}

func validateWorkflowDispositionState(item store.WorkItem, action workflowDispositionAction) error {
	// A called run has no workspace of its own — it borrows its caller's (§3a),
	// so every disposition action would operate on the caller's branch and
	// worktree while the caller may still be running. The run tree is disposed
	// through its root.
	if item.ParentItemID != "" {
		return fmt.Errorf(
			"workflow disposition %s: this run was called by %s; dispose the run that called it",
			item.ID, item.ParentItemID,
		)
	}
	state := engine.State(item.State)
	switch action {
	case workflowDispositionMerge, workflowDispositionPR:
		parkedDisposition := state == engine.StateNeedsHuman && item.Reason == string(engine.ReasonDisposition)
		if state != engine.StateDone && !parkedDisposition {
			return fmt.Errorf("workflow disposition %s: %s requires item state done or needs-human(disposition), got %s(%s)", item.ID, action, state, item.Reason)
		}
	case workflowDispositionDiscard:
		if state != engine.StateDone && state != engine.StateFailed && state != engine.StateCancelled && state != engine.StateNeedsHuman {
			return fmt.Errorf("workflow disposition %s: discard is invalid from state %s", item.ID, state)
		}
	default:
		return fmt.Errorf("workflow disposition %s: unknown action %q", item.ID, action)
	}
	return nil
}

func (a *App) applyWorkflowDisposition(item store.WorkItem, action workflowDispositionAction, policy profile.Disposition) (WorkflowDispositionReceipt, error) {
	receipt := WorkflowDispositionReceipt{
		Action: string(action), Policy: string(policy), At: time.Now().UnixMilli(),
	}
	cleanupAuto := false
	if action == workflowDispositionDiscard {
		// Discard is tree-scoped and destroys the branches too (D23): the run
		// tree's checkouts, its fan-out sub-worktrees, and the branches all of
		// them held. WorkflowDiscardPreview is what a human consents to before
		// reaching here, and the tree walk does its own per-checkout validation
		// — a run whose worktree is already gone still has descendants and unit
		// branches to discard.
		discarded, err := a.discardWorkflowTree(item)
		if err != nil {
			return WorkflowDispositionReceipt{}, err
		}
		receipt.Discarded = &discarded
	} else {
		projectRow, err := a.validateWorkflowDispositionWorktree(item)
		if err != nil {
			return WorkflowDispositionReceipt{}, err
		}
		cleanupAuto, err = a.workflowCleanupAuto(item)
		if err != nil {
			return WorkflowDispositionReceipt{}, err
		}
		if err := a.landWorkflowDisposition(&receipt, item, projectRow, action); err != nil {
			return WorkflowDispositionReceipt{}, err
		}
	}

	encoded, err := json.Marshal(receipt)
	if err != nil {
		return receipt, fmt.Errorf("workflow disposition %s encode receipt: %w", item.ID, err)
	}
	if err := a.store.UpdateWorkItemDisposition(item.ID, encoded); err != nil {
		return receipt, err
	}
	cleanupErr := error(nil)
	if cleanupAuto {
		if err := a.removeWorkflowUnitWorktrees(item); err != nil {
			cleanupErr = fmt.Errorf("workflow disposition %s landed but automatic cleanup failed: %w", item.ID, err)
		} else if _, err := a.RemoveOtherWorktreeForProject(item.ProjectID, "", item.WorktreePath, false); err != nil {
			cleanupErr = fmt.Errorf("workflow disposition %s landed but automatic cleanup failed: %w", item.ID, err)
		} else if err := a.store.UpdateWorkItemWorkspace(item.ID, "", "", ""); err != nil {
			cleanupErr = err
		}
	}
	if cleanupErr != nil {
		receipt.CleanupFailed = true
		updated, encodeErr := json.Marshal(receipt)
		persistErr := encodeErr
		if encodeErr == nil {
			persistErr = a.store.UpdateWorkItemDisposition(item.ID, updated)
		}
		if persistErr != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("persist cleanup failure receipt: %w", persistErr))
		}
		log.Printf("workflow disposition %s: %v", item.ID, cleanupErr)
		a.emit("workflow:error", engine.ErrorEvent{
			ItemID: item.ID,
			Error:  "workflow disposition landed but automatic worktree cleanup failed",
		})
	}
	a.emit("workflow:item-state", engine.StateEvent{
		ItemID: item.ID, ProjectID: item.ProjectID,
		From: engine.State(item.State), To: engine.State(item.State),
	})
	return receipt, nil
}

// landWorkflowDisposition performs the git side of the two landing actions and
// records what it did on the receipt.
func (a *App) landWorkflowDisposition(
	receipt *WorkflowDispositionReceipt, item store.WorkItem,
	projectRow store.Project, action workflowDispositionAction,
) error {
	baseBranch, err := a.workflowDispositionBase(item)
	if err != nil {
		return err
	}
	receipt.Base = baseBranch
	core := a.gitCore()
	switch action {
	case workflowDispositionMerge:
		merged, err := core.MergeBranch(projectRow.Path, baseBranch, item.Branch)
		if err != nil {
			return fmt.Errorf("workflow disposition %s refused: %w", item.ID, err)
		}
		receipt.Mode = merged.Mode
		receipt.SHA = merged.SHA
		return nil
	case workflowDispositionPR:
		sha, err := core.HeadSHA(item.WorktreePath)
		if err != nil {
			return fmt.Errorf("workflow disposition %s read PR head: %w", item.ID, err)
		}
		if err := core.PushUnattended(item.WorktreePath); err != nil {
			return fmt.Errorf("workflow disposition %s push: %w", item.ID, err)
		}
		prRef, err := core.CreatePR(
			item.WorktreePath, item.Goal,
			fmt.Sprintf("Created from Agent Overflow workflow %s.", item.WorkflowID), baseBranch, false,
		)
		if err != nil {
			return fmt.Errorf("workflow disposition %s create PR: %w", item.ID, err)
		}
		receipt.PRRef = prRef
		receipt.SHA = sha
		return nil
	default:
		return fmt.Errorf("workflow disposition %s: %q does not land a branch", item.ID, action)
	}
}

func (a *App) workflowDispositionBase(item store.WorkItem) (string, error) {
	projectProfile, err := a.workflowProjectProfile(item.ProjectID)
	if err != nil {
		return "", err
	}
	baseBranch := strings.TrimSpace(projectProfile.BaseBranch)
	if baseBranch == "" {
		baseBranch = strings.TrimSpace(item.BaseBranch)
	}
	if baseBranch == "" {
		return "", fmt.Errorf("workflow disposition %s: base branch is unavailable", item.ID)
	}
	return baseBranch, nil
}

// validateWorkflowDispositionWorktree gates the two landing actions: they read
// the run's worktree and push its branch, so the checkout must exist, be
// registered on that branch, and be clean. Discard does not come through here —
// it removes checkouts rather than reading them, and tolerates the ones that
// are already gone.
func (a *App) validateWorkflowDispositionWorktree(item store.WorkItem) (store.Project, error) {
	projectRow, err := a.store.GetProject(item.ProjectID)
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
	worktree, found, err := a.findWorktree(projectRow.Path, item.WorktreePath)
	if err != nil {
		return store.Project{}, err
	}
	if !found || worktree.Branch != item.Branch {
		return store.Project{}, fmt.Errorf("workflow disposition %s: worktree is not registered on branch %q", item.ID, item.Branch)
	}
	changes, err := a.gitCore().CountWorkingTreeChanges(item.WorktreePath)
	if err != nil {
		return store.Project{}, fmt.Errorf("workflow disposition %s: inspect item worktree: %w", item.ID, err)
	}
	if changes > 0 {
		return store.Project{}, fmt.Errorf("workflow disposition %s: item worktree is dirty (%d changed files)", item.ID, changes)
	}
	return projectRow, nil
}

func (a *App) workflowProjectProfile(projectID string) (*profile.Profile, error) {
	source := workflowProfileSource{store: a.store, configRoot: a.workflowDataRoot()}
	return source.Profile(a.lifeCtx(), projectID)
}

func (a *App) workflowCleanupAuto(item store.WorkItem) (bool, error) {
	if len(item.Snapshot) == 0 || item.WorktreePath == "" {
		return false, nil
	}
	var snapshot engine.Snapshot
	if err := json.Unmarshal(item.Snapshot, &snapshot); err != nil {
		return false, fmt.Errorf("workflow disposition %s: decode cleanup policy: %w", item.ID, err)
	}
	return snapshot.Workflow.Cleanup == def.CleanupAuto, nil
}

func (a *App) queueAutoDisposition(itemID string) {
	a.workflowAutoDisposition.Go(func() { a.autoDisposeWorkflowItem(itemID) })
}

func (a *App) autoDisposeWorkflowItem(itemID string) {
	item, err := a.store.GetWorkItem(itemID)
	if err != nil {
		log.Printf("workflow auto-disposition %s: load item: %v", itemID, err)
		if workflowEngine, engineErr := a.requireWorkflowEngine(); engineErr == nil {
			if parkErr := workflowEngine.ParkDisposition(itemID); parkErr != nil {
				log.Printf("workflow auto-disposition %s: park after load failure: %v", itemID, parkErr)
			}
		}
		a.emit("workflow:error", engine.ErrorEvent{
			ItemID: itemID,
			Error:  "automatic workflow disposition failed; inspect the item's disposition state",
		})
		return
	}
	if item.State != string(engine.StateDone) || item.WorktreePath == "" || len(item.Disposition) > 0 {
		return
	}
	// Called runs are never auto-disposed: the worktree they finished in belongs
	// to their caller, which is typically still running. Not an error — there is
	// simply nothing at this level to dispose.
	if item.ParentItemID != "" {
		return
	}
	projectProfile, err := a.workflowProjectProfile(item.ProjectID)
	if err != nil {
		failure := fmt.Errorf("load live workflow profile: %w", err)
		workflowEngine, engineErr := a.requireWorkflowEngine()
		if engineErr != nil {
			failure = errors.Join(failure, engineErr)
		} else if parkErr := workflowEngine.ParkDisposition(item.ID); parkErr != nil {
			failure = errors.Join(failure, fmt.Errorf("park disposition: %w", parkErr))
		}
		log.Printf("workflow auto-disposition %s: %v", itemID, failure)
		a.emit("workflow:error", engine.ErrorEvent{
			ItemID: itemID,
			Error:  "automatic workflow disposition failed; inspect the item's disposition state",
		})
		return
	}
	var action workflowDispositionAction
	switch projectProfile.Disposition {
	case profile.DispositionManual, "":
		return
	case profile.DispositionAutoPR:
		action = workflowDispositionPR
	case profile.DispositionAutoMerge:
		action = workflowDispositionMerge
	default:
		log.Printf("workflow auto-disposition %s: unsupported policy %q", itemID, projectProfile.Disposition)
		return
	}
	if _, err := a.runWorkflowDisposition(itemID, action, projectProfile.Disposition); err != nil {
		log.Printf("workflow auto-disposition %s (%s): %v", itemID, projectProfile.Disposition, err)
		a.emit("workflow:error", engine.ErrorEvent{
			ItemID: itemID,
			Error:  "automatic workflow disposition failed; inspect the item's disposition state",
		})
	}
}
