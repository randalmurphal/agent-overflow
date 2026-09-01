package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/aocli"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/logging"
	"agent-overflow/internal/project"
	"agent-overflow/internal/store"
	"agent-overflow/internal/usageledger"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/profile"
	"agent-overflow/internal/workflow/scheduler"
	"agent-overflow/internal/workflowapp"
)

// newWorkflowEngineLog adapts the engine's run-lifecycle sink onto the NDJSON
// engine log, answering a nil interface for a nil logger — a bare internal
// test app that never ran initStores has none, and the engine's own fallback
// to the standard logger is reachable only through a nil SINK. Wrapping a nil
// logger in a non-nil interface would defeat that check, so the choice is made
// once, here.
func newWorkflowEngineLog(logger *logging.Logger) engine.LogSink {
	if logger == nil {
		return nil
	}
	return workflowEngineLog{logger: logger}
}

// workflowEngineLog returns nothing because it is called on the command loop
// and a failed log write is not something the FSM can act on, so a write error
// is surfaced here instead of being handed back into a transition.
type workflowEngineLog struct {
	logger *logging.Logger
}

func (l workflowEngineLog) LogEngineEvent(event engine.LogEvent) {
	if err := l.logger.LogEngineEvent(logging.EngineEventEntry{
		Event:     event.Event,
		ItemID:    event.ItemID,
		ProjectID: event.ProjectID,
		PhaseID:   event.PhaseID,
		Attempt:   event.Attempt,
		State:     string(event.State),
		Reason:    string(event.Reason),
		ThreadID:  event.ThreadID,
		Message:   event.Message,
	}); err != nil {
		log.Printf("workflow engine log: %v (dropped %s for item %s)", err, event.Event, event.ItemID)
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

// workflowSources builds the two resolvers every workflow entry point needs,
// from one config root. They are built together because the definition source
// validates through the profile source: a site that assembled them from two
// different roots would check a definition against a profile no run would ever
// load. configRoot stays a parameter because the callers genuinely differ:
// engine init runs BEFORE `a.workflowDataRoot()` can answer (that read resolves
// through the runner it is about to install), and the harness seeds from its own
// paths with no *App method in reach.
func workflowSources(database *store.Store, configRoot string) (workflowProfileSource, workflowDefinitionSource) {
	profiles := workflowProfileSource{store: database, configRoot: configRoot}
	return profiles, workflowDefinitionSource{store: database, configRoot: configRoot, profiles: profiles}
}

// workflowProfiles is the profile half alone, for the readers that resolve
// a budget or a project profile without touching a definition.
func (a *App) workflowProfiles() workflowProfileSource {
	profiles, _ := workflowSources(a.store, a.workflowDataRoot())
	return profiles
}

// workflowSources is the App-bound pair, rooted at the live data root.
func (a *App) workflowSources() (workflowProfileSource, workflowDefinitionSource) {
	return workflowSources(a.store, a.workflowDataRoot())
}

type workflowSpendSource struct{ store *store.Store }

// TreeSpend prices one run tree: the root's own ledger rows plus every run it
// called, transitively (§12 budgets are enforced against the root across the
// tree). Composition goes through the one ledger pricing rule
// (internal/usageledger), so the dollars a budget is enforced against are the
// dollars every other surface reports for the same rows.
//
// A model with no rate is NOT an error here. Its tokens are exact and a token
// ceiling is unaffected by it; only a dollar ceiling cannot be judged, and that
// refusal belongs where the ceiling's kind is known (engine.ResolveBudget) —
// failing here would park a token-budgeted run over a model nobody has priced
// yet.
func (s workflowSpendSource) TreeSpend(_ context.Context, rootItemID string) (engine.Spend, error) {
	usage, err := s.store.QueryWorkItemTreeUsage(rootItemID)
	if err != nil {
		return engine.Spend{}, err
	}
	details, err := s.store.QueryWorkItemTreeUsageDetail(rootItemID)
	if err != nil {
		return engine.Spend{}, err
	}
	spend, err := usageledger.PriceGroups(details)
	if err != nil {
		return engine.Spend{}, fmt.Errorf("workflow spend for run %s: %w", rootItemID, err)
	}
	return engine.Spend{
		Tokens:    usage.TotalTokens,
		USD:       spend.TotalUSD(),
		Estimated: spend.Estimated(),
		Unpriced:  spend.UnpricedRows,
	}, nil
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
	profiles, definitions := workflowSources(a.store, dataRoot)
	runner := newWorkflowAppRunner(a, dataRoot, profiles)
	// Live workflow turns are the only turns that need work-item usage
	// attribution. Crash recovery parks every orphan running item before new
	// sessions can start, so there are no post-crash live turns requiring a
	// durable registry; the runner's bounded process-local map is sufficient.
	// Bare internal test apps may intentionally omit triage; production startup
	// always installs it before the workflow engine.
	if a.triage != nil {
		a.triage.SetUsageWorkItemResolver(runner.WorkItemForThread)
	}
	// Transfer prior-process usage-attention claims before Engine.Start can emit
	// recovery transitions and create claims owned by this process's in-memory
	// delivery queue. The rows they describe are surfaced only after Start has
	// rebuilt workflow state below.
	usageAttentionRecoveries := a.workflowApplication().ReclaimUsageAttention()
	if err := a.workflowApplication().StartEngine(a.lifeCtx(), workflowapp.EngineRuntimeConfig{
		Runner: runner, Definitions: definitions, Profiles: profiles,
		Spend: workflowSpendSource{store: a.store}, Paused: settingsSnapshot.WorkflowPaused,
		Log: newWorkflowEngineLog(a.engineLogger), Emit: a.emitWithReplay(),
	}); err != nil {
		if a.triage != nil {
			a.triage.SetUsageWorkItemResolver(nil)
		}
		return fmt.Errorf("start workflow engine: %w", err)
	}
	// A queued workflow wake is process memory. Re-surface the prior process's
	// transferred claims only after the engine has rebuilt their item rows.
	a.workflowApplication().SurfaceReclaimedUsageAttention(usageAttentionRecoveries)
	return nil
}

// startWorkflowAutomation arms the two things that make a run happen with
// nobody asking: the self-resume timers and the §11 scheduler.
//
// Split out of initWorkflowEngine because those two are the workflow half of
// the activation gate's set (app_activation.go). Rebuilding engine state from
// SQLite is what a supervisor trial has to prove; firing a trigger into a
// backend that may be rolled back in ninety seconds is what it must not do —
// an auto-resume's boot delay is thirty seconds, comfortably inside a trial's
// budget, and what it resumes is a provider turn that spends real tokens and
// runs real commands.
//
// On every ordinary boot this runs inline from Start, in the same order and
// with the same fatal-on-failure behaviour it had inside initWorkflowEngine.
func (a *App) startWorkflowAutomation() error {
	// Self-resume schedules are re-armed over the engine that just rebuilt: a
	// timer must not be able to fire into a run the rebuild has not decided
	// about yet, and the rebuild is what parks the runs a crash interrupted.
	a.workflowApplication().SweepAutoResumes()
	// The §11 scheduler starts last and over the running engine: a trigger must
	// never be able to fire into an engine that does not exist yet.
	if err := a.initWorkflowScheduler(); err != nil {
		if closeErr := a.workflowApplication().AbortEngine(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close workflow engine: %w", closeErr))
		}
		if a.triage != nil {
			a.triage.SetUsageWorkItemResolver(nil)
		}
		return err
	}
	return nil
}

func (a *App) requireWorkflowEngine() (*engine.Engine, error) {
	if a.shuttingDown.Load() {
		return nil, ErrShuttingDown
	}
	workflowEngine := a.workflowApplication().Engine()
	if workflowEngine == nil {
		return nil, fmt.Errorf("workflow engine unavailable")
	}
	return workflowEngine, nil
}

// Scheduler ownership lives in workflowapp. These adapters retain the startup,
// shutdown, and automation-start integration points on App.
func (a *App) initWorkflowScheduler() error {
	return a.workflowApplication().StartScheduler(a.startAutomationRun, scheduler.SystemClock())
}

func (a *App) stopWorkflowScheduler(ctx context.Context) error {
	return a.workflowApplication().StopScheduler(ctx)
}

func (a *App) requireWorkflowScheduler() (*scheduler.Scheduler, error) {
	if a.shuttingDown.Load() {
		return nil, ErrShuttingDown
	}
	workflowScheduler := a.workflowApplication().Scheduler()
	if workflowScheduler == nil {
		return nil, fmt.Errorf("workflow scheduler unavailable")
	}
	return workflowScheduler, nil
}

// startAutomationRun is the scheduler's only host port. It deliberately uses
// the same validated start path as every other producer and stamps provenance.
func (a *App) startAutomationRun(automation store.Automation, goal string, seeds json.RawMessage) (string, error) {
	item, err := a.startWorkflowRun(
		automation.ProjectID, automation.WorkflowID, automation.WorkflowScope, goal, seeds,
		nil, "", false, scheduler.Source, automation.ID,
	)
	if err != nil {
		return "", err
	}
	return item.ID, nil
}

// WorkflowScheduleResume arms a parked run's resume at an explicit time. This
// is always an operator-authored schedule; provider usage limits never create
// one automatically. It resumes nothing now: the run stays exactly where it is
// until the moment arrives, and every action that repairs it in the meantime
// disarms the schedule.
//
// `at` is either an RFC 3339 timestamp or a leading-`+` duration relative to the
// APP's clock (`+36h`), which is the clock the timer will actually run on.
//
// It returns the armed moment in RFC 3339 so the caller prints the time the app
// holds rather than re-deriving one from a duration it sent.
//
//ao:scope threads:autonomy
func (a *App) WorkflowScheduleResume(ctx context.Context, itemID, at string) (string, error) {
	// The engine is required even though nothing resumes now: what this arms is
	// a `WorkflowResumeItem`, and an app with no engine would take the schedule,
	// persist it, and fail the resume on this boot and every boot after. Its
	// siblings refuse for the same reason, at the same point.
	if _, err := a.requireWorkflowEngine(); err != nil {
		return "", err
	}
	if err := a.authorizeScopedRunAction(ctx, itemID, "schedule workflow resume"); err != nil {
		return "", err
	}
	return a.workflowApplication().ScheduleResume(itemID, at)
}

// WorkflowStartRun is the one start path every producer calls. The run begins
// immediately; contention shows up as its first phase waiting on resource
// capacity, never as a queued item.
//
//ao:scope threads:autonomy
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
	profiles, definitions := a.workflowSources()
	projectProfile, err := profiles.Profile(a.lifeCtx(), projectID)
	if err != nil {
		return store.WorkItem{}, fmt.Errorf("start workflow run: load project profile: %w", err)
	}
	if baseBranch == "" {
		baseBranch = strings.TrimSpace(projectProfile.BaseBranch)
	}
	resolved, err := definitions.Resolve(a.lifeCtx(), store.WorkItem{
		ProjectID: projectID, WorkflowID: workflowID, WorkflowScope: string(scope),
	})
	if err != nil {
		return store.WorkItem{}, fmt.Errorf("start workflow run: %w", err)
	}
	workflow := resolved.Workflow
	normalizedSeeds := append(json.RawMessage(nil), seeds...)
	// Agent-supplied seeds are untrusted input and are validated against the
	// workflow's declared inputs before anything starts, whether they came from
	// a conversation's `ao run start` or a granted phase's. The manual/harness
	// contract is preserved as-is: those callers may intentionally let the
	// workflow runner derive values from Goal.
	if source == workflowSourceAgent {
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

//ao:scope threads:autonomy
func (a *App) WorkflowCancelItem(ctx context.Context, itemID string) error {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return err
	}
	if err := a.authorizeScopedRunAction(ctx, itemID, "cancel workflow run"); err != nil {
		return err
	}
	return workflowEngine.Cancel(itemID)
}

// WorkflowResumeItem returns a parked run to running. With no target phase it
// CONTINUES: a run stopped mid-attempt (`paused`, `interrupted`, `checkpoint`)
// picks up the provider session it parked on and carries its whole tree with it,
// a fan-out parked on a failed unit reopens what blocked it while every finished
// unit — and every run its call units already completed — keeps its result, and
// a park with nothing to continue re-enters the phase. Naming a target phase is
// always the fresh entry, including when it names the parked phase. The engine
// owns that dispatch; this method adds only the takeover bookkeeping the runner
// needs.
//
// refreshDefinition re-reads the workflow and its prompt files from disk for
// this entry instead of rendering the definition the run froze at start — the
// repair for a phase whose prompt was edited while the run was parked. The
// engine offers it at fresh phase entries only.
//
//ao:scope threads:autonomy
func (a *App) WorkflowResumeItem(ctx context.Context, itemID, targetPhase string, refreshDefinition bool) error {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return err
	}
	if err := a.authorizeScopedRunAction(ctx, itemID, "resume workflow run"); err != nil {
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
		if err := a.workflowApplication().BeginTakeoverTransition(context.Background(), itemID, phase.ThreadID); err != nil {
			unlock()
			return err
		}
		unlock()
		if err := workflowEngine.Resume(itemID, targetPhase, refreshDefinition); err != nil {
			a.workflowApplication().CancelTakeoverTransition(itemID, phase.ThreadID)
			return err
		}
		a.workflowApplication().ClearTakeover(itemID)
		return nil
	}
	if err := workflowEngine.Resume(itemID, targetPhase, refreshDefinition); err != nil {
		return err
	}
	return nil
}

// WorkflowCompleteTakeover runs one schema-attached finalize turn on the
// phase thread currently parked under human control.
//
//ao:scope threads:autonomy
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
	if !a.workflowApplication().HasRunner() {
		unlock()
		return fmt.Errorf("complete workflow takeover %s: runner unavailable", itemID)
	}
	if err := a.workflowApplication().BeginTakeoverTransition(context.Background(), itemID, threadID); err != nil {
		unlock()
		return err
	}
	unlock()
	if err := workflowEngine.CompleteTakeover(itemID); err != nil {
		a.workflowApplication().CancelTakeoverTransition(itemID, threadID)
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

// WorkflowAnswerQuestion and WorkflowResolveGate settle the two parks a
// workflow author routed to a person. Both are reachable from the CLI as well
// as the overlay, and both carry the `resolve` grant rather than `start-run`:
// starting and stopping work is routine, while deciding a park the author
// deliberately handed to a human is authority the author hands out just as
// deliberately. The scope check is the same one every run-control RPC applies —
// a webview call carries none and passes untouched, a phase session is confined
// to the runs it started.
//
//ao:scope threads:autonomy
func (a *App) WorkflowAnswerQuestion(ctx context.Context, itemID, answer string) error {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return err
	}
	if err := a.authorizeScopedRunAction(ctx, itemID, "answer workflow question"); err != nil {
		return err
	}
	return workflowEngine.Answer(itemID, answer)
}

//ao:scope threads:autonomy
func (a *App) WorkflowResolveGate(ctx context.Context, itemID, decision, note string) error {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return err
	}
	if err := a.authorizeScopedRunAction(ctx, itemID, "resolve workflow gate"); err != nil {
		return err
	}
	return workflowEngine.ResolveHumanGate(itemID, engine.HumanDecision(decision), note)
}

