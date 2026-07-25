package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

const commandBuffer = 256

type runtimeItem struct {
	item              store.WorkItem
	workflow          def.Workflow
	phaseID           string
	attempt           int
	runnerActive      bool
	runnerStarting    bool
	runnerStartCancel context.CancelFunc
	waiting           bool
	acquired          []string
	feedback          *Feedback
	priorThreadID     string
	takeoverFinalize  bool
}

type resourceKey struct {
	projectID string
	name      string
}

// Engine owns all mutable scheduler and FSM state on its command goroutine.
type Engine struct {
	store       persistence
	runner      Runner
	emitter     Emitter
	definitions DefinitionSource
	profiles    ProfileSource
	spend       SpendSource
	now         func() time.Time

	commands chan any
	done     chan struct{}
	lifeMu   sync.Mutex
	started  bool
	closing  bool
	used     bool

	ctx            context.Context
	items          map[string]*runtimeItem
	waiting        []*runtimeItem
	waitingByID    map[string]struct{}
	holders        map[resourceKey]int
	paused         bool
	lastTimestamp  int64
	commandStarts  []*runnerStartFuture
	inflightStarts map[*runnerStartFuture]struct{}
}

func New(store persistence, runner Runner, emitter Emitter, definitions DefinitionSource, profiles ProfileSource, spend SpendSource, config Config) (*Engine, error) {
	if store == nil || runner == nil || emitter == nil || definitions == nil || profiles == nil || spend == nil {
		return nil, fmt.Errorf("workflow engine: store, runner, emitter, definition source, profile source, and spend source are required")
	}
	return &Engine{
		store: store, runner: runner, emitter: emitter, definitions: definitions,
		profiles: profiles, spend: spend, paused: config.Paused, now: time.Now,
		commands: make(chan any, commandBuffer), done: make(chan struct{}),
		items: make(map[string]*runtimeItem), holders: make(map[resourceKey]int),
		waitingByID:    make(map[string]struct{}),
		inflightStarts: make(map[*runnerStartFuture]struct{}),
	}, nil
}

// Start launches the single owner goroutine and synchronously completes the
// SQLite rebuild and crash sweep before returning.
func (e *Engine) Start(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("workflow engine: start context is required")
	}
	e.lifeMu.Lock()
	if e.started || e.used {
		e.lifeMu.Unlock()
		return fmt.Errorf("workflow engine: instances may be started only once")
	}
	e.started = true
	e.used = true
	e.ctx = ctx
	e.lifeMu.Unlock()

	go e.loop()
	if err := e.request(initCommand{}); err != nil {
		return errors.Join(err, e.Close())
	}
	return nil
}

// Close stops the command goroutine. Active persisted attempts intentionally
// remain running so the next Start performs the specified interrupted sweep;
// in-flight startup workers are cancelled because they cannot report a result
// after the command loop exits.
func (e *Engine) Close() error {
	e.lifeMu.Lock()
	if !e.started {
		if e.used {
			e.lifeMu.Unlock()
			return nil
		}
		e.lifeMu.Unlock()
		return fmt.Errorf("workflow engine: not started")
	}
	if e.closing {
		done := e.done
		e.lifeMu.Unlock()
		<-done
		return nil
	}
	e.closing = true
	e.lifeMu.Unlock()

	err := e.request(closeCommand{})
	<-e.done
	e.lifeMu.Lock()
	e.started = false
	e.closing = false
	e.lifeMu.Unlock()
	return err
}

// StartItem admits a run and begins its first phase immediately. There is no
// queue: the item is persisted running, and contention shows up as its phase
// waiting on resource capacity.
func (e *Engine) StartItem(item store.WorkItem) error {
	return e.request(startCommand{item: item, waitForStarts: true})
}

// StartItemDetachedStarts validates and persists synchronously, but does not
// attach runner provisioning futures to the caller's response. Async startup
// failures still park the item and emit workflow:error.
func (e *Engine) StartItemDetachedStarts(item store.WorkItem) error {
	return e.request(startCommand{item: item})
}

