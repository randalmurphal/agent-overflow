package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"agent-overflow/internal/eventchan"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/procutil"
	"agent-overflow/internal/slicesx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/worktreesetup"

	"github.com/google/uuid"
)

// Chat-thread worktree setup.
//
// The same recipe the workflow engine runs, on the opposite posture. Workflow
// provisioning runs it BLOCKING and rolls the worktree back on failure,
// because nothing human is watching an unattended run and half a worktree is
// worse than none. A chat thread is the other case: the person who asked for
// the worktree is looking at it, the worktree and the thread are usable
// whatever the recipe did, and the agent can often repair a failed setup
// itself. So here the run is:
//
//   - ASYNC. The thread is created / switched and returns immediately; the
//     recipe streams into a panel over the `worktree:setup` channel.
//   - WATCHABLE. Every step, its output, and its outcome are pushed, with a
//     snapshot RPC (GetThreadWorktreeSetup) as the reconnect companion.
//   - NOT ROLLED BACK. A failure leaves the worktree in place.
//   - VISIBLY FAILED. `threads.worktree_setup_state` outlives the process, so
//     a restart still shows the sidebar pill and keeps Retry reachable.
//
// It runs on worktrees THIS app cut for the chat surface, whether or not a
// thread existed at the time: CreateThread's worktree-branch option,
// PrepareThreadWorktree and AttachThreadWorktree (bound), and
// PrepareProjectWorktree / AttachProjectWorktree (unbound — a draft's cut, see
// app_worktree_setup_workspace.go). The recipe is a project convention about a
// freshly created checkout, so whether the branch is new or pre-existing
// (attach) does not matter, and neither does whether a row exists yet.
// Adopting a sibling's worktree, forks, and PR threads skip it: no worktree is
// created there, and running an arbitrary argv recipe over a checkout someone
// else provisioned is not a safe default.
//
// Lifecycle mirrors startBackgroundGitFetch: run contexts derive from
// lifeCtx() so cancellation kills the process group in flight, and a WaitGroup
// joins every goroutine in Shutdown before the store it writes to closes.
//
// This file is the lifecycle half. The wire contract it emits lives in
// app_worktree_setup_types.go and the emitter itself in
// app_worktree_setup_observer.go.

// worktreeSetupRun is the App's record of one run. Records live in
// App.worktreeSetup.runs under a KEY that is the thread id for a bound run and
// workspaceSetupRunKey(worktreePath) for an unbound one, so exactly one entry
// per owner exists at a time and adoption is a re-key rather than a copy.
//
// Only two kinds are retained: a run that is still going, and a run that
// FAILED. Success and cancellation drop their record the moment they settle —
// the success card is a transient acknowledgement of something a hydrating
// client never saw begin, so keeping it would make every later pane mount
// replay it. What that leaves is a map bounded by "owners with a failed
// setup". For a bound run that is exactly what the durable column tracks; an
// UNBOUND failure has no column, so its record is the only thing that
// remembers it — retained until a thread adopts it, a retry replaces it, or
// the process ends (see sweepCrashedWorktreeSetups).
type worktreeSetupRun struct {
	id           string
	projectID    string
	projectRoot  string
	worktreePath string
	steps        []WorktreeSetupStep
	config       worktreesetup.Config

	// key is this run's identity in App.worktreeSetup.runs, and threadID is
	// the thread it reports against — the same string for an ordinary run,
	// and (workspaceSetupRunKey(worktreePath), "") for one that was started
	// before any thread existed. Both are guarded by App.worktreeSetup.mu
	// because adoption rewrites them mid-run; read threadID through
	// worktreeSetupThreadID, never directly off the record.
	key      string
	threadID string

	// Guarded by App.worktreeSetup.mu.
	statuses     []string
	state        string
	errorText    string
	startedAt    int64
	finishedAt   int64
	cancelled    bool
	shuttingDown bool
	// settled flips in finishThreadWorktreeSetup's FIRST critical section,
	// together with the terminal `state`. It is what adoption reads to know
	// the run's outcome is already decided: without it, an adoption whose
	// critical section wins the race would stamp the durable column "running"
	// over a settle that has already written the truth, and the row would say
	// "running" until the next boot sweep. Both stamps are issued under the
	// mutex for the same reason — reading the flag would not order the two
	// SQLite writes.
	settled bool

	// tail is self-guarded; seq is atomic. Both are read by the snapshot RPC
	// while the run goroutine writes them.
	tail *procutil.TailBuffer
	seq  atomic.Uint64

	cancel context.CancelFunc
	// done closes after the run goroutine has settled the record, so a
	// canceller can join it before the caller tears the thread down.
	done chan struct{}
}

