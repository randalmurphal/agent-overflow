package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
)

// The event sequence that zombied a production run, and the rules that make it
// impossible. A codex phase turn delegating to collab child threads saw a
// CHILD's `serverOverloaded` error forwarded onto the parent session's stream;
// the parent turn machine read it as its own transient failure, armed the retry
// ladder, and re-sent the phase prompt into a session whose real turn was still
// running. Codex queued that send into the live turn, so no `turn/started` ever
// arrived, and the real completion was then dropped by the retry's own filter
// with no timer left armed — permanently `running`.

// workflowValidEnvelope is the smallest payload the zero contract accepts.
var workflowValidEnvelope = json.RawMessage(`{"status":"done","outputs":{},"question":null,"reason":null}`)

const (
	codexOverloadedMeta   = `{"fatal":false,"wire":{"error":{"codexErrorInfo":"serverOverloaded"}}}`
	codexUsageLimitMeta   = `{"fatal":false,"wire":{"error":{"codexErrorInfo":"usageLimitExceeded"}}}`
	claudeRateLimitedMeta = `{"api_error_enum":"rate_limit","fatal":true,"expect_turn_complete":true}`
)

func codexOverloadedEvent(parent string) provider.ProviderEvent {
	return provider.ProviderEvent{
		Kind: provider.EventError, ParentToolUseID: parent,
		Content: "server overloaded", Meta: json.RawMessage(codexOverloadedMeta),
		Failure: &provider.FailureMeta{
			Class: provider.FailureTransient, Boundary: provider.FailureBoundaryTurn,
		},
	}
}

func claudeRateLimitedEvent(parent string) provider.ProviderEvent {
	return provider.ProviderEvent{
		Kind: provider.EventError, ParentToolUseID: parent,
		Content: "Claude usage limit reached", Meta: json.RawMessage(claudeRateLimitedMeta),
		Failure: &provider.FailureMeta{
			Class: provider.FailureTransient, Boundary: provider.FailureBoundaryTurn,
			Reason: provider.FailureReasonUsageLimit,
			Scope:  provider.FailureScopeParentTurn,
		},
	}
}

// observeHarness drives `observe` directly against one installed attempt with a
// deterministic clock, timer factory, and send stub. That is the level these
// rules live at: which events may move the turn machine, and what is left armed
// afterwards.
type observeHarness struct {
	t        *testing.T
	app      *App
	runner   *workflowAppRunner
	attempt  *workflowAttempt
	runKey   string
	outcomes chan engine.Outcome
	sends    chan string
	now      time.Time
}

// observeState is a snapshot of the attempt taken under the runner lock, so an
// assertion never races the retry send `timerFired` dispatches.
type observeState struct {
	mode                workflowTimerMode
	timer               *fakeWorkflowTimer
	deadline            time.Time
	installed           bool
	turnStarted         bool
	currentTurnID       string
	pendingTransient    bool
	pendingSessionDeath bool
	awaitingTurnStart   bool
	retryCount          int
	sendEpoch           int
	lastFailure         string
	claudeRetryable     bool
}

func newObserveHarness(t *testing.T, providerName string) *observeHarness {
	t.Helper()
	// A real store, because a typed usage-limit park is one of the outcomes under test here:
	// with a bare App the park panics on the schedule write, and a panic reports
	// as a crashed binary rather than as the assertion that caught the
	// regression. Nothing in this file wants the schedule to succeed.
	app := newTestAppWithStore(t)
	harness := &observeHarness{
		t: t, app: app, outcomes: make(chan engine.Outcome, 4), sends: make(chan string, 4),
		now: time.Unix(1_700_000_000, 0),
	}
	app.sendMessageFn = func(_, content string, _ []string) error {
		harness.sends <- content
		return nil
	}
	app.workflowAutoResumeNowFn = func() time.Time { return harness.now }
	app.newWorkflowAutoResumeTimer = func(delay time.Duration, fire func()) workflowTimer {
		return &armedWorkflowTimer{callback: fire, delay: delay}
	}
	harness.runner = newWorkflowAppRunner(app, t.TempDir(), nil)
	harness.runner.now = func() time.Time { return harness.now }
	harness.runner.newTimer = func(delay time.Duration, callback func()) workflowTimer {
		return &fakeWorkflowTimer{callback: callback, delay: delay, active: true}
	}
	key := engine.RunKey{ItemID: "item", PhaseID: "phase", Attempt: 1}
	harness.runKey = workflowRunKey(key)
	harness.attempt = &workflowAttempt{
		workflowCompletion: workflowCompletion{key: key},
		threadID:           "thread",
		schema:             json.RawMessage(`{"type":"object"}`),
		provider:           providerName,
		phase:              def.Phase{ID: "phase", Provider: providerName},
		complete:           func(outcome engine.Outcome) { harness.outcomes <- outcome },
		unsubscribe:        func() {},
		currentPrompt:      "run the phase",
		watchdog:           10 * time.Minute,
		backoff:            []time.Duration{30 * time.Second, 2 * time.Minute},
	}
	harness.runner.runs[harness.runKey] = harness.attempt
	harness.runner.schemas["thread"] = harness.attempt.schema
	harness.runner.workItems["thread"] = key.ItemID
	return harness
}

func (h *observeHarness) observe(event provider.ProviderEvent) {
	h.runner.observe(h.runKey, event)
}

// send drives the one send chokepoint the way every POST-START producer does:
// the start's context is long gone by then, so the bound on these sends is the
// attempt's own watchdog rather than the start deadline.
func (h *observeHarness) send(message string, epoch int) (workflowSendDropReason, error) {
	h.t.Helper()
	return h.runner.sendIfActive(context.Background(), h.runKey, "the test turn", message, h.attempt.schema, epoch)
}

