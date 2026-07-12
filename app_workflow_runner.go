package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	workflowrunner "agent-overflow/internal/workflow/runner"
)

type workflowAppRunner struct {
	app      *App
	dataRoot string
	profiles engine.ProfileSource
	now      func() time.Time
	newTimer func(time.Duration, func()) workflowTimer

	mu        sync.Mutex
	runs      map[string]*workflowAttempt
	schemas   map[string]json.RawMessage
	takeovers map[string]workflowTakeover
	// workItems attributes usage only while a phase thread can emit a live
	// turn. Startup crash recovery parks orphan attempts before any session
	// resumes, so no durable thread-to-item registry is needed after restart.
	workItems map[string]string
}

type workflowTakeover struct {
	itemID         string
	schemaAttached bool
	transitioning  bool
}

type workflowAttempt struct {
	sendMu               sync.Mutex
	key                  engine.RunKey
	threadID             string
	schema               json.RawMessage
	phase                def.Phase
	workflow             def.Workflow
	workspace            string
	complete             func(engine.Outcome)
	unsubscribe          func()
	envelopeRetryUsed    bool
	currentPrompt        string
	watchdog             time.Duration
	backoff              []time.Duration
	transientRetryCount  int
	turnStarted          bool
	pendingTransient     bool
	pendingSessionDeath  bool
	awaitingRetryStart   bool
	claudeTransientRetry bool
	timer                workflowTimer
	timerMode            workflowTimerMode
	timerDeadline        time.Time
}

type workflowTakeoverTimer struct {
	mode     workflowTimerMode
	deadline time.Time
}

func newWorkflowAppRunner(app *App, dataRoot string, profiles engine.ProfileSource) *workflowAppRunner {
	return &workflowAppRunner{
		app: app, dataRoot: dataRoot, profiles: profiles, now: time.Now,
		newTimer: func(delay time.Duration, callback func()) workflowTimer {
			return time.AfterFunc(delay, callback)
		},
		runs: make(map[string]*workflowAttempt), schemas: make(map[string]json.RawMessage),
		takeovers: make(map[string]workflowTakeover),
		workItems: make(map[string]string),
	}
}

