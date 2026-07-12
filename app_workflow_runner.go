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

	mu      sync.Mutex
	runs    map[string]*workflowAttempt
	schemas map[string]json.RawMessage
	// workItems attributes usage only while a phase thread can emit a live
	// turn. Startup crash recovery parks orphan attempts before any session
	// resumes, so no durable thread-to-item registry is needed after restart.
	workItems map[string]string
}

type workflowAttempt struct {
	sendMu               sync.Mutex
	key                  engine.RunKey
	threadID             string
	schema               json.RawMessage
	phase                def.Phase
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

func newWorkflowAppRunner(app *App, dataRoot string, profiles engine.ProfileSource) *workflowAppRunner {
	return &workflowAppRunner{
		app: app, dataRoot: dataRoot, profiles: profiles, now: time.Now,
		newTimer: func(delay time.Duration, callback func()) workflowTimer {
			return time.AfterFunc(delay, callback)
		},
		runs: make(map[string]*workflowAttempt), schemas: make(map[string]json.RawMessage),
		workItems: make(map[string]string),
	}
}

func (r *workflowAppRunner) Start(ctx context.Context, request engine.RunRequest, complete func(engine.Outcome)) error {
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
		thread, createErr := r.app.createWorkflowThread(request)
		if createErr != nil {
			return createErr
		}
		threadID = thread.ID
		createdThread = true
	} else if err := r.validatePriorThread(request, threadID); err != nil {
		return err
	}
	if err := r.app.store.AttachWorkItemPhaseRun(
		request.Key.ItemID, request.Key.PhaseID, request.Key.Attempt, threadID, narrativePath,
	); err != nil {
		if createdThread {
			err = errors.Join(err, r.app.store.DeleteThread(threadID))
		}
		return fmt.Errorf("workflow runner: attach phase run: %w", err)
	}

	prompt, err := workflowrunner.BuildPrompt(request.Phase, request.Vars, narrativePath, request.Feedback)
	if err != nil {
		return err
	}
	attempt := &workflowAttempt{
		key: request.Key, threadID: threadID,
		schema: append(json.RawMessage(nil), schema...), phase: request.Phase, complete: complete,
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
	r.mu.Unlock()

	if _, err := r.sendIfActive(key, prompt, schema); err != nil {
		r.finish(key, engine.Outcome{Kind: engine.OutcomeExecutionFailure})
	}
	return nil
}

func (r *workflowAppRunner) Stop(_ context.Context, key engine.RunKey) (json.RawMessage, error) {
	runKey := workflowRunKey(key)
	attempt, ok := r.detach(runKey)
	if !ok {
		return nil, nil
	}
	attempt.sendMu.Lock()
	attempt.sendMu.Unlock()
	if err := r.app.InterruptTurn(attempt.threadID); err != nil {
		// Stop's v1 contract does not return interrupt errors or partial
		// envelopes. Keep the failure visible while still releasing all local
		// runner state and returning the required no-op result.
		log.Printf("workflow runner: interrupt %s: %v", runKey, err)
	}
	return nil, nil
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
	attempt.complete(outcome)
}

func (r *workflowAppRunner) schemaForThread(threadID string) json.RawMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append(json.RawMessage(nil), r.schemas[threadID]...)
}

func (r *workflowAppRunner) workItemForThread(threadID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.workItems[threadID]
}

func findingsForEnvelopeError(err error) []def.EnvelopeFinding {
	var envelopeErr *def.EnvelopeValidationError
	if errors.As(err, &envelopeErr) {
		return envelopeErr.Findings
	}
	return []def.EnvelopeFinding{{Path: "$", Message: err.Error()}}
}

func (r *workflowAppRunner) validatePriorThread(request engine.RunRequest, threadID string) error {
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
	return nil
}

func (a *App) createWorkflowThread(request engine.RunRequest) (store.Thread, error) {
	project, err := a.store.GetProject(request.Item.ProjectID)
	if err != nil {
		return store.Thread{}, fmt.Errorf("workflow runner: load project %q: %w", request.Item.ProjectID, err)
	}
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
		WorkspacePath: project.Path, Mode: "workflow",
		ReasoningEffort: seed.ReasoningEffort, FastMode: seed.FastMode,
		ContextWindow: seed.ContextWindow, RuntimeMode: string(provider.RuntimeFullAccess),
		CreatedAt: now, UpdatedAt: now,
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
