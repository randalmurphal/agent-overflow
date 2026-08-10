package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/profile"
)

// Both halves of the recognition are the providers' own typed enums. The test
// pins the cross-provider exclusions too: the enums are namespaced by provider,
// and reading one provider's signal off the other's meta would act on an error
// that never said what this mechanism thinks it said.
func TestWorkflowQuotaRefusalReadsTypedProviderSignalsOnly(t *testing.T) {
	for _, test := range []struct {
		name     string
		provider string
		kind     provider.EventKind
		meta     string
		want     bool
	}{
		{name: "claude rate limit", provider: string(provider.Claude), kind: provider.EventError,
			meta: `{"api_error_enum":"rate_limit","fatal":true,"expect_turn_complete":true}`, want: true},
		{name: "claude other enum", provider: string(provider.Claude), kind: provider.EventError,
			meta: `{"api_error_enum":"authentication_failed","fatal":true}`},
		{name: "codex usage limit", provider: string(provider.Codex), kind: provider.EventError,
			meta: `{"fatal":true,"wire":{"error":{"codexErrorInfo":"usageLimitExceeded"}}}`, want: true},
		{name: "codex overloaded", provider: string(provider.Codex), kind: provider.EventError,
			meta: `{"fatal":true,"wire":{"error":{"codexErrorInfo":"serverOverloaded"}}}`},
		{name: "codex structured variant is not the scalar", provider: string(provider.Codex), kind: provider.EventError,
			meta: `{"fatal":true,"wire":{"error":{"codexErrorInfo":{"usageLimitExceeded":{}}}}}`},
		{name: "claude signal excluded for codex", provider: string(provider.Codex), kind: provider.EventError,
			meta: `{"api_error_enum":"rate_limit"}`},
		{name: "codex signal excluded for claude", provider: string(provider.Claude), kind: provider.EventError,
			meta: `{"fatal":true,"wire":{"error":{"codexErrorInfo":"usageLimitExceeded"}}}`},
		{name: "unknown provider", provider: "claude-tui", kind: provider.EventError,
			meta: `{"api_error_enum":"rate_limit"}`},
		{name: "not an error event", provider: string(provider.Claude), kind: provider.EventTurnComplete,
			meta: `{"api_error_enum":"rate_limit"}`},
		{name: "no meta", provider: string(provider.Claude), kind: provider.EventError},
		{name: "malformed meta", provider: string(provider.Claude), kind: provider.EventError, meta: `{`},
	} {
		t.Run(test.name, func(t *testing.T) {
			event := provider.ProviderEvent{Kind: test.kind, Meta: json.RawMessage(test.meta)}
			if got := workflowQuotaRefusal(test.provider, event); got != test.want {
				t.Fatalf("workflowQuotaRefusal = %v, want %v", got, test.want)
			}
		})
	}
}