func (r *workflowAppRunner) Start(ctx context.Context, request engine.RunRequest, entered func(), complete func(engine.Outcome)) (err error) {
	entered()
	if request.FinalizeTakeover {
		// A failed finalize start must not strand the thread's takeover
		// registration in transitioning state: steering sends would be
		// rejected until process restart. Success deletes the registration
		// wholesale when the attempt is installed.
		defer func() {
			if err != nil {
				r.cancelTakeoverTransition(request.Key.ItemID, request.PriorThreadID)
			}
		}()
	}
	if request.Phase.Driver != def.DriverAgent {
		return fmt.Errorf("workflow runner: phase %q uses unsupported driver %q; only agent is available", request.Phase.ID, request.Phase.Driver)
	}
	if request.Phase.Shape != "" && request.Phase.Shape != def.ShapeSingle {
		return fmt.Errorf("workflow runner: phase %q uses unsupported shape %q; only single is available", request.Phase.ID, request.Phase.Shape)
	}
	schema, err := def.EnvelopeSchema(request.Phase)
	if err != nil {
		return fmt.Errorf("workflow runner: build phase %q envelope schema: %w", request.Phase.ID, err)
	}
	watchdog, backoff, err := r.reliability(ctx, request)
	if err != nil {
		return err
	}
	preparedWorkspace, err := r.prepareWorkspace(ctx, request)
	if err != nil {
		return errors.Join(engine.ErrSetupFailed, err)
	}
	workspace := preparedWorkspace.path
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("workflow runner: startup cancelled: %w", err)
	}
	narrativePath, err := workflowrunner.NarrativePath(r.dataRoot, request.Key.ItemID, request.Key.PhaseID, request.Key.Attempt)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(narrativePath), appPrivateDirPerm); err != nil {
		return fmt.Errorf("workflow runner: create narrative directory: %w", err)
	}

	threadID := request.PriorThreadID
	createdThread := false
	if threadID == "" {
		thread, createErr := r.app.createWorkflowThread(request, workspace, preparedWorkspace.project)
		if createErr != nil {
			return createErr
		}
		threadID = thread.ID
		createdThread = true
	} else if err := r.validatePriorThread(request, threadID, workspace); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		if createdThread {
			err = errors.Join(err, r.app.store.DeleteThread(threadID))
		}
		return fmt.Errorf("workflow runner: startup cancelled: %w", err)
	}
	if err := r.app.store.AttachWorkItemPhaseRun(
		request.Key.ItemID, request.Key.PhaseID, request.Key.Attempt, threadID, narrativePath,
	); err != nil {
		if createdThread {
			err = errors.Join(err, r.app.store.DeleteThread(threadID))
		}
		return fmt.Errorf("workflow runner: attach phase run: %w", err)
	}

	var prompt string
	if request.FinalizeTakeover {
		prompt, err = workflowrunner.BuildTakeoverFinalizePrompt(narrativePath)
	} else {
		prompt, err = workflowrunner.BuildPrompt(request.Phase, request.Vars, narrativePath, request.Feedback)
	}
	if err != nil {
		return err
	}

	restartClaudeWithSchema := false
	if request.FinalizeTakeover && request.Phase.Provider == string(provider.Claude) {
		r.mu.Lock()
		takeover, registered := r.takeovers[threadID]
		restartClaudeWithSchema = registered && !takeover.schemaAttached
		if restartClaudeWithSchema {
			r.schemas[threadID] = append(json.RawMessage(nil), schema...)
		}
		r.mu.Unlock()
		if restartClaudeWithSchema {
			if err := r.app.StopSession(threadID); err != nil {
				r.removeTemporarySchema(threadID)
				return fmt.Errorf("workflow runner: stop schema-less takeover session: %w", err)
			}
			if err := r.app.StartSession(threadID); err != nil {
				r.removeTemporarySchema(threadID)
				return fmt.Errorf("workflow runner: restart takeover session with schema: %w", err)
			}
		}
	}
	attempt := &workflowAttempt{
		key: request.Key, threadID: threadID,
		schema: append(json.RawMessage(nil), schema...), phase: request.Phase, complete: complete,
		workflow: request.Workflow, workspace: workspace,
		currentPrompt: prompt, watchdog: watchdog, backoff: backoff,
	}
	key := workflowRunKey(request.Key)
	attempt.unsubscribe = r.app.subscribeThreadTurnObserver(threadID, func(_ string, event provider.ProviderEvent) {
		r.observe(key, event)
	})
	r.mu.Lock()
	if _, exists := r.runs[key]; exists {
		r.mu.Unlock()
		attempt.unsubscribe()
		return fmt.Errorf("workflow runner: attempt %s is already active", key)
	}
	r.schemas[threadID] = append(json.RawMessage(nil), schema...)
	r.runs[key] = attempt
	r.workItems[threadID] = request.Key.ItemID
	delete(r.takeovers, threadID)
	r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		r.detach(key)
		if createdThread {
			err = errors.Join(err, r.app.store.DeleteThread(threadID))
		}
		return fmt.Errorf("workflow runner: startup cancelled: %w", err)
	}

	if _, err := r.sendIfActive(key, prompt, schema); err != nil {
		r.finish(key, engine.Outcome{Kind: engine.OutcomeExecutionFailure})
	}
	return nil
}

const workflowTakeoverYieldTimeout = 15 * time.Second

func (r *workflowAppRunner) Stop(_ context.Context, key engine.RunKey) (json.RawMessage, error) {
	runKey := workflowRunKey(key)
	attempt, ok := r.detach(runKey)
	if !ok {
		return nil, nil
	}
	attempt.sendMu.Lock()
	attempt.sendMu.Unlock()
	if err := r.app.InterruptTurn(attempt.threadID); err != nil {
		log.Printf("workflow runner: interrupt %s: %v", runKey, err)
	}
	return nil, nil
}

