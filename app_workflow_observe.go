package main

import (
	"encoding/json"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/workflow/engine"
	workflowrunner "agent-overflow/internal/workflow/runner"
)

// observe is the whole provider-event state machine for one agent-backed
// workflow turn — a phase attempt, a fan-out unit, or a join alike. It decides
// when a turn started, when a failure is transient enough to retry, and when a
// final message is the envelope the attempt was waiting for.
func (r *workflowAppRunner) observe(runKey string, event provider.ProviderEvent) {
	r.mu.Lock()
	attempt := r.runs[runKey]
	if attempt == nil {
		r.mu.Unlock()
		return
	}
	// The quota windows this session's account is under, recorded ABOVE every
	// early return below and without one of its own.
	//
	// Recording has no state-machine effect — nothing downstream reads it except
	// a usage-limit refusal deciding when the run can come back — so the guards
	// that exist to stop a dead or retrying attempt from ACTING must not also
	// stop it from listening. The ordering between a provider's snapshot and its
	// refusal is verified for Codex (`update_rate_limits` runs before
	// `UsageLimitReached` returns) and verified for NEITHER on Claude: if the CLI
	// ever emits `rate_limit_event` after `assistant.error`, the snapshot lands
	// while the ladder is armed, and dropping it here would make the whole
	// mechanism silently inert on Claude — a `provider-retries-exhausted` park
	// with no self-resume, indistinguishable from the bug this feature fixes.
	// The event then continues through the machine exactly as any other
	// non-terminal event does.
	if event.Kind == provider.EventRateLimits {
		attempt.noteRateLimits(event.Meta)
	}
	// A child thread's events are the parent turn's ACTIVITY, never its signals.
	// Both adapters stamp `ParentToolUseID` on everything a subagent or collab
	// child emits — including the child's own `error`, which the Codex adapter
	// forwards onto the parent session's stream with the child's `codexErrorInfo`
	// intact — and a child's turn boundaries never arrive here at all (they are
	// diverted to EventSubagentStatus). So nothing parented may enter the retry
	// ladder, park for quota, answer the turn start a retry is waiting on, or be
	// consumed as this turn's completion.
	//
	// The one exception is the parented error that IS the parent turn ending —
	// see `workflowParentedErrorClosesTurn`. Filtering that one would downgrade a
	// Claude subagent's rate limit from the ladder and the dated-quota self-resume
	// into a bare execution failure.
	//
	// The watchdog reset is the one thing every other child event keeps, and it is
	// load-bearing: a delegating turn leaves the parent stream quiet for as long
	// as its children work, and that quiet is not a stall.
	if event.ParentToolUseID != "" && !workflowParentedErrorClosesTurn(event) {
		if attempt.turnStarted {
			r.resetWatchdogLocked(attempt)
		}
		r.mu.Unlock()
		return
	}
	if attempt.timerMode == workflowTimerBackoff {
		// A session that dies INSIDE the backoff window is latched rather than
		// dropped. Without the latch `sessionDisconnected` skips this attempt, and
		// the resend the backoff is holding lands in a session that has already
		// been reaped — a transient failure converted into an `agent-error` park.
		// Folding it in costs the next rung, which is the honest account: two
		// failures happened.
		if event.Kind == provider.EventSessionStatus && strings.TrimSpace(event.Content) == "error" {
			attempt.pendingSessionDeath = true
		}
		r.mu.Unlock()
		return
	}
	if attempt.awaitingTurnStart {
		if event.Kind == provider.EventSessionStatus && strings.TrimSpace(event.Content) == "error" {
			attempt.awaitingTurnStart = false
			attempt.pendingSessionDeath = true
			r.mu.Unlock()
			return
		}
		if !workflowProviderTurnStarted(attempt.provider, event) {
			r.mu.Unlock()
			return
		}
		attempt.awaitingTurnStart = false
	}
	if workflowProviderTurnStarted(attempt.provider, event) {
		// A turn start arriving while a turn is already started is a REPLAY, never
		// a fresh one: every legitimate start follows a state that set
		// `turnStarted` false — a consumed completion, the transient ladder, a
		// session death. The Codex adapter's start-side dedupe drops a turn id at
		// that turn's completion, so a `thread/read` replay of a COMPLETED turn's
		// `turn/started` reaches here unclaimed, and adopting it would retarget
		// the attempt at the dead turn and drop the live one's completion as a
		// mismatch.
		if attempt.turnStarted {
			r.resetWatchdogLocked(attempt)
			r.mu.Unlock()
			return
		}
		attempt.turnStarted = true
		// Codex names every turn; Claude names none, which leaves this empty and
		// the completion filter below inert for it. Assigned rather than merged so
		// a turn start can never inherit the previous turn's identity.
		attempt.currentTurnID = event.TurnID
		attempt.pendingTransient = false
		attempt.providerRetryHint = false
		r.armWatchdogLocked(runKey, attempt)
		r.mu.Unlock()
		return
	}
	if attempt.turnStarted {
		r.resetWatchdogLocked(attempt)
	}
	if event.Kind == provider.EventSessionStatus {
		content := strings.TrimSpace(event.Content)
		if content == "error" {
			// The disconnect that folds this into the retry ladder is not
			// guaranteed to arrive. Arming rather than disarming makes a session
			// that errors and then goes quiet park `stalled` — loud and repairable
			// — instead of sitting `running` with no timer and nothing left to
			// wake it.
			r.armWatchdogLocked(runKey, attempt)
			attempt.turnStarted = false
			attempt.pendingSessionDeath = true
			r.mu.Unlock()
			return
		}
		if content == "disconnected" {
			if attempt.pendingSessionDeath {
				r.mu.Unlock()
				return
			}
			detail := attempt.failureDetail("the provider session disconnected before the phase produced an envelope")
			r.mu.Unlock()
			r.finish(runKey, engine.Outcome{Kind: engine.OutcomeExecutionFailure, Detail: detail})
			return
		}
	}
	if event.Kind == provider.EventAPIRetry && event.Failure != nil && event.Failure.Class == provider.FailureTransient {
		attempt.providerRetryHint = true
		r.mu.Unlock()
		return
	}
	if event.Kind == provider.EventError {
		if detail := workflowProviderErrorDetail(event); detail != "" {
			attempt.lastFailure = detail
		}
		transient, waitsForCompletion := workflowTransientError(event, attempt.providerRetryHint)
		// A dated quota refusal leaves the retry ladder before it starts: the
		// backoffs run out in minutes and the allowance comes back in hours or
		// days, so every retry is waste and the park is the answer. It takes the
		// exhausted path deliberately — it IS the retries being exhausted, and
		// `provider-retries-exhausted` is the park a bare resume continues on the
		// session the turn died in.
		if transient {
			if resetsAt, resumeAt, ok := r.quotaParkLocked(attempt, event); ok {
				r.disarmTimerLocked(attempt)
				attempt.pendingTransient = false
				itemID := attempt.key.ItemID
				r.mu.Unlock()
				r.parkForQuotaLimit(runKey, itemID, resetsAt, resumeAt)
				return
			}
		}
		if transient && waitsForCompletion {
			attempt.pendingTransient = true
			// The turn's own terminal event is what acts on this, so a live turn's
			// watchdog is already the bound. When no turn is live there is no
			// completion coming and nothing else would ever fire.
			if attempt.timerMode == workflowTimerNone {
				r.armWatchdogLocked(runKey, attempt)
			}
			r.mu.Unlock()
			return
		}
		if transient {
			exhausted := r.scheduleTransientLocked(runKey, attempt)
			detail := attempt.failureDetail("the provider kept failing until the retry schedule ran out")
			r.mu.Unlock()
			if exhausted {
				r.stopAndFinishOffWire(runKey, engine.Outcome{Kind: engine.OutcomeTransientExhausted, Detail: detail})
			}
			return
		}
		if workflowTurnErrorIsTerminal(event) {
			detail := attempt.failureDetail("the provider reported a fatal error")
			r.mu.Unlock()
			r.finish(runKey, engine.Outcome{Kind: engine.OutcomeExecutionFailure, Detail: detail})
			return
		}
	}
	if event.Kind != provider.EventTurnComplete {
		r.mu.Unlock()
		return
	}
	// A `thread/read` replay re-emits turn lifecycle. The started side is
	// handled above (a start while a turn is started is a replay); this is the
	// completed side, without which a replayed completion of an earlier turn
	// finishes the phase on a stale envelope.
	if attempt.currentTurnID != "" && event.TurnID != "" && event.TurnID != attempt.currentTurnID {
		r.mu.Unlock()
		return
	}
	r.disarmTimerLocked(attempt)
	attempt.turnStarted = false
	attempt.currentTurnID = ""
	attempt.providerRetryHint = false
	payload := append(json.RawMessage(nil), event.StructuredOutput...)
	if len(payload) == 0 && (attempt.pendingTransient || workflowTransientTurnComplete(event)) {
		attempt.pendingTransient = false
		exhausted := r.scheduleTransientLocked(runKey, attempt)
		detail := attempt.failureDetail("the provider kept failing until the retry schedule ran out")
		r.mu.Unlock()
		if exhausted {
			r.stopAndFinishOffWire(runKey, engine.Outcome{Kind: engine.OutcomeTransientExhausted, Detail: detail})
		}
		return
	}
	attempt.pendingTransient = false
	if len(payload) == 0 && workflowTurnCompletedWithError(event) {
		detail := attempt.failureDetail(workflowTurnCompleteFailureDetail(event))
		r.mu.Unlock()
		r.finish(runKey, engine.Outcome{Kind: engine.OutcomeExecutionFailure, Detail: detail})
		return
	}
	validationErr := attempt.contract.Validate(payload)
	if validationErr == nil {
		r.mu.Unlock()
		outcome, err := workflowrunner.OutcomeFromEnvelope(payload)
		if err != nil {
			r.finish(runKey, engine.Outcome{
				Kind: engine.OutcomeExecutionFailure, Envelope: payload,
				Detail: workflowFailureDetail("the envelope validated but named no outcome: " + err.Error()),
			})
			return
		}
		r.finish(runKey, outcome)
		return
	}
	if attempt.envelopeRetryUsed {
		// The detail matters most where the payload is EMPTY — a turn that ended
		// having said nothing at all is the shape that used to park with no
		// account whatsoever. `outcomeDetailCause` drops it whenever the payload
		// has content, so a rejected envelope stays its own account.
		detail := workflowFailureDetail("the turn produced no valid envelope after a retry: " + validationErr.Error())
		r.mu.Unlock()
		r.finish(runKey, engine.Outcome{Kind: engine.OutcomeExecutionFailure, Envelope: payload, Detail: detail})
		return
	}
	attempt.envelopeRetryUsed = true
	schema := append(json.RawMessage(nil), attempt.schema...)
	attempt.currentPrompt = workflowrunner.RetryMessage(findingsForEnvelopeError(validationErr))
	message := attempt.currentPrompt
	epoch := attempt.sendEpoch
	r.mu.Unlock()
	go func() {
		sent, err := r.sendIfActive(runKey, message, schema, epoch)
		if sent && err != nil {
			r.finish(runKey, engine.Outcome{
				Kind: engine.OutcomeExecutionFailure, Envelope: payload,
				Detail: workflowFailureDetail("sending the envelope-feedback turn failed: " + err.Error()),
			})
		}
	}()
}

