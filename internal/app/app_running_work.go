package app

import (
	"fmt"
	"slices"

	"agent-overflow/internal/providerupdate"
	"agent-overflow/internal/terminal"
)

// workKind is one kind of work that can be running on this computer. Every
// action that waits for running work or refuses on it asks runningWorkIn,
// which probes every kind in workKinds, so a kind added there reaches all of
// them: the merged-PR worktree cleanup, worktree removal, a thread's
// workspace switch, project deletion, conversation transfer and the restart
// to update. An action that stops or keeps a kind itself names it as
// handled; every other kind refuses or waits.
type workKind int

const (
	// workTurn is an open provider turn.
	workTurn workKind = iota + 1
	// workAwaitingAnswer is an approval or question the agent asked.
	workAwaitingAnswer
	// workQueuedMessages is a message queued, being sent or awaiting its
	// echo.
	workQueuedMessages
	// workBackgroundTasks is a background agent or command the agent
	// launched.
	workBackgroundTasks
	// workShellCommand is a command the person ran from the composer.
	workShellCommand
	// workTerminal is an open terminal.
	workTerminal
	// workWorktreeSetup is a worktree setup recipe.
	workWorktreeSetup
	// workRemoteWatch is a command another computer runs for a thread,
	// whose completion message has not reached it yet.
	workRemoteWatch
	// workRemoteJob is a command this computer runs for another computer.
	workRemoteJob
	// workWorkflow is a workflow run.
	workWorkflow
	// workProviderUpdate is a provider CLI update.
	workProviderUpdate
	// workAgentWatch is a standing agent's watch script, which runs in its
	// thread's workspace or a directory the watch names.
	workAgentWatch
)

// workKinds is every kind, in the order runningWorkIn probes them. Part of
// the order is load-bearing for the update: triage (workTurn) is read before
// the flush dispatcher (workQueuedMessages), because a queue handoff keeps
// its triage claim until the dispatcher has published its in-flight count.
var workKinds = []workKind{
	workTurn,
	workAwaitingAnswer,
	workQueuedMessages,
	workBackgroundTasks,
	workShellCommand,
	workTerminal,
	workWorktreeSetup,
	workRemoteWatch,
	workRemoteJob,
	workWorkflow,
	workProviderUpdate,
	workAgentWatch,
}

// String says that work of the kind is running, for a consumer without
// wording of its own ("cannot X while <it>").
func (k workKind) String() string {
	switch k {
	case workTurn:
		return "an agent is responding"
	case workAwaitingAnswer:
		return "an agent is waiting for an answer"
	case workQueuedMessages:
		return "messages are queued"
	case workBackgroundTasks:
		return "background tasks are running"
	case workShellCommand:
		return "a shell command is running"
	case workTerminal:
		return "a terminal is open"
	case workWorktreeSetup:
		return "the worktree is running its setup"
	case workRemoteWatch:
		return "a remote command is running"
	case workRemoteJob:
		return "a command is running here for another computer"
	case workWorkflow:
		return "a workflow is running"
	case workProviderUpdate:
		return "a provider CLI update is running"
	case workAgentWatch:
		return "an assistant's watch script is running"
	default:
		return fmt.Sprintf("work of kind %d is running", int(k))
	}
}

// workScope is where runningWorkIn looks.
type workScope struct {
	// threads are the threads whose own work counts: their turns, queues,
	// background tasks, shell commands, terminals and remote commands.
	threads []string
	// dir is a checkout whose work counts whichever thread started it: a
	// shell command or terminal in it or below it, or its setup recipe.
	dir string
	// computer counts all work on this computer, and threads and dir are
	// not read. Its probes only read their owners: the update asks under
	// the work admission lock, where no probe may take a thread action lock
	// or call the workflow actor.
	computer bool
}

// runningWork is one kind found running, with how many of it: background
// tasks are counted, every other kind is 1.
type runningWork struct {
	kind  workKind
	count int
}

func (w runningWork) running() bool { return w.count > 0 }

// runningWorkIn returns the first kind of work running in scope that the
// caller does not handle itself, or a runningWork that is not running.
func (a *App) runningWorkIn(scope workScope, handled ...workKind) (runningWork, error) {
	for _, kind := range workKinds {
		if slices.Contains(handled, kind) {
			continue
		}
		count, err := a.probeWork(kind, scope)
		if err != nil {
			return runningWork{}, fmt.Errorf("check whether %s: %w", kind, err)
		}
		if count > 0 {
			return runningWork{kind: kind, count: count}, nil
		}
	}
	return runningWork{}, nil
}

// probeWork counts the work of one kind running in scope.
func (a *App) probeWork(kind workKind, scope workScope) (int, error) {
	if scope.computer {
		return a.probeComputerWork(kind)
	}
	count := 0
	if scope.dir != "" {
		found, err := a.probeDirWork(kind, scope.dir)
		if err != nil || found {
			return boolCount(found), err
		}
	}
	for _, threadID := range scope.threads {
		n, err := a.probeThreadWork(kind, threadID)
		if err != nil {
			return 0, fmt.Errorf("thread %s: %w", threadID, err)
		}
		if n > 0 && kind != workBackgroundTasks {
			return n, nil
		}
		count += n
	}
	return count, nil
}

