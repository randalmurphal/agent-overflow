package main

import (
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/eventchan"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
)

// Pre-thread worktree setup.
//
// A draft thread has no row — materializing one just to cut a worktree is what
// produced the empty-draft-cleanup race this surface exists to remove. So the
// worktree/branch operations a draft performs are PROJECT-scoped
// (app_project_workspace.go), and the setup recipe they kick off has to be able
// to run before any thread exists.
//
// The run machinery is unchanged; only its identity is. A run is registered
// under a KEY, which is:
//
//   - the thread id, for an ordinary bound run — exactly as before; or
//   - workspaceSetupRunKey(worktreePath) for an unbound one.
//
// An unbound run differs from a bound one in exactly three ways, all of them
// consequences of having no row: it writes no durable state, it emits an empty
// `threadId` (clients key on `worktreePath`, which every frame now carries),
// and it is never demoted by the occupancy check — nobody occupies a worktree
// nobody has been created into yet.
//
// When a thread is finally created into that worktree, CreateThread calls
// adoptWorkspaceWorktreeSetup: the record is re-keyed onto the thread id, the
// durable column is stamped from whatever state the run has reached, and the
// re-key is announced on the wire. From that moment it is an ordinary bound
// run in every respect.
//
// A restart LOSES an unbound run, deliberately — see sweepCrashedWorktreeSetups.

// workspaceSetupRunKey namespaces a pre-thread run's map key. The NUL byte is
// what makes a collision with a thread id (a uuid) structurally impossible
// rather than merely unlikely.
func workspaceSetupRunKey(worktreePath string) string {
	return "ws\x00" + worktreePath
}

// findWorkspaceSetupRunLocked resolves an unbound run by the worktree it
// provisions. Callers hold a.worktreeSetup.mu.
//
// The exact-key hit is the ordinary case: the caller round-trips the very
// string defaultWorktreePath returned. The fallback scan exists because the
// adopting caller does NOT necessarily hold that string — CreateThread
// validates its inherited path through findWorktree and adopts git's own
// spelling of it, which can differ (a symlinked parent, a case-insensitive
// volume). The map is bounded by "runs in flight plus retained failures", so
// the walk is cheap, and it is restricted to unbound runs so it can never
// steal a thread's record.
func (a *App) findWorkspaceSetupRunLocked(worktreePath string) (*worktreeSetupRun, string) {
	key := workspaceSetupRunKey(worktreePath)
	if run := a.worktreeSetup.runs[key]; run != nil {
		return run, key
	}
	for candidate, run := range a.worktreeSetup.runs {
		if run.threadID != "" {
			continue
		}
		if gitops.SameFilesystemPath(run.worktreePath, worktreePath) {
			return run, candidate
		}
	}
	return nil, ""
}

// --- Kickoff ---

// startWorkspaceWorktreeSetup is the fire-and-forget entry point the
// project-scoped worktree paths use. Like startThreadWorktreeSetup, a refusal
// is logged rather than surfaced: the worktree was created, and that is what
// the caller was asked to report on.
func (a *App) startWorkspaceWorktreeSetup(project store.Project, worktreePath string) {
	if err := a.launchWorkspaceWorktreeSetup(project, worktreePath, false); err != nil {
		log.Printf("workspace %s: worktree setup not started: %v", worktreePath, err)
	}
}

func (a *App) launchWorkspaceWorktreeSetup(project store.Project, worktreePath string, requireRecipe bool) error {
	// The key is deliberately NOT chosen here. An unbound target's key depends
	// on what the run map already holds (a retained failure under a
	// differently-spelled path must be REPLACED, not registered beside), and
	// resolving that in its own critical section left a window in which a
	// concurrent retry or adoption moved the answer. launchWorktreeSetup
	// resolves and reserves it in one.
	return a.launchWorktreeSetup(worktreeSetupTarget{
		projectID:    strings.TrimSpace(project.ID),
		projectRoot:  strings.TrimSpace(project.Path),
		worktreePath: strings.TrimSpace(worktreePath),
	}, requireRecipe)
}

// --- Adoption ---