func (r *workflowAppRunner) StopForTakeover(ctx context.Context, key engine.RunKey) (json.RawMessage, error) {
	runKey := workflowRunKey(key)
	attempt, timerState, ok := r.detachForTakeover(runKey)
	if !ok {
		return nil, fmt.Errorf("workflow runner: live takeover attempt %s is not registered", runKey)
	}
	attempt.sendMu.Lock()
	attempt.sendMu.Unlock()
	partial, err := r.interruptAndWaitForYield(ctx, runKey, attempt.threadID)
	if err == nil {
		return partial, nil
	}
	attempt.unsubscribe = r.app.subscribeThreadTurnObserver(attempt.threadID, func(_ string, event provider.ProviderEvent) {
		r.observe(runKey, event)
	})
	r.mu.Lock()
	r.runs[runKey] = attempt
	r.schemas[attempt.threadID] = append(json.RawMessage(nil), attempt.schema...)
	r.workItems[attempt.threadID] = attempt.key.ItemID
	r.restoreTakeoverTimerLocked(runKey, attempt, timerState)
	r.mu.Unlock()
	return nil, err
}

func (r *workflowAppRunner) detachForTakeover(runKey string) (*workflowAttempt, workflowTakeoverTimer, bool) {
	r.mu.Lock()
	attempt, ok := r.runs[runKey]
	state := workflowTakeoverTimer{}
	if ok {
		state.mode = attempt.timerMode
		state.deadline = attempt.timerDeadline
		delete(r.runs, runKey)
		delete(r.schemas, attempt.threadID)
		if r.workItems[attempt.threadID] == attempt.key.ItemID {
			delete(r.workItems, attempt.threadID)
		}
		r.disarmTimerLocked(attempt)
	}
	r.mu.Unlock()
	if ok && attempt.unsubscribe != nil {
		attempt.unsubscribe()
	}
	return attempt, state, ok
}

func (r *workflowAppRunner) restoreTakeoverTimerLocked(runKey string, attempt *workflowAttempt, state workflowTakeoverTimer) {
	if state.mode == workflowTimerNone {
		return
	}
	delay := state.deadline.Sub(r.now())
	if delay < 0 {
		delay = 0
	}
	attempt.timerMode = state.mode
	attempt.timerDeadline = state.deadline
	attempt.timer = r.newTimer(delay, func() { r.timerFired(runKey) })
}

func (r *workflowAppRunner) interruptAndWaitForYield(ctx context.Context, runKey, threadID string) (json.RawMessage, error) {
	yielded := make(chan struct{}, 1)
	unsubscribe := r.app.subscribeThreadTurnObserver(threadID, func(_ string, event provider.ProviderEvent) {
		if event.Kind == provider.EventTurnComplete {
			select {
			case yielded <- struct{}{}:
			default:
			}
		}
	})
	defer unsubscribe()
	if err := r.app.InterruptTurn(threadID); err != nil {
		return nil, fmt.Errorf("workflow runner: interrupt %s: %w", runKey, err)
	}
	if _, active, err := r.app.store.GetActiveTurn(threadID); err != nil {
		return nil, fmt.Errorf("workflow runner: inspect interrupted turn %s: %w", runKey, err)
	} else if !active {
		return nil, nil
	}
	timer := time.NewTimer(workflowTakeoverYieldTimeout)
	defer timer.Stop()
	select {
	case <-yielded:
		return nil, nil
	case <-timer.C:
		return nil, fmt.Errorf("workflow runner: interrupted turn %s did not yield within %s", runKey, workflowTakeoverYieldTimeout)
	case <-ctx.Done():
		return nil, fmt.Errorf("workflow runner: wait for interrupted turn %s: %w", runKey, ctx.Err())
	}
}

