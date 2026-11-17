package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sync"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/usermessage"
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
	err     error
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
		askErr = fmt.Errorf("end the requests of %s: %w", id, askErr)
	}
	commands, commandErr := a.disarmRemoteCommands(id)
	var dismissErr error
	if !cause.keepFinished {
		// Under the mutation lock a completion's queue write takes, so the
		// watcher queues none after this, and one queued or handed to the
		// provider is not given back to a watch that would deliver it again.
		dismissErr = a.dismissUndeliveredRemoteCompletions(id)
	}
	wakesErr := a.cancelAgentWakesForThread(id, cause.outcome)
	rows, queueErr := a.store.ListFlushQueueItems(id)
	if queueErr != nil {
		queueErr = fmt.Errorf("read the queue of %s: %w", id, queueErr)
	}
	for _, row := range rows {
		work.queued = append(work.queued, row.SendID)
	}
	work.asks, work.commands, work.err = asks, commands, errors.Join(askErr, commandErr, dismissErr, wakesErr, queueErr)
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
// take a while.
func (a *App) stopRestingThreadWork(work restingThreadWork) error {
	if !work.captured {
		return work.err
	}
	ctx, id := a.lifeCtx(), work.threadID
	a.cancelOwnedAsks(ctx, id, work.asks)
	undelivered, remoteErr := a.cancelRemoteCommands(ctx, id, work.commands, work.cause.keepFinished)
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
	withdrawErr := a.withdrawQueuedMessages(id, work.held, queuedWithdrawal{why: work.cause.why})
	return errors.Join(work.err, undelivered, remoteErr, withdrawErr)
}

// errThreadResting refuses an app-written message for a thread a Stop is
// bringing to rest, as errThreadArchived refuses one for an archived thread.
// Its caller keeps what the message was for, or settles it as undeliverable:
// a worker's prompt goes to the user, an outside send is refused.
var errThreadResting = errors.New("the thread is being stopped")

// errSessionEnded refuses work a call starts once the session it
// authenticated as has ended.
var errSessionEnded = errors.New("the calling session has ended")

// threadRestFence holds the threads being brought to rest. While a Stop
// holds a thread (hold), from the start of the rest to the end of its
// cascade, the thread takes no app-written message (registerQueueItem) and
// starts no session for one (startAgentNoticeSession), so a worker's prompt,
// an answer or a completion arriving meanwhile does not start it again; a
// person's own message is not held. While a Stop or an archive has closed a
// thread's admission (close), it starts no work of its own: every API that
// admits work a thread starts (a spawn, send, ask or reminder, an assistant,
// a loop or watch, a remote command, a workflow run's start, binding or
// resumption) admits it through admit, and close waits out the admissions
// already under way, so what the rest reads after close is everything the
// thread started.
type threadRestFence struct {
	mu      sync.Mutex
	changed *sync.Cond
	// threads counts, by thread, the holds on its app-written messages.
	threads map[string]int
	// closed counts, by thread, the closures of its admission.
	closed map[string]int
	// admitting counts, by thread, the admissions under way.
	admitting map[string]int
}

func (f *threadRestFence) init() {
	if f.changed == nil {
		f.threads = make(map[string]int)
		f.closed = make(map[string]int)
		f.admitting = make(map[string]int)
		f.changed = sync.NewCond(&f.mu)
	}
}

// hold fences threadID's app-written messages until the returned release
// runs. Holds nest.
func (f *threadRestFence) hold(threadID string) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	f.threads[threadID]++
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.threads[threadID]--; f.threads[threadID] <= 0 {
			delete(f.threads, threadID)
		}
	}
}

// close closes threadID's admission until the returned reopen runs, and
// returns once every admission already under way for it has finished.
// Closures nest. An admission may wait on any lock, so close must not be
// called with one held.
func (f *threadRestFence) close(threadID string) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	f.closed[threadID]++
	for f.admitting[threadID] > 0 {
		f.changed.Wait()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.closed[threadID]--; f.closed[threadID] <= 0 {
				delete(f.closed, threadID)
			}
		})
	}
}

// admit admits one piece of work threadID starts, refused with
// errThreadResting while its admission is closed, and with
// errSessionEnded when current, the check that the session the work
// belongs to is still the thread's, says it is not. current runs under the
// fence, so a rest, which ends the session only while admission is closed,
// either waits for the admission or is seen by it. done runs once the work
// is recorded where a rest reads it.
func (f *threadRestFence) admit(threadID string, current func() bool) (done func(), err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	if f.closed[threadID] > 0 {
		return nil, errThreadResting
	}
	if current != nil && !current() {
		return nil, errSessionEnded
	}
	f.admitting[threadID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.admitting[threadID]--; f.admitting[threadID] <= 0 {
				delete(f.admitting, threadID)
			}
			f.changed.Broadcast()
		})
	}, nil
}

