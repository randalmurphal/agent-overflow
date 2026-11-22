package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflowapp"
)

// restRunFixture is a spawned worker thread with workflow runs on a live
// engine whose members hold a provider session until stopped.
type restRunFixture struct {
	app      *App
	project  store.Project
	caller   store.Thread
	worker   store.Thread
	spawn    string
	startLog string
}

func newRestRunFixture(t *testing.T) *restRunFixture {
	t.Helper()
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
	startLog := filepath.Join(t.TempDir(), "starts")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": writeRecordingStallClaude(t, startLog)}); err != nil {
		t.Fatal(err)
	}
	project := testutil.EnsureProject(t, app.store, t.TempDir())
	project = mustReloadProject(t, app.store, project.ID)
	writeReliabilityProfile(t, configRoot, project.Slug, "watchdog: 1h\n  backoff: [5ms]\n")
	startWorkflowEngineForTest(t, app, configRoot)
	thread := func(title string) store.Thread {
		row, err := app.CreateThread(t.Context(), CreateThreadOptions{
			ProjectID: project.ID, Provider: string(provider.Claude), Model: "claude-opus-4-7", Mode: threadmode.ModeChat,
		})
		if err != nil {
			t.Fatalf("create %s: %v", title, err)
		}
		return row
	}
	f := &restRunFixture{app: app, project: project, caller: thread("spawner"), worker: thread("worker"), spawn: "spawn-run-starter", startLog: startLog}
	if err := app.store.InsertThreadRequest(store.ThreadRequest{
		Token: f.spawn, CallerThreadID: f.caller.ID, Kind: store.ThreadRequestSpawn,
		TargetThreadID: f.worker.ID, State: store.ThreadRequestAccepted,
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

// startRun starts a run the worker asked for, as run_start does for an
// interactive thread: agent-sourced and bound to it.
func (f *restRunFixture) startRun(t *testing.T, goal string) (store.WorkItem, string) {
	t.Helper()
	item, err := f.app.startWorkflowRun(f.project.ID, "reliability-flow", "shared", goal, json.RawMessage(`{}`), nil, "", false, workflowSourceAgent, "")
	if err != nil {
		t.Fatal(err)
	}
	scope := transport.CallerScope{Kind: transport.ScopeKindInteractive, ThreadID: f.worker.ID, ProjectID: f.project.ID}
	if reason := f.app.bindOriginThread(scope, item); reason != "" {
		t.Fatalf("bind the run to the worker: %s", reason)
	}
	return item, awaitLivePhaseSession(t, f.app, item.ID)
}

// stop presses Stop on the worker's row.
func (f *restRunFixture) stop(t *testing.T) ThreadAgentStop {
	t.Helper()
	stop, err := f.app.StopThreadAgentRow(t.Context(), f.caller.ID, f.spawn)
	if err != nil {
		t.Fatalf("StopThreadAgentRow: %v", err)
	}
	return stop
}

// settle lets every wake the run's transitions queued be delivered.
func (f *restRunFixture) settle() {
	f.app.workflowApplication().WaitWake()
	f.app.waitAgentNoticeWork()
	f.app.workflowApplication().WaitWake()
}

// workerMessages counts the messages written to the worker's timeline.
func (f *restRunFixture) workerMessages(t *testing.T) int {
	t.Helper()
	items, err := f.app.store.ListItems(f.worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, item := range items {
		if item.Kind == string(provider.ItemUserText) {
			count++
		}
	}
	return count
}

// assertAtRest checks that no wake reached the worker once the wakes have
// settled: a wake is written to its timeline or queue before it starts a
// session, so the check does not race the start.
func (f *restRunFixture) assertAtRest(t *testing.T) {
	t.Helper()
	items, err := f.app.store.ListItems(f.worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Kind == string(provider.ItemUserText) {
			t.Fatalf("a message reached the stopped worker: %q", item.Summary)
		}
	}
	if queued, err := f.app.store.ListFlushQueueItems(f.worker.ID); err != nil || len(queued) != 0 {
		t.Fatalf("the stopped worker's queue = %+v, %v; want it empty", queued, err)
	}
	if _, live := f.app.sessionManager().get(f.worker.ID); live {
		t.Fatal("the stopped worker started again")
	}
}

// The Stop pauses the run the worker started and ends its members'
// sessions, so the run's next transition, the pause itself, does not start
// the worker again; Resume from the workflow surface continues the run on
// the session it parked on.
func TestAStopPausesTheWorkflowRunTheThreadStarted(t *testing.T) {
	t.Parallel()
	f := newRestRunFixture(t)
	item, member := f.startRun(t, "hold")
	waitUntil(t, 8*time.Second, func() bool {
		row, err := f.app.store.GetThread(member)
		return err == nil && row.SessionRef == "stall"
	})
	if stop := f.stop(t); len(stop.Failures) != 0 {
		t.Fatalf("the Stop failed: %q", stop.Failures)
	}
	waitForWorkflowItem(t, f.app, item.ID, engine.StateNeedsHuman, engine.ReasonPaused)
	if _, live := f.app.sessionManager().get(member); live {
		t.Fatal("the paused run's member kept its session")
	}
	f.settle()
	f.assertAtRest(t)
	if bound, err := f.app.store.GetWorkItem(item.ID); err != nil || bound.OriginThreadID != f.worker.ID {
		t.Fatalf("the run's binding = %q, %v; want it still reporting to the worker", bound.OriginThreadID, err)
	}

	if err := f.app.WorkflowResumeItem(context.Background(), item.ID, "", false); err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, f.app, item.ID, engine.StateRunning, "")
	waitUntil(t, 8*time.Second, func() bool {
		starts := readLines(t, f.startLog)
		return len(starts) == 2 && strings.Contains(starts[1], "--resume stall")
	})
	if _, live := f.app.sessionManager().get(f.worker.ID); live {
		t.Fatal("the run's resume started the stopped worker")
	}
}

// A run of the worker's that is already paused or has finished is left as it
// is, a paused run's member session ended, and the Stop reports no failure
// for it.
func TestAStopLeavesARunThatIsAlreadyAtRestAsItIs(t *testing.T) {
	t.Parallel()
	f := newRestRunFixture(t)
	paused, member := f.startRun(t, "paused")
	cancelled, _ := f.startRun(t, "cancelled")
	if err := f.app.WorkflowPauseItem(context.Background(), paused.ID); err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, f.app, paused.ID, engine.StateNeedsHuman, engine.ReasonPaused)
	if err := f.app.WorkflowCancelItem(context.Background(), cancelled.ID); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 8*time.Second, func() bool {
		row, err := f.app.store.GetWorkItem(cancelled.ID)
		return err == nil && row.State == string(engine.StateCancelled)
	})
	f.settle()

	if stop := f.stop(t); len(stop.Failures) != 0 {
		t.Fatalf("the Stop failed: %q", stop.Failures)
	}
	f.settle()
	for id, want := range map[string]string{paused.ID: string(engine.StateNeedsHuman), cancelled.ID: string(engine.StateCancelled)} {
		row, err := f.app.store.GetWorkItem(id)
		if err != nil || row.State != want || row.OriginThreadID != f.worker.ID {
			t.Fatalf("run %s = %s bound to %q, %v; want %s and still bound", id, row.State, row.OriginThreadID, err, want)
		}
	}
	if _, live := f.app.sessionManager().get(f.worker.ID); live {
		t.Fatal("the stopped worker has a session")
	}
	if _, live := f.app.sessionManager().get(member); live {
		t.Fatal("the paused run's member kept its session")
	}
}

