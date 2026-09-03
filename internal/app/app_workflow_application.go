package app

import (
	"context"
	"encoding/json"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/gitdiff"
	"agent-overflow/internal/notify"
	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflowapp"
)

func (a *App) workflowApplication() *workflowapp.Service {
	a.workflowAppOnce.Do(func() {
		a.workflowApp = workflowapp.New(workflowapp.Deps{
			Store:             a.store,
			DataRoot:          a.workflowDataRoot,
			MemoryProvenance:  a.workflowMemoryProvenance,
			RecordMemory:      a.recordWorkflowMemory,
			Git:               func() workflowapp.Git { return a.gitCore() },
			Context:           a.lifeCtx,
			ListBranchCommits: gitdiff.ListBranchCommits,
			ProjectProfile: func(projectID string) (workflowapp.DispositionProfile, error) {
				value, err := a.workflowProjectProfile(projectID)
				if err != nil {
					return workflowapp.DispositionProfile{}, err
				}
				return workflowapp.DispositionProfile{
					BaseBranch: value.BaseBranch, Disposition: string(value.Disposition),
				}, nil
			},
			ParkDisposition: func(itemID string) error {
				workflowEngine, err := a.requireWorkflowEngine()
				if err != nil {
					return err
				}
				return workflowEngine.ParkDisposition(itemID)
			},
			ResolveDisposition: func(itemID string) error {
				workflowEngine, err := a.requireWorkflowEngine()
				if err != nil {
					return err
				}
				return workflowEngine.ResolveDisposition(itemID)
			},
			CancelRun: func(itemID string) error {
				workflowEngine, err := a.requireWorkflowEngine()
				if err != nil {
					return err
				}
				return workflowEngine.Cancel(itemID)
			},
			RemoveOtherWorktree: func(projectID, path string) error {
				// An empty WorkspacePath resolves to the project root: the
				// disposition cleanup has no caller checkout of its own, it
				// is removing the item's worktree from outside.
				_, err := a.RemoveOtherWorktree(WorkspaceRef{ProjectID: projectID}, path, false)
				return err
			},
			InvalidateWorkspace: func(path string) {
				if a.workspaceFiles != nil {
					a.workspaceFiles.Invalidate(path)
				}
			},
			EmitState: func(event engine.StateEvent) {
				a.emit(eventchan.WorkflowItemState, event)
			},
			EmitError: func(event engine.ErrorEvent) {
				a.emit(eventchan.WorkflowError, event)
			},
			EnsureWorkflowReady: func() error {
				_, err := a.requireWorkflowEngine()
				return err
			},
			LockTriage: func(itemID string) func() {
				return a.threadLocks().Lock("workflow-item-triage:" + itemID)
			},
			NewTriageThread: func(input workflowapp.TriageThreadInput) store.Thread {
				return a.newWorkflowTriageThread(
					input.ID, input.Project, input.Workspace, input.Branch,
					input.Title, input.Provider, input.Model,
				)
			},
			SendThreadMessage: func(threadID, message string) error {
				_, err := a.sendMessageWithOptions(context.Background(), threadID, message, sendMessageOptions{})
				return err
			},
			DeleteThread: a.DeleteThread,
			Digest: func(item store.WorkItem, phaseID string, output json.RawMessage, check string) workflowapp.Digest {
				value := workflowTemplateDigest(item, phaseID, output, check)
				return workflowapp.Digest{WhatHappened: value.WhatHappened, WhatItNeeds: value.WhatItNeeds}
			},
			GenerateDigest: func(_ context.Context, item store.WorkItem, template workflowapp.Digest) (workflowapp.Digest, error) {
				generated, err := a.generateWorkflowDigest(item, WorkflowDigest{
					WhatHappened: template.WhatHappened, WhatItNeeds: template.WhatItNeeds,
				})
				return workflowapp.Digest{WhatHappened: generated.WhatHappened, WhatItNeeds: generated.WhatItNeeds}, err
			},
			Lifecycle: workflowapp.LifecyclePorts{
				AutoDispose: a.autoDisposeWorkflowItem,
			},
			ResumeRun: func(ctx context.Context, itemID string) error {
				return a.WorkflowResumeItem(ctx, itemID, "", false)
			},
			Attention: workflowapp.AttentionPorts{
				Notify: func(itemID, title, body string) error {
					// Keyed on the item, so a run that rests twice replaces
					// its own notification instead of stacking a second one
					// beside a state that is no longer true.
					return a.notifyOS(notify.Send{
						ID:    "workflow-item:" + itemID,
						Kind:  notify.KindWorkflowAttention,
						Title: title,
						Body:  body,
						Target: notify.Target{
							Kind:       "workflow-item",
							WorkItemID: itemID,
							BackendID:  a.notificationBackendID(),
						},
					})
				},
				CanUpgradeDigest: func() bool {
					return a.eventBus.Load() != nil && a.osNotifications != nil
				},
			},
			WakeDelivery: workflowapp.WakeDeliveryPorts{
				HasLiveSession: func(threadID string) bool {
					_, live := a.sessionManager().get(threadID)
					return live
				},
				QueueMessage: func(threadID, message string, onDurable func()) error {
					_, err := a.registerQueueItem(
						threadID, message, SendMessageOptions{},
						injectedQueueOptions{preserveDraft: true, onDurable: onDurable},
					)
					return err
				},
				SendMessage: func(threadID, message string) error {
					_, err := a.sendMessageWithOptions(
						context.Background(), threadID, message, sendMessageOptions{PreserveDraft: true},
					)
					return err
				},
			},
			MemoryTree: func(item store.WorkItem) (workflowapp.MemoryTree, error) {
				tree, err := a.workflowMemoryTreeFor(item)
				return workflowapp.MemoryTree{RootID: tree.RootID, NotesPath: tree.NotesPath, Wave: tree.Wave}, err
			},
			RunBudget: func(ctx context.Context, item store.WorkItem) (*workflowapp.Budget, error) {
				budget, err := a.workflowRunBudget(ctx, item)
				if err != nil || budget == nil {
					return nil, err
				}
				return &workflowapp.Budget{
					Kind: budget.Kind, CeilingTokens: budget.CeilingTokens,
					CeilingUSD: budget.CeilingUSD, CeilingMillis: budget.CeilingMillis,
					SpentTokens: budget.SpentTokens, SpentUSD: budget.SpentUSD,
					ElapsedMillis: budget.ElapsedMillis, Percent: budget.Percent,
					Estimated: budget.Estimated, UnpricedRows: budget.UnpricedRows,
					Exhausted: budget.Exhausted, RootItemID: budget.RootItemID,
				}, nil
			},
		})
	})
	return a.workflowApp
}

