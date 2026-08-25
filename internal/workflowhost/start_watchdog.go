package workflowhost

import (
	"context"
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/workflow/engine"
)

// The bound on `Runner.Start`, and the record of how far one got.
//
// Nothing else bounds it. A start that wedges leaves the engine `runnerStarting`
// with a worker-less `running` attempt: no verb but a root pause reaches such a
// run, no timer is armed for it, and the run map shows a phase that has been
// "starting" for an hour (incident 2026-08-15 — a reconnect-on-send stopped the
// session and its restart half blocked under the per-thread action lock, and the
// run sat there for 60+ minutes).
//
// Three pieces, in order of preference:
//
//  1. A per-start STEP MARKER, so a start that fails or expires can say what it
//     was doing rather than only that it did not finish.
//  2. A DEADLINE that cancels the start's own context. Every wedge-prone wait
//     inside a start is ctx-cancellable, so cancelling unwinds them and `Start`
//     returns an error — the SINGLE reporting channel, which is what keeps this
//     free of double-report races with the engine's own start settlement.
//  3. A GRACE FALLBACK for the wait that is not ctx-aware (wedged provider IO).
//     If `Start` has still not returned a grace period after the cancel, the
//     attempt is reported dead directly, so the run parks even if the goroutine
//     leaks. Reporting is once-only across both channels.
const (
	// workflowStartDeadline bounds the INTERNAL half of a start: session proof,
	// spawn handshakes (which carry their own 30s provider timeouts), thread
	// attachment, and the opening send dispatch. Every one of those is bounded by
	// design, so five minutes is already far past any of them succeeding.
	//
	// It is deliberately NOT armed until workspace provisioning has finished.
	// Cutting a worktree on a huge repository and running a project's setup hooks
	// is arbitrary user-authored work — a dependency install can legitimately run
	// for many minutes — and killing it would turn a slow project into a broken
	// one. That half of a start is bounded by the hooks' own timeouts, not here.
	workflowStartDeadline = 5 * time.Minute
	// workflowStartGrace is how long the cancelled start is given to unwind
	// before the attempt is reported dead without it.
	workflowStartGrace = 30 * time.Second
)

// workflowStartStep names one boundary inside `Runner.Start`. The value is
// prose because its only consumers are a park cause and a log line: what a human
// reading "the run stopped here" needs is the name of the thing that did not
// finish.
type workflowStartStep string

const (
	workflowStartStepValidate     workflowStartStep = "validating the launch"
	workflowStartStepReliability  workflowStartStep = "reading the project reliability profile"
	workflowStartStepWorkspace    workflowStartStep = "preparing the workspace"
	workflowStartStepNarrative    workflowStartStep = "creating the attempt narrative"
	workflowStartStepSessionProof workflowStartStep = "proving the provider session"
	workflowStartStepInstall      workflowStartStep = "installing the attempt"
	workflowStartStepOpeningSend  workflowStartStep = "sending the opening prompt"
	workflowStartStepToolResolve  workflowStartStep = "resolving the tool command"
	workflowStartStepToolSpawn    workflowStartStep = "spawning the tool process"
)

// workflowStartProgress is one `Runner.Start` call's live state: the step it is
// on, the thread it selected, its deadline, and the once-guard both reporting
// channels share.
//
// Every field is guarded by the RUNNER mutex, not a mutex of its own. That is
// load-bearing rather than convenient: `reported` and the attempt registry have
// to be decided together, or a start that unwedges just after the fallback
// reported it would install a live attempt under a run the engine has already
// parked.
type workflowStartProgress struct {
	runner   *Runner
	runKey   string
	cancel   context.CancelFunc
	complete func(engine.Outcome)
	// cancelTakeover, set only for a takeover-finalize start, releases the
	// thread's `transitioning` takeover registration. `Start`'s wrapper runs it
	// when the start RETURNS with an error; `reportDead` runs it for the start
	// that never returns — without it, the fallback would park the run and leave
	// every steering send into that thread rejected until process restart, the
	// exact strand the wrapper's error path exists to prevent. It is idempotent,
	// so both channels firing for one slow-unwinding start is harmless.
	cancelTakeover func()

	step     workflowStartStep
	threadID string
	deadline Timer
	grace    Timer
	// expired records that the deadline fired, and what it fired on. The step and
	// thread are captured at that moment because the wedged goroutine may still
	// advance either afterwards.
	expired       bool
	expiredStep   workflowStartStep
	expiredThread string
	// returned means `Start` has returned; reported means the attempt's fate has
	// been told to the engine, by either channel. `Start`'s return is itself a
	// report, so it sets both.
	returned bool
	reported bool
}