// A run that cannot be paused is named in the Stop's failures, and the rest
// of the Stop still runs: here the store holds a running run the engine
// does not, which the pause refuses.
func TestAStopNamesARunItCouldNotPauseAndRunsTheRest(t *testing.T) {
	t.Parallel()
	f := newRestRunFixture(t)
	orphan := store.WorkItem{
		ID: "run-the-engine-lost", ProjectID: f.project.ID, Goal: "lost", WorkflowID: "reliability-flow",
		WorkflowScope: "shared", State: string(engine.StateRunning), Source: workflowSourceAgent,
		OriginThreadID: f.worker.ID, CreatedAt: time.Now().UnixMilli(),
	}
	if err := f.app.store.CreateWorkItem(orphan); err != nil {
		t.Fatal(err)
	}
	loop := registerTestWake(t, f.app, store.AgentWake{
		ThreadID: f.worker.ID, Kind: store.AgentWakeLoop, Label: "board", Note: "n",
		IntervalSeconds: 3600, NextAt: time.Now().Add(time.Hour).UnixMilli(),
	})

	stop := f.stop(t)
	if len(stop.Failures) != 1 || !strings.Contains(stop.Failures[0], "pause its workflow run run-the-engine-lost") {
		t.Fatalf("the Stop's failures = %q, want the run it could not pause named", stop.Failures)
	}
	row, found, err := f.app.store.GetAgentWake(loop.ID)
	if err != nil || !found || row.State != store.AgentWakeCancelled {
		t.Fatalf("the worker's loop = %+v found %v, %v; want the rest of the Stop to have run", row, found, err)
	}
}