// --- Bound methods ---

// GetThreadWorktreeSetup returns the current worktree-setup state for a
// thread: the live run if one is going, the retained failure if the last one
// failed, otherwise idle. A durable failure with no retained run (the app
// restarted since) reports "failed" with no steps and no output — the failure
// survived; the transcript did not.
//
// LocalOnly: the payload is the stdout/stderr of local commands run against
// the user's checkout, the same data class as GetTerminalReplay.
func (a *App) GetThreadWorktreeSetup(threadID string) (WorktreeSetupRunState, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return WorktreeSetupRunState{}, fmt.Errorf("get worktree setup: thread id is required")
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return WorktreeSetupRunState{}, fmt.Errorf("get worktree setup: %w", err)
	}
	return a.worktreeSetupSnapshot(thread), nil
}

// RetryThreadWorktreeSetup re-runs the project's recipe against the thread's
// current worktree. It re-reads the recipe rather than replaying the failed
// run's copy, so fixing the recipe in Settings and hitting Retry does what the
// user means.
//
// Refusals are loud and specific: a thread that is no longer in a worktree, a
// project with nothing configured, and a run already in flight are three
// different mistakes and say so.
//
// LocalOnly: this executes the project's argv commands. RCE-equivalent.
func (a *App) RetryThreadWorktreeSetup(threadID string) error {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return fmt.Errorf("retry worktree setup: thread id is required")
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return fmt.Errorf("retry worktree setup: %w", err)
	}
	if !threadOccupiesItsWorktree(thread) {
		return fmt.Errorf("retry worktree setup: thread %s is not working in a worktree", threadID)
	}
	return a.launchThreadWorktreeSetup(thread, true)
}

// --- Kickoff ---

// startThreadWorktreeSetup is the fire-and-forget entry point the worktree
// creation paths use. A refusal is logged rather than surfaced: the worktree
// and the thread were created successfully, and that is what the caller was
// asked to report on.
func (a *App) startThreadWorktreeSetup(thread store.Thread) {
	if err := a.launchThreadWorktreeSetup(thread, false); err != nil {
		log.Printf("thread %s: worktree setup not started: %v", thread.ID, err)
	}
}

// launchThreadWorktreeSetup registers and starts a run. It returns before the
// recipe finishes but AFTER the durable state and the run record exist, so a
// caller that re-reads the thread row sees "running".
//
// requireRecipe separates the two callers: a retry with nothing configured is
// a mistake worth naming, while a freshly cut worktree in an unconfigured
// project is the ordinary case and must show nothing at all.
func (a *App) launchThreadWorktreeSetup(thread store.Thread, requireRecipe bool) error {
	return a.launchWorktreeSetup(worktreeSetupTarget{
		threadID:     thread.ID,
		projectID:    strings.TrimSpace(thread.ProjectID),
		projectRoot:  strings.TrimSpace(thread.ProjectPath),
		worktreePath: strings.TrimSpace(thread.WorktreePath),
	}, requireRecipe)
}

// worktreeSetupTarget names what a run is being started for. A BOUND target
// carries a threadID and stamps `threads.worktree_setup_state`; an UNBOUND one
// (threadID empty) describes a worktree cut for a draft that has no row yet and
// therefore has no column to write — see app_worktree_setup_workspace.go.
//
// It deliberately carries no key. The key is resolved inside the registration
// critical section (resolveSetupKeyLocked), because for an unbound target it
// depends on what the map already holds.
type worktreeSetupTarget struct {
	threadID     string
	projectID    string
	projectRoot  string
	worktreePath string
}

// resolveSetupKeyLocked picks the map key a target registers under. Callers
// hold a.worktreeSetup.mu, and that is the point: an unbound target's key depends
// on whether an existing unbound record already covers the directory under a
// differently-spelled path, so choosing it in a separate critical section from
// the registration let a retry land beside the record it meant to replace.
func (a *App) resolveSetupKeyLocked(target worktreeSetupTarget) string {
	if target.threadID != "" {
		return target.threadID
	}
	if _, existing := a.findWorkspaceSetupRunLocked(target.worktreePath); existing != "" {
		return existing
	}
	return workspaceSetupRunKey(target.worktreePath)
}

