package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/aocli"
	gitops "agent-overflow/internal/git"
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

// TreeSpend prices one run tree: the root's own ledger rows plus every run it
// called, transitively (§12 budgets are enforced against the root across the
// tree). The pricing rules are the item-scoped ones — the aggregation is what
// differs.
func (s workflowSpendSource) TreeSpend(_ context.Context, rootItemID string) (engine.Spend, error) {
	usage, err := s.store.QueryWorkItemTreeUsage(rootItemID)
	if err != nil {
		return engine.Spend{}, err
	}
	details, err := s.store.QueryWorkItemTreeUsageDetail(rootItemID)
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

// Resolve freezes an item's workflow at run start, by the scope the item
// records.
func (s workflowDefinitionSource) Resolve(ctx context.Context, item store.WorkItem) (engine.ResolvedDefinition, error) {
	return s.resolve(ctx, item.ProjectID, item.WorkflowID, def.Scope(item.WorkflowScope))
}

// ResolveCall freezes a call phase's target at call time. The scope is not
// supplied: a call names a static id and resolution follows §8 precedence
// (project scope wins over shared), exactly as a run start by id would.
func (s workflowDefinitionSource) ResolveCall(ctx context.Context, projectID, workflowID string) (engine.ResolvedDefinition, error) {
	return s.resolve(ctx, projectID, workflowID, "")
}

// resolve is the one path a definition becomes runnable through: resolve by
// scope (or by §8 precedence), dry-run it including its whole call graph,
// derive the workspace need with call edges propagated, and inline prompts.
func (s workflowDefinitionSource) resolve(ctx context.Context, projectID, workflowID string, scope def.Scope) (engine.ResolvedDefinition, error) {
	projectRow, err := s.store.GetProject(projectID)
	if err != nil {
		return engine.ResolvedDefinition{}, fmt.Errorf("workflow definition: load project %q: %w", projectID, err)
	}
	calls, err := aocli.NewCallResolver(s.configRoot, projectRow.Slug)
	if err != nil {
		return engine.ResolvedDefinition{}, err
	}
	var resolved def.ResolvedWorkflow
	if scope == "" {
		resolved, err = calls.ResolveCall(workflowID)
	} else {
		resolved, err = aocli.ResolveWorkflow(s.configRoot, projectRow.Slug, workflowID, scope)
	}
	if err != nil {
		return engine.ResolvedDefinition{}, err
	}
	bindings, err := s.profiles.Profile(ctx, projectID)
	if err != nil {
		return engine.ResolvedDefinition{}, err
	}
	validation := def.Validate(resolved, bindings, calls)
	if !validation.Valid() {
		messages := make([]string, 0, len(validation.Findings))
		for _, finding := range validation.Findings {
			messages = append(messages, finding.Error())
		}
		return engine.ResolvedDefinition{}, fmt.Errorf("workflow definition %q is invalid: %s", workflowID, strings.Join(messages, "; "))
	}
	need, err := def.PropagatedWorkspaceNeed(resolved.Workflow, calls)
	if err != nil {
		return engine.ResolvedDefinition{}, fmt.Errorf("workflow definition %q workspace need: %w", workflowID, err)
	}
	inlined, err := def.InlinePrompts(resolved)
	if err != nil {
		return engine.ResolvedDefinition{}, err
	}
	return engine.ResolvedDefinition{Workflow: inlined, Scope: resolved.Scope, WorkspaceNeed: need}, nil
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
		engine.Config{Paused: settingsSnapshot.WorkflowPaused},
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

// WorkflowStartRun is the one start path every producer calls. The run begins
// immediately; contention shows up as its first phase waiting on resource
// capacity, never as a queued item.
func (a *App) WorkflowStartRun(projectID, workflowID, workflowScope, goal string, seeds json.RawMessage, budget *profile.Budget, baseBranch string, stepMode bool) (store.WorkItem, error) {
	return a.startWorkflowRun(projectID, workflowID, workflowScope, goal, seeds, budget, baseBranch, stepMode, "manual", "")
}

func (a *App) startWorkflowRun(projectID, workflowID, workflowScope, goal string, seeds json.RawMessage, budget *profile.Budget, baseBranch string, stepMode bool, source, sourceRef string) (store.WorkItem, error) {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return store.WorkItem{}, err
	}
	projectID = strings.TrimSpace(projectID)
	workflowID = strings.TrimSpace(workflowID)
	goal = strings.TrimSpace(goal)
	baseBranch = strings.TrimSpace(baseBranch)
	scope := def.Scope(strings.TrimSpace(workflowScope))
	if projectID == "" || workflowID == "" || goal == "" {
		return store.WorkItem{}, fmt.Errorf("start workflow run: project id, workflow id, and goal are required")
	}
	if scope != def.ScopeProject && scope != def.ScopeShared {
		return store.WorkItem{}, fmt.Errorf("start workflow run: scope must be project or shared")
	}
	if baseBranch != "" {
		if err := gitops.ValidateBranchName(baseBranch); err != nil {
			return store.WorkItem{}, fmt.Errorf("start workflow run: invalid base branch: %w", err)
		}
	}
	if validation := profile.ValidateBudget(budget); !validation.Valid() {
		messages := make([]string, 0, len(validation.Findings))
		for _, finding := range validation.Findings {
			messages = append(messages, finding.Error())
		}
		return store.WorkItem{}, fmt.Errorf("start workflow run: %s", strings.Join(messages, "; "))
	}
	var encodedBudget json.RawMessage
	if budget != nil {
		encodedBudget, err = json.Marshal(budget)
		if err != nil {
			return store.WorkItem{}, fmt.Errorf("start workflow run: encode budget: %w", err)
		}
	}
	projectRow, err := a.store.GetProject(projectID)
	if err != nil {
		return store.WorkItem{}, fmt.Errorf("start workflow run: %w", err)
	}
	if projectRow.Archived {
		return store.WorkItem{}, fmt.Errorf("start workflow run: project %q is archived", projectRow.Name)
	}
	// Definition/profile errors are synchronous validation failures, not
	// provisioning failures. Resolve before persistence so an unknown or broken
	// workflow is refused at the call under the fire-and-forget start contract.
	profiles := workflowProfileSource{store: a.store, configRoot: a.workflowDataRoot()}
	projectProfile, err := profiles.Profile(a.lifeCtx(), projectID)
	if err != nil {
		return store.WorkItem{}, fmt.Errorf("start workflow run: load project profile: %w", err)
	}
	if baseBranch == "" {
		baseBranch = strings.TrimSpace(projectProfile.BaseBranch)
	}
	definitions := workflowDefinitionSource{store: a.store, configRoot: a.workflowDataRoot(), profiles: profiles}
	resolved, err := definitions.Resolve(a.lifeCtx(), store.WorkItem{
		ProjectID: projectID, WorkflowID: workflowID, WorkflowScope: string(scope),
	})
	if err != nil {
		return store.WorkItem{}, fmt.Errorf("start workflow run: %w", err)
	}
	workflow := resolved.Workflow
	normalizedSeeds := append(json.RawMessage(nil), seeds...)
	// Chat proposals are untrusted agent-produced input and must be checked at
	// both proposal time and the approval commit point (the user may edit it).
	// Preserve the established manual/harness start contract, whose callers
	// may intentionally let the workflow runner derive values from Goal.
	if source == "agent" {
		seedValues, encodedSeeds, err := decodeWorkflowSeeds(seeds)
		if err != nil {
			return store.WorkItem{}, fmt.Errorf("start workflow run: %w", err)
		}
		if validationErrors := def.ValidateInputs(workflow, seedValues); len(validationErrors) > 0 {
			return store.WorkItem{}, fmt.Errorf("start workflow run: %s", strings.Join(validationErrors, "; "))
		}
		normalizedSeeds = encodedSeeds
	}
	item := store.WorkItem{
		ID: uuid.NewString(), ProjectID: projectID, Goal: goal,
		WorkflowID: workflowID, WorkflowScope: string(scope),
		State: string(engine.StateRunning),
		Seeds: normalizedSeeds, Budget: encodedBudget,
		BaseBranch: baseBranch, StepMode: stepMode, Source: source, SourceRef: sourceRef,
		CreatedAt: time.Now().UnixMilli(),
	}
	if err := workflowEngine.StartItemDetachedStarts(item); err != nil {
		return store.WorkItem{}, err
	}
	return a.store.GetWorkItem(item.ID)
}

func decodeWorkflowSeeds(seeds json.RawMessage) (map[string]any, json.RawMessage, error) {
	if len(seeds) == 0 || string(seeds) == "null" {
		seeds = json.RawMessage(`{}`)
	}
	var values map[string]any
	decoder := json.NewDecoder(bytes.NewReader(seeds))
	if err := decoder.Decode(&values); err != nil {
		return nil, nil, fmt.Errorf("seeds must be an object: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, nil, fmt.Errorf("seeds must contain one JSON object")
	}
	if values == nil {
		return nil, nil, fmt.Errorf("seeds must be an object")
	}
	normalized, err := json.Marshal(values)
	if err != nil {
		return nil, nil, fmt.Errorf("encode seeds: %w", err)
	}
	return values, normalized, nil
}

func (a *App) WorkflowCancelItem(itemID string) error {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return err
	}
	return workflowEngine.Cancel(itemID)
}

// WorkflowResumeItem returns a parked run to running. With no target phase it
// dispatches on why the run parked: a run stopped mid-attempt (`paused`,
// `interrupted`) continues on the provider session it parked on and carries its
// whole tree with it, while every other reason re-enters the phase with a fresh
// attempt. Naming a target phase is always a fresh entry — that is what
// choosing a different phase means.
func (a *App) WorkflowResumeItem(itemID, targetPhase string) error {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return err
	}
	item, itemErr := a.store.GetWorkItem(itemID)
	if itemErr != nil {
		return itemErr
	}
	if targetPhase == "" && engine.ResumableReason(engine.Reason(item.Reason)) {
		return workflowEngine.ResumeItem(itemID)
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

// WorkflowSetGlobalPause toggles the one engine-level kill switch: no new
// phase starts anywhere while paused, in-flight turns finish. It is persisted
// before it is applied, so a restart recovers the requested state even if
// shutdown races the live update.
func (a *App) WorkflowSetGlobalPause(paused bool) error {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return err
	}
	previous := a.currentSettings()
	if _, err := a.settings.Update(map[string]any{"workflowPaused": paused}); err != nil {
		return err
	}
	if err := workflowEngine.PauseDetachedStarts(paused); err != nil {
		_, rollbackErr := a.settings.Update(map[string]any{"workflowPaused": previous.WorkflowPaused})
		return errors.Join(err, rollbackErr)
	}
	return nil
}

// WorkflowGetEngineState reports the live global pause flag. The engine is the
// authority; settings are only its restart-surviving copy.
func (a *App) WorkflowGetEngineState() (engine.EngineState, error) {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return engine.EngineState{}, err
	}
	paused, err := workflowEngine.Paused()
	if err != nil {
		return engine.EngineState{}, err
	}
	return engine.EngineState{Paused: paused}, nil
}

