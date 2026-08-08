package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"agent-overflow/internal/provider"
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
	// workspaceLocks serializes primary-workspace provisioning per work item.
	// It is its own registry rather than the App's thread locks because the key
	// space is item ids, and the two must never contend.
	workspaceLocks *keyedLockRegistry

	mu        sync.Mutex
	runs      map[string]*workflowAttempt
	tools     map[string]*workflowToolAttempt
	schemas   map[string]json.RawMessage
	takeovers map[string]workflowTakeover
	// workItems attributes usage only while a phase or unit thread can emit a
	// live turn. Startup crash recovery parks orphan attempts before any session
	// resumes, so no durable thread-to-item registry is needed after restart.
	workItems map[string]string
}

// workflowCompletion identifies one piece of engine work and carries everything
// the runner still owes the run once that work reports done. Both execution
// paths — a provider turn and a deterministic command — embed it, so artifact
// capture and sub-worktree retirement happen for a tool join exactly as they do
// for an agent one.
type workflowCompletion struct {
	key      engine.RunKey
	unitKind engine.UnitKind
	workflow def.Workflow
	// narrativePath is where this piece of work's human-readable account lives.
	// It is part of the completion contract rather than a per-driver field
	// because every path owes the run one: a tool run writes it from its output,
	// and an agent run that ended without one has it recovered from its final
	// message.
	narrativePath string
	workspace     string
	projectPath   string
}

// producesPhaseEnvelope reports whether this work's envelope is the phase's own
// — true for a single-shape phase and for a fan-out's join, false for a work
// unit, whose outputs reach the gate only through its join.
func (c workflowCompletion) producesPhaseEnvelope() bool {
	return c.key.UnitID == "" || c.unitKind == engine.UnitJoin
}

// workflowAttempt is one live provider-backed workflow turn: a single-shape
// phase attempt, a fan-out unit, or a fan-out's join. `contract` rather than
// `phase` decides what its envelope must satisfy, because a work unit answers
// its own declaration while a join answers the phase's.
type workflowAttempt struct {
	workflowCompletion
	sendMu               sync.Mutex
	threadID             string
	schema               json.RawMessage
	contract             def.EnvelopeContract
	provider             string
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
		workspaceLocks: newKeyedLocks(),
		runs:           make(map[string]*workflowAttempt), tools: make(map[string]*workflowToolAttempt),
		schemas:   make(map[string]json.RawMessage),
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
	if request.Key.UnitID != "" {
		return r.startUnit(ctx, request, complete)
	}
	// A fan-out phase runs no turn of its own: the engine drives its units and
	// its join, and only they reach a runner. A phase key arriving here for one
	// is a scheduling bug, not a definition problem.
	if request.Phase.EffectiveShape() != def.ShapeSingle {
		return fmt.Errorf(
			"workflow runner: phase %q has shape %q and cannot run as one attempt",
			request.Phase.ID, request.Phase.EffectiveShape(),
		)
	}
	switch request.Phase.Driver {
	case def.DriverTool:
		if request.FinalizeTakeover {
			return fmt.Errorf("workflow runner: tool phase %q cannot finalize a takeover", request.Phase.ID)
		}
		return r.startToolPhase(ctx, request, complete)
	case def.DriverAgent:
	default:
		return fmt.Errorf("workflow runner: phase %q uses unsupported driver %q; agent and tool are available", request.Phase.ID, request.Phase.Driver)
	}
	contract := def.PhaseEnvelope(request.Phase)
	schema, err := contract.Schema()
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

	spec := workflowThreadSpec{
		itemID: request.Key.ItemID, label: fmt.Sprintf("phase %q", request.Phase.ID),
		title:        workflowThreadTitle(request.Phase.Name, request.Phase.ID),
		providerName: request.Phase.Provider, model: request.Phase.Model,
		effort: request.Phase.Effort,
		access: request.Phase.EffectiveAccess(), workspace: preparedWorkspace,
	}
	threadID := request.PriorThreadID
	createdThread := false
	if threadID == "" {
		thread, createErr := r.app.createWorkflowThread(spec)
		if createErr != nil {
			return createErr
		}
		threadID = thread.ID
		createdThread = true
	} else if err := r.validatePriorThread(spec, threadID); err != nil {
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
		prompt, err = workflowrunner.BuildTakeoverFinalizePrompt(narrativePath, request.Phase.EffectiveAccess())
	} else {
		prompt, err = workflowrunner.BuildPrompt(request.Phase, request.Vars, narrativePath, request.Feedback)
	}
	if err != nil {
		return err
	}

	if request.FinalizeTakeover && request.Phase.Provider == string(provider.Claude) {
		if err := r.restartClaudeTakeoverWithSchema(threadID, schema); err != nil {
			return err
		}
	}
	attempt := &workflowAttempt{
		workflowCompletion: workflowCompletion{
			key: request.Key, workflow: request.Workflow, narrativePath: narrativePath,
			workspace: workspace, projectPath: preparedWorkspace.project.Path,
		},
		threadID: threadID,
		schema:   append(json.RawMessage(nil), schema...), contract: contract,
		provider: request.Phase.Provider, phase: request.Phase, complete: complete,
		currentPrompt: prompt, watchdog: watchdog, backoff: backoff,
	}
	return r.installAttempt(ctx, attempt, createdThread)
}