// setupRunBlockingLocked reports the run that must refuse a registration, or
// nil. Two distinct questions, because they protect different things:
//
//   - the KEY is taken by a live run. Overwriting it would strand a goroutine
//     whose record nothing can reach, and therefore whose cancel nothing can
//     call.
//   - the DIRECTORY already has a live recipe in it, whoever owns it. Two
//     recipes in one checkout race each other's writes, and there are two ways
//     to get there that a key check cannot see: a retry issued through the
//     workspace RPC after a thread adopted the run (the run is bound now, so
//     the workspace key is free), and two threads sharing one worktree.
//
// Callers hold a.worktreeSetup.mu.
func (a *App) setupRunBlockingLocked(key, worktreePath string) *worktreeSetupRun {
	if existing := a.worktreeSetup.runs[key]; existing != nil && existing.state == worktreeSetupRunRunning {
		return existing
	}
	for _, run := range a.worktreeSetup.runs {
		if run.state != worktreeSetupRunRunning {
			continue
		}
		if gitops.SameFilesystemPath(run.worktreePath, worktreePath) {
			return run
		}
	}
	return nil
}

// describe names the target in refusals. "thread <id>" reads the same as it
// always did; an unbound run names the directory instead, which is the only
// identity it has.
func (t worktreeSetupTarget) describe() string {
	if t.threadID != "" {
		return "thread " + t.threadID
	}
	return "workspace " + t.worktreePath
}

// launchWorktreeSetup is the engine behind both kickoff paths. The bound and
// unbound cases differ only in whether a durable column exists to stamp.
func (a *App) launchWorktreeSetup(target worktreeSetupTarget, requireRecipe bool) error {
	projectID := target.projectID
	worktreePath := target.worktreePath
	projectRoot := target.projectRoot
	switch {
	case a.store == nil:
		return fmt.Errorf("worktree setup: store unavailable")
	case projectID == "" || projectRoot == "":
		return fmt.Errorf("worktree setup: %s has no project", target.describe())
	case worktreePath == "":
		return fmt.Errorf("worktree setup: %s has no worktree", target.describe())
	}

	config, _, err := a.store.ProjectWorktreeSetup(projectID)
	if err != nil {
		// A recipe that cannot be read is a setup FAILURE, not a reason to
		// skip setup: a worktree provisioned without what its project
		// declared is broken in ways that only surface mid-turn. The workflow
		// runner reaches the same conclusion (worktreeSetup) and parks; here
		// it becomes a failed run the user can see and retry.
		a.recordUnstartableWorktreeSetupRun(target,
			fmt.Errorf("load worktree setup for project %q: %w", projectID, err))
		return nil
	}
	// Resolved steps, not Config.IsZero, decide whether there is anything to
	// do: a recipe carrying only a timeout is non-zero but runs nothing, and a
	// panel with no steps is noise either way.
	steps := wireWorktreeSetupSteps(worktreesetup.ResolveSteps(config))
	if len(steps) == 0 {
		if requireRecipe {
			return fmt.Errorf("retry worktree setup: project has no worktree setup configured")
		}
		return nil
	}

	run := &worktreeSetupRun{
		id:           uuid.New().String(),
		threadID:     target.threadID,
		projectID:    projectID,
		projectRoot:  projectRoot,
		worktreePath: worktreePath,
		// Resolved from the same pure function RunObserved walks, so the
		// record the snapshot RPC can already serve names exactly the steps
		// the observer will report against.
		steps:     steps,
		config:    config,
		state:     worktreeSetupRunRunning,
		startedAt: time.Now().UnixMilli(),
		tail:      procutil.NewTailBuffer(worktreeSetupOutputTailBytes),
		done:      make(chan struct{}),
	}
	run.statuses = make([]string, len(run.steps))
	for index := range run.statuses {
		run.statuses[index] = worktreeSetupStepPending
	}
	ctx, cancel := context.WithCancel(a.lifeCtx())
	run.cancel = cancel

	a.worktreeSetup.mu.Lock()
	// The stopped flag and the WaitGroup Add sit in ONE critical section, and
	// stopThreadWorktreeSetups sets the flag in that same section before it
	// waits. That is what makes "no goroutine joins the WaitGroup after Wait
	// began" structural rather than a matter of call ordering.
	if a.worktreeSetup.stopped {
		a.worktreeSetup.mu.Unlock()
		cancel()
		return ErrShuttingDown
	}
	// Resolve-and-reserve in ONE critical section: the key an unbound target
	// takes is a function of the map's current contents.
	key := a.resolveSetupKeyLocked(target)
	if blocking := a.setupRunBlockingLocked(key, worktreePath); blocking != nil {
		a.worktreeSetup.mu.Unlock()
		cancel()
		return fmt.Errorf("worktree setup for %s is already running", target.describe())
	}
	if a.worktreeSetup.runs == nil {
		a.worktreeSetup.runs = make(map[string]*worktreeSetupRun)
	}
	run.key = key
	a.worktreeSetup.runs[key] = run
	a.worktreeSetup.wg.Add(1)
	a.worktreeSetup.mu.Unlock()

	// An unbound run has no row to stamp; its state lives only in the record
	// and on the wire until a thread adopts it.
	if target.threadID != "" {
		a.setThreadWorktreeSetupState(target.threadID, store.WorktreeSetupStateRunning)
	}

	go func() {
		defer a.worktreeSetup.wg.Done()
		defer cancel()
		observer := newWorktreeSetupObserver(a, run)
		// The observer owns every emission for this run, including the
		// started frame — one emitter, so the frames cannot disagree with the
		// record they describe.
		_ = worktreesetup.RunObserved(ctx, run.projectRoot, run.worktreePath, run.config, observer)
	}()
	return nil
}

