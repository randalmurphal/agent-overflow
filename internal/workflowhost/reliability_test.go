package workflowhost

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/profile"
)

func TestWorkflowReliabilityResolutionPrecedenceAndDefaults(t *testing.T) {
	runner := newTestRunner(t, nil, nil, staticWorkflowProfileSource{value: &profile.Profile{}})
	request := engine.RunRequest{
		Item: store.WorkItem{ProjectID: "project"}, Phase: def.Phase{ID: "phase"},
		Launch: engine.FreshTurn(),
	}
	watchdog, backoff, err := runner.reliability(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if watchdog != defaultWorkflowWatchdog || len(backoff) != 3 || backoff[0] != 30*time.Second || backoff[1] != 2*time.Minute || backoff[2] != 5*time.Minute {
		t.Fatalf("defaults = watchdog %v backoff %v", watchdog, backoff)
	}

	projectWatchdog := profile.Duration("2m")
	runner.profiles = staticWorkflowProfileSource{value: &profile.Profile{Reliability: profile.ReliabilityDefaults{
		Watchdog: projectWatchdog,
		Backoff:  []profile.Duration{"1s"},
	}}}
	request.Phase.Watchdog = "3s"
	watchdog, backoff, err = runner.reliability(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if watchdog != 3*time.Second || len(backoff) != 1 || backoff[0] != time.Second {
		t.Fatalf("overrides = watchdog %v backoff %v", watchdog, backoff)
	}
}

func TestFailedTakeoverStopRestoresBackoffTimer(t *testing.T) {
	runner := newTestRunner(t, nil, nil, staticWorkflowProfileSource{value: &profile.Profile{}})
	// The interrupt is what fails here: production reaches a session whose
	// provider it cannot name. What the test is about is the state the refusal
	// leaves behind, not which refusal it was.
	runner.interrupt = func(context.Context, string) error {
		return errors.New("unknown provider")
	}
	now := time.Unix(100, 0)
	runner.now = func() time.Time { return now }
	var restored *fakeWorkflowTimer
	runner.newTimer = func(delay time.Duration, callback func()) Timer {
		restored = &fakeWorkflowTimer{callback: callback, delay: delay, active: true}
		return restored
	}
	key := engine.RunKey{ItemID: "item", PhaseID: "phase", Attempt: 1}
	runKey := workflowRunKey(key)
	deadline := now.Add(45 * time.Second)
	originalTimer := &fakeWorkflowTimer{active: true}
	attempt := &workflowAttempt{
		workflowCompletion: workflowCompletion{key: key},
		threadID:           "thread", schema: json.RawMessage(`{"type":"object"}`),
		watchdog: time.Minute, timer: originalTimer, timerMode: workflowTimerBackoff,
		timerDeadline: deadline, unsubscribe: func() {},
	}
	runner.runs[runKey] = attempt
	runner.schemas[attempt.threadID] = attempt.schema
	runner.workItems[attempt.threadID] = key.ItemID

	if _, err := runner.StopForTakeover(t.Context(), key); err == nil {
		t.Fatal("StopForTakeover error = nil, want missing provider error")
	}
	runner.mu.Lock()
	restoredAttempt := runner.runs[runKey]
	runner.mu.Unlock()
	if restoredAttempt != attempt || attempt.timerMode != workflowTimerBackoff || !attempt.timerDeadline.Equal(deadline) {
		t.Fatalf("restored attempt timer = mode %v deadline %v", attempt.timerMode, attempt.timerDeadline)
	}
	if originalTimer.active || restored == nil || restored.delay != 45*time.Second || !restored.active {
		t.Fatalf("timer restore originalActive=%v restored=%+v", originalTimer.active, restored)
	}
}

func TestWorkflowTransientSignalAllowlist(t *testing.T) {
	tests := []struct {
		name          string
		failure       *provider.FailureMeta
		providerRetry bool
		transient     bool
		waits         bool
	}{
		{name: "missing classification"},
		{name: "fatal", failure: &provider.FailureMeta{Class: provider.FailureFatal, Boundary: provider.FailureBoundaryEvent}},
		{name: "transient immediate", failure: &provider.FailureMeta{Class: provider.FailureTransient}, transient: true},
		{name: "transient after turn", failure: &provider.FailureMeta{Class: provider.FailureTransient, Boundary: provider.FailureBoundaryTurn}, transient: true, waits: true},
		{name: "conditional without retry hint", failure: &provider.FailureMeta{Class: provider.FailureTransientAfterRetry, Boundary: provider.FailureBoundaryTurn}},
		{name: "conditional with retry hint", failure: &provider.FailureMeta{Class: provider.FailureTransientAfterRetry, Boundary: provider.FailureBoundaryTurn}, providerRetry: true, transient: true, waits: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			transient, waits := workflowTransientError(provider.ProviderEvent{
				Kind: provider.EventError, Failure: tc.failure,
			}, tc.providerRetry)
			if transient != tc.transient || waits != tc.waits {
				t.Fatalf("classification = (%v, %v), want (%v, %v)", transient, waits, tc.transient, tc.waits)
			}
		})
	}

	if !workflowTransientTurnComplete(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, Failure: &provider.FailureMeta{Class: provider.FailureTransient},
	}) {
		t.Fatal("Claude network_error terminal reason was not transient")
	}
	if workflowTransientTurnComplete(provider.ProviderEvent{Kind: provider.EventTurnComplete}) {
		t.Fatal("unclassified turn completion was transient")
	}
}