// Selection is the EARLIEST future boundary among the spent windows. Resuming
// early costs one turn and re-parks against whatever is still in force;
// over-sleeping is silent and unrecoverable, so the tie always goes to early.
func TestWorkflowQuotaResetPicksTheEarliestFutureSpentWindow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	fiveHour := now.Add(2 * time.Hour).Unix()
	weekly := now.Add(72 * time.Hour).Unix()

	resetsAt, ok := workflowQuotaReset([]provider.RateLimitEntry{
		{LimitID: "weekly_all", UsedPercent: 100, ResetsAt: weekly},
		{LimitID: "session", UsedPercent: 99.4, ResetsAt: fiveHour},
	}, now)
	if !ok || resetsAt.Unix() != fiveHour {
		t.Fatalf("reset = %v (%v), want the five-hour window at %d", resetsAt, ok, fiveHour)
	}

	// A window that is not spent does not decide the boundary even when it is
	// the nearest one: the refusal came from the window that ran out.
	resetsAt, ok = workflowQuotaReset([]provider.RateLimitEntry{
		{LimitID: "session", UsedPercent: 40, ResetsAt: now.Add(time.Minute).Unix()},
		{LimitID: "weekly_all", UsedPercent: 100, ResetsAt: weekly},
	}, now)
	if !ok || resetsAt.Unix() != weekly {
		t.Fatalf("reset = %v (%v), want the weekly window at %d", resetsAt, ok, weekly)
	}

	// Nothing to come back at: a boundary already past is stale, a missing one
	// says only "later", and neither may be turned into a schedule.
	for _, limits := range [][]provider.RateLimitEntry{
		nil,
		{{LimitID: "session", UsedPercent: 100, ResetsAt: 0}},
		{{LimitID: "session", UsedPercent: 100, ResetsAt: now.Add(-time.Hour).Unix()}},
		{{LimitID: "session", UsedPercent: 100, ResetsAt: now.Unix()}},
		{{LimitID: "session", UsedPercent: 98, ResetsAt: weekly}},
	} {
		if resetsAt, ok := workflowQuotaReset(limits, now); ok {
			t.Fatalf("limits %+v produced a boundary %v", limits, resetsAt)
		}
	}
}

// The jitter is derived from the run id, never drawn: the time an operator
// reads in the park cause has to be the time the timer actually holds, and two
// runs of one wave must not come back in the same second.
func TestWorkflowQuotaResumeJitterIsDeterministicAndBounded(t *testing.T) {
	first := workflowQuotaResumeJitter("run-alpha")
	if first != workflowQuotaResumeJitter("run-alpha") {
		t.Fatal("jitter for one run id was not stable")
	}
	spread := map[time.Duration]struct{}{}
	for _, itemID := range []string{"run-alpha", "run-beta", "run-gamma", "run-delta", "run-epsilon"} {
		jitter := workflowQuotaResumeJitter(itemID)
		if jitter < workflowQuotaResumeJitterMin ||
			jitter >= workflowQuotaResumeJitterMin+workflowQuotaResumeJitterSpan {
			t.Fatalf("jitter for %s = %v, outside [%v, %v)", itemID, jitter,
				workflowQuotaResumeJitterMin, workflowQuotaResumeJitterMin+workflowQuotaResumeJitterSpan)
		}
		spread[jitter] = struct{}{}
	}
	if len(spread) < 2 {
		t.Fatalf("five run ids produced %d distinct jitters; a wave would resume as a burst", len(spread))
	}
}

// The cause answers two different questions — when the provider says the
// allowance returns, and when this run acts on that — so it states both. And it
// only says the second where the second is TRUE: the unscheduled form is what a
// park that could not arm a timer is allowed to claim.
func TestWorkflowQuotaCausesClaimOnlyWhatTheParkDid(t *testing.T) {
	resetsAt := time.Unix(1_700_000_000, 0)
	resumeAt := resetsAt.Add(97 * time.Second)

	scheduled := workflowQuotaScheduledCause(resetsAt, resumeAt)
	for _, want := range []string{
		"provider usage limit reached",
		"the limit resets " + resetsAt.Local().Format(time.RFC3339),
		"this run resumes itself at " + resumeAt.Local().Format(time.RFC3339),
	} {
		if !strings.Contains(scheduled, want) {
			t.Fatalf("scheduled cause %q is missing %q", scheduled, want)
		}
	}

	unscheduled := workflowQuotaUnscheduledCause(resetsAt)
	// The reset boundary is true whatever the store did, so it stays.
	if !strings.Contains(unscheduled, "the limit resets "+resetsAt.Local().Format(time.RFC3339)) {
		t.Fatalf("unscheduled cause %q dropped the reset boundary", unscheduled)
	}
	// The promise is not, so it goes — and the human is told it is theirs.
	if strings.Contains(unscheduled, "resumes itself") {
		t.Fatalf("unscheduled cause %q promises a resume nothing armed", unscheduled)
	}
	if !strings.Contains(unscheduled, "resume it once the limit is back") {
		t.Fatalf("unscheduled cause %q does not tell a human to act", unscheduled)
	}
}

