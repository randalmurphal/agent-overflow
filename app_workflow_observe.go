package main

import (
	"context"
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
	// Claude subagent's rate limit from the typed provider-usage park into a bare
	// execution failure.
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
			r.finishOffWire(runKey, engine.Outcome{Kind: engine.OutcomeExecutionFailure, Detail: detail})
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
		// A typed usage refusal leaves the retry ladder before it starts. Reset
		// windows are advisory and model/bucket granularity is not reliable across
		// providers, so neither is used for control flow: the real refusal is the
		// proof, and an explicit resume always gets another real attempt.
		if workflowUsageLimitRefusal(event) {
			r.disarmTimerLocked(attempt)
			attempt.pendingTransient = false
			identity, identified := attempt.dispatchIdentity, attempt.dispatchIdentitySet
			detail := attempt.failureDetail("provider usage limit reached; resume after changing accounts or when usage is available")
			r.mu.Unlock()
			r.parkForUsageLimit(runKey, attempt.key.ItemID, identity, identified, detail)
			return
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
			r.finishOffWire(runKey, engine.Outcome{Kind: engine.OutcomeExecutionFailure, Detail: detail})
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
		r.finishOffWire(runKey, engine.Outcome{Kind: engine.OutcomeExecutionFailure, Detail: detail})
		return
	}
	validationErr := attempt.contract.Validate(payload)
	if validationErr == nil {
		r.mu.Unlock()
		outcome, err := workflowrunner.OutcomeFromEnvelope(payload)
		if err != nil {
			r.finishOffWire(runKey, engine.Outcome{
				Kind: engine.OutcomeExecutionFailure, Envelope: payload,
				Detail: workflowFailureDetail("the envelope validated but named no outcome: " + err.Error()),
			})
			return
		}
		r.finishOffWire(runKey, outcome)
		return
	}
	if attempt.envelopeRetryUsed {
		// The detail matters most where the payload is EMPTY — a turn that ended
		// having said nothing at all is the shape that used to park with no
		// account whatsoever. `outcomeDetailCause` drops it whenever the payload
		// has content, so a rejected envelope stays its own account.
		detail := workflowFailureDetail("the turn produced no valid envelope after a retry: " + validationErr.Error())
		r.mu.Unlock()
		r.finishOffWire(runKey, engine.Outcome{Kind: engine.OutcomeExecutionFailure, Envelope: payload, Detail: detail})
		return
	}
	attempt.envelopeRetryUsed = true
	schema := append(json.RawMessage(nil), attempt.schema...)
	attempt.currentPrompt = workflowrunner.RetryMessage(findingsForEnvelopeError(validationErr))
	message := attempt.currentPrompt
	epoch := attempt.sendEpoch
	r.mu.Unlock()
	go func() {
		// Post-start, on a session that has already run a turn: the start's context
		// is gone by now, and the bound on this send is the attempt's own watchdog.
		// A drop is owned and logged by the door; only an error is this caller's.
		if _, err := r.sendIfActive(context.Background(), runKey, "the envelope-feedback turn", message, schema, epoch); err != nil {
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
//
// The latch this looks for is claimed inside `foldSessionDeathIntoLadder`
// under the runner lock, so one death advances the ladder exactly once however
// many times it is reported.
func (r *workflowAppRunner) sessionDisconnected(threadID string) {
	var deadKey string
	var dead *workflowAttempt
	r.mu.Lock()
	for runKey, attempt := range r.runs {
		if attempt.threadID != threadID || !attempt.pendingSessionDeath {
			continue
		}
		deadKey, dead = runKey, attempt
		break
	}
	r.mu.Unlock()
	if dead == nil {
		return
	}
	r.foldSessionDeathIntoLadder(deadKey, dead)
}

func workflowProviderTurnStarted(providerName string, event provider.ProviderEvent) bool {
	if event.Kind == provider.EventTurnStart {
		return true
	}
	return providerName == string(provider.Claude) && event.Kind == provider.EventInit
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
