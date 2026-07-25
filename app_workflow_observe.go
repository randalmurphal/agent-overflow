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
			r.mu.Unlock()
			r.finish(runKey, engine.Outcome{Kind: engine.OutcomeExecutionFailure})
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
		transient, waitsForCompletion := workflowTransientError(
			attempt.provider, event, attempt.claudeTransientRetry,
		)
		if transient && waitsForCompletion {
			attempt.pendingTransient = true
			r.mu.Unlock()
			return
		}
		if transient {
			exhausted := r.scheduleTransientLocked(runKey, attempt)
			r.mu.Unlock()
			if exhausted {
				r.stopAndFinish(runKey, engine.Outcome{Kind: engine.OutcomeTransientExhausted})
			}
			return
		}
		if workflowTurnErrorIsTerminal(event.Meta) {
			r.mu.Unlock()
			r.finish(runKey, engine.Outcome{Kind: engine.OutcomeExecutionFailure})
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
		r.mu.Unlock()
		if exhausted {
			r.stopAndFinish(runKey, engine.Outcome{Kind: engine.OutcomeTransientExhausted})
		}
		return
	}
	attempt.pendingTransient = false
	if len(payload) == 0 && workflowTurnCompletedWithError(event) {
		r.mu.Unlock()
		r.finish(runKey, engine.Outcome{Kind: engine.OutcomeExecutionFailure})
		return
	}
	validationErr := attempt.contract.Validate(payload)
	if validationErr == nil {
		r.mu.Unlock()
		outcome, err := workflowrunner.OutcomeFromEnvelope(payload)
		if err != nil {
			r.finish(runKey, engine.Outcome{Kind: engine.OutcomeExecutionFailure, Envelope: payload})
			return
		}
		r.finish(runKey, outcome)
		return
	}
	if attempt.envelopeRetryUsed {
		r.mu.Unlock()
		r.finish(runKey, engine.Outcome{Kind: engine.OutcomeExecutionFailure, Envelope: payload})
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
			r.finish(runKey, engine.Outcome{Kind: engine.OutcomeExecutionFailure, Envelope: payload})
		}
	}()
}

// sessionDisconnected advances subprocess-death retry only after the dead
// session has been removed from App's registry. That ordering prevents a
// millisecond harness backoff from sending into the session being reaped.
func (r *workflowAppRunner) sessionDisconnected(threadID string) {
	var exhaustedKey string
	r.mu.Lock()
	for runKey, attempt := range r.runs {
		if attempt.threadID != threadID || !attempt.pendingSessionDeath {
			continue
		}
		attempt.pendingSessionDeath = false
		if r.scheduleTransientLocked(runKey, attempt) {
			exhaustedKey = runKey
		}
		break
	}
	r.mu.Unlock()
	if exhaustedKey != "" {
		r.stopAndFinish(exhaustedKey, engine.Outcome{Kind: engine.OutcomeTransientExhausted})
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