// quotaParkLocked needs BOTH halves. A refusal with no boundary falls through
// to the ordinary ladder rather than inventing a delay.
func TestQuotaParkRequiresBothTheRefusalAndABoundary(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	runner := newWorkflowAppRunner(&App{}, t.TempDir(), staticWorkflowProfileSource{value: &profile.Profile{}})
	runner.now = func() time.Time { return now }
	refusal := provider.ProviderEvent{
		Kind: provider.EventError,
		Meta: json.RawMessage(`{"api_error_enum":"rate_limit","fatal":true,"expect_turn_complete":true}`),
	}
	resetsAt := now.Add(3 * time.Hour)
	spent := []provider.RateLimitEntry{{LimitID: "session", UsedPercent: 100, ResetsAt: resetsAt.Unix()}}

	attempt := &workflowAttempt{provider: string(provider.Claude), rateLimits: spent}
	attempt.key.ItemID = "run-1"
	gotResetsAt, resumeAt, ok := runner.quotaParkLocked(attempt, refusal)
	if !ok {
		t.Fatal("a dated quota refusal was not recognized")
	}
	if !gotResetsAt.Equal(resetsAt) {
		t.Fatalf("reset boundary = %v, want %v", gotResetsAt, resetsAt)
	}
	if want := resetsAt.Add(workflowQuotaResumeJitter("run-1")); !resumeAt.Equal(want) {
		t.Fatalf("resume at %v, want %v", resumeAt, want)
	}

	// No windows reported: the refusal is real but says only "later".
	bare := &workflowAttempt{provider: string(provider.Claude)}
	bare.key.ItemID = "run-1"
	if _, _, ok := runner.quotaParkLocked(bare, refusal); ok {
		t.Fatal("a refusal with no reported window produced a schedule")
	}

	// Windows reported, but the error is an ordinary transient one.
	overloaded := &workflowAttempt{provider: string(provider.Codex), rateLimits: spent}
	overloaded.key.ItemID = "run-1"
	transient := provider.ProviderEvent{
		Kind: provider.EventError,
		Meta: json.RawMessage(`{"fatal":true,"wire":{"error":{"codexErrorInfo":"serverOverloaded"}}}`),
	}
	if _, _, ok := runner.quotaParkLocked(overloaded, transient); ok {
		t.Fatal("an overloaded-server error was parked as a usage limit")
	}
}

// noteRateLimits keeps the last good answer rather than clearing on a snapshot
// it cannot use: the only reader is a refusal deciding when to come back.
func TestNoteRateLimitsKeepsTheLastUsableSnapshot(t *testing.T) {
	attempt := &workflowAttempt{}
	good, err := json.Marshal(provider.RateLimitsSnapshot{
		Provider: string(provider.Claude),
		Limits:   []provider.RateLimitEntry{{LimitID: "session", UsedPercent: 100, ResetsAt: 42}},
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt.noteRateLimits(good)
	for _, meta := range []string{"", "{", `{"limits":[]}`} {
		attempt.noteRateLimits(json.RawMessage(meta))
		if len(attempt.rateLimits) != 1 || attempt.rateLimits[0].ResetsAt != 42 {
			t.Fatalf("meta %q cleared the held windows: %+v", meta, attempt.rateLimits)
		}
	}
}

// Feature 2's raw material: whatever the runner knows about a failure the
// element authored no envelope for. The typed code is appended because "API
// error" alone does not distinguish an expired login from a refused prompt.
func TestWorkflowProviderErrorDetailNamesTheProviderError(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
		meta    string
		want    string
	}{
		{name: "summary and claude enum", content: "Claude usage limit reached",
			meta: `{"api_error_enum":"rate_limit"}`,
			want: "provider error rate_limit: Claude usage limit reached"},
		{name: "summary and codex code", content: "stream disconnected",
			meta: `{"wire":{"error":{"codexErrorInfo":"serverOverloaded"}}}`,
			want: "provider error serverOverloaded: stream disconnected"},
		{name: "summary only", content: "something went wrong", want: "provider error: something went wrong"},
		{name: "code only", meta: `{"api_error_enum":"authentication_failed"}`,
			want: "provider error authentication_failed"},
		{name: "nothing to say", meta: `{}`},
		{name: "malformed meta drops the code", content: "boom", meta: `{`, want: "provider error: boom"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := workflowProviderErrorDetail(provider.ProviderEvent{
				Kind: provider.EventError, Content: test.content, Meta: json.RawMessage(test.meta),
			})
			if got != test.want {
				t.Fatalf("detail = %q, want %q", got, test.want)
			}
		})
	}
}