func (a *App) probeThreadWork(kind workKind, threadID string) (int, error) {
	switch kind {
	case workTurn:
		if _, open, err := a.store.GetActiveTurn(threadID); err != nil || open {
			return boolCount(open), err
		}
		if entry, ok := a.sessionManager().get(threadID); ok && entry.Liveness != nil && entry.Liveness.ActiveTurns.Load() > 0 {
			return 1, nil
		}
		return boolCount(a.triage != nil && a.triage.HasInFlightTurnOrRound(threadID)), nil
	case workAwaitingAnswer:
		if a.triage == nil {
			return 0, nil
		}
		pending := a.triage.PendingInteractiveRequests(threadID)
		return boolCount(len(pending.Approvals)+len(pending.UserInputs) > 0), nil
	case workQueuedMessages:
		return boolCount((a.triage != nil && a.triage.HasPendingWork(threadID)) || a.pendingFlushWorkCount(threadID) > 0), nil
	case workBackgroundTasks:
		return a.countRunningBackgroundTasks(threadID)
	case workShellCommand:
		return boolCount(a.shellCommands != nil && a.shellCommands.ActiveRunID(threadID) != ""), nil
	case workTerminal:
		return boolCount(a.terminals != nil && len(a.terminals.List(threadID)) > 0), nil
	case workRemoteWatch:
		pending, err := a.store.HasPendingRemoteWatches(threadID)
		return boolCount(pending), err
	case workAgentWatch:
		return boolCount(len(a.agentWatchDirs(threadID)) > 0), nil
	default:
		// The other kinds belong to a directory or to the computer.
		return 0, nil
	}
}

func (a *App) probeDirWork(kind workKind, dir string) (bool, error) {
	switch kind {
	case workShellCommand:
		return a.shellCommands != nil && a.shellCommands.HasActiveIn(dir), nil
	case workTerminal:
		return a.terminals != nil && len(a.terminals.Matching(func(summary terminal.SessionSummary) bool {
			return pathWithin(summary.Cwd, dir)
		})) > 0, nil
	case workWorktreeSetup:
		return a.worktreeSetupService().HasRunningPath(dir), nil
	case workAgentWatch:
		return slices.ContainsFunc(a.agentWatchDirs(""), func(running string) bool { return pathWithin(running, dir) }), nil
	default:
		// The other kinds belong to a thread or to the computer.
		return false, nil
	}
}

func (a *App) probeComputerWork(kind workKind) (int, error) {
	switch kind {
	case workTurn:
		// Queues, echoes, open rounds and pending answers are read together
		// so a handoff cannot look idle between two separate reads.
		if a.triage.AnyUnfinishedWork(a.wakeupReapGrace()) {
			return 1, nil
		}
		for _, sess := range a.sessionManager().snapshot() {
			if sess.Liveness != nil && sess.Liveness.ActiveTurns.Load() > 0 {
				return 1, nil
			}
		}
		return 0, nil
	case workQueuedMessages:
		a.flushDispatch.mu.Lock()
		flushing := len(a.flushDispatch.inflightItems) != 0
		a.flushDispatch.mu.Unlock()
		return boolCount(flushing), nil
	case workBackgroundTasks:
		if a.store == nil {
			return 0, nil // Bare update fixtures have no session-capable store.
		}
		// A background task runs inside its thread's session.
		count := 0
		for threadID := range a.sessionManager().snapshot() {
			n, err := a.countRunningBackgroundTasks(threadID)
			if err != nil {
				return 0, fmt.Errorf("thread %s: %w", threadID, err)
			}
			count += n
		}
		return count, nil
	case workShellCommand:
		return boolCount(a.shellCommands != nil && a.shellCommands.HasActive()), nil
	case workTerminal:
		// A terminal whose shell has exited holds nothing a restart ends.
		return boolCount(a.terminals != nil && len(a.terminals.ThreadProcesses()) != 0), nil
	case workRemoteJob:
		return boolCount(a.remoteJobs != nil && a.remoteJobs.HasActive()), nil
	case workWorkflow:
		if a.store == nil {
			return 0, nil
		}
		return a.store.CountWorkItemsInStates("running")
	case workProviderUpdate:
		for _, status := range a.providerUpdateService().Snapshot() {
			if status.Run.Phase == providerupdate.PhaseRunning {
				return 1, nil
			}
		}
		return 0, nil
	default:
		// workAwaitingAnswer is part of the triage read under workTurn. A
		// setup recipe holds a work admission lease for its whole run, which
		// the update's quiescence reports before it asks here. A remote
		// watch is durable and resumes after a restart, and so is an
		// assistant's watch: the row of an interval run cut short stays
		// `running` and the next boot runs it again, and a long-running
		// watch starts afresh (recoverAgentWakesAtBoot).
		return 0, nil
	}
}

func boolCount(running bool) int {
	if running {
		return 1
	}
	return 0
}
