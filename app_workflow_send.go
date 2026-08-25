package main

import (
	"context"
	"encoding/json"
	"log"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/workflow/engine"
)

// The one door every workflow send goes through, and everything that decides
// whether a queued send may still happen: the admission recheck and the drop
// vocabulary.
//
// It is separate from `observe` because it is the other direction: `observe`
// turns provider events into attempt state, and this turns attempt state into
// one decision about one send.

// workflowSendDropReason names why `sendIfActive` did not deliver a queued send.
// All three causes are legitimate and have owners elsewhere; naming them exists
// so no drop is silent — a run that stopped moving used to be indistinguishable
// from one whose send was correctly retired.
type workflowSendDropReason string

const (
	// workflowSendNotDropped means the send passed admission and was handed to
	// the wire. Whether the wire ACCEPTED it is the returned error's answer, not
	// this one's — which is why it is named for what it rules out rather than for
	// a delivery it cannot promise.
	workflowSendNotDropped workflowSendDropReason = ""
	// workflowSendDropUnregistered: the attempt is no longer in `runs` — it was
	// stopped, finished, or torn down while this send was queued.
	workflowSendDropUnregistered workflowSendDropReason = "the attempt is no longer registered"
	// workflowSendDropSessionDeath: a session death is latched, so
	// `sessionDisconnected` owns what happens next.
	workflowSendDropSessionDeath workflowSendDropReason = "the attempt latched a session death"
	// workflowSendDropStaleEpoch: the retry ladder advanced past the state that
	// queued this send, so a newer send owns the next turn.
	workflowSendDropStaleEpoch workflowSendDropReason = "the retry ladder advanced past the state that queued it"
)

// logWorkflowSendDrop is the one rendering of a dropped send. It is called
// from `sendIfActive` itself — never from a caller — so a send that is dropped
// is logged by the same door that dropped it, and no future caller can forget.
func logWorkflowSendDrop(runKey, what string, reason workflowSendDropReason) {
	log.Printf("workflow runner: dropped %s for %s: %s", what, runKey, reason)
}

// sendIfActive is the one door every workflow send goes through. It serializes
// with Stop and rechecks, after taking the per-attempt lock, that the state
// which decided to send is still the state that exists: the attempt is still
// installed, its session is not already known dead, and the ladder has not
// advanced past the rung this send belongs to. A send decided by a superseded
// state is dropped rather than delivered late, and says which of the three it
// was.
//
// The invariant every caller relies on, stated once here rather than at each of
// them: a DROP is fully owned by whatever state caused it — teardown, the
// session-death latch, or the newer ladder rung — and is already logged by this
// function, so a caller has nothing left to do with it. The only thing a caller
// must handle is a non-nil error, which means the send was admitted and the wire
// refused it.
//
// `what` labels the send for the drop log ("the opening prompt", "the retry
// turn"). The logging lives HERE, on every drop-carrying return path at once,
// because a drop each caller had to remember to log is one new caller away
// from a silently vanished send.
//
// `ctx` bounds the dispatch. The OPENING send passes the start's context, so a
// send that wedges is unwound by the start deadline; every other send happens
// after the start returned, on a session already proven live, and its bound is
// the reliability ladder's watchdog instead.
//
// `epoch` is the caller's `attempt.sendEpoch`, read under the runner lock at the
// moment the send was decided. See the field.
func (r *workflowAppRunner) sendIfActive(
	ctx context.Context, runKey, what, message string, schema json.RawMessage, epoch int,
) (drop workflowSendDropReason, err error) {
	defer func() {
		if drop != workflowSendNotDropped {
			logWorkflowSendDrop(runKey, what, drop)
		}
	}()
	r.mu.Lock()
	attempt := r.runs[runKey]
	r.mu.Unlock()
	if attempt == nil {
		return workflowSendDropUnregistered, nil
	}

	attempt.sendMu.Lock()
	drop, err = r.dispatchSendLocked(ctx, runKey, attempt, message, schema, epoch)
	attempt.sendMu.Unlock()
	if drop == workflowSendNotDropped && err == nil {
		// On its own goroutine, deliberately, for two locks this caller may be
		// inside. The ack round-trips the engine's command loop, and that loop is
		// what calls `Runner.Stop` — which waits on the very send lock this door
		// just released, so an inline ack that raced a teardown would queue behind
		// a command that is waiting for this goroutine. And the OPENING send runs
		// inside the start-deadline window: an inline ack would put an unbounded,
		// non-cancellable wait back into the one path the watchdog exists to bound.
		//
		// Nothing is lost by acking a moment later, in any order: the engine
		// settles the ack against the run's CURRENT attempt and only ever clears a
		// flag, so a late or duplicate ack for a torn-down attempt is a no-op
		// rather than a wrong stamp.
		go r.ackFeedbackRendered(attempt.key)
	}
	return drop, err
}