func (a *App) WorkflowListItems(projectID string) ([]store.WorkItem, error) {
	if a.store == nil {
		return nil, fmt.Errorf("workflow store unavailable")
	}
	return a.store.ListWorkItemSummaries(store.WorkItemListFilter{ProjectID: projectID})
}

// WorkflowListUnresolvedItems returns summary rows for active runs and
// terminal runs that still need a disposition. An empty project ID is
// app-wide, matching WorkflowListItems.
func (a *App) WorkflowListUnresolvedItems(projectID string) ([]store.WorkItem, error) {
	if a.store == nil {
		return nil, fmt.Errorf("workflow store unavailable")
	}
	return a.store.ListWorkItemSummaries(store.WorkItemListFilter{
		ProjectID: projectID, UnresolvedOnly: true,
	})
}

// WorkflowItemView is the run-record portion of the detail response. It keeps
// every item field except the frozen definition snapshot, which is an engine
// recovery payload rather than frontend state.
type WorkflowItemView struct {
	ID             string          `json:"id"`
	ProjectID      string          `json:"projectId"`
	Goal           string          `json:"goal"`
	WorkflowID     string          `json:"workflowId"`
	WorkflowScope  string          `json:"workflowScope"`
	State          string          `json:"state"`
	Reason         string          `json:"reason,omitempty"`
	Seeds          json.RawMessage `json:"seeds,omitempty"`
	StepMode       bool            `json:"stepMode"`
	WorktreePath   string          `json:"worktreePath,omitempty"`
	Branch         string          `json:"branch,omitempty"`
	BaseBranch     string          `json:"baseBranch,omitempty"`
	Budget         json.RawMessage `json:"budget,omitempty"`
	Source         string          `json:"source"`
	SourceRef      string          `json:"sourceRef,omitempty"`
	TriageThreadID string          `json:"triageThreadId,omitempty"`
	Disposition    json.RawMessage `json:"disposition,omitempty"`
	Digest         json.RawMessage `json:"digest,omitempty"`
	// Parent linkage is present only on a called run (§3a). It names the caller,
	// the caller's call phase, and the attempt of it that invoked this run, so a
	// child is always navigable back to the exact invocation that created it.
	ParentItemID  string `json:"parentItemId,omitempty"`
	ParentPhaseID string `json:"parentPhaseId,omitempty"`
	ParentAttempt int    `json:"parentAttempt,omitempty"`
	CallDepth     int    `json:"callDepth,omitempty"`
	CreatedAt     int64  `json:"createdAt"`
	StartedAt     int64  `json:"startedAt,omitempty"`
	EndedAt       int64  `json:"endedAt,omitempty"`
}

