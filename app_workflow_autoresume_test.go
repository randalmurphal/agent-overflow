package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflowhost"
)

// A self-resume has two halves that must agree — the durable column and the
// live timer — and the contract is that every way OUT of the park takes both
// back. These tests drive the transitions rather than the states: a schedule
// that survives a cancel is a run that resumes work somebody already abandoned.

// armedWorkflowTimer is one installed self-resume. It records its delay and
// whether it is still live, and `fire` is what the test uses in place of
// waiting out a boundary measured in hours.
type armedWorkflowTimer struct {
	callback func()
	delay    time.Duration
	stopped  bool
}

func (t *armedWorkflowTimer) Stop() bool {
	wasArmed := !t.stopped
	t.stopped = true
	return wasArmed
}

func (t *armedWorkflowTimer) Reset(time.Duration) bool { return !t.stopped }

// autoResumeHarness is an app whose self-resume clock and timers are the
// test's. Nothing here spawns: the fire's resume reaches an app with no engine
// and is refused there, so the assertions are about the schedule, and the
// end-to-end proof that the resume actually runs the phase again lives in
// TestWorkflowQuotaParkResumesItselfWhenTheLimitReturns.
type autoResumeHarness struct {
	app *App
	now time.Time
	// pending is where the injected constructor leaves the timer it just
	// built. The constructor is handed no run id — the id lives in the
	// callback's closure — so `arm` is what associates the two, from the call
	// it made itself rather than by reading the registry under test.
	pending *armedWorkflowTimer
}

func newAutoResumeHarness(t *testing.T) *autoResumeHarness {
	t.Helper()
	harness := &autoResumeHarness{
		app: newTestAppWithStore(t),
		now: time.Unix(1_700_000_000, 0),
	}
	harness.app.workflowAutoResume.nowFn = func() time.Time { return harness.now }
	harness.app.workflowAutoResume.newTimer = func(delay time.Duration, fire func()) workflowhost.Timer {
		timer := &armedWorkflowTimer{callback: fire, delay: delay}
		harness.pending = timer
		return timer
	}
	return harness
}

// startEngine gives the harness the engine `WorkflowScheduleResume` requires.
// Most of these tests deliberately go without one — an engine-less app is how
// they observe a resume that fails — so it is opt-in.
func (h *autoResumeHarness) startEngine(t *testing.T) {
	t.Helper()
	if err := h.app.initWorkflowEngine(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.app.workflowEngine.Close() })
}

func (h *autoResumeHarness) parkedRun(t *testing.T, itemID string, reason engine.Reason) store.WorkItem {
	t.Helper()
	item := store.WorkItem{
		ID: itemID, ProjectID: defaultTestProjectID, Goal: "quota " + itemID,
		WorkflowID: "wf", WorkflowScope: "shared",
		State: string(engine.StateNeedsHuman), Reason: string(reason),
		Source: "manual", CreatedAt: h.now.UnixMilli(), StartedAt: h.now.UnixMilli(),
	}
	if err := h.app.store.CreateWorkItem(item); err != nil {
		t.Fatal(err)
	}
	return item
}

// arm is the runner's write: the column and the timer, in that order.
func (h *autoResumeHarness) arm(t *testing.T, itemID string, at time.Time) *armedWorkflowTimer {
	t.Helper()
	if err := h.app.setWorkflowAutoResume(itemID, at); err != nil {
		t.Fatal(err)
	}
	timer := h.pending
	if timer == nil {
		t.Fatalf("arming %s installed no timer", itemID)
	}
	h.pending = nil
	return timer
}

func (h *autoResumeHarness) scheduleAt(t *testing.T, itemID string) int64 {
	t.Helper()
	at, err := h.app.store.WorkItemAutoResumeAt(itemID)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// transition drives the one hook every repair passes through.
func (h *autoResumeHarness) transition(itemID string, from, to engine.State, reason engine.Reason) {
	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: itemID, From: from, To: to, Reason: reason,
	})
}