func (r *workflowAppRunner) observe(runKey string, event provider.ProviderEvent) {
	r.mu.Lock()
	attempt := r.runs[runKey]
	if attempt == nil {
		r.mu.Unlock()
		return
	}
	if attempt.timerMode == workflowTimerBackoff {
		r.mu.Unlock()
		return
	}
	if attempt.awaitingRetryStart {
		if event.Kind == provider.EventSessionStatus && strings.TrimSpace(event.Content) == "error" {
			attempt.awaitingRetryStart = false
			attempt.pendingSessionDeath = true
			r.mu.Unlock()
			return
		}
		if !workflowProviderTurnStarted(attempt.phase.Provider, event) {
			r.mu.Unlock()
			return
		}
		attempt.awaitingRetryStart = false
	}
	if workflowProviderTurnStarted(attempt.phase.Provider, event) {
		attempt.turnStarted = true
		attempt.pendingTransient = false
		attempt.claudeTransientRetry = false
		r.armWatchdogLocked(runKey, attempt)
		r.mu.Unlock()
		return
	}
	if attempt.turnStarted {
		r.resetWatchdogLocked(attempt)
	}
	if event.Kind == provider.EventSessionStatus {
		content := strings.TrimSpace(event.Content)
		if content == "error" {
			r.disarmTimerLocked(attempt)
			attempt.turnStarted = false
			attempt.pendingSessionDeath = true
			r.mu.Unlock()
			return
		}
		if content == "disconnected" {
			if attempt.pendingSessionDeath {
				r.mu.Unlock()
				return
			}
			r.mu.Unlock()
			r.finish(runKey, engine.Outcome{Kind: engine.OutcomeExecutionFailure})
			return
		}
	}
	if event.Kind == provider.EventAPIRetry && attempt.phase.Provider == string(provider.Claude) {
		if workflowClaudeTransientAPIRetry(event) {
			attempt.claudeTransientRetry = true
		}
		r.mu.Unlock()
		return
	}
	if event.Kind == provider.EventError {
		transient, waitsForCompletion := workflowTransientError(
			attempt.phase.Provider, event, attempt.claudeTransientRetry,
		)
		if transient && waitsForCompletion {
			attempt.pendingTransient = true
			r.mu.Unlock()
			return
		}
		if transient {
			exhausted := r.scheduleTransientLocked(runKey, attempt)
			r.mu.Unlock()
			if exhausted {
				r.stopAndFinish(runKey, engine.Outcome{Kind: engine.OutcomeTransientExhausted})
			}
			return
		}
		if workflowTurnErrorIsTerminal(event.Meta) {
			r.mu.Unlock()
			r.finish(runKey, engine.Outcome{Kind: engine.OutcomeExecutionFailure})
			return
		}
	}
	if event.Kind != provider.EventTurnComplete {
		r.mu.Unlock()
		return
	}
	r.disarmTimerLocked(attempt)
	attempt.turnStarted = false
	attempt.claudeTransientRetry = false
	payload := append(json.RawMessage(nil), event.StructuredOutput...)
	if len(payload) == 0 && (attempt.pendingTransient || workflowTransientTurnComplete(attempt.phase.Provider, event)) {
		attempt.pendingTransient = false
		exhausted := r.scheduleTransientLocked(runKey, attempt)
		r.mu.Unlock()
		if exhausted {
			r.stopAndFinish(runKey, engine.Outcome{Kind: engine.OutcomeTransientExhausted})
		}
		return
	}
	attempt.pendingTransient = false
	if len(payload) == 0 && workflowTurnCompletedWithError(event) {
		r.mu.Unlock()
		r.finish(runKey, engine.Outcome{Kind: engine.OutcomeExecutionFailure})
		return
	}
	validationErr := def.ValidateEnvelope(attempt.phase, payload)
	if validationErr == nil {
		r.mu.Unlock()
		outcome, err := workflowrunner.OutcomeFromEnvelope(payload)
		if err != nil {
			r.finish(runKey, engine.Outcome{Kind: engine.OutcomeExecutionFailure, Envelope: payload})
			return
		}
		r.finish(runKey, outcome)
		return
	}
	if attempt.envelopeRetryUsed {
		r.mu.Unlock()
		r.finish(runKey, engine.Outcome{Kind: engine.OutcomeExecutionFailure, Envelope: payload})
		return
	}
	attempt.envelopeRetryUsed = true
	schema := append(json.RawMessage(nil), attempt.schema...)
	attempt.currentPrompt = workflowrunner.RetryMessage(findingsForEnvelopeError(validationErr))
	message := attempt.currentPrompt
	r.mu.Unlock()
	go func() {
		sent, err := r.sendIfActive(runKey, message, schema)
		if sent && err != nil {
			r.finish(runKey, engine.Outcome{Kind: engine.OutcomeExecutionFailure, Envelope: payload})
		}
	}()
}

