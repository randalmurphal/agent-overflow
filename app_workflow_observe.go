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
	// mechanism silently inert on Claude — a generic `retries-exhausted` park
	// with no self-resume, indistinguishable from the bug this feature fixes.
	// The event then continues through the machine exactly as any other
	// non-terminal event does.
	if event.Kind == provider.EventRateLimits {
		attempt.noteRateLimits(event.Meta)
	}
	if attempt.timerMode == workflowTimerBackoff {
		r.mu.Unlock()
		return
	}
	if attempt.awaitingRetryStart {
		if event.Kind == provider.EventSessionStatus && strings.TrimSpace(event.Content) == "error" {
			attempt.awaitingRetryStart = false
			attempt.pendingSessionDeath = true
			r.mu.Unlock()
			return
		}
		if !workflowProviderTurnStarted(attempt.provider, event) {
			r.mu.Unlock()
			return
		}
		attempt.awaitingRetryStart = false
	}
	if workflowProviderTurnStarted(attempt.provider, event) {
		attempt.turnStarted = true
		attempt.pendingTransient = false
		attempt.claudeTransientRetry = false
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
			r.disarmTimerLocked(attempt)
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
	if event.Kind == provider.EventAPIRetry && attempt.provider == string(provider.Claude) {
		if workflowClaudeTransientAPIRetry(event) {
			attempt.claudeTransientRetry = true
		}
		r.mu.Unlock()
		return
	}
	if event.Kind == provider.EventError {
		if detail := workflowProviderErrorDetail(event); detail != "" {
			attempt.lastFailure = detail
		}
		transient, waitsForCompletion := workflowTransientError(
			attempt.provider, event, attempt.claudeTransientRetry,
		)
		// A dated quota refusal leaves the retry ladder before it starts: the
		// backoffs run out in minutes and the allowance comes back in hours or
		// days, so every retry is waste and the park is the answer. It takes the
		// exhausted path deliberately — it IS the retries being exhausted, and
		// `retries-exhausted` is the one park a bare resume continues on the
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
		if workflowTurnErrorIsTerminal(event.Meta) {
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
	r.disarmTimerLocked(attempt)
	attempt.turnStarted = false
	attempt.claudeTransientRetry = false
	payload := append(json.RawMessage(nil), event.StructuredOutput...)
	if len(payload) == 0 && (attempt.pendingTransient || workflowTransientTurnComplete(attempt.provider, event)) {
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
	r.mu.Unlock()
	go func() {
		sent, err := r.sendIfActive(runKey, message, schema)
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

// sendIfActive serializes sends with Stop and rechecks ownership after taking
// the per-attempt lock. A retry queued before cancellation therefore cannot
// start a new provider turn after Stop has removed the attempt.
func (r *workflowAppRunner) sendIfActive(runKey, message string, schema json.RawMessage) (bool, error) {
	r.mu.Lock()
	attempt := r.runs[runKey]
	r.mu.Unlock()
	if attempt == nil {
		return false, nil
	}

	attempt.sendMu.Lock()
	defer attempt.sendMu.Unlock()
	r.mu.Lock()
	active := r.runs[runKey] == attempt
	r.mu.Unlock()
	if !active {
		return false, nil
	}
	err := r.app.sendWorkflowMessage(attempt.threadID, message, schema)
	if err != nil || attempt.provider != string(provider.Claude) {
		return true, err
	}

	// Claude has no per-turn EventTurnStart. A successful send is therefore
	// the start signal when an existing session emits no fresh EventInit (for
	// example, the envelope-feedback turn and a transient sub-attempt).
	r.mu.Lock()
	if r.runs[runKey] == attempt && !attempt.turnStarted && !attempt.pendingSessionDeath && attempt.timerMode != workflowTimerBackoff {
		attempt.turnStarted = true
		attempt.pendingTransient = false
		attempt.claudeTransientRetry = false
		r.armWatchdogLocked(runKey, attempt)
	}
	r.mu.Unlock()
	return true, nil
}

func workflowTurnErrorIsTerminal(meta json.RawMessage) bool {
	if len(meta) == 0 {
		return false
	}
	var flags struct {
		Fatal              bool `json:"fatal"`
		ExpectTurnComplete bool `json:"expect_turn_complete"`
	}
	return json.Unmarshal(meta, &flags) == nil && flags.Fatal && !flags.ExpectTurnComplete
}
