package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/profile"
)

type staticWorkflowProfileSource struct{ value *profile.Profile }

func (s staticWorkflowProfileSource) Profile(context.Context, string) (*profile.Profile, error) {
	return s.value, nil
}

type fakeWorkflowTimer struct {
	callback func()
	delay    time.Duration
	active   bool
	resets   []time.Duration
}

func (t *fakeWorkflowTimer) Stop() bool {
	wasActive := t.active
	t.active = false
	return wasActive
}

func (t *fakeWorkflowTimer) Reset(delay time.Duration) bool {
	wasActive := t.active
	t.delay = delay
	t.active = true
	t.resets = append(t.resets, delay)
	return wasActive
}

func (t *fakeWorkflowTimer) fire() { t.callback() }

func TestWorkflowReliabilityResolutionPrecedenceAndDefaults(t *testing.T) {
	runner := newWorkflowAppRunner(&App{}, t.TempDir(), staticWorkflowProfileSource{value: &profile.Profile{}})
	request := engine.RunRequest{Item: store.WorkItem{ProjectID: "project"}, Phase: def.Phase{ID: "phase"}}
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
	app := newTestAppWithStore(t)
	runner := newWorkflowAppRunner(app, t.TempDir(), staticWorkflowProfileSource{value: &profile.Profile{}})
	now := time.Unix(100, 0)
	runner.now = func() time.Time { return now }
	var restored *fakeWorkflowTimer
	runner.newTimer = func(delay time.Duration, callback func()) workflowTimer {
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
	app.sessions[attempt.threadID] = session{provider: "unknown", token: "test"}

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
		name      string
		provider  string
		meta      string
		transient bool
		waits     bool
	}{
		{name: "claude rate limit", provider: string(provider.Claude), meta: `{"api_error_enum":"rate_limit","fatal":true,"expect_turn_complete":true}`, transient: true, waits: true},
		{name: "claude server error without typed precursor excluded", provider: string(provider.Claude), meta: `{"api_error_enum":"server_error","fatal":true,"expect_turn_complete":true}`},
		{name: "claude signal excluded for codex", provider: string(provider.Codex), meta: `{"api_error_enum":"rate_limit","fatal":true,"expect_turn_complete":true}`},
		{name: "codex overloaded", provider: string(provider.Codex), meta: `{"fatal":true,"wire":{"error":{"codexErrorInfo":"serverOverloaded"}}}`, transient: true},
		{name: "codex usage limit", provider: string(provider.Codex), meta: `{"fatal":true,"wire":{"error":{"codexErrorInfo":"usageLimitExceeded"}}}`, transient: true},
		{name: "codex http connection", provider: string(provider.Codex), meta: `{"fatal":true,"wire":{"error":{"codexErrorInfo":{"httpConnectionFailed":{"httpStatusCode":503}}}}}`, transient: true},
		{name: "codex stream connection", provider: string(provider.Codex), meta: `{"fatal":true,"wire":{"error":{"codexErrorInfo":{"responseStreamConnectionFailed":{"httpStatusCode":429}}}}}`, transient: true},
		{name: "codex stream disconnected", provider: string(provider.Codex), meta: `{"fatal":true,"wire":{"error":{"codexErrorInfo":{"responseStreamDisconnected":{"httpStatusCode":null}}}}}`, transient: true},
		{name: "codex signal excluded for claude", provider: string(provider.Claude), meta: `{"fatal":true,"wire":{"error":{"codexErrorInfo":"serverOverloaded"}}}`},
		{name: "codex internal server excluded", provider: string(provider.Codex), meta: `{"fatal":true,"wire":{"error":{"codexErrorInfo":"internalServerError"}}}`},
		{name: "codex unknown excluded", provider: string(provider.Codex), meta: `{"fatal":true,"wire":{"error":{"codexErrorInfo":"other"}}}`},
		{name: "malformed excluded", provider: string(provider.Claude), meta: `{`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			transient, waits := workflowTransientError(tc.provider, provider.ProviderEvent{Kind: provider.EventError, Meta: json.RawMessage(tc.meta)}, false)
			if transient != tc.transient || waits != tc.waits {
				t.Fatalf("classification = (%v, %v), want (%v, %v)", transient, waits, tc.transient, tc.waits)
			}
		})
	}

	serverError := provider.ProviderEvent{Kind: provider.EventError, Meta: json.RawMessage(`{"api_error_enum":"server_error","expect_turn_complete":true}`)}
	if transient, waits := workflowTransientError(string(provider.Claude), serverError, true); !transient || !waits {
		t.Fatalf("Claude server error after typed precursor = (%v, %v)", transient, waits)
	}
	for _, meta := range []string{
		`{"wire":{"error_status":529}}`,
		`{"wire":{"error":{"connection":{"code":"ECONNRESET"}}}}`,
	} {
		if !workflowClaudeTransientAPIRetry(provider.ProviderEvent{Kind: provider.EventAPIRetry, Meta: json.RawMessage(meta)}) {
			t.Fatalf("Claude api_retry %s was not transient", meta)
		}
	}
	if workflowClaudeTransientAPIRetry(provider.ProviderEvent{Kind: provider.EventAPIRetry, Meta: json.RawMessage(`{"wire":{"error_status":500}}`)}) {
		t.Fatal("ambiguous Claude api_retry was transient")
	}

	if !workflowTransientTurnComplete(string(provider.Claude), provider.ProviderEvent{Kind: provider.EventTurnComplete, Raw: json.RawMessage(`{"terminal_reason":"network_error"}`)}) {
		t.Fatal("Claude network_error terminal reason was not transient")
	}
	if workflowTransientTurnComplete(string(provider.Claude), provider.ProviderEvent{Kind: provider.EventTurnComplete, Raw: json.RawMessage(`{"terminal_reason":"timeout"}`)}) {
		t.Fatal("ambiguous Claude timeout was transient")
	}
	if workflowTransientTurnComplete(string(provider.Codex), provider.ProviderEvent{Kind: provider.EventTurnComplete, Raw: json.RawMessage(`{"terminal_reason":"network_error"}`)}) {
		t.Fatal("Claude terminal shape was accepted for Codex")
	}
}