// sessionDisconnected advances subprocess-death retry only after the dead
// session has been removed from App's registry. That ordering prevents a
// millisecond harness backoff from sending into the session being reaped.
func (r *workflowAppRunner) sessionDisconnected(threadID string) {
	var exhaustedKey string
	r.mu.Lock()
	for runKey, attempt := range r.runs {
		if attempt.threadID != threadID || !attempt.pendingSessionDeath {
			continue
		}
		attempt.pendingSessionDeath = false
		if r.scheduleTransientLocked(runKey, attempt) {
			exhaustedKey = runKey
		}
		break
	}
	r.mu.Unlock()
	if exhaustedKey != "" {
		r.stopAndFinish(exhaustedKey, engine.Outcome{Kind: engine.OutcomeTransientExhausted})
	}
}

func workflowProviderTurnStarted(providerName string, event provider.ProviderEvent) bool {
	if event.Kind == provider.EventTurnStart {
		return true
	}
	return providerName == string(provider.Claude) && event.Kind == provider.EventInit
}

// sendIfActive serializes sends with Stop and rechecks ownership after taking
// the per-attempt lock. A retry queued before cancellation therefore cannot
// start a new provider turn after Stop has removed the attempt.
func (r *workflowAppRunner) sendIfActive(runKey, message string, schema json.RawMessage) (bool, error) {
	r.mu.Lock()
	attempt := r.runs[runKey]
	r.mu.Unlock()
	if attempt == nil {
		return false, nil
	}

	attempt.sendMu.Lock()
	defer attempt.sendMu.Unlock()
	r.mu.Lock()
	active := r.runs[runKey] == attempt
	r.mu.Unlock()
	if !active {
		return false, nil
	}
	err := r.app.sendWorkflowMessage(attempt.threadID, message, schema)
	if err != nil || attempt.phase.Provider != string(provider.Claude) {
		return true, err
	}

	// Claude has no per-turn EventTurnStart. A successful send is therefore
	// the start signal when an existing session emits no fresh EventInit (for
	// example, the envelope-feedback turn and a transient sub-attempt).
	r.mu.Lock()
	if r.runs[runKey] == attempt && !attempt.turnStarted && !attempt.pendingSessionDeath && attempt.timerMode != workflowTimerBackoff {
		attempt.turnStarted = true
		attempt.pendingTransient = false
		attempt.claudeTransientRetry = false
		r.armWatchdogLocked(runKey, attempt)
	}
	r.mu.Unlock()
	return true, nil
}

func workflowTurnErrorIsTerminal(meta json.RawMessage) bool {
	if len(meta) == 0 {
		return false
	}
	var flags struct {
		Fatal              bool `json:"fatal"`
		ExpectTurnComplete bool `json:"expect_turn_complete"`
	}
	return json.Unmarshal(meta, &flags) == nil && flags.Fatal && !flags.ExpectTurnComplete
}

func (r *workflowAppRunner) finish(runKey string, outcome engine.Outcome) {
	attempt, ok := r.detach(runKey)
	if !ok {
		return
	}
	attempt.sendMu.Lock()
	attempt.sendMu.Unlock()
	if outcome.Kind == engine.OutcomeDone {
		go func() {
			r.captureArtifacts(attempt, outcome.Envelope)
			attempt.complete(outcome)
		}()
		return
	}
	attempt.complete(outcome)
}

func (r *workflowAppRunner) schemaForThread(threadID string) json.RawMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append(json.RawMessage(nil), r.schemas[threadID]...)
}

