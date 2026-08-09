package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

const commandBuffer = 256

type runtimeItem struct {
	item     store.WorkItem
	workflow def.Workflow
	// workspaceNeed is the run's frozen §9 decision, carried on every RunRequest
	// so the runner never re-derives it from the workflow alone (which cannot
	// see what this run's call edges reach).
	workspaceNeed def.WorkspaceNeed
	// rootID is the tree's root item, cached because parent linkage is immutable.
	// Budgets are enforced against the root across the whole tree (§12), so every
	// budget check on a child resolves it; an empty value means "not resolved
	// yet", and a root item resolves to its own id.
	rootID            string
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
	// fan is the live fan-out state of the current attempt, or nil for a
	// single-shape phase. It never outlives its attempt: teardown clears it.
	fan *fanOutRun
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

	ctx   context.Context
	items map[string]*runtimeItem
	// waiting is one FIFO of held starts — phase attempts and fan-out units
	// alike — so freed capacity always goes to the longest-waiting work.
	waiting        []waiter
	waitingKeys    map[waitKey]struct{}
	holders        map[resourceKey]int
	paused         bool
	lastTimestamp  int64
	commandStarts  []*runnerStartFuture
	inflightStarts map[*runnerStartFuture]struct{}
	// deferred is work an FSM transition queued for the command loop to run once
	// the current command settles — today, a finished child run re-entering the
	// parent phase that called it. It is drained on the owner goroutine before
	// the next command is read, so a child completion is serialized like every
	// other transition without a self-send that could deadlock the buffer.
	deferred []deferredWork
}