// WorkflowSetGlobalPause toggles the one engine-level kill switch: no new
// phase starts anywhere while paused, in-flight turns finish. It is persisted
// before it is applied, so a restart recovers the requested state even if
// shutdown races the live update.
//
// This is the ONE write path for the `workflowPaused` setting: the generic
// UpdateSettings patch refuses the key (docs/specs/remote-access.md §6, "one
// write path per key"), because persisting a pause the engine never heard
// about is not the same act as pausing.
//
//ao:scope threads:autonomy
func (a *App) WorkflowSetGlobalPause(paused bool) error {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return err
	}
	previous := a.currentSettings()
	if _, err := a.settings.SetWorkflowPaused(paused); err != nil {
		return err
	}
	if err := workflowEngine.PauseDetachedStarts(paused); err != nil {
		_, rollbackErr := a.settings.SetWorkflowPaused(previous.WorkflowPaused)
		return errors.Join(err, rollbackErr)
	}
	return nil
}

// WorkflowGetEngineState reports the live global pause flag. The engine is the
// authority; settings are only its restart-surviving copy.
//
//ao:scope threads:read
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

//ao:scope threads:read
func (a *App) WorkflowListItems(projectID string) ([]store.WorkItem, error) {
	if a.store == nil {
		return nil, fmt.Errorf("workflow store unavailable")
	}
	return a.store.ListWorkItemSummaries(store.WorkItemListFilter{ProjectID: projectID})
}

