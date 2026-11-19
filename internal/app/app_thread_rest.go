package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// Bringing a thread to rest (docs/specs/standing-agents.md, "At rest"): it
// ends with no turn, no queued message, no open prompt, no background work,
// no armed loop, watch or reminder, and no open work below it. An archive
// and a Stop of a spawned thread share the capture (captureRestingThreadWork),
// the session teardown (teardownAndCloseSession) and the release of what the
// app queued for the thread and what it started elsewhere
// (stopRestingThreadWork). An archive then hides the thread and archives
// the workers it owns; a Stop leaves the thread where it is and goes on to
// every request on it and every request it made (restThreadHere).

// restCause is why a thread is brought to rest, in the words its records
// keep.
type restCause struct {
	// outcome is the last outcome of each loop and watch the rest cancels.
	outcome string
	// why ends the notice an outside send leaves when it is withdrawn.
	why string
	// keepFinished keeps a finished remote job's completion for the thread:
	// an archived thread takes it once unarchived. A thread brought to rest
	// and left open would take it at once, so every other rest drops it.
	keepFinished bool
}

// archiveRest is an archive's cause.
var archiveRest = restCause{
	outcome:      "cancelled: the thread was archived",
	why:          "the thread was archived before the agent took it in",
	keepFinished: true,
}

// restingThreadWork is what a thread was waiting on at the moment it was
// brought to rest: the asks it owns and the remote commands it started, the
// messages queued for it, and for an archive the workers it owned then.
type restingThreadWork struct {
	threadID string
	cause    restCause
	// captured is set when this rest captured the thread's work; a repeated
	// archive captures and stops nothing of the thread's own.
	captured bool
	asks     []store.ThreadRequest
	commands []store.RemoteWatch
	// queued are the send ids of the messages in the thread's queue.
	queued []string
	// workers are the unarchived threads an archived thread owned, at any
	// depth through archived workers, once its archive stood and its session
	// was stopped (readArchivedWorkers), each with the owner that fences its
	// flip. A repeated archive reads them too, so a worker an earlier one
	// could not archive is tried again however deep it is.
	workers []store.OwnedWorker
	// cleanup gathers what the rest could not do, part by part.
	cleanup threadCleanupError
}

// held reports whether sendID was in the thread's queue when it was
// brought to rest.
func (w restingThreadWork) held(sendID string) bool {
	return slices.Contains(w.queued, sendID)
}

// captureRestingThreadWork runs in the critical section that brings the
// thread to rest, under the thread's action and mutation locks, which
// unarchive, remote command admission and every queue write also take. It
// ends the thread's parked calls, disarms its wakes, cancels its loops and
// watches and stops their runs, drops the remote completions the cause does
// not keep, and names the work open and the messages queued at that moment.
// A later wait or thread_status re-arms the wakes; for an archive, delivering
// one unarchives the thread.
//
// Its caller has closed the thread's admission (threadRestFence), so this is
// all the thread started. Once the rest reopens it, work may start again: a
// wake unarchives an archived thread, a person or another thread may send to
// it, and a loop or watch registered later reuses its label's row. So the
// schedule is cancelled here rather than after the locks are released, and
// the stop acts on these identities alone, never on what the thread is
// running or holds queued by the time it gets there.
func (a *App) captureRestingThreadWork(id string, cause restCause) restingThreadWork {
	work := restingThreadWork{threadID: id, cause: cause, captured: true}
	asks, askErr := a.disarmThreadRequests(id)
	if askErr != nil {
		work.cleanup.asks = fmt.Errorf("end the requests of %s: %w", id, askErr)
	}
	commands, commandErr := a.disarmRemoteCommands(id)
	if commandErr != nil {
		work.cleanup.commandRecords = fmt.Errorf("list the remote commands of %s: %w", id, commandErr)
	}
	if !cause.keepFinished {
		// Under the mutation lock a completion's queue write takes, so the
		// watcher queues none after this, and one queued or handed to the
		// provider is not given back to a watch that would deliver it again.
		work.cleanup.commandRecords = errors.Join(work.cleanup.commandRecords, a.dismissUndeliveredRemoteCompletions(id))
	}
	work.cleanup.schedule = a.cancelAgentWakesForThread(id, cause.outcome)
	rows, queueErr := a.store.ListFlushQueueItems(id)
	if queueErr != nil {
		work.cleanup.queue = fmt.Errorf("read the queue of %s: %w", id, queueErr)
	}
	for _, row := range rows {
		work.queued = append(work.queued, row.SendID)
	}
	work.asks, work.commands = asks, commands
	return work
}

// dismissUndeliveredRemoteCompletions drops every remote completion the
// thread's agent has not taken in.
func (a *App) dismissUndeliveredRemoteCompletions(threadID string) error {
	dropped, err := a.store.DismissUndeliveredRemoteWatches(threadID)
	if dropped > 0 {
		a.emitBackgroundChanged(threadID)
	}
	return err
}