// A provider error message is prose of unknown length, and it lands on a
// `run status` line and in a wake. Every runner-authored detail passes the
// same bound so no path can put an unbounded string on a park cause.
func TestWorkflowFailureDetailIsBounded(t *testing.T) {
	detail := workflowFailureDetail("  " + strings.Repeat("x", maxWorkflowFailureDetailRunes*3) + "  ")
	if length := len([]rune(detail)); length > maxWorkflowFailureDetailRunes {
		t.Fatalf("detail is %d runes, want at most %d", length, maxWorkflowFailureDetailRunes)
	}
	if got := workflowFailureDetail("  short  "); got != "short" {
		t.Fatalf("detail = %q, want the trimmed original", got)
	}
}

// The turn-complete path has no error event to quote, so it renders the error
// the turn closed with — and still says something when there is none.
func TestWorkflowTurnCompleteFailureDetailAlwaysSaysSomething(t *testing.T) {
	withMessage := workflowTurnCompleteFailureDetail(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, TurnComplete: &provider.WireTurnCompleteMeta{ErrorMessage: "quota exceeded"},
	})
	if withMessage != "the turn completed with an error: quota exceeded" {
		t.Fatalf("detail = %q", withMessage)
	}
	for _, event := range []provider.ProviderEvent{
		{Kind: provider.EventTurnComplete},
		{Kind: provider.EventTurnComplete, TurnComplete: &provider.WireTurnCompleteMeta{}},
	} {
		if got := workflowTurnCompleteFailureDetail(event); got != "the turn completed with an error and no envelope" {
			t.Fatalf("detail = %q, want the fallback", got)
		}
	}
}

