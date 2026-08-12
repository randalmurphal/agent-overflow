package engine

import (
	"errors"
	"fmt"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

// resumeAction labels resume's failures in the messages continueParkedAttempt
// and continueFanOutJoin raise, so a refusal names the verb the human used.
const resumeAction = "resume item"

// Per-run pause and its resume (spec §7 / §12, decision D23).
//
// Pause is a *root* action over a whole run tree, and it joins the one teardown
// contract: interrupt the in-flight turn(s) → release locks → write the partial
// envelope → park `needs-human(paused)`. It differs from cancel in exactly one
// respect — the attempt is preserved rather than abandoned, so a call phase
// keeps the child it is waiting on and resume re-enters the same phase on the
// same provider session.
//
// Resume is the inverse and covers `paused` and `interrupted` identically: both
// stopped an attempt without the phase producing a result, so continuing is a
// next turn on the session that parked, not a fresh attempt. The distinct
// reasons exist for the human reading the run list, not for the recovery.
//
// A `unit-failed` park enters here too (ContinuableReason). It stopped an
// attempt without a phase result for a different cause — a unit of the wave, its
// join included, ended without one — and the answer is the same: repair what is
// blocking and let the attempt finish, rather than re-expanding a wave whose
// finished units may each be an entire called run. Starting over is what
// `Resume(itemID, phaseID)` is for, and naming the parked phase itself is how a
// human asks for exactly that.
//
// So does a `retries-exhausted` park. Its turn did not stop short of a result —
// it DIED, on a provider API failure the runner's transient layer retried until
// it ran out — but the session it died in is alive, and continuing on it is the
// same move the retry layer was making one backoff earlier. The one thing a
// continuation cannot do is refill a loop bound, which is why every surface that
// names this resume names `--phase <id>` beside it.

// PauseItem parks a whole run tree `needs-human(paused)`. Members already
// parked for another reason keep the reason they parked under — a pause never
// rewrites why a run needs a human.
func (e *Engine) PauseItem(itemID string) error {
	return e.request(pauseItemCommand{itemID: itemID})
}

// PauseAllActive pauses every active root run. It is the graceful-quit path:
// the app calls it before provider sessions die so a restart shows resumable
// `needs-human(paused)` runs instead of crash-parked interrupted ones.
func (e *Engine) PauseAllActive() error {
	return e.request(pauseAllActiveCommand{})
}

// ResumeItem continues a parked run where it stopped instead of re-entering its
// phase: a stopped attempt (`paused`, `interrupted`, `checkpoint`) resumes on
// the provider sessions it parked on and carries its whole tree with it, a
// `unit-failed` fan-out reopens what is blocking it while every finished unit
// keeps its result, and a `retries-exhausted` phase takes its next turn on the
// session its last one died in. Descendants parked for any other reason stay
// parked and the root returns to waiting on them.
func (e *Engine) ResumeItem(itemID string) error {
	return e.request(resumeItemCommand{itemID: itemID})
}

func (e *Engine) pauseItem(itemID string) error {
	stored, err := e.store.GetWorkItem(itemID)
	if err != nil {
		return fmt.Errorf("pause item %q: %w", itemID, err)
	}
	if stored.ParentItemID != "" {
		return fmt.Errorf(
			"pause item %q: this run was called by %s; pause the run that called it",
			itemID, stored.ParentItemID,
		)
	}
	if err := e.pauseSubtree(stored, 0); err != nil {
		return err
	}
	// A paused member released its locks and its held starts; hand the freed
	// capacity to the longest-waiting work rather than leaving it idle.
	return e.startWaiting()
}

func (e *Engine) pauseAllActive() error {
	active, err := e.store.ListWorkItems(store.WorkItemListFilter{States: []string{string(StateRunning)}})
	if err != nil {
		return fmt.Errorf("pause all active runs: %w", err)
	}
	var errs []error
	for _, item := range active {
		if item.ParentItemID != "" {
			// A descendant comes down with its root; pausing it separately would
			// walk the same subtree twice.
			continue
		}
		if err := e.pauseSubtree(item, 0); err != nil {
			errs = append(errs, err)
		}
	}
	errs = append(errs, e.startWaiting())
	return errors.Join(errs...)
}

// pauseSubtree pauses a run and everything below it, deepest first. Depth order
// is load-bearing: a call parent's teardown retains its children, so those
// children must already be parked when it runs, or the tree would rest with a
// paused parent above a still-running child.
func (e *Engine) pauseSubtree(item store.WorkItem, depth int) error {
	if depth > MaxCallDepth {
		return fmt.Errorf("pause item %q: tree is deeper than %d", item.ID, MaxCallDepth)
	}
	children, err := e.store.ListWorkItemChildren(item.ID)
	if err != nil {
		return fmt.Errorf("pause item %q: list children: %w", item.ID, err)
	}
	var errs []error
	for _, child := range children {
		if State(child.State) != StateRunning {
			continue
		}
		errs = append(errs, e.pauseSubtree(child, depth+1))
	}
	errs = append(errs, e.pauseMember(item))
	return errors.Join(errs...)
}

// pauseMember parks one live run. A member that is not running holds nothing
// and is left exactly as it is: a parked member keeps its own reason, and a
// terminal one is already finished.
func (e *Engine) pauseMember(item store.WorkItem) error {
	if State(item.State) != StateRunning {
		return nil
	}
	resident, tracked := e.items[item.ID]
	if !tracked {
		// Every persisted `running` run is resident: startup rebuild either
		// adopts it or parks it, and nothing creates one off the command loop.
		// A row that says otherwise is corruption, and skipping it silently
		// would leave a run nothing can ever stop.
		return fmt.Errorf("pause item %q: run is persisted running but the scheduler does not hold it", item.ID)
	}
	return e.teardown(resident, teardownRequest{
		phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonPaused,
		retainCallChildren: true,
	})
}

func (e *Engine) resumeItem(itemID string) error {
	item, err := e.loadParked(itemID)
	if err != nil {
		return err
	}
	if !ContinuableReason(Reason(item.item.Reason)) {
		return fmt.Errorf(
			"resume item %q: item is parked %q; continuing a parked run applies to %s",
			itemID, item.item.Reason, continuableReasonList(),
		)
	}
	if _, tracked := e.items[itemID]; tracked {
		return fmt.Errorf("resume item %q: the run is still active", itemID)
	}
	if len(item.workflow.Phases) == 0 || item.phaseID == "" || item.attempt < 1 {
		// Nothing was ever attempted (a run interrupted before its first phase
		// row landed, or a setup failure with no frozen snapshot). There is no
		// session to continue, so this is an ordinary fresh entry. It goes
		// straight to the entry itself rather than back through resume, which
		// would dispatch right back to here.
		return e.enterPhaseFresh(item, "", false)
	}
	phase, ok := findPhase(item.workflow, item.phaseID)
	if !ok {
		return fmt.Errorf("resume item %q: phase %q is not in the frozen workflow", itemID, item.phaseID)
	}
	if phase.IsCall() {
		return e.resumeCallPhase(item)
	}
	phases, err := e.store.ListWorkItemPhases(itemID)
	if err != nil {
		return fmt.Errorf("resume item %q phases: %w", itemID, err)
	}
	current, found := phaseAttempt(phases, item.phaseID, item.attempt)
	if !found {
		return fmt.Errorf("resume item %q: attempt %s/%d has no row", itemID, item.phaseID, item.attempt)
	}
	threadID, err := e.resumableThread(item, current.ThreadID)
	if err != nil {
		return err
	}
	sessionLost := threadID == "" && current.ThreadID != ""
	feedback := &Feedback{Note: resumeNote(Reason(item.item.Reason), sessionLost)}
	if phase.EffectiveShape() == def.ShapeFanOut {
		e.noteResume(item, "continuing the parked attempt by repairing its fan-out")
		return e.resumeFanOutAttempt(item, threadID, feedback)
	}
	if threadID == "" {
		// No session to continue: a tool phase never had one, and an agent
		// phase whose thread was deleted cannot have one. Re-enter the phase
		// from its inputs with the loss recorded in the feedback the next
		// attempt carries — and in the record, because this is a fresh attempt
		// reached through the CONTINUATION path, which is precisely the case a
		// note taken from the request would have described as a continuation.
		e.noteResume(item, "fresh entry into the parked phase: "+missingSessionReason(sessionLost))
		if err := e.transition(item, StateRunning, ""); err != nil {
			return err
		}
		item.feedback = feedback
		item.attempt = 0
		e.items[itemID] = item
		return errors.Join(e.startWaiting(), e.enterPhase(item, entryFresh))
	}
	e.noteResume(item, "continuing the parked attempt on its own session")
	return e.continueParkedAttempt(item, threadID, feedback, false, resumeAction)
}

// missingSessionReason names why there is no session to continue. The two cases
// are not the same event: one is a phase that never had a session (every tool
// phase, and an agent phase that parked before its runner reported), the other
// is a session the app no longer has — which is a deletion a reader will want to
// correlate with something.
func missingSessionReason(sessionLost bool) string {
	if sessionLost {
		return "the attempt's provider session no longer exists"
	}
	return "the parked attempt held no provider session to continue"
}

// resumeFanOutAttempt repairs a fan-out attempt that stopped short of its
// result and returns it to running: the pause or crash that cut it mid-flight,
// and the unit failure that blocked it. Units resting failed are exactly what
// RetryUnit recovers, so resume applies the same repair to all of them at once,
// and a join that failed is continued rather than re-launched from scratch.
// Every unit that finished keeps its result, including a call unit's child run.
//
// A unit under human steering is not repaired: the human is driving it, and
// deciding what happens to it is their call, not the resume's.
func (e *Engine) resumeFanOutAttempt(item *runtimeItem, threadID string, feedback *Feedback) error {
	itemID := item.item.ID
	if err := e.restoreFanOut(item); err != nil {
		return fmt.Errorf("resume item %q: %w", itemID, err)
	}
	for _, unit := range item.fan.units {
		if unit.status == store.WorkItemUnitTakenOver {
			return fmt.Errorf(
				"resume item %q: unit %q is under human steering; retry or drop it before resuming",
				itemID, unit.id,
			)
		}
	}
	for _, unit := range item.fan.units {
		if unit.status != store.WorkItemUnitFailed {
			continue
		}
		if err := e.reopenUnit(item, unit, feedback); err != nil {
			return fmt.Errorf("resume item %q: reopen unit %q: %w", itemID, unit.id, err)
		}
	}
	if item.fan.join.status == store.WorkItemUnitPending {
		// The join never ran, so the units are the whole attempt: relaunch the
		// repaired ones and let the join follow them as it normally would.
		return e.resumeRepairedFanOut(item)
	}
	// The join already ran; its envelope is the phase's, so continuing the
	// phase means continuing the join — on the session it parked on.
	return e.continueFanOutJoin(item, threadID, feedback, false, resumeAction)
}

// resumableThread resolves the provider session a parked attempt would
// continue on, reporting empty when there is none to continue. A thread id that
// no longer resolves is cleared here rather than handed to the runner: the
// runner would fail startup and the run would park `agent-error`, which reads
// as an agent problem rather than as the deleted session it is.
func (e *Engine) resumableThread(item *runtimeItem, threadID string) (string, error) {
	if threadID == "" {
		return "", nil
	}
	exists, err := e.store.ThreadExists(threadID)
	if err != nil {
		return "", fmt.Errorf("resume item %q: resolve parked thread %q: %w", item.item.ID, threadID, err)
	}
	if exists {
		return threadID, nil
	}
	return "", nil
}

// resumeNote is the feedback a resumed attempt carries. A session that could
// not be continued is stated in the note rather than logged and forgotten: it
// is the only place the next turn learns that its predecessor's context is
// gone, and the only place a human reading the attempt input sees it.
//
// `checkpoint` is listed even though a soft stop can only park a call phase,
// which resumes through resumeCallPhase and never reaches here: a note that
// said "after a pause" for a reason that is not one would be a small lie
// waiting for the day a second boundary kind exists.
//
// `retries-exhausted` is worded for both causes it covers — a transient
// provider failure the runner stopped retrying, and a loop bound the gate spent
// — because the reason does not distinguish them and a note that named the
// wrong one would tell the next turn something untrue about why it exists.
func resumeNote(reason Reason, sessionLost bool) string {
	note := "resumed after a pause"
	switch reason {
	case ReasonInterrupted:
		note = "resumed after the run was interrupted"
	case ReasonCheckpoint:
		note = "resumed after the run stopped at the requested checkpoint"
	case ReasonUnitFailed:
		note = "resumed after a unit of the fan-out failed"
	case ReasonRetriesExhausted:
		note = "resumed after the phase ran out of retries"
	}
	if sessionLost {
		return note + "; the previous attempt's provider session no longer exists, so its context is gone — redo the phase from its inputs"
	}
	return note + "; continue from where the previous turn stopped"
}

// resumeCallPhase returns a paused call phase to waiting and resumes the child
// it was waiting on. The attempt row is reopened rather than replaced: a fresh
// attempt would start a second child run and orphan the work the first one did.
func (e *Engine) resumeCallPhase(item *runtimeItem) error {
	itemID := item.item.ID
	child, found, err := e.callChildOf(itemID, item.phaseID, item.attempt)
	if err != nil {
		return err
	}
	if !found {
		// The pause landed between the attempt row and the child's creation.
		// Re-entering the phase invokes the call again, which is exactly what
		// crash recovery does with the same gap.
		return e.enterPhaseFresh(item, "", false)
	}
	e.noteResume(item, "continuing the parked attempt by re-linking the child run it was waiting on")
	if err := e.store.ReopenWorkItemPhase(itemID, item.phaseID, item.attempt); err != nil {
		return fmt.Errorf("resume item %q: reopen call attempt %s/%d: %w", itemID, item.phaseID, item.attempt, err)
	}
	if err := e.transition(item, StateRunning, ""); err != nil {
		return err
	}
	e.items[itemID] = item
	// No OccurredAt, for the reason resumeRepairedFanOut states: a reopened
	// attempt keeps its original `started_at`, so nothing persisted this moment.
	e.emitPhaseState(PhaseEvent{
		ItemID: itemID, PhaseID: item.phaseID, Attempt: item.attempt, Status: "running",
	})
	switch State(child.State) {
	case StateNeedsHuman:
		if !ResumableReason(Reason(child.Reason)) {
			// The child needs a human for a reason of its own. The parent is
			// back to waiting on it, which is the correct resting shape: the
			// child's resolution re-enters the parent exactly as it would have.
			return nil
		}
		return e.resumeItem(child.ID)
	case StateRunning:
		return nil // Already live; the parent is simply waiting on it again.
	default:
		// The child finished while the parent was parked, so nothing will
		// re-enter the parent on its own. Settle it now.
		return e.settleCallChild(child)
	}
}