// stopRestingThreadWork stops exactly the work its rest captured. Every door
// that brings a thread to rest runs it once the session is handled, so a
// thread stopped or archived from any of them is left in the same state. It
// runs on the app's lifetime, not its caller's, because the rest is already
// under way, and outside the locks because reaching the other computer may
// take a while. It returns what the rest could not do, the capture's and the
// session stop's part included.
func (a *App) stopRestingThreadWork(work restingThreadWork) threadCleanupError {
	if !work.captured {
		return work.cleanup
	}
	ctx, id := a.lifeCtx(), work.threadID
	work.cleanup.asks = errors.Join(work.cleanup.asks, a.cancelOwnedAsks(ctx, id, work.asks))
	undelivered, remoteErr := a.cancelRemoteCommands(ctx, id, work.commands, work.cause.keepFinished)
	work.cleanup.commands = undelivered
	work.cleanup.commandRecords = errors.Join(work.cleanup.commandRecords, remoteErr)
	// What the app had queued for the thread goes back to its owners, live
	// session or not, so nothing of it reaches the agent when the thread is
	// next started (withdrawQueuedMessages): a request's answer or reminder
	// to its request, disarmed with it, so the retry cannot bring the thread
	// back, and the spawner's answer to its async questions to them. Its
	// loops and watches were cancelled with the capture. After the cancel, so
	// a finished job's completion an archive keeps goes back to the watch the
	// cancel kept, and arrives once the thread is unarchived; one the cause
	// does not keep was dismissed with the capture, so its queued message is
	// only taken out.
	work.cleanup.queue = errors.Join(work.cleanup.queue, a.withdrawQueuedMessages(id, work.held, queuedWithdrawal{why: work.cause.why}))
	return work.cleanup
}

// restStop is who brings a spawned thread to rest, and what the records of
// the rest say about it.
type restStop struct {
	cause restCause
	// turn is the stop row of the turn the rest cuts, and the cause a
	// request that turn answers settles with.
	turn turnStop
	// answer is what a request on the thread, and one the thread made, is
	// cancelled with.
	answer string
	// user marks a person's Stop, which a paired computer is told about so
	// its records name the user.
	user bool
}

// userRestStop is a person's Stop of a spawned thread's Agents row.
func userRestStop() restStop {
	return restStop{
		cause: restCause{
			outcome: "cancelled: the user stopped the thread",
			why:     "the user stopped the thread before the agent took it in",
		},
		turn:   stopFromAgentsTray(),
		answer: threadRequestUserStop,
		user:   true,
	}
}

// threadRestStop is an agent's thread_cancel of a thread it spawned,
// attributed to the calling thread as stopByThread attributes an interrupt.
func threadRestStop(title, threadID, computer string) restStop {
	turn := stopByThread(title, threadID, computer)
	return restStop{
		cause: restCause{
			outcome: fmt.Sprintf("cancelled: thread %q stopped the thread", title),
			why:     fmt.Sprintf("thread %q stopped the thread before the agent took it in", title),
		},
		turn:   turn,
		answer: turn.cause.Answer(""),
	}
}

// restThreadHere brings a spawned thread on this computer to rest for a Stop
// of the spawn that made it: its loops, watches and reminders, its queue,
// the requests on it, its turn and session, the commands it started
// elsewhere, and the requests it made, each spawned thread brought to rest
// by this same rule. It is not archived or moved: a later message starts it
// again on its history. visited holds the threads this Stop already reached
// here, so a spawn cycle ends. What fails is named in result.Failures and
// does not keep the rest of it from running.
func (a *App) restThreadHere(ctx context.Context, threadID string, by restStop, visited map[string]bool, result *ThreadAgentStop) {
	ctx = context.WithoutCancel(ctx)
	fail := func(step string, err error) {
		if err != nil {
			result.Failures = append(result.Failures, step+": "+err.Error())
		}
	}
	// A thread a running workflow run works in is that run's: ending its
	// session would stop the run's work and the run would start it again.
	// The runner makes every member thread, so a spawn reaches one only
	// through a ledger row nothing writes.
	if run, err := a.store.ThreadRunningWorkflowRun(threadID); err != nil {
		fail("read its workflow runs", err)
	} else if run != "" {
		result.Failures = append(result.Failures, fmt.Sprintf(
			"it works in workflow run %s, which it did not start; pause that run from the workflows view", run))
		return
	}
	// From here to the end of the rest the thread takes no app-written
	// message and starts nothing, and what it had started is recorded where
	// the steps below read it.
	release := a.holdRest(threadID)
	defer release()
	reopen := a.resting.close(threadID)
	defer reopen()
	work, gone, err := a.captureThreadRest(threadID, by.cause)
	if gone {
		return
	}
	fail("stop the thread", err)
	if err != nil {
		// Without the capture nothing is known to be stopped; the requests it
		// made are still stopped below.
		work = restingThreadWork{threadID: threadID}
	}
	// Before anything else, so a run of the thread's that rests meanwhile
	// does not start it again (pauseRunsOfRestingThread).
	a.pauseRunsOfRestingThread(threadID, result)
	// Every request on it settles before its session ends, so the teardown
	// gives none of their messages back to the queue.
	a.stopRequestsOnRestingThread(ctx, threadID, by, result)
	background, err := a.endRestingThreadSession(threadID, by, result)
	result.Background += background
	fail("end its session", err)
	// Its async questions outlive the session, and close with the rest as
	// its other prompts did.
	fail("dismiss its questions", a.dismissRestingAsyncQuestions(threadID))
	// After the session end, as an archive's: what the teardown dropped went
	// back to its owners, and what it left is withdrawn here.
	cleanup := a.stopRestingThreadWork(work)
	fail("stop its work", cleanup.Unwrap())
	a.stopRequestsOfRestingThread(ctx, threadID, by, visited, result)
	// A prompt of a worker left running goes back to the user; the workers
	// brought to rest took theirs with their sessions.
	a.reroutePromptsFrom(threadID)
}