// admitWork is threadRestFence.admit with the refusal an agent reads.
// session is the token of the provider session the call authenticated as
// (threadtools.Caller.Session, transport.CallerScope.Session), empty for a
// call no session made, such as a person's: work a session starts is
// admitted only while that session is still threadID's, so a call that
// passed authentication before a rest ended its session starts nothing
// however late it gets here, and one from a session started since does.
func (a *App) admitWork(threadID, session string) (func(), error) {
	if hook := a.admitWorkForTest; hook != nil {
		hook(threadID)
	}
	// A thread a delete claimed starts nothing; one admitted before the
	// claim is drained by the delete's close (familyDelete.verify).
	if err := a.store.CheckThreadNotDeleting(threadID); err != nil {
		return nil, errorsx.Public(threadtools.CodeInvalidRequest,
			"This thread is being deleted and cannot start new work.", err)
	}
	// A thread under an assistant starts nothing while assistants are off;
	// one admitted before they went off is drained by the pause's close.
	if err := a.refusePausedAssistantThread(threadID); err != nil {
		return nil, err
	}
	var current func() bool
	if session != "" {
		current = func() bool {
			live, ok := a.sessionManager().get(threadID)
			return ok && live.Token == session
		}
	}
	done, err := a.resting.admit(threadID, current)
	switch {
	case errors.Is(err, errSessionEnded):
		return nil, errorsx.Public(threadtools.CodeInvalidRequest,
			"The agent session that made this call has ended, so it cannot start new work.", err)
	case err != nil:
		return nil, errorsx.Public(threadtools.CodeInvalidRequest,
			"This thread is being stopped and cannot start new work now.", err)
	}
	return done, nil
}