// WorkflowItemChildView is one run this item called. A child is a real run with
// a detail page of its own; this is the caller-side summary — enough to render
// the call phase's progress without loading the child's timeline.
type WorkflowItemChildView struct {
	ItemID              string `json:"itemId"`
	WorkflowID          string `json:"workflowId"`
	State               string `json:"state"`
	Reason              string `json:"reason,omitempty"`
	ParentPhaseID       string `json:"parentPhaseId"`
	ParentAttempt       int    `json:"parentAttempt"`
	CallDepth           int    `json:"callDepth"`
	CurrentPhaseID      string `json:"currentPhaseId,omitempty"`
	CurrentPhaseOrdinal int    `json:"currentPhaseOrdinal"`
	PhaseCount          int    `json:"phaseCount"`
	StartedAt           int64  `json:"startedAt,omitempty"`
	EndedAt             int64  `json:"endedAt,omitempty"`
}

// WorkflowItemPhaseView is the timeline projection used by run detail. Input
// context, gate traces, interventions, and narrative paths remain durable in
// SQLite but load only through backend diagnostics, not the ordinary UI path.
type WorkflowItemPhaseView struct {
	ItemID         string          `json:"itemId"`
	PhaseID        string          `json:"phaseId"`
	Attempt        int             `json:"attempt"`
	ThreadID       string          `json:"threadId,omitempty"`
	OutputEnvelope json.RawMessage `json:"outputEnvelope,omitempty"`
	Status         string          `json:"status"`
	StartedAt      int64           `json:"startedAt"`
	EndedAt        int64           `json:"endedAt,omitempty"`
}