// The whole mechanism end to end, over the real engine, runner, and provider
// wire: a session whose 429 carries a reset boundary parks WITHOUT spending the
// retry ladder, records the boundary, states both moments on the attempt, and
// comes back on its own when the timer the app holds fires.
func TestWorkflowQuotaParkResumesItselfWhenTheLimitReturns(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	writeReliabilityWorkflow(t, configRoot, `
  - id: run
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: run.md
    gate:
      routes:
        - to: done`)
	resetsAt := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	refusedPath := filepath.Join(t.TempDir(), "refused")
	binary := writeQuotaRefusingWorkflowClaude(t, refusedPath, resetsAt)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	projectRow := mustReloadProject(t, app.store, testutil.EnsureProject(t, app.store, t.TempDir()).ID)
	// A backoff ladder that would take a minute to spend if the park ever
	// reached it. It must not: the whole point is that the ladder is skipped.
	writeReliabilityProfile(t, configRoot, projectRow.Slug, "watchdog: 1h\n  backoff: [30s, 30s]\n")

	armed := make(chan *armedWorkflowTimer, 4)
	app.newWorkflowAutoResumeTimer = func(delay time.Duration, fire func()) workflowTimer {
		timer := &armedWorkflowTimer{callback: fire, delay: delay}
		armed <- timer
		return timer
	}
	startWorkflowEngineForTest(t, app, configRoot)

	item, err := app.WorkflowStartRun(projectRow.ID, "reliability-flow", "shared", "quota", json.RawMessage(`{}`), nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	parked := waitForWorkflowItem(t, app, item.ID, engine.StateNeedsHuman, engine.ReasonRetriesExhausted)

	wantResumeAt := resetsAt.Add(workflowQuotaResumeJitter(parked.ID))
	scheduled, err := app.store.WorkItemAutoResumeAt(parked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if scheduled != wantResumeAt.UnixMilli() {
		t.Fatalf("persisted schedule = %d, want %d (%v)", scheduled, wantResumeAt.UnixMilli(), wantResumeAt)
	}
	phases, err := app.store.ListWorkItemPhases(parked.ID)
	if err != nil || len(phases) == 0 {
		t.Fatalf("phases = %+v, %v", phases, err)
	}
	cause := phases[len(phases)-1].ParkCause
	for _, want := range []string{
		"provider usage limit reached",
		"the limit resets " + resetsAt.Local().Format(time.RFC3339),
		"this run resumes itself at " + wantResumeAt.Local().Format(time.RFC3339),
	} {
		if !strings.Contains(cause, want) {
			t.Fatalf("park cause %q is missing %q", cause, want)
		}
	}

	var timer *armedWorkflowTimer
	select {
	case timer = <-armed:
	case <-time.After(2 * time.Second):
		t.Fatal("the park armed no self-resume timer")
	}
	if timer.delay < 2*time.Hour || timer.delay > 4*time.Hour {
		t.Fatalf("timer delay = %v, want roughly the three hours until the limit returns", timer.delay)
	}

	timer.callback()

	done := waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")
	if scheduled, err := app.store.WorkItemAutoResumeAt(done.ID); err != nil || scheduled != 0 {
		t.Fatalf("schedule after the self-resume = %d, %v; want it cleared", scheduled, err)
	}
}

// writeQuotaRefusingWorkflowClaude refuses the first turn with the exact pair a
// real 429 produces — the `rate_limit_event` the CLI emits from its own 429
// handler (`status:"rejected"` carries the `anthropic-ratelimit-unified-reset`
// boundary and no utilization) followed by the `assistant.error` enum — and
// completes the next turn normally, which is what the limit returning looks
// like from here.
func writeQuotaRefusingWorkflowClaude(t *testing.T, refusedPath string, resetsAt time.Time) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/bash
while IFS= read -r line; do
  if [[ "$line" == *'"subtype":"interrupt"'* ]]; then
    reqid=$(printf '%%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
    printf '{"type":"control_response","response":{"subtype":"success","request_id":"%%s","response":{}}}\n' "$reqid"
    continue
  fi
  printf '%%s\n' '{"type":"system","subtype":"init","session_id":"quota","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}'
  if [ -f %[1]q ]; then
    printf '%%s\n' '{"type":"result","subtype":"success","is_error":false,"stop_reason":"end_turn","structured_output":{"status":"done","outputs":{},"question":null,"reason":null}}'
  else
    printf 'refused\n' > %[1]q
    printf '%%s\n' '{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":%[2]d,"rateLimitType":"five_hour"}}'
    printf '%%s\n' '{"type":"assistant","message":{"id":"msg_quota","type":"message","role":"assistant","model":"claude-opus-4-7","content":[],"error":"rate_limit"},"session_id":"quota"}'
  fi
done
`, refusedPath, resetsAt.Unix())
	return writeExecutable(t, "quota-claude.sh", script)
}

// quotaObserveHarness drives `observe` directly with one installed attempt. It
// is the level the ordering questions live at: which events reach the recorder,
// and what the park says when the store refuses the schedule.
type quotaObserveHarness struct {
	app      *App
	runner   *workflowAppRunner
	attempt  *workflowAttempt
	runKey   string
	outcomes chan engine.Outcome
	now      time.Time
}

// newQuotaObserveHarness persists the run row the schedule is written to.
func newQuotaObserveHarness(t *testing.T, itemID string) *quotaObserveHarness {
	t.Helper()
	return newQuotaObserveHarnessOpts(t, itemID, true)
}

// newQuotaObserveHarnessOpts can leave the run row out, which is the shape every
// failure of the schedule write takes: the UPDATE affects no rows.
func newQuotaObserveHarnessOpts(t *testing.T, itemID string, persistRun bool) *quotaObserveHarness {
	t.Helper()
	app := newTestAppWithStore(t)
	harness := &quotaObserveHarness{
		app: app, outcomes: make(chan engine.Outcome, 4), now: time.Unix(1_700_000_000, 0),
	}
	harness.runner = newWorkflowAppRunner(app, t.TempDir(), staticWorkflowProfileSource{value: &profile.Profile{}})
	harness.runner.now = func() time.Time { return harness.now }
	app.workflowAutoResumeNowFn = func() time.Time { return harness.now }
	app.newWorkflowAutoResumeTimer = func(delay time.Duration, fire func()) workflowTimer {
		return &armedWorkflowTimer{callback: fire, delay: delay}
	}
	key := engine.RunKey{ItemID: itemID, PhaseID: "work", Attempt: 1}
	harness.runKey = workflowRunKey(key)
	harness.attempt = &workflowAttempt{
		workflowCompletion: workflowCompletion{key: key},
		complete:           func(outcome engine.Outcome) { harness.outcomes <- outcome },
		provider:           string(provider.Claude), threadID: "thread-" + itemID,
		turnStarted: true, unsubscribe: func() {},
	}
	harness.runner.runs[harness.runKey] = harness.attempt
	if persistRun {
		if err := app.store.CreateWorkItem(store.WorkItem{
			ID: itemID, ProjectID: defaultTestProjectID, Goal: "quota", WorkflowID: "wf",
			WorkflowScope: "shared", State: string(engine.StateRunning), Source: "manual",
			CreatedAt: harness.now.UnixMilli(), StartedAt: harness.now.UnixMilli(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	return harness
}

func (h *quotaObserveHarness) rateLimitsEvent(t *testing.T, resetsAt time.Time) provider.ProviderEvent {
	t.Helper()
	meta, err := json.Marshal(provider.RateLimitsSnapshot{
		Provider: string(provider.Claude),
		Limits: []provider.RateLimitEntry{
			{LimitID: "session", LimitName: "Current session", UsedPercent: 100, WindowMins: 300, ResetsAt: resetsAt.Unix()},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return provider.ProviderEvent{Kind: provider.EventRateLimits, Meta: meta}
}

func (h *quotaObserveHarness) refusal() provider.ProviderEvent {
	return provider.ProviderEvent{
		Kind:    provider.EventError,
		Content: "Claude usage limit reached",
		Meta:    json.RawMessage(`{"api_error_enum":"rate_limit","fatal":true,"expect_turn_complete":true}`),
	}
}

func (h *quotaObserveHarness) awaitOutcome(t *testing.T) engine.Outcome {
	t.Helper()
	select {
	case outcome := <-h.outcomes:
		return outcome
	case <-time.After(2 * time.Second):
		t.Fatal("the attempt never completed")
		return engine.Outcome{}
	}
}

// The recorder sits ABOVE every guard in `observe`, because recording a quota
// window has no state-machine effect and the guards exist to stop an attempt
// from ACTING. Claude's ordering between `rate_limit_event` and
// `assistant.error` is not verified anywhere: if the snapshot lands while the
// backoff ladder is armed — or while a retry's first turn is still awaited —
// dropping it would make the whole mechanism silently inert on Claude.
func TestQuotaWindowsAreRecordedThroughEveryObserveGuard(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*workflowAttempt)
	}{
		{name: "backoff ladder armed", setup: func(a *workflowAttempt) {
			a.timerMode = workflowTimerBackoff
			a.timer = &fakeWorkflowTimer{active: true}
		}},
		{name: "awaiting a retry's first turn", setup: func(a *workflowAttempt) {
			a.awaitingRetryStart = true
		}},
		{name: "a turn in flight", setup: func(*workflowAttempt) {}},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newQuotaObserveHarness(t, "run-record")
			test.setup(harness.attempt)
			resetsAt := harness.now.Add(3 * time.Hour)

			harness.runner.observe(harness.runKey, harness.rateLimitsEvent(t, resetsAt))

			harness.runner.mu.Lock()
			limits := harness.attempt.rateLimits
			harness.runner.mu.Unlock()
			if len(limits) != 1 || limits[0].ResetsAt != resetsAt.Unix() {
				t.Fatalf("recorded windows = %+v, want the reported five-hour boundary", limits)
			}
		})
	}
}

// The consequence of the rule above: a snapshot that arrived while the ladder
// was armed is still the boundary the next refusal parks on. Without the hoist
// this is the Claude case that parks generic `retries-exhausted` forever.
func TestQuotaParkUsesAWindowRecordedDuringBackoff(t *testing.T) {
	harness := newQuotaObserveHarness(t, "run-late-window")
	harness.attempt.timerMode = workflowTimerBackoff
	harness.attempt.timer = &fakeWorkflowTimer{active: true}
	resetsAt := harness.now.Add(3 * time.Hour)

	harness.runner.observe(harness.runKey, harness.rateLimitsEvent(t, resetsAt))

	// The ladder's next turn starts, which is what clears the backoff mode.
	harness.runner.mu.Lock()
	harness.runner.disarmTimerLocked(harness.attempt)
	harness.runner.mu.Unlock()

	harness.runner.observe(harness.runKey, harness.refusal())

	outcome := harness.awaitOutcome(t)
	resumeAt := resetsAt.Add(workflowQuotaResumeJitter("run-late-window"))
	if outcome.Kind != engine.OutcomeTransientExhausted ||
		!strings.Contains(outcome.Detail, "this run resumes itself at "+resumeAt.Local().Format(time.RFC3339)) {
		t.Fatalf("outcome = %+v, want a quota park scheduled from the window recorded during backoff", outcome)
	}
	scheduled, err := harness.app.store.WorkItemAutoResumeAt("run-late-window")
	if err != nil || scheduled != resumeAt.UnixMilli() {
		t.Fatalf("persisted schedule = %d (err %v), want %d", scheduled, err, resumeAt.UnixMilli())
	}
}

// A schedule that could not be written is a different park, and the cause is
// composed from what the write DID rather than from what it was asked to do: a
// park promising "this run resumes itself at 19:58" when nothing armed is a run
// that never comes back and a human who was told not to look at it.
func TestQuotaParkWithAFailedScheduleWriteMakesNoPromise(t *testing.T) {
	harness := newQuotaObserveHarnessOpts(t, "run-unwritable", false)
	resetsAt := harness.now.Add(3 * time.Hour)
	harness.runner.observe(harness.runKey, harness.rateLimitsEvent(t, resetsAt))

	harness.runner.observe(harness.runKey, harness.refusal())

	outcome := harness.awaitOutcome(t)
	if outcome.Kind != engine.OutcomeTransientExhausted {
		t.Fatalf("outcome kind = %v, want the park to still happen", outcome.Kind)
	}
	if !strings.Contains(outcome.Detail, "the limit resets "+resetsAt.Local().Format(time.RFC3339)) {
		t.Fatalf("cause %q dropped the reset boundary, which is true either way", outcome.Detail)
	}
	if strings.Contains(outcome.Detail, "resumes itself") {
		t.Fatalf("cause %q promises a self-resume nothing armed", outcome.Detail)
	}
}