// The park writes both halves, and the delay the timer holds is the moment the
// cause states — not a duration recomputed from a second clock reading.
func TestWorkflowAutoResumeArmsTheColumnAndTheTimerTogether(t *testing.T) {
	harness := newAutoResumeHarness(t)
	item := harness.parkedRun(t, "run-arm", engine.ReasonProviderRetriesExhausted)
	resumeAt := harness.now.Add(3 * time.Hour)

	timer := harness.arm(t, item.ID, resumeAt)
	if got := harness.scheduleAt(t, item.ID); got != resumeAt.UnixMilli() {
		t.Fatalf("persisted schedule = %d, want %d", got, resumeAt.UnixMilli())
	}
	if timer.delay != 3*time.Hour || timer.stopped {
		t.Fatalf("timer = %+v, want a live 3h delay", timer)
	}
}

// park → the timer fires → the column is left standing, and the registration
// with it, so the run's own transition to `running` is what clears both. The
// fire clearing the column itself would make a resume that failed look like one
// that succeeded; dropping the registration would leave the column set on the
// very run the resume repaired, and every later boot would re-arm a schedule
// that had already been spent.
//
// (The end-to-end success path — fire, resume, run reaches done, schedule gone
// — is TestWorkflowQuotaParkResumesItselfWhenTheLimitReturns. This fixture has
// no engine, so what it pins is the bookkeeping either way.)
func TestWorkflowAutoResumeFiresIntoAResumeAndLeavesTheColumnToTheTransition(t *testing.T) {
	harness := newAutoResumeHarness(t)
	item := harness.parkedRun(t, "run-fire", engine.ReasonProviderRetriesExhausted)
	timer := harness.arm(t, item.ID, harness.now.Add(2*time.Hour))

	timer.callback()

	if got := harness.scheduleAt(t, item.ID); got == 0 {
		t.Fatal("the fire cleared the schedule of a run that is still parked")
	}
	// The registration outlives the fire on purpose: it is what lets a
	// transition reach the column, through the same hook a manual resume goes
	// through. (Here it is the retry re-arm holding the slot; on the success
	// path it is the fire's own spent timer.)
	harness.app.workflowAutoResume.mu.Lock()
	_, stillRegistered := harness.app.workflowAutoResume.timers[item.ID]
	harness.app.workflowAutoResume.mu.Unlock()
	if !stillRegistered {
		t.Fatal("the fire dropped its registration, so no transition can clear the column")
	}

	harness.transition(item.ID, engine.StateNeedsHuman, engine.StateRunning, "")

	if got := harness.scheduleAt(t, item.ID); got != 0 {
		t.Fatalf("schedule after the resume transition = %d, want 0", got)
	}
	harness.app.workflowAutoResume.mu.Lock()
	_, stillRegistered = harness.app.workflowAutoResume.timers[item.ID]
	harness.app.workflowAutoResume.mu.Unlock()
	if stillRegistered {
		t.Fatal("the transition left a registration behind")
	}
}

