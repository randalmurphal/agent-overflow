package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/profile"
)

const defaultWorkflowWatchdog = 15 * time.Minute

type workflowTimer interface {
	Stop() bool
	Reset(time.Duration) bool
}

type workflowTimerMode uint8

const (
	workflowTimerNone workflowTimerMode = iota
	workflowTimerWatchdog
	workflowTimerBackoff
)

// projectProfile is the one live-profile read every phase start goes through.
// Capacities, bindings, reliability defaults, and secrets all come from the
// profile as it is on disk right now — editing it takes effect on the next
// phase start, with no restart and no re-run.
func (r *workflowAppRunner) projectProfile(ctx context.Context, projectID string) (*profile.Profile, error) {
	if r.profiles == nil {
		return nil, fmt.Errorf("workflow runner: profile source unavailable")
	}
	projectProfile, err := r.profiles.Profile(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("workflow runner: load profile for project %q: %w", projectID, err)
	}
	if projectProfile == nil {
		return nil, fmt.Errorf("workflow runner: load profile for project %q: nil profile", projectID)
	}
	return projectProfile, nil
}

func (r *workflowAppRunner) reliability(ctx context.Context, request engine.RunRequest) (time.Duration, []time.Duration, error) {
	projectProfile, err := r.projectProfile(ctx, request.Item.ProjectID)
	if err != nil {
		return 0, nil, err
	}
	return workflowReliability(projectProfile, request.Phase)
}

// workflowReliability derives one phase's inactivity watchdog and transient
// backoff schedule. Both drivers read the same profile defaults; a phase-level
// watchdog overrides the project default for either.
func workflowReliability(projectProfile *profile.Profile, phase def.Phase) (time.Duration, []time.Duration, error) {
	watchdog := defaultWorkflowWatchdog
	var err error
	if projectProfile.Reliability.Watchdog != "" {
		watchdog, err = parseWorkflowDuration(projectProfile.Reliability.Watchdog, "profile watchdog")
		if err != nil {
			return 0, nil, err
		}
	}
	if phase.Watchdog != "" {
		watchdog, err = parseWorkflowDuration(profile.Duration(phase.Watchdog), fmt.Sprintf("phase %q watchdog", phase.ID))
		if err != nil {
			return 0, nil, err
		}
	}

	authoredBackoff := projectProfile.Reliability.Backoff
	if len(authoredBackoff) == 0 {
		authoredBackoff = profile.DefaultBackoff()
	}
	backoff := make([]time.Duration, len(authoredBackoff))
	for index, value := range authoredBackoff {
		backoff[index], err = parseWorkflowDuration(value, fmt.Sprintf("profile backoff[%d]", index))
		if err != nil {
			return 0, nil, err
		}
	}
	return watchdog, backoff, nil
}

func parseWorkflowDuration(value profile.Duration, name string) (time.Duration, error) {
	duration, err := time.ParseDuration(string(value))
	if err != nil {
		return 0, fmt.Errorf("workflow runner: %s is invalid: %w", name, err)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("workflow runner: %s must be greater than 0", name)
	}
	return duration, nil
}

func (r *workflowAppRunner) armWatchdogLocked(runKey string, attempt *workflowAttempt) {
	r.disarmTimerLocked(attempt)
	attempt.timerMode = workflowTimerWatchdog
	attempt.timerDeadline = r.now().Add(attempt.watchdog)
	attempt.timer = r.newTimer(attempt.watchdog, func() { r.timerFired(runKey) })
}

func (r *workflowAppRunner) resetWatchdogLocked(attempt *workflowAttempt) {
	if attempt.timerMode != workflowTimerWatchdog || attempt.timer == nil {
		return
	}
	attempt.timerDeadline = r.now().Add(attempt.watchdog)
	attempt.timer.Reset(attempt.watchdog)
}

func (r *workflowAppRunner) disarmTimerLocked(attempt *workflowAttempt) {
	if attempt.timer != nil {
		attempt.timer.Stop()
	}
	attempt.timer = nil
	attempt.timerMode = workflowTimerNone
	attempt.timerDeadline = time.Time{}
}

