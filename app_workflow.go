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
	"agent-overflow/internal/usagecost"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/profile"
)

type workflowEmitter struct {
	app  *App
	emit func(string, any)
}

func (e workflowEmitter) Emit(name string, payload any) {
	if e.app != nil {
		e.app.prepareWorkflowEngineEvent(name, payload)
	}
	e.emit(name, payload)
	if e.app != nil {
		e.app.afterWorkflowEngineEvent(name, payload)
	}
}

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

type workflowSpendSource struct{ store *store.Store }

func (s workflowSpendSource) ItemSpend(_ context.Context, itemID string) (engine.Spend, error) {
	usage, err := s.store.QueryWorkItemUsage(itemID)
	if err != nil {
		return engine.Spend{}, err
	}
	details, err := s.store.QueryWorkItemUsageDetail(itemID)
	if err != nil {
		return engine.Spend{}, err
	}
	cost := usage.CostUSD
	for _, detail := range details {
		switch detail.CostSource {
		case "wire":
			// QueryWorkItemUsage already includes the wire-reported sum.
		case "none":
			estimate, ok := usagecost.Price(
				detail.Model, detail.InputTokens, detail.OutputTokens,
				detail.CacheReadInputTokens, detail.CacheCreationInputTokens,
			)
			if !ok {
				return engine.Spend{}, fmt.Errorf("workflow spend: model %q has no USD rate", detail.Model)
			}
			cost += estimate
		default:
			return engine.Spend{}, fmt.Errorf("workflow spend: unexpected cost_source %q for model %q", detail.CostSource, detail.Model)
		}
	}
	return engine.Spend{Tokens: usage.TotalTokens, USD: cost}, nil
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
	profiles := workflowProfileSource{store: a.store, configRoot: dataRoot}
	runner := newWorkflowAppRunner(a, dataRoot, profiles)
	// Live workflow turns are the only turns that need work-item usage
	// attribution. Crash recovery parks every orphan running item before new
	// sessions can start, so there are no post-crash live turns requiring a
	// durable registry; the runner's bounded process-local map is sufficient.
	// Bare internal test apps may intentionally omit triage; production startup
	// always installs it before the workflow engine.
	if a.triage != nil {
		a.triage.SetUsageWorkItemResolver(runner.workItemForThread)
	}
	definitions := workflowDefinitionSource{store: a.store, configRoot: dataRoot, profiles: profiles}
	workflowEngine, err := engine.New(
		a.store, runner, workflowEmitter{app: a, emit: a.emitWithReplay()}, definitions, profiles,
		workflowSpendSource{store: a.store},
		engine.Config{Active: settingsSnapshot.WorkflowQueueActive, GlobalConcurrency: settingsSnapshot.WorkflowConcurrency},
	)
	if err != nil {
		if a.triage != nil {
			a.triage.SetUsageWorkItemResolver(nil)
		}
		return fmt.Errorf("initialize workflow engine: %w", err)
	}
	a.workflowRunner = runner
	a.workflowEngine = workflowEngine
	if err := workflowEngine.Start(a.lifeCtx()); err != nil {
		if a.triage != nil {
			a.triage.SetUsageWorkItemResolver(nil)
		}
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

func (a *App) WorkflowEnqueueItem(projectID, workflowID, workflowScope, goal string, seeds json.RawMessage, budget *profile.Budget, stepMode bool) (store.WorkItem, error) {
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
	if validation := profile.ValidateBudget(budget); !validation.Valid() {
		messages := make([]string, 0, len(validation.Findings))
		for _, finding := range validation.Findings {
			messages = append(messages, finding.Error())
		}
		return store.WorkItem{}, fmt.Errorf("enqueue workflow item: %s", strings.Join(messages, "; "))
	}
	var encodedBudget json.RawMessage
	if budget != nil {
		encodedBudget, err = json.Marshal(budget)
		if err != nil {
			return store.WorkItem{}, fmt.Errorf("enqueue workflow item: encode budget: %w", err)
		}
	}
	if _, err := a.store.GetProject(projectID); err != nil {
		return store.WorkItem{}, fmt.Errorf("enqueue workflow item: %w", err)
	}
	// Definition/profile errors are synchronous validation failures, not
	// provisioning failures. Resolve before persistence so an unknown or broken
	// workflow never enters the queue under the fire-and-forget start contract.
	profiles := workflowProfileSource{store: a.store, configRoot: a.workflowDataRoot()}
	definitions := workflowDefinitionSource{store: a.store, configRoot: a.workflowDataRoot(), profiles: profiles}
	if _, err := definitions.Resolve(a.lifeCtx(), store.WorkItem{
		ProjectID: projectID, WorkflowID: workflowID, WorkflowScope: string(scope),
	}); err != nil {
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
		Seeds: append(json.RawMessage(nil), seeds...), Budget: encodedBudget,
		StepMode: stepMode, Source: "manual",
		CreatedAt: time.Now().UnixMilli(),
	}
	if err := workflowEngine.EnqueueDetachedStarts(item); err != nil {
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
	item, itemErr := a.store.GetWorkItem(itemID)
	if itemErr != nil {
		return itemErr
	}
	if item.Reason == string(engine.ReasonTakenOver) {
		phase, phaseErr := a.currentWorkflowPhaseAttempt(itemID)
		if phaseErr != nil {
			return fmt.Errorf("resume workflow takeover %s: %w", itemID, phaseErr)
		}
		unlock := a.threadLocks().Lock(phase.ThreadID)
		if _, active, activeErr := a.store.GetActiveTurn(phase.ThreadID); activeErr != nil {
			unlock()
			return fmt.Errorf("resume workflow takeover %s: inspect active turn: %w", itemID, activeErr)
		} else if active {
			unlock()
			return fmt.Errorf("resume workflow takeover %s: the steering turn must yield first", itemID)
		}
		if err := a.workflowRunner.beginTakeoverTransition(itemID, phase.ThreadID); err != nil {
			unlock()
			return err
		}
		unlock()
		if err := workflowEngine.Resume(itemID, targetPhase); err != nil {
			a.workflowRunner.cancelTakeoverTransition(itemID, phase.ThreadID)
			return err
		}
		a.workflowRunner.clearTakeover(itemID)
		return nil
	}
	if err := workflowEngine.Resume(itemID, targetPhase); err != nil {
		return err
	}
	return nil
}

// WorkflowCompleteTakeover runs one schema-attached finalize turn on the
// phase thread currently parked under human control.
func (a *App) WorkflowCompleteTakeover(itemID string) error {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return err
	}
	item, err := a.store.GetWorkItem(itemID)
	if err != nil {
		return err
	}
	if item.State != string(engine.StateNeedsHuman) || item.Reason != string(engine.ReasonTakenOver) {
		return fmt.Errorf("complete workflow takeover %s: item is %s(%s), want needs-human(taken-over)", itemID, item.State, item.Reason)
	}
	phase, err := a.currentWorkflowPhaseAttempt(itemID)
	if err != nil {
		return fmt.Errorf("complete workflow takeover %s: %w", itemID, err)
	}
	threadID := phase.ThreadID
	unlock := a.threadLocks().Lock(threadID)
	if _, active, err := a.store.GetActiveTurn(threadID); err != nil {
		unlock()
		return fmt.Errorf("complete workflow takeover %s: inspect active turn: %w", itemID, err)
	} else if active {
		unlock()
		return fmt.Errorf("complete workflow takeover %s: the steering turn must yield first", itemID)
	}
	if a.workflowRunner == nil {
		unlock()
		return fmt.Errorf("complete workflow takeover %s: runner unavailable", itemID)
	}
	if err := a.workflowRunner.beginTakeoverTransition(itemID, threadID); err != nil {
		unlock()
		return err
	}
	unlock()
	if err := workflowEngine.CompleteTakeover(itemID); err != nil {
		a.workflowRunner.cancelTakeoverTransition(itemID, threadID)
		return err
	}
	return nil
}

func (a *App) currentWorkflowPhaseAttempt(itemID string) (store.WorkItemPhase, error) {
	phases, err := a.store.ListWorkItemPhases(itemID)
	if err != nil {
		return store.WorkItemPhase{}, err
	}
	current, ok := currentWorkflowPhaseAttempt(phases)
	if !ok || current.ThreadID == "" {
		return store.WorkItemPhase{}, fmt.Errorf("parked phase thread is missing")
	}
	return current, nil
}

func currentWorkflowPhaseAttempt(phases []store.WorkItemPhase) (store.WorkItemPhase, bool) {
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
	if err := workflowEngine.SetQueueDetachedStarts(active, maxStarts, concurrency); err != nil {
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
	Item      store.WorkItem        `json:"item"`
	Phases    []store.WorkItemPhase `json:"phases"`
	Artifacts []WorkflowArtifact    `json:"artifacts"`
	Usage     store.WorkItemUsage   `json:"usage"`
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
	artifacts, err := listWorkflowArtifacts(a.workflowDataRoot(), itemID)
	if err != nil {
		return WorkflowItemDetail{}, err
	}
	usage, err := a.store.QueryWorkItemUsage(itemID)
	if err != nil {
		return WorkflowItemDetail{}, err
	}
	return WorkflowItemDetail{Item: item, Phases: phases, Artifacts: artifacts, Usage: usage}, nil
}