// A thread a running run works in is that run's, even when the run also
// reports to it: the Stop names the run and leaves the run and the thread's
// session alone, rather than stopping the run's work or ending a session the
// run would start again.
func TestAStopLeavesAThreadARunWorksInToTheRun(t *testing.T) {
	t.Parallel()
	f := newRestRunFixture(t)
	item, member := f.startRun(t, "member")
	if _, err := f.app.store.UpdateWorkItemOriginThread(item.ID, member); err != nil {
		t.Fatal(err)
	}
	if err := f.app.store.InsertThreadRequest(store.ThreadRequest{
		Token: "spawn-of-a-member", CallerThreadID: f.caller.ID, Kind: store.ThreadRequestSpawn,
		TargetThreadID: member, State: store.ThreadRequestAccepted,
	}); err != nil {
		t.Fatal(err)
	}

	stop, err := f.app.StopThreadAgentRow(t.Context(), f.caller.ID, "spawn-of-a-member")
	if err != nil {
		t.Fatalf("StopThreadAgentRow: %v", err)
	}
	if len(stop.Failures) != 1 || !strings.Contains(stop.Failures[0], "works in workflow run "+item.ID) {
		t.Fatalf("the Stop's failures = %q, want the run named", stop.Failures)
	}
	if row, err := f.app.store.GetWorkItem(item.ID); err != nil || row.State != string(engine.StateRunning) {
		t.Fatalf("the run = %s, %v; want it still running", row.State, err)
	}
	if _, live := f.app.sessionManager().get(member); !live {
		t.Fatal("the Stop ended the session the run works in")
	}
}

