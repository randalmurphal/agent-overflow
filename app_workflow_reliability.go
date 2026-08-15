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

// workflowStopSendWait bounds how long a stop RUNNING ON THE ENGINE'S COMMAND
// LOOP will wait for its blocking half — the in-flight send barrier and, for
// `Stop`, the interrupt behind it (whose thread-action-lock acquisition is the
// other wait that can wedge).
//
// Both directions of the bound are load-bearing. A healthy send dispatch is a
// write plus, for Codex, one JSON-RPC reply — seconds at the outside — so a wait
// this long never truncates a send that was going to land, and the ordinary stop
// still waits the send out exactly as it always did. And every second past the
// bound is a second the ENGINE IS FROZEN: `teardown` calls `Runner.Stop`
// synchronously on the command-loop goroutine, which is the sole owner of all
// workflow FSM state, so a single send wedged on provider IO stalls every run
// and every verb in the process rather than only its own run (incident
// 2026-08-15 — a session restart blocked under the per-thread action lock
// mid-send and the run sat there for 60+ minutes).
//
// The bound's stated cost: on expiry the engine's teardown completes and
// releases the phase's resources — the `provider:<name>` capacity included —
// while the wedged session stays live until the abandoned goroutine's late
// interrupt lands, so the concurrent-session bound is exceeded by exactly the
// wedged sessions for exactly that window. That is the direction the incident
// chose deliberately: a briefly over-subscribed provider beats a frozen engine.
const workflowStopSendWait = 10 * time.Second

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
	attempt.providerRetryHint = false
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
		// A resend runs after the start returned, on a session the start already
		// proved: the start context is cancelled by then, and this send's bounded-wait
		// story is the reliability ladder's — the watchdog armed just above.
		// A drop is owned and logged by the door; only an error is this caller's.
		if _, err := r.sendIfActive(context.Background(), runKey, "the retry turn", message, schema, epoch); err != nil {
			r.finish(runKey, engine.Outcome{
				Kind:   engine.OutcomeExecutionFailure,
				Detail: workflowFailureDetail("sending the retry turn failed: " + err.Error()),
			})
		}
	}()
}

func (r *workflowAppRunner) detach(runKey string) (*workflowAttempt, bool) {
	r.mu.Lock()
	attempt, ok := r.detachLocked(runKey)
	r.mu.Unlock()
	if ok && attempt.unsubscribe != nil {
		attempt.unsubscribe()
	}
	return attempt, ok
}

// detachLocked is detach's registry half, for the one caller that has to decide
// to detach inside a wider critical section — the start watchdog's fallback,
// which reports an attempt dead and must not race an install of the same key.
// The observer unsubscribe deliberately stays with `detach`: it runs outside the
// runner lock, and a locked caller does it itself once it has released.
func (r *workflowAppRunner) detachLocked(runKey string) (*workflowAttempt, bool) {
	attempt, ok := r.runs[runKey]
	if !ok {
		return nil, false
	}
	delete(r.runs, runKey)
	delete(r.schemas, attempt.threadID)
	if r.workItems[attempt.threadID] == attempt.key.ItemID {
		delete(r.workItems, attempt.threadID)
	}
	r.disarmTimerLocked(attempt)
	return attempt, true
}

func (r *workflowAppRunner) stopAndFinish(runKey string, outcome engine.Outcome) {
	attempt, ok := r.detach(runKey)
	if !ok {
		return
	}
	r.stopDetachedAttempt(runKey, attempt, outcome)
}

// stopDetachedAttempt performs the blocking half of a stop after the attempt
// has been made unreachable to provider events. Keeping detach outside this
// helper lets an on-wire caller establish that invariant synchronously and move
// only the interrupt wait to another goroutine.
func (r *workflowAppRunner) stopDetachedAttempt(runKey string, attempt *workflowAttempt, outcome engine.Outcome) {
	r.interruptDetachedAttempt(runKey, attempt)
	attempt.complete(outcome)
}

// interruptDetachedAttempt is the blocking half of a stop on its own: wait out
// any send in flight, then interrupt the turn — unless the thread has moved on
// underneath the wait.
//
// It is split from `stopDetachedAttempt` for the callers that need the interrupt
// WITHOUT the completion: the start watchdog's grace fallback, which has already
// reported the attempt dead, and `Stop`'s bounded wait. Both run this on a
// goroutine of their own, because the wedge they are escaping usually IS a send
// holding `sendMu`, and the session comes down whenever that wedge clears.
func (r *workflowAppRunner) interruptDetachedAttempt(runKey string, attempt *workflowAttempt) {
	r.awaitInFlightSend(attempt)
	// The wait above is deliberately unbounded — its callers bound THEMSELVES
	// instead — so by the time it clears, this attempt's thread may belong to
	// somebody else. An Answer or resume continuation re-enters the phase on the
	// parked attempt's thread, and a human takeover claims that same thread.
	// Interrupting then would cut a live turn this attempt never owned, which is
	// worse than not interrupting at all: the dead attempt's session comes down
	// with its thread's next legitimate teardown either way.
	if owner, reclaimed := r.threadReclaimed(attempt); reclaimed {
		log.Printf(
			"workflow runner: not interrupting %s: thread %s now belongs to %s",
			runKey, attempt.threadID, owner,
		)
		return
	}
	if err := r.interrupt(context.Background(), attempt.threadID); err != nil {
		log.Printf("workflow runner: interrupt %s: %v", runKey, err)
	}
}