// WorkflowItemUnitView is one fan-out unit (or join) of one phase attempt. A
// unit is part of its phase rather than a timeline of its own, so it rides the
// same detail response the phases do; single-shape runs simply have none.
// Envelopes stay out of the list for the same reason phase inputs do — the
// attempt's own envelope already carries the result the UI shows.
type WorkflowItemUnitView struct {
	ItemID       string `json:"itemId"`
	PhaseID      string `json:"phaseId"`
	Attempt      int    `json:"attempt"`
	UnitID       string `json:"unitId"`
	UnitIndex    int    `json:"unitIndex"`
	Kind         string `json:"kind"`
	ThreadID     string `json:"threadId,omitempty"`
	Branch       string `json:"branch,omitempty"`
	WorktreePath string `json:"worktreePath,omitempty"`
	Status       string `json:"status"`
	UnitAttempt  int    `json:"unitAttempt"`
	Feedback     string `json:"feedback,omitempty"`
	StartedAt    int64  `json:"startedAt,omitempty"`
	EndedAt      int64  `json:"endedAt,omitempty"`
}

type WorkflowItemDetailView struct {
	Item          WorkflowItemView        `json:"item"`
	CheckPhaseIDs []string                `json:"checkPhaseIds"`
	Phases        []WorkflowItemPhaseView `json:"phases"`
	Units         []WorkflowItemUnitView  `json:"units"`
	Children      []WorkflowItemChildView `json:"children"`
	Outputs       map[string]any          `json:"outputs"`
	Artifacts     []WorkflowArtifact      `json:"artifacts"`
	Usage         store.WorkItemUsage     `json:"usage"`
}