func (h *observeHarness) state() observeState {
	h.runner.mu.Lock()
	defer h.runner.mu.Unlock()
	timer, _ := h.attempt.timer.(*fakeWorkflowTimer)
	return observeState{
		mode: h.attempt.timerMode, timer: timer, deadline: h.attempt.timerDeadline,
		installed:   h.runner.runs[h.runKey] == h.attempt,
		turnStarted: h.attempt.turnStarted, currentTurnID: h.attempt.currentTurnID,
		pendingTransient: h.attempt.pendingTransient, pendingSessionDeath: h.attempt.pendingSessionDeath,
		awaitingTurnStart: h.attempt.awaitingTurnStart, sendEpoch: h.attempt.sendEpoch,
		retryCount: h.attempt.transientRetryCount, lastFailure: h.attempt.lastFailure,
		claudeRetryable: h.attempt.providerRetryHint,
	}
}

// sendEpoch reads the epoch a caller deciding to send right now would capture.
func (h *observeHarness) epoch() int {
	h.runner.mu.Lock()
	defer h.runner.mu.Unlock()
	return h.attempt.sendEpoch
}

// fire advances the clock past the armed deadline and runs the timer callback,
// which is how every deadline in this file is reached.
func (h *observeHarness) fire(extra time.Duration) {
	h.t.Helper()
	state := h.state()
	if state.timer == nil {
		h.t.Fatal("no timer is armed")
	}
	h.now = state.deadline.Add(extra)
	state.timer.fire()
}

// waitForMode polls the attempt until it reaches a timer mode. The sends that
// arm one run on the goroutine `observe` and `timerFired` dispatch them to, so
// the arming is not ordered against the send stub returning.
func (h *observeHarness) waitForMode(want workflowTimerMode) observeState {
	h.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		state := h.state()
		if state.mode == want {
			return state
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timer mode = %v, want %v", state.mode, want)
			return state
		}
		time.Sleep(time.Millisecond)
	}
}

func (h *observeHarness) refuteSend(reason string) {
	h.t.Helper()
	select {
	case message := <-h.sends:
		h.t.Fatalf("%s: sent %q", reason, message)
	default:
	}
}

// refuteSendWithin is the same statement about a send that is already dispatched
// on its own goroutine: the window has to be long enough that a send which was
// going to happen would have happened.
func (h *observeHarness) refuteSendWithin(reason string, window time.Duration) {
	h.t.Helper()
	select {
	case message := <-h.sends:
		h.t.Fatalf("%s: sent %q", reason, message)
	case <-time.After(window):
	}
}

func (h *observeHarness) awaitOutcome() engine.Outcome {
	h.t.Helper()
	select {
	case outcome := <-h.outcomes:
		return outcome
	case <-time.After(2 * time.Second):
		h.t.Fatal("the attempt never completed")
		return engine.Outcome{}
	}
}

func (h *observeHarness) refuteOutcome(reason string) {
	h.t.Helper()
	select {
	case outcome := <-h.outcomes:
		h.t.Fatalf("%s: outcome %+v", reason, outcome)
	default:
	}
}

// awaitSend drains the retry `timerFired` dispatches, so an assertion that
// follows cannot race the goroutine that carries it.
func (h *observeHarness) awaitSend() string {
	h.t.Helper()
	select {
	case message := <-h.sends:
		return message
	case <-time.After(2 * time.Second):
		h.t.Fatal("the scheduled retry never sent")
		return ""
	}
}

// driveToFirstBackoffRung takes a Codex attempt through one whole transient
// cycle: a turn, an informational error, and the `turn/completed` that is the
// terminal signal the ladder actually arms on.
func (h *observeHarness) driveToFirstBackoffRung() {
	h.t.Helper()
	h.observe(provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: "turn-1"})
	h.observe(provider.ProviderEvent{
		Kind: provider.EventError, Content: "server overloaded", Meta: json.RawMessage(codexOverloadedMeta),
		Failure: &provider.FailureMeta{Class: provider.FailureTransient, Boundary: provider.FailureBoundaryTurn},
	})
	h.observe(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, TurnID: "turn-1",
		TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "error", ErrorMessage: "server overloaded"},
	})
	if state := h.state(); state.mode != workflowTimerBackoff || state.retryCount != 1 {
		h.t.Fatalf("state = %+v, want the first backoff rung armed", state)
	}
}

// The incident, replayed. A collab child's own error may not be read as the
// parent turn's failure — the child's turn boundaries never reach this machine
// at all, so nothing parented can be answering for the parent — and the real
// turn's completion has to still be the one that finishes the phase.
func TestWorkflowCollabChildErrorNeverArmsTheParentRetryLadder(t *testing.T) {
	harness := newObserveHarness(t, string(provider.Codex))
	harness.observe(provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: "turn-1"})
	armed := harness.state()
	if armed.mode != workflowTimerWatchdog || armed.timer == nil {
		t.Fatalf("turn start armed %+v, want the inactivity watchdog", armed)
	}

	childError := codexOverloadedEvent("collab-child")
	childError.Content = "stream error: server overloaded"
	harness.observe(childError)

	after := harness.state()
	if after.mode != workflowTimerWatchdog || after.timer != armed.timer {
		t.Fatalf("child error replaced the parent's timer: %+v", after)
	}
	if after.retryCount != 0 || after.pendingTransient || after.awaitingTurnStart {
		t.Fatalf("child error entered the parent's retry ladder: %+v", after)
	}
	if !after.turnStarted || after.currentTurnID != "turn-1" {
		t.Fatalf("child error disturbed the parent's turn identity: %+v", after)
	}
	if after.lastFailure != "" {
		t.Fatalf("child error was recorded as the parent's failure: %q", after.lastFailure)
	}

	harness.observe(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, TurnID: "turn-1", StructuredOutput: workflowValidEnvelope,
	})
	if outcome := harness.awaitOutcome(); outcome.Kind != engine.OutcomeDone {
		t.Fatalf("the real turn's completion produced %+v, want the envelope outcome", outcome)
	}
}

