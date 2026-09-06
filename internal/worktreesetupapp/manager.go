package worktreesetupapp

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
// It runs on worktrees this app cut for a persisted chat thread through
// CreateThread's worktree-branch option, PrepareThreadWorktree, or
// AttachThreadWorktree. Sharing an existing worktree, forks, and PR threads
// skip it because no new checkout was provisioned for them.
//
// Lifecycle mirrors startBackgroundGitFetch: run contexts derive from
// lifeCtx() so cancellation kills the process group in flight, and a WaitGroup
// joins every goroutine in Shutdown before the store it writes to closes.
//
// This file is the lifecycle half. The wire contract it emits lives in
// types.go and the emitter itself in observer.go.

// worktreeSetupRun is the Service's record of one run. Records live in runs
// under the owning thread id.
//
// Only two kinds are retained: a run that is still going, and a run that
// FAILED. Success and cancellation drop their record the moment they settle —
// the success card is a transient acknowledgement of something a hydrating
// client never saw begin, so keeping it would make every later pane mount
// replay it. What that leaves is a map bounded by "owners with a failed
// setup", exactly what the durable thread column tracks.
type worktreeSetupRun struct {
	id           string
	projectID    string
	projectRoot  string
	worktreePath string
	steps        []Step
	config       worktreesetup.Config

	// threadID is both the map key and the owner carried on wire frames.
	threadID string

	// Guarded by Service.worktreeSetup.mu.
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
// The App wrapper carries //ao:scope terminal:operate: the payload is the
// stdout/stderr of local commands run against the user's checkout, the same
// data class as GetTerminalReplay, and it takes the same grant.
func (s *Service) GetThreadWorktreeSetup(threadID string) (RunState, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return RunState{}, fmt.Errorf("get worktree setup: thread id is required")
	}
	thread, err := s.store.GetThread(threadID)
	if err != nil {
		return RunState{}, fmt.Errorf("get worktree setup: %w", err)
	}
	return s.worktreeSetupSnapshot(thread), nil
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
// The App wrapper carries //ao:scope terminal:operate: this executes the
// project's argv commands, which is running a command on the host by another
// name. Configuring the recipe is stricter still — SetProjectWorktreeSetup is
// //ao:stepup, because it stores argv that then runs unattended on every
// worktree cut.
func (s *Service) RetryThreadWorktreeSetup(threadID string) error {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return fmt.Errorf("retry worktree setup: thread id is required")
	}
	thread, err := s.store.GetThread(threadID)
	if err != nil {
		return fmt.Errorf("retry worktree setup: %w", err)
	}
	if !threadOccupiesItsWorktree(thread) {
		return fmt.Errorf("retry worktree setup: thread %s is not working in a worktree", threadID)
	}
	return s.LaunchThread(thread, true)
}

// --- Kickoff ---

// StartThread is the fire-and-forget entry point the worktree
// creation paths use. A refusal is logged rather than surfaced: the worktree
// and the thread were created successfully, and that is what the caller was
// asked to report on.
func (s *Service) StartThread(thread store.Thread) {
	if err := s.LaunchThread(thread, false); err != nil {
		log.Printf("thread %s: worktree setup not started: %v", thread.ID, err)
	}
}

// LaunchThread registers and starts a run. It returns before the
// recipe finishes but AFTER the durable state and the run record exist, so a
// caller that re-reads the thread row sees "running".
//
// requireRecipe separates the two callers: a retry with nothing configured is
// a mistake worth naming, while a freshly cut worktree in an unconfigured
// project is the ordinary case and must show nothing at all.
func (s *Service) LaunchThread(thread store.Thread, requireRecipe bool) error {
	return s.launchWorktreeSetup(worktreeSetupTarget{
		threadID:     thread.ID,
		projectID:    strings.TrimSpace(thread.ProjectID),
		projectRoot:  strings.TrimSpace(thread.ProjectPath),
		worktreePath: strings.TrimSpace(thread.WorktreePath),
	}, requireRecipe)
}