// beginStartProgress opens the record for one start and returns the context
// every step of that start must run under. The caller rebinds its own `ctx`
// parameter to the returned one, so no downstream site can accidentally keep
// using the engine's uncancellable original.
func (r *Runner) beginStartProgress(
	ctx context.Context, key engine.RunKey, complete func(engine.Outcome),
) (context.Context, *workflowStartProgress) {
	startCtx, cancel := context.WithCancel(ctx)
	runKey := workflowRunKey(key)
	progress := &workflowStartProgress{
		runner: r, runKey: runKey, cancel: cancel, complete: complete,
		step: workflowStartStepValidate,
	}
	r.mu.Lock()
	// The engine starts one runner per key (`runnerStarting` is its guard), so a
	// record already here is a scheduling bug. The record is replaced — this start
	// is the one that will report — but never silently: the displaced start's steps
	// and deadline now belong to nothing.
	//
	// Its timers are stopped in the same critical section. A displaced deadline
	// would cancel a context nothing here owns, and a displaced grace fallback
	// would report THIS start's attempt dead on a wedge that was never its own.
	// `expire` and `reportDead` also refuse to act for a record the registry has
	// moved on from, so a timer already in flight cannot slip past this.
	displaced := r.startProgress[runKey]
	if displaced != nil {
		displaced.stopTimersLocked()
	}
	r.startProgress[runKey] = progress
	r.mu.Unlock()
	if displaced != nil {
		// Cancelled as well as disarmed: with its deadline gone, the displaced
		// start would otherwise be the one unbounded start in the process. The
		// cancel unwinds it through its own `finish`, which no-ops on a registry
		// entry it no longer owns.
		displaced.cancel()
		log.Printf("workflow runner: start %s began while an earlier start of the same key was still in flight", runKey)
	}
	return startCtx, progress
}

// stopTimersLocked disarms whichever of the record's timers are armed. The
// runner mutex is held: the timers are runner-mutex state like every other field
// on the record.
func (p *workflowStartProgress) stopTimersLocked() {
	if p.deadline != nil {
		p.deadline.Stop()
		p.deadline = nil
	}
	if p.grace != nil {
		p.grace.Stop()
		p.grace = nil
	}
}

// currentLocked reports whether this record is still the registry's record for
// its key. A displaced record's timers must not act: the attempt they would tear
// down belongs to the start that replaced it.
func (p *workflowStartProgress) currentLocked() bool {
	return p.runner.startProgress[p.runKey] == p
}

// startReportedDeadLocked reports whether the grace fallback has already
// reported this key's start dead. The caller holds the runner mutex: the
// decision to report and the decision to register live work must share one
// critical section, or a start that unwedges just after the fallback would
// install a live attempt (or leave a spawned tool process unreaped) under a
// run the engine has already parked.
func (r *Runner) startReportedDeadLocked(runKey string) bool {
	progress := r.startProgress[runKey]
	return progress != nil && progress.reported
}

// markStartStep records that a start reached a boundary. It is a lookup by key
// rather than a method on a pointer threaded through every helper: the steps
// happen four call levels deep across three element shapes, and a parameter that
// many sites have to remember to pass is one forgotten site away from a start
// that expires saying it was still validating.
//
// A key with no live start is not an error — `Stop`, retries, and unit paths all
// call into helpers that mark steps outside a start.
func (r *Runner) markStartStep(key engine.RunKey, step workflowStartStep) {
	r.mu.Lock()
	if progress := r.startProgress[workflowRunKey(key)]; progress != nil {
		progress.step = step
	}
	r.mu.Unlock()
}