// Child events are the parent turn's ACTIVITY: a delegating turn leaves the
// parent stream quiet for as long as its children work, and that quiet must not
// read as a stall. The one place it buys nothing is the wait for a retry's first
// turn, which is a hard deadline by design.
func TestWorkflowChildActivityFeedsTheWatchdogButNotTheRetryWait(t *testing.T) {
	harness := newObserveHarness(t, string(provider.Codex))
	harness.observe(provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: "turn-1"})
	started := harness.state()

	harness.now = harness.now.Add(9 * time.Minute)
	harness.observe(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ParentToolUseID: "collab-child", Content: "child is working",
	})

	fed := harness.state()
	if want := harness.now.Add(harness.attempt.watchdog); !fed.deadline.Equal(want) {
		t.Fatalf("watchdog deadline = %v, want %v — a delegating turn would false-stall", fed.deadline, want)
	}
	if len(fed.timer.resets) != 1 || fed.timer.resets[0] != harness.attempt.watchdog {
		t.Fatalf("watchdog resets = %v, want one full window", fed.timer.resets)
	}
	if fed.timer != started.timer {
		t.Fatal("child activity re-armed rather than reset the watchdog")
	}

	// Now the same event while the attempt is waiting for a retry's first turn.
	harness.observe(codexOverloadedEvent(""))
	harness.observe(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, TurnID: "turn-1",
		TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "error", ErrorMessage: "server overloaded"},
	})
	harness.fire(0)
	harness.awaitSend()
	waiting := harness.state()
	if !waiting.awaitingTurnStart || waiting.mode != workflowTimerWatchdog {
		t.Fatalf("state after the backoff fired = %+v, want a bounded wait for the retry's turn", waiting)
	}

	harness.now = harness.now.Add(time.Minute)
	harness.observe(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ParentToolUseID: "collab-child", Content: "child is still working",
	})
	held := harness.state()
	if !held.awaitingTurnStart {
		t.Fatal("a child event answered the turn start the retry was waiting for")
	}
	if !held.deadline.Equal(waiting.deadline) {
		t.Fatalf("a child event pushed out the retry's deadline: %v -> %v", waiting.deadline, held.deadline)
	}
}

// A child's usage-limit refusal is the CHILD's account being spent, not proof
// that the parent's turn can no longer run. Parking the whole run on it stops a
// turn that is still working.
func TestWorkflowChildQuotaRefusalDoesNotParkTheRun(t *testing.T) {
	harness := newObserveHarness(t, string(provider.Codex))
	harness.observe(provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: "turn-1"})

	harness.observe(provider.ProviderEvent{
		Kind: provider.EventError, ParentToolUseID: "collab-child",
		Content: "usage limit reached", Meta: json.RawMessage(codexUsageLimitMeta),
	})

	harness.refuteOutcome("a child's usage limit parked the parent run")
	state := harness.state()
	if !state.installed || state.mode != workflowTimerWatchdog || !state.turnStarted {
		t.Fatalf("state after a child usage limit = %+v, want the parent turn still live", state)
	}
}

// A codex `error` notification is informational; `turn/completed` is the
// terminal signal core always emits. Deferring to it is what keeps the ladder
// from arming while the turn is alive — the arming that let a retry be absorbed
// into the live turn as queued input.
func TestWorkflowCodexTransientArmsTheLadderOnlyAtTheTurnBoundary(t *testing.T) {
	harness := newObserveHarness(t, string(provider.Codex))
	harness.observe(provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: "turn-1"})
	live := harness.state()

	harness.observe(codexOverloadedEvent(""))
	held := harness.state()
	if !held.pendingTransient {
		t.Fatalf("codex error did not defer to the turn's own completion: %+v", held)
	}
	if held.mode != workflowTimerWatchdog || held.timer != live.timer || held.retryCount != 0 {
		t.Fatalf("codex error touched the timers while the turn was alive: %+v", held)
	}

	harness.observe(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, TurnID: "turn-1",
		TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "error", ErrorMessage: "server overloaded"},
	})
	scheduled := harness.state()
	if scheduled.mode != workflowTimerBackoff || scheduled.timer.delay != 30*time.Second {
		t.Fatalf("the terminal event armed %+v, want the first backoff rung", scheduled)
	}
	if scheduled.retryCount != 1 || scheduled.pendingTransient {
		t.Fatalf("ladder state after the terminal event = %+v", scheduled)
	}
	// The turn's identity ends with the turn. A retry that inherited it would
	// have its own completion filtered as a ghost.
	if scheduled.currentTurnID != "" {
		t.Fatalf("the spent turn's id survived the ladder: %q", scheduled.currentTurnID)
	}

	harness.fire(0)
	harness.awaitSend()
	harness.observe(provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: "turn-2"})
	harness.observe(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, TurnID: "turn-2", StructuredOutput: workflowValidEnvelope,
	})
	if outcome := harness.awaitOutcome(); outcome.Kind != engine.OutcomeDone {
		t.Fatalf("the retry's own turn produced %+v, want the envelope outcome", outcome)
	}
}

