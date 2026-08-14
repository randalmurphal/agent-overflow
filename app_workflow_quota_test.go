package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/profile"
)

// The workflow layer reads only adapter-normalized failure metadata. Raw
// provider prose or JSON cannot accidentally become quota control flow.
func TestWorkflowQuotaRefusalReadsNormalizedFailureOnly(t *testing.T) {
	for _, test := range []struct {
		name    string
		kind    provider.EventKind
		failure *provider.FailureMeta
		meta    string
		want    bool
	}{
		{name: "normalized usage limit", kind: provider.EventError,
			failure: &provider.FailureMeta{Reason: provider.FailureReasonUsageLimit}, want: true},
		{name: "ordinary transient", kind: provider.EventError,
			failure: &provider.FailureMeta{Class: provider.FailureTransient}},
		{name: "raw claude enum without classification", kind: provider.EventError,
			meta: `{"api_error_enum":"rate_limit"}`},
		{name: "raw codex code without classification", kind: provider.EventError,
			meta: `{"codexErrorInfo":"usageLimitExceeded"}`},
		{name: "not an error event", kind: provider.EventTurnComplete,
			failure: &provider.FailureMeta{Reason: provider.FailureReasonUsageLimit}},
		{name: "no failure", kind: provider.EventError},
	} {
		t.Run(test.name, func(t *testing.T) {
			event := provider.ProviderEvent{Kind: test.kind, Failure: test.failure, Meta: json.RawMessage(test.meta)}
			if got := workflowUsageLimitRefusal(event); got != test.want {
				t.Fatalf("workflowUsageLimitRefusal = %v, want %v", got, test.want)
			}
		})
	}
}

type usageLimitObserveHarness struct {
	app     *App
	runner  *workflowAppRunner
	attempt *workflowAttempt
	runKey  string
	outcome chan engine.Outcome
	now     time.Time
}