// Pause toggles the global kill switch. While paused no phase starts anywhere;
// in-flight turns finish and their items rest at the next phase boundary with
// the run still `running`. Unpausing releases every held start.
func (e *Engine) Pause(paused bool) error {
	return e.request(pauseCommand{paused: paused, waitForStarts: true})
}

// PauseDetachedStarts applies the pause flag synchronously while leaving any
// starts an unpause triggers to report through workflow events.
func (e *Engine) PauseDetachedStarts(paused bool) error {
	return e.request(pauseCommand{paused: paused})
}

// Paused reports the live global pause flag. The result pointer is written on
// the owner goroutine and read only after the reply channel receives, so the
// channel handoff is the synchronization.
func (e *Engine) Paused() (bool, error) {
	var paused bool
	if err := e.request(pauseStateCommand{result: &paused}); err != nil {
		return false, err
	}
	return paused, nil
}

func (e *Engine) Cancel(itemID string) error {
	return e.request(cancelCommand{itemID: itemID})
}

// Resume moves a parked item back to running and starts targetPhase. An empty
// target re-runs the phase that parked, which is appropriate for question and
// stuck intervention flows.
func (e *Engine) Resume(itemID, targetPhase string) error {
	return e.request(resumeCommand{itemID: itemID, targetPhase: targetPhase})
}

// Answer continues a question-parked phase as the next turn on its prior
// workflow thread with the phase schema still attached.
func (e *Engine) Answer(itemID, answer string) error {
	return e.request(answerCommand{itemID: itemID, answer: answer})
}

// TakeOver detaches a running or parked phase attempt from engine control and
// parks it for schema-less human steering.
func (e *Engine) TakeOver(itemID string) error {
	return e.request(takeoverCommand{itemID: itemID})
}

// CompleteTakeover starts one schema-attached finalize turn on the phase
// thread that was taken over.
func (e *Engine) CompleteTakeover(itemID string) error {
	return e.request(completeTakeoverCommand{itemID: itemID})
}

func (e *Engine) ResolveHumanGate(itemID string, decision HumanDecision, note string) error {
	return e.request(humanGateCommand{itemID: itemID, decision: decision, note: note})
}

// Sync waits until every command submitted before it has been processed.
func (e *Engine) Sync() error { return e.request(syncCommand{}) }

type response struct {
	err    error
	starts []*runnerStartFuture
}

type runnerStartFuture struct {
	key  RunKey
	done chan response
}
type initCommand struct{ reply chan response }
type closeCommand struct{ reply chan response }
type syncCommand struct{ reply chan response }
type startCommand struct {
	item          store.WorkItem
	waitForStarts bool
	reply         chan response
}
type cancelCommand struct {
	itemID string
	reply  chan response
}
type parkDispositionCommand struct {
	itemID string
	reply  chan response
}
type rerunFailedCommand struct {
	itemID string
	reply  chan response
}
type resolveDispositionCommand struct {
	itemID string
	reply  chan response
}
type resumeCommand struct {
	itemID      string
	targetPhase string
	reply       chan response
}
type answerCommand struct {
	itemID string
	answer string
	reply  chan response
}
type takeoverCommand struct {
	itemID string
	reply  chan response
}
type completeTakeoverCommand struct {
	itemID string
	reply  chan response
}
type pauseCommand struct {
	paused        bool
	waitForStarts bool
	reply         chan response
}
type pauseStateCommand struct {
	result *bool
	reply  chan response
}
type humanGateCommand struct {
	itemID   string
	decision HumanDecision
	note     string
	reply    chan response
}
type completionCommand struct {
	key     RunKey
	outcome Outcome
}
type runnerStartCommand struct {
	key    RunKey
	future *runnerStartFuture
	err    error
}