// noteStartThread records the provider thread a start selected, so an expiry can
// name it. Empty is ignored: a failed thread creation has no thread to name, and
// it must not erase one an earlier step recorded.
func (r *Runner) noteStartThread(key engine.RunKey, threadID string) {
	if threadID == "" {
		return
	}
	r.mu.Lock()
	if progress := r.startProgress[workflowRunKey(key)]; progress != nil {
		progress.threadID = threadID
	}
	r.mu.Unlock()
}

// armStartDeadline starts the clock. Every element shape calls it at the same
// point: the moment its workspace is provisioned and only bounded internal work
// remains. See workflowStartDeadline for why not earlier.
func (r *Runner) armStartDeadline(key engine.RunKey) {
	r.mu.Lock()
	progress := r.startProgress[workflowRunKey(key)]
	if progress == nil || progress.returned || progress.deadline != nil {
		r.mu.Unlock()
		return
	}
	progress.deadline = r.newTimer(workflowStartDeadline, progress.expire)
	r.mu.Unlock()
}

// expire cancels the start's context and opens the grace window. It does not
// report anything: the cancel is expected to unwind the start, and the start's
// own return is the report.
func (p *workflowStartProgress) expire() {
	r := p.runner
	r.mu.Lock()
	// The expired re-check makes the guard structural: today the deadline is a
	// one-shot timer and `armStartDeadline` refuses a re-arm, but a second firing
	// slipping through would arm a second grace timer and a second reportDead.
	if p.returned || p.expired || !p.currentLocked() {
		r.mu.Unlock()
		return
	}
	p.expired = true
	step, threadID := p.step, p.threadID
	p.expiredStep, p.expiredThread = step, threadID
	p.grace = r.newTimer(workflowStartGrace, p.reportDead)
	r.mu.Unlock()
	// Outside the lock: cancellation wakes the wedged goroutine, which marks its
	// next step through the same mutex on its way out.
	p.cancel()
	log.Printf(
		"workflow runner: start %s exceeded %s at step %q %s; cancelling it",
		p.runKey, workflowStartDeadline, step, workflowStartThreadPhrase(threadID),
	)
}

// reportDead is the fallback for a start that did not unwind. It reports the
// attempt dead itself so the run parks rather than sitting `running` behind a
// leaked goroutine.
//
// It deliberately does NOT go through `r.finish`, which barriers on the
// attempt's `sendMu`: the wait that wedged is very often a send holding exactly
// that lock, so the fallback would deadlock behind the thing it exists to
// escape. Detaching first is what makes that safe — a late send finds the
// attempt unregistered and drops at `sendIfActive`'s recheck.
func (p *workflowStartProgress) reportDead() {
	r := p.runner
	r.mu.Lock()
	if p.reported || !p.currentLocked() {
		r.mu.Unlock()
		return
	}
	p.reported = true
	step, threadID := p.expiredStep, p.expiredThread
	// Read under the runner mutex: the wrapper writes it before the deadline is
	// armed, and the arming is the release this read pairs with.
	cancelTakeover := p.cancelTakeover
	attempt, installed := r.detachLocked(p.runKey)
	// A tool start that reached its spawn is claimed the same way, in the same
	// critical section: `workflowToolTornDown` makes the reaping goroutine write
	// its narrative and report NOTHING, so the fate below is the only one the
	// engine hears.
	tool, spawned := r.stopToolAttemptLocked(p.runKey, workflowToolTornDown)
	// The record deliberately STAYS in the registry. `reported` is what
	// `installAttempt` reads to refuse an attempt this fallback has already
	// parked, and a start that unwedges after this point still has to find it
	// there. `finish` is the sole remover, so a start that never returns leaks
	// exactly one record — bounded, because a later start of the same key
	// displaces it.
	r.mu.Unlock()
	if installed && attempt.unsubscribe != nil {
		attempt.unsubscribe()
	}
	if spawned {
		tool.cancel()
	}

	if cancelTakeover != nil {
		cancelTakeover()
	}

	detail := workflowFailureDetail(fmt.Sprintf(
		"the runner start never returned: it exceeded %s at step %q %s and was still blocked %s after cancellation; "+
			"a fresh phase entry (bare `run resume`) starts on a new thread",
		workflowStartDeadline, step, workflowStartThreadPhrase(threadID), workflowStartGrace,
	))
	log.Printf("workflow runner: reporting start %s dead without it: %s", p.runKey, detail)
	// The same classification the deadline's own return produces: a start that blew
	// its bound is a SETUP failure whichever channel reports it, so the two park the
	// run identically rather than the fallback landing it as an agent error a retry
	// verb would treat differently. See `finish`, which renders `ErrSetupFailed`.
	outcome := engine.Outcome{Kind: engine.OutcomeSetupFailure, Detail: detail}
	if installed {
		// The engine will not stop this attempt for us: `complete` clears
		// `runnerStarting`/`runnerActive` before the teardown that would have called
		// `Runner.Stop`. So a session the wedged start may have opened is interrupted
		// here — on its own goroutine, because the interrupt takes the thread action
		// lock and waits out the in-flight send, which is precisely what is wedged.
		// The run parks now; the process comes down when the wedge clears.
		go r.interruptDetachedAttempt(p.runKey, attempt)
		attempt.complete(outcome)
		return
	}
	if p.complete != nil {
		p.complete(outcome)
	}
}