// sessionDisconnected advances subprocess-death retry only after the dead
// session has been removed from App's registry. That ordering prevents a
// millisecond harness backoff from sending into the session being reaped.
func (r *workflowAppRunner) sessionDisconnected(threadID string) {
	var exhaustedKey, exhaustedDetail string
	r.mu.Lock()
	for runKey, attempt := range r.runs {
		if attempt.threadID != threadID || !attempt.pendingSessionDeath {
			continue
		}
		attempt.pendingSessionDeath = false
		if r.scheduleTransientLocked(runKey, attempt) {
			exhaustedKey = runKey
			exhaustedDetail = attempt.failureDetail("the provider process kept dying until the retry schedule ran out")
		}
		break
	}
	r.mu.Unlock()
	if exhaustedKey != "" {
		// Off the wire like every other mid-turn park. This one is safe to block
		// today only because the session was unregistered a moment ago, which
		// makes the interrupt a no-op — safety that lives in the caller's
		// ordering rather than in the call, and the next caller does not inherit
		// it. The same helper removes the question.
		r.stopAndFinishOffWire(exhaustedKey, engine.Outcome{Kind: engine.OutcomeTransientExhausted, Detail: exhaustedDetail})
	}
}

func workflowProviderTurnStarted(providerName string, event provider.ProviderEvent) bool {
	if event.Kind == provider.EventTurnStart {
		return true
	}
	return providerName == string(provider.Claude) && event.Kind == provider.EventInit
}

