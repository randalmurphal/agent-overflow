package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/aocli"
	"agent-overflow/internal/project"
	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/profile"
)

type workflowEmitter struct{ emit func(string, any) }

func (e workflowEmitter) Emit(name string, payload any) { e.emit(name, payload) }

type workflowProfileSource struct {
	store      *store.Store
	configRoot string
}

func (s workflowProfileSource) Profile(_ context.Context, projectID string) (*profile.Profile, error) {
	projectRow, err := s.store.GetProject(projectID)
	if err != nil {
		return nil, fmt.Errorf("workflow profile: load project %q: %w", projectID, err)
	}
	loaded, _, err := profile.Load(filepath.Join(project.ConfigDir(s.configRoot, projectRow.Slug), "profile.yaml"))
	if err != nil {
		return nil, fmt.Errorf("workflow profile: load project %q: %w", projectID, err)
	}
	return &loaded, nil
}

type workflowDefinitionSource struct {
	store      *store.Store
	configRoot string
	profiles   workflowProfileSource
}

func (s workflowDefinitionSource) Resolve(ctx context.Context, item store.WorkItem) (def.Workflow, error) {
	projectRow, err := s.store.GetProject(item.ProjectID)
	if err != nil {
		return def.Workflow{}, fmt.Errorf("workflow definition: load project %q: %w", item.ProjectID, err)
	}
	resolved, err := aocli.ResolveWorkflow(s.configRoot, projectRow.Slug, item.WorkflowID, def.Scope(item.WorkflowScope))
	if err != nil {
		return def.Workflow{}, err
	}
	bindings, err := s.profiles.Profile(ctx, item.ProjectID)
	if err != nil {
		return def.Workflow{}, err
	}
	validation := def.Validate(resolved, bindings)
	if !validation.Valid() {
		messages := make([]string, 0, len(validation.Findings))
		for _, finding := range validation.Findings {
			messages = append(messages, finding.Error())
		}
		return def.Workflow{}, fmt.Errorf("workflow definition %q is invalid: %s", item.WorkflowID, strings.Join(messages, "; "))
	}
	inlined, err := def.InlinePrompts(resolved)
	if err != nil {
		return def.Workflow{}, err
	}
	return inlined, nil
}

func (a *App) initWorkflowEngine(dataRoot string) error {
	settingsSnapshot := a.currentSettings()
	runner := newWorkflowAppRunner(a, dataRoot)
	profiles := workflowProfileSource{store: a.store, configRoot: dataRoot}
	definitions := workflowDefinitionSource{store: a.store, configRoot: dataRoot, profiles: profiles}
	workflowEngine, err := engine.New(
		a.store, runner, workflowEmitter{emit: a.emitWithReplay()}, definitions, profiles,
		engine.Config{Active: settingsSnapshot.WorkflowQueueActive, GlobalConcurrency: settingsSnapshot.WorkflowConcurrency},
	)
	if err != nil {
		return fmt.Errorf("initialize workflow engine: %w", err)
	}
	a.workflowRunner = runner
	a.workflowEngine = workflowEngine
	if err := workflowEngine.Start(a.lifeCtx()); err != nil {
		a.workflowEngine = nil
		a.workflowRunner = nil
		return fmt.Errorf("start workflow engine: %w", err)
	}
	return nil
}

func (a *App) requireWorkflowEngine() (*engine.Engine, error) {
	if a.shuttingDown.Load() {
		return nil, ErrShuttingDown
	}
	if a.workflowEngine == nil {
		return nil, fmt.Errorf("workflow engine unavailable")
	}
	return a.workflowEngine, nil
}

