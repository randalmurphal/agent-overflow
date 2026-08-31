package app

import (
	"agent-overflow/internal/notify"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/scheduler"
	"agent-overflow/internal/workflowapp"
	"agent-overflow/internal/workflowhost"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// automationHarness is a real app with a real engine and scheduler over a
// temp config root, with the engine globally paused so a started run is
// admitted and persisted without any provider session existing.
type automationHarness struct {
	app       *App
	projectID string
}

func newAutomationHarness(t *testing.T) *automationHarness {
	t.Helper()
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	writeAutomationWorkflowFixture(t, configRoot)
	startWorkflowEngineForTest(t, app, configRoot)
	if err := app.WorkflowSetGlobalPause(true); err != nil {
		t.Fatal(err)
	}
	return &automationHarness{app: app, projectID: defaultTestProjectID}
}

func (h *automationHarness) input() WorkflowAutomationInput {
	return WorkflowAutomationInput{
		ProjectID: h.projectID, WorkflowID: "automation-flow", WorkflowScope: "shared",
		Name: "Nightly audit", Enabled: true,
		Trigger: json.RawMessage(`{"kind":"cron","expr":"0 3 * * *"}`),
		Seeds:   json.RawMessage(`{"goal":"audit the API"}`),
	}
}

func writeAutomationWorkflowFixture(t *testing.T, configRoot string) {
	t.Helper()
	dir := filepath.Join(configRoot, "workflows")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	workflow := `id: automation-flow
name: Automation flow
inputs:
  goal:
    schema:
      type: string
phases:
  - id: run
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: run.md
    access: read-only
    inputs:
      goal:
        schema:
          type: string
    outputs:
      complete:
        schema:
          type: boolean
    gate:
      routes:
        - to: done
cleanup: manual
`
	for name, content := range map[string]string{
		"automation-flow.yaml": workflow,
		"run.md":               "Audit {{goal}} and return the envelope.",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWorkflowCreateAutomationRefusesUnrunnableDefinitions(t *testing.T) {
	h := newAutomationHarness(t)
	cases := []struct {
		name   string
		mutate func(*WorkflowAutomationInput)
		want   string
	}{
		{
			name: "invalid cron",
			mutate: func(in *WorkflowAutomationInput) {
				in.Trigger = json.RawMessage(`{"kind":"cron","expr":"every night"}`)
			},
			want: "fields",
		},
		{
			name:   "cron field out of range",
			mutate: func(in *WorkflowAutomationInput) { in.Trigger = json.RawMessage(`{"kind":"cron","expr":"99 3 * * *"}`) },
			want:   "is invalid",
		},
		{
			name:   "unknown trigger kind",
			mutate: func(in *WorkflowAutomationInput) { in.Trigger = json.RawMessage(`{"kind":"webhook"}`) },
			want:   "must be cron or event",
		},
		{
			name:   "event outside the closed set",
			mutate: func(in *WorkflowAutomationInput) { in.Trigger = json.RawMessage(`{"kind":"event","on":"phase-done"}`) },
			want:   "event trigger on must be one of",
		},
		{
			name:   "unknown workflow",
			mutate: func(in *WorkflowAutomationInput) { in.WorkflowID = "no-such-flow" },
			want:   "no-such-flow",
		},
		{
			name:   "unknown project",
			mutate: func(in *WorkflowAutomationInput) { in.ProjectID = "no-such-project" },
			want:   "no rows in result set",
		},
		{
			name:   "reserved trigger seed",
			mutate: func(in *WorkflowAutomationInput) { in.Seeds = json.RawMessage(`{"trigger":"mine"}`) },
			want:   `seed "trigger" is reserved`,
		},
		{
			name:   "reserved job-notes seed",
			mutate: func(in *WorkflowAutomationInput) { in.Seeds = json.RawMessage(`{"job-notes":"mine"}`) },
			want:   `seed "job-notes" is reserved`,
		},
		{
			name:   "seeds that are not an object",
			mutate: func(in *WorkflowAutomationInput) { in.Seeds = json.RawMessage(`["goal"]`) },
			want:   "seeds must be an object",
		},
		{
			name: "malformed condition",
			mutate: func(in *WorkflowAutomationInput) {
				in.Condition = json.RawMessage(`{"eq":{"ref":"a","value":1},"exists":"b"}`)
			},
			want: "condition is malformed",
		},
		{
			name:   "missing name",
			mutate: func(in *WorkflowAutomationInput) { in.Name = "  " },
			want:   "name are required",
		},
		{
			name:   "bad scope",
			mutate: func(in *WorkflowAutomationInput) { in.WorkflowScope = "global" },
			want:   "scope must be project or shared",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			input := h.input()
			testCase.mutate(&input)
			if _, err := h.app.WorkflowCreateAutomation(input); err == nil {
				t.Fatalf("create succeeded, want a refusal containing %q", testCase.want)
			} else if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error = %v, want it to contain %q", err, testCase.want)
			}
			automations, err := h.app.WorkflowListAutomations(h.projectID)
			if err != nil {
				t.Fatal(err)
			}
			if len(automations) != 0 {
				t.Fatalf("a refused create persisted a row: %#v", automations)
			}
		})
	}
}

func TestWorkflowAutomationCRUDRoundTrip(t *testing.T) {
	h := newAutomationHarness(t)
	created, err := h.app.WorkflowCreateAutomation(h.input())
	if err != nil {
		t.Fatal(err)
	}
	if created.TriggerKind != "cron" || created.TriggerSummary != "cron 0 3 * * *" {
		t.Fatalf("created view = %#v", created)
	}
	if created.NextFireAt <= time.Now().UnixMilli() {
		t.Fatalf("next fire = %d, want a future time", created.NextFireAt)
	}
	if created.TriggerError != "" {
		t.Fatalf("trigger error = %q", created.TriggerError)
	}

	// Notes are their own surface: a definition update must not clobber what the
	// last run wrote back (§5 update-job-notes).
	if err := h.app.WorkflowSetJobNotes(created.ID, "the flaky suite is quarantined"); err != nil {
		t.Fatal(err)
	}
	updated := h.input()
	updated.Name = "Nightly audit v2"
	updated.Trigger = json.RawMessage(`{"kind":"event","on":"item-failed","workflowId":"automation-flow"}`)
	view, err := h.app.WorkflowUpdateAutomation(created.ID, updated)
	if err != nil {
		t.Fatal(err)
	}
	if view.Name != "Nightly audit v2" || view.TriggerSummary != "event item-failed on automation-flow" {
		t.Fatalf("updated view = %#v", view)
	}
	if view.Notes != "the flaky suite is quarantined" {
		t.Fatalf("update clobbered the job notes: %q", view.Notes)
	}
	// An event trigger has no next fire, and saying otherwise would be a lie.
	if view.NextFireAt != 0 {
		t.Fatalf("event trigger reported next fire %d", view.NextFireAt)
	}

	// A disabled automation reports no next fire either.
	if err := h.app.WorkflowSetAutomationEnabled(created.ID, false); err != nil {
		t.Fatal(err)
	}
	disabledCron := h.input()
	if _, err := h.app.WorkflowUpdateAutomation(created.ID, disabledCron); err != nil {
		t.Fatal(err)
	}
	if err := h.app.WorkflowSetAutomationEnabled(created.ID, false); err != nil {
		t.Fatal(err)
	}
	listed, err := h.app.WorkflowListAutomations(h.projectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Enabled || listed[0].NextFireAt != 0 {
		t.Fatalf("listed = %#v, want one disabled row with no next fire", listed)
	}

	if err := h.app.WorkflowDeleteAutomation(created.ID); err != nil {
		t.Fatal(err)
	}
	listed, err = h.app.WorkflowListAutomations(h.projectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("listed after delete = %#v", listed)
	}
}

// A trigger that stopped parsing — an older shape, a hand-edited row — is
// reported on the row rather than dropped in silence.
func TestWorkflowListAutomationsSurfacesBrokenTriggers(t *testing.T) {
	h := newAutomationHarness(t)
	now := time.Now().UnixMilli()
	if err := h.app.store.CreateAutomation(store.Automation{
		ID: "legacy", ProjectID: h.projectID, WorkflowID: "automation-flow", WorkflowScope: "shared",
		Name: "Legacy", Enabled: true, Trigger: json.RawMessage(`{"cron":"0 3 * * *"}`),
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	listed, err := h.app.WorkflowListAutomations(h.projectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("listed = %#v", listed)
	}
	if listed[0].TriggerError == "" || listed[0].NextFireAt != 0 || listed[0].TriggerSummary != "" {
		t.Fatalf("broken row = %#v, want a standing trigger error and no schedule", listed[0])
	}
}

func TestWorkflowRunAutomationNowStartsThroughTheOneStartPath(t *testing.T) {
	h := newAutomationHarness(t)
	input := h.input()
	// Run now bypasses the condition: pressing the button is the decision. This
	// condition is false for a manual fire and would skip a scheduled one.
	input.Condition = json.RawMessage(`{"eq":{"ref":"trigger.kind","value":"event"}}`)
	created, err := h.app.WorkflowCreateAutomation(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.app.WorkflowSetJobNotes(created.ID, "last run left the migration half applied"); err != nil {
		t.Fatal(err)
	}

	item, err := h.app.WorkflowRunAutomationNow(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if item.Source != scheduler.Source || item.SourceRef != created.ID {
		t.Fatalf("run provenance = (%q, %q), want (automation, %s)", item.Source, item.SourceRef, created.ID)
	}
	if item.Goal != "Nightly audit (run now)" {
		t.Fatalf("goal = %q", item.Goal)
	}
	if item.StepMode {
		t.Fatal("an unattended run started in step mode")
	}

	var seeds map[string]any
	if err := json.Unmarshal(item.Seeds, &seeds); err != nil {
		t.Fatal(err)
	}
	if seeds["goal"] != "audit the API" {
		t.Fatalf("stored seeds were dropped: %#v", seeds)
	}
	if seeds[scheduler.JobNotesVariable] != "last run left the migration half applied" {
		t.Fatalf("job notes seed = %#v", seeds[scheduler.JobNotesVariable])
	}
	trigger, ok := seeds[scheduler.TriggerVariable].(map[string]any)
	if !ok || trigger["kind"] != string(scheduler.KindManual) {
		t.Fatalf("trigger seed = %#v", seeds[scheduler.TriggerVariable])
	}

	// The fire is recorded on the row, and the second press is refused loudly
	// while that run is still live — no queueing, no overlap, no silent skip.
	listed, err := h.app.WorkflowListAutomations(h.projectID)
	if err != nil {
		t.Fatal(err)
	}
	if listed[0].LastRunItemID != item.ID || listed[0].LastFiredAt == 0 {
		t.Fatalf("fire record = %#v", listed[0])
	}
	_, err = h.app.WorkflowRunAutomationNow(created.ID)
	if err == nil {
		t.Fatal("a second Run now while the first run is live succeeded")
	}
	if !strings.Contains(err.Error(), item.ID) || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("error = %v, want it to name the active run", err)
	}
	listed, err = h.app.WorkflowListAutomations(h.projectID)
	if err != nil {
		t.Fatal(err)
	}
	if listed[0].SkipCount != 0 {
		t.Fatalf("a manual refusal recorded a skip: %#v", listed[0])
	}

	// Once that run is out of the way, the automation can fire again.
	if err := h.app.store.UpdateWorkItemState(item.ID, "done", "", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	second, err := h.app.WorkflowRunAutomationNow(created.ID)
	if err != nil {
		t.Fatalf("Run now after the previous run settled: %v", err)
	}
	if second.ID == item.ID {
		t.Fatal("Run now returned the previous run")
	}
}

func TestWorkflowRunAutomationNowWorksWhileDisabledAndBroken(t *testing.T) {
	h := newAutomationHarness(t)
	created, err := h.app.WorkflowCreateAutomation(h.input())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.app.WorkflowSetAutomationEnabled(created.ID, false); err != nil {
		t.Fatal(err)
	}
	// A stored trigger that no longer parses says nothing about the workflow the
	// human is asking to run, so Run now still works on it.
	stored, err := h.app.store.GetAutomation(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.Trigger = json.RawMessage(`{"kind":"cron","expr":"nope"}`)
	if err := h.app.store.UpdateAutomation(stored); err != nil {
		t.Fatal(err)
	}

	item, err := h.app.WorkflowRunAutomationNow(created.ID)
	if err != nil {
		t.Fatalf("Run now on a disabled, broken-trigger automation: %v", err)
	}
	if item.SourceRef != created.ID {
		t.Fatalf("run provenance = %q", item.SourceRef)
	}
}

func TestWorkflowAutomationRPCsRequireIdentifiers(t *testing.T) {
	h := newAutomationHarness(t)
	for name, err := range map[string]error{
		"update":  mustErr(h.app.WorkflowUpdateAutomation("  ", h.input())),
		"delete":  h.app.WorkflowDeleteAutomation("  "),
		"enable":  h.app.WorkflowSetAutomationEnabled("  ", true),
		"run now": mustErr(h.app.WorkflowRunAutomationNow("  ")),
		"list":    mustErrList(h.app.WorkflowListAutomations("  ")),
	} {
		if err == nil {
			t.Errorf("%s with a blank identifier succeeded", name)
		}
	}
}

func mustErr[T any](_ T, err error) error { return err }

func mustErrList(_ []WorkflowAutomationView, err error) error { return err }

const (
	workflowAutoResumeBootDelay  = workflowapp.AutoResumeBootDelay
	workflowAutoResumeRetryDelay = workflowapp.AutoResumeRetryDelay
)

func (a *App) setWorkflowAutoResume(itemID string, at time.Time) error {
	return a.workflowApplication().SetAutoResume(itemID, at)
}

func (a *App) stopWorkflowAutoResumes() { a.workflowApplication().StopAutoResumes() }

func (a *App) sweepWorkflowAutoResumes() { a.workflowApplication().SweepAutoResumes() }

func parseWorkflowResumeAt(value string, now time.Time) (time.Time, error) {
	return workflowapp.ParseResumeAt(value, now)
}

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
	harness.app.workflowApplication().ConfigureClock(func() time.Time { return harness.now })
	harness.app.workflowApplication().ConfigureAutoResumeTimer(func(delay time.Duration, fire func()) workflowapp.Timer {
		timer := &armedWorkflowTimer{callback: fire, delay: delay}
		harness.pending = timer
		return timer
	})
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
	t.Cleanup(func() { _ = h.app.workflowApplication().Engine().Close() })
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
	stillRegistered := harness.app.workflowApplication().AutoResumeRegistered(item.ID)
	if !stillRegistered {
		t.Fatal("the fire dropped its registration, so no transition can clear the column")
	}

	harness.transition(item.ID, engine.StateNeedsHuman, engine.StateRunning, "")

	if got := harness.scheduleAt(t, item.ID); got != 0 {
		t.Fatalf("schedule after the resume transition = %d, want 0", got)
	}
	stillRegistered = harness.app.workflowApplication().AutoResumeRegistered(item.ID)
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
	for _, itemID := range []string{future.ID, elapsed.ID} {
		if timer, ok := harness.app.workflowApplication().RegisteredAutoResumeTimer(itemID); ok {
			rearmed[itemID] = timer.(*armedWorkflowTimer).delay
		}
	}

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
	if harness.app.workflowApplication().Engine() != nil {
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
	registered, _ := harness.app.workflowApplication().RegisteredAutoResumeTimer(item.ID)
	if registered != workflowapp.Timer(retry) {
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
	registered := harness.app.workflowApplication().AutoResumeTimerCount()
	if registered != 0 {
		t.Fatalf("the refused schedule armed %d timers", registered)
	}
}

func TestWorkflowBindThreadRefusesThreadsARunCannotReportInto(t *testing.T) {
	h := newWakeHarness(t)
	root := h.run(t, "bind-root", engine.StateRunning, "")
	child := store.WorkItem{
		ID: "bind-child", ProjectID: defaultTestProjectID, Goal: "called", WorkflowID: "c",
		WorkflowScope: "shared", State: string(engine.StateRunning), Source: "call",
		ParentItemID: root.ID, ParentPhaseID: "call", ParentAttempt: 1, CallDepth: 1, CreatedAt: 2,
	}
	if err := h.app.store.CreateWorkItem(child); err != nil {
		t.Fatal(err)
	}
	if _, err := h.app.store.CreateProject(store.Project{
		ID: "other-project", Name: "other", Path: "/tmp/other", Slug: "other", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	makeThread := func(id, projectID, mode string, archived bool) store.Thread {
		thread := store.Thread{
			ID: id, ProjectID: projectID, ProjectPath: "/tmp/project", Title: id,
			Provider: string(provider.Claude), Model: "sonnet", Mode: mode,
			Archived: archived, CreatedAt: 1, UpdatedAt: 1,
		}
		if err := h.app.store.CreateThread(thread); err != nil {
			t.Fatal(err)
		}
		return thread
	}
	makeThread("cross-project", "other-project", threadmode.ModeChat, false)
	makeThread("phase-thread", defaultTestProjectID, threadmode.ModeWorkflow, false)
	makeThread("archived-thread", defaultTestProjectID, threadmode.ModeChat, true)
	makeThread("ok-thread", defaultTestProjectID, threadmode.ModeChat, false)

	for _, tc := range []struct{ name, itemID, threadID, want string }{
		{"called run", child.ID, "ok-thread", "bind the run that called it"},
		{"unknown run", "nope", "ok-thread", "no rows in result set"},
		{"missing thread id", root.ID, "  ", "thread id is required"},
		{"unknown thread", root.ID, "ghost", "no rows in result set"},
		{"cross project", root.ID, "cross-project", "belongs to project"},
		{"workflow phase thread", root.ID, "phase-thread", "binds a conversation thread"},
		{"archived thread", root.ID, "archived-thread", "is archived"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := h.app.WorkflowBindThread(tc.itemID, tc.threadID); err == nil {
				t.Fatalf("bind succeeded, want a refusal containing %q", tc.want)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
			stored, err := h.app.store.GetWorkItem(root.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.OriginThreadID != "" {
				t.Fatalf("a refused bind still wrote a binding: %q", stored.OriginThreadID)
			}
		})
	}
}

func TestWorkflowBindAndUnbindThreadRoundTrip(t *testing.T) {
	h := newWakeHarness(t)
	first := h.chatThread(t, "bind-first")
	second := h.chatThread(t, "bind-second")
	item := h.run(t, "bind-run", engine.StateRunning, "")

	bound, err := h.app.WorkflowBindThread(item.ID, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bound.OriginThreadID != first.ID {
		t.Fatalf("bound to %q, want %q", bound.OriginThreadID, first.ID)
	}
	// Rebinding replaces rather than accumulating: one run, one conversation.
	rebound, err := h.app.WorkflowBindThread(item.ID, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rebound.OriginThreadID != second.ID {
		t.Fatalf("rebound to %q, want %q", rebound.OriginThreadID, second.ID)
	}
	unbound, err := h.app.WorkflowUnbindThread(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unbound.OriginThreadID != "" {
		t.Fatalf("unbind left %q", unbound.OriginThreadID)
	}
	// An unbound run rests without waking anything.
	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: item.ID, ProjectID: item.ProjectID, From: engine.StateRunning, To: engine.StateDone,
	})
	h.drain()
	if sends, queued, _, _ := h.snapshot(); len(sends) != 0 || len(queued) != 0 {
		t.Fatalf("unbound run still delivered: sends=%d queued=%d", len(sends), len(queued))
	}
}

func TestWorkflowUnbindRefusesACalledRun(t *testing.T) {
	h := newWakeHarness(t)
	root := h.run(t, "unbind-root", engine.StateRunning, "")
	child := store.WorkItem{
		ID: "unbind-child", ProjectID: defaultTestProjectID, Goal: "called", WorkflowID: "c",
		WorkflowScope: "shared", State: string(engine.StateRunning), Source: "call",
		ParentItemID: root.ID, ParentPhaseID: "call", ParentAttempt: 1, CallDepth: 1, CreatedAt: 2,
	}
	if err := h.app.store.CreateWorkItem(child); err != nil {
		t.Fatal(err)
	}
	if _, err := h.app.WorkflowUnbindThread(child.ID); err == nil ||
		!strings.Contains(err.Error(), "bind the run that called it") {
		t.Fatalf("unbind of a called run = %v, want a refusal", err)
	}
}

// Thread deletion must clear the binding rather than leaving a dangling one:
// a run whose origin thread is gone has to fall back to the overlay, not keep
// trying to wake a thread that no longer exists.
func TestDeletingABoundThreadClearsTheBinding(t *testing.T) {
	h := newWakeHarness(t)
	item := h.run(t, "orphan-run", engine.StateRunning, "")
	thread := h.chatThread(t, "orphan-thread")
	if _, err := h.app.WorkflowBindThread(item.ID, thread.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.app.DeleteThread(thread.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := h.app.store.GetWorkItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.OriginThreadID != "" {
		t.Fatalf("deleting the bound thread left binding %q", stored.OriginThreadID)
	}
	// And the run rests silently rather than waking a thread that is gone.
	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: item.ID, ProjectID: item.ProjectID, From: engine.StateRunning, To: engine.StateDone,
	})
	h.drain()
	if sends, queued, _, errorTexts := h.snapshot(); len(sends) != 0 || len(queued) != 0 || len(errorTexts) != 0 {
		t.Fatalf("orphaned run delivered: sends=%d queued=%d errors=%v", len(sends), len(queued), errorTexts)
	}
}

func TestWorkflowArtifactAdapterPreservesTheWireModel(t *testing.T) {
	workspace := t.TempDir()
	source := filepath.Join(workspace, "report.txt")
	contents := []byte("artifact contents")
	if err := os.WriteFile(source, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	dataRoot := t.TempDir()
	if err := workflowhost.CaptureArtifact(dataRoot, "run-1", "report", workspace, "report.txt"); err != nil {
		t.Fatal(err)
	}

	artifacts, err := listWorkflowArtifacts(dataRoot, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("artifacts = %+v", artifacts)
	}
	artifact := artifacts[0]
	if artifact.Name != "report" || artifact.Path != filepath.Join(workflowhost.ArtifactDir(dataRoot, "run-1"), "report.txt") || artifact.Size != int64(len(contents)) || artifact.ModTime == 0 {
		t.Fatalf("wire artifact = %+v", artifact)
	}
}

func TestWorkflowJobNotesBoundedAndUnknownRejected(t *testing.T) {
	app, _ := setupE2EApp(t)
	automation := store.Automation{
		ID: "automation", ProjectID: "project", WorkflowID: "wf", WorkflowScope: "shared",
		Name: "Nightly", Enabled: true, Trigger: json.RawMessage(`{}`), CreatedAt: 1, UpdatedAt: 1,
	}
	if err := app.store.CreateAutomation(automation); err != nil {
		t.Fatal(err)
	}
	if err := app.WorkflowSetJobNotes(automation.ID, "continue here"); err != nil {
		t.Fatal(err)
	}
	if notes, err := app.WorkflowGetJobNotes(automation.ID); err != nil || notes != "continue here" {
		t.Fatalf("notes = %q err=%v", notes, err)
	}
	if err := app.WorkflowSetJobNotes(automation.ID, strings.Repeat("x", notify.MaxBodyBytes+1)); err == nil {
		t.Fatal("oversized notes unexpectedly succeeded")
	}
	if _, err := app.WorkflowGetJobNotes("missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown notes error = %v, want sql.ErrNoRows", err)
	}
	if err := app.WorkflowSetJobNotes("missing", "notes"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown set error = %v, want sql.ErrNoRows", err)
	}
}

func TestWorkflowListDefinitionsIncludesValidation(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	app.configDir = configRoot
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, app.store, repo)
	projectRow, err := app.store.GetProject(projectRow.ID)
	if err != nil {
		t.Fatal(err)
	}
	writeWorkflowListingFixtures(t, configRoot, projectRow.Slug)
	catalog, err := app.WorkflowListDefinitions(projectRow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.BaseBranch != "main" {
		t.Fatalf("catalog metadata = %+v", catalog)
	}
	listings := catalog.Workflows
	if len(listings) != 2 {
		t.Fatalf("listings = %+v", listings)
	}
	byID := make(map[string]WorkflowDefinitionListing, len(listings))
	for _, listing := range listings {
		byID[listing.ID] = listing
	}
	valid := byID["valid"]
	if !valid.Valid || !valid.AllBindingsAvailable || valid.PhaseCount != 2 || valid.HumanGateCount != 1 || len(valid.Phases) != 2 || valid.Phases[1].Provider != "codex" {
		t.Fatalf("valid listing = %+v", valid)
	}
	if len(valid.Inputs) != 4 || valid.Inputs[0].Name != "approved" || valid.Inputs[0].Type != "boolean" || !valid.Inputs[0].Required ||
		valid.Inputs[1].Name != "mode" || valid.Inputs[1].Type != "string" || len(valid.Inputs[1].Enum) != 2 || valid.Inputs[1].Required ||
		valid.Inputs[2].Name != "notes" || !valid.Inputs[2].Multiline || !valid.Inputs[2].Required ||
		valid.Inputs[3].Name != "source" || valid.Inputs[3].Format != "path" || !valid.Inputs[3].Required {
		t.Fatalf("valid inputs = %+v", valid.Inputs)
	}
	invalid := byID["invalid"]
	if invalid.Valid || invalid.AllBindingsAvailable || !strings.Contains(invalid.FirstValidationError, "not bindable") {
		t.Fatalf("invalid listing = %+v", invalid)
	}
}

func TestWorkflowItemDetailAndListCostsIncludeUsage(t *testing.T) {
	app := newTestAppWithStore(t)
	snapshot, err := json.Marshal(engine.Snapshot{Workflow: def.Workflow{
		Outputs: map[string]def.WorkflowOutput{
			"summary": {From: "verify.result.summary"},
			"report":  {From: "verify.report", Artifact: true},
		},
		Phases: []def.Phase{
			{ID: "verify", Driver: def.DriverTool, Check: "go-test"},
			{ID: "build", Driver: def.DriverAgent},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	item := store.WorkItem{
		ID: "usage-item", ProjectID: defaultTestProjectID, Goal: "measure", WorkflowID: "wf",
		WorkflowScope: "shared", State: string(engine.StateDone), Source: "manual", CreatedAt: 1,
		Disposition: json.RawMessage(`{"action":"merged","policy":"manual","at":2}`),
		Digest:      json.RawMessage(`{"whatHappened":"detail only","whatItNeeds":"nothing"}`),
		Snapshot:    snapshot,
	}
	if err := app.store.CreateWorkItem(item); err != nil {
		t.Fatal(err)
	}
	if err := app.store.AppendUsage([]store.UsageLedgerRow{
		{CreatedAt: 2, ProjectID: defaultTestProjectID, WorkItemID: item.ID, ThreadID: "phase", Provider: "claude", Model: "m", InputTokens: 8, OutputTokens: 5, CostUSD: 1.25},
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.store.CreateWorkItemPhase(store.WorkItemPhase{
		ItemID: item.ID, PhaseID: "verify", Attempt: 1, ThreadID: "phase",
		InputEnvelope:  json.RawMessage(`{"input":true}`),
		OutputEnvelope: json.RawMessage(`{"status":"done","outputs":{"result":{"summary":"All checks passed"},"report":"report.md"}}`),
		GateTrace:      json.RawMessage(`{"trace":true}`),
		Intervention:   json.RawMessage(`{"kind":"manual"}`), NarrativePath: "/tmp/narrative",
		Status: "failed", StartedAt: 1, EndedAt: 2,
	}); err != nil {
		t.Fatal(err)
	}
	detail, err := app.WorkflowGetItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Usage.TotalTokens != 13 || detail.Usage.CostUSD != 1.25 {
		t.Fatalf("detail usage = %+v", detail.Usage)
	}
	if len(detail.CheckPhaseIDs) != 1 || detail.CheckPhaseIDs[0] != "verify" ||
		len(detail.Phases) != 1 || len(detail.Phases[0].OutputEnvelope) == 0 ||
		detail.Outputs["summary"] != "All checks passed" || len(detail.Outputs) != 1 {
		t.Fatalf("detail view = %+v", detail)
	}
	wire, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	for _, omitted := range []string{"snapshot", "inputEnvelope", "gateTrace", "intervention", "narrativePath"} {
		if strings.Contains(string(wire), `"`+omitted+`"`) {
			t.Fatalf("detail wire includes omitted field %q: %s", omitted, wire)
		}
	}
	if !strings.Contains(string(wire), `"outputEnvelope"`) {
		t.Fatalf("detail wire omitted output envelope: %s", wire)
	}
	costs, err := app.WorkflowListItemCosts(defaultTestProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(costs) != 1 || costs[item.ID] != 1.25 {
		t.Fatalf("costs = %#v", costs)
	}
	summaries, err := app.WorkflowListItems(defaultTestProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || string(summaries[0].Disposition) != string(item.Disposition) || len(summaries[0].Digest) != 0 {
		t.Fatalf("summaries = %#v", summaries)
	}
}

// TestWorkflowListItemCostsRollUpRunTrees — every entry is a TREE total. A
// recursive campaign's spend lands almost entirely on its call children, and
// the overview lists only roots: before the rollup, a root's row priced its
// own coordination rows alone and a $1,500 campaign rendered as $10.
func TestWorkflowListItemCostsRollUpRunTrees(t *testing.T) {
	app := newTestAppWithStore(t)
	runs := []store.WorkItem{
		{ID: "root", Goal: "campaign"},
		{ID: "wave", Goal: "wave", ParentItemID: "root", ParentPhaseID: "run", ParentAttempt: 1, CallDepth: 1},
		{ID: "lane", Goal: "lane", ParentItemID: "wave", ParentPhaseID: "fan", ParentAttempt: 1, CallDepth: 2},
	}
	for _, item := range runs {
		item.ProjectID = defaultTestProjectID
		item.WorkflowID = "wf"
		item.WorkflowScope = "shared"
		item.State = string(engine.StateRunning)
		item.Source = "manual"
		item.CreatedAt = 1
		if err := app.store.CreateWorkItem(item); err != nil {
			t.Fatal(err)
		}
	}
	claudeRow := func(itemID, threadID string, costUSD float64) store.UsageLedgerRow {
		return store.UsageLedgerRow{
			CreatedAt: 2, ProjectID: defaultTestProjectID, WorkItemID: itemID, ThreadID: threadID,
			Provider: "claude", Model: "claude-opus-5", InputTokens: 10, OutputTokens: 5,
			CostUSD: costUSD, CostSource: "wire",
		}
	}
	if err := app.store.AppendUsage([]store.UsageLedgerRow{
		claudeRow("root", "root-phase", 1),
		claudeRow("wave", "wave-phase", 2),
		claudeRow("lane", "lane-phase", 4),
		// A ledger row whose run record no longer exists keeps its own entry:
		// ledger rows deliberately outlive the runs they attribute.
		claudeRow("deleted-run", "orphan-phase", 8),
	}); err != nil {
		t.Fatal(err)
	}
	costs, err := app.WorkflowListItemCosts(defaultTestProjectID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"root": 7, "wave": 6, "lane": 4, "deleted-run": 8}
	if len(costs) != len(want) {
		t.Fatalf("costs = %#v, want %#v", costs, want)
	}
	for itemID, total := range want {
		if costs[itemID] != total {
			t.Fatalf("costs[%s] = %v, want %v (all: %#v)", itemID, costs[itemID], total, costs)
		}
	}
}

func TestWorkflowStartRunResolvesBaseBranchAndCancelKeepsTheRecord(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	writeWorkspaceWorkflow(t, configRoot, "done")
	// Paused: every run is admitted and persisted, but its first phase is held,
	// so no provider process starts and the assertions stay deterministic.
	if _, err := app.settings.Update(map[string]any{"workflowPaused": true}); err != nil {
		t.Fatal(err)
	}
	if err := app.initWorkflowEngine(configRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.workflowApplication().Engine().Close() })
	projectRow := testutil.EnsureProject(t, app.store, testutil.InitGitRepo(t))
	projectRow = mustReloadProject(t, app.store, projectRow.ID)
	writeWorkspaceProfile(t, configRoot, projectRow.Slug, "\nbase_branch: main\n")
	if _, err := app.WorkflowStartRun(projectRow.ID, "missing", "shared", "invalid", json.RawMessage(`{}`), nil, "", false); err == nil {
		t.Fatal("unknown workflow id did not return synchronously")
	}
	if count, err := app.store.CountWorkItemsInStates(string(engine.StateRunning)); err != nil || count != 0 {
		t.Fatalf("unknown workflow persistence count = %d err=%v, want zero", count, err)
	}
	item, err := app.WorkflowStartRun(projectRow.ID, "workspace-flow", "shared", "cancel me", json.RawMessage(`{}`), nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if item.State != string(engine.StateRunning) || item.BaseBranch != "main" {
		t.Fatalf("started run = %+v, want running on main", item)
	}
	override, err := app.WorkflowStartRun(projectRow.ID, "workspace-flow", "shared", "override base", json.RawMessage(`{}`), nil, "release/v2", false)
	if err != nil {
		t.Fatal(err)
	}
	if override.BaseBranch != "release/v2" {
		t.Fatalf("override start base branch = %q", override.BaseBranch)
	}
	if err := app.WorkflowCancelItem(context.Background(), override.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.WorkflowStartRun(projectRow.ID, "workspace-flow", "shared", "invalid base", json.RawMessage(`{}`), nil, "--base", false); err == nil {
		t.Fatal("invalid base branch override succeeded")
	}
	if err := app.WorkflowCancelItem(context.Background(), item.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := app.store.GetWorkItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != string(engine.StateCancelled) || stored.WorktreePath != "" {
		t.Fatalf("cancelled item = %+v", stored)
	}
	if err := app.WorkflowCancelItem(context.Background(), item.ID); err == nil {
		t.Fatal("second cancellation unexpectedly succeeded")
	}
}

func writeWorkflowListingFixtures(t *testing.T, configRoot, slug string) {
	t.Helper()
	shared := filepath.Join(configRoot, "workflows")
	projectDir := filepath.Join(configRoot, "projects", slug)
	if err := os.MkdirAll(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	valid := `id: valid
name: Valid workflow
default_step_mode: true
inputs:
  approved:
    schema:
      type: boolean
  mode:
    optional: true
    schema:
      type: string
      enum: [fast, thorough]
  notes:
    schema:
      type: string
      multiline: true
  source:
    schema:
      type: string
      format: path
phases:
  - id: prepare
    driver: agent
    provider: codex
    model: gpt-5
    prompt: prompt.md
    access: read-only
    gate:
      routes:
        - to: work
  - id: work
    driver: agent
    provider: codex
    model: gpt-5
    prompt: prompt.md
    access: read-only
    gate:
      routes:
        - human:
            approve: done
            reject:
              loop: prepare
              max: 1
`
	invalid := `id: invalid
name: Invalid workflow
phases:
  - id: check
    driver: tool
    check: missing-check
    access: read-only
    gate:
      routes:
        - to: done
`
	for name, content := range map[string]string{"valid.yaml": valid, "invalid.yaml": invalid, "prompt.md": "Do the work."} {
		if err := os.WriteFile(filepath.Join(shared, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(projectDir, "profile.yaml"), []byte("base_branch: main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}