// Every transition OUT of the park disarms, and the park itself does not. The
// order is what the runner depends on: the schedule is written immediately
// before the park that carries it.
func TestWorkflowAutoResumeTransitionsClearOrPreserveTheSchedule(t *testing.T) {
	for _, test := range []struct {
		name    string
		from    engine.State
		to      engine.State
		reason  engine.Reason
		cleared bool
	}{
		{name: "manual resume", from: engine.StateNeedsHuman, to: engine.StateRunning, cleared: true},
		{name: "cancel", from: engine.StateNeedsHuman, to: engine.StateCancelled, cleared: true},
		{name: "the run finished", from: engine.StateRunning, to: engine.StateDone, cleared: true},
		{name: "the run failed", from: engine.StateRunning, to: engine.StateFailed, cleared: true},
		{
			name: "a park keeps the schedule the runner just wrote",
			from: engine.StateRunning, to: engine.StateNeedsHuman,
			reason: engine.ReasonProviderRetriesExhausted,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newAutoResumeHarness(t)
			item := harness.parkedRun(t, "run-transition", engine.ReasonProviderRetriesExhausted)
			timer := harness.arm(t, item.ID, harness.now.Add(5*time.Hour))

			harness.transition(item.ID, test.from, test.to, test.reason)

			schedule := harness.scheduleAt(t, item.ID)
			if test.cleared {
				if schedule != 0 || !timer.stopped {
					t.Fatalf("schedule=%d timerStopped=%v, want both cleared", schedule, timer.stopped)
				}
				return
			}
			if schedule == 0 || timer.stopped {
				t.Fatalf("schedule=%d timerStopped=%v, want both intact", schedule, timer.stopped)
			}
		})
	}
}

// A cleared schedule cannot come back: a timer that fires after the run was
// repaired must not resume work somebody already took over.
func TestWorkflowAutoResumeFireAfterARepairResumesNothing(t *testing.T) {
	harness := newAutoResumeHarness(t)
	// A pre-v56 row retains the same timer cleanup contract.
	item := harness.parkedRun(t, "run-repaired", engine.ReasonRetriesExhausted)
	timer := harness.arm(t, item.ID, harness.now.Add(4*time.Hour))

	// The repair: a human resumed it, so the run is running and the schedule
	// is gone. The timer is stopped, but a real one can already be in flight.
	harness.transition(item.ID, engine.StateNeedsHuman, engine.StateRunning, "")
	if err := harness.app.store.UpdateWorkItemState(
		item.ID, string(engine.StateRunning), "", harness.now.UnixMilli(),
	); err != nil {
		t.Fatal(err)
	}

	timer.callback()

	if got := harness.scheduleAt(t, item.ID); got != 0 {
		t.Fatalf("a late fire re-armed the schedule: %d", got)
	}
}

// A run that moved to a park a bare resume does NOT continue is not the park
// this schedule was written for. The fire clears the column rather than
// leaving a dead schedule to be re-armed on every restart from here on.
func TestWorkflowAutoResumeFireOnANonContinuableParkClearsTheSchedule(t *testing.T) {
	harness := newAutoResumeHarness(t)
	item := harness.parkedRun(t, "run-moved", engine.ReasonProviderRetriesExhausted)
	timer := harness.arm(t, item.ID, harness.now.Add(6*time.Hour))
	if err := harness.app.store.UpdateWorkItemState(
		item.ID, string(engine.StateNeedsHuman), string(engine.ReasonGate), harness.now.UnixMilli(),
	); err != nil {
		t.Fatal(err)
	}

	timer.callback()

	if got := harness.scheduleAt(t, item.ID); got != 0 {
		t.Fatalf("a stale schedule survived its own fire: %d", got)
	}
}

// A restart re-arms from the column, which is the whole reason the column
// exists: a five-day stall outlives every process that could hold a timer.
// A moment that already passed is armed a short way out rather than fired into
// the crash rebuild that is still deciding what the run is.
func TestWorkflowAutoResumeSweepReArmsAcrossARestart(t *testing.T) {
	harness := newAutoResumeHarness(t)
	future := harness.parkedRun(t, "run-future", engine.ReasonProviderRetriesExhausted)
	elapsed := harness.parkedRun(t, "run-elapsed", engine.ReasonProviderRetriesExhausted)
	harness.arm(t, future.ID, harness.now.Add(8*time.Hour))
	harness.arm(t, elapsed.ID, harness.now.Add(30*time.Minute))

	// The restart: the timers die with the process, the column does not.
	harness.app.stopWorkflowAutoResumes()
	harness.now = harness.now.Add(2 * time.Hour)

	harness.app.sweepWorkflowAutoResumes()

	rearmed := map[string]time.Duration{}
	harness.app.workflowAutoResume.mu.Lock()
	for itemID, timer := range harness.app.workflowAutoResume.timers {
		rearmed[itemID] = timer.(*armedWorkflowTimer).delay
	}
	harness.app.workflowAutoResume.mu.Unlock()

	if delay, ok := rearmed[future.ID]; !ok || delay != 6*time.Hour {
		t.Fatalf("future schedule re-armed at %v (present=%v), want 6h", delay, ok)
	}
	if delay, ok := rearmed[elapsed.ID]; !ok || delay != workflowAutoResumeBootDelay {
		t.Fatalf("elapsed schedule re-armed at %v (present=%v), want the boot delay %v",
			delay, ok, workflowAutoResumeBootDelay)
	}
}