func (e *Engine) request(command any) error {
	e.lifeMu.Lock()
	if !e.started {
		e.lifeMu.Unlock()
		return fmt.Errorf("workflow engine: not started")
	}
	_, closeRequest := command.(closeCommand)
	if e.closing && !closeRequest {
		e.lifeMu.Unlock()
		return fmt.Errorf("workflow engine: closing")
	}
	reply := make(chan response, 1)
	switch command := command.(type) {
	case initCommand:
		command.reply = reply
		e.commands <- command
	case closeCommand:
		command.reply = reply
		e.commands <- command
	case syncCommand:
		command.reply = reply
		e.commands <- command
	case startCommand:
		command.reply = reply
		e.commands <- command
	case cancelCommand:
		command.reply = reply
		e.commands <- command
	case parkDispositionCommand:
		command.reply = reply
		e.commands <- command
	case rerunFailedCommand:
		command.reply = reply
		e.commands <- command
	case resolveDispositionCommand:
		command.reply = reply
		e.commands <- command
	case resumeCommand:
		command.reply = reply
		e.commands <- command
	case answerCommand:
		command.reply = reply
		e.commands <- command
	case takeoverCommand:
		command.reply = reply
		e.commands <- command
	case completeTakeoverCommand:
		command.reply = reply
		e.commands <- command
	case pauseCommand:
		command.reply = reply
		e.commands <- command
	case pauseStateCommand:
		command.reply = reply
		e.commands <- command
	case humanGateCommand:
		command.reply = reply
		e.commands <- command
	default:
		e.lifeMu.Unlock()
		return fmt.Errorf("workflow engine: unsupported command %T", command)
	}
	e.lifeMu.Unlock()
	return waitEngineResponse(<-reply)
}

func waitEngineResponse(result response) error {
	errs := []error{result.err}
	for _, start := range result.starts {
		errs = append(errs, waitEngineResponse(<-start.done))
	}
	return errors.Join(errs...)
}

func (e *Engine) commandResponse(err error) response {
	starts := append([]*runnerStartFuture(nil), e.commandStarts...)
	e.commandStarts = nil
	return response{err: err, starts: starts}
}

func (e *Engine) itemCommandResponse(itemID string, err error) response {
	starts := make([]*runnerStartFuture, 0, len(e.commandStarts))
	for _, start := range e.commandStarts {
		if start.key.ItemID == itemID {
			starts = append(starts, start)
		}
	}
	e.commandStarts = nil
	return response{err: err, starts: starts}
}

func (e *Engine) syncResponse() response {
	starts := make([]*runnerStartFuture, 0, len(e.inflightStarts))
	for start := range e.inflightStarts {
		starts = append(starts, start)
	}
	return response{starts: starts}
}

func settleRunnerStart(start *runnerStartFuture, result response) {
	select {
	case start.done <- result:
	default:
	}
}