// holds reports whether threadID is being brought to rest.
func (f *threadRestFence) holds(threadID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.threads[threadID] > 0
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
		answer: turn.cause,
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
	fail("stop its work", a.stopRestingThreadWork(work))
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

// restRequestsPage bounds one read of the requests a rest stops.
const restRequestsPage = 200

// stopRequestsOfRestingThread stops every request the thread made: a
// reminder is dropped, a send or ask is cancelled with its wake disarmed and
// only the turn it started cut, and a spawn's thread is brought to rest by
// the same rule, on whichever computer it runs (stopAgentRequestBy). What it
// stops is what the ledger held once the session had ended, read whole before
// any of it is stopped, as a Stop All reads its rows; a request made after
// that read belongs to whoever started the thread again.
func (a *App) stopRequestsOfRestingThread(ctx context.Context, threadID string, by restStop, visited map[string]bool, result *ThreadAgentStop) {
	rows, err := a.threadRestRequests(threadID)
	if err != nil {
		result.Failures = append(result.Failures, "list its agents: "+err.Error())
		return
	}
	if hook := a.agentStopCapturedForTest; hook != nil {
		hook(threadID)
	}
	for _, row := range rows {
		if row.Kind == store.ThreadRequestRemind {
			if _, err := a.dropThreadReminder(row); err != nil {
				result.Failures = append(result.Failures, "drop its reminder: "+err.Error())
			}
			continue
		}
		stop := a.stopAgentRequestBy(ctx, row, visited, false, by)
		if stop.Outcome == threadAgentStopped {
			result.Nested++
		}
		result.Nested += stop.Nested
		result.Background += stop.Background
		result.Failures = append(result.Failures, stop.Failures...)
	}
}

// threadRestRequests reads every request a rest of threadID stops
// (ListThreadRestRequests), page by page.
func (a *App) threadRestRequests(threadID string) ([]store.ThreadRequest, error) {
	var out []store.ThreadRequest
	var after store.ThreadRestCursor
	for {
		rows, err := a.store.ListThreadRestRequests(threadID, after, restRequestsPage)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
		if len(rows) < restRequestsPage {
			return out, nil
		}
		after = after.After(rows[len(rows)-1])
	}
}

// stopAgentRequestBy stops one request and, for a spawn, brings the thread it
// made to rest by the same rule, wherever it runs. pressed is the row a
// person stopped, whose caller's wake stays armed so its agent learns of it;
// any other request has its wake disarmed first, because the thread that made
// it is being stopped too and a wake would start it again. A send or ask
// only has its request cancelled: the cancel stops the turn the request
// started there and nothing else of a thread that belongs to someone else.
func (a *App) stopAgentRequestBy(ctx context.Context, row store.ThreadRequest, visited map[string]bool, pressed bool, by restStop) ThreadAgentStop {
	result := ThreadAgentStop{Token: row.Token, Outcome: threadAgentStopped}
	current, found, err := a.store.GetThreadRequest(row.Token)
	if err != nil {
		result.Outcome = threadAgentFailed
		result.Failures = append(result.Failures, "read the request: "+err.Error())
		return result
	}
	if !found {
		result.Outcome = threadAgentEnded
		if !row.LineageOnly {
			return result
		}
		// Past the retention floor a spawn's token is unknown, and its row
		// still links the thread it made: the Stop reaches into that thread.
		current = row
	}
	settled := threadRequestSettled(current)
	if settled {
		result.Outcome = threadAgentEnded
	} else if !pressed && current.Notify {
		if _, err := a.store.SetThreadRequestNotify(current.Token, false); err != nil {
			result.Failures = append(result.Failures, "disarm its wake: "+err.Error())
		}
	}
	spawned := current.Kind == store.ThreadRequestSpawn && current.TargetThreadID != ""
	// A Stop of a spawn that already settled that finds its worker not at
	// rest, or whose rest stopped anything of it or below it
	// (restStoppedWork), reports it stopped, and ended otherwise. When a
	// person pressed it, the spawner is told too, since no settlement
	// tells it. A cascade tells nobody: the spawner it reaches is being
	// brought to rest itself. An agent's own thread_cancel does not come
	// through here.
	outlived := pressed && settled && spawned
	if current.TargetComputerID != "" {
		notice := a.workerStoppedNotice(current, outlived, &result)
		a.stopRemoteAgentRequestBy(ctx, current, settled, spawned, by, &result)
		if result.Outcome == threadAgentStopped {
			a.tellSpawnerOfUserStop(current, notice, &result)
		}
		return result
	}
	if !settled {
		effect, err := a.stopThreadRequestWorkBy(ctx, current.Token, by.answer, func(store.ThreadRequestReceipt) turnStop { return by.turn })
		if err != nil {
			result.Outcome = threadAgentFailed
			result.Failures = append(result.Failures, "cancel the request: "+err.Error())
		}
		result.Interrupted = effect == threadtools.EffectInterrupted
	}
	if !spawned || visited[current.TargetThreadID] {
		return result
	}
	visited[current.TargetThreadID] = true
	working := a.workerNotAtRest(current.TargetThreadID, &result)
	notice := a.workerStoppedNotice(current, outlived, &result)
	before := result
	a.restThreadHere(ctx, current.TargetThreadID, by, visited, &result)
	if settled && (working || restStoppedWork(before, result)) {
		result.Outcome = threadAgentStopped
		a.tellSpawnerOfUserStop(current, notice, &result)
	}
	return result
}

// workerStoppedNotice is the send id of the message a person's Stop of a
// worker that outlived its spawn tells the spawner with, empty for any other
// Stop. It is read before the rest, as the Stop finds the worker, so Stops
// racing on one stopping of it read the same send id (workerStoppedSendID).
// A read that fails is named in result and tells nothing.
func (a *App) workerStoppedNotice(spawn store.ThreadRequest, outlived bool, result *ThreadAgentStop) string {
	if !outlived {
		return ""
	}
	sendID, err := a.workerStoppedSendID(spawn)
	if err != nil {
		result.Failures = append(result.Failures, "tell its spawner: "+err.Error())
		return ""
	}
	if hook := a.workerStopObservedForTest; hook != nil {
		hook(spawn.Token)
	}
	return sendID
}

// workerNotAtRest reports whether threadID is not at rest as a Stop reaches
// it (workerUnrest). A read that fails is named in result.Failures and counts
// as not at rest, so a Stop that may have stopped work says so.
func (a *App) workerNotAtRest(threadID string, result *ThreadAgentStop) bool {
	rest, err := a.workerUnrest(threadID)
	if err != nil {
		result.Failures = append(result.Failures, "read what it was doing: "+err.Error())
		return true
	}
	return rest.unrest != ""
}

// restStoppedWork reports whether a rest added to after, over before, a cut
// turn, stopped background work, or an agent stopped below the thread: a
// thread with a worker below it that is not at rest is not at rest itself.
func restStoppedWork(before, after ThreadAgentStop) bool {
	return (after.Interrupted && !before.Interrupted) || after.Background > before.Background ||
		after.Nested > before.Nested
}

// workerStoppedSendIDRoot opens the send id of the message that tells a
// spawner the user stopped a worker that outlived its spawn,
// `worker-stopped:<token>:<n>`, n counting the Stops of the worker that
// stopped something (workerStoppedSendID).
const workerStoppedSendIDRoot = "worker-stopped:"

// workerStoppedSendID names a Stop's message to spawn's caller: the first n
// no message the caller holds or took in carries. The message of a Stop is
// recorded once its rest is over, the worker then at rest, so a Stop that
// stops the worker again, after it was continued, reads the next n, and
// Stops that found it working before the first message was recorded read
// the same n, which keeps them to one message.
func (a *App) workerStoppedSendID(spawn store.ThreadRequest) (string, error) {
	for n := 1; ; n++ {
		sendID := fmt.Sprintf("%s%s:%d", workerStoppedSendIDRoot, spawn.Token, n)
		_, found, err := a.findRecordedSend(spawn.CallerThreadID, sendID)
		if err != nil || !found {
			return sendID, err
		}
	}
}

// tellSpawnerOfUserStop tells the thread that made spawn that the user
// stopped the worker it made after the spawn had settled, as an app-written
// message with sendID (workerStoppedNotice) attributed to the worker, the
// way the spawn's answer was; an empty sendID tells nothing. The spawn and
// its answer stay as they are. An archived spawner is told nothing; any
// other failure is named in the result.
func (a *App) tellSpawnerOfUserStop(spawn store.ThreadRequest, sendID string, result *ThreadAgentStop) {
	if sendID == "" {
		return
	}
	title, computerID, computerName := spawn.TargetThreadTitle, "", ""
	if spawn.TargetComputerID != "" {
		computerName, computerID = a.threadTools().threadToolsBackendName(spawn.TargetComputerID)
	} else if thread, err := a.store.GetThread(spawn.TargetThreadID); err == nil {
		title = thread.Title
	}
	// The Stop's failures, named as its result names them: the worker is at
	// rest when there are none.
	note, err := usermessage.StoppedWorker(threadtools.WorkerStopped{
		Title: title, ThreadID: spawn.TargetThreadID, Token: spawn.Token, Failures: result.Failures,
	}, computerID, computerName)
	if err != nil {
		result.Failures = append(result.Failures, "tell its spawner: "+err.Error())
		return
	}
	err = a.queueAgentNotice(agentNotice{
		threadID: spawn.CallerThreadID,
		sendID:   sendID,
		prepare:  func() (usermessage.AgentMessage, bool, error) { return note, true, nil },
		startFailed: func(err error) string {
			return "A message that the user stopped a thread this one spawned is queued, but the agent could not start: " + err.Error()
		},
	})
	if err != nil && !errors.Is(err, errThreadArchived) {
		result.Failures = append(result.Failures, "tell its spawner: "+err.Error())
	}
}

// stopRemoteAgentRequestBy is stopAgentRequestBy for a target on a paired
// computer: the request through thread_cancel, and for a spawn the rest
// there. A person's Stop asks for it through agent_stop, which reports what
// it stopped; a computer that predates agent_stop has the spawned thread's
// turn interrupted instead, and the result names it in Failures. An agent's
// stop asks through thread_cancel by thread id, which a computer that knows
// the rest applies to a thread the caller spawned, and an older one answers
// with an interrupt.
func (a *App) stopRemoteAgentRequestBy(ctx context.Context, current store.ThreadRequest, settled, spawned bool, by restStop, result *ThreadAgentStop) {
	tools := a.threadTools()
	// The destination's receipts name the thread that made the request,
	// which New Thread, Same Assistant may since have replaced.
	caller := a.threadAgentCaller(current.MadeIn())
	if !settled {
		report, err := tools.cancelRemoteRequest(ctx, caller, current, by.user)
		if err != nil {
			result.Outcome = threadAgentFailed
			result.Failures = append(result.Failures, "cancel the request: "+err.Error())
		}
		result.Interrupted = report.Effect == threadtools.EffectInterrupted
	}
	if !spawned {
		return
	}
	stopTurn := func() {
		report, err := tools.cancelRemoteThread(ctx, caller, threadtools.CancelCall{
			ThreadID: current.TargetThreadID, ComputerID: current.TargetComputerID,
		}, by.user)
		if err != nil {
			result.Failures = append(result.Failures, "interrupt its turn: "+err.Error())
		} else if report.Effect == threadtools.EffectInterrupted || report.Effect == threadtools.EffectThreadStopped {
			result.Interrupted = true
		}
	}
	if !by.user {
		stopTurn()
		return
	}
	remote, err := a.peerThreadAgentStop(ctx, current.TargetComputerID, current.MadeIn(), current.TargetThreadID, "")
	if err != nil {
		if errors.Is(err, errThreadAgentPeerOlder) {
			stopTurn()
		}
		result.Failures = append(result.Failures, "stop its work: "+err.Error())
		return
	}
	// The destination's outcome says whether its rest stopped a thread not
	// at rest (stopThreadAgentWorkHere); for a request that settled it is
	// this Stop's.
	if settled && remote.Outcome == threadAgentStopped {
		result.Outcome = threadAgentStopped
	}
	result.Interrupted = result.Interrupted || remote.Interrupted
	result.Background += remote.Background
	result.Nested += remote.Nested
	result.Failures = append(result.Failures, remote.Failures...)
}