// recordUnstartableWorktreeSetupRun registers a run that failed before it
// could start a single step. It exists so a pre-flight failure (an unreadable
// recipe) reaches the same panel and the same durable state as a failed
// command, instead of being a log line nobody reads — for a bound and an
// unbound target alike.
func (a *App) recordUnstartableWorktreeSetupRun(target worktreeSetupTarget, cause error) {
	now := time.Now().UnixMilli()
	run := &worktreeSetupRun{
		id:           uuid.New().String(),
		threadID:     target.threadID,
		projectID:    target.projectID,
		projectRoot:  target.projectRoot,
		worktreePath: target.worktreePath,
		steps:        []WorktreeSetupStep{},
		statuses:     []string{},
		state:        worktreeSetupRunFailed,
		errorText:    cause.Error(),
		startedAt:    now,
		finishedAt:   now,
		// Already terminal: adoption must read the outcome, never stamp
		// "running" over it.
		settled: true,
		tail:    procutil.NewTailBuffer(worktreeSetupOutputTailBytes),
		cancel:  func() {},
		done:    make(chan struct{}),
	}
	close(run.done)

	a.worktreeSetup.mu.Lock()
	if a.worktreeSetup.runs == nil {
		a.worktreeSetup.runs = make(map[string]*worktreeSetupRun)
	}
	key := a.resolveSetupKeyLocked(target)
	if blocking := a.setupRunBlockingLocked(key, target.worktreePath); blocking != nil {
		a.worktreeSetup.mu.Unlock()
		log.Printf("%s: worktree setup unreadable while a run is in flight: %v", target.describe(), cause)
		return
	}
	run.key = key
	a.worktreeSetup.runs[key] = run
	a.worktreeSetup.mu.Unlock()

	if target.threadID != "" {
		a.setThreadWorktreeSetupState(target.threadID, store.WorktreeSetupStateFailed)
	}
	a.emitEvent(eventchan.WorktreeSetup, WorktreeSetupEvent{
		Phase:        worktreeSetupPhaseStarted,
		ThreadID:     run.threadID,
		RunID:        run.id,
		WorktreePath: run.worktreePath,
		Steps:        run.steps,
		StartedAt:    run.startedAt,
	})
	a.emitEvent(eventchan.WorktreeSetup, WorktreeSetupEvent{
		Phase: worktreeSetupPhaseFinished,
		// The path rides every frame, terminal ones included: a client
		// following an UNBOUND run has no thread id to key on, and the
		// finished frame is the one it most needs to match.
		ThreadID:     run.threadID,
		RunID:        run.id,
		WorktreePath: run.worktreePath,
		State:        worktreeSetupRunFailed,
		Error:        run.errorText,
		FinishedAt:   run.finishedAt,
	})
}