func (e *Engine) loop() {
	defer close(e.done)
	for command := range e.commands {
		e.commandStarts = nil
		var err error
		switch command := command.(type) {
		case initCommand:
			err = e.rebuild()
			if err == nil {
				err = e.startWaiting()
			}
			command.reply <- e.commandResponse(err)
		case startCommand:
			err = e.startNewItem(command.item)
			if command.waitForStarts {
				command.reply <- e.itemCommandResponse(command.item.ID, err)
			} else {
				e.commandStarts = nil
				command.reply <- response{err: err}
			}
		case cancelCommand:
			err = e.cancel(command.itemID)
			e.commandStarts = nil
			command.reply <- response{err: err}
		case parkDispositionCommand:
			err = e.parkDisposition(command.itemID)
			e.commandStarts = nil
			command.reply <- response{err: err}
		case rerunFailedCommand:
			err = e.rerunFailed(command.itemID)
			command.reply <- e.itemCommandResponse(command.itemID, err)
		case resolveDispositionCommand:
			err = e.resolveDisposition(command.itemID)
			e.commandStarts = nil
			command.reply <- response{err: err}
		case resumeCommand:
			err = e.resume(command.itemID, command.targetPhase)
			command.reply <- e.itemCommandResponse(command.itemID, err)
		case answerCommand:
			err = e.answer(command.itemID, command.answer)
			command.reply <- e.itemCommandResponse(command.itemID, err)
		case takeoverCommand:
			err = errors.Join(e.takeOver(command.itemID), e.startWaiting())
			e.commandStarts = nil
			command.reply <- response{err: err}
		case completeTakeoverCommand:
			err = e.completeTakeover(command.itemID)
			command.reply <- e.itemCommandResponse(command.itemID, err)
		case pauseCommand:
			if e.paused != command.paused {
				e.paused = command.paused
				e.emitEngineState()
			}
			if !e.paused {
				startErr := e.startWaiting()
				if command.waitForStarts {
					err = startErr
				}
			}
			if command.waitForStarts {
				command.reply <- e.commandResponse(err)
			} else {
				e.commandStarts = nil
				command.reply <- response{err: err}
			}
		case pauseStateCommand:
			*command.result = e.paused
			command.reply <- response{}
		case humanGateCommand:
			err = errors.Join(e.resolveHumanGate(command.itemID, command.decision, command.note), e.startWaiting())
			command.reply <- e.itemCommandResponse(command.itemID, err)
		case runnerStartCommand:
			err = e.finishRunnerStart(command)
			if err != nil {
				e.emitError(command.key.ItemID, err)
			}
			_ = e.startWaiting() // startWaiting reports item-scoped failures itself.
			delete(e.inflightStarts, command.future)
			e.commandStarts = nil
			settleRunnerStart(command.future, response{err: err})
		case completionCommand:
			if err = e.complete(command.key, command.outcome); err != nil {
				e.emitError(command.key.ItemID, err)
			}
			_ = e.startWaiting() // startWaiting emits item-scoped asynchronous errors itself.
		case syncCommand:
			command.reply <- e.syncResponse()
		case closeCommand:
			for _, item := range e.items {
				if item.runnerStarting && item.runnerStartCancel != nil {
					item.runnerStartCancel()
				}
			}
			for start := range e.inflightStarts {
				settleRunnerStart(start, response{err: fmt.Errorf("workflow engine closed before runner startup settled")})
				delete(e.inflightStarts, start)
			}
			command.reply <- response{}
			return
		}
	}
}

func (e *Engine) cancel(itemID string) error {
	item, ok := e.items[itemID]
	if !ok {
		return fmt.Errorf("cancel item %q: not tracked", itemID)
	}
	if State(item.item.State) != StateRunning {
		return fmt.Errorf("cancel item %q: invalid transition %s -> %s", itemID, item.item.State, StateCancelled)
	}
	err := e.teardown(item, teardownRequest{phaseStatus: "cancelled", nextState: StateCancelled, reason: ReasonInterrupted})
	return errors.Join(err, e.startWaiting())
}