// WorkflowListUnresolvedItems returns summary rows for active runs and
// terminal runs that still need a disposition. An empty project ID is
// app-wide, matching WorkflowListItems.
//
//ao:scope threads:read
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
	// ParentUnitID narrows that to one fan-out unit when the call was declared on
	// a unit rather than on the phase; it is empty for a phase call.
	ParentItemID  string `json:"parentItemId,omitempty"`
	ParentPhaseID string `json:"parentPhaseId,omitempty"`
	ParentUnitID  string `json:"parentUnitId,omitempty"`
	ParentAttempt int    `json:"parentAttempt,omitempty"`
	CallDepth     int    `json:"callDepth,omitempty"`
	// SoftStop is the standing request to stop this run tree at its next call
	// boundary (D36). Only a root run carries one; the overlay reads it to show
	// the request as armed rather than as a state change that has not happened.
	SoftStop  bool  `json:"softStop"`
	CreatedAt int64 `json:"createdAt"`
	StartedAt int64 `json:"startedAt,omitempty"`
	EndedAt   int64 `json:"endedAt,omitempty"`
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
	// Cause is why the ENGINE parked this attempt, when it was the engine that
	// diagnosed the park rather than the phase resting on its own envelope. It
	// is the only account a park that ran no turn has, so the detail pane reads
	// it where it would otherwise show a parked attempt and nothing else.
	Cause     string `json:"cause,omitempty"`
	Status    string `json:"status"`
	StartedAt int64  `json:"startedAt"`
	EndedAt   int64  `json:"endedAt,omitempty"`
}

