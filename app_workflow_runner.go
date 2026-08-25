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
	"sync/atomic"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	workflowrunner "agent-overflow/internal/workflow/runner"
)

type workflowAppRunner struct {
	// host is the process around the runner, behind the capability seams in
	// app_workflow_runner_host.go. It is those interfaces rather than *App so
	// the runner's contract is what it actually uses, not everything the App
	// happens to expose.
	host workflowRunnerHost
	// store is a dependency rather than a host capability, held the same way
	// every other workflow collaborator in this package holds it.
	store    *store.Store
	dataRoot string
	profiles engine.ProfileSource
	now      func() time.Time
	newTimer func(time.Duration, func()) workflowTimer
	// stopSendWait is how long a stop taken on the engine's command loop waits
	// for an in-flight send before it leaves the rest to a goroutine. It is a
	// field rather than a bare constant read so a test can shrink it: the bound's
	// whole subject is a wait no test may actually sit through. See
	// workflowStopSendWait.
	stopSendWait time.Duration
	// interrupt fires a provider-level interrupt on a thread's active session,
	// with ctx bounding only the thread-action-lock acquisition. It is a field
	// rather than a direct call for the tests that assert an interrupt was — or,
	// for the late-interrupt guard, was NOT — asked for: the real body answers
	// nil for a thread with no live session either way. Production wires it to
	// `App.interruptTurnCtx` in the constructor.
	interrupt func(context.Context, string) error
	// wedgedStops counts bounded stop waits that expired and whose abandoned
	// work has not yet completed. While it is non-zero, `Stop` skips its wait
	// outright: the wedge cause is usually shared (a stalled provider IO layer,
	// a held per-thread lock), and a teardown looping over a wide fan-out's
	// units must not pay the bound once per unit — width × bound is exactly the
	// engine freeze the bound exists to prevent.
	wedgedStops atomic.Int32
	// workspaceLocks serializes primary-workspace provisioning per work item.
	// It is its own registry rather than the App's thread locks because the key
	// space is item ids, and the two must never contend.
	workspaceLocks *keyedLockRegistry

	mu        sync.Mutex
	runs      map[string]*workflowAttempt
	tools     map[string]*workflowToolAttempt
	schemas   map[string]json.RawMessage
	takeovers map[string]workflowTakeover
	// startProgress holds one entry per in-flight Runner.Start, keyed the same
	// way runs are. It is where the start deadline and its once-guard live. See
	// app_workflow_start_watchdog.go.
	startProgress map[string]*workflowStartProgress
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
	sendMu              sync.Mutex
	threadID            string
	schema              json.RawMessage
	contract            def.EnvelopeContract
	provider            string
	dispatchIdentity    providerDispatchIdentity
	dispatchIdentitySet bool
	phase               def.Phase
	complete            func(engine.Outcome)
	unsubscribe         func()
	envelopeRetryUsed   bool
	currentPrompt       string
	watchdog            time.Duration
	backoff             []time.Duration
	transientRetryCount int
	turnStarted         bool
	// currentTurnID is the id of the turn this attempt is waiting on, for
	// providers that name their turns. It is the completion filter's whole basis:
	// a replayed `turn/completed` for an earlier turn would otherwise finish the
	// phase on a stale envelope. Empty for Claude, which names no turn — the
	// filter is then inert rather than wrong.
	currentTurnID       string
	pendingTransient    bool
	pendingSessionDeath bool
	// awaitingTurnStart holds while a send has been dispatched and the provider
	// has not yet named the turn it started. Only the next EventTurnStart clears
	// it, so a terminal event still in flight from the PREVIOUS turn cannot be
	// mistaken for this one's — the shape that used to settle an attempt on a
	// stale envelope. Every Codex send sets it (retry, opening, envelope
	// feedback); Claude names no turn and never does.
	awaitingTurnStart bool
	// sendEpoch identifies the ladder state a queued send was decided in. The
	// send runs on its own goroutine, and by the time it reaches the wire the
	// attempt may have advanced a rung — a resend for the state that queued it is
	// then a stale turn landing inside somebody else's backoff window. Every
	// advance invalidates what the previous state queued.
	sendEpoch         int
	providerRetryHint bool
	timer             workflowTimer
	timerMode         workflowTimerMode
	timerDeadline     time.Time
	// lastFailure is the runner's account of the most recent provider failure
	// this attempt saw, carried onto the outcome so a park with no envelope
	// still names something. See `Outcome.Detail`.
	lastFailure string
}