// dispatchSendLocked is `sendIfActive`'s admission and dispatch. The caller
// must hold `attempt.sendMu`, which serializes it with Stop's wait on an
// in-flight send.
func (r *workflowAppRunner) dispatchSendLocked(
	ctx context.Context, runKey string, attempt *workflowAttempt,
	message string, schema json.RawMessage, epoch int,
) (workflowSendDropReason, error) {
	r.mu.Lock()
	// A latched session death makes the attempt as inactive as cancellation does:
	// the session is being reaped and `sessionDisconnected` owns what happens
	// next. A send queued before the death would otherwise land in the dying
	// process and convert a transient ladder into an execution-failure park.
	//
	// The epoch catches the case the latch cannot: the death was latched AND
	// already answered, so the flag is clear again and the attempt is installed
	// and healthy — but it is healthy in a NEW ladder state whose own resend is
	// pending. Delivering the old one then starts a turn inside somebody else's
	// backoff window, where the guard drops its events and the next rung's send
	// lands on a session with a turn already in flight.
	var drop workflowSendDropReason
	switch {
	case r.runs[runKey] != attempt:
		drop = workflowSendDropUnregistered
	case attempt.pendingSessionDeath:
		drop = workflowSendDropSessionDeath
	case attempt.sendEpoch != epoch:
		drop = workflowSendDropStaleEpoch
	}
	r.mu.Unlock()
	if drop != workflowSendNotDropped {
		return drop, nil
	}
	if err := r.host.sendWorkflowMessage(ctx, attempt.threadID, message, schema, func(identity providerDispatchIdentity) {
		r.mu.Lock()
		if r.runs[runKey] == attempt && attempt.sendEpoch == epoch {
			attempt.dispatchIdentity = identity
			attempt.dispatchIdentitySet = true
		}
		r.mu.Unlock()
	}); err != nil {
		return workflowSendNotDropped, err
	}

	r.mu.Lock()
	if r.runs[runKey] == attempt && !attempt.pendingSessionDeath && attempt.sendEpoch == epoch {
		if attempt.provider == string(provider.Claude) {
			// Claude has no per-turn EventTurnStart. A successful send is therefore
			// the start signal when an existing session emits no fresh EventInit (for
			// example, the envelope-feedback turn and a transient sub-attempt).
			if !attempt.turnStarted && attempt.timerMode != workflowTimerBackoff {
				attempt.turnStarted = true
				attempt.pendingTransient = false
				attempt.providerRetryHint = false
				r.armWatchdogLocked(runKey, attempt)
			}
		} else {
			// Every other provider names its own turn, so a send is not a start —
			// it is the beginning of a wait for one, and both halves of that wait
			// belong at this chokepoint. The opening send and the envelope-feedback
			// send were the two doors that had neither: until `turn/started` names
			// the turn, a replayed lifecycle event from an EARLIER turn passes the
			// identity filter (an empty `currentTurnID` makes it inert) and can
			// settle the attempt on a stale envelope.
			//
			// The turn start for THIS send may already have arrived — events and
			// this block serialize on the runner lock, so the ordering is a race —
			// and setting the flag then would eat the completion of the very turn
			// it was meant to protect. `turnStarted` is the record of that, so it
			// is the condition.
			if !attempt.turnStarted {
				attempt.awaitingTurnStart = true
			}
			// Only the unarmed state is touched, so the retry path's own watchdog
			// and a live turn's are left exactly as they were.
			if attempt.timerMode == workflowTimerNone {
				r.armWatchdogLocked(runKey, attempt)
			}
		}
	}
	r.mu.Unlock()
	return workflowSendNotDropped, nil
}

// ackFeedbackRendered tells the engine that this attempt's prompt reached a live
// provider session, which is what settles any feedback the attempt was carrying.
//
// It lives on the SEND door because that is the earliest event that proves a
// model saw the note. A start returning nil does not: a start can succeed having
// dropped its opening send at admission, and a stamp written there records a
// note no turn ever rendered — the loss the create-then-settle ordering exists
// to prevent, one step further along.
//
// Ack failures never fail the send. The engine's own contract is that an
// unstamped note stays owed and is redelivered on the phase's next entry, so the
// worst case here is a repeat, and a send already on the wire must not be
// reported as failed because a bookkeeping write did not land.
func (r *workflowAppRunner) ackFeedbackRendered(key engine.RunKey) {
	workflowEngine, err := r.host.requireWorkflowEngine()
	if err != nil {
		// No engine to tell: the process is shutting down, or this is a runner
		// under test. Neither is a send failure and neither loses a note — an
		// unstamped one is redelivered on the phase's next entry. Logged rather
		// than swallowed so an engine that is unexpectedly gone outside those two
		// cases still leaves a trace beside the redeliveries it causes.
		log.Printf("workflow runner: ack rendered feedback for %s: %v", workflowRunKey(key), err)
		return
	}
	if err := workflowEngine.AckFeedbackRendered(key); err != nil {
		log.Printf("workflow runner: ack rendered feedback for %s: %v", workflowRunKey(key), err)
	}
}