// `run resume --at` takes both forms, and both are resolved against the clock
// the timer will actually run on.
func TestParseWorkflowResumeAtAcceptsBothFormsAndRefusesTheRest(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	absolute := now.Add(36 * time.Hour).UTC().Format(time.RFC3339)
	parsed, err := parseWorkflowResumeAt(absolute, now)
	if err != nil || !parsed.Equal(now.Add(36*time.Hour)) {
		t.Fatalf("RFC 3339 = %v, %v", parsed, err)
	}
	parsed, err = parseWorkflowResumeAt("  +36h  ", now)
	if err != nil || !parsed.Equal(now.Add(36*time.Hour)) {
		t.Fatalf("duration = %v, %v", parsed, err)
	}

	for _, test := range []struct{ value, wants string }{
		{"", "a time is required"},
		{"tomorrow", "neither RFC 3339 nor a duration"},
		{"+banana", "is not a duration"},
		{"+0s", "is not in the future"},
		{"+-2h", "is not in the future"},
		{now.Add(-time.Hour).UTC().Format(time.RFC3339), "is not in the future"},
		{"+900h", "cancel the run instead"},
	} {
		if _, err := parseWorkflowResumeAt(test.value, now); err == nil ||
			!strings.Contains(err.Error(), test.wants) {
			t.Fatalf("parse %q error = %v, want one naming %q", test.value, err, test.wants)
		}
	}
}

// The manual verb arms the same mechanism, and refuses everywhere a scheduled
// bare resume would have nothing to continue. The refusal names the engine's
// own membership so it cannot fall behind a reason the engine later admits.
func TestWorkflowScheduleResumeArmsAContinuableParkAndRefusesTheRest(t *testing.T) {
	harness := newAutoResumeHarness(t)
	harness.startEngine(t)
	item := harness.parkedRun(t, "run-scheduled", engine.ReasonProviderRetriesExhausted)

	armed, err := harness.app.WorkflowScheduleResume(context.Background(), item.ID, "+36h")
	if err != nil {
		t.Fatal(err)
	}
	want := harness.now.Add(36 * time.Hour)
	if armed != want.Local().Format(time.RFC3339) {
		t.Fatalf("armed = %q, want %q", armed, want.Local().Format(time.RFC3339))
	}
	if got := harness.scheduleAt(t, item.ID); got != want.UnixMilli() {
		t.Fatalf("persisted schedule = %d, want %d", got, want.UnixMilli())
	}

	for _, test := range []struct {
		name   string
		state  engine.State
		reason engine.Reason
	}{
		{name: "a gate is decided, not waited out", state: engine.StateNeedsHuman, reason: engine.ReasonGate},
		{name: "a stuck park re-enters fresh", state: engine.StateNeedsHuman, reason: engine.ReasonStuck},
		{name: "a running run is not parked", state: engine.StateRunning},
		{name: "a done run has nothing left", state: engine.StateDone},
	} {
		t.Run(test.name, func(t *testing.T) {
			refused := harness.parkedRun(t, "run-refuse-"+string(test.state)+string(test.reason), test.reason)
			if err := harness.app.store.UpdateWorkItemState(
				refused.ID, string(test.state), string(test.reason), harness.now.UnixMilli(),
			); err != nil {
				t.Fatal(err)
			}
			_, err := harness.app.WorkflowScheduleResume(context.Background(), refused.ID, "+2h")
			if err == nil {
				t.Fatal("a run a bare resume cannot continue accepted a schedule")
			}
			for _, reason := range engine.ContinuableReasons() {
				if !strings.Contains(err.Error(), string(reason)) {
					t.Fatalf("refusal %q does not name the continuable reason %q", err, reason)
				}
			}
			if got := harness.scheduleAt(t, refused.ID); got != 0 {
				t.Fatalf("a refused schedule was still written: %d", got)
			}
		})
	}
}