// failureDetail is what this attempt tells the engine about a failure the
// element authored no envelope for: the last provider error it actually saw,
// with the fallback naming the shape of the failure when there was none. The
// fallback is never dropped — a park whose only account is "the session
// disconnected" is still an account, and it is what the empty envelope left
// missing.
func (a *workflowAttempt) failureDetail(fallback string) string {
	if a.lastFailure == "" {
		return workflowFailureDetail(fallback)
	}
	if fallback == "" {
		return a.lastFailure
	}
	return workflowFailureDetail(fallback + " (" + a.lastFailure + ")")
}

func newWorkflowAppRunner(app *App, dataRoot string, profiles engine.ProfileSource) *workflowAppRunner {
	return &workflowAppRunner{
		host: app, store: app.store,
		dataRoot: dataRoot, profiles: profiles, now: time.Now,
		newTimer: func(delay time.Duration, callback func()) workflowTimer {
			return time.AfterFunc(delay, callback)
		},
		stopSendWait:   workflowStopSendWait,
		interrupt:      app.interruptTurnCtx,
		workspaceLocks: newKeyedLocks(),
		runs:           make(map[string]*workflowAttempt), tools: make(map[string]*workflowToolAttempt),
		schemas:       make(map[string]json.RawMessage),
		takeovers:     make(map[string]workflowTakeover),
		workItems:     make(map[string]string),
		startProgress: make(map[string]*workflowStartProgress),
	}
}

// Start is the engine's entry point for one piece of work. The body is `start`;
// this wrapper owns the two things that must read the start's FINAL answer.
//
// `progress.finish` is where an expired start becomes an error, so it is the
// only place that answer exists. Anything that has to unwind on failure must
// therefore read `finish`'s result — not the body's. That used to be written as
// a pair of defers inside one function, and defers run LIFO: the takeover-cancel
// defer, registered second, ran FIRST and read the body's `nil` for a start the
// expiry was about to fail. The registration stayed `transitioning`, and every
// steering send into that thread was rejected until the process restarted.
//
// Written this way there is no named return for a defer to mutate, and the order
// is the reading order.
func (r *workflowAppRunner) Start(ctx context.Context, request engine.RunRequest, entered func(), complete func(engine.Outcome)) error {
	entered()
	// The body runs under the start's own cancellable context and inside its
	// progress record. `ctx` is rebound rather than shadowed by a new name so no
	// downstream site can keep using the engine's original: the deadline's whole
	// mechanism is that cancelling this context unwinds the start. See
	// app_workflow_start_watchdog.go.
	ctx, progress := r.beginStartProgress(ctx, request.Key, complete)
	if request.Launch.FinalizesTakeover() {
		// Registered on the record as well as handled below, because a wedged
		// finalize start may never return: `reportDead` releases the registration
		// for that channel. See the field.
		itemID, threadID := request.Key.ItemID, request.Launch.ThreadID()
		progress.cancelTakeover = func() { r.cancelTakeoverTransition(itemID, threadID) }
	}
	err := progress.finish(r.start(ctx, request, complete))
	if err != nil && request.Launch.FinalizesTakeover() {
		// A failed finalize start must not strand the thread's takeover
		// registration in transitioning state: steering sends would be rejected
		// until process restart. Success deletes the registration wholesale when
		// the attempt is installed, so this is a no-op on that path.
		r.cancelTakeoverTransition(request.Key.ItemID, request.Launch.ThreadID())
	}
	return err
}

