package app

import (
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// A notice is a message the app writes into a thread on something else's
// behalf: a remote job's completion, a thread request's answer, a loop or
// watch wake. They all arrive the same way, and the way is the part that
// must not drift between them: the ordinary durable queue with the caller's
// own persist hook, so the queue row and the caller's record commit
// together, and a lazy session start for a thread with nobody to read it.
//
// Acceptance is the queue write alone. It takes the queue's own locks, never
// the thread's action lock, so a notice is not held up by a send, a revert or
// a session start already running on the thread. The start runs after, on
// its own goroutine; a start that fails is an error row in the thread and a
// notification, and the message stays queued for the next start. A loop or
// watch whose wake is held by such a failure tries the start again on each
// later fire (retryAgentWakeStart).
//
// An archived thread takes no notice and starts no session for one. A
// notice that reaches the queue after an archive is refused, and its caller
// keeps it (errThreadArchived); one queued before the archive waits without
// a start for the archive to take it back (stopArchivedThreadWork). A
// person's own answers are the exception: a person can write to an archived
// thread they opened, and their answers are theirs the same way.
//
// What each notice decides for itself stays its own: who may receive it,
// what it says, whether it is still wanted when its session is about to
// start, and what to do when the session will not start.

// errThreadArchived is the refusal of a notice for an archived thread. Its
// caller still has whatever the notice was for: the persist hook did not
// run.
var errThreadArchived = errors.New("the thread is archived")

// agentNotice is one such message.
type agentNotice struct {
	threadID string
	// sendID is the notice's identity for send admission, so a repeated
	// observation of the same event cannot queue it twice.
	sendID string
	// origin, originThread and originWake attribute the message in the
	// timeline. Every notice that is not the person's own words (fromPerson)
	// carries one, so it reads as an agent's message and not as theirs.
	origin       string
	originThread *usermessage.OriginThread
	originWake   *usermessage.OriginWake
	// prepare returns the message body. A false result abandons the notice
	// without an error: the reason for it went away.
	prepare func() (string, bool, error)
	// persist writes the caller's durable record in the queue insert's own
	// transaction.
	persist func(store.FlushQueueItem) error
	// keep is asked under the thread's action lock, just before a session is
	// started for the notice: false takes it back out of the queue, because
	// what it was for ended while it waited. An archive is not keep's: the
	// start checks it first, for every notice. A cancel of a loop or watch
	// does not take the lock: it withdraws its wake, and one the start has
	// already handed to the session is delivered, so nothing is lost either
	// way. An error is a failed read of what keep checks, and names that
	// read. Nil keeps every notice.
	keep func() (bool, error)
	// withdrawn runs after keep took the notice back out of the queue.
	withdrawn func()
	// startFailed renders what the thread is told when its session could
	// not be started. The message is already queued by then.
	startFailed func(error) string
	// fromPerson marks a person's own words, which an archived thread takes.
	fromPerson bool
}

// queueAgentNotice queues one notice and makes sure something will run to
// read it. It returns once the notice is in the durable queue; a session
// start it needs runs afterwards (startAgentNoticeSession). An archived
// thread refuses it with errThreadArchived, which each caller treats as its
// own reason not to proceed.
func (a *App) queueAgentNotice(notice agentNotice) error {
	if notice.prepare == nil || notice.startFailed == nil {
		return fmt.Errorf("agent notice %s for %s names no body or no start failure message", notice.sendID, notice.threadID)
	}
	body, proceed, err := notice.prepare()
	if err != nil || !proceed {
		return err
	}
	if _, err := a.registerQueueItem(notice.threadID, body,
		SendMessageOptions{SendID: notice.sendID},
		injectedQueueOptions{
			preserveDraft: true,
			origin:        notice.origin,
			originThread:  notice.originThread,
			originWake:    notice.originWake,
			persist:       notice.persist,
			// The flag, not the check, is the notice's: the queue write
			// decides it under the lock both archive doors take.
			heldWhileArchived: !notice.fromPerson,
		}); err != nil {
		return err
	}
	if _, live := a.sessionManager().get(notice.threadID); live {
		return nil
	}
	// Refused only once shutdown began: the durable row is the next boot's.
	a.goAgentNoticeWork(notice.threadID, func() { a.startAgentNoticeSession(notice, false) })
	return nil
}

// goAgentNoticeWork runs fn for threadID on its own goroutine, joined at
// shutdown before sessions close. False when shutdown has begun and fn did
// not run. A panic in fn is logged with its stack and put on the thread as an
// error row: the work runs with nobody watching, and the panic must not take
// the process down.
func (a *App) goAgentNoticeWork(threadID string, fn func()) bool {
	a.agentNoticeWork.mu.Lock()
	defer a.agentNoticeWork.mu.Unlock()
	if a.agentNoticeWork.stopped {
		return false
	}
	a.agentNoticeWork.wg.Add(1)
	go func() {
		defer a.agentNoticeWork.wg.Done()
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf("agent notice: work for %s panicked: %v\n%s", threadID, recovered, debug.Stack())
				a.emitErrorToThread(threadID, fmt.Sprintf(
					"The app hit an internal error while handling a queued message for this thread (%v). The message may still be queued; send anything to start the agent.", recovered))
			}
		}()
		fn()
	}()
	return true
}