// worktreeSetupTarget names the persisted thread and checkout a run owns.
type worktreeSetupTarget struct {
	threadID     string
	projectID    string
	projectRoot  string
	worktreePath string
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
//     another thread starting setup in a shared worktree.
//
// Callers hold s.mu.
func (s *Service) setupRunBlockingLocked(key, worktreePath string) *worktreeSetupRun {
	if existing := s.runs[key]; existing != nil && existing.state == runRunning {
		return existing
	}
	for _, run := range s.runs {
		if run.state != runRunning {
			continue
		}
		if gitops.SameFilesystemPath(run.worktreePath, worktreePath) {
			return run
		}
	}
	return nil
}

func (t worktreeSetupTarget) describe() string {
	return "thread " + t.threadID
}

func (s *Service) launchWorktreeSetup(target worktreeSetupTarget, requireRecipe bool) error {
	projectID := target.projectID
	worktreePath := target.worktreePath
	projectRoot := target.projectRoot
	switch {
	case s.store == nil:
		return fmt.Errorf("worktree setup: store unavailable")
	case target.threadID == "":
		return fmt.Errorf("worktree setup: thread id is required")
	case projectID == "" || projectRoot == "":
		return fmt.Errorf("worktree setup: %s has no project", target.describe())
	case worktreePath == "":
		return fmt.Errorf("worktree setup: %s has no worktree", target.describe())
	}
	endWork, err := s.beginWork(s.context())
	if err != nil {
		return err
	}
	transferred := false
	defer func() {
		if !transferred {
			endWork()
		}
	}()

	config, _, err := s.store.ProjectWorktreeSetup(projectID)
	if err != nil {
		// A recipe that cannot be read is a setup FAILURE, not a reason to
		// skip setup: a worktree provisioned without what its project
		// declared is broken in ways that only surface mid-turn. The workflow
		// runner reaches the same conclusion (worktreeSetup) and parks; here
		// it becomes a failed run the user can see and retry.
		s.recordUnstartableWorktreeSetupRun(target,
			fmt.Errorf("load worktree setup for project %q: %w", projectID, err))
		return nil
	}
	// Resolved steps, not Config.IsZero, decide whether there is anything to
	// do: a recipe carrying only a timeout is non-zero but runs nothing, and a
	// panel with no steps is noise either way.
	steps := wireSteps(worktreesetup.ResolveSteps(config))
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
		state:     runRunning,
		startedAt: time.Now().UnixMilli(),
		tail:      procutil.NewTailBuffer(outputTailBytes),
		done:      make(chan struct{}),
	}
	run.statuses = make([]string, len(run.steps))
	for index := range run.statuses {
		run.statuses[index] = stepPending
	}
	ctx, cancel := context.WithCancel(s.context())
	run.cancel = cancel

	s.mu.Lock()
	// The stopped flag and the WaitGroup Add sit in ONE critical section, and
	// Stop sets the flag in that same section before it
	// waits. That is what makes "no goroutine joins the WaitGroup after Wait
	// began" structural rather than a matter of call ordering.
	if s.stopped {
		s.mu.Unlock()
		cancel()
		return s.shutdownError
	}
	key := target.threadID
	if blocking := s.setupRunBlockingLocked(key, worktreePath); blocking != nil {
		s.mu.Unlock()
		cancel()
		return fmt.Errorf("worktree setup for %s is already running", target.describe())
	}
	if s.runs == nil {
		s.runs = make(map[string]*worktreeSetupRun)
	}
	s.runs[key] = run
	s.wg.Add(1)
	s.mu.Unlock()

	s.setThreadWorktreeSetupState(target.threadID, store.WorktreeSetupStateRunning)

	transferred = true
	go func() {
		defer s.wg.Done()
		defer endWork()
		defer cancel()
		observer := newWorktreeSetupObserver(s, run)
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
// command, instead of being a log line nobody reads.
func (s *Service) recordUnstartableWorktreeSetupRun(target worktreeSetupTarget, cause error) {
	now := time.Now().UnixMilli()
	run := &worktreeSetupRun{
		id:           uuid.New().String(),
		threadID:     target.threadID,
		projectID:    target.projectID,
		projectRoot:  target.projectRoot,
		worktreePath: target.worktreePath,
		steps:        []Step{},
		statuses:     []string{},
		state:        runFailed,
		errorText:    cause.Error(),
		startedAt:    now,
		finishedAt:   now,
		tail:         procutil.NewTailBuffer(outputTailBytes),
		cancel:       func() {},
		done:         make(chan struct{}),
	}
	close(run.done)

	s.mu.Lock()
	if s.runs == nil {
		s.runs = make(map[string]*worktreeSetupRun)
	}
	key := target.threadID
	if blocking := s.setupRunBlockingLocked(key, target.worktreePath); blocking != nil {
		s.mu.Unlock()
		log.Printf("%s: worktree setup unreadable while a run is in flight: %v", target.describe(), cause)
		return
	}
	s.runs[key] = run
	s.mu.Unlock()

	s.setThreadWorktreeSetupState(target.threadID, store.WorktreeSetupStateFailed)
	s.emitSetup(Event{
		Phase:        phaseStarted,
		ThreadID:     run.threadID,
		RunID:        run.id,
		WorktreePath: run.worktreePath,
		Steps:        run.steps,
		StartedAt:    run.startedAt,
	})
	s.emitSetup(Event{
		Phase:        phaseFinished,
		ThreadID:     run.threadID,
		RunID:        run.id,
		WorktreePath: run.worktreePath,
		State:        runFailed,
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
func (s *Service) finishThreadWorktreeSetup(run *worktreeSetupRun, runErr error) {
	finishedAt := time.Now().UnixMilli()

	s.mu.Lock()
	shuttingDown := run.shuttingDown
	cancelled := run.cancelled
	threadID := run.threadID
	run.finishedAt = finishedAt
	if runErr != nil {
		run.errorText = runErr.Error()
	}
	// Decide the outcome and publish it with the rest of the terminal record.
	state := runSucceeded
	switch {
	case cancelled:
		state = runCancelled
	case runErr != nil:
		state = runFailed
	}
	run.state = state
	s.mu.Unlock()

	if shuttingDown {
		s.dropWorktreeSetupRun(run)
		close(run.done)
		return
	}

	if state == runFailed && !s.threadOccupiesWorktree(threadID, run.worktreePath) {
		state = runCancelled
		s.mu.Lock()
		run.state = state
		s.mu.Unlock()
	}

	if state == runFailed {
		s.setThreadWorktreeSetupState(threadID, store.WorktreeSetupStateFailed)
	} else {
		s.dropWorktreeSetupRun(run)
		s.setThreadWorktreeSetupState(threadID, store.WorktreeSetupStateNone)
	}

	s.emitSetup(Event{
		Phase:        phaseFinished,
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
// under the run's key. The identity check matters because a retry can already
// have replaced it.
func (s *Service) dropWorktreeSetupRun(run *worktreeSetupRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runs[run.threadID] == run {
		delete(s.runs, run.threadID)
	}
}

// --- Cancellation ---

// CancelThread stops any run for the thread, joins its goroutine,
// drops the record, and clears the durable state. Safe on a thread that never
// had a run, and safe to call twice.
//
// It BLOCKS until the run goroutine has settled: callers are tearing the
// thread or its worktree down, and a recipe still writing into a directory
// that is about to be removed is the race this join closes.
func (s *Service) CancelThread(threadID string) {
	s.mu.Lock()
	run := s.runs[threadID]
	if run != nil {
		run.cancelled = true
	}
	s.mu.Unlock()

	if run != nil {
		run.cancel()
		<-run.done
		s.dropWorktreeSetupRun(run)
	}
	s.setThreadWorktreeSetupState(threadID, store.WorktreeSetupStateNone)
}

// CancelPath stops every recipe executing in a directory and joins it before
// the caller removes that directory. The path is authoritative because a
// thread can move away while its prior setup is still settling.
func (s *Service) CancelPath(worktreePath string) {
	worktreePath = strings.TrimSpace(worktreePath)
	if worktreePath == "" {
		return
	}
	s.mu.Lock()
	var runs []*worktreeSetupRun
	for _, run := range s.runs {
		if !gitops.SameFilesystemPath(run.worktreePath, worktreePath) {
			continue
		}
		run.cancelled = true
		runs = append(runs, run)
	}
	s.mu.Unlock()
	for _, run := range runs {
		run.cancel()
		<-run.done
		s.dropWorktreeSetupRun(run)
	}
}

// ReleaseThread is what every app-layer path that MOVES a chat
// thread's workspace calls. A run whose worktree the thread still occupies is
// left alone; anything else is cancelled and cleared, because both the panel
// and the sidebar pill describe a worktree the thread has left.
//
// The completion-time occupancy check in finishThreadWorktreeSetup is the
// structural backstop for a future mutator that forgets this call; this one
// exists so the UI reacts at the moment of the switch rather than at the end
// of the recipe.
func (s *Service) ReleaseThread(threadID, workspacePath string) {
	s.mu.Lock()
	run := s.runs[threadID]
	retained := run != nil && gitops.SameFilesystemPath(run.worktreePath, workspacePath)
	s.mu.Unlock()
	if retained {
		return
	}
	s.CancelThread(threadID)
}

// Stop cancels every in-flight run and joins their
// goroutines. Called from Shutdown before the store closes, because settling a
// run writes to it. Idempotent.
func (s *Service) Stop() {
	s.mu.Lock()
	s.stopped = true
	cancels := make([]context.CancelFunc, 0, len(s.runs))
	for _, run := range s.runs {
		if run.state != runRunning {
			continue
		}
		run.shuttingDown = true
		cancels = append(cancels, run.cancel)
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	s.wg.Wait()
}

// SweepCrashed settles rows a previous app instance left
// mid-setup. A run exists only inside a live process, so a "running" row at
// boot means the app died with the recipe in flight and the worktree's state
// is unknown — which is what "failed" means here. Counterpart of the workflow
// engine's unit sweep.
func (s *Service) SweepCrashed() {
	if s.store == nil {
		return
	}
	swept, err := s.store.SweepRunningThreadWorktreeSetups()
	if err != nil {
		log.Printf("app: sweep crashed worktree setups: %v", err)
		return
	}
	if swept > 0 {
		log.Printf("app: marked %d interrupted worktree setup(s) as failed", swept)
	}
}

// WaitThread waits for the currently registered thread run to settle. It
// returns immediately when no run is registered and false only on context
// cancellation. Callers use it when teardown must join before deleting state.
func (s *Service) WaitThread(ctx context.Context, threadID string) bool {
	s.mu.Lock()
	run := s.runs[threadID]
	s.mu.Unlock()
	if run == nil {
		return true
	}
	select {
	case <-run.done:
		return true
	case <-ctx.Done():
		return false
	}
}

// HasThreadRun reports whether a retained or live record is owned by threadID.
func (s *Service) HasThreadRun(threadID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runs[threadID] != nil
}

// RecordUnstartableThread surfaces a pre-execution recipe failure through the
// same retained state and event path as an execution failure.
func (s *Service) RecordUnstartableThread(thread store.Thread, cause error) {
	s.recordUnstartableWorktreeSetupRun(worktreeSetupTarget{
		threadID: thread.ID, projectID: thread.ProjectID, projectRoot: thread.ProjectPath,
		worktreePath: thread.WorktreePath,
	}, cause)
}

// --- Snapshot ---

func (s *Service) worktreeSetupSnapshot(thread store.Thread) RunState {
	s.mu.Lock()
	run := s.runs[thread.ID]
	s.mu.Unlock()
	if run != nil {
		return s.worktreeSetupRunState(run)
	}

	// No record. A durable failure the process outlived still has to answer,
	// so the panel can offer Retry after a restart.
	result := RunState{
		ThreadID:     thread.ID,
		State:        runIdle,
		Steps:        []Step{},
		StepStatuses: []string{},
	}
	if thread.WorktreeSetupState == store.WorktreeSetupStateFailed {
		result.State = runFailed
		result.WorktreePath = thread.WorktreePath
	}
	return result
}

// worktreeSetupRunState projects a registered run into its wire shape.
func (s *Service) worktreeSetupRunState(run *worktreeSetupRun) RunState {
	s.mu.Lock()
	state := RunState{
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
	s.mu.Unlock()

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
func (s *Service) setThreadWorktreeSetupState(threadID, state string) {
	if s.store == nil {
		return
	}
	current, err := s.store.GetThread(threadID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("thread %s: read worktree setup state: %v", threadID, err)
		}
		return
	}
	if current.WorktreeSetupState == state {
		return
	}
	if err := s.store.SetThreadWorktreeSetupState(threadID, state); err != nil {
		log.Printf("thread %s: persist worktree setup state %q: %v", threadID, state, err)
		return
	}
	current.WorktreeSetupState = state
	s.emitThreadUpdated(current)
}

// threadOccupiesWorktree reports whether the thread is still working in the
// worktree a run belongs to. A thread that has moved on (or been deleted)
// reports false, and a run settling against it is neither success nor failure.
func (s *Service) threadOccupiesWorktree(threadID, worktreePath string) bool {
	thread, err := s.store.GetThread(threadID)
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

func wireSteps(steps []worktreesetup.Step) []Step {
	wire := make([]Step, len(steps))
	for index, step := range steps {
		wire[index] = Step{
			Index: step.Index,
			Kind:  string(step.Kind),
			Label: step.Label,
			Argv:  step.Argv,
		}
	}
	return wire
}