func newUsageLimitObserveHarness(t *testing.T, providerName string, identified bool) *usageLimitObserveHarness {
	t.Helper()
	app := newTestAppWithStore(t)
	h := &usageLimitObserveHarness{
		app: app, outcome: make(chan engine.Outcome, 1), now: time.Unix(1_700_000_000, 0),
	}
	h.runner = newWorkflowAppRunner(app, t.TempDir(), staticWorkflowProfileSource{value: &profile.Profile{}})
	h.runner.now = func() time.Time { return h.now }
	key := engine.RunKey{ItemID: "run-limit", PhaseID: "work", Attempt: 1}
	h.runKey = workflowRunKey(key)
	h.attempt = &workflowAttempt{
		workflowCompletion: workflowCompletion{key: key},
		complete:           func(outcome engine.Outcome) { h.outcome <- outcome },
		provider:           providerName,
		threadID:           "thread-limit",
		turnStarted:        true,
		unsubscribe:        func() {},
		timer:              &fakeWorkflowTimer{active: true},
		timerMode:          workflowTimerWatchdog,
	}
	if identified {
		h.attempt.dispatchIdentity = providerDispatchIdentity{
			Provider: providerName, AccountID: "account-2", CredentialGeneration: 17,
		}
		h.attempt.dispatchIdentitySet = true
	}
	h.runner.runs[h.runKey] = h.attempt
	if err := app.store.CreateWorkItem(store.WorkItem{
		ID: key.ItemID, ProjectID: defaultTestProjectID, Goal: "usage limit", WorkflowID: "wf",
		WorkflowScope: "shared", State: string(engine.StateRunning), Source: "manual",
		CreatedAt: h.now.UnixMilli(), StartedAt: h.now.UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *usageLimitObserveHarness) refusal(code string, class provider.FailureClass) provider.ProviderEvent {
	return provider.ProviderEvent{
		Kind: provider.EventError, Content: "usage is exhausted",
		Failure: &provider.FailureMeta{
			Class: class, Boundary: provider.FailureBoundaryTurn,
			Reason: provider.FailureReasonUsageLimit, Code: code,
		},
	}
}

func (h *usageLimitObserveHarness) awaitOutcome(t *testing.T) engine.Outcome {
	t.Helper()
	select {
	case outcome := <-h.outcome:
		return outcome
	case <-time.After(2 * time.Second):
		t.Fatal("usage refusal did not settle the attempt")
		return engine.Outcome{}
	}
}

// Both provider-native classifications take the same immediate path, even if
// a provider marks the refusal non-transient and even without a reset window.
// The retry counter and retry timer are not advanced.
func TestUsageLimitRefusalParksImmediatelyWithoutRetries(t *testing.T) {
	for _, test := range []struct {
		provider string
		code     string
		class    provider.FailureClass
	}{
		{provider: string(provider.Claude), code: "rate_limit", class: provider.FailureTransient},
		{provider: string(provider.Codex), code: "usageLimitExceeded", class: provider.FailureFatal},
	} {
		t.Run(test.provider, func(t *testing.T) {
			h := newUsageLimitObserveHarness(t, test.provider, true)
			h.runner.observe(h.runKey, h.refusal(test.code, test.class))
			h.runner.mu.Lock()
			_, stillInstalled := h.runner.runs[h.runKey]
			h.runner.mu.Unlock()
			if stillInstalled {
				t.Fatal("usage-limited attempt remained reachable to late provider events")
			}
			outcome := h.awaitOutcome(t)
			if outcome.Kind != engine.OutcomeProviderUsageLimited {
				t.Fatalf("outcome kind = %q, want %q", outcome.Kind, engine.OutcomeProviderUsageLimited)
			}
			if outcome.ProviderUsageScopeID == 0 {
				t.Fatal("typed usage refusal lost provider-account scope")
			}
			if !strings.Contains(outcome.Detail, test.code) || !strings.Contains(outcome.Detail, "resume after changing accounts") {
				t.Fatalf("outcome detail = %q", outcome.Detail)
			}
			scope, err := h.app.store.GetWorkflowProviderUsageScope(outcome.ProviderUsageScopeID)
			if err != nil {
				t.Fatal(err)
			}
			if scope.Provider != test.provider || scope.AccountID != "account-2" || scope.CredentialGeneration != 17 {
				t.Fatalf("usage scope = %+v", scope)
			}
			if h.attempt.transientRetryCount != 0 {
				t.Fatalf("usage refusal consumed %d app retries", h.attempt.transientRetryCount)
			}
			if at, err := h.app.store.WorkItemAutoResumeAt("run-limit"); err != nil || at != 0 {
				t.Fatalf("usage refusal armed auto-resume: at=%d err=%v", at, err)
			}
		})
	}
}

// Missing dispatch attribution degrades only notification coalescing. It must
// never put a known usage refusal back through the retry ladder.
func TestUsageLimitWithoutDispatchIdentityStillParksImmediately(t *testing.T) {
	h := newUsageLimitObserveHarness(t, string(provider.Claude), false)
	h.runner.observe(h.runKey, h.refusal("rate_limit", provider.FailureTransient))
	outcome := h.awaitOutcome(t)
	if outcome.Kind != engine.OutcomeProviderUsageLimited || outcome.ProviderUsageScopeID != 0 {
		t.Fatalf("outcome = %+v", outcome)
	}
	if h.attempt.transientRetryCount != 0 {
		t.Fatalf("usage refusal consumed %d retries", h.attempt.transientRetryCount)
	}
}

func TestUsageLimitParkDoesNotStopOtherWorkOnTheProvider(t *testing.T) {
	h := newUsageLimitObserveHarness(t, string(provider.Codex), true)
	otherKey := engine.RunKey{ItemID: "run-long-tool", PhaseID: "work", Attempt: 1}
	otherRunKey := workflowRunKey(otherKey)
	otherTimer := &fakeWorkflowTimer{active: true}
	otherOutcome := make(chan engine.Outcome, 1)
	other := &workflowAttempt{
		workflowCompletion: workflowCompletion{key: otherKey},
		complete:           func(outcome engine.Outcome) { otherOutcome <- outcome },
		provider:           string(provider.Codex),
		threadID:           "thread-long-tool",
		turnStarted:        true,
		unsubscribe:        func() {},
		timer:              otherTimer,
		timerMode:          workflowTimerWatchdog,
	}
	h.runner.runs[otherRunKey] = other

	h.runner.observe(h.runKey, h.refusal("usageLimitExceeded", provider.FailureTransient))
	_ = h.awaitOutcome(t)

	h.runner.mu.Lock()
	stillRunning := h.runner.runs[otherRunKey] == other
	h.runner.mu.Unlock()
	if !stillRunning || !otherTimer.active {
		t.Fatalf("another provider attempt was disturbed: installed=%v timer-active=%v", stillRunning, otherTimer.active)
	}
	select {
	case outcome := <-otherOutcome:
		t.Fatalf("another provider attempt was completed: %+v", outcome)
	default:
	}
}

func TestUsageLimitCleanupDoesNotRecordAUserInterrupt(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(string, any) {})
	app.configureTriageQueueCallbacks()
	thread := testThread("usage-cleanup-interrupt")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: thread.ID, TurnIndex: 0, Timestamp: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventError, ThreadID: thread.ID, Content: "usage is exhausted",
		Meta: json.RawMessage(`{"fatal":true,"expect_turn_complete":true,"codexErrorInfo":"usageLimitExceeded"}`),
		Failure: &provider.FailureMeta{
			Class: provider.FailureFatal, Boundary: provider.FailureBoundaryTurn,
			Reason: provider.FailureReasonUsageLimit, Code: "usageLimitExceeded",
		},
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if open := app.triage.OpenTurnIndex(thread.ID); open != -1 {
		t.Fatalf("usage refusal left turn %d open before workflow cleanup", open)
	}

	sess := installSteerTestSession(t, app, thread, "ok")
	app.sessionManager().put(thread.ID, session{
		provider: string(provider.Codex), token: "usage-cleanup-token", codex: sess,
	})
	if err := app.InterruptTurn(thread.ID); err != nil {
		t.Fatal(err)
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Kind == "error" && item.Summary == "Stopped by user" {
			t.Fatalf("usage-limit cleanup created user-interrupt row %+v", item)
		}
	}
}

func TestWorkflowProviderErrorDetailNamesAndBoundsTheProviderError(t *testing.T) {
	got := workflowProviderErrorDetail(provider.ProviderEvent{
		Kind: provider.EventError, Content: "Claude usage limit reached",
		Failure: &provider.FailureMeta{Code: "rate_limit"},
	})
	if got != "provider error rate_limit: Claude usage limit reached" {
		t.Fatalf("detail = %q", got)
	}
	long := workflowFailureDetail(strings.Repeat("x", maxWorkflowFailureDetailRunes+100))
	if runes := []rune(long); len(runes) > maxWorkflowFailureDetailRunes {
		t.Fatalf("bounded detail has %d runes", len(runes))
	}
}