// startAgentNoticeSession starts the session a queued notice needs, and
// reports whether the thread has one afterwards. It takes the thread's
// action lock the way StartSession does, and re-checks under it: a session
// someone else started meanwhile already has the message, because startup's
// flush trigger dispatches the queue, and starting again would restart that
// session.
//
// Both archive doors flip the row under the same lock, so a thread archived
// after the notice was queued is seen here and is not started. The notice
// stays queued for the archive to take back: a remote job's completion goes
// to its watch, a thread request's answer or reminder to its request, and
// every other app message is withdrawn (stopArchivedThreadWork).
//
// A retry is a later fire trying the start again for a wake an earlier
// failed start left queued. It runs only once the thread's last failed
// start is old enough (agentNoticeRetryDue), and its own failure writes no
// row: the failure that began the streak said why, and the message it would
// speak for is the one already said to be queued.
func (a *App) startAgentNoticeSession(notice agentNotice, retry bool) bool {
	unlock, err := a.threadLocks().LockCtx(a.lifeCtx(), notice.threadID)
	if err != nil {
		// Shutting down: the durable row is the next boot's.
		return false
	}
	if _, live := a.sessionManager().get(notice.threadID); live {
		unlock()
		return true
	}
	if retry && !a.agentNoticeRetryDue(notice.threadID) {
		unlock()
		return false
	}
	held, kept := false, true
	var checkErr, startErr error
	func() {
		// A panic in the check or the start is this start's failure: the
		// lock is released below and the thread is told, as for any other.
		// Callers run on fire and notice goroutines nobody else recovers.
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf("agent notice: start of %s panicked: %v\n%s", notice.threadID, recovered, debug.Stack())
				startErr = fmt.Errorf("the start hit an internal error: %v", recovered)
			}
		}()
		if !notice.fromPerson {
			held, checkErr = a.threadArchivedNow(notice.threadID)
		}
		if checkErr == nil && !held && notice.keep != nil {
			kept, checkErr = notice.keep()
		}
		if checkErr == nil && !held && kept {
			startErr = a.startSession(a.lifeCtx(), notice.threadID)
		}
	}()
	failed := checkErr != nil || startErr != nil
	stopping := failed && (errors.Is(startErr, ErrShuttingDown) || a.lifeCtx().Err() != nil)
	first := false
	if failed && !stopping {
		// Nobody watched this start, so the row alone goes unread.
		first = a.noteAgentNoticeStartFailed(notice.threadID)
	}
	unlock()
	// The rows are not wire events: the thread is usually stopped, and the
	// gate for a torn-down session's frames would drop the one row that
	// says why nothing answered.
	switch {
	case stopping:
		// The durable row is the next boot's.
	case failed && retry && !first:
		log.Printf("agent notice: %s still cannot start: %v", notice.threadID, errors.Join(checkErr, startErr))
	case checkErr != nil:
		a.emitErrorToThread(notice.threadID, fmt.Sprintf(
			"A message is queued for this thread, but the agent was not started for it: the app could not check that the message is still wanted (%v).", checkErr))
	case startErr != nil:
		a.emitErrorToThread(notice.threadID, notice.startFailed(startErr))
	case held:
		// The archive takes it back.
	case !kept:
		a.withdrawAgentNotice(notice)
	}
	return !failed && !held && kept
}