// --- Settlement ---

// finishThreadWorktreeSetup settles a run: it decides what the durable column
// says, whether the record is retained, and what the terminal frame reports.
//
// The four outcomes are genuinely different, which is why this is not a
// success/failure boolean:
//
//   - Shutting down. The column stays at "running" on purpose: the app is
//     dying mid-recipe, the worktree's state is unknown, and the next boot's
//     sweep is what turns that into a visible failure. Nothing is emitted —
//     the bus is going away.
//   - Cancelled. The thread is being deleted or has left this worktree. The
//     run is neither a success nor a failure; the column clears and the record
//     is dropped.
//   - The thread moved off this worktree while the recipe ran. Same as
//     cancelled: a failure about a worktree the thread no longer occupies is
//     noise, and this check is what keeps that true without depending on every
//     workspace mutator remembering to call the release helper.
//   - Success or failure, for a thread still in the worktree.
func (a *App) finishThreadWorktreeSetup(run *worktreeSetupRun, runErr error) {
	finishedAt := time.Now().UnixMilli()

	a.worktreeSetup.mu.Lock()
	shuttingDown := run.shuttingDown
	cancelled := run.cancelled
	threadID := run.threadID
	run.finishedAt = finishedAt
	if runErr != nil {
		run.errorText = runErr.Error()
	}
	// Decide the outcome and publish it to the record in the SAME critical
	// section that reads the inputs. Adoption reads both fields under this
	// mutex, so from here on it can never mistake a settled run for a live one
	// — and can report its real state rather than assuming "running".
	state := worktreeSetupRunSucceeded
	switch {
	case cancelled:
		state = worktreeSetupRunCancelled
	case runErr != nil:
		state = worktreeSetupRunFailed
	}
	run.state = state
	run.settled = true
	a.worktreeSetup.mu.Unlock()

	if shuttingDown {
		a.dropWorktreeSetupRun(run)
		close(run.done)
		return
	}

	// The occupancy question only exists for a BOUND run. An unbound one
	// describes a worktree nobody occupies yet — that is the whole point of it
	// — so asking would demote every unbound failure to "cancelled" and throw
	// away exactly the state the adopting thread needs to inherit. It is asked
	// against the binding as it was at settle time: a thread adopting the run
	// right now is by definition IN the worktree.
	if state == worktreeSetupRunFailed && threadID != "" && !a.threadOccupiesWorktree(threadID, run.worktreePath) {
		state = worktreeSetupRunCancelled
		a.worktreeSetup.mu.Lock()
		run.state = state
		a.worktreeSetup.mu.Unlock()
	}

	// Re-read the binding at the moment the outcome is committed, and stamp
	// UNDER the mutex. An unbound run can be adopted while it settles; reading
	// the binding is not enough, because two unsynchronised stamps can still
	// reach SQLite out of order and leave the row saying "running" until the
	// next boot sweep. Serialising the decision and the write is what orders
	// them — adoption issues its stamp the same way.
	if state == worktreeSetupRunFailed {
		a.worktreeSetup.mu.Lock()
		threadID = run.threadID
		if threadID != "" {
			a.setThreadWorktreeSetupState(threadID, store.WorktreeSetupStateFailed)
		}
		a.worktreeSetup.mu.Unlock()
	} else {
		a.dropWorktreeSetupRun(run)
		a.worktreeSetup.mu.Lock()
		threadID = run.threadID
		if threadID != "" {
			a.setThreadWorktreeSetupState(threadID, store.WorktreeSetupStateNone)
		}
		a.worktreeSetup.mu.Unlock()
	}

	a.emitEvent(eventchan.WorktreeSetup, WorktreeSetupEvent{
		Phase:        worktreeSetupPhaseFinished,
		ThreadID:     threadID,
		RunID:        run.id,
		WorktreePath: run.worktreePath,
		State:        state,
		Error:        run.errorText,
		FinishedAt:   finishedAt,
	})
	close(run.done)
}