func (a *App) WorkflowEnqueueItem(projectID, workflowID, workflowScope, goal string, seeds json.RawMessage) (store.WorkItem, error) {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return store.WorkItem{}, err
	}
	projectID = strings.TrimSpace(projectID)
	workflowID = strings.TrimSpace(workflowID)
	goal = strings.TrimSpace(goal)
	scope := def.Scope(strings.TrimSpace(workflowScope))
	if projectID == "" || workflowID == "" || goal == "" {
		return store.WorkItem{}, fmt.Errorf("enqueue workflow item: project id, workflow id, and goal are required")
	}
	if scope != def.ScopeProject && scope != def.ScopeShared {
		return store.WorkItem{}, fmt.Errorf("enqueue workflow item: scope must be project or shared")
	}
	if _, err := a.store.GetProject(projectID); err != nil {
		return store.WorkItem{}, fmt.Errorf("enqueue workflow item: %w", err)
	}
	sortPosition, err := a.store.NextWorkItemSortPosition(projectID)
	if err != nil {
		return store.WorkItem{}, err
	}
	item := store.WorkItem{
		ID: uuid.NewString(), ProjectID: projectID, Goal: goal,
		WorkflowID: workflowID, WorkflowScope: string(scope),
		State: string(engine.StateQueued), SortPosition: sortPosition,
		Seeds: append(json.RawMessage(nil), seeds...), Source: "manual",
		CreatedAt: time.Now().UnixMilli(),
	}
	if err := workflowEngine.Enqueue(item); err != nil {
		return store.WorkItem{}, err
	}
	return a.store.GetWorkItem(item.ID)
}

func (a *App) WorkflowCancelItem(itemID string) error {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return err
	}
	return workflowEngine.Cancel(itemID)
}

func (a *App) WorkflowResumeItem(itemID, targetPhase string) error {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return err
	}
	return workflowEngine.Resume(itemID, targetPhase)
}

func (a *App) WorkflowAnswerQuestion(itemID, answer string) error {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return err
	}
	return workflowEngine.Answer(itemID, answer)
}

func (a *App) WorkflowResolveGate(itemID, decision, note string) error {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return err
	}
	return workflowEngine.ResolveHumanGate(itemID, engine.HumanDecision(decision), note)
}

func (a *App) WorkflowReorderQueue(projectID string, orderedIDs []string) error {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return err
	}
	return workflowEngine.Reorder(projectID, orderedIDs)
}

func (a *App) WorkflowSetQueue(active bool, maxStarts, concurrency int) error {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return err
	}
	if concurrency < 1 || concurrency > engine.MaxGlobalConcurrency {
		return fmt.Errorf("set workflow queue: concurrency must be between 1 and %d", engine.MaxGlobalConcurrency)
	}
	previous := a.currentSettings()
	if _, err := a.settings.Update(map[string]any{
		"workflowQueueActive": active, "workflowConcurrency": concurrency,
	}); err != nil {
		return err
	}
	if err := workflowEngine.SetQueue(active, maxStarts, concurrency); err != nil {
		_, rollbackErr := a.settings.Update(map[string]any{
			"workflowQueueActive": previous.WorkflowQueueActive,
			"workflowConcurrency": previous.WorkflowConcurrency,
		})
		return errors.Join(err, rollbackErr)
	}
	return nil
}

func (a *App) WorkflowListItems(projectID string) ([]store.WorkItem, error) {
	if a.store == nil {
		return nil, fmt.Errorf("workflow store unavailable")
	}
	return a.store.ListWorkItemSummaries(store.WorkItemListFilter{ProjectID: projectID})
}

type WorkflowItemDetail struct {
	Item   store.WorkItem        `json:"item"`
	Phases []store.WorkItemPhase `json:"phases"`
}

func (a *App) WorkflowGetItem(itemID string) (WorkflowItemDetail, error) {
	if a.store == nil {
		return WorkflowItemDetail{}, fmt.Errorf("workflow store unavailable")
	}
	item, err := a.store.GetWorkItem(itemID)
	if err != nil {
		return WorkflowItemDetail{}, err
	}
	phases, err := a.store.ListWorkItemPhases(itemID)
	if err != nil {
		return WorkflowItemDetail{}, err
	}
	return WorkflowItemDetail{Item: item, Phases: phases}, nil
}
