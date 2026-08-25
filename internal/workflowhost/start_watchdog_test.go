package workflowhost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/profile"
)

// The bound on Runner.Start. A start that wedges pre-spawn used to leave the
// engine `runnerStarting` forever with a worker-less `running` attempt — no
// timer, no verb but a root pause, and a run map showing a phase that had been
// "starting" for an hour.

// startWatchdogFixture drives a real Runner.Start far enough to wedge it, with
// the start's own timers under the test's control.
type startWatchdogFixture struct {
	t        *testing.T
	store    *store.Store
	host     *fakeHost
	runner   *Runner
	project  store.Project
	itemID   string
	phaseID  string
	threadID string
	outcomes chan engine.Outcome

	mu sync.Mutex
	// timers keeps every timer armed for a delay, in arm order. A map holding
	// only the latest would let two timers armed for one delay silently shadow
	// each other, and awaitTimer would hand a test the wrong one.
	timers map[time.Duration][]*fakeWorkflowTimer
}

func newStartWatchdogFixture(t *testing.T) *startWatchdogFixture {
	t.Helper()
	dataStore := newTestStore(t)
	project := testutil.EnsureProject(t, dataStore, t.TempDir())
	fixture := &startWatchdogFixture{
		t: t, store: dataStore, host: &fakeHost{}, project: project,
		itemID: "wedged-item", phaseID: "work",
		outcomes: make(chan engine.Outcome, 4),
		timers:   make(map[time.Duration][]*fakeWorkflowTimer),
	}
	fixture.runner = newTestRunner(t, fixture.host, dataStore, staticWorkflowProfileSource{value: &profile.Profile{}})
	fixture.runner.newTimer = func(delay time.Duration, callback func()) Timer {
		timer := &fakeWorkflowTimer{callback: callback, delay: delay, active: true}
		fixture.mu.Lock()
		fixture.timers[delay] = append(fixture.timers[delay], timer)
		fixture.mu.Unlock()
		return timer
	}
	if err := dataStore.CreateWorkItem(store.WorkItem{
		ID: fixture.itemID, ProjectID: project.ID, Goal: "wedge the start",
		WorkflowID: "flow", WorkflowScope: "shared",
		State: string(engine.StateRunning), Source: "manual", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.CreateWorkItemPhase(store.WorkItemPhase{
		ItemID: fixture.itemID, PhaseID: fixture.phaseID, Attempt: 1,
		Status: "running", StartedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *startWatchdogFixture) phase() def.Phase {
	return def.Phase{
		ID: f.phaseID, Driver: def.DriverAgent, Shape: def.ShapeSingle,
		Provider: string(provider.Codex), Model: "gpt-5.5", Prompt: "do the work",
		Outputs: map[string]def.Variable{"ok": {Schema: def.JSONSchema{Type: "boolean"}}},
	}
}

func (f *startWatchdogFixture) request(launch engine.TurnLaunch) engine.RunRequest {
	phase := f.phase()
	return engine.RunRequest{
		Key:  engine.RunKey{ItemID: f.itemID, PhaseID: f.phaseID, Attempt: 1},
		Item: store.WorkItem{ID: f.itemID, ProjectID: f.project.ID},
		Workflow: def.Workflow{
			ID: "flow", Name: "Flow", Phases: []def.Phase{phase},
		},
		WorkspaceNeed: def.WorkspaceProjectRoot,
		Phase:         phase, Vars: map[string]any{}, Launch: launch,
	}
}

func (f *startWatchdogFixture) runKey() string {
	return workflowRunKey(engine.RunKey{ItemID: f.itemID, PhaseID: f.phaseID, Attempt: 1})
}

// seedPriorThread writes the workflow thread a continuation launch resumes onto,
// with a durable session cursor and no live process — the shape that sends a
// start into `startSessionTakingLock`.
func (f *startWatchdogFixture) seedPriorThread() string {
	f.t.Helper()
	now := time.Now().UnixMilli()
	thread := store.Thread{
		ID: "wedged-workflow-thread", ProjectID: f.project.ID, ProjectPath: f.project.Path,
		Title: "wedged", CreatedAt: now, UpdatedAt: now,
	}
	thread.Mode = "workflow"
	thread.Provider = string(provider.Codex)
	thread.Model = provider.NormalizeModelSlug(string(provider.Codex), "gpt-5.5")
	thread.WorkspacePath = f.project.Path
	thread.SessionRef = "codex-thread-cursor"
	if err := f.store.CreateThread(thread); err != nil {
		f.t.Fatal(err)
	}
	f.threadID = thread.ID
	return thread.ID
}

func (f *startWatchdogFixture) start(request engine.RunRequest) <-chan error {
	errs := make(chan error, 1)
	go func() {
		errs <- f.runner.Start(context.Background(), request, func() {}, func(outcome engine.Outcome) {
			f.outcomes <- outcome
		})
	}()
	return errs
}

// awaitStep blocks until the start's progress marker reaches a step, which is
// also how a test knows a wedge is actually where it means to be.
func (f *startWatchdogFixture) awaitStep(step workflowStartStep) {
	f.t.Helper()
	f.await("step "+string(step), func() bool {
		f.runner.mu.Lock()
		defer f.runner.mu.Unlock()
		progress := f.runner.startProgress[f.runKey()]
		return progress != nil && progress.step == step
	})
}

// awaitTimer returns the ONE timer armed for a delay, once it exists. Exactly
// one: a second timer for the same delay means the code armed something the
// test did not expect, and returning either would assert against the wrong one.
func (f *startWatchdogFixture) awaitTimer(delay time.Duration) *fakeWorkflowTimer {
	f.t.Helper()
	var timer *fakeWorkflowTimer
	f.await("a timer armed for "+delay.String(), func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.timers[delay]) > 1 {
			f.t.Fatalf("%d timers armed for %s; awaitTimer can only vouch for one", len(f.timers[delay]), delay)
		}
		if len(f.timers[delay]) == 0 {
			return false
		}
		timer = f.timers[delay][0]
		return true
	})
	return timer
}

func (f *startWatchdogFixture) await(what string, ready func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			f.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func (f *startWatchdogFixture) refuteOutcome(reason string) {
	f.t.Helper()
	select {
	case outcome := <-f.outcomes:
		f.t.Fatalf("%s: %+v", reason, outcome)
	default:
	}
}

// The primary path: the deadline cancels the start's context, every ctx-aware
// wait unwinds, and Start's RETURN is the single channel the failure reaches the
// engine on — no second report, and a typed setup failure the engine parks
// `setup-failed` and a bare `run resume` re-enters fresh.
func TestWorkflowStartDeadlineCancelsAWedgedSessionProof(t *testing.T) {
	fixture := newStartWatchdogFixture(t)
	threadID := fixture.seedPriorThread()
	// The incident's own wedge: a session restart blocked under the per-thread
	// action lock. The App's `startSessionTakingLock` waits for that lock on the
	// start context, so cancelling the context is what unwinds it — which is the
	// contract the seam is held to here.
	fixture.host.startSessionTakingLock = func(ctx context.Context, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	}

	continuation, err := engine.ContinueThread(threadID)
	if err != nil {
		t.Fatal(err)
	}
	errs := fixture.start(fixture.request(continuation))
	fixture.awaitStep(workflowStartStepSessionProof)

	fixture.awaitTimer(workflowStartDeadline).fire()

	err = <-errs
	if !errors.Is(err, engine.ErrSetupFailed) {
		t.Fatalf("start error = %v, want the typed setup failure the engine parks on", err)
	}
	// Rendered, not wrapped: an unwinding that surfaced as unavailable provider
	// context would otherwise route the engine into reconstructing the round on a
	// new thread instead of parking a run nobody can see is wedged.
	if errors.Is(err, engine.ErrProviderContextUnavailable) {
		t.Fatalf("start error carried a sentinel from the cancellation: %v", err)
	}
	if !strings.Contains(err.Error(), string(workflowStartStepSessionProof)) {
		t.Fatalf("start error does not name the wedged step: %v", err)
	}
	if !strings.Contains(err.Error(), threadID) {
		t.Fatalf("start error does not name the wedged thread: %v", err)
	}
	if !strings.Contains(err.Error(), "run resume") {
		t.Fatalf("start error does not say how to recover: %v", err)
	}
	fixture.refuteOutcome("the deadline reported an outcome as well as failing the start")
	fixture.runner.mu.Lock()
	leaked := len(fixture.runner.startProgress)
	fixture.runner.mu.Unlock()
	if leaked != 0 {
		t.Fatalf("start progress records left behind = %d", leaked)
	}
}

// The fallback: a start wedged in a wait the context cannot reach. The attempt is
// already INSTALLED here and the wedge is the opening send holding `sendMu`,
// which is exactly why the fallback must not go through `r.finish` — that
// barriers on the same lock and would deadlock behind the thing it exists to
// escape.
func TestWorkflowStartGraceFallbackReportsAWedgedOpeningSendExactlyOnce(t *testing.T) {
	fixture := newStartWatchdogFixture(t)
	sending := make(chan struct{})
	hold := make(chan struct{})
	var once sync.Once
	fixture.host.send = func(
		context.Context, string, string, json.RawMessage, func(DispatchIdentity),
	) error {
		// Deliberately deaf to the start context: this is the non-ctx-aware wait
		// the grace fallback exists for.
		once.Do(func() { close(sending) })
		<-hold
		return nil
	}

	errs := fixture.start(fixture.request(engine.FreshTurn()))
	<-sending
	runKey := fixture.runKey()
	fixture.runner.mu.Lock()
	installed := fixture.runner.runs[runKey] != nil
	fixture.runner.mu.Unlock()
	if !installed {
		t.Fatal("the wedged send is supposed to run with the attempt already installed")
	}

	fixture.awaitTimer(workflowStartDeadline).fire()
	fixture.awaitTimer(workflowStartGrace).fire()

	outcome := <-fixture.outcomes
	// A start that blew its bound is a SETUP failure whichever channel reports it,
	// so the fallback parks the run exactly as the deadline's own return does.
	if outcome.Kind != engine.OutcomeSetupFailure {
		t.Fatalf("fallback outcome = %+v, want the setup failure a blown start bound parks as", outcome)
	}
	if !strings.Contains(outcome.Detail, string(workflowStartStepOpeningSend)) {
		t.Fatalf("fallback detail does not name the wedged step: %q", outcome.Detail)
	}
	if !strings.Contains(outcome.Detail, "never returned") {
		t.Fatalf("fallback detail does not say the start never returned: %q", outcome.Detail)
	}

	// Detached before reporting, so a send decided before the fallback drops at
	// the chokepoint's recheck instead of landing on a run the engine has parked.
	if drop, err := fixture.runner.sendIfActive(
		context.Background(), runKey, "the late send", "late", json.RawMessage(`{"type":"object"}`), 0,
	); drop != workflowSendDropUnregistered || err != nil {
		t.Fatalf("post-fallback send = (%q, %v), want the drop named", drop, err)
	}

	close(hold)
	err := <-errs
	if !errors.Is(err, engine.ErrSetupFailed) {
		t.Fatalf("late start return = %v, want the deadline's typed failure", err)
	}
	fixture.refuteOutcome("the late start return reported the attempt a second time")
	fixture.runner.mu.Lock()
	stillInstalled := fixture.runner.runs[runKey] != nil
	leaked := len(fixture.runner.startProgress)
	fixture.runner.mu.Unlock()
	if stillInstalled {
		t.Fatal("the late return reinstalled an attempt the engine has already parked")
	}
	if leaked != 0 {
		t.Fatalf("start progress records left behind = %d", leaked)
	}
}

// A start that unwedges after the fallback already reported it must not register
// its attempt: the run has left `running`, so a live attempt under it would be a
// turn nothing could ever settle.
//
// The record is the REAL one and the death is reported by the real fallback. A
// hand-written `reported: true` would keep passing even if `reportDead` stopped
// leaving the record where install looks — which is the whole mechanism, and
// exactly what regressed once already.
func TestWorkflowInstallRefusesAnAttemptTheFallbackAlreadyReportedDead(t *testing.T) {
	fixture := newStartWatchdogFixture(t)
	runKey := fixture.runKey()
	key := engine.RunKey{ItemID: fixture.itemID, PhaseID: fixture.phaseID, Attempt: 1}
	_, progress := fixture.runner.beginStartProgress(
		context.Background(), key, func(outcome engine.Outcome) { fixture.outcomes <- outcome },
	)
	progress.expire()
	fixture.awaitTimer(workflowStartGrace).fire()
	if reported := <-fixture.outcomes; reported.Kind != engine.OutcomeSetupFailure {
		t.Fatalf("fallback outcome = %+v, want the setup failure a blown start bound parks as", reported)
	}

	attempt := &workflowAttempt{
		workflowCompletion: workflowCompletion{
			key: engine.RunKey{ItemID: fixture.itemID, PhaseID: fixture.phaseID, Attempt: 1},
		},
		threadID: "late-thread", schema: json.RawMessage(`{"type":"object"}`),
		complete: func(outcome engine.Outcome) { fixture.outcomes <- outcome },
	}
	err := fixture.runner.installAttempt(context.Background(), attempt, preparedWorkflowAgentTurn{
		threadID: "late-thread", attach: func(string) error { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "reported dead") {
		t.Fatalf("install after a reported death = %v, want a refusal naming it", err)
	}
	fixture.runner.mu.Lock()
	installed := fixture.runner.runs[runKey] != nil
	fixture.runner.mu.Unlock()
	if installed {
		t.Fatal("an attempt was installed under a run the fallback already parked")
	}
	fixture.refuteOutcome("the refused install reported an outcome")
}

// All three inactive causes are legitimate and owned elsewhere. What they are not
// allowed to be is indistinguishable from each other, or silent.
//
// The log line comes from the door itself, not from caller discipline: a drop
// returned by sendIfActive is logged by sendIfActive, so a new caller cannot
// forget one — which is why every reason is checked for its own line here rather
// than one standing in for the rest.
func TestWorkflowSendDropReasonsAreNamedAndLogged(t *testing.T) {
	captured := testutil.CaptureLogOutput(t)

	requireLogged := func(runKey, what string, reason workflowSendDropReason) {
		t.Helper()
		line := captured.String()
		for _, want := range []string{runKey, what, string(reason)} {
			if !strings.Contains(line, want) {
				t.Fatalf("dropped-send log %q does not name %q", line, want)
			}
		}
		captured.Reset()
	}

	detached := newObserveHarness(t, string(provider.Codex))
	detached.runner.detach(detached.runKey)
	if drop, err := detached.runner.sendIfActive(
		context.Background(), detached.runKey, "the opening prompt", "late", detached.attempt.schema, 0,
	); drop != workflowSendDropUnregistered || err != nil {
		t.Fatalf("send to a detached attempt = (%q, %v)", drop, err)
	}
	detached.refuteSend("a send reached a detached attempt")
	requireLogged(detached.runKey, "the opening prompt", workflowSendDropUnregistered)

	dying := newObserveHarness(t, string(provider.Codex))
	dying.runner.mu.Lock()
	dying.attempt.pendingSessionDeath = true
	dying.runner.mu.Unlock()
	if drop, err := dying.runner.sendIfActive(
		context.Background(), dying.runKey, "the retry turn", "retry", dying.attempt.schema, dying.epoch(),
	); drop != workflowSendDropSessionDeath || err != nil {
		t.Fatalf("send with a latched session death = (%q, %v)", drop, err)
	}
	dying.refuteSend("a send reached a session already known dead")
	requireLogged(dying.runKey, "the retry turn", workflowSendDropSessionDeath)

	stale := newObserveHarness(t, string(provider.Codex))
	if drop, err := stale.runner.sendIfActive(
		context.Background(), stale.runKey, "the envelope-feedback turn", "retry",
		stale.attempt.schema, stale.epoch()-1,
	); drop != workflowSendDropStaleEpoch || err != nil {
		t.Fatalf("send from a retired ladder rung = (%q, %v)", drop, err)
	}
	stale.refuteSend("a send from a retired ladder rung reached the wire")
	requireLogged(stale.runKey, "the envelope-feedback turn", workflowSendDropStaleEpoch)
}

// The deadline fired and the start finished anyway, inside the grace window.
// Nothing has been reported by the fallback, so the honest answer is the one the
// start gave: success, with the live attempt left exactly where it is. Failing it
// here would park a run whose turn is on the wire.
func TestWorkflowStartThatBeatsTheGraceWindowKeepsItsLiveAttempt(t *testing.T) {
	fixture := newStartWatchdogFixture(t)
	sending := make(chan struct{})
	hold := make(chan struct{})
	var once sync.Once
	fixture.host.send = func(
		context.Context, string, string, json.RawMessage, func(DispatchIdentity),
	) error {
		once.Do(func() { close(sending) })
		<-hold
		return nil
	}

	errs := fixture.start(fixture.request(engine.FreshTurn()))
	<-sending
	runKey := fixture.runKey()

	// The deadline fires; the grace timer is armed but never reaches its callback.
	fixture.awaitTimer(workflowStartDeadline).fire()
	grace := fixture.awaitTimer(workflowStartGrace)
	close(hold)

	if err := <-errs; err != nil {
		t.Fatalf("start that returned inside the grace window = %v, want success", err)
	}
	if grace.active {
		t.Fatal("the grace timer is still armed after the start returned")
	}
	fixture.refuteOutcome("a start that succeeded also reported an outcome")
	fixture.runner.mu.Lock()
	installed := fixture.runner.runs[runKey] != nil
	leaked := len(fixture.runner.startProgress)
	fixture.runner.mu.Unlock()
	if !installed {
		t.Fatal("the live attempt was torn down by a deadline its start beat")
	}
	if leaked != 0 {
		t.Fatalf("start progress records left behind = %d", leaked)
	}
}

// The fallback's TOOL branch. A `driver: tool` start registers a live process
// before it returns, so a start that wedges with one registered has to have that
// process claimed in the same breath the attempt is reported dead — otherwise the
// reaping goroutine reports a second outcome for work the engine has already
// parked, or the process outlives the run entirely.
func TestWorkflowStartGraceFallbackClaimsASpawnedToolProcess(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep is not on PATH")
	}
	fixture := newStartWatchdogFixture(t)
	key := engine.RunKey{ItemID: fixture.itemID, PhaseID: fixture.phaseID, Attempt: 1}
	runKey := fixture.runKey()
	ctx, progress := fixture.runner.beginStartProgress(
		context.Background(), key, func(outcome engine.Outcome) { fixture.outcomes <- outcome },
	)
	workspace := t.TempDir()
	narrativePath := filepath.Join(t.TempDir(), "attempt.md")
	if err := fixture.runner.startToolRun(ctx, workflowToolRun{
		workflowCompletion: workflowCompletion{
			key: key, narrativePath: narrativePath,
			workspace: workspace, projectPath: workspace,
		},
		label: `phase "work"`, contract: def.PhaseEnvelope(fixture.phase()),
		binding: `command "hang"`, argv: []string{"sleep", "120"},
		envelopePath: filepath.Join(t.TempDir(), "envelope.json"),
		watchdog:     time.Hour,
	}, func(outcome engine.Outcome) { fixture.outcomes <- outcome }); err != nil {
		t.Fatalf("startToolRun = %v", err)
	}

	progress.expire()
	fixture.awaitTimer(workflowStartGrace).fire()

	outcome := <-fixture.outcomes
	if outcome.Kind != engine.OutcomeSetupFailure {
		t.Fatalf("fallback outcome = %+v, want the setup failure a blown start bound parks as", outcome)
	}
	// The reaper still writes the attempt's account — a killed command has to
	// explain itself — and reports NOTHING, because the fallback above is the one
	// fate the engine hears.
	fixture.await("the reaper's narrative", func() bool {
		narrative, err := os.ReadFile(narrativePath)
		return err == nil && bytes.Contains(narrative, []byte("killed by workflow teardown"))
	})
	fixture.runner.mu.Lock()
	stillRegistered := fixture.runner.tools[runKey] != nil
	fixture.runner.mu.Unlock()
	if stillRegistered {
		t.Fatal("the fallback left the tool attempt registered")
	}
	fixture.refuteOutcome("the reaper reported a second outcome for work the fallback already parked")

	// And the tool path refuses to register anything after that report, for the
	// same reason the agent path does — here the stake is a process nothing reaps.
	// A live context, so the refusal under test is the registration guard rather
	// than the cancellation the expiry already delivered.
	err := fixture.runner.startToolRun(context.Background(), workflowToolRun{
		workflowCompletion: workflowCompletion{
			key: key, narrativePath: filepath.Join(t.TempDir(), "late.md"),
			workspace: workspace, projectPath: workspace,
		},
		label: `phase "work"`, contract: def.PhaseEnvelope(fixture.phase()),
		binding: `command "hang"`, argv: []string{"sleep", "120"},
		envelopePath: filepath.Join(t.TempDir(), "late-envelope.json"),
		watchdog:     time.Hour,
	}, func(outcome engine.Outcome) { fixture.outcomes <- outcome })
	if err == nil || !strings.Contains(err.Error(), "reported dead") {
		t.Fatalf("tool start after a reported death = %v, want a refusal naming it", err)
	}
	fixture.refuteOutcome("the refused tool start reported an outcome")
}

// A displaced progress record owns nothing: the start that replaced it is the one
// that will report. Its timers must therefore never fire against the new start —
// a displaced grace fallback would report the LIVE attempt dead over a wedge that
// was never its own.
func TestWorkflowDisplacedStartProgressCannotTearDownItsReplacement(t *testing.T) {
	fixture := newStartWatchdogFixture(t)
	key := engine.RunKey{ItemID: fixture.itemID, PhaseID: fixture.phaseID, Attempt: 1}
	runKey := fixture.runKey()

	_, displaced := fixture.runner.beginStartProgress(
		context.Background(), key, func(outcome engine.Outcome) { fixture.outcomes <- outcome },
	)
	fixture.runner.armStartDeadline(key)
	deadline := fixture.awaitTimer(workflowStartDeadline)

	_, replacement := fixture.runner.beginStartProgress(
		context.Background(), key, func(outcome engine.Outcome) { fixture.outcomes <- outcome },
	)
	if deadline.active {
		t.Fatal("the displaced record's deadline is still armed")
	}

	// Even a timer already in flight is inert: both callbacks refuse to act for a
	// record the registry has moved on from.
	displaced.expire()
	displaced.reportDead()
	fixture.refuteOutcome("a displaced start's fallback reported the run dead")
	fixture.runner.mu.Lock()
	current := fixture.runner.startProgress[runKey]
	reported := replacement.reported
	fixture.runner.mu.Unlock()
	if current != replacement {
		t.Fatalf("registry record = %p, want the replacement %p", current, replacement)
	}
	if reported {
		t.Fatal("the displaced record's fallback marked the replacement reported")
	}
}

// A wedged takeover-finalize start never reaches the wrapper's error path, so
// the grace fallback is the only thing left that can release the thread's
// `transitioning` takeover registration. Without it, the run parks and every
// steering send into that thread is rejected until process restart.
func TestWorkflowStartGraceFallbackReleasesTheTakeoverRegistration(t *testing.T) {
	fixture := newStartWatchdogFixture(t)
	key := engine.RunKey{ItemID: fixture.itemID, PhaseID: fixture.phaseID, Attempt: 1}
	_, progress := fixture.runner.beginStartProgress(
		context.Background(), key, func(outcome engine.Outcome) { fixture.outcomes <- outcome },
	)
	released := 0
	// Written where the wrapper writes it: before the deadline is armed, which
	// is the release `reportDead`'s mutex-guarded read pairs with.
	progress.cancelTakeover = func() { released++ }

	progress.expire()
	fixture.awaitTimer(workflowStartGrace).fire()
	if outcome := <-fixture.outcomes; outcome.Kind != engine.OutcomeSetupFailure {
		t.Fatalf("fallback outcome = %+v, want the setup failure a blown start bound parks as", outcome)
	}
	if released != 1 {
		t.Fatalf("takeover registration released %d times by the fallback, want exactly once", released)
	}
}

// The deadline fires while the opening send is in flight, and the cancel DOES
// unwind it — the send returns an error with the start context dead. That
// error must flow out through the start's single reporting channel and park
// `setup-failed`; finishing the attempt locally would park it `agent-error`
// (the shared bucket no repair verb reaches) and then let the watchdog log the
// start as a live success it kept.
func TestWorkflowStartDeadlineCancelUnwindingTheOpeningSendParksSetupFailed(t *testing.T) {
	logs := testutil.CaptureLogOutput(t)
	fixture := newStartWatchdogFixture(t)
	sending := make(chan struct{})
	hold := make(chan error)
	var once sync.Once
	fixture.host.send = func(
		context.Context, string, string, json.RawMessage, func(DispatchIdentity),
	) error {
		once.Do(func() { close(sending) })
		return <-hold
	}

	errs := fixture.start(fixture.request(engine.FreshTurn()))
	<-sending
	fixture.awaitTimer(workflowStartDeadline).fire()
	// The cancelled ctx unwinds the send: it returns the error the dead wire
	// gave it, with ctx.Err() already non-nil.
	hold <- errors.New("write EPIPE: the send was cancelled under the provider write")

	err := <-errs
	if !errors.Is(err, engine.ErrSetupFailed) {
		t.Fatalf("start error = %v, want the deadline's typed setup failure", err)
	}
	// The single channel reported it; a local completion would be a second
	// report, in the wrong bucket.
	fixture.refuteOutcome("the cancelled opening send parked the attempt locally as well as failing the start")
	if strings.Contains(logs.String(), "keeping the live attempt") {
		t.Fatalf("the watchdog logged a parked start as a kept live attempt:\n%s", logs.String())
	}
	fixture.runner.mu.Lock()
	installed := fixture.runner.runs[fixture.runKey()] != nil
	leaked := len(fixture.runner.startProgress)
	fixture.runner.mu.Unlock()
	if installed {
		t.Fatal("the cancelled start left its attempt installed")
	}
	if leaked != 0 {
		t.Fatalf("start progress records left behind = %d", leaked)
	}
}