// The self-correcting half of the same rule: an error that turned out not to be
// terminal is answered by a completion carrying an envelope, and the attempt
// takes the ordinary envelope path rather than a retry it no longer needs.
func TestWorkflowCodexTransientYieldsToACompletionThatCarriesAnEnvelope(t *testing.T) {
	harness := newObserveHarness(t, string(provider.Codex))
	harness.observe(provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: "turn-1"})
	harness.observe(codexOverloadedEvent(""))

	harness.observe(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, TurnID: "turn-1", StructuredOutput: workflowValidEnvelope,
	})

	if outcome := harness.awaitOutcome(); outcome.Kind != engine.OutcomeDone {
		t.Fatalf("outcome = %+v, want the envelope the turn actually produced", outcome)
	}
}

// A retry's turn is not guaranteed to start: a send that reaches a session whose
// turn is somehow still alive is queued into that turn rather than starting one.
// The wait is therefore bounded by the watchdog, and an unanswered retry parks
// `stalled` — loud and repairable — instead of running forever.
func TestWorkflowRetryThatNeverStartsParksStalled(t *testing.T) {
	harness := newObserveHarness(t, string(provider.Codex))
	harness.driveToFirstBackoffRung()

	// The bound is taken as the retry is dispatched, not when its send lands.
	// Holding the send lock parks the goroutine `timerFired` spawns exactly where
	// production would park it behind a slow provider write, and the watchdog has
	// to be armed already — a bound that only appears once the send returns
	// leaves the whole window between them unarmed.
	harness.attempt.sendMu.Lock()
	harness.fire(0)
	dispatched := harness.state()
	harness.attempt.sendMu.Unlock()
	if dispatched.mode != workflowTimerWatchdog || !dispatched.awaitingTurnStart {
		t.Fatalf("state at retry dispatch = %+v, want the wait already bounded", dispatched)
	}

	harness.awaitSend()
	waiting := harness.state()
	if !waiting.awaitingTurnStart || waiting.mode != workflowTimerWatchdog || waiting.timer == nil {
		t.Fatalf("state after the backoff fired = %+v, want the retry wait under a watchdog", waiting)
	}

	harness.fire(0)
	if outcome := harness.awaitOutcome(); outcome.Kind != engine.OutcomeStalled {
		t.Fatalf("outcome = %+v, want the stall park", outcome)
	}
}

// A session that reports `error` and then goes quiet used to leave the attempt
// with no timer at all, waiting on a disconnect nothing promises. The watchdog
// is what makes that shape end.
func TestWorkflowSessionErrorWithoutADisconnectParksStalled(t *testing.T) {
	harness := newObserveHarness(t, string(provider.Claude))
	harness.observe(provider.ProviderEvent{Kind: provider.EventInit})

	harness.observe(provider.ProviderEvent{Kind: provider.EventSessionStatus, Content: "error"})

	latched := harness.state()
	if !latched.pendingSessionDeath || latched.turnStarted {
		t.Fatalf("state after a session error = %+v, want the death latched and the turn ended", latched)
	}
	if latched.mode != workflowTimerWatchdog || latched.timer == nil {
		t.Fatalf("a session error left %+v armed, want the watchdog", latched)
	}

	harness.fire(0)
	if outcome := harness.awaitOutcome(); outcome.Kind != engine.OutcomeStalled {
		t.Fatalf("outcome = %+v, want the stall park", outcome)
	}
}

func TestWorkflowClaude529SequenceEntersTheTransientLadder(t *testing.T) {
	harness := newObserveHarness(t, string(provider.Claude))
	harness.observe(provider.ProviderEvent{Kind: provider.EventInit})
	for attempt := 1; attempt <= 10; attempt++ {
		event := provider.ProviderEvent{
			Kind:    provider.EventAPIRetry,
			Failure: &provider.FailureMeta{Class: provider.FailureTransient},
			Meta: json.RawMessage(fmt.Sprintf(
				`{"attempt":%d,"max_retries":10,"wire":{"error_status":529,"error":"rate_limit"}}`, attempt,
			)),
		}
		harness.observe(event)
	}
	if state := harness.state(); !state.claudeRetryable {
		t.Fatalf("529 api_retry sequence state = %+v, want the typed retry precursor latched", state)
	}
	harness.observe(provider.ProviderEvent{
		Kind: provider.EventError,
		Failure: &provider.FailureMeta{
			Class: provider.FailureTransientAfterRetry, Boundary: provider.FailureBoundaryTurn,
		},
		Meta: json.RawMessage(`{"api_error_enum":"server_error","fatal":true,"expect_turn_complete":true}`),
	})
	if state := harness.state(); !state.pendingTransient {
		t.Fatalf("529 server_error state = %+v, want the terminal result awaited as transient", state)
	}
	harness.observe(provider.ProviderEvent{
		Kind: provider.EventTurnComplete,
		Raw:  json.RawMessage(`{"type":"result","is_error":true,"terminal_reason":"api_error","api_error_status":529}`),
		TurnComplete: &provider.WireTurnCompleteMeta{
			StopReason: "error", ErrorMessage: "API Error: 529 Overloaded",
		},
	})
	state := harness.state()
	if state.mode != workflowTimerBackoff || state.retryCount != 1 {
		t.Fatalf("529 completion state = %+v, want the first transient backoff", state)
	}
	harness.refuteOutcome("a 529 was classified as agent-error")
}