// WorkflowItemUnitView is one fan-out unit (or join) of one phase attempt. A
// unit is part of its phase rather than a timeline of its own, so it rides the
// same detail response the phases do; single-shape runs simply have none.
// Envelopes stay out of the list for the same reason phase inputs do — the
// attempt's own envelope already carries the result the UI shows.
type WorkflowItemUnitView struct {
	ItemID    string `json:"itemId"`
	PhaseID   string `json:"phaseId"`
	Attempt   int    `json:"attempt"`
	UnitID    string `json:"unitId"`
	UnitIndex int    `json:"unitIndex"`
	Kind      string `json:"kind"`
	// Provider names the resource a pending unit is waiting capacity on. The
	// overlay renders it verbatim ("waiting on provider:codex"), which is the
	// only place a human sees the implicit `provider:<name>` bound (D16).
	Provider     string `json:"provider,omitempty"`
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
	Item          WorkflowItemView `json:"item"`
	CheckPhaseIDs []string         `json:"checkPhaseIds"`
	// CallPhaseIDs names the frozen phases that invoke another run. Empty means
	// this run has no call boundary, which is the one thing a soft stop needs to
	// know before offering itself: a request that can never fire must not be
	// presented as a stop that will happen.
	CallPhaseIDs []string                `json:"callPhaseIds"`
	Phases       []WorkflowItemPhaseView `json:"phases"`
	Units        []WorkflowItemUnitView  `json:"units"`
	// There is deliberately NO child list here. The runs this one called are a
	// TREE fact, and `WorkflowGetRunMap` is the read that answers for a tree —
	// with every wave's linkage rather than one level of it, and without the
	// per-child summary join (which parses each row's frozen snapshot to find a
	// phase ordinal) that this view was paying for on every detail fetch.
	Outputs   map[string]any     `json:"outputs"`
	Artifacts []WorkflowArtifact `json:"artifacts"`
	// Usage carries the run tree's TOKEN totals and the providers' own reported
	// cost. Its CostUSD is the wire half alone — read Spend for what the run
	// actually cost.
	Usage store.WorkItemUsage `json:"usage"`
	// Spend is the composed cost: what providers reported plus what the rate
	// table priced their token-only rows at, through the one ledger pricing rule
	// the workflow budget check also enforces with.
	Spend WorkflowRunSpend `json:"spend"`
}