// threadArchivedNow reads whether a thread is archived. An error names the
// read, as keep's do.
func (a *App) threadArchivedNow(threadID string) (bool, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return false, fmt.Errorf("read the thread: %w", err)
	}
	return thread.Archived, nil
}

// noteAgentNoticeStartFailed records a failed notice start on its thread
// and reports whether it began the thread's failure streak. The first
// failure of a streak raises the notification. It is queued under mu, where
// noteSessionStarted queues the withdrawal, so the two reach the queue in
// the order the streak began and ended.
//
// The streak is marked in the store before the notification is raised, so
// the notification is never out without the mark that withdraws it after a
// restart (loadSessionStartFailures).
func (a *App) noteAgentNoticeStartFailed(threadID string) bool {
	w := &a.agentNoticeWork
	w.mu.Lock()
	defer w.mu.Unlock()
	_, streak := w.failing[threadID]
	if w.failing == nil {
		w.failing = make(map[string]time.Time)
	}
	now := time.Now()
	w.failing[threadID] = now
	if !streak {
		if err := a.store.MarkSessionStartFailed(threadID, now.UnixMilli()); err != nil {
			log.Printf("agent notice: the start-failure notification of %s will not be withdrawn after a restart: %v", threadID, err)
		}
		a.notifySessionStartFailed(threadID)
	}
	return !streak
}

// noteSessionStarted ends a thread's failure streak, withdrawing its
// notification. Every successful start calls it, whoever made the start.
// The mark goes after the withdrawal is queued; one left by a failed clear
// withdraws the notification again after a restart, which changes nothing.
func (a *App) noteSessionStarted(threadID string) {
	w := &a.agentNoticeWork
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, streak := w.failing[threadID]; !streak {
		return
	}
	delete(w.failing, threadID)
	a.withdrawSessionStartFailed(threadID)
	if _, err := a.store.ClearSessionStartFailed(threadID); err != nil {
		log.Printf("agent notice: %v", err)
	}
}

// loadSessionStartFailures resumes the failure streaks a previous process
// left marked, so the first start that succeeds withdraws the notification
// each one raised. Boot runs it before any session can start.
func (a *App) loadSessionStartFailures() error {
	marks, err := a.store.SessionStartFailures()
	if err != nil {
		return err
	}
	w := &a.agentNoticeWork
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failing == nil {
		w.failing = make(map[string]time.Time, len(marks))
	}
	for threadID, at := range marks {
		w.failing[threadID] = time.UnixMilli(at)
	}
	return nil
}

// agentNoticeRetryDue reports whether a thread's last notice start failed
// and may be tried again: half the wake floor after that try. Interval
// fires are a floor apart, so each of them tries; a long-running watch's
// held batch, which tries every agentWakeBatchWindow, and several loops on
// one thread, do not start it any more often. A start that has not failed
// is not retried: it is still running, and its wake waits for it.
func (a *App) agentNoticeRetryDue(threadID string) bool {
	w := &a.agentNoticeWork
	w.mu.Lock()
	defer w.mu.Unlock()
	last, streak := w.failing[threadID]
	return streak && time.Since(last) >= a.agentWakeFloor()/2
}

// withdrawAgentNotice takes a notice keep refused back out of the queue. It
// runs with the action lock released, because removal takes send admission
// first. A notice a start dispatched meanwhile was delivered after all, and
// one already gone was answered by whatever removed it.
func (a *App) withdrawAgentNotice(notice agentNotice) {
	removal, err := a.removeQueuedItem(a.lifeCtx(), notice.threadID, notice.sendID)
	if err != nil {
		log.Printf("agent notice: withdraw %s from %s: %v", notice.sendID, notice.threadID, err)
		return
	}
	if removal == queueRemovalRemoved && notice.withdrawn != nil {
		notice.withdrawn()
	}
}

// waitAgentNoticeWork blocks until the notice work already begun has
// finished, without refusing more. It is the observation point for tests.
func (a *App) waitAgentNoticeWork() {
	a.agentNoticeWork.wg.Wait()
}

// stopAgentNoticeWork refuses new notice work and waits out what is in
// flight. Shutdown calls it before it closes sessions, so a start that
// finishes cannot register a session the close has already passed.
func (a *App) stopAgentNoticeWork() {
	a.agentNoticeWork.mu.Lock()
	a.agentNoticeWork.stopped = true
	a.agentNoticeWork.mu.Unlock()
	a.agentNoticeWork.wg.Wait()
}