func TestWorkflowWatchdogArmsResetsAndTripsDeterministically(t *testing.T) {
	now := time.Unix(100, 0)
	runner := newTestRunner(t, nil, nil, nil)
	runner.now = func() time.Time { return now }
	var timers []*fakeWorkflowTimer
	runner.newTimer = func(delay time.Duration, callback func()) Timer {
		timer := &fakeWorkflowTimer{callback: callback, delay: delay, active: true}
		timers = append(timers, timer)
		return timer
	}

	key := engine.RunKey{ItemID: "item", PhaseID: "phase", Attempt: 1}
	runKey := workflowRunKey(key)
	outcomes := make(chan engine.Outcome, 1)
	runner.runs[runKey] = &workflowAttempt{
		workflowCompletion: workflowCompletion{key: key},
		threadID:           "thread", watchdog: 10 * time.Second,
		phase: def.Phase{ID: "phase"}, complete: func(outcome engine.Outcome) { outcomes <- outcome },
	}
	runner.schemas["thread"] = json.RawMessage(`{}`)
	runner.workItems["thread"] = key.ItemID

	runner.observe(runKey, provider.ProviderEvent{Kind: provider.EventTurnStart})
	if len(timers) != 1 || timers[0].delay != 10*time.Second {
		t.Fatalf("armed timers = %+v", timers)
	}
	now = now.Add(5 * time.Second)
	runner.observe(runKey, provider.ProviderEvent{Kind: provider.EventTextDelta})
	if len(timers[0].resets) != 1 || timers[0].resets[0] != 10*time.Second {
		t.Fatalf("watchdog resets = %v", timers[0].resets)
	}

	// A stale callback from before the reset observes the moved deadline and
	// rearms for the remaining interval instead of tripping.
	now = now.Add(5 * time.Second)
	timers[0].fire()
	select {
	case outcome := <-outcomes:
		t.Fatalf("stale timer produced outcome %+v", outcome)
	default:
	}
	if got := timers[0].resets[len(timers[0].resets)-1]; got != 5*time.Second {
		t.Fatalf("stale callback reset = %v, want 5s", got)
	}

	now = now.Add(5 * time.Second)
	timers[0].fire()
	select {
	case outcome := <-outcomes:
		if outcome.Kind != engine.OutcomeStalled {
			t.Fatalf("outcome = %+v", outcome)
		}
	case <-time.After(time.Second):
		t.Fatal("watchdog trip did not complete")
	}
	if runner.runs[runKey] != nil || runner.schemaForThread("thread") != nil || runner.WorkItemForThread("thread") != "" {
		t.Fatalf("watchdog cleanup left run/schema/attribution state")
	}
}

func TestWorkflowStopDuringBackoffDisarmsRetry(t *testing.T) {
	runner := newTestRunner(t, nil, nil, nil)
	var timer *fakeWorkflowTimer
	runner.newTimer = func(delay time.Duration, callback func()) Timer {
		timer = &fakeWorkflowTimer{callback: callback, delay: delay, active: true}
		return timer
	}
	key := engine.RunKey{ItemID: "item", PhaseID: "phase", Attempt: 1}
	runKey := workflowRunKey(key)
	completed := false
	attempt := &workflowAttempt{
		workflowCompletion: workflowCompletion{key: key},
		threadID:           "thread", watchdog: time.Hour, backoff: []time.Duration{time.Second},
		complete: func(engine.Outcome) { completed = true },
	}
	runner.runs[runKey] = attempt
	runner.schemas["thread"] = json.RawMessage(`{}`)
	runner.workItems["thread"] = key.ItemID

	// A session death is latched, not laddered: the backoff may only start once
	// the dead session has left App's registry. What the error DOES arm is the
	// watchdog, so a session that errors and then never reports the disconnect
	// still ends the attempt instead of waiting forever.
	runner.observe(runKey, provider.ProviderEvent{Kind: provider.EventSessionStatus, Content: "error"})
	if attempt.timerMode != workflowTimerWatchdog {
		t.Fatalf("transient death armed mode %v before session unregister, want the watchdog", attempt.timerMode)
	}
	runner.observe(runKey, provider.ProviderEvent{Kind: provider.EventSessionStatus, Content: "disconnected"})
	if attempt.timerMode != workflowTimerWatchdog {
		t.Fatalf("disconnected observer armed mode %v before session unregister", attempt.timerMode)
	}
	runner.SessionDisconnected("thread")
	if attempt.timerMode != workflowTimerBackoff || timer == nil || !timer.active {
		t.Fatalf("post-unregister death armed mode %v, want the backoff", attempt.timerMode)
	}
	if _, err := runner.Stop(nil, key); err != nil {
		t.Fatal(err)
	}
	if timer.active {
		t.Fatal("Stop left backoff timer active")
	}
	timer.fire()
	if completed || runner.runs[runKey] != nil || runner.WorkItemForThread("thread") != "" {
		t.Fatal("stopped backoff retried or leaked registration")
	}
}

