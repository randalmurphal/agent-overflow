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
	item         store.WorkItem
	workflow     def.Workflow
	phaseID      string
	attempt      int
	runnerActive bool
	waiting      bool
	acquired     []string
	slot         bool
	feedback     *Feedback
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
	config      Config
	now         func() time.Time

	commands chan any
	done     chan struct{}
	lifeMu   sync.Mutex
	started  bool
	closing  bool
	used     bool

	ctx             context.Context
	items           map[string]*runtimeItem
	queued          []*runtimeItem
	queueHead       int
	pendingHuman    []string
	holders         map[resourceKey]int
	activeSlots     int
	queueActive     bool
	startsRemaining int
	lastTimestamp   int64
}

func New(store persistence, runner Runner, emitter Emitter, definitions DefinitionSource, profiles ProfileSource, config Config) (*Engine, error) {
	if store == nil || runner == nil || emitter == nil || definitions == nil || profiles == nil {
		return nil, fmt.Errorf("workflow engine: store, runner, emitter, definition source, and profile source are required")
	}
	if config.GlobalConcurrency < 1 {
		return nil, fmt.Errorf("workflow engine: global concurrency must be at least 1")
	}
	return &Engine{
		store: store, runner: runner, emitter: emitter, definitions: definitions,
		profiles: profiles, config: config, now: time.Now,
		commands: make(chan any, commandBuffer), done: make(chan struct{}),
		items: make(map[string]*runtimeItem), holders: make(map[resourceKey]int),
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
// remain running so the next Start performs the specified interrupted sweep.
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

func (e *Engine) ResolveHumanGate(itemID string, decision HumanDecision, note string) error {
	return e.request(humanGateCommand{itemID: itemID, decision: decision, note: note})
}

func (e *Engine) Reorder(projectID string, orderedIDs []string) error {
	return e.request(reorderCommand{projectID: projectID, orderedIDs: append([]string(nil), orderedIDs...)})
}

// SetQueue toggles draining. maxStarts <= 0 means unbounded; a positive value
// pauses the engine after that many new item starts and is never persisted.
func (e *Engine) SetQueue(active bool, maxStarts int) error {
	return e.request(queueCommand{active: active, maxStarts: maxStarts})
}

// Sync waits until every command enqueued before it has been processed.
func (e *Engine) Sync() error { return e.request(syncCommand{}) }

type response struct{ err error }
type initCommand struct{ reply chan response }
type closeCommand struct{ reply chan response }
type syncCommand struct{ reply chan response }
type enqueueCommand struct {
	item  store.WorkItem
	reply chan response
}
type cancelCommand struct {
	itemID string
	reply  chan response
}
type resumeCommand struct {
	itemID      string
	targetPhase string
	reply       chan response
}
type reorderCommand struct {
	projectID  string
	orderedIDs []string
	reply      chan response
}
type queueCommand struct {
	active    bool
	maxStarts int
	reply     chan response
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
	case resumeCommand:
		command.reply = reply
		e.commands <- command
	case reorderCommand:
		command.reply = reply
		e.commands <- command
	case queueCommand:
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
	return (<-reply).err
}

func (e *Engine) loop() {
	defer close(e.done)
	for command := range e.commands {
		var err error
		switch command := command.(type) {
		case initCommand:
			err = e.rebuild()
			if err == nil {
				err = e.schedule()
			}
			command.reply <- response{err}
		case enqueueCommand:
			err = e.enqueue(command.item)
			command.reply <- response{err}
		case cancelCommand:
			err = e.cancel(command.itemID)
			command.reply <- response{err}
		case resumeCommand:
			err = e.resume(command.itemID, command.targetPhase)
			command.reply <- response{err}
		case reorderCommand:
			err = e.reorder(command.projectID, command.orderedIDs)
			command.reply <- response{err}
		case queueCommand:
			e.queueActive = command.active
			e.startsRemaining = -1
			if command.active && command.maxStarts > 0 {
				e.startsRemaining = command.maxStarts
			}
			e.emitQueue()
			if command.active {
				err = e.schedule()
			}
			command.reply <- response{err}
		case humanGateCommand:
			e.removePendingHuman(command.itemID)
			err = errors.Join(e.resolveHumanGate(command.itemID, command.decision, command.note), e.schedule())
			command.reply <- response{err}
		case completionCommand:
			if err = e.complete(command.key, command.outcome); err != nil {
				e.emitError(command.key.ItemID, err)
			}
			_ = e.schedule() // schedule emits item-scoped asynchronous errors itself.
		case syncCommand:
			command.reply <- response{}
		case closeCommand:
			command.reply <- response{}
			return
		}
	}
}

func (e *Engine) enqueue(item store.WorkItem) error {
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
	e.items[item.ID] = runtime
	e.insertQueued(runtime)
	return e.schedule()
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
	e.emitter.Emit("workflow:queue-state", QueueEvent{
		Active: e.queueActive, GlobalConcurrency: e.config.GlobalConcurrency,
		StartsRemaining: startsRemaining,
	})
}