// restartClaudeTakeoverWithSchema re-launches a Claude session that was started
// for human steering without `--json-schema`. The finalize turn has to produce a
// validated envelope, and Claude only attaches a schema at process start.
func (r *workflowAppRunner) restartClaudeTakeoverWithSchema(threadID string, schema json.RawMessage) error {
	r.mu.Lock()
	takeover, registered := r.takeovers[threadID]
	restart := registered && !takeover.schemaAttached
	if restart {
		r.schemas[threadID] = append(json.RawMessage(nil), schema...)
	}
	r.mu.Unlock()
	if !restart {
		return nil
	}
	if err := r.app.StopSession(threadID); err != nil {
		r.removeTemporarySchema(threadID)
		return fmt.Errorf("workflow runner: stop schema-less takeover session: %w", err)
	}
	if err := r.app.StartSession(threadID); err != nil {
		r.removeTemporarySchema(threadID)
		return fmt.Errorf("workflow runner: restart takeover session with schema: %w", err)
	}
	return nil
}

// installAttempt registers a fully provisioned attempt and sends its opening
// prompt. Every agent-backed workflow turn — phase, unit, and join — ends here,
// so registration, observer wiring, the cancellation window, and the send-failure
// outcome are written once.
func (r *workflowAppRunner) installAttempt(ctx context.Context, attempt *workflowAttempt, createdThread bool) error {
	key := workflowRunKey(attempt.key)
	threadID := attempt.threadID
	attempt.unsubscribe = r.app.subscribeThreadTurnObserver(threadID, func(_ string, event provider.ProviderEvent) {
		r.observe(key, event)
	})
	r.mu.Lock()
	if _, exists := r.runs[key]; exists {
		r.mu.Unlock()
		attempt.unsubscribe()
		return fmt.Errorf("workflow runner: attempt %s is already active", key)
	}
	r.schemas[threadID] = append(json.RawMessage(nil), attempt.schema...)
	r.runs[key] = attempt
	r.workItems[threadID] = attempt.key.ItemID
	delete(r.takeovers, threadID)
	r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		r.detach(key)
		if createdThread {
			err = errors.Join(err, r.app.store.DeleteThread(threadID))
		}
		return fmt.Errorf("workflow runner: startup cancelled: %w", err)
	}

	schema := append(json.RawMessage(nil), attempt.schema...)
	if _, err := r.sendIfActive(key, attempt.currentPrompt, schema); err != nil {
		r.finish(key, engine.Outcome{Kind: engine.OutcomeExecutionFailure})
	}
	return nil
}

func (r *workflowAppRunner) Stop(_ context.Context, key engine.RunKey) (json.RawMessage, error) {
	runKey := workflowRunKey(key)
	// A tool phase has no turn to interrupt: teardown kills its process group
	// and the reaping goroutine writes the attempt narrative.
	if _, stopped := r.stopToolAttempt(runKey, workflowToolTornDown); stopped {
		return nil, nil
	}
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

func (r *workflowAppRunner) finish(runKey string, outcome engine.Outcome) {
	attempt, ok := r.detach(runKey)
	if !ok {
		return
	}
	attempt.sendMu.Lock()
	attempt.sendMu.Unlock()
	// An accepted envelope means the turn ended the way the element was told to
	// end it, so this is the last moment its narrative can still be recovered —
	// the engine transition that follows is what the wake and the triage seed
	// read it for. It runs before `complete` on every branch, including the
	// asynchronous done one, so neither surface can observe a missing file that
	// was about to appear.
	if workflowOutcomeCarriesEnvelope(outcome.Kind) {
		r.recoverAttemptNarrative(attempt, outcome.Envelope)
	}
	if outcome.Kind == engine.OutcomeDone {
		go func() {
			r.settleDone(attempt.workflowCompletion, outcome.Envelope)
			attempt.complete(outcome)
		}()
		return
	}
	attempt.complete(outcome)
}

// workflowOutcomeCarriesEnvelope reports whether an outcome is one the element
// produced through a validated control envelope. The three envelope statuses are
// enumerated rather than defaulted, because every other outcome — a failure, a
// stall, an exhausted retry — reached `finish` without the element having said
// anything the run can quote.
func workflowOutcomeCarriesEnvelope(kind engine.OutcomeKind) bool {
	switch kind {
	case engine.OutcomeDone, engine.OutcomeQuestion, engine.OutcomeStuck:
		return true
	default:
		return false
	}
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

func findingsForEnvelopeError(err error) []def.EnvelopeFinding {
	var envelopeErr *def.EnvelopeValidationError
	if errors.As(err, &envelopeErr) {
		return envelopeErr.Findings
	}
	return []def.EnvelopeFinding{{Path: "$", Message: err.Error()}}
}

// workflowRunKey is the app runner's map key for one live piece of engine work.
// It carries the unit id so a fan-out attempt's units never collide with each
// other or with their phase's own key.
func workflowRunKey(key engine.RunKey) string {
	if key.UnitID != "" {
		return fmt.Sprintf("%s/%s/%d/%s", key.ItemID, key.PhaseID, key.Attempt, key.UnitID)
	}
	return fmt.Sprintf("%s/%s/%d", key.ItemID, key.PhaseID, key.Attempt)
}