func (a *App) WorkflowGetItem(itemID string) (WorkflowItemDetailView, error) {
	if a.store == nil {
		return WorkflowItemDetailView{}, fmt.Errorf("workflow store unavailable")
	}
	item, err := a.store.GetWorkItem(itemID)
	if err != nil {
		return WorkflowItemDetailView{}, err
	}
	phases, err := a.store.ListWorkItemPhaseTimeline(itemID)
	if err != nil {
		return WorkflowItemDetailView{}, err
	}
	artifacts, err := listWorkflowArtifacts(a.workflowDataRoot(), itemID)
	if err != nil {
		return WorkflowItemDetailView{}, err
	}
	// Tree usage, not this item's own: a run's spend includes the runs it called
	// (§12), which is the same total the engine's budget check compares against.
	// An item with no children returns exactly what its own ledger sums to.
	usage, err := a.store.QueryWorkItemTreeUsage(itemID)
	if err != nil {
		return WorkflowItemDetailView{}, err
	}
	checkPhaseIDs, err := workflowCheckPhaseIDs(item.Snapshot)
	if err != nil {
		return WorkflowItemDetailView{}, fmt.Errorf("workflow item %s snapshot: %w", itemID, err)
	}
	outputs, err := workflowNamedOutputs(item.Snapshot, phases)
	if err != nil {
		return WorkflowItemDetailView{}, fmt.Errorf("workflow item %s outputs: %w", itemID, err)
	}
	phaseViews := make([]WorkflowItemPhaseView, 0, len(phases))
	for _, phase := range phases {
		phaseViews = append(phaseViews, WorkflowItemPhaseView{
			ItemID: phase.ItemID, PhaseID: phase.PhaseID, Attempt: phase.Attempt,
			ThreadID: phase.ThreadID, OutputEnvelope: phase.OutputEnvelope,
			Status: phase.Status, StartedAt: phase.StartedAt, EndedAt: phase.EndedAt,
		})
	}
	units, err := a.store.ListWorkItemUnits(itemID)
	if err != nil {
		return WorkflowItemDetailView{}, err
	}
	unitViews := make([]WorkflowItemUnitView, 0, len(units))
	for _, unit := range units {
		unitViews = append(unitViews, WorkflowItemUnitView{
			ItemID: unit.ItemID, PhaseID: unit.PhaseID, Attempt: unit.Attempt,
			UnitID: unit.UnitID, UnitIndex: unit.UnitIndex, Kind: unit.Kind,
			ThreadID: unit.ThreadID, Branch: unit.Branch, WorktreePath: unit.WorktreePath,
			Status: unit.Status, UnitAttempt: unit.UnitAttempt, Feedback: unit.Feedback,
			StartedAt: unit.StartedAt, EndedAt: unit.EndedAt,
		})
	}
	children, err := a.store.ListWorkItemSummaries(store.WorkItemListFilter{ParentItemID: itemID})
	if err != nil {
		return WorkflowItemDetailView{}, err
	}
	childViews := make([]WorkflowItemChildView, 0, len(children))
	for _, child := range children {
		childViews = append(childViews, WorkflowItemChildView{
			ItemID: child.ID, WorkflowID: child.WorkflowID, State: child.State,
			Reason: child.Reason, ParentPhaseID: child.ParentPhaseID,
			ParentAttempt: child.ParentAttempt, CallDepth: child.CallDepth,
			CurrentPhaseID: child.CurrentPhaseID, CurrentPhaseOrdinal: child.CurrentPhaseOrdinal,
			PhaseCount: child.PhaseCount, StartedAt: child.StartedAt, EndedAt: child.EndedAt,
		})
	}
	return WorkflowItemDetailView{
		Item: WorkflowItemView{
			ID: item.ID, ProjectID: item.ProjectID, Goal: item.Goal,
			WorkflowID: item.WorkflowID, WorkflowScope: item.WorkflowScope,
			State: item.State, Reason: item.Reason, Seeds: item.Seeds, StepMode: item.StepMode, WorktreePath: item.WorktreePath,
			Branch: item.Branch, BaseBranch: item.BaseBranch, Budget: item.Budget,
			Source: item.Source, SourceRef: item.SourceRef, TriageThreadID: item.TriageThreadID,
			Disposition: item.Disposition, Digest: item.Digest,
			ParentItemID: item.ParentItemID, ParentPhaseID: item.ParentPhaseID,
			ParentAttempt: item.ParentAttempt, CallDepth: item.CallDepth,
			CreatedAt: item.CreatedAt, StartedAt: item.StartedAt, EndedAt: item.EndedAt,
		},
		CheckPhaseIDs: checkPhaseIDs, Phases: phaseViews, Units: unitViews,
		Children: childViews, Outputs: outputs, Artifacts: artifacts, Usage: usage,
	}, nil
}

