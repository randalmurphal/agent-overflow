package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"agent-overflow/internal/eventchan"
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
	// runnerStart is the identity of the in-flight start, compared by
	// `finishRunnerStart` so a superseded start's late return cannot settle a
	// newer one. See `unitRun.runnerStart` for the shape that made this
	// necessary; the phase copy exists so the two guards cannot drift.
	runnerStart   *runnerStartFuture
	waiting       bool
	acquired      []string
	feedback      *Feedback
	parkedVars    map[string]any
	priorThreadID string
	// entry is the current attempt's prompt semantics. It is stored separately
	// from priorThreadID because warm loop rounds reuse a session but start a new
	// logical task, while answers and resumes continue the task already there.
	entry phaseEntry
	// nextPromptRoute is the loop route a gate decision ARMED for the entry it is
	// about to make; promptRoute is the one the CURRENT attempt rendered.
	// `enterPhase` is the only consumer of either, and it consumes exactly one:
	// a fresh entry takes the arming (and only when the armed route's loop target
	// IS the phase being entered — `consumePromptRoute`), a continuation keeps
	// what the round it continues rendered. That target check is what makes the
	// override belong to one entry of one phase: a coordinate that survives a
	// park into some other phase's entry is inert rather than a body that leaks.
	//
	// Two fields rather than one because the arming is written BEFORE the entry
	// and the rendered value is read AFTER it — a phase held on resource capacity
	// sits between the two — and because a fan-out or a tool phase never reaches
	// the runner at all, so a single field cleared at the runner would leak into
	// the phase after them.
	nextPromptRoute *PromptRoute
	promptRoute     *PromptRoute
	// guidance is the operator guidance the CURRENT attempt was entered with. It
	// outlives the request that carried it because a fan-out's units and its join
	// are launched after the entry and render the same block; teardown clears it
	// with the rest of the attempt's state.
	guidance []GuidanceEntry
	// guidanceUnacked is the part of `guidance` whose pending-slot entries have
	// not been cleared yet. The slot is cleared only once the send door reports a
	// prompt that renders them dispatched (`ackGuidance`), so an attempt that never
	// reached one — held by a pause and then torn down, parked by a failed
	// acquisition, killed by a crash — leaves the entries pending for the next
	// entry to deliver again. Teardown clears it, which is what turns every one
	// of those into a redelivery instead of a loss.
	guidanceUnacked []GuidanceEntry
	// feedbackOwed says the CURRENT attempt persisted a feedback note that no
	// provider session has rendered yet. It is the in-memory half of
	// `work_item_phases.feedback_delivered_at`, kept for the reason
	// `guidanceUnacked` is kept: the durable stamp lands only when the send door
	// reports the prompt reached a live session (`AckFeedbackRendered`), and the
	// ordinary attempt — one carrying no feedback at all — must not pay a store
	// write to settle nothing. Teardown clears it, which is what turns a wedged
	// start, a pause, and a failed acquisition into a redelivery into the
	// phase's next attempt instead of a lost instruction. Like `guidanceUnacked`
	// it is assigned at `enterPhase` and RESTORED by `loadParked` from the row's
	// stamp, because the fan-out repair paths relaunch elements of the parked
	// attempt without a new entry and their sends must still settle the debt.
	feedbackOwed bool
	// feedbackCarriedFrom is the attempt whose still-owed note the NEXT phase
	// entry carries verbatim rather than through the redelivery read — set by
	// `restartPhaseWithoutProviderContext` and consumed by the one `enterPhase`
	// that follows it. It does two things there: it excludes that source from the
	// redelivery window (the entry already holds the note, so collecting it would
	// prepend it to itself), and it is what `enterPhase` settles once the
	// reconstruction's row EXISTS. Settling it any earlier is a window in which a
	// crash destroys the note; settling it after is a window in which the note is
	// redelivered, which is the direction this contract errs in.
	//
	// Zero means "nothing carried" and matches no attempt. `enterPhase` clears it
	// on read and `teardown` clears it with the rest of the attempt's state, so it
	// cannot outlive the entry it was written for.
	feedbackCarriedFrom int
	takeoverFinalize    bool
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
	log         LogSink
	now         func() time.Time
	// startReplyBudget bounds how long one API call's reply waits on the runner
	// starts its command produced. It is a field rather than a bare constant read
	// for the reason `now` is: the behaviour is a timeout, and a test that had to
	// spend the real one to prove it exists would not be run.
	startReplyBudget time.Duration

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
		profiles: profiles, spend: spend, log: config.Log,
		paused: config.Paused, now: time.Now, startReplyBudget: runnerStartReplyBudget,
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
	parkedAs := item.item.Reason
	var errs []error
	if err := e.cancelCallChildren(item); err != nil {
		errs = append(errs, err)
	}
	// The run comes down even if a descendant would not: a human's cancel that a
	// store failure could veto would leave a run nothing can stop.
	transitionErr := e.transition(item, StateCancelled, ReasonInterrupted)
	if transitionErr == nil {
		e.logEvent(LogEvent{
			Event: LogEventCancel, ItemID: itemID, ProjectID: item.item.ProjectID,
			PhaseID: item.phaseID, Attempt: item.attempt,
			State: StateCancelled, Reason: ReasonInterrupted,
			Message: fmt.Sprintf("cancelled while parked %q", parkedAs),
		})
	}
	return errors.Join(append(errs, transitionErr)...)
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
	// Nothing is logged here. This decides which PATH a bare resume takes, not
	// what it ends up doing: the continuation path still re-enters fresh where
	// there is nothing to continue, so the record is emitted by the branch that
	// knows (`noteResume`).
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
//
// Every fresh re-entry lands here — the human's `--phase`, a bare resume of a
// park that cannot be continued, and the gaps the continuation path falls
// through — so this is where that fact is recorded, once, for all of them.
func (e *Engine) enterPhaseFresh(item *runtimeItem, targetPhase string, refreshDefinition bool) error {
	itemID := item.item.ID
	// A deliberate start-over does not inherit the parked attempt's control note
	// (for example, "continue from where the previous turn stopped"). Callers
	// with real fresh-entry feedback enter through enterPhase directly; the only
	// feedback authored here is the definition-refresh note below.
	item.feedback = nil
	e.noteResume(item, freshEntryNote(targetPhase, refreshDefinition))
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
	// Starting a phase over is the one entry that drops a loop route's prompt
	// override outright, whatever arming or rendered coordinate survived the park:
	// this verb is the human saying "run this phase again from its inputs", and
	// the phase's own body is what that means. A CONTINUATION of the parked round
	// keeps the narrower question it was asked, and that is the other half of the
	// rule, decided in `enterPhase` from the entry kind.
	item.nextPromptRoute = nil
	item.promptRoute = nil
	e.items[itemID] = item
	return e.enterPhase(item, entryFresh)
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
			item.parkedVars = input.Vars
			// The parked attempt's own state, restored so a continuation of this
			// round runs what the round was running: the loop route's prompt
			// override (dropped again by enterPhaseFresh, which is the deliberate
			// start-over), and the operator guidance the entry delivered — a
			// fan-out repaired unit by unit renders the same block its siblings
			// did rather than one the operator would have to leave twice.
			//
			// What is restored is the RENDERED coordinate, never the arming: the
			// arming belongs to an entry that has already happened, and restoring
			// it would re-arm the override for whatever phase the run enters next
			// — which is a gate resolve's next phase as readily as a continuation
			// of this one. A park that landed BEFORE the armed entry re-arms from
			// the persisted decision instead (`applyLoopRoute`, reached by every
			// recovery and step-mode path), so nothing needs it here.
			item.promptRoute = input.PromptRoute
			item.guidance = input.Guidance
			// Both delivery debts are restored from their durable halves, because
			// the repair paths (`resumeFanOutAttempt`, `continueFanOutJoin`,
			// `RetryUnit`) relaunch elements of THIS attempt without ever passing
			// through `enterPhase` — the only other assigner of either flag. Without
			// this, a repaired element rendered both blocks and its send's ack
			// settled neither: the row stayed at 0 forever and the phase's next
			// entry redelivered a note a session had demonstrably rendered, under a
			// provenance sentence claiming it never was.
			item.feedbackOwed = current.FeedbackDeliveredAt == 0
			if len(input.Guidance) > 0 {
				pending, err := e.pendingGuidance(itemID)
				if err != nil {
					if _, healed := healedGuidance(err); !healed {
						return nil, fmt.Errorf("load parked item %q pending guidance: %w", itemID, err)
					}
					// The heal emptied the slot: nothing is left for an ack to
					// clear, so the attempt owes it nothing.
					pending = nil
				}
				item.guidanceUnacked = matchingGuidance(pending, input.Guidance)
			}
		}
	} else if len(item.workflow.Phases) > 0 {
		item.phaseID = item.workflow.Phases[0].ID
	}
	return item, nil
}