// An agent's thread_cancel of a thread it spawned pauses the runs every
// thread below it started, at any depth.
func TestAnAgentsCancelPausesTheRunsOfEveryThreadBelow(t *testing.T) {
	t.Parallel()
	f := newRestRunFixture(t)
	grand, err := f.app.CreateThread(t.Context(), CreateThreadOptions{
		ProjectID: f.project.ID, Provider: string(provider.Claude), Model: "claude-opus-4-7", Mode: threadmode.ModeChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.app.store.InsertThreadRequest(store.ThreadRequest{
		Token: "spawn-of-the-grandchild", CallerThreadID: f.worker.ID, Kind: store.ThreadRequestSpawn,
		TargetThreadID: grand.ID, State: store.ThreadRequestAccepted,
	}); err != nil {
		t.Fatal(err)
	}
	own, _ := f.startRun(t, "the worker's")
	below, err := f.app.startWorkflowRun(f.project.ID, "reliability-flow", "shared", "the grandchild's", json.RawMessage(`{}`), nil, "", false, workflowSourceAgent, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.store.UpdateWorkItemOriginThread(below.ID, grand.ID); err != nil {
		t.Fatal(err)
	}
	awaitLivePhaseSession(t, f.app, below.ID)

	report, err := f.app.threadTools().cancelThread(t.Context(), f.app.threadAgentCaller(f.caller.ID), f.worker.ID)
	if err != nil || report.Effect != threadtools.EffectThreadStopped {
		t.Fatalf("cancel = %+v, %v; want the worker brought to rest", report, err)
	}
	for _, id := range []string{own.ID, below.ID} {
		waitForWorkflowItem(t, f.app, id, engine.StateNeedsHuman, engine.ReasonPaused)
	}
	f.settle()
	f.assertAtRest(t)
	for _, id := range []string{f.worker.ID, grand.ID} {
		if items, err := f.app.store.ListItems(id); err != nil || slices.ContainsFunc(items, func(item store.Item) bool {
			return item.Kind == string(provider.ItemUserText)
		}) {
			t.Fatalf("a wake reached %s after the cancel (%v)", id, err)
		}
	}
}

// A Stop cancels the resume scheduled on a parked run the thread started:
// neither its timer, firing anyway, nor the boot sweep resumes the run, and
// Resume from the workflows view still does.
func TestAStopCancelsTheScheduledResumeOfAParkedRun(t *testing.T) {
	t.Parallel()
	f := newRestRunFixture(t)
	var mu sync.Mutex
	var timers []*armedWorkflowTimer
	f.app.workflowApplication().ConfigureAutoResumeTimer(func(delay time.Duration, fire func()) workflowapp.Timer {
		mu.Lock()
		defer mu.Unlock()
		timer := &armedWorkflowTimer{callback: fire, delay: delay}
		timers = append(timers, timer)
		return timer
	})
	item, _ := f.startRun(t, "parked")
	if err := f.app.WorkflowPauseItem(context.Background(), item.ID); err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, f.app, item.ID, engine.StateNeedsHuman, engine.ReasonPaused)
	if _, err := f.app.WorkflowScheduleResume(context.Background(), item.ID, "+1h"); err != nil {
		t.Fatal(err)
	}
	f.settle()

	if stop := f.stop(t); len(stop.Failures) != 0 {
		t.Fatalf("the Stop failed: %q", stop.Failures)
	}
	if at, err := f.app.store.WorkItemAutoResumeAt(item.ID); err != nil || at != 0 {
		t.Fatalf("the run's schedule after the Stop = %d, %v; want it cleared", at, err)
	}
	mu.Lock()
	armed := slices.Clone(timers)
	mu.Unlock()
	for _, timer := range armed {
		timer.callback()
	}
	f.app.workflowApplication().SweepAutoResumes()
	if count := f.app.workflowApplication().AutoResumeTimerCount(); count != 0 {
		t.Fatalf("%d resumes armed after the Stop and a boot sweep, want none", count)
	}
	f.settle()
	if row, err := f.app.store.GetWorkItem(item.ID); err != nil || row.State != string(engine.StateNeedsHuman) || row.Reason != string(engine.ReasonPaused) {
		t.Fatalf("the run after its timer fired = %s(%s), %v; want it still paused", row.State, row.Reason, err)
	}
	if _, live := f.app.sessionManager().get(f.worker.ID); live {
		t.Fatal("the stopped worker started again")
	}

	if err := f.app.WorkflowResumeItem(context.Background(), item.ID, "", false); err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, f.app, item.ID, engine.StateRunning, "")
}

// A scheduled resume that fires after the Stop listed its parked run, before
// the Stop disarms it, leaves the run running for the Stop's pause; the rest
// that pause makes wakes nobody, however late the wake queue gets to it, and
// a later deliberate resume still works.
func TestAStopHoldsTheWakeOfARunAScheduledResumeSetRunning(t *testing.T) {
	t.Parallel()
	f := newRestRunFixture(t)
	var mu sync.Mutex
	var timers []*armedWorkflowTimer
	f.app.workflowApplication().ConfigureAutoResumeTimer(func(delay time.Duration, fire func()) workflowapp.Timer {
		mu.Lock()
		defer mu.Unlock()
		timer := &armedWorkflowTimer{callback: fire, delay: delay}
		timers = append(timers, timer)
		return timer
	})
	item, _ := f.startRun(t, "parked")
	if err := f.app.WorkflowPauseItem(context.Background(), item.ID); err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, f.app, item.ID, engine.StateNeedsHuman, engine.ReasonPaused)
	if _, err := f.app.WorkflowScheduleResume(context.Background(), item.ID, "+1h"); err != nil {
		t.Fatal(err)
	}
	f.settle()

	fired := false
	f.app.restRunsListedForTest = func(runs []store.WorkItem) {
		if fired || len(runs) != 1 || runs[0].ID != item.ID || runs[0].State != string(engine.StateNeedsHuman) {
			return
		}
		fired = true
		mu.Lock()
		armed := slices.Clone(timers)
		mu.Unlock()
		for _, timer := range armed {
			timer.callback()
		}
		waitForWorkflowItem(t, f.app, item.ID, engine.StateRunning, "")
	}
	release := f.app.workflowApplication().BlockWakeQueueForTesting()
	defer release()
	if stop := f.stop(t); len(stop.Failures) != 0 {
		t.Fatalf("the Stop failed: %q", stop.Failures)
	}
	if !fired {
		t.Fatal("the scheduled resume never fired between the listing and the pause")
	}
	waitForWorkflowItem(t, f.app, item.ID, engine.StateNeedsHuman, engine.ReasonPaused)
	before := f.workerMessages(t)
	release()
	f.settle()
	if after := f.workerMessages(t); after != before {
		t.Fatalf("the stopped worker took %d messages after the Stop, want none", after-before)
	}
	if queued, err := f.app.store.ListFlushQueueItems(f.worker.ID); err != nil || len(queued) != 0 {
		t.Fatalf("the stopped worker's queue = %+v, %v; want it empty", queued, err)
	}
	if _, live := f.app.sessionManager().get(f.worker.ID); live {
		t.Fatal("the stopped worker started again")
	}

	if err := f.app.WorkflowResumeItem(context.Background(), item.ID, "", false); err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, f.app, item.ID, engine.StateRunning, "")
}