// adoptWorkspaceWorktreeSetup hands an unbound run to the thread that has just
// been created into its worktree. It is a no-op when no unbound run covers the
// path, which is the common case (no recipe configured, or the run already
// succeeded and dropped its record).
//
// Wire contract: adopting a LIVE run emits a `started`-shaped SNAPSHOT frame
// carrying BOTH the new threadId and the worktreePath. That is deliberately the
// same shape a fresh run's opening frame has — a client that was following the
// unbound run re-keys onto the thread, and a client that was following the
// thread learns the whole run at once rather than joining mid-stream. Because
// it is a snapshot and not a beginning, it carries the run's progress too
// (step statuses, the output tail and its sequence), so applying it never
// rewinds a client that had been watching.
//
// A run that has already SETTLED gets no started frame at all: it is not
// running, and a frame that says it is would be a lie the adopting client has
// to be corrected out of. It gets the terminal frame — the run's real state —
// and the durable column is stamped from that same truth.
//
// Both the settled read and the stamp happen under a.worktreeSetup.mu, which is
// what orders this write against a settle racing it. See worktreeSetupRun.settled.
//
// Reports whether a run was adopted, so a caller holding a thread row it has
// already read knows the durable column moved underneath it.
func (a *App) adoptWorkspaceWorktreeSetup(worktreePath string, thread store.Thread) bool {
	worktreePath = strings.TrimSpace(worktreePath)
	threadID := strings.TrimSpace(thread.ID)
	if worktreePath == "" || threadID == "" {
		return false
	}

	a.worktreeSetup.mu.Lock()
	run, key := a.findWorkspaceSetupRunLocked(worktreePath)
	if run == nil {
		a.worktreeSetup.mu.Unlock()
		return false
	}
	// A brand-new thread cannot already own a run, but the map is keyed by a
	// caller-supplied id in tests and by uuid collision in theory. Refusing is
	// the only safe move: overwriting would strand a live goroutine whose
	// record nothing can reach, and therefore whose cancel nothing can call.
	if existing := a.worktreeSetup.runs[threadID]; existing != nil && existing != run {
		a.worktreeSetup.mu.Unlock()
		log.Printf("thread %s: not adopting the worktree setup for %s; the thread already has a run", threadID, worktreePath)
		return false
	}
	delete(a.worktreeSetup.runs, key)
	run.key = threadID
	run.threadID = threadID
	a.worktreeSetup.runs[threadID] = run

	var frame WorktreeSetupEvent
	if run.settled {
		// The outcome is already decided. Stamp THAT, never "running", and say
		// so on the wire in one terminal frame — a `started` frame here would
		// claim a run is in flight that has already ended, and the client would
		// have to be corrected out of it. A client meeting the run for the first
		// time on a terminal frame re-snapshots, which is the store's standing
		// answer for a frame about a run it never saw begin.
		switch run.state {
		case worktreeSetupRunFailed:
			a.setThreadWorktreeSetupState(threadID, store.WorktreeSetupStateFailed)
		case worktreeSetupRunSucceeded, worktreeSetupRunCancelled:
			a.setThreadWorktreeSetupState(threadID, store.WorktreeSetupStateNone)
		}
		frame = WorktreeSetupEvent{
			Phase:        worktreeSetupPhaseFinished,
			ThreadID:     threadID,
			RunID:        run.id,
			WorktreePath: run.worktreePath,
			State:        run.state,
			Error:        run.errorText,
			FinishedAt:   run.finishedAt,
		}
	} else {
		a.setThreadWorktreeSetupState(threadID, store.WorktreeSetupStateRunning)
		// Sequence BEFORE content, exactly as worktreeSetupRunState reads them:
		// a chunk emitted between the two reads lands in Output but reports a
		// seq the client already has, so it is ignored rather than appended
		// twice. The reverse order would drop it. (The tail is self-guarded and
		// seq is atomic; neither is covered by the mutex we hold.)
		outputSeq := run.seq.Load()
		output := run.tail.String()
		frame = WorktreeSetupEvent{
			Phase:        worktreeSetupPhaseStarted,
			ThreadID:     threadID,
			RunID:        run.id,
			WorktreePath: run.worktreePath,
			Steps:        run.steps,
			StepStatuses: append([]string(nil), run.statuses...),
			Output:       output,
			OutputSeq:    outputSeq,
			StartedAt:    run.startedAt,
		}
	}
	a.worktreeSetup.mu.Unlock()

	a.emitEvent(eventchan.WorktreeSetup, frame)
	return true
}

// --- Cancellation ---

