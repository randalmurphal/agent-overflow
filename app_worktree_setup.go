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
// It runs on worktrees THIS app cut for a chat thread: CreateThread's
// worktree-branch option and PrepareThreadWorktree. Adopting a sibling's
// worktree, attaching an existing branch's checkout, forks, and PR threads all
// skip it — setup either already ran there or the provisioning state is
// unknowable, and re-running an arbitrary argv recipe over someone else's
// checkout is not a safe default.
//
// Lifecycle mirrors startBackgroundGitFetch: run contexts derive from
// lifeCtx() so cancellation kills the process group in flight, and a WaitGroup
// joins every goroutine in Shutdown before the store it writes to closes.
//
// This file is the lifecycle half. The wire contract it emits lives in
// app_worktree_setup_types.go and the emitter itself in
// app_worktree_setup_observer.go.

// worktreeSetupRun is the App's record of one thread's run. Exactly one entry
// per thread exists at a time, and only two kinds are retained: a run that is
// still going, and a run that FAILED. Success and cancellation drop their
// record the moment they settle — the success card is a transient
// acknowledgement of something a hydrating client never saw begin, so keeping
// it would make every later pane mount replay it. What that leaves is a map
// bounded by "threads with a failed setup", which is also exactly what the
// durable column tracks.
type worktreeSetupRun struct {
	id           string
	threadID     string
	projectID    string
	projectRoot  string
	worktreePath string
	steps        []WorktreeSetupStep
	config       worktreesetup.Config

	// Guarded by App.worktreeSetupMu.
	statuses     []string
	state        string
	errorText    string
	startedAt    int64
	finishedAt   int64
	cancelled    bool
	shuttingDown bool

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
	projectID := strings.TrimSpace(thread.ProjectID)
	worktreePath := strings.TrimSpace(thread.WorktreePath)
	projectRoot := strings.TrimSpace(thread.ProjectPath)
	switch {
	case a.store == nil:
		return fmt.Errorf("worktree setup: store unavailable")
	case projectID == "" || projectRoot == "":
		return fmt.Errorf("worktree setup: thread %s has no project", thread.ID)
	case worktreePath == "":
		return fmt.Errorf("worktree setup: thread %s has no worktree", thread.ID)
	}

	config, _, err := a.store.ProjectWorktreeSetup(projectID)
	if err != nil {
		// A recipe that cannot be read is a setup FAILURE, not a reason to
		// skip setup: a worktree provisioned without what its project
		// declared is broken in ways that only surface mid-turn. The workflow
		// runner reaches the same conclusion (worktreeSetup) and parks; here
		// it becomes a failed run the user can see and retry.
		a.recordUnstartableWorktreeSetup(thread, worktreePath,
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
		threadID:     thread.ID,
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

	a.worktreeSetupMu.Lock()
	// The stopped flag and the WaitGroup Add sit in ONE critical section, and
	// stopThreadWorktreeSetups sets the flag in that same section before it
	// waits. That is what makes "no goroutine joins the WaitGroup after Wait
	// began" structural rather than a matter of call ordering.
	if a.worktreeSetupStopped {
		a.worktreeSetupMu.Unlock()
		cancel()
		return ErrShuttingDown
	}
	if existing := a.worktreeSetupRuns[thread.ID]; existing != nil && existing.state == worktreeSetupRunRunning {
		a.worktreeSetupMu.Unlock()
		cancel()
		return fmt.Errorf("worktree setup for thread %s is already running", thread.ID)
	}
	if a.worktreeSetupRuns == nil {
		a.worktreeSetupRuns = make(map[string]*worktreeSetupRun)
	}
	a.worktreeSetupRuns[thread.ID] = run
	a.worktreeSetupWG.Add(1)
	a.worktreeSetupMu.Unlock()

	a.setThreadWorktreeSetupState(thread.ID, store.WorktreeSetupStateRunning)

	go func() {
		defer a.worktreeSetupWG.Done()
		defer cancel()
		observer := newWorktreeSetupObserver(a, run)
		// The observer owns every emission for this run, including the
		// started frame — one emitter, so the frames cannot disagree with the
		// record they describe.
		_ = worktreesetup.RunObserved(ctx, run.projectRoot, run.worktreePath, run.config, observer)
	}()
	return nil
}

// recordUnstartableWorktreeSetup registers a run that failed before it could
// start a single step. It exists so a pre-flight failure (an unreadable
// recipe) reaches the same panel and the same durable state as a failed
// command, instead of being a log line nobody reads.
func (a *App) recordUnstartableWorktreeSetup(thread store.Thread, worktreePath string, cause error) {
	now := time.Now().UnixMilli()
	run := &worktreeSetupRun{
		id:           uuid.New().String(),
		threadID:     thread.ID,
		projectID:    thread.ProjectID,
		projectRoot:  thread.ProjectPath,
		worktreePath: worktreePath,
		steps:        []WorktreeSetupStep{},
		statuses:     []string{},
		state:        worktreeSetupRunFailed,
		errorText:    cause.Error(),
		startedAt:    now,
		finishedAt:   now,
		tail:         procutil.NewTailBuffer(worktreeSetupOutputTailBytes),
		cancel:       func() {},
		done:         make(chan struct{}),
	}
	close(run.done)

	a.worktreeSetupMu.Lock()
	if a.worktreeSetupRuns == nil {
		a.worktreeSetupRuns = make(map[string]*worktreeSetupRun)
	}
	if existing := a.worktreeSetupRuns[thread.ID]; existing != nil && existing.state == worktreeSetupRunRunning {
		a.worktreeSetupMu.Unlock()
		log.Printf("thread %s: worktree setup unreadable while a run is in flight: %v", thread.ID, cause)
		return
	}
	a.worktreeSetupRuns[thread.ID] = run
	a.worktreeSetupMu.Unlock()

	a.setThreadWorktreeSetupState(thread.ID, store.WorktreeSetupStateFailed)
	a.emitEvent(worktreeSetupChannel, WorktreeSetupEvent{
		Phase:        worktreeSetupPhaseStarted,
		ThreadID:     run.threadID,
		RunID:        run.id,
		WorktreePath: run.worktreePath,
		Steps:        run.steps,
		StartedAt:    run.startedAt,
	})
	a.emitEvent(worktreeSetupChannel, WorktreeSetupEvent{
		Phase:      worktreeSetupPhaseFinished,
		ThreadID:   run.threadID,
		RunID:      run.id,
		State:      worktreeSetupRunFailed,
		Error:      run.errorText,
		FinishedAt: run.finishedAt,
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

	a.worktreeSetupMu.Lock()
	shuttingDown := run.shuttingDown
	cancelled := run.cancelled
	run.finishedAt = finishedAt
	if runErr != nil {
		run.errorText = runErr.Error()
	}
	a.worktreeSetupMu.Unlock()

	if shuttingDown {
		a.dropWorktreeSetupRun(run)
		close(run.done)
		return
	}

	state := worktreeSetupRunSucceeded
	switch {
	case cancelled:
		state = worktreeSetupRunCancelled
	case runErr != nil:
		state = worktreeSetupRunFailed
	}
	if state == worktreeSetupRunFailed && !a.threadOccupiesWorktree(run.threadID, run.worktreePath) {
		state = worktreeSetupRunCancelled
	}

	if state == worktreeSetupRunFailed {
		a.worktreeSetupMu.Lock()
		run.state = worktreeSetupRunFailed
		a.worktreeSetupMu.Unlock()
		a.setThreadWorktreeSetupState(run.threadID, store.WorktreeSetupStateFailed)
	} else {
		a.dropWorktreeSetupRun(run)
		a.setThreadWorktreeSetupState(run.threadID, store.WorktreeSetupStateNone)
	}

	a.emitEvent(worktreeSetupChannel, WorktreeSetupEvent{
		Phase:      worktreeSetupPhaseFinished,
		ThreadID:   run.threadID,
		RunID:      run.id,
		State:      state,
		Error:      run.errorText,
		FinishedAt: finishedAt,
	})
	close(run.done)
}

// dropWorktreeSetupRun releases the record if it is still the thread's current
// one. The identity check matters: a retry can already have replaced it.
func (a *App) dropWorktreeSetupRun(run *worktreeSetupRun) {
	a.worktreeSetupMu.Lock()
	defer a.worktreeSetupMu.Unlock()
	if a.worktreeSetupRuns[run.threadID] == run {
		delete(a.worktreeSetupRuns, run.threadID)
	}
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
	a.worktreeSetupMu.Lock()
	run := a.worktreeSetupRuns[threadID]
	if run != nil {
		run.cancelled = true
	}
	a.worktreeSetupMu.Unlock()

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
	a.worktreeSetupMu.Lock()
	run := a.worktreeSetupRuns[threadID]
	retained := run != nil && gitops.SameFilesystemPath(run.worktreePath, workspacePath)
	a.worktreeSetupMu.Unlock()
	if retained {
		return
	}
	a.cancelThreadWorktreeSetup(threadID)
}

// stopThreadWorktreeSetups cancels every in-flight run and joins their
// goroutines. Called from Shutdown before the store closes, because settling a
// run writes to it. Idempotent.
func (a *App) stopThreadWorktreeSetups() {
	a.worktreeSetupMu.Lock()
	a.worktreeSetupStopped = true
	cancels := make([]context.CancelFunc, 0, len(a.worktreeSetupRuns))
	for _, run := range a.worktreeSetupRuns {
		if run.state != worktreeSetupRunRunning {
			continue
		}
		run.shuttingDown = true
		cancels = append(cancels, run.cancel)
	}
	a.worktreeSetupMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	a.worktreeSetupWG.Wait()
}

// sweepCrashedWorktreeSetups settles rows a previous app instance left
// mid-setup. A run exists only inside a live process, so a "running" row at
// boot means the app died with the recipe in flight and the worktree's state
// is unknown — which is what "failed" means here. Counterpart of the workflow
// engine's unit sweep.
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
	a.worktreeSetupMu.Lock()
	run := a.worktreeSetupRuns[thread.ID]
	var state WorktreeSetupRunState
	if run != nil {
		state = WorktreeSetupRunState{
			ThreadID:     thread.ID,
			RunID:        run.id,
			State:        run.state,
			Steps:        run.steps,
			StepStatuses: append([]string(nil), run.statuses...),
			Error:        run.errorText,
			WorktreePath: run.worktreePath,
			StartedAt:    run.startedAt,
			FinishedAt:   run.finishedAt,
		}
	}
	a.worktreeSetupMu.Unlock()

	if run != nil {
		// Sequence BEFORE content: a chunk emitted between these two reads
		// lands in Output but reports a seq the client already has, so it is
		// ignored rather than appended twice. The reverse order would drop it.
		state.OutputSeq = run.seq.Load()
		state.Output = run.tail.String()
		state.Steps = slicesx.OrEmpty(state.Steps)
		state.StepStatuses = slicesx.OrEmpty(state.StepStatuses)
		return state
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
	a.emitEvent("thread:updated", triage.ThreadUpdateEvent{Action: "full", Thread: &current})
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