// scheduleTransientLocked disarms the watchdog and starts the next configured
// backoff. It returns true when the closed retry schedule is exhausted.
func (r *workflowAppRunner) scheduleTransientLocked(runKey string, attempt *workflowAttempt) bool {
	r.disarmTimerLocked(attempt)
	attempt.turnStarted = false
	attempt.currentTurnID = ""
	attempt.pendingTransient = false
	attempt.claudeTransientRetry = false
	// Advancing the ladder retires whatever the previous state queued. This is the
	// only place a rung is armed, so it is the only place the epoch has to move.
	attempt.sendEpoch++
	if attempt.transientRetryCount >= len(attempt.backoff) {
		return true
	}
	delay := attempt.backoff[attempt.transientRetryCount]
	attempt.transientRetryCount++
	attempt.timerMode = workflowTimerBackoff
	attempt.timerDeadline = r.now().Add(delay)
	attempt.timer = r.newTimer(delay, func() { r.timerFired(runKey) })
	return false
}

func (r *workflowAppRunner) timerFired(runKey string) {
	r.mu.Lock()
	attempt := r.runs[runKey]
	if attempt == nil || attempt.timerMode == workflowTimerNone {
		r.mu.Unlock()
		return
	}
	if remaining := attempt.timerDeadline.Sub(r.now()); remaining > 0 {
		attempt.timer.Reset(remaining)
		r.mu.Unlock()
		return
	}
	mode := attempt.timerMode
	r.disarmTimerLocked(attempt)
	if mode == workflowTimerWatchdog {
		r.mu.Unlock()
		r.stopAndFinish(runKey, engine.Outcome{Kind: engine.OutcomeStalled})
		return
	}
	// The session died while this backoff was pending. `sessionDisconnected` owns
	// the next rung from here, so the held resend is dropped rather than fired
	// into a process being reaped — which is how a transient ladder used to turn
	// into an execution-failure park. The watchdog bounds the wait for a
	// disconnect that is not guaranteed to arrive.
	if attempt.pendingSessionDeath {
		r.armWatchdogLocked(runKey, attempt)
		r.mu.Unlock()
		return
	}
	// Codex identifies the next sub-attempt with EventTurnStart. Until it
	// arrives, ignore a delayed terminal event from the preceding turn. This one
	// is set BEFORE the send is dispatched rather than after it lands, because
	// the events it guards against are already in flight from the turn that just
	// failed — the other two Codex sends open their window at the send itself and
	// are covered at the chokepoint.
	attempt.awaitingTurnStart = attempt.provider == string(provider.Codex)
	// The retry's turn is not guaranteed to start: a send that reaches a session
	// whose turn is somehow still alive is queued into that turn rather than
	// starting one. The watchdog is the hard deadline on that wait — `observe`
	// returns above its reset while `awaitingTurnStart` holds, so nothing can
	// push it out — and a retry that never starts parks `stalled` instead of
	// leaving the run `running` with no timer. Claude re-arms it on a successful
	// send, which is the same watchdog disarmed and armed again.
	r.armWatchdogLocked(runKey, attempt)
	message := attempt.currentPrompt
	schema := append(json.RawMessage(nil), attempt.schema...)
	epoch := attempt.sendEpoch
	r.mu.Unlock()

	go func() {
		sent, err := r.sendIfActive(runKey, message, schema, epoch)
		if sent && err != nil {
			r.finish(runKey, engine.Outcome{
				Kind:   engine.OutcomeExecutionFailure,
				Detail: workflowFailureDetail("sending the retry turn failed: " + err.Error()),
			})
		}
	}()
}

func (r *workflowAppRunner) detach(runKey string) (*workflowAttempt, bool) {
	r.mu.Lock()
	attempt, ok := r.runs[runKey]
	if ok {
		delete(r.runs, runKey)
		delete(r.schemas, attempt.threadID)
		if r.workItems[attempt.threadID] == attempt.key.ItemID {
			delete(r.workItems, attempt.threadID)
		}
		r.disarmTimerLocked(attempt)
	}
	r.mu.Unlock()
	if ok && attempt.unsubscribe != nil {
		attempt.unsubscribe()
	}
	return attempt, ok
}

func (r *workflowAppRunner) stopAndFinish(runKey string, outcome engine.Outcome) {
	attempt, ok := r.detach(runKey)
	if !ok {
		return
	}
	attempt.sendMu.Lock()
	attempt.sendMu.Unlock()
	if err := r.app.InterruptTurn(attempt.threadID); err != nil {
		log.Printf("workflow runner: interrupt %s: %v", runKey, err)
	}
	attempt.complete(outcome)
}