func (r *workflowAppRunner) sessionSchemaForThread(threadID string) (json.RawMessage, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if schema, ok := r.schemas[threadID]; ok {
		return append(json.RawMessage(nil), schema...), true
	}
	takeover, ok := r.takeovers[threadID]
	if ok {
		// Reaching session startup through a takeover registration means this
		// process is intentionally schema-less. Preserve that fact so a later
		// Claude finalize restarts with --json-schema even though the process is
		// alive by then.
		takeover.schemaAttached = false
		r.takeovers[threadID] = takeover
	}
	return nil, ok
}

func (r *workflowAppRunner) workItemForThread(threadID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.workItems[threadID]
}

func (r *workflowAppRunner) registerTakeover(itemID, threadID string) error {
	r.mu.Lock()
	if existing, ok := r.takeovers[threadID]; ok && existing.itemID == itemID {
		if existing.transitioning {
			r.mu.Unlock()
			return fmt.Errorf("workflow runner: takeover item %s is completing or resuming", itemID)
		}
		r.workItems[threadID] = itemID
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()
	item, err := r.app.store.GetWorkItem(itemID)
	if err != nil {
		return fmt.Errorf("workflow runner: load takeover item: %w", err)
	}
	if item.State != string(engine.StateNeedsHuman) || item.Reason != string(engine.ReasonTakenOver) {
		return fmt.Errorf("workflow runner: item %s is %s(%s), want needs-human(taken-over)", itemID, item.State, item.Reason)
	}
	var snapshot engine.Snapshot
	if err := json.Unmarshal(item.Snapshot, &snapshot); err != nil {
		return fmt.Errorf("workflow runner: decode takeover snapshot: %w", err)
	}
	phases, err := r.app.store.ListWorkItemPhases(itemID)
	if err != nil {
		return fmt.Errorf("workflow runner: list takeover phases: %w", err)
	}
	var current store.WorkItemPhase
	for index := len(phases) - 1; index >= 0; index-- {
		if phases[index].ThreadID == threadID {
			current = phases[index]
			break
		}
	}
	if current.ThreadID == "" {
		return fmt.Errorf("workflow runner: thread %s is not attached to item %s", threadID, itemID)
	}
	var phase def.Phase
	for _, candidate := range snapshot.Workflow.Phases {
		if candidate.ID == current.PhaseID {
			phase = candidate
			break
		}
	}
	if phase.ID == "" {
		return fmt.Errorf("workflow runner: phase %s is absent from item %s snapshot", current.PhaseID, itemID)
	}
	if _, err := def.EnvelopeSchema(phase); err != nil {
		return fmt.Errorf("workflow runner: takeover phase schema: %w", err)
	}
	_, sessionAlive := r.app.sessionManager().get(threadID)
	r.mu.Lock()
	if existing, ok := r.takeovers[threadID]; ok && existing.itemID == itemID {
		sessionAlive = existing.schemaAttached
	}
	r.takeovers[threadID] = workflowTakeover{
		itemID: itemID, schemaAttached: sessionAlive,
	}
	r.workItems[threadID] = itemID
	delete(r.schemas, threadID)
	r.mu.Unlock()
	if phase.Provider == string(provider.Claude) && sessionAlive {
		if err := r.app.stopSession(threadID); err != nil {
			return fmt.Errorf("workflow runner: stop schema-attached takeover session: %w", err)
		}
		if err := r.app.startSession(threadID); err != nil {
			return fmt.Errorf("workflow runner: restart takeover session without schema: %w", err)
		}
	}
	return nil
}

func (r *workflowAppRunner) beginTakeoverTransition(itemID, threadID string) error {
	if err := r.registerTakeover(itemID, threadID); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	takeover, ok := r.takeovers[threadID]
	if !ok || takeover.itemID != itemID {
		return fmt.Errorf("workflow runner: takeover item %s is not registered on thread %s", itemID, threadID)
	}
	takeover.transitioning = true
	r.takeovers[threadID] = takeover
	return nil
}

func (r *workflowAppRunner) cancelTakeoverTransition(itemID, threadID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	takeover, ok := r.takeovers[threadID]
	if ok && takeover.itemID == itemID {
		takeover.transitioning = false
		r.takeovers[threadID] = takeover
	}
}

func (r *workflowAppRunner) clearTakeover(itemID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for threadID, takeover := range r.takeovers {
		if takeover.itemID != itemID {
			continue
		}
		delete(r.takeovers, threadID)
		if r.workItems[threadID] == itemID {
			delete(r.workItems, threadID)
		}
	}
}

func (r *workflowAppRunner) removeTemporarySchema(threadID string) {
	r.mu.Lock()
	delete(r.schemas, threadID)
	r.mu.Unlock()
}

func findingsForEnvelopeError(err error) []def.EnvelopeFinding {
	var envelopeErr *def.EnvelopeValidationError
	if errors.As(err, &envelopeErr) {
		return envelopeErr.Findings
	}
	return []def.EnvelopeFinding{{Path: "$", Message: err.Error()}}
}

func (r *workflowAppRunner) validatePriorThread(request engine.RunRequest, threadID, workspace string) error {
	thread, err := r.app.store.GetThread(threadID)
	if err != nil {
		return fmt.Errorf("workflow runner: load prior thread %q: %w", threadID, err)
	}
	if thread.Mode != "workflow" {
		return fmt.Errorf("workflow runner: prior thread %q has mode %q, want workflow", threadID, thread.Mode)
	}
	if thread.Provider != request.Phase.Provider || thread.Model != provider.NormalizeModelSlug(request.Phase.Provider, request.Phase.Model) {
		return fmt.Errorf("workflow runner: prior thread %q provider/model no longer matches phase %s/%s", threadID, request.Phase.Provider, request.Phase.Model)
	}
	if !filepath.IsAbs(workspace) || !filepath.IsAbs(thread.WorkspacePath) || !gitops.SameFilesystemPath(thread.WorkspacePath, workspace) {
		return fmt.Errorf("workflow runner: prior thread %q workspace %q no longer matches item workspace %q", threadID, thread.WorkspacePath, workspace)
	}
	return nil
}

func (a *App) createWorkflowThread(request engine.RunRequest, workspace string, project store.Project) (store.Thread, error) {
	seed := a.seedChatModelProfile(request.Phase.Provider, request.Phase.Model)
	now := time.Now().UnixMilli()
	title := request.Phase.Name
	if strings.TrimSpace(title) == "" {
		title = request.Phase.ID
	}
	thread := store.Thread{
		ID: uuid.NewString(), ProjectID: project.ID, ProjectPath: project.Path,
		Title: "Workflow: " + title, Provider: request.Phase.Provider,
		Model:         provider.NormalizeModelSlug(request.Phase.Provider, request.Phase.Model),
		WorkspacePath: workspace, Mode: "workflow",
		ReasoningEffort: seed.ReasoningEffort, FastMode: seed.FastMode,
		ContextWindow: seed.ContextWindow, RuntimeMode: string(provider.RuntimeFullAccess),
		CreatedAt: now, UpdatedAt: now,
	}
	if !gitops.SameFilesystemPath(workspace, project.Path) {
		thread.WorktreePath = workspace
		current, err := a.store.GetWorkItem(request.Key.ItemID)
		if err != nil {
			return store.Thread{}, fmt.Errorf("workflow runner: reload item workspace: %w", err)
		}
		thread.Branch = current.Branch
	}
	thread = a.sanitizeThreadModelSettings(thread)
	thread.RuntimeMode = string(provider.RuntimeFullAccess)
	thread.DisabledMcpServers = a.snapshotDisabledMcpServers(thread.Provider, thread.WorkspacePath)
	if err := a.store.CreateThread(thread); err != nil {
		return store.Thread{}, fmt.Errorf("workflow runner: create phase thread: %w", err)
	}
	return a.store.GetThread(thread.ID)
}

func workflowRunKey(key engine.RunKey) string {
	return fmt.Sprintf("%s/%s/%d", key.ItemID, key.PhaseID, key.Attempt)
}