func (e *Engine) resume(itemID, targetPhase string) error {
	item, ok := e.items[itemID]
	if !ok {
		var err error
		item, err = e.loadParked(itemID)
		if err != nil {
			return err
		}
	}
	if State(item.item.State) != StateNeedsHuman {
		return fmt.Errorf("resume item %q: invalid transition %s -> %s", itemID, item.item.State, StateRunning)
	}
	if Reason(item.item.Reason) == ReasonDisposition {
		return fmt.Errorf("resume item %q: disposition requires WorkflowMergeItem, WorkflowCreateItemPR, or WorkflowDiscardItem", itemID)
	}
	if Reason(item.item.Reason) == ReasonGate {
		humanGate, err := e.isHumanGate(item)
		if err != nil {
			return err
		}
		if humanGate {
			return fmt.Errorf("resume item %q: human gate decisions require ResolveHumanGate", itemID)
		}
	}
	if len(item.workflow.Phases) == 0 {
		workflow, err := e.definitions.Resolve(e.ctx, item.item)
		if err != nil {
			return fmt.Errorf("resume setup-failed item %q: %w", itemID, err)
		}
		if len(workflow.Phases) == 0 {
			return fmt.Errorf("resume setup-failed item %q: workflow has no phases", itemID)
		}
		snapshot, err := json.Marshal(Snapshot{Workflow: workflow})
		if err != nil {
			return fmt.Errorf("resume setup-failed item %q snapshot: %w", itemID, err)
		}
		if len(snapshot) > MaxSnapshotBytes {
			return fmt.Errorf("resume setup-failed item %q snapshot is %d bytes; maximum is %d", itemID, len(snapshot), MaxSnapshotBytes)
		}
		startedAt := e.timestamp()
		if err := e.store.UpdateWorkItemRunStart(
			itemID, snapshot, item.item.WorktreePath, item.item.Branch,
			item.item.BaseBranch, startedAt,
		); err != nil {
			return err
		}
		item.workflow = workflow
		item.item.Snapshot = snapshot
		item.item.StartedAt = startedAt
		item.phaseID = workflow.Phases[0].ID
	}
	if targetPhase == "" {
		targetPhase = item.phaseID
	}
	if _, ok := findPhase(item.workflow, targetPhase); !ok {
		return fmt.Errorf("resume item %q: phase %q is not in the frozen workflow", itemID, targetPhase)
	}
	if err := e.transition(item, StateRunning, ""); err != nil {
		return err
	}
	item.phaseID = targetPhase
	item.attempt = 0
	e.items[itemID] = item
	return e.enterPhase(item)
}

func (e *Engine) loadParked(itemID string) (*runtimeItem, error) {
	storedItem, err := e.store.GetWorkItem(itemID)
	if err != nil {
		return nil, fmt.Errorf("resume item %q: %w", itemID, err)
	}
	if State(storedItem.State) != StateNeedsHuman {
		return nil, fmt.Errorf("resume item %q: invalid transition %s -> %s", itemID, storedItem.State, StateRunning)
	}
	item := &runtimeItem{item: storedItem}
	if len(storedItem.Snapshot) == 0 {
		return item, nil
	}
	if err := decodeSnapshot(storedItem.Snapshot, &item.workflow); err != nil {
		return nil, fmt.Errorf("resume item %q snapshot: %w", itemID, err)
	}
	phases, err := e.store.ListWorkItemPhases(itemID)
	if err != nil {
		return nil, fmt.Errorf("resume item %q phases: %w", itemID, err)
	}
	if current, ok := currentPhaseAttempt(phases); ok {
		item.phaseID = current.PhaseID
		item.attempt = current.Attempt
		if len(current.InputEnvelope) > 0 {
			var input PhaseInput
			if err := decodeJSON(current.InputEnvelope, &input); err != nil {
				return nil, fmt.Errorf("resume item %q input envelope: %w", itemID, err)
			}
			item.feedback = input.Feedback
		}
	} else if len(item.workflow.Phases) > 0 {
		item.phaseID = item.workflow.Phases[0].ID
	}
	return item, nil
}

func (e *Engine) emitError(itemID string, err error) {
	e.emitter.Emit("workflow:error", ErrorEvent{
		ItemID: itemID,
		Error:  "workflow operation failed; inspect the item's typed state and local diagnostics",
		detail: err,
	})
}

func (e *Engine) emitEngineState() {
	e.emitter.Emit("workflow:engine-state", EngineState{Paused: e.paused})
}

// emitItemState is the single place a lifecycle transition reaches the wire,
// including the birth event a newly started run emits from the empty state.
func (e *Engine) emitItemState(itemID, projectID string, from, to State, reason Reason) {
	e.emitter.Emit("workflow:item-state", StateEvent{
		ItemID: itemID, ProjectID: projectID, From: from, To: to, Reason: reason,
	})
}