func TestWorkflowWatchdogArmsResetsAndTripsDeterministically(t *testing.T) {
	now := time.Unix(100, 0)
	runner := newWorkflowAppRunner(&App{}, t.TempDir(), nil)
	runner.now = func() time.Time { return now }
	var timers []*fakeWorkflowTimer
	runner.newTimer = func(delay time.Duration, callback func()) workflowTimer {
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
	if runner.runs[runKey] != nil || runner.schemaForThread("thread") != nil || runner.workItemForThread("thread") != "" {
		t.Fatalf("watchdog cleanup left run/schema/attribution state")
	}
}

func TestWorkflowStopDuringBackoffDisarmsRetry(t *testing.T) {
	runner := newWorkflowAppRunner(&App{}, t.TempDir(), nil)
	var timer *fakeWorkflowTimer
	runner.newTimer = func(delay time.Duration, callback func()) workflowTimer {
		timer = &fakeWorkflowTimer{callback: callback, delay: delay, active: true}
		return timer
	}
	key := engine.RunKey{ItemID: "item", PhaseID: "phase", Attempt: 1}
	runKey := workflowRunKey(key)
	completed := false
	runner.runs[runKey] = &workflowAttempt{
		workflowCompletion: workflowCompletion{key: key},
		threadID:           "thread", backoff: []time.Duration{time.Second},
		complete: func(engine.Outcome) { completed = true },
	}
	runner.schemas["thread"] = json.RawMessage(`{}`)
	runner.workItems["thread"] = key.ItemID

	runner.observe(runKey, provider.ProviderEvent{Kind: provider.EventSessionStatus, Content: "error"})
	if timer != nil {
		t.Fatal("transient death armed backoff before session unregister")
	}
	runner.observe(runKey, provider.ProviderEvent{Kind: provider.EventSessionStatus, Content: "disconnected"})
	if timer != nil {
		t.Fatal("disconnected observer armed backoff before session unregister")
	}
	runner.sessionDisconnected("thread")
	if timer == nil || !timer.active {
		t.Fatal("post-unregister death did not arm backoff")
	}
	if _, err := runner.Stop(nil, key); err != nil {
		t.Fatal(err)
	}
	if timer.active {
		t.Fatal("Stop left backoff timer active")
	}
	timer.fire()
	if completed || runner.runs[runKey] != nil || runner.workItemForThread("thread") != "" {
		t.Fatal("stopped backoff retried or leaked registration")
	}
}

func TestWorkflowTransientRetryExhaustionCleansAttempt(t *testing.T) {
	runner := newWorkflowAppRunner(&App{}, t.TempDir(), nil)
	key := engine.RunKey{ItemID: "item", PhaseID: "phase", Attempt: 1}
	runKey := workflowRunKey(key)
	outcomes := make(chan engine.Outcome, 1)
	runner.runs[runKey] = &workflowAttempt{
		workflowCompletion: workflowCompletion{key: key}, threadID: "thread",
		complete: func(outcome engine.Outcome) { outcomes <- outcome },
	}
	runner.schemas["thread"] = json.RawMessage(`{}`)
	runner.workItems["thread"] = key.ItemID

	runner.observe(runKey, provider.ProviderEvent{Kind: provider.EventSessionStatus, Content: "error"})
	runner.observe(runKey, provider.ProviderEvent{Kind: provider.EventSessionStatus, Content: "disconnected"})
	runner.sessionDisconnected("thread")
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
	if leaked || runner.workItemForThread("thread") != "" {
		t.Fatal("retry exhaustion leaked runner state")
	}
}

func TestWorkflowCodexRetryIgnoresPriorTerminalUntilTurnStarts(t *testing.T) {
	runner := newWorkflowAppRunner(&App{}, t.TempDir(), nil)
	key := engine.RunKey{ItemID: "item", PhaseID: "phase", Attempt: 1}
	runKey := workflowRunKey(key)
	outcomes := make(chan engine.Outcome, 1)
	runner.runs[runKey] = &workflowAttempt{
		workflowCompletion: workflowCompletion{key: key}, threadID: "thread", awaitingRetryStart: true,
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
