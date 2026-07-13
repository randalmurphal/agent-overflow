package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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
	slot              bool
	feedback          *Feedback
	priorThreadID     string
	takeoverFinalize  bool
	reenqueued        bool
}

type resourceKey struct {
	projectID string
	name      string
}

type projectQueueState struct {
	paused      bool
	concurrency int
}

// Engine owns all mutable scheduler and FSM state on its command goroutine.
type Engine struct {
	store       persistence
	runner      Runner
	emitter     Emitter
	definitions DefinitionSource
	profiles    ProfileSource
	spend       SpendSource
	config      Config
	now         func() time.Time

	commands chan any
	done     chan struct{}
	lifeMu   sync.Mutex
	started  bool
	closing  bool
	used     bool

	ctx              context.Context
	items            map[string]*runtimeItem
	queued           []*runtimeItem
	waiting          []*runtimeItem
	waitingByID      map[string]struct{}
	pendingHuman     []string
	holders          map[resourceKey]int
	activeSlots      int
	projectQueues    map[string]projectQueueState
	runningByProject map[string]int
	queueActive      bool
	startsRemaining  int
	lastTimestamp    int64
	commandStarts    []*runnerStartFuture
	inflightStarts   map[*runnerStartFuture]struct{}
}

func New(store persistence, runner Runner, emitter Emitter, definitions DefinitionSource, profiles ProfileSource, spend SpendSource, config Config) (*Engine, error) {
	if store == nil || runner == nil || emitter == nil || definitions == nil || profiles == nil || spend == nil {
		return nil, fmt.Errorf("workflow engine: store, runner, emitter, definition source, profile source, and spend source are required")
	}
	if config.GlobalConcurrency < 1 || config.GlobalConcurrency > MaxGlobalConcurrency {
		return nil, fmt.Errorf("workflow engine: global concurrency must be between 1 and %d", MaxGlobalConcurrency)
	}
	projectQueues := make(map[string]projectQueueState, len(config.ProjectQueues))
	for _, project := range config.ProjectQueues {
		if project.ProjectID == "" {
			return nil, fmt.Errorf("workflow engine: project queue id is required")
		}
		if project.Concurrency < 0 || project.Concurrency > MaxProjectConcurrency {
			return nil, fmt.Errorf("workflow engine: project %q concurrency must be between 0 and %d", project.ProjectID, MaxProjectConcurrency)
		}
		if _, exists := projectQueues[project.ProjectID]; exists {
			return nil, fmt.Errorf("workflow engine: duplicate project queue %q", project.ProjectID)
		}
		projectQueues[project.ProjectID] = projectQueueState{paused: project.Paused, concurrency: project.Concurrency}
	}
	return &Engine{
		store: store, runner: runner, emitter: emitter, definitions: definitions,
		profiles: profiles, spend: spend, config: config, now: time.Now,
		commands: make(chan any, commandBuffer), done: make(chan struct{}),
		items: make(map[string]*runtimeItem), holders: make(map[resourceKey]int),
		projectQueues: projectQueues, runningByProject: make(map[string]int),
		waitingByID:     make(map[string]struct{}),
		inflightStarts:  make(map[*runnerStartFuture]struct{}),
		startsRemaining: -1,
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

func (e *Engine) Enqueue(item store.WorkItem) error {
	return e.request(enqueueCommand{item: item, waitForStarts: true})
}

// EnqueueDetachedStarts validates and persists synchronously, but does not
// attach runner provisioning futures to the caller's response. Async startup
// failures still park the item and emit workflow:error.
func (e *Engine) EnqueueDetachedStarts(item store.WorkItem) error {
	return e.request(enqueueCommand{item: item})
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

func (e *Engine) Reorder(projectID string, orderedIDs []string) error {
	return e.request(reorderCommand{projectID: projectID, orderedIDs: append([]string(nil), orderedIDs...)})
}

// SetQueue toggles draining and updates live global concurrency. maxStarts <= 0
// means unbounded; a positive value pauses after that many new item starts and
// is never persisted.
func (e *Engine) SetQueue(active bool, maxStarts, concurrency int) error {
	return e.request(queueCommand{active: active, maxStarts: maxStarts, setMaxStarts: true, concurrency: concurrency, waitForStarts: true})
}

// SetQueueDetachedStarts applies queue state synchronously while leaving any
// starts it triggers to report through workflow events.
func (e *Engine) SetQueueDetachedStarts(active bool, maxStarts, concurrency int) error {
	return e.request(queueCommand{active: active, maxStarts: maxStarts, setMaxStarts: true, concurrency: concurrency})
}

// UpdateQueueSettings changes persisted queue settings without altering the
// transient process-N budget owned by the most recent explicit SetQueue call.
// A nil active value is a concurrency-only update.
func (e *Engine) UpdateQueueSettings(active *bool, concurrency int) error {
	command := queueCommand{concurrency: concurrency, waitForStarts: true}
	if active != nil {
		command.active = *active
		command.setActive = true
	}
	return e.request(command)
}

// UpdateQueueSettingsDetachedStarts is the settings-bound counterpart to
// SetQueueDetachedStarts and preserves the current process-N budget.
func (e *Engine) UpdateQueueSettingsDetachedStarts(active *bool, concurrency int) error {
	command := queueCommand{concurrency: concurrency}
	if active != nil {
		command.active = *active
		command.setActive = true
	}
	return e.request(command)
}

// UpdateProjectQueueSettings changes one project's persisted queue controls.
// A nil paused value is a concurrency-only update; concurrency 0 inherits the
// live global cap.
func (e *Engine) UpdateProjectQueueSettings(projectID string, paused *bool, concurrency int) error {
	return e.request(newProjectQueueCommand(projectID, paused, concurrency, true))
}

// UpdateProjectQueueSettingsDetachedStarts applies project settings without
// attaching any newly triggered runner starts to the caller's response.
func (e *Engine) UpdateProjectQueueSettingsDetachedStarts(projectID string, paused *bool, concurrency int) error {
	return e.request(newProjectQueueCommand(projectID, paused, concurrency, false))
}

func newProjectQueueCommand(projectID string, paused *bool, concurrency int, waitForStarts bool) projectQueueCommand {
	command := projectQueueCommand{
		projectID: projectID, concurrency: concurrency, waitForStarts: waitForStarts,
	}
	if paused != nil {
		command.paused = *paused
		command.setPaused = true
	}
	return command
}

// Sync waits until every command enqueued before it has been processed.
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
type enqueueCommand struct {
	item          store.WorkItem
	waitForStarts bool
	reply         chan response
}
type cancelCommand struct {
	itemID string
	reply  chan response
}
type removeQueuedCommand struct {
	itemID string
	reply  chan response
}
type parkDispositionCommand struct {
	itemID string
	reply  chan response
}
type reenqueueFailedCommand struct {
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
type reorderCommand struct {
	projectID  string
	orderedIDs []string
	reply      chan response
}
type queueCommand struct {
	active        bool
	setActive     bool
	maxStarts     int
	setMaxStarts  bool
	concurrency   int
	waitForStarts bool
	reply         chan response
}
type projectQueueCommand struct {
	projectID     string
	paused        bool
	setPaused     bool
	concurrency   int
	waitForStarts bool
	reply         chan response
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
	case enqueueCommand:
		command.reply = reply
		e.commands <- command
	case cancelCommand:
		command.reply = reply
		e.commands <- command
	case removeQueuedCommand:
		command.reply = reply
		e.commands <- command
	case parkDispositionCommand:
		command.reply = reply
		e.commands <- command
	case reenqueueFailedCommand:
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
	case reorderCommand:
		command.reply = reply
		e.commands <- command
	case queueCommand:
		command.reply = reply
		e.commands <- command
	case projectQueueCommand:
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
				err = e.schedule()
			}
			command.reply <- e.commandResponse(err)
		case enqueueCommand:
			err = e.enqueue(command.item, command.waitForStarts)
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
		case removeQueuedCommand:
			err = e.removeQueued(command.itemID)
			e.commandStarts = nil
			command.reply <- response{err: err}
		case parkDispositionCommand:
			err = e.parkDisposition(command.itemID)
			e.commandStarts = nil
			command.reply <- response{err: err}
		case reenqueueFailedCommand:
			err = e.reenqueueFailed(command.itemID)
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
			e.removePendingHuman(command.itemID)
			err = errors.Join(e.takeOver(command.itemID), e.schedule())
			e.commandStarts = nil
			command.reply <- response{err: err}
		case completeTakeoverCommand:
			err = e.completeTakeover(command.itemID)
			command.reply <- e.itemCommandResponse(command.itemID, err)
		case reorderCommand:
			err = e.reorder(command.projectID, command.orderedIDs)
			command.reply <- e.commandResponse(err)
		case queueCommand:
			if command.concurrency < 1 || command.concurrency > MaxGlobalConcurrency {
				err = fmt.Errorf("set workflow queue: concurrency must be between 1 and %d", MaxGlobalConcurrency)
			} else {
				e.config.GlobalConcurrency = command.concurrency
				if command.setActive || command.setMaxStarts {
					e.queueActive = command.active && e.startsRemaining != 0
				}
				if command.setMaxStarts {
					e.startsRemaining = -1
					if command.active && command.maxStarts > 0 {
						e.startsRemaining = command.maxStarts
					}
					e.queueActive = command.active
				}
				e.emitQueue()
				if e.queueActive {
					startErr := e.schedule()
					if command.waitForStarts {
						err = startErr
					}
				}
			}
			if command.waitForStarts {
				command.reply <- e.commandResponse(err)
			} else {
				e.commandStarts = nil
				command.reply <- response{err: err}
			}
		case projectQueueCommand:
			if command.projectID == "" {
				err = fmt.Errorf("set project workflow queue: project id is required")
			} else if command.concurrency < 0 || command.concurrency > MaxProjectConcurrency {
				err = fmt.Errorf("set project workflow queue: concurrency must be between 0 and %d", MaxProjectConcurrency)
			} else {
				state := e.projectQueues[command.projectID]
				state.concurrency = command.concurrency
				if command.setPaused {
					state.paused = command.paused
				}
				e.projectQueues[command.projectID] = state
				e.emitQueue()
				if e.queueActive {
					startErr := e.schedule()
					if command.waitForStarts {
						err = startErr
					}
				}
			}
			if command.waitForStarts {
				command.reply <- e.commandResponse(err)
			} else {
				e.commandStarts = nil
				command.reply <- response{err: err}
			}
		case humanGateCommand:
			e.removePendingHuman(command.itemID)
			err = errors.Join(e.resolveHumanGate(command.itemID, command.decision, command.note), e.schedule())
			command.reply <- e.itemCommandResponse(command.itemID, err)
		case runnerStartCommand:
			err = e.finishRunnerStart(command)
			if err != nil {
				e.emitError(command.key.ItemID, err)
			}
			_ = e.schedule() // schedule reports item-scoped failures itself.
			delete(e.inflightStarts, command.future)
			e.commandStarts = nil
			settleRunnerStart(command.future, response{err: err})
		case completionCommand:
			if err = e.complete(command.key, command.outcome); err != nil {
				e.emitError(command.key.ItemID, err)
			}
			_ = e.schedule() // schedule emits item-scoped asynchronous errors itself.
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

func (e *Engine) enqueue(item store.WorkItem, reportStartErrors bool) error {
	if State(item.State) != StateQueued {
		return fmt.Errorf("enqueue item %q: state must be queued, got %q", item.ID, item.State)
	}
	if item.ID == "" || item.ProjectID == "" {
		return fmt.Errorf("enqueue item: id and project id are required")
	}
	if len(item.Seeds) > MaxSeedBytes {
		return fmt.Errorf("enqueue item %q: seeds are %d bytes; maximum is %d", item.ID, len(item.Seeds), MaxSeedBytes)
	}
	if len(item.Seeds) > 0 {
		var seeds map[string]any
		if err := decodeJSON(item.Seeds, &seeds); err != nil || seeds == nil {
			return fmt.Errorf("enqueue item %q: seeds must be one JSON object", item.ID)
		}
	}
	if _, exists := e.items[item.ID]; exists {
		return fmt.Errorf("enqueue item %q: already tracked", item.ID)
	}
	if err := e.store.CreateWorkItem(item); err != nil {
		return fmt.Errorf("enqueue item %q: %w", item.ID, err)
	}
	runtime := &runtimeItem{item: item}
	if _, exists := e.projectQueues[item.ProjectID]; !exists {
		e.projectQueues[item.ProjectID] = projectQueueState{}
	}
	e.items[item.ID] = runtime
	e.insertQueued(runtime)
	startErr := e.schedule()
	if reportStartErrors {
		return startErr
	}
	return nil
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
	return errors.Join(err, e.schedule())
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
	if e.activeSlots >= e.config.GlobalConcurrency {
		return fmt.Errorf("resume item %q: global concurrency is full", itemID)
	}
	if !e.projectHasCapacity(item.item.ProjectID) {
		return fmt.Errorf("resume item %q: project concurrency is full", itemID)
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
	if err := e.enterPhase(item); err != nil {
		return err
	}
	return e.schedule()
}

func (e *Engine) reorder(projectID string, orderedIDs []string) error {
	for _, id := range orderedIDs {
		item, ok := e.items[id]
		if !ok || item.item.ProjectID != projectID || State(item.item.State) != StateQueued {
			return fmt.Errorf("reorder project %q: item %q is not a tracked queued item in that project", projectID, id)
		}
	}
	if err := e.store.ReorderQueuedWorkItems(projectID, orderedIDs); err != nil {
		return err
	}
	for position, id := range orderedIDs {
		e.items[id].item.SortPosition = position
	}
	e.sortQueued()
	return nil
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

func (e *Engine) emitQueue() {
	startsRemaining := e.startsRemaining
	if startsRemaining < 0 {
		startsRemaining = 0
	}
	projectIDs := make([]string, 0, len(e.projectQueues))
	for projectID := range e.projectQueues {
		projectIDs = append(projectIDs, projectID)
	}
	sort.Strings(projectIDs)
	projects := make([]ProjectQueueState, 0, len(projectIDs))
	for _, projectID := range projectIDs {
		state := e.projectQueues[projectID]
		projects = append(projects, ProjectQueueState{
			ProjectID: projectID, Paused: state.paused, Concurrency: state.concurrency,
			RunningCount: e.runningByProject[projectID],
		})
	}
	e.emitter.Emit("workflow:queue-state", QueueEvent{
		Active: e.queueActive, GlobalConcurrency: e.config.GlobalConcurrency,
		RunningCount: e.activeSlots, SlotCapacity: e.config.GlobalConcurrency,
		StartsRemaining: startsRemaining, Projects: projects,
	})
}