// deferredWork is one queued follow-up plus the run it belongs to, so a failure
// is reported against the item a human would look at.
type deferredWork struct {
	itemID string
	run    func() error
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
		waitingKeys:    make(map[waitKey]struct{}),
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

// Resume moves a parked item back to running. Naming targetPhase is the
// explicit start-over: that phase is entered fresh, from outside every cycle
// through it, and may be the parked phase itself. An empty target continues the
// parked attempt where one can be continued (ContinuableReason) and otherwise
// re-runs the phase that parked, which is what a question or stuck intervention
// wants.
//
// refreshDefinition re-reads the workflow from disk for this entry instead of
// rendering the definition the run froze at start. It is available only where a
// phase is entered fresh; see refresh.go for why.
func (e *Engine) Resume(itemID, targetPhase string, refreshDefinition bool) error {
	return e.request(resumeCommand{itemID: itemID, targetPhase: targetPhase, refreshDefinition: refreshDefinition})
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

func (e *Engine) cancel(itemID string) error {
	item, tracked := e.items[itemID]
	if !tracked {
		// Not resident means parked: the scheduler evicts an item the moment it
		// leaves `running`, and a run resting at a gate nobody will ever approve
		// has to be stoppable without being resumed into work nobody wants first.
		return errors.Join(e.cancelParked(itemID), e.startWaiting())
	}
	if State(item.item.State) != StateRunning {
		return fmt.Errorf("cancel item %q: invalid transition %s -> %s", itemID, item.item.State, StateCancelled)
	}
	err := e.teardown(item, teardownRequest{phaseStatus: "cancelled", nextState: StateCancelled, reason: ReasonInterrupted})
	return errors.Join(err, e.startWaiting())
}

// cancelParked cancels a run resting `needs-human`, under any park reason. A
// parked run holds no runner, no resources, and no held start — parking
// released all three — so this is persistence and tree bookkeeping rather than
// a teardown: its live descendant subtree comes down first through the same
// path a running run's teardown uses, and the transition is the whole teardown
// for the run itself.
//
// The parked attempt row is deliberately left as it is. It already records how
// the run stopped — the gate it rests on, the envelope that parked it — and
// rewriting it as cancelled would erase the only account of why a human was
// ever asked. A descendant cancelled here reaches the parent that called it
// through the ordinary terminal-transition path, so a live root waiting on it
// settles instead of waiting forever.
//
// A run parked awaiting disposition is refused: its work is done, and the
// disposition verbs are what settle it. Cancelling it would rewrite a
// successful run as stopped.
func (e *Engine) cancelParked(itemID string) error {
	item, err := e.loadParked(itemID)
	if err != nil {
		return err
	}
	if Reason(item.item.Reason) == ReasonDisposition {
		return fmt.Errorf(
			"cancel item %q: this run is done and awaiting disposition; settle it with WorkflowMergeItem, WorkflowCreateItemPR, or WorkflowDiscardItem",
			itemID,
		)
	}
	var errs []error
	if err := e.cancelCallChildren(item); err != nil {
		errs = append(errs, err)
	}
	// The run comes down even if a descendant would not: a human's cancel that a
	// store failure could veto would leave a run nothing can stop.
	return errors.Join(append(errs, e.transition(item, StateCancelled, ReasonInterrupted))...)
}

func (e *Engine) resume(itemID, targetPhase string, refreshDefinition bool) error {
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
		// A decision the gate declared belongs to ResolveHumanGate, so re-entering
		// the phase that is waiting on one is refused. Naming a DIFFERENT phase is
		// not that decision: it is the human abandoning the gate to redo earlier
		// work, which is the one escape from a gate whose reject budget is spent —
		// and it enters that phase from outside every cycle through it, refilling
		// the loop bounds that got there (`freshLoopEntry`).
		if humanGate && (targetPhase == "" || targetPhase == item.phaseID) {
			return fmt.Errorf(
				"resume item %q: human gate decisions require ResolveHumanGate; naming an earlier phase instead abandons the gate and re-enters that phase",
				itemID,
			)
		}
	}
	// Bare resume continues a parked attempt that still holds work worth keeping,
	// and re-enters the phase only where there is nothing to continue. The
	// dispatch lives here rather than in the caller so no entry point can reach a
	// fresh attempt for a park whose finished units — whole called runs among them
	// — it would silently redo. Naming a phase always skips it: that IS the
	// request to start over, and it may name the parked phase itself.
	if targetPhase == "" && ContinuableReason(Reason(item.item.Reason)) {
		// A run that froze no definition has no attempt to continue and no phase to
		// name: its entry resolves from disk whatever the caller asked for, so the
		// refusal below would be about work that does not exist.
		if refreshDefinition && item.phaseID != "" {
			// The attempt this continues was launched under the frozen definition —
			// its units, and the runs those units called, are mid-flight work. Handing
			// the continuation a different definition would render one half of an
			// attempt from each. Naming a phase is the way to say "discard it".
			return fmt.Errorf(
				"resume item %q: the run is parked %q, so a bare resume continues the attempt its work was launched under; re-reading the definition needs a fresh entry, so name the phase to enter (--phase %s) if discarding that attempt is intended",
				itemID, item.item.Reason, item.phaseID,
			)
		}
		return e.resumeItem(itemID)
	}
	return e.enterPhaseFresh(item, targetPhase, refreshDefinition)
}

// enterPhaseFresh re-enters a parked run at targetPhase with a new attempt: the
// phase expands from its inputs again, which for a fan-out means a new wave and
// new children for its call units. It is the "start over" half of resume, and
// the only half a named phase can take.
//
// A run parked before its workflow was ever frozen (a setup failure with no
// snapshot) resolves its definition here — it is the one resume that has to find
// the workflow before it can name a phase at all, and it re-reads from disk
// whether or not it was asked to. `refreshDefinition` is that same re-read
// asked for deliberately, on a run that already has a frozen definition.
func (e *Engine) enterPhaseFresh(item *runtimeItem, targetPhase string, refreshDefinition bool) error {
	itemID := item.item.ID
	switch {
	case len(item.workflow.Phases) == 0:
		snapshot, encoded, err := e.resolveDefinition(item, "", "resume setup-failed item")
		if err != nil {
			return err
		}
		if err := e.freezeSnapshot(item, snapshot, encoded, e.timestamp()); err != nil {
			return err
		}
		item.phaseID = item.workflow.Phases[0].ID
	case refreshDefinition:
		entry := targetPhase
		if entry == "" {
			entry = item.phaseID
		}
		snapshot, encoded, err := e.resolveDefinition(item, entry, resumeAction)
		if err != nil {
			return err
		}
		// The run start is NOT re-stamped: a wall-clock budget is measured against
		// it, and re-reading a definition is not the run beginning again.
		if err := e.freezeSnapshot(item, snapshot, encoded, item.item.StartedAt); err != nil {
			return err
		}
		e.noteDefinitionRefresh(item, entry)
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

// loadParked rebuilds a parked run from SQLite. Every human action on a
// non-resident item goes through it — resume, answer, takeover, gate
// resolution, unit recovery — so its failures are labelled for the item rather
// than for any one of those verbs.
func (e *Engine) loadParked(itemID string) (*runtimeItem, error) {
	storedItem, err := e.store.GetWorkItem(itemID)
	if err != nil {
		return nil, fmt.Errorf("load parked item %q: %w", itemID, err)
	}
	if State(storedItem.State) != StateNeedsHuman {
		return nil, fmt.Errorf("load parked item %q: state is %s, want %s", itemID, storedItem.State, StateNeedsHuman)
	}
	item := &runtimeItem{item: storedItem}
	if len(storedItem.Snapshot) == 0 {
		return item, nil
	}
	snapshot, err := decodeSnapshot(storedItem.Snapshot)
	if err != nil {
		return nil, fmt.Errorf("load parked item %q snapshot: %w", itemID, err)
	}
	item.adoptSnapshot(snapshot)
	phases, err := e.store.ListWorkItemPhases(itemID)
	if err != nil {
		return nil, fmt.Errorf("load parked item %q phases: %w", itemID, err)
	}
	if current, ok := currentPhaseAttempt(phases); ok {
		item.phaseID = current.PhaseID
		item.attempt = current.Attempt
		if len(current.InputEnvelope) > 0 {
			var input PhaseInput
			if err := decodeJSON(current.InputEnvelope, &input); err != nil {
				return nil, fmt.Errorf("load parked item %q input envelope: %w", itemID, err)
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