func (r *workflowAppRunner) start(ctx context.Context, request engine.RunRequest, complete func(engine.Outcome)) error {
	if err := request.Launch.Validate(); err != nil {
		return fmt.Errorf("workflow runner: %w", err)
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
		if request.Launch.FinalizesTakeover() {
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
	r.markStartStep(request.Key, workflowStartStepReliability)
	watchdog, backoff, err := r.reliability(ctx, request)
	if err != nil {
		return err
	}
	r.markStartStep(request.Key, workflowStartStepWorkspace)
	preparedWorkspace, err := r.prepareWorkspace(ctx, request)
	if err != nil {
		return errors.Join(engine.ErrSetupFailed, err)
	}
	// Provisioning and setup hooks are behind us; everything left is bounded by
	// design, so this is where the start deadline starts running.
	r.armStartDeadline(request.Key)
	workspace := preparedWorkspace.path
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("workflow runner: startup cancelled: %w", err)
	}
	r.markStartStep(request.Key, workflowStartStepNarrative)
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
	promptContext := r.host.workflowPromptAncestry(request.Key.ItemID, request.Workflow)
	promptContext.NarrativePath = narrativePath
	promptContext.Access = request.Phase.EffectiveAccess()
	r.markStartStep(request.Key, workflowStartStepSessionProof)
	prepared, err := r.prepareAgentTurn(ctx, workflowAgentTurnPlan{
		request: request, thread: spec, schema: schema,
		promptContext: promptContext,
		buildFull: func(context workflowrunner.PromptContext) (string, error) {
			return workflowrunner.BuildPrompt(request.Phase, request.Vars, context)
		},
		attach: func(threadID string) error {
			if err := r.store.AttachWorkItemPhaseRun(
				request.Key.ItemID, request.Key.PhaseID, request.Key.Attempt, threadID, narrativePath,
			); err != nil {
				return fmt.Errorf("workflow runner: attach phase run: %w", err)
			}
			return nil
		},
	})
	if err != nil {
		return err
	}

	if request.Launch.FinalizesTakeover() && request.Phase.Provider == string(provider.Claude) && !prepared.startedSession {
		restarted, err := r.restartClaudeTakeoverWithSchema(ctx, prepared.threadID, schema)
		if err != nil {
			return prepared.discard(r, err)
		}
		prepared.startedSession = restarted
	}
	attempt := &workflowAttempt{
		workflowCompletion: workflowCompletion{
			key: request.Key, workflow: request.Workflow, narrativePath: narrativePath,
			workspace: workspace, projectPath: preparedWorkspace.project.Path,
		},
		threadID: prepared.threadID,
		schema:   append(json.RawMessage(nil), schema...), contract: contract,
		provider: request.Phase.Provider, phase: request.Phase, complete: complete,
		currentPrompt: prepared.prompt, watchdog: watchdog, backoff: backoff,
	}
	return r.installAttempt(ctx, attempt, prepared)
}

// restartClaudeTakeoverWithSchema re-launches a Claude session that was started
// for human steering without `--json-schema`. The finalize turn has to produce a
// validated envelope, and Claude only attaches a schema at process start.
//
// ctx is the start's, and it reaches the half that can wedge: the restart waits
// on the per-thread action lock, which is exactly where the start-wedge incident
// blocked. The stop has no context to take — it tears a live session down and
// that teardown has to complete — so a stop that hangs is the grace fallback's
// case rather than the deadline's.
func (r *workflowAppRunner) restartClaudeTakeoverWithSchema(ctx context.Context, threadID string, schema json.RawMessage) (bool, error) {
	r.mu.Lock()
	takeover, registered := r.takeovers[threadID]
	restart := registered && !takeover.schemaAttached
	if restart {
		r.schemas[threadID] = append(json.RawMessage(nil), schema...)
	}
	r.mu.Unlock()
	if !restart {
		return false, nil
	}
	if err := r.host.stopSession(threadID); err != nil {
		r.removeTemporarySchema(threadID)
		return false, fmt.Errorf("workflow runner: stop schema-less takeover session: %w", err)
	}
	if err := r.host.startSessionTakingLock(ctx, threadID); err != nil {
		r.removeTemporarySchema(threadID)
		return false, fmt.Errorf("workflow runner: restart takeover session with schema: %w", err)
	}
	return true, nil
}

// installAttempt registers a fully provisioned attempt and sends its opening
// prompt. Every agent-backed workflow turn — phase, unit, and join — ends here,
// so registration, observer wiring, the cancellation window, and the send-failure
// outcome are written once.
func (r *workflowAppRunner) installAttempt(ctx context.Context, attempt *workflowAttempt, prepared preparedWorkflowAgentTurn) error {
	key := workflowRunKey(attempt.key)
	threadID := attempt.threadID
	r.markStartStep(attempt.key, workflowStartStepInstall)
	attempt.unsubscribe = r.host.subscribeThreadTurnObserver(threadID, func(_ string, event provider.ProviderEvent) {
		r.observe(key, event)
	})
	r.mu.Lock()
	if _, exists := r.runs[key]; exists {
		r.mu.Unlock()
		attempt.unsubscribe()
		return prepared.discard(r, fmt.Errorf("workflow runner: attempt %s is already active", key))
	}
	// See startReportedDeadLocked: an attempt the fallback has already reported
	// dead must not be installed under a run the engine has parked.
	if r.startReportedDeadLocked(key) {
		r.mu.Unlock()
		attempt.unsubscribe()
		return prepared.discard(r, fmt.Errorf(
			"workflow runner: attempt %s was reported dead before it could be installed", key))
	}
	r.schemas[threadID] = append(json.RawMessage(nil), attempt.schema...)
	r.runs[key] = attempt
	r.workItems[threadID] = attempt.key.ItemID
	delete(r.takeovers, threadID)
	epoch := attempt.sendEpoch
	r.mu.Unlock()
	if err := prepared.attach(threadID); err != nil {
		r.detach(key)
		return prepared.discard(r, err)
	}
	prepared.attached = true
	if err := ctx.Err(); err != nil {
		r.detach(key)
		return prepared.discard(r, fmt.Errorf("workflow runner: startup cancelled: %w", err))
	}

	r.markStartStep(attempt.key, workflowStartStepOpeningSend)
	schema := append(json.RawMessage(nil), attempt.schema...)
	// A drop is owned and logged by the door; only an error is this caller's.
	if _, err := r.sendIfActive(ctx, key, "the opening prompt", attempt.currentPrompt, schema, epoch); err != nil {
		if ctx.Err() != nil {
			// The send did not fail on its own — the start's context was cancelled
			// under it (the start deadline, or engine teardown). Returning the error
			// hands the classification to the ONE reporting channel: the watchdog's
			// `finish` renders an expired start as `ErrSetupFailed`, where a local
			// completion here would park it `agent-error` and then let that same
			// channel log the start as a live success.
			r.detach(key)
			return fmt.Errorf("workflow runner: the opening prompt was cancelled mid-send: %w", err)
		}
		r.finish(key, engine.Outcome{
			Kind:   engine.OutcomeExecutionFailure,
			Detail: workflowFailureDetail("sending the opening prompt failed: " + err.Error()),
		})
	}
	return nil
}

// Stop is the engine's teardown of one live attempt, and it runs ON the engine's
// command-loop goroutine — the sole owner of every run's FSM state.
//
// That is what shapes the body. The detach is SYNCHRONOUS and unconditional:
// single-claim semantics, so a send decided before it drops at `sendIfActive`'s
// admission recheck and no provider event can reach the attempt again. Only then
// is the blocking half — waiting out an in-flight send, then interrupting the
// turn — put on its own goroutine and given a bound. A send wedged on provider
// IO used to hold this method, and with it the command loop, indefinitely.
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
	if r.wedgedStops.Load() > 0 {
		// An earlier stop's wait already expired and its work is still stuck, so
		// this one's would almost certainly expire too — the wedge cause is
		// usually shared — and a teardown looping over a wave's units must not
		// pay the bound once per unit. Skipping is exactly the expiry path taken
		// immediately: the attempt is already detached, and the goroutine owns
		// the barrier wait and the interrupt from here.
		log.Printf(
			"workflow runner: stopping %s without waiting: an earlier stop's send barrier is still wedged; "+
				"interrupting the turn on thread %s when this one clears",
			runKey, attempt.threadID,
		)
		go r.interruptDetachedAttempt(runKey, attempt)
		return nil, nil
	}
	if !r.runBoundedBySendWait(func() { r.interruptDetachedAttempt(runKey, attempt) }) {
		// A finding, not routine slowness: nothing healthy holds this attempt's
		// send barrier or its thread's action lock for this long, so the stop is
		// wedged on provider IO — mid-send, or under the lock the interrupt
		// needs. The run is already torn down — the detach above saw to that —
		// and the goroutine still owns the interrupt, so the session comes down
		// whenever the wedge clears.
		log.Printf(
			"workflow runner: stopping %s left its send barrier and interrupt on thread %s unfinished after %s; "+
				"returning to the engine now and interrupting the turn when the wedge clears",
			runKey, attempt.threadID, r.stopSendWait,
		)
	}
	return nil, nil
}

