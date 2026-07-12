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

	mu      sync.Mutex
	runs    map[string]*workflowAttempt
	schemas map[string]json.RawMessage
}

type workflowAttempt struct {
	sendMu      sync.Mutex
	key         engine.RunKey
	threadID    string
	schema      json.RawMessage
	phase       def.Phase
	complete    func(engine.Outcome)
	unsubscribe func()
	retried     bool
}

func newWorkflowAppRunner(app *App, dataRoot string) *workflowAppRunner {
	return &workflowAppRunner{
		app: app, dataRoot: dataRoot,
		runs: make(map[string]*workflowAttempt), schemas: make(map[string]json.RawMessage),
	}
}

func (r *workflowAppRunner) Start(_ context.Context, request engine.RunRequest, complete func(engine.Outcome)) error {
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
	r.mu.Unlock()

	if _, err := r.sendIfActive(key, prompt, schema); err != nil {
		r.finish(key, engine.Outcome{Kind: engine.OutcomeExecutionFailure})
	}
	return nil
}

func (r *workflowAppRunner) Stop(_ context.Context, key engine.RunKey) (json.RawMessage, error) {
	runKey := workflowRunKey(key)
	r.mu.Lock()
	attempt, ok := r.runs[runKey]
	if ok {
		delete(r.runs, runKey)
		delete(r.schemas, attempt.threadID)
	}
	r.mu.Unlock()
	if !ok {
		return nil, nil
	}
	if attempt.unsubscribe != nil {
		attempt.unsubscribe()
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
	if event.Kind == provider.EventError && workflowTurnErrorIsTerminal(event.Meta) {
		r.finish(runKey, engine.Outcome{Kind: engine.OutcomeExecutionFailure})
		return
	}
	if event.Kind == provider.EventSessionStatus && (event.Content == "error" || event.Content == "disconnected") {
		r.finish(runKey, engine.Outcome{Kind: engine.OutcomeExecutionFailure})
		return
	}
	if event.Kind != provider.EventTurnComplete {
		return
	}

	r.mu.Lock()
	attempt := r.runs[runKey]
	if attempt == nil {
		r.mu.Unlock()
		return
	}
	payload := append(json.RawMessage(nil), event.StructuredOutput...)
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
	if attempt.retried {
		r.mu.Unlock()
		r.finish(runKey, engine.Outcome{Kind: engine.OutcomeExecutionFailure, Envelope: payload})
		return
	}
	attempt.retried = true
	schema := append(json.RawMessage(nil), attempt.schema...)
	r.mu.Unlock()

	var envelopeErr *def.EnvelopeValidationError
	var findings []def.EnvelopeFinding
	if errors.As(validationErr, &envelopeErr) {
		findings = envelopeErr.Findings
	} else {
		findings = []def.EnvelopeFinding{{Path: "$", Message: validationErr.Error()}}
	}
	message := workflowrunner.RetryMessage(findings)
	go func() {
		sent, err := r.sendIfActive(runKey, message, schema)
		if sent && err != nil {
			r.finish(runKey, engine.Outcome{Kind: engine.OutcomeExecutionFailure, Envelope: payload})
		}
	}()
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
	return true, r.app.sendWorkflowMessage(attempt.threadID, message, schema)
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
	r.mu.Lock()
	attempt, ok := r.runs[runKey]
	if ok {
		delete(r.runs, runKey)
		delete(r.schemas, attempt.threadID)
	}
	r.mu.Unlock()
	if !ok {
		return
	}
	if attempt.unsubscribe != nil {
		attempt.unsubscribe()
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