// finish closes the record when `Start` returns and decides what that return
// says. It is the only place an expiry becomes an error, which is what makes
// this a single reporting channel: the engine settles a start exactly once, from
// exactly one value.
func (p *workflowStartProgress) finish(err error) error {
	r := p.runner
	r.mu.Lock()
	p.returned = true
	alreadyReported := p.reported
	p.reported = true
	p.stopTimersLocked()
	expired, step, threadID := p.expired, p.expiredStep, p.expiredThread
	if r.startProgress[p.runKey] == p {
		delete(r.startProgress, p.runKey)
	}
	r.mu.Unlock()
	// The engine cancels this context the moment it settles the start anyway, so
	// releasing it here changes nothing but the leak.
	p.cancel()

	if !expired {
		return err
	}
	if err == nil && !alreadyReported {
		// The deadline fired and the start finished anyway — the attempt is
		// installed and live, so the honest answer is success. Said out loud
		// because a start this close to its bound is a finding either way.
		log.Printf(
			"workflow runner: start %s completed after its %s deadline fired at step %q; keeping the live attempt",
			p.runKey, workflowStartDeadline, step,
		)
		return nil
	}
	cause := "the start then returned without an error"
	if err != nil {
		cause = err.Error()
	}
	if alreadyReported {
		// The grace fallback already parked this attempt, and it detached the
		// attempt before doing so — so nothing this late return produced is live.
		// The error below still reaches the engine and is inert there (the run has
		// left `running`, so `finishRunnerStart` returns before it can act on it);
		// it is returned rather than swallowed because it is the truthful answer to
		// "how did this start end".
		log.Printf(
			"workflow runner: start %s returned after the grace fallback already reported it dead: %s",
			p.runKey, cause,
		)
	}
	// The cause is RENDERED rather than wrapped. Wrapping would carry whatever
	// sentinel the unwinding produced — an `ErrProviderContextUnavailable` from a
	// cancelled session proof would route the engine into reconstructing the round
	// on a new thread instead of parking it — and a start that blew its deadline is
	// a setup failure whatever the cancellation happened to surface as.
	return fmt.Errorf(
		"%w: runner start exceeded %s at step %q %s; a fresh phase entry (bare `run resume`) "+
			"starts on a new thread (cancellation surfaced as: %s)",
		engine.ErrSetupFailed, workflowStartDeadline, step, workflowStartThreadPhrase(threadID), cause,
	)
}

func workflowStartThreadPhrase(threadID string) string {
	if threadID == "" {
		return "before a provider thread was selected"
	}
	return "on thread " + threadID
}