func (r *workflowAppRunner) finish(runKey string, outcome engine.Outcome) {
	attempt, ok := r.detach(runKey)
	if !ok {
		return
	}
	r.finishDetachedAttempt(attempt, outcome)
}

// finishOffWire is `finish` for a caller running ON the provider event path —
// `observe`, dispatched synchronously on the session's read-loop goroutine.
//
// The detach is synchronous, so the single-claim semantics are identical to
// `finish`: any event behind this one finds no attempt. What moves off the wire
// is the send barrier and the completion. `finishDetachedAttempt` waits out an
// in-flight send, and a send can hold `sendMu` while it waits on the read loop
// itself — a reconnect inside the send path blocks in `Session.Close` until the
// read loop exits, and a dispatched send waits for a JSON-RPC reply only the
// read loop can deliver. Blocking the read loop on `sendMu` therefore deadlocks
// the two against each other permanently; `stopAndFinishOffWire` documents the
// same rule for the interrupt half.
func (r *workflowAppRunner) finishOffWire(runKey string, outcome engine.Outcome) {
	attempt, ok := r.detach(runKey)
	if !ok {
		return
	}
	go r.finishDetachedAttempt(attempt, outcome)
}

func (r *workflowAppRunner) finishDetachedAttempt(attempt *workflowAttempt, outcome engine.Outcome) {
	r.awaitInFlightSend(attempt)
	// Every agent-backed workflow turn — phase, unit, join, Answer continuation,
	// takeover finalize — reports here, which makes this the one seam the
	// envelope's optional `narrative` can be lifted out at. Stripping it is
	// unconditional so nothing downstream can ever see prose in an envelope:
	// gate evaluation, a join's `units` results, call synthesis, and the
	// persisted attempt envelope all read `outcome.Envelope` from here on.
	authored, stripped := def.SplitEnvelopeNarrative(outcome.Envelope)
	// Campaign memory rides the same seam and is stripped just as
	// unconditionally: `memory` is a prose channel, and the engine's contract is
	// that nothing downstream of here ever carries one. The notes are recorded
	// after the narrative settles, below, because the recording is best-effort
	// and must never sit between an accepted envelope and its account.
	notes, stripped := def.SplitEnvelopeMemory(stripped)
	// The ORIGINAL payload is what settles the narrative, not the stripped one:
	// recovery recognizes the assistant text that IS the envelope by comparing
	// decoded documents, and the text the session sent still carries the field.
	original := outcome.Envelope
	outcome.Envelope = stripped
	// An accepted envelope means the turn ended the way the element was told to
	// end it, so this is the last moment its narrative can still be settled —
	// the engine transition that follows is what the wake and the triage seed
	// read it for. It runs before `complete` on every branch, including the
	// asynchronous done one, so neither surface can observe a missing file that
	// was about to appear.
	if workflowOutcomeCarriesEnvelope(outcome.Kind) {
		r.settleAttemptNarrative(attempt, authored, original)
		r.host.recordEnvelopeMemory(attempt.key, notes)
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