// sendIfActive is the one door every workflow send goes through. It serializes
// with Stop and rechecks, after taking the per-attempt lock, that the state
// which decided to send is still the state that exists: the attempt is still
// installed, its session is not already known dead, and the ladder has not
// advanced past the rung this send belongs to. A send decided by a superseded
// state is dropped rather than delivered late.
//
// `epoch` is the caller's `attempt.sendEpoch`, read under the runner lock at the
// moment the send was decided. See the field.
func (r *workflowAppRunner) sendIfActive(runKey, message string, schema json.RawMessage, epoch int) (bool, error) {
	r.mu.Lock()
	attempt := r.runs[runKey]
	r.mu.Unlock()
	if attempt == nil {
		return false, nil
	}

	attempt.sendMu.Lock()
	defer attempt.sendMu.Unlock()
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
	active := r.runs[runKey] == attempt && !attempt.pendingSessionDeath && attempt.sendEpoch == epoch
	r.mu.Unlock()
	if !active {
		return false, nil
	}
	if err := r.app.sendWorkflowMessage(attempt.threadID, message, schema); err != nil {
		return true, err
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
	return true, nil
}

// workflowParentedErrorClosesTurn reports whether an event carrying a child's
// `ParentToolUseID` is nonetheless the PARENT turn failing.
//
// The two adapters mean different things by the same field. Claude stamps a Task
// subagent's id on that subagent's `assistant.error`, but the error is the
// parent's open turn failing — the CLI follows it with the `result{is_error}`
// that closes that turn, and the adapter records that as parent-turn scope.
// Codex forwards a collab child's own error as child-turn scope, because the
// child's failure is the child's alone.
//
// The adapter-normalized scope is the whole test: a completion boundary alone
// is insufficient because a Codex child's error also waits for that CHILD's
// completion, which never belongs to this parent machine.
func workflowParentedErrorClosesTurn(event provider.ProviderEvent) bool {
	return event.Kind == provider.EventError && event.Failure != nil &&
		event.Failure.Scope == provider.FailureScopeParentTurn
}

func workflowTurnErrorIsTerminal(event provider.ProviderEvent) bool {
	return event.Kind == provider.EventError && event.Failure != nil && event.Failure.EndsTurn()
}