func TestWorkflowTransientRetryExhaustionCleansAttempt(t *testing.T) {
	runner := newTestRunner(t, nil, nil, nil)
	key := engine.RunKey{ItemID: "item", PhaseID: "phase", Attempt: 1}
	runKey := workflowRunKey(key)
	outcomes := make(chan engine.Outcome, 1)
	runner.runs[runKey] = &workflowAttempt{
		workflowCompletion: workflowCompletion{key: key}, threadID: "thread",
		watchdog: time.Hour,
		complete: func(outcome engine.Outcome) { outcomes <- outcome },
	}
	runner.schemas["thread"] = json.RawMessage(`{}`)
	runner.workItems["thread"] = key.ItemID

	runner.observe(runKey, provider.ProviderEvent{Kind: provider.EventSessionStatus, Content: "error"})
	runner.observe(runKey, provider.ProviderEvent{Kind: provider.EventSessionStatus, Content: "disconnected"})
	runner.SessionDisconnected("thread")
	// The stop runs off the provider event path (`stopAndFinishOffWire`), so the
	// completion is awaited rather than expected inline: the interrupt inside it
	// must never be able to block the pipeline that would deliver its own
	// response. Detach happens before `complete`, so the registry is already
	// clean by the time the outcome lands.
	select {
	case outcome := <-outcomes:
		if outcome.Kind != engine.OutcomeTransientExhausted {
			t.Fatalf("outcome = %+v", outcome)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("retry exhaustion never completed")
	}
	runner.mu.Lock()
	leaked := runner.runs[runKey] != nil
	runner.mu.Unlock()
	if leaked || runner.WorkItemForThread("thread") != "" {
		t.Fatal("retry exhaustion leaked runner state")
	}
}

func TestWorkflowCodexRetryIgnoresPriorTerminalUntilTurnStarts(t *testing.T) {
	runner := newTestRunner(t, nil, nil, nil)
	runner.newTimer = func(delay time.Duration, callback func()) Timer {
		return &fakeWorkflowTimer{callback: callback, delay: delay, active: true}
	}
	key := engine.RunKey{ItemID: "item", PhaseID: "phase", Attempt: 1}
	runKey := workflowRunKey(key)
	outcomes := make(chan engine.Outcome, 1)
	runner.runs[runKey] = &workflowAttempt{
		workflowCompletion: workflowCompletion{key: key}, threadID: "thread", awaitingTurnStart: true,
		provider: string(provider.Codex), watchdog: time.Hour,
		phase:    def.Phase{ID: "phase", Provider: string(provider.Codex)},
		complete: func(outcome engine.Outcome) { outcomes <- outcome },
	}
	runner.schemas["thread"] = json.RawMessage(`{}`)
	runner.workItems["thread"] = key.ItemID

	valid := json.RawMessage(`{"status":"done","outputs":{},"question":null,"reason":null}`)
	runner.observe(runKey, provider.ProviderEvent{Kind: provider.EventTurnComplete, StructuredOutput: valid})
	select {
	case outcome := <-outcomes:
		t.Fatalf("prior terminal completed retry: %+v", outcome)
	default:
	}
	if runner.runs[runKey] == nil {
		t.Fatal("prior terminal detached retry")
	}

	runner.observe(runKey, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: "retry-turn"})
	runner.observe(runKey, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnID: "retry-turn", StructuredOutput: valid})
	select {
	case outcome := <-outcomes:
		if outcome.Kind != engine.OutcomeDone {
			t.Fatalf("retry outcome = %+v", outcome)
		}
	case <-time.After(time.Second):
		t.Fatal("retry turn did not complete")
	}
}