// The definitions watcher is workflow runtime state; App only adapts its
// callback onto the stable typed frontend event channel.
func (a *App) startWorkflowDefinitionsWatcher(root string) {
	a.workflowApplication().StartDefinitionsWatcher(root, func() {
		a.emit(eventchan.WorkflowDefinitionsChanged, nil)
	})
}

func projectWorkflowRunView(view workflowapp.RunView) WorkflowAgentRunView {
	out := WorkflowAgentRunView{
		ItemID: view.ItemID, WorkflowID: view.WorkflowID, Goal: view.Goal,
		State: view.State, Reason: view.Reason, CurrentPhaseID: view.CurrentPhaseID,
		CurrentPhaseOrdinal: view.CurrentPhaseOrdinal, PhaseCount: view.PhaseCount,
		ParentItemID: view.ParentItemID, Resting: view.Resting,
		StartedAt: view.StartedAt, EndedAt: view.EndedAt, Seeds: view.Seeds,
		PendingGuidance: view.PendingGuidance,
	}
	if view.Budget != nil {
		out.Budget = &WorkflowAgentRunBudget{
			Kind: view.Budget.Kind, CeilingTokens: view.Budget.CeilingTokens,
			CeilingUSD: view.Budget.CeilingUSD, CeilingMillis: view.Budget.CeilingMillis,
			SpentTokens: view.Budget.SpentTokens, SpentUSD: view.Budget.SpentUSD,
			ElapsedMillis: view.Budget.ElapsedMillis, Percent: view.Budget.Percent,
			Estimated: view.Budget.Estimated, UnpricedRows: view.Budget.UnpricedRows,
			Exhausted: view.Budget.Exhausted, RootItemID: view.Budget.RootItemID,
		}
	}
	if view.FailedUnits != nil {
		out.FailedUnits = make([]WorkflowAgentFailedUnit, 0, len(view.FailedUnits))
		for _, unit := range view.FailedUnits {
			out.FailedUnits = append(out.FailedUnits, WorkflowAgentFailedUnit{
				UnitID: unit.UnitID, UnitAttempt: unit.UnitAttempt, Note: unit.Note,
			})
		}
	}
	if view.Phases != nil {
		out.Phases = make([]WorkflowAgentPhaseAttempt, 0, len(view.Phases))
		for _, phase := range view.Phases {
			out.Phases = append(out.Phases, projectWorkflowPhaseAttempt(phase))
		}
	}
	return out
}