// dropWorktreeSetupRun releases the record if it is still the one registered
// under the run's key. The identity check matters: a retry can already have
// replaced it, and adoption can already have moved it to a thread id.
func (a *App) dropWorktreeSetupRun(run *worktreeSetupRun) {
	a.worktreeSetup.mu.Lock()
	defer a.worktreeSetup.mu.Unlock()
	if a.worktreeSetup.runs[run.key] == run {
		delete(a.worktreeSetup.runs, run.key)
	}
}

// worktreeSetupThreadID reads a run's bound thread under the mutex. Adoption
// rewrites the field mid-run, so the observer and every emitter must go through
// here rather than reading the record directly.
func (a *App) worktreeSetupThreadID(run *worktreeSetupRun) string {
	a.worktreeSetup.mu.Lock()
	defer a.worktreeSetup.mu.Unlock()
	return run.threadID
}

// --- Cancellation ---

// cancelThreadWorktreeSetup stops any run for the thread, joins its goroutine,
// drops the record, and clears the durable state. Safe on a thread that never
// had a run, and safe to call twice.
//
// It BLOCKS until the run goroutine has settled: callers are tearing the
// thread or its worktree down, and a recipe still writing into a directory
// that is about to be removed is the race this join closes.
func (a *App) cancelThreadWorktreeSetup(threadID string) {
	a.worktreeSetup.mu.Lock()
	run := a.worktreeSetup.runs[threadID]
	if run != nil {
		run.cancelled = true
	}
	a.worktreeSetup.mu.Unlock()

	if run != nil {
		run.cancel()
		<-run.done
		a.dropWorktreeSetupRun(run)
	}
	a.setThreadWorktreeSetupState(threadID, store.WorktreeSetupStateNone)
}

// releaseThreadWorktreeSetup is what every app-layer path that MOVES a chat
// thread's workspace calls. A run whose worktree the thread still occupies is
// left alone; anything else is cancelled and cleared, because both the panel
// and the sidebar pill describe a worktree the thread has left.
//
// The completion-time occupancy check in finishThreadWorktreeSetup is the
// structural backstop for a future mutator that forgets this call; this one
// exists so the UI reacts at the moment of the switch rather than at the end
// of the recipe.
func (a *App) releaseThreadWorktreeSetup(threadID, workspacePath string) {
	a.worktreeSetup.mu.Lock()
	run := a.worktreeSetup.runs[threadID]
	retained := run != nil && gitops.SameFilesystemPath(run.worktreePath, workspacePath)
	a.worktreeSetup.mu.Unlock()
	if retained {
		return
	}
	a.cancelThreadWorktreeSetup(threadID)
}

// stopThreadWorktreeSetups cancels every in-flight run and joins their
// goroutines. Called from Shutdown before the store closes, because settling a
// run writes to it. Idempotent.
//
// It walks the whole map, so unbound (pre-thread) runs are covered for free —
// which is what we want: their recipe is a live child process group like any
// other, and the fact that no row describes it changes nothing about shutdown.
func (a *App) stopThreadWorktreeSetups() {
	a.worktreeSetup.mu.Lock()
	a.worktreeSetup.stopped = true
	cancels := make([]context.CancelFunc, 0, len(a.worktreeSetup.runs))
	for _, run := range a.worktreeSetup.runs {
		if run.state != worktreeSetupRunRunning {
			continue
		}
		run.shuttingDown = true
		cancels = append(cancels, run.cancel)
	}
	a.worktreeSetup.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	a.worktreeSetup.wg.Wait()
}

// sweepCrashedWorktreeSetups settles rows a previous app instance left
// mid-setup. A run exists only inside a live process, so a "running" row at
// boot means the app died with the recipe in flight and the worktree's state
// is unknown — which is what "failed" means here. Counterpart of the workflow
// engine's unit sweep.
//
// It touches ROWS only, which means an unbound (pre-thread) run is deliberately
// not swept: it never had a row, so there is nothing on disk that could outlive
// the process, and a restart simply loses it. That is the disk-state ontology
// this feature already follows — the durable column exists precisely because a
// thread is the only thing that can carry state across a restart, and a draft
// that never became one has nothing to carry.
func (a *App) sweepCrashedWorktreeSetups() {
	if a.store == nil {
		return
	}
	swept, err := a.store.SweepRunningThreadWorktreeSetups()
	if err != nil {
		log.Printf("app: sweep crashed worktree setups: %v", err)
		return
	}
	if swept > 0 {
		log.Printf("app: marked %d interrupted worktree setup(s) as failed", swept)
	}
}