// A resume that fails is transient by construction — the fire has already
// confirmed the run is still the park this schedule was written for — so it
// re-arms rather than spending the timer. Without it the schedule gets its next
// chance at the next boot, which on a desktop app is days: the exact stall this
// mechanism exists to end.
func TestWorkflowAutoResumeRetriesAFailedResume(t *testing.T) {
	harness := newAutoResumeHarness(t)
	item := harness.parkedRun(t, "run-retry", engine.ReasonProviderRetriesExhausted)
	timer := harness.arm(t, item.ID, harness.now.Add(2*time.Hour))
	// No engine on this app, so WorkflowResumeItem refuses — one instance of the
	// transient class (an engine still starting, a store that blinked).
	if harness.app.workflowEngine != nil {
		t.Fatal("the fixture unexpectedly has an engine; the refusal under test would not happen")
	}

	timer.callback()

	if got := harness.scheduleAt(t, item.ID); got == 0 {
		t.Fatal("a failed resume cleared the schedule instead of retrying it")
	}
	retry := harness.pending
	if retry == nil || retry.delay != workflowAutoResumeRetryDelay || retry.stopped {
		t.Fatalf("retry timer = %+v, want a live %v re-arm", retry, workflowAutoResumeRetryDelay)
	}
	harness.app.workflowAutoResume.mu.Lock()
	registered := harness.app.workflowAutoResume.timers[item.ID]
	harness.app.workflowAutoResume.mu.Unlock()
	if registered != workflowhost.Timer(retry) {
		t.Fatalf("registry holds %+v, want the retry timer", registered)
	}

	// The retry re-checks resumability like any other fire, so a run somebody
	// repaired in the meantime clears itself instead of looping forever.
	if err := harness.app.store.UpdateWorkItemState(
		item.ID, string(engine.StateRunning), "", harness.now.UnixMilli(),
	); err != nil {
		t.Fatal(err)
	}
	retry.callback()
	if got := harness.scheduleAt(t, item.ID); got != 0 {
		t.Fatalf("a retry into a repaired run left the schedule armed: %d", got)
	}
}

// Scheduling requires the engine even though nothing resumes now: what is being
// armed IS a WorkflowResumeItem, and an engine-less app would take the schedule,
// persist it, and fail the resume on this boot and every boot after.
func TestWorkflowScheduleResumeRefusesWithoutAnEngine(t *testing.T) {
	harness := newAutoResumeHarness(t)
	item := harness.parkedRun(t, "run-no-engine", engine.ReasonProviderRetriesExhausted)

	if _, err := harness.app.WorkflowScheduleResume(context.Background(), item.ID, "+36h"); err == nil {
		t.Fatal("an engine-less app armed a schedule it could never honour")
	}
	if got := harness.scheduleAt(t, item.ID); got != 0 {
		t.Fatalf("the refused schedule was still persisted: %d", got)
	}
	harness.app.workflowAutoResume.mu.Lock()
	registered := len(harness.app.workflowAutoResume.timers)
	harness.app.workflowAutoResume.mu.Unlock()
	if registered != 0 {
		t.Fatalf("the refused schedule armed %d timers", registered)
	}
}