// stopAndFinishOffWire is stopAndFinish for a caller running ON the provider
// event path — `observe`, which is dispatched synchronously from the session's
// event consumer.
//
// The interrupt inside `stopAndFinish` waits for the CLI's control_response,
// and that response can only arrive through the very pipeline this callback is
// blocking. A live process therefore deadlocks the stop against itself until
// the interrupt times out, and the run sits `running` for the whole of it —
// which is what a park that decides mid-turn (a spent retry ladder, a dated
// quota refusal) does every time the provider is still alive to be interrupted.
// `detach` is single-shot under the runner lock, so handing the rest to a
// goroutine cannot double-finish: any event arriving in between finds no
// attempt for this key.
func (r *workflowAppRunner) stopAndFinishOffWire(runKey string, outcome engine.Outcome) {
	go r.stopAndFinish(runKey, outcome)
}

func workflowTransientError(providerName string, event provider.ProviderEvent, claudeRetryable bool) (transient, waitsForCompletion bool) {
	if event.Kind != provider.EventError || len(event.Meta) == 0 {
		return false, false
	}
	var meta struct {
		APIErrorEnum       string `json:"api_error_enum"`
		ExpectTurnComplete bool   `json:"expect_turn_complete"`
		Wire               struct {
			Error struct {
				CodexErrorInfo json.RawMessage `json:"codexErrorInfo"`
			} `json:"error"`
		} `json:"wire"`
	}
	if json.Unmarshal(event.Meta, &meta) != nil {
		return false, false
	}
	switch providerName {
	case string(provider.Claude):
		if meta.APIErrorEnum == "rate_limit" || meta.APIErrorEnum == "server_error" && claudeRetryable {
			return true, meta.ExpectTurnComplete
		}
	case string(provider.Codex):
		// A Codex `error` notification is informational: the authoritative
		// terminal signal is `turn/completed`, which core always emits and marks
		// `failed` when the turn errored. Waiting for it is what keeps the ladder
		// from arming while the turn is still alive — a retry sent into a live
		// turn is absorbed as queued input and mints a turn id nothing ever
		// starts, which is a permanent hang rather than a retry.
		if codexTransientErrorInfo(meta.Wire.Error.CodexErrorInfo) {
			return true, true
		}
	}
	return false, false
}

func workflowClaudeTransientAPIRetry(event provider.ProviderEvent) bool {
	if event.Kind != provider.EventAPIRetry || len(event.Meta) == 0 {
		return false
	}
	var meta struct {
		Wire json.RawMessage `json:"wire"`
	}
	if json.Unmarshal(event.Meta, &meta) != nil || len(meta.Wire) == 0 {
		return false
	}
	var wire struct {
		ErrorStatus int `json:"error_status"`
		Error       struct {
			Connection struct {
				Code string `json:"code"`
			} `json:"connection"`
		} `json:"error"`
	}
	if json.Unmarshal(meta.Wire, &wire) != nil {
		return false
	}
	return wire.ErrorStatus == 529 || wire.Error.Connection.Code == "ECONNRESET"
}

func codexTransientErrorInfo(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var scalar string
	if json.Unmarshal(raw, &scalar) == nil {
		switch scalar {
		case "serverOverloaded", "usageLimitExceeded":
			return true
		default:
			return false
		}
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) != 1 {
		return false
	}
	for kind := range object {
		switch kind {
		case "httpConnectionFailed", "responseStreamConnectionFailed", "responseStreamDisconnected":
			return true
		}
	}
	return false
}

func workflowTransientTurnComplete(providerName string, event provider.ProviderEvent) bool {
	if providerName != string(provider.Claude) || event.Kind != provider.EventTurnComplete || len(event.Raw) == 0 {
		return false
	}
	var result struct {
		TerminalReason string `json:"terminal_reason"`
	}
	return json.Unmarshal(event.Raw, &result) == nil && result.TerminalReason == "network_error"
}

func workflowTurnCompletedWithError(event provider.ProviderEvent) bool {
	meta, ok := event.TurnComplete.(*provider.WireTurnCompleteMeta)
	return ok && meta != nil && (strings.TrimSpace(meta.ErrorMessage) != "" || meta.StopReason == "error")
}