// WorkflowRunSpend is one run tree's dollars, with the halves kept apart so a
// surface can say which part of the number is an estimate. A Codex phase
// reports tokens and no cost at all, so a run's `usage.costUsd` alone reported
// a codex-heavy campaign as nearly free.
type WorkflowRunSpend struct {
	// CostUSD is the composed total: WireCostUSD + EstimatedCostUSD.
	CostUSD          float64 `json:"costUsd"`
	WireCostUSD      float64 `json:"wireCostUsd"`
	EstimatedCostUSD float64 `json:"estimatedCostUsd"`
	// UnpricedRows counts ledger rows whose model has no rate. Their tokens are
	// in Usage; their dollars are in nothing, so a total carrying them is a
	// lower bound.
	UnpricedRows int64 `json:"unpricedRows"`
}

//ao:scope threads:read
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
	treeDetail, err := a.store.QueryWorkItemTreeUsageDetail(itemID)
	if err != nil {
		return WorkflowItemDetailView{}, err
	}
	priced, err := usageledger.PriceGroups(treeDetail)
	if err != nil {
		return WorkflowItemDetailView{}, fmt.Errorf("workflow item %s spend: %w", itemID, err)
	}
	checkPhaseIDs, callPhaseIDs, err := workflowSnapshotPhaseIDs(item.Snapshot)
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
			Cause:  phase.ParkCause,
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
			Provider: unit.Provider,
			ThreadID: unit.ThreadID, Branch: unit.Branch, WorktreePath: unit.WorktreePath,
			Status: unit.Status, UnitAttempt: unit.UnitAttempt, Feedback: unit.Feedback,
			StartedAt: unit.StartedAt, EndedAt: unit.EndedAt,
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
			ParentUnitID:  item.ParentUnitID,
			ParentAttempt: item.ParentAttempt, CallDepth: item.CallDepth,
			SoftStop:  item.SoftStop,
			CreatedAt: item.CreatedAt, StartedAt: item.StartedAt, EndedAt: item.EndedAt,
		},
		CheckPhaseIDs: checkPhaseIDs, CallPhaseIDs: callPhaseIDs,
		Phases: phaseViews, Units: unitViews,
		Outputs: outputs, Artifacts: artifacts, Usage: usage,
		Spend: WorkflowRunSpend{
			CostUSD:          priced.TotalUSD(),
			WireCostUSD:      priced.WireUSD,
			EstimatedCostUSD: priced.EstimatedUSD,
			UnpricedRows:     priced.UnpricedRows,
		},
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