// The backoff guard exists to stop a retrying attempt from ACTING on a delayed
// event, not to stop it from LISTENING. A session that dies inside the backoff
// window has to reach the ladder: without the latch the resend the backoff is
// holding lands in a session that has already been reaped, and a transient
// failure becomes an `agent-error` park.
func TestWorkflowSessionDeathDuringBackoffStillReachesTheLadder(t *testing.T) {
	harness := newObserveHarness(t, string(provider.Codex))
	harness.driveToFirstBackoffRung()
	backoff := harness.state()

	harness.observe(provider.ProviderEvent{Kind: provider.EventSessionStatus, Content: "error"})
	latched := harness.state()
	if !latched.pendingSessionDeath {
		t.Fatal("a session death inside the backoff window was dropped")
	}
	if latched.mode != workflowTimerBackoff || latched.timer != backoff.timer {
		t.Fatalf("the pending backoff was disturbed by the session death: %+v", latched)
	}

	harness.runner.sessionDisconnected("thread")

	harness.refuteOutcome("the ladder ended on a death it should have absorbed")
	next := harness.state()
	if next.mode != workflowTimerBackoff || next.timer == backoff.timer || next.timer.delay != 2*time.Minute {
		t.Fatalf("state after the disconnect = %+v, want the next rung armed", next)
	}
	if next.retryCount != 2 || next.pendingSessionDeath {
		t.Fatalf("ladder state after the disconnect = %+v", next)
	}
	if backoff.timer.active {
		t.Fatal("the superseded backoff timer was left armed")
	}
}

// The gate is the turn-completion promise, not the presence of a parent id.
// Claude stamps a Task subagent's id on that subagent's `assistant.error`, but
// the error is the PARENT's open turn failing and the CLI closes that turn with
// the `result{is_error}` the flag promises. Codex forwards a collab child's own
// error with `fatal:false` and no promise, because it is the child's alone.
func TestWorkflowParentedErrorGateReadsTheTurnCompletionPromise(t *testing.T) {
	for _, test := range []struct {
		name  string
		event provider.ProviderEvent
		want  bool
	}{
		{name: "claude subagent assistant error", want: true, event: claudeRateLimitedEvent("toolu_subagent")},
		{name: "codex collab child error", event: codexOverloadedEvent("collab-child")},
		{name: "fatal without the promise", event: provider.ProviderEvent{
			Kind: provider.EventError, ParentToolUseID: "collab-child",
			Meta: json.RawMessage(`{"api_error_enum":"rate_limit","fatal":true}`),
		}},
		{name: "not an error event", event: provider.ProviderEvent{
			Kind: provider.EventTextDelta, ParentToolUseID: "collab-child",
			Meta: json.RawMessage(claudeRateLimitedMeta),
		}},
		{name: "no meta", event: provider.ProviderEvent{Kind: provider.EventError, ParentToolUseID: "x"}},
		{name: "malformed meta", event: provider.ProviderEvent{
			Kind: provider.EventError, ParentToolUseID: "x", Meta: json.RawMessage(`{`),
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := workflowParentedErrorClosesTurn(test.event); got != test.want {
				t.Fatalf("workflowParentedErrorClosesTurn = %v, want %v", got, test.want)
			}
		})
	}
}

// A Claude subagent error scoped to the parent turn belongs to the phase turn:
// an ordinary transient still waits for the provider's completion boundary.
// Gating it out would downgrade a retryable parent failure into a hard
// execution failure.
func TestWorkflowClaudeSubagentParentTurnTransientEntersTheWait(t *testing.T) {
	harness := newObserveHarness(t, string(provider.Claude))
	harness.observe(provider.ProviderEvent{Kind: provider.EventInit})
	live := harness.state()

	harness.observe(provider.ProviderEvent{
		Kind: provider.EventError, ParentToolUseID: "toolu_subagent",
		Content: "provider overloaded",
		Failure: &provider.FailureMeta{
			Class: provider.FailureTransient, Boundary: provider.FailureBoundaryTurn,
			Scope: provider.FailureScopeParentTurn, Code: "overloaded",
		},
	})

	harness.refuteOutcome("a parent-turn transient ended the phase before its completion boundary")
	state := harness.state()
	if !state.pendingTransient {
		t.Fatalf("state after a subagent error = %+v, want the wait for the turn's own completion", state)
	}
	if state.retryCount != 0 || state.mode != workflowTimerWatchdog || state.timer != live.timer {
		t.Fatalf("state after a subagent error = %+v, want the ladder unarmed and the turn alive", state)
	}
	if state.lastFailure == "" {
		t.Fatal("the parent turn's failure was not recorded as the attempt's account")
	}
}

// The typed usage-limit park keys off the same normalized enum, so gating the
// error out would silently spend retries for every delegating Claude phase.
func TestWorkflowClaudeSubagentRateLimitStillReachesTheQuotaPark(t *testing.T) {
	harness := newObserveHarness(t, string(provider.Claude))
	harness.observe(provider.ProviderEvent{Kind: provider.EventInit})

	harness.observe(claudeRateLimitedEvent("toolu_subagent"))

	outcome := harness.awaitOutcome()
	if outcome.Kind != engine.OutcomeProviderUsageLimited ||
		!strings.Contains(outcome.Detail, "provider usage limit reached") {
		t.Fatalf("outcome = %+v, want the typed usage-limit park rather than the generic ladder", outcome)
	}
}