// --- Snapshot ---

func (a *App) worktreeSetupSnapshot(thread store.Thread) WorktreeSetupRunState {
	a.worktreeSetup.mu.Lock()
	run := a.worktreeSetup.runs[thread.ID]
	a.worktreeSetup.mu.Unlock()
	if run != nil {
		return a.worktreeSetupRunState(run)
	}

	// No record. A durable failure the process outlived still has to answer,
	// so the panel can offer Retry after a restart.
	result := WorktreeSetupRunState{
		ThreadID:     thread.ID,
		State:        worktreeSetupRunIdle,
		Steps:        []WorktreeSetupStep{},
		StepStatuses: []string{},
	}
	if thread.WorktreeSetupState == store.WorktreeSetupStateFailed {
		result.State = worktreeSetupRunFailed
		result.WorktreePath = thread.WorktreePath
	}
	return result
}

// worktreeSetupRunState projects a registered run into its wire shape. It
// reports the run's OWN thread id, which is empty for an unbound run and is
// what re-keys a client the moment adoption fills it in.
func (a *App) worktreeSetupRunState(run *worktreeSetupRun) WorktreeSetupRunState {
	a.worktreeSetup.mu.Lock()
	state := WorktreeSetupRunState{
		ThreadID:     run.threadID,
		RunID:        run.id,
		State:        run.state,
		Steps:        run.steps,
		StepStatuses: append([]string(nil), run.statuses...),
		Error:        run.errorText,
		WorktreePath: run.worktreePath,
		StartedAt:    run.startedAt,
		FinishedAt:   run.finishedAt,
	}
	a.worktreeSetup.mu.Unlock()

	// Sequence BEFORE content: a chunk emitted between these two reads lands
	// in Output but reports a seq the client already has, so it is ignored
	// rather than appended twice. The reverse order would drop it.
	state.OutputSeq = run.seq.Load()
	state.Output = run.tail.String()
	state.Steps = slicesx.OrEmpty(state.Steps)
	state.StepStatuses = slicesx.OrEmpty(state.StepStatuses)
	return state
}

// --- Durable state ---

// setThreadWorktreeSetupState persists the durable state and broadcasts the
// refreshed row when the value actually moved, so the sidebar pill follows
// without a refetch.
//
// A persistence failure is logged, not propagated: the column is a
// restart-survival convenience, and the panel already carries the run's real
// outcome. Losing it must not take the run's own reporting down with it.
func (a *App) setThreadWorktreeSetupState(threadID, state string) {
	if a.store == nil {
		return
	}
	current, err := a.store.GetThread(threadID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("thread %s: read worktree setup state: %v", threadID, err)
		}
		return
	}
	if current.WorktreeSetupState == state {
		return
	}
	if err := a.store.SetThreadWorktreeSetupState(threadID, state); err != nil {
		log.Printf("thread %s: persist worktree setup state %q: %v", threadID, state, err)
		return
	}
	current.WorktreeSetupState = state
	a.emitEvent(eventchan.ThreadUpdated, triage.ThreadUpdateEvent{Action: "full", Thread: &current})
}

// threadOccupiesWorktree reports whether the thread is still working in the
// worktree a run belongs to. A thread that has moved on (or been deleted)
// reports false, and a run settling against it is neither success nor failure.
func (a *App) threadOccupiesWorktree(threadID, worktreePath string) bool {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return false
	}
	return gitops.SameFilesystemPath(thread.WorkspacePath, worktreePath)
}

// threadOccupiesItsWorktree is the retry precondition: the thread must both
// have a worktree and be working IN it. A thread that switched back to the
// project root has no worktree to set up.
func threadOccupiesItsWorktree(thread store.Thread) bool {
	worktreePath := strings.TrimSpace(thread.WorktreePath)
	return worktreePath != "" && gitops.SameFilesystemPath(thread.WorkspacePath, worktreePath)
}

func wireWorktreeSetupSteps(steps []worktreesetup.Step) []WorktreeSetupStep {
	wire := make([]WorktreeSetupStep, len(steps))
	for index, step := range steps {
		wire[index] = WorktreeSetupStep{
			Index: step.Index,
			Kind:  string(step.Kind),
			Label: step.Label,
			Argv:  step.Argv,
		}
	}
	return wire
}