// workflowSnapshotPhaseIDs projects the two phase classifications the detail
// view carries: the deterministic checks a gate's evidence block names, and the
// call sites a soft stop can fire at. Both come off the same frozen snapshot, so
// they are read in one decode rather than two — the snapshot is the largest
// column in the row and decoding it twice per detail load is pure waste.
func workflowSnapshotPhaseIDs(payload json.RawMessage) (checks, calls []string, err error) {
	checks, calls = make([]string, 0), make([]string, 0)
	phases, err := workflowSnapshotPhases(payload)
	if err != nil {
		return nil, nil, err
	}
	for _, phase := range phases {
		if workflowPhaseIsCheck(phase) {
			checks = append(checks, phase.ID)
		}
		if phase.IsCall() {
			calls = append(calls, phase.ID)
		}
	}
	return checks, calls, nil
}

// workflowSnapshotPhases decodes a run's frozen definition down to its phase
// list — the one decode every snapshot projection shares, so a caller that
// wants two facts about the frozen graph pays for one pass over the largest
// column in the row. An empty column is a run that never froze a definition and
// yields no phases without an error; a column that will not decode is the
// caller's to judge.
func workflowSnapshotPhases(payload json.RawMessage) ([]def.Phase, error) {
	if len(payload) == 0 {
		return nil, nil
	}
	var snapshot engine.Snapshot
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		return nil, err
	}
	return snapshot.Workflow.Phases, nil
}

// workflowPhaseIsCheck is the one definition of a check phase every projection
// reads: a tool-driver phase bound to a named deterministic check. The gate's
// evidence block and the run map's skeleton must agree about which phases those
// are, so neither restates the rule.
func workflowPhaseIsCheck(phase def.Phase) bool {
	return phase.Driver == def.DriverTool && phase.Check != ""
}