// A turn start for a turn that already completed is a replay: the Codex adapter
// drops a claimed turn id AT completion, so its start-side dedupe cannot catch
// one. Adopting it would retarget the attempt at the dead turn and drop the live
// one's completion as a mismatch.
func TestWorkflowReplayedTurnStartDoesNotRetargetTheLiveTurn(t *testing.T) {
	harness := newObserveHarness(t, string(provider.Codex))
	harness.observe(provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: "turn-A"})
	live := harness.state()
	harness.observe(codexOverloadedEvent(""))

	harness.observe(provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: "turn-B"})

	replayed := harness.state()
	if replayed.currentTurnID != "turn-A" || !replayed.turnStarted {
		t.Fatalf("a replayed turn start retargeted the attempt: %+v", replayed)
	}
	if !replayed.pendingTransient {
		t.Fatalf("a replayed turn start cleared the live turn's pending failure: %+v", replayed)
	}
	if replayed.mode != workflowTimerWatchdog || replayed.timer != live.timer {
		t.Fatalf("a replayed turn start re-armed the live turn's watchdog: %+v", replayed)
	}

	harness.observe(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, TurnID: "turn-A", StructuredOutput: workflowValidEnvelope,
	})
	if outcome := harness.awaitOutcome(); outcome.Kind != engine.OutcomeDone {
		t.Fatalf("the live turn's completion produced %+v, want the envelope outcome", outcome)
	}
}

// Every door into a provider turn has to leave a timer behind. Codex announces
// its own turn, so the send is not a start — but the wait for that announcement
// is unbounded without one, which is the retry-path hang on the opening door.
func TestWorkflowCodexOpeningSendArmsTheWatchdogUntilTheTurnAnnouncesItself(t *testing.T) {
	harness := newObserveHarness(t, string(provider.Codex))

	if drop, err := harness.send(harness.attempt.currentPrompt, harness.epoch()); drop != workflowSendNotDropped || err != nil {
		t.Fatalf("sendIfActive = (%q, %v)", drop, err)
	}
	harness.awaitSend()

	armed := harness.waitForMode(workflowTimerWatchdog)
	if armed.turnStarted || armed.currentTurnID != "" {
		t.Fatalf("the send claimed a turn Codex has not announced: %+v", armed)
	}
	if !armed.awaitingTurnStart {
		t.Fatalf("the send did not wait for the turn it started: %+v", armed)
	}

	harness.fire(0)
	if outcome := harness.awaitOutcome(); outcome.Kind != engine.OutcomeStalled {
		t.Fatalf("outcome = %+v, want the stall park", outcome)
	}
}

// The window the wait exists for: until `turn/started` names this send's turn,
// `currentTurnID` is empty and the identity filter is inert, so a `thread/read`
// replay of an EARLIER turn's completion would settle the attempt on an envelope
// that answers a prompt nobody sent.
func TestWorkflowOpeningSendWaitsForItsOwnTurnBeforeConsumingACompletion(t *testing.T) {
	harness := newObserveHarness(t, string(provider.Codex))
	if drop, err := harness.send(harness.attempt.currentPrompt, harness.epoch()); drop != workflowSendNotDropped || err != nil {
		t.Fatalf("sendIfActive = (%q, %v)", drop, err)
	}
	harness.awaitSend()
	harness.waitForMode(workflowTimerWatchdog)

	harness.observe(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, TurnID: "turn-old", StructuredOutput: workflowValidEnvelope,
	})

	harness.refuteOutcome("a replayed completion settled the attempt before its own turn started")
	if state := harness.state(); !state.installed || !state.awaitingTurnStart {
		t.Fatalf("state after the replay = %+v, want the attempt still waiting", state)
	}

	harness.observe(provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: "turn-1"})
	harness.observe(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, TurnID: "turn-1", StructuredOutput: workflowValidEnvelope,
	})
	if outcome := harness.awaitOutcome(); outcome.Kind != engine.OutcomeDone {
		t.Fatalf("the send's own turn produced %+v, want the envelope outcome", outcome)
	}
}

// The turn start for a send can reach the event goroutine before the send's own
// post-send block runs — the two serialize on the runner lock, so which side
// wins is a race. Waiting then would eat the completion of the very turn the
// wait was meant to protect.
func TestWorkflowSendDoesNotWaitForATurnThatAlreadyStarted(t *testing.T) {
	harness := newObserveHarness(t, string(provider.Codex))
	harness.app.sendMessageFn = func(_, content string, _ []string) error {
		harness.sends <- content
		harness.observe(provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: "turn-1"})
		return nil
	}

	if drop, err := harness.send(harness.attempt.currentPrompt, harness.epoch()); drop != workflowSendNotDropped || err != nil {
		t.Fatalf("sendIfActive = (%q, %v)", drop, err)
	}
	harness.awaitSend()

	state := harness.state()
	if state.awaitingTurnStart {
		t.Fatalf("the send waited for a turn that had already announced itself: %+v", state)
	}
	if !state.turnStarted || state.currentTurnID != "turn-1" {
		t.Fatalf("state after the raced turn start = %+v", state)
	}

	harness.observe(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, TurnID: "turn-1", StructuredOutput: workflowValidEnvelope,
	})
	if outcome := harness.awaitOutcome(); outcome.Kind != engine.OutcomeDone {
		t.Fatalf("outcome = %+v, want the envelope outcome", outcome)
	}
}