// holdRest holds threadID's rest fence for a stop (threadRestFence.hold).
// A request held for the thread's rest does not settle at rest meanwhile
// (heldThreadAtRest); one the stop left open reads the rest again once it is
// released.
func (a *App) holdRest(threadID string) func() {
	release := a.resting.hold(threadID)
	return func() {
		release()
		a.noteRestWaitsMayChange()
	}
}

// captureThreadRest captures the work of a thread its rest has fenced, under
// its action and mutation locks. The action lock waits out a session start in
// progress, and the mutation lock is the one every queue write checks the
// fence under. Gone reports a thread that no longer exists here.
func (a *App) captureThreadRest(threadID string, cause restCause) (work restingThreadWork, gone bool, err error) {
	unlock := a.threadLocks().Lock(threadID)
	defer unlock()
	unlockMutations, err := a.threadApplication().LockMutable(a.lifeCtx(), threadID)
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, store.ErrThreadGone) {
		return restingThreadWork{}, true, nil
	}
	if err != nil {
		return restingThreadWork{}, false, err
	}
	defer unlockMutations()
	return a.captureRestingThreadWork(threadID, cause), false, nil
}

// stopRequestsOnRestingThread settles every open request on the thread, from
// any thread on any computer, as cancelled by the stop: its queued message is
// taken back and a turn it started is cut, and its sender is woken or not as
// it asked to be.
func (a *App) stopRequestsOnRestingThread(ctx context.Context, threadID string, by restStop, result *ThreadAgentStop) {
	receipts, err := a.store.ListThreadRequestReceiptsForThread(threadID)
	if err != nil {
		result.Failures = append(result.Failures, "list the requests on it: "+err.Error())
		return
	}
	for _, receipt := range receipts {
		if !slices.Contains(store.ThreadReceiptOpenStates(), receipt.State) {
			continue
		}
		effect, err := a.stopThreadRequestWorkBy(ctx, receipt.Token, by.answer, func(store.ThreadRequestReceipt) turnStop { return by.turn })
		if err != nil {
			result.Failures = append(result.Failures, "stop a request on it: "+err.Error())
			continue
		}
		result.Interrupted = result.Interrupted || effect == threadtools.EffectInterrupted
	}
}

// endRestingThreadSession cuts the thread's turn with the stop's row and
// ends its session, the provider process and everything it runs with it,
// under the action lock every send and session start takes, so no turn
// opens between the two. A thread with no session still has its queue
// dropped, as End Session drops it. Its browser pages stay open, as End
// Session leaves them. It reports the background tasks the session end
// stopped.
func (a *App) endRestingThreadSession(threadID string, by restStop, result *ThreadAgentStop) (int, error) {
	unlock := a.threadLocks().Lock(threadID)
	defer unlock()
	var errs []error
	background := 0
	sess, live := a.sessionManager().get(threadID)
	if live {
		running, err := a.runningBackgroundWorkForThread(threadID, sess.Provider)
		if err != nil {
			errs = append(errs, fmt.Errorf("list its background work: %w", err))
		}
		background = len(running)
		if a.triage != nil && a.triage.SampleInterrupt(threadID).CutTurn() >= 0 {
			interrupted, err := a.interruptTurnHoldingLock(threadID, anyOpenTurn, by.turn, a.observeInterruptTarget(threadID))
			if err != nil {
				errs = append(errs, fmt.Errorf("interrupt its turn: %w", err))
			}
			result.Interrupted = result.Interrupted || interrupted
		}
	}
	sess, _ = a.sessionManager().take(threadID)
	if err := a.teardownAndCloseSession(threadID, sess); err != nil {
		errs = append(errs, err)
	}
	return background, errors.Join(errs...)
}