// threadReclaimed reports whether the runner holds a live claim on the detached
// attempt's thread that is not the attempt itself, and names it. The three
// registries are the three ways a thread can be live: a registered attempt, a
// start still provisioning (which selects and resumes the thread's provider
// session before `installAttempt` makes it visible in `runs`), and a human
// takeover. The identity comparison is what makes "any claim found is somebody
// else's" structural rather than caller discipline — a restored takeover
// re-registers the SAME attempt pointer under the same key, and skipping only
// `live != attempt` keeps the guard correct for any future caller that has not
// detached first.
//
// The scans are linear over maps holding one entry per live workflow element in
// the process; an index keyed by thread would be a second registry to keep
// coherent for maps this size.
func (r *workflowAppRunner) threadReclaimed(attempt *workflowAttempt) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for runKey, live := range r.runs {
		if live.threadID == attempt.threadID && live != attempt {
			return "attempt " + runKey, true
		}
	}
	for runKey, progress := range r.startProgress {
		if progress.threadID == attempt.threadID && !progress.reported {
			return "the starting attempt " + runKey, true
		}
	}
	if takeover, ok := r.takeovers[attempt.threadID]; ok {
		return "the human steering item " + takeover.itemID, true
	}
	return "", false
}

// awaitInFlightSend waits out a send that is mid-way through reaching the wire,
// so a completion or an interrupt cannot land underneath one. Every caller has
// already DETACHED the attempt, which is what bounds the wait in the ordinary
// case: the send door's admission drops anything decided before the detach.
//
// What it does NOT bound is a send already past admission and wedged on provider
// IO. A caller that cannot afford that wait uses `runBoundedBySendWait`.
func (r *workflowAppRunner) awaitInFlightSend(attempt *workflowAttempt) {
	attempt.sendMu.Lock()
	attempt.sendMu.Unlock()
}

// runBoundedBySendWait runs work on its own goroutine and waits
// `r.stopSendWait` for it, reporting whether it finished inside the bound.
//
// The goroutine is NOT abandoned when the bound expires: it runs to completion
// whenever the wedge it is behind clears, so the interrupt still happens and the
// session still comes down. What the bound protects is the CALLER — the engine's
// command-loop goroutine, which owns every run's FSM state and must never be
// parked behind one attempt's provider IO. See workflowStopSendWait.
//
// An expiry is counted on `wedgedStops` until the abandoned work completes, so
// `Stop` can refuse to pay the bound again while a wedge is already known to be
// outstanding — see the latch read there.
func (r *workflowAppRunner) runBoundedBySendWait(work func()) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		work()
	}()
	timer := time.NewTimer(r.stopSendWait)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		r.wedgedStops.Add(1)
		go func() {
			<-done
			r.wedgedStops.Add(-1)
		}()
		return false
	}
}

// foldSessionDeathIntoLadder advances one attempt's retry ladder for a provider
// session death, and parks the run when the schedule is spent.
//
// The latch (`pendingSessionDeath`) is the CLAIM, consumed here under the
// runner lock: `observe` latches the death it saw, `sessionDisconnected` folds
// it once the dead session has left the registry, and a second arrival for the
// same death finds the latch spent.
//
// The caller must not hold the runner mutex.
func (r *workflowAppRunner) foldSessionDeathIntoLadder(runKey string, attempt *workflowAttempt) {
	r.mu.Lock()
	if r.runs[runKey] != attempt || !attempt.pendingSessionDeath {
		r.mu.Unlock()
		return
	}
	attempt.pendingSessionDeath = false
	exhausted := r.scheduleTransientLocked(runKey, attempt)
	detail := attempt.failureDetail("the provider process kept dying until the retry schedule ran out")
	r.mu.Unlock()
	if !exhausted {
		return
	}
	// Off the wire like every other mid-turn park: the interrupt inside a plain
	// stop waits for a control response that a dead or dying session will never
	// send, and the caller here runs on the provider event path.
	r.stopAndFinishOffWire(runKey, engine.Outcome{Kind: engine.OutcomeTransientExhausted, Detail: detail})
}

// stopAndFinishOffWire is stopAndFinish for a caller running ON the provider
// event path — `observe`, which is dispatched synchronously from the session's
// event consumer.
//
// The interrupt inside `stopAndFinish` waits for the CLI's control_response,
// and that response can only arrive through the very pipeline this callback is
// blocking. A live process therefore deadlocks the stop against itself until
// the interrupt times out, and the run sits `running` for the whole of it —
// which is what a park that decides mid-turn (a spent retry ladder or a typed
// usage-limit refusal) does every time the provider is still alive to be
// interrupted.
// `detach` is single-shot under the runner lock and happens before this method
// returns to the event pipeline. Any later event therefore finds no attempt for
// this key; only the interrupt wait and completion callback move off-wire.
func (r *workflowAppRunner) stopAndFinishOffWire(runKey string, outcome engine.Outcome) {
	attempt, ok := r.detach(runKey)
	if !ok {
		return
	}
	go r.stopDetachedAttempt(runKey, attempt, outcome)
}

func workflowTransientError(event provider.ProviderEvent, providerRetryHint bool) (transient, waitsForCompletion bool) {
	if event.Kind != provider.EventError || event.Failure == nil {
		return false, false
	}
	switch event.Failure.Class {
	case provider.FailureTransient:
		return true, event.Failure.WaitsForTurnComplete()
	case provider.FailureTransientAfterRetry:
		return providerRetryHint, providerRetryHint && event.Failure.WaitsForTurnComplete()
	}
	return false, false
}

func workflowTransientTurnComplete(event provider.ProviderEvent) bool {
	return event.Kind == provider.EventTurnComplete && event.Failure != nil &&
		event.Failure.Class == provider.FailureTransient
}

func workflowTurnCompletedWithError(event provider.ProviderEvent) bool {
	meta, ok := event.TurnComplete.(*provider.WireTurnCompleteMeta)
	return ok && meta != nil && (strings.TrimSpace(meta.ErrorMessage) != "" || meta.StopReason == "error")
}