// Claude names no turn, so it has none to wait for: its send IS its start, and
// a completion carrying no turn id has to be consumable immediately.
func TestWorkflowClaudeSendNeverWaitsForATurnItCannotName(t *testing.T) {
	harness := newObserveHarness(t, string(provider.Claude))

	if drop, err := harness.send(harness.attempt.currentPrompt, harness.epoch()); drop != workflowSendNotDropped || err != nil {
		t.Fatalf("sendIfActive = (%q, %v)", drop, err)
	}
	harness.awaitSend()

	state := harness.waitForMode(workflowTimerWatchdog)
	if state.awaitingTurnStart || !state.turnStarted {
		t.Fatalf("state after a Claude send = %+v, want the send read as the start", state)
	}

	harness.observe(provider.ProviderEvent{Kind: provider.EventTurnComplete, StructuredOutput: workflowValidEnvelope})
	if outcome := harness.awaitOutcome(); outcome.Kind != engine.OutcomeDone {
		t.Fatalf("outcome = %+v, want the envelope outcome", outcome)
	}
}

// A session death during the wait still reaches the ladder: the flag is cleared
// by the same sub-branch whichever producer set it.
func TestWorkflowSessionErrorClearsTheWaitASendOpened(t *testing.T) {
	harness := newObserveHarness(t, string(provider.Codex))
	if drop, err := harness.send(harness.attempt.currentPrompt, harness.epoch()); drop != workflowSendNotDropped || err != nil {
		t.Fatalf("sendIfActive = (%q, %v)", drop, err)
	}
	harness.awaitSend()
	harness.waitForMode(workflowTimerWatchdog)

	harness.observe(provider.ProviderEvent{Kind: provider.EventSessionStatus, Content: "error"})

	latched := harness.state()
	if latched.awaitingTurnStart || !latched.pendingSessionDeath {
		t.Fatalf("state after a session error during the wait = %+v", latched)
	}

	harness.runner.sessionDisconnected("thread")
	next := harness.state()
	if next.mode != workflowTimerBackoff || next.retryCount != 1 {
		t.Fatalf("state after the disconnect = %+v, want the ladder to take it", next)
	}
}

// A queued send belongs to the ladder state that queued it. The latch alone
// cannot say so: a death that is latched AND answered leaves the attempt
// installed and healthy in a NEW state, whose own resend is pending. Delivering
// the old one then starts a turn inside somebody else's backoff window.
func TestWorkflowSupersededResendIsDroppedWhenTheLadderAdvances(t *testing.T) {
	harness := newObserveHarness(t, string(provider.Codex))
	harness.driveToFirstBackoffRung()
	firstRung := harness.epoch()

	// Park the dispatched resend where a slow provider write would park it, and
	// let the whole death-and-answer cycle run underneath it.
	harness.attempt.sendMu.Lock()
	harness.fire(0)
	harness.observe(provider.ProviderEvent{Kind: provider.EventSessionStatus, Content: "error"})
	harness.runner.sessionDisconnected("thread")

	advanced := harness.state()
	if advanced.mode != workflowTimerBackoff || advanced.retryCount != 2 || advanced.pendingSessionDeath {
		t.Fatalf("state after the disconnect = %+v, want the next rung armed and the latch answered", advanced)
	}
	if advanced.sendEpoch == firstRung {
		t.Fatal("advancing the ladder did not retire what the previous rung queued")
	}
	harness.attempt.sendMu.Unlock()

	harness.refuteSendWithin("a superseded resend reached the provider", 250*time.Millisecond)

	// The rung that IS current owns the wire, and sends exactly once.
	harness.fire(0)
	harness.awaitSend()
	harness.refuteSendWithin("the current rung's send was doubled", 100*time.Millisecond)
}

// The same door, one turn later: a completion the contract rejects spends the
// envelope retry and sends again, and that send is the only thing standing
// between the attempt and an unarmed wait.
func TestWorkflowCodexEnvelopeFeedbackSendArmsTheWatchdog(t *testing.T) {
	harness := newObserveHarness(t, string(provider.Codex))
	harness.observe(provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: "turn-1"})

	// A payload the contract rejects: present, so it is not the empty-turn shape,
	// and missing `status`, which every envelope owes.
	harness.observe(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, TurnID: "turn-1", StructuredOutput: json.RawMessage(`{}`),
	})
	harness.awaitSend()

	armed := harness.waitForMode(workflowTimerWatchdog)
	if armed.turnStarted || armed.currentTurnID != "" {
		t.Fatalf("the feedback send claimed a turn Codex has not announced: %+v", armed)
	}
	if !armed.awaitingTurnStart {
		t.Fatalf("the feedback send did not wait for the turn it started: %+v", armed)
	}

	harness.fire(0)
	if outcome := harness.awaitOutcome(); outcome.Kind != engine.OutcomeStalled {
		t.Fatalf("outcome = %+v, want the stall park", outcome)
	}
}

// A latched session death outranks the resend the backoff is holding:
// `sessionDisconnected` owns the next rung, and firing the send would land it in
// a process being reaped.
func TestWorkflowLatchedSessionDeathSuppressesTheHeldResend(t *testing.T) {
	harness := newObserveHarness(t, string(provider.Codex))
	harness.driveToFirstBackoffRung()

	harness.observe(provider.ProviderEvent{Kind: provider.EventSessionStatus, Content: "error"})
	harness.fire(0)

	harness.refuteSend("the held resend was fired into a dying session")
	held := harness.state()
	if held.mode != workflowTimerWatchdog || held.awaitingTurnStart || !held.pendingSessionDeath {
		t.Fatalf("state after the backoff fired on a dead session = %+v", held)
	}
	if held.retryCount != 1 {
		t.Fatalf("the suppressed resend spent a rung: %+v", held)
	}

	harness.runner.sessionDisconnected("thread")

	harness.refuteOutcome("the ladder ended on a death it should have absorbed")
	next := harness.state()
	if next.mode != workflowTimerBackoff || next.timer.delay != 2*time.Minute || next.retryCount != 2 {
		t.Fatalf("state after the disconnect = %+v, want the next rung armed", next)
	}
}