// cancelWorktreeSetupsForPath stops EVERY run executing in a directory — bound
// and unbound alike — joins their goroutines, and drops their records. The join
// is what makes it safe to remove the directory afterwards.
//
// Keyed by path rather than by owner because the directory is what is being
// destroyed. A caller that walked the threads pointing at the worktree sees
// neither an unbound run (it has no thread to be found by) nor a run belonging
// to a thread the walk missed, and either one is a live process still writing
// into a directory git is about to delete.
//
// Durable state is NOT cleared here: an unbound run has none, and a bound one's
// column belongs to its thread, which the caller clears through
// cancelThreadWorktreeSetup.
func (a *App) cancelWorktreeSetupsForPath(worktreePath string) {
	worktreePath = strings.TrimSpace(worktreePath)
	if worktreePath == "" {
		return
	}
	a.worktreeSetup.mu.Lock()
	var runs []*worktreeSetupRun
	for _, run := range a.worktreeSetup.runs {
		if !gitops.SameFilesystemPath(run.worktreePath, worktreePath) {
			continue
		}
		run.cancelled = true
		runs = append(runs, run)
	}
	a.worktreeSetup.mu.Unlock()
	for _, run := range runs {
		run.cancel()
		<-run.done
		a.dropWorktreeSetupRun(run)
	}
}

// --- Bound methods ---

// GetWorkspaceWorktreeSetup is GetThreadWorktreeSetup for a worktree that no
// thread occupies yet. The result's ThreadID is empty; the panel keys on
// WorktreePath until a thread adopts the run.
//
// Unlike the thread variant there is no durable fallback: an unbound run has no
// row, so "no record" is genuinely idle (see sweepCrashedWorktreeSetups).
//
// projectID is not decoration: the path must be one of THAT project's
// worktrees. Without the membership check the pair would be an oracle for any
// directory a caller cares to name — answering with the stdout of whatever
// recipe happened to run there.
//
// LocalOnly: the payload is the stdout/stderr of local commands run against the
// user's checkout, same data class as GetThreadWorktreeSetup.
func (a *App) GetWorkspaceWorktreeSetup(projectID, worktreePath string) (WorktreeSetupRunState, error) {
	worktreePath = strings.TrimSpace(worktreePath)
	if worktreePath == "" {
		return WorktreeSetupRunState{}, fmt.Errorf("get worktree setup: worktree path is required")
	}
	project, err := a.projectForWorkspaceOp(projectID)
	if err != nil {
		return WorktreeSetupRunState{}, fmt.Errorf("get worktree setup: %w", err)
	}
	worktree, ok, err := a.findWorktree(project.Path, worktreePath)
	if err != nil {
		return WorktreeSetupRunState{}, fmt.Errorf("get worktree setup: validate worktree: %w", err)
	}
	if !ok {
		return WorktreeSetupRunState{}, fmt.Errorf("get worktree setup: %s is not a worktree of project %s", worktreePath, project.Path)
	}
	worktreePath = worktree.Path

	a.worktreeSetup.mu.Lock()
	run, _ := a.findWorkspaceSetupRunLocked(worktreePath)
	a.worktreeSetup.mu.Unlock()
	if run != nil {
		return a.worktreeSetupRunState(run), nil
	}
	return WorktreeSetupRunState{
		State:        worktreeSetupRunIdle,
		WorktreePath: worktreePath,
		Steps:        []WorktreeSetupStep{},
		StepStatuses: []string{},
	}, nil
}

// RetryWorkspaceWorktreeSetup re-runs the project's recipe over a worktree no
// thread occupies yet. Like the thread variant it re-reads the recipe rather
// than replaying the failed run's copy, so fixing it in Settings and hitting
// Retry does what the user means.
//
// The path must be one of the project's registered worktrees, and must not be
// the project ROOT. Both are boundaries, not conveniences: without the first
// this method would run the project's argv recipe with an arbitrary
// caller-supplied cwd, and git lists the main worktree alongside the others, so
// without the second the recipe would run over the user's primary checkout —
// the same refusal removeProjectWorktree issues, for the same reason.
//
// LocalOnly: this executes the project's argv commands. RCE-equivalent.
func (a *App) RetryWorkspaceWorktreeSetup(projectID, worktreePath string) error {
	worktreePath = strings.TrimSpace(worktreePath)
	if worktreePath == "" {
		return fmt.Errorf("retry worktree setup: worktree path is required")
	}
	project, err := a.projectForWorkspaceOp(projectID)
	if err != nil {
		return fmt.Errorf("retry worktree setup: %w", err)
	}
	if gitops.SameFilesystemPath(project.Path, worktreePath) {
		return fmt.Errorf("retry worktree setup: refusing to run the worktree recipe in the project root")
	}
	worktree, ok, err := a.findWorktree(project.Path, worktreePath)
	if err != nil {
		return fmt.Errorf("retry worktree setup: validate worktree: %w", err)
	}
	if !ok {
		return fmt.Errorf("retry worktree setup: %s is not a worktree of project %s", worktreePath, project.Path)
	}
	return a.launchWorkspaceWorktreeSetup(project, worktree.Path, true)
}