func (e *Engine) emitError(itemID string, err error) {
	e.emitter.Emit(eventchan.WorkflowError, ErrorEvent{
		ItemID: itemID,
		Error:  "workflow operation failed; inspect the item's typed state and local diagnostics",
		detail: err,
	})
}

func (e *Engine) emitEngineState() {
	e.emitter.Emit(eventchan.WorkflowEngineState, EngineState{Paused: e.paused})
}

// emitItemState is the single place a lifecycle transition reaches the wire,
// including the birth event a newly started run emits from the empty state.
// It carries no phase coordinate: the sites that reach it hold a store row
// rather than a resident run, and a phase read back from the row could name the
// phase the run moved on to rather than the one the transition happened in.
func (e *Engine) emitItemState(itemID, projectID string, from, to State, reason Reason) {
	e.emitter.Emit(eventchan.WorkflowItemState, StateEvent{
		ItemID: itemID, ProjectID: projectID, From: from, To: to, Reason: reason,
	})
}

// emitPhaseState is the single place a phase-attempt or unit status reaches the
// wire. It exists to guarantee `OccurredAt`: a field every call site had to
// remember would be one forgotten emit away from a consumer silently falling
// back to its own clock, so the guarantee lives on the one path instead of in
// eight constructions.
//
// A site that PERSISTED the transition's time passes that same value, so the
// event and the row it announces agree to the millisecond — which is what lets
// a consumer patching a live view keep its patch when the row is refetched.
// The default is only for the transitions no row records a time for (a
// reopened attempt, an expanded-but-unstarted unit); `timestamp()` is strictly
// monotonic, so defaulting where a time WAS written would guarantee the two
// disagree rather than merely risk it. It runs on the command goroutine like
// every other emit, which is what makes `timestamp()` safe here.
func (e *Engine) emitPhaseState(event PhaseEvent) {
	if event.OccurredAt == 0 {
		event.OccurredAt = e.timestamp()
	}
	e.emitter.Emit(eventchan.WorkflowPhaseState, event)
}

// emitItemStateAt is emitItemState for the transitions taken with the run
// resident, where the phase and attempt being left are known exactly. A park
// rests on that attempt, so its cause and its narrative are filed under this
// coordinate and an observer needs it to read either.
func (e *Engine) emitItemStateAt(item *runtimeItem, from, to State, reason Reason) {
	e.emitter.Emit(eventchan.WorkflowItemState, StateEvent{
		ItemID: item.item.ID, ProjectID: item.item.ProjectID, From: from, To: to, Reason: reason,
		PhaseID: item.phaseID, Attempt: item.attempt,
	})
}