func workflowNamedOutputs(payload json.RawMessage, phases []store.WorkItemPhaseTimeline) (map[string]any, error) {
	outputs := make(map[string]any)
	if len(payload) == 0 {
		return outputs, nil
	}
	var snapshot engine.Snapshot
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		return nil, err
	}
	vars := make(map[string]any)
	latestAttempts := make(map[string]int)
	for _, phase := range phases {
		if (phase.Status != "completed" && phase.Status != "failed") || len(phase.OutputEnvelope) == 0 || phase.Attempt < latestAttempts[phase.PhaseID] {
			continue
		}
		var envelope struct {
			Status  string         `json:"status"`
			Outputs map[string]any `json:"outputs"`
		}
		if err := json.Unmarshal(phase.OutputEnvelope, &envelope); err != nil {
			return nil, fmt.Errorf("decode phase %s attempt %d: %w", phase.PhaseID, phase.Attempt, err)
		}
		if envelope.Status != "done" {
			continue
		}
		latestAttempts[phase.PhaseID] = phase.Attempt
		for name, value := range envelope.Outputs {
			if value != nil {
				vars[phase.PhaseID+"."+name] = value
			}
		}
	}
	for name, declaration := range snapshot.Workflow.Outputs {
		if declaration.Artifact {
			continue
		}
		if value, ok := def.LookupVariable(vars, declaration.From); ok {
			outputs[name] = value
		}
	}
	return outputs, nil
}

func workflowCheckPhaseIDs(payload json.RawMessage) ([]string, error) {
	ids := make([]string, 0)
	if len(payload) == 0 {
		return ids, nil
	}
	var snapshot engine.Snapshot
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		return nil, err
	}
	for _, phase := range snapshot.Workflow.Phases {
		if phase.Driver == def.DriverTool && phase.Check != "" {
			ids = append(ids, phase.ID)
		}
	}
	return ids, nil
}