// A Stop pauses every run that reports to the thread, a run a person started
// and bound to it included, and leaves alone a run the thread started that a
// person bound elsewhere or unbound.
func TestAStopPausesTheRunsThatReportToTheThread(t *testing.T) {
	t.Parallel()
	f := newRestRunFixture(t)
	bound, err := f.app.WorkflowStartRun(f.project.ID, "reliability-flow", "shared", "a person's", json.RawMessage(`{}`), nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.WorkflowBindThread(bound.ID, f.worker.ID); err != nil {
		t.Fatal(err)
	}
	awaitLivePhaseSession(t, f.app, bound.ID)
	repointed, _ := f.startRun(t, "the worker's, bound elsewhere")
	if _, err := f.app.WorkflowBindThread(repointed.ID, f.caller.ID); err != nil {
		t.Fatal(err)
	}
	// Two runs hold the provider's capacity; this one waits for it, running.
	unbound, err := f.app.startWorkflowRun(f.project.ID, "reliability-flow", "shared", "the worker's, unbound", json.RawMessage(`{}`), nil, "", false, workflowSourceAgent, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.store.UpdateWorkItemOriginThread(unbound.ID, f.worker.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.WorkflowUnbindThread(unbound.ID); err != nil {
		t.Fatal(err)
	}

	if stop := f.stop(t); len(stop.Failures) != 0 {
		t.Fatalf("the Stop failed: %q", stop.Failures)
	}
	waitForWorkflowItem(t, f.app, bound.ID, engine.StateNeedsHuman, engine.ReasonPaused)
	f.settle()
	for _, id := range []string{repointed.ID, unbound.ID} {
		if row, err := f.app.store.GetWorkItem(id); err != nil || row.State != string(engine.StateRunning) {
			t.Fatalf("run %s = %s, %v; want it still running", id, row.State, err)
		}
	}
}

// A running workflow run that reports to a worker keeps it from rest: its
// rest reads waiting on the run by its goal, and so does its spawn's row,
// framed as the run is bound. The run paused for a person, and the turn its
// notice started stopped, it is at rest; the run resumed keeps it from rest
// again, framed by the run's state change alone.
func TestARunningWorkflowRunKeepsTheThreadItReportsToFromRest(t *testing.T) {
	t.Parallel()
	f := newRestRunFixture(t)
	item, _ := f.startRun(t, "ship it")
	if rest, err := f.app.threadRest(f.worker.ID); err != nil || rest.unrest != workerWaiting || rest.detail != "Workflow · ship it" {
		t.Fatalf("rest with the run running = %+v, %v; want waiting on the run", rest, err)
	}
	row := func() ThreadAgentRow {
		rows, err := f.app.ListThreadAgentRows(t.Context(), f.caller.ID)
		if err != nil {
			t.Fatal(err)
		}
		row, _ := agentRowByToken(rows.Rows, f.spawn)
		return row
	}
	waitUntil(t, 10*time.Second, func() bool {
		r := row()
		return r.Worker == workerWaiting && r.Activity == "Workflow · ship it"
	})
	if err := f.app.WorkflowPauseItem(context.Background(), item.ID); err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, f.app, item.ID, engine.StateNeedsHuman, engine.ReasonPaused)
	// The pause's notice starts a turn of the worker; with its session
	// stopped, the run paused, it is at rest.
	f.settle()
	if err := f.app.StopSession(f.worker.ID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	waitUntil(t, 10*time.Second, func() bool { return row().Worker == "" })
	if rest, err := f.app.threadRest(f.worker.ID); err != nil || rest.unrest != "" {
		t.Fatalf("rest with the run paused = %+v, %v; want at rest", rest, err)
	}
	// Resumed, the run keeps it from rest: no message reaches the worker
	// for that, so only the run's state change brings its row back to
	// waiting.
	if err := f.app.WorkflowResumeItem(context.Background(), item.ID, "", false); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		r := row()
		return r.Worker == workerWaiting && r.Activity == "Workflow · ship it"
	})
}