// The disconnect is not guaranteed to arrive, so the suppression is bounded by
// the same watchdog rather than by hope.
func TestWorkflowLatchedSessionDeathWithoutADisconnectParksStalled(t *testing.T) {
	harness := newObserveHarness(t, string(provider.Codex))
	harness.driveToFirstBackoffRung()

	harness.observe(provider.ProviderEvent{Kind: provider.EventSessionStatus, Content: "error"})
	harness.fire(0)
	harness.fire(0)

	if outcome := harness.awaitOutcome(); outcome.Kind != engine.OutcomeStalled {
		t.Fatalf("outcome = %+v, want the stall park", outcome)
	}
}

// The send chokepoint enforces the same rule for a goroutine that was already in
// flight when the death latched: a dying session is as inactive as a cancelled
// attempt.
func TestWorkflowSendIsInactiveOnceASessionDeathIsLatched(t *testing.T) {
	harness := newObserveHarness(t, string(provider.Codex))
	harness.runner.mu.Lock()
	harness.attempt.pendingSessionDeath = true
	harness.runner.mu.Unlock()

	drop, err := harness.send("retry", harness.epoch())

	if drop != workflowSendDropSessionDeath || err != nil {
		t.Fatalf("sendIfActive = (%q, %v), want the latched session death named", drop, err)
	}
	harness.refuteSend("a queued send reached a session already known dead")
}

// `thread/read` replays re-emit turn lifecycle. The started side already claims
// each turn id once; this is its completed-side symmetry, without which a
// replayed completion of an earlier turn finishes the phase on a stale envelope.
func TestWorkflowReplayedCompletionOfAnotherTurnIsIgnored(t *testing.T) {
	harness := newObserveHarness(t, string(provider.Codex))
	harness.observe(provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: "turn-A"})
	live := harness.state()

	harness.observe(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, TurnID: "turn-B", StructuredOutput: workflowValidEnvelope,
	})

	harness.refuteOutcome("a replayed completion of another turn finished the phase")
	ghosted := harness.state()
	if !ghosted.installed || !ghosted.turnStarted || ghosted.currentTurnID != "turn-A" {
		t.Fatalf("state after a ghost completion = %+v, want the live turn untouched", ghosted)
	}
	if ghosted.mode != workflowTimerWatchdog || ghosted.timer != live.timer {
		t.Fatalf("a ghost completion disarmed the live turn's watchdog: %+v", ghosted)
	}

	harness.observe(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, TurnID: "turn-A", StructuredOutput: workflowValidEnvelope,
	})
	if outcome := harness.awaitOutcome(); outcome.Kind != engine.OutcomeDone {
		t.Fatalf("the attempt's own completion produced %+v, want the envelope outcome", outcome)
	}
}

// Claude names no turn, so the filter has to be inert for it rather than
// filtering every completion out.
func TestWorkflowUnnamedTurnCompletionIsNeverFiltered(t *testing.T) {
	harness := newObserveHarness(t, string(provider.Claude))
	harness.observe(provider.ProviderEvent{Kind: provider.EventInit})
	if state := harness.state(); state.currentTurnID != "" {
		t.Fatalf("Claude's turn start named a turn: %q", state.currentTurnID)
	}

	harness.observe(provider.ProviderEvent{Kind: provider.EventTurnComplete, StructuredOutput: workflowValidEnvelope})

	if outcome := harness.awaitOutcome(); outcome.Kind != engine.OutcomeDone {
		t.Fatalf("outcome = %+v, want the envelope outcome", outcome)
	}
}

// The disconnected branch used to call `finish` synchronously on the provider
// read-loop goroutine, and `finish` barriers on the attempt's send lock. A send
// can hold that lock while it WAITS on the read loop — an in-send reconnect
// blocks in `Session.Close` until the read loop exits, and a dispatched send
// waits for a JSON-RPC reply only the read loop can deliver — so the two
// deadlocked each other permanently, with the thread action lock held under
// them (the incident class of 2026-08-15). `observe` must return without
// taking the send lock; the completion lands off-wire once the send drains.
func TestObserveDisconnectReturnsWhileASendHoldsTheSendLock(t *testing.T) {
	h := newObserveHarness(t, string(provider.Codex))
	h.attempt.sendMu.Lock() // an in-flight send, from observe's point of view

	returned := make(chan struct{})
	go func() {
		h.observe(provider.ProviderEvent{Kind: provider.EventSessionStatus, Content: "disconnected"})
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("observe blocked on the send lock; the read loop would never exit and the send would never drain")
	}

	// The claim is synchronous even though the completion is not: an event
	// behind the disconnect finds no attempt.
	h.runner.mu.Lock()
	_, still := h.runner.runs[h.runKey]
	h.runner.mu.Unlock()
	if still {
		t.Fatal("the disconnect did not detach the attempt on the wire")
	}
	// The completion waits for the send to drain; it must not land under it.
	h.refuteOutcome("a completion landed while the send still held the send lock")

	h.attempt.sendMu.Unlock()
	outcome := h.awaitOutcome()
	if outcome.Kind != engine.OutcomeExecutionFailure {
		t.Fatalf("outcome = %+v, want the execution failure a disconnect parks as", outcome)
	}
}