func projectWorkflowPhaseAttempt(phase workflowapp.PhaseAttempt) WorkflowAgentPhaseAttempt {
	out := WorkflowAgentPhaseAttempt{
		PhaseID: phase.PhaseID, Attempt: phase.Attempt, Status: phase.Status,
		Provider: phase.Provider, Model: phase.Model, Effort: phase.Effort,
		Cause: phase.Cause, Session: phase.Session, Decision: phase.Decision,
		DecisionTarget: phase.DecisionTarget, ExhaustedLoops: phase.ExhaustedLoops,
		OutputOverflow: phase.OutputOverflow,
	}
	if phase.Outputs != nil {
		out.Outputs = make([]WorkflowAgentOutputDigest, 0, len(phase.Outputs))
		for _, digest := range phase.Outputs {
			out.Outputs = append(out.Outputs, WorkflowAgentOutputDigest{Name: digest.Name, Value: digest.Value})
		}
	}
	return out
}

func projectWorkflowInspection(value workflowapp.RunInspection) WorkflowAgentRunInspection {
	out := WorkflowAgentRunInspection{
		Run: projectWorkflowRunView(value.Run), WorktreePath: value.WorktreePath,
		Branch: value.Branch, BaseBranch: value.BaseBranch,
	}
	if value.Children != nil {
		out.Children = make([]WorkflowAgentChildRun, 0, len(value.Children))
		for _, child := range value.Children {
			out.Children = append(out.Children, WorkflowAgentChildRun{
				ItemID: child.ItemID, WorkflowID: child.WorkflowID, Goal: child.Goal,
				State: child.State, Reason: child.Reason, ParentPhaseID: child.ParentPhaseID,
				ParentUnitID: child.ParentUnitID, ParentAttempt: child.ParentAttempt,
			})
		}
	}
	if value.Guidance != nil {
		out.Guidance = make([]WorkflowAgentGuidanceEntry, 0, len(value.Guidance))
		for _, guidance := range value.Guidance {
			out.Guidance = append(out.Guidance, WorkflowAgentGuidanceEntry{
				Text: guidance.Text, At: guidance.At, AgeSeconds: guidance.AgeSeconds,
				By: guidance.By, ByRun: guidance.ByRun,
			})
		}
	}
	if value.Phase != nil {
		phase := value.Phase
		out.Phase = &WorkflowAgentPhaseDetail{
			PhaseID: phase.PhaseID, Attempt: phase.Attempt, Status: phase.Status,
			Provider: phase.Provider, Model: phase.Model, Effort: phase.Effort,
			Cause: phase.Cause, Outputs: phase.Outputs, Decision: phase.Decision,
			DecisionTarget: phase.DecisionTarget, ExhaustedLoops: phase.ExhaustedLoops,
			Units: make([]WorkflowAgentUnitView, 0, len(phase.Units)),
		}
		for _, unit := range phase.Units {
			out.Phase.Units = append(out.Phase.Units, WorkflowAgentUnitView{
				UnitID: unit.UnitID, Kind: unit.Kind, Status: unit.Status,
				UnitAttempt: unit.UnitAttempt, Note: unit.Note, Branch: unit.Branch,
				WorktreePath: unit.WorktreePath, ThreadID: unit.ThreadID,
			})
		}
	}
	return out
}

func projectWorkflowNarrative(value workflowapp.Narrative) WorkflowAgentNarrative {
	return WorkflowAgentNarrative{
		ItemID: value.ItemID, PhaseID: value.PhaseID, Attempt: value.Attempt,
		UnitID: value.UnitID, UnitAttempt: value.UnitAttempt, Path: value.Path,
		Present: value.Present, Bytes: value.Bytes, Truncated: value.Truncated,
		Content: value.Content,
	}
}

func projectWorkflowMemoryLog(value workflowapp.MemoryLog) WorkflowAgentMemoryLog {
	return WorkflowAgentMemoryLog{
		ItemID: value.ItemID, RootID: value.RootID, Path: value.Path,
		Notes: value.Notes, Total: value.Total, Skipped: value.Skipped,
	}
}

func projectWorkflowWatch(value workflowapp.WatchResult) WorkflowAgentWatchResult {
	out := WorkflowAgentWatchResult{
		ItemID: value.ItemID, Cursor: value.Cursor, Gap: value.Gap,
		Transitions: make([]WorkflowAgentTransition, 0, len(value.Transitions)),
		Run: WorkflowAgentWatchRunState{
			ItemID: value.Run.ItemID, WorkflowID: value.Run.WorkflowID,
			Goal: value.Run.Goal, State: value.Run.State, Reason: value.Run.Reason,
			PhaseID: value.Run.PhaseID, Resting: value.Run.Resting, Repair: value.Run.Repair,
		},
	}
	for _, transition := range value.Transitions {
		out.Transitions = append(out.Transitions, WorkflowAgentTransition{
			Seq: transition.Seq, At: transition.At, ItemID: transition.ItemID,
			PhaseID: transition.PhaseID, Attempt: transition.Attempt,
			From: transition.From, To: transition.To, Reason: transition.Reason,
			Cause: transition.Cause, Resting: transition.Resting,
		})
	}
	return out
}
