package triage

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"agent-overflow/internal/store"
)

// Turn ends triage knows and the store does not hold.
//
// A turn's row is open (completed_at NULL) until the write of its end, and
// every durable reader takes an open newest row for a running turn
// (store/turn_newest.go). When that write fails the turn has still ended,
// so triage keeps the end it could not write here, as the turn's one
// owner until it is durable:
//
//   - the live-state snapshot reports the thread as having an unrecorded
//     end (LiveStateSnapshot.TurnEndUnrecorded), which the thread tools
//     read as error at rest, not as running (threadtools.State);
//   - the thread gets an error row for each distinct failure;
//   - a failed write is tried again before the thread's next turn opens,
//     before a late completion of the turn folds, when a send is placed on
//     the thread (RetryUnrecordedTurnEnds), and on a backoff timer while
//     anything is unrecorded, so the row settles once the database takes
//     writes again, without a restart;
//   - once it is written, the thread's row is announced and its queue is
//     flushed: a send placed while the row was open queued behind it.
//
// unrecordedEnds is the only writer of the set. Its mutex is a leaf lock,
// never held across a store write, an emit or r.mu; r.mu may be held
// while taking it (the snapshots read it). An entry being written carries
// a channel closed when the write returns, which is how a reopen of the
// turn waits out a write in flight instead of racing it.

// unrecordedEndKind is why a turn's end is not in the store.
type unrecordedEndKind uint8

const (
	// endWriteFailed: the write of the end failed; it is written again.
	endWriteFailed unrecordedEndKind = iota + 1
	// endWriteRefused: the store refused the write for a fork's sake
	// (store.IsShownHistoryRefusal). It cannot succeed and is not written
	// again; the next boot's sweep meets the same refusal.
	endWriteRefused
	// endCrashed: a turn the previous instance left open, which the boot
	// sweep failed to settle. It is settled by the sweep's own rule
	// (store.RecoverCrashedTurnsByID).
	endCrashed
)

const (
	firstEndRetryDelay = time.Second
	maxEndRetryDelay   = time.Minute
)

// turnEnd is what the write of a turn's end records
// (store.UpdateTurnCompleted).
type turnEnd struct {
	completedAt                                             int64
	stopReason, assistantMessageID, usageJSON, errorMessage string
}

// unrecordedEnd is one turn whose end the store does not hold.
type unrecordedEnd struct {
	kind      unrecordedEndKind
	threadID  string
	turnID    string
	turnIndex int
	// end is what the write records; unused for endCrashed, whose end the
	// sweep's rule decides.
	end turnEnd
	// reported is the failure text the thread's error row says, so a
	// retry that fails the same way reports nothing new.
	reported string
	// writing is non-nil while a write of the end runs, closed when it
	// returns.
	writing chan struct{}
}

// unrecordedEnds is the set and its retry timer. mu guards every field
// but wg.
type unrecordedEnds struct {
	mu     sync.Mutex
	byTurn map[string]*unrecordedEnd
	// timer is the scheduled pass, nil when none is. A pass is scheduled
	// only while an end is one to write again and the retries are not
	// stopped.
	timer   *time.Timer
	delay   time.Duration
	stopped bool
	// wg counts the scheduled pass and the one running, so
	// StopTurnEndRetries returns only once no pass can touch the store.
	wg sync.WaitGroup
	// passed is a test-only hook called after each timer pass, and
	// dropWaiting one called when a drop waits out a write in flight. Nil
	// in production.
	passed      func()
	dropWaiting func()
}

// any reports whether threadID has an end the store does not hold.
func (e *unrecordedEnds) any(threadID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, end := range e.byTurn {
		if end.threadID == threadID {
			return true
		}
	}
	return false
}

// armed reports whether a retry pass is scheduled.
func (e *unrecordedEnds) armed() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.timer != nil
}

// claim marks threadID's ends ("" for every thread) that can be written
// as being written and returns them, with the channels of those already
// being written. A refused end is never claimed.
func (e *unrecordedEnds) claim(threadID string) (claimed []*unrecordedEnd, inFlight []chan struct{}) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, end := range e.byTurn {
		if threadID != "" && end.threadID != threadID {
			continue
		}
		if end.writing != nil {
			inFlight = append(inFlight, end.writing)
			continue
		}
		switch end.kind {
		case endWriteRefused:
			continue
		case endWriteFailed, endCrashed:
		}
		end.writing = make(chan struct{})
		claimed = append(claimed, end)
	}
	return claimed, inFlight
}

// RetryUnrecordedTurnEnds writes threadID's unrecorded turn ends again and
// returns once every write of them that was running or started here has
// returned. A send placement calls it before it reads whether a turn runs.
func (r *Router) RetryUnrecordedTurnEnds(threadID string) {
	if r == nil || threadID == "" {
		return
	}
	r.retryUnrecordedEnds(threadID)
}

// UnrecordedTurnEnd reports whether the store does not hold the end of
// turnID, a turn that has ended, and when it ended: 0 for a crashed turn,
// whose end the sweep's rule decides. Its row reads open, and the app does
// not read it as a running turn (App.turnRuns).
func (r *Router) UnrecordedTurnEnd(turnID string) (endedAt int64, unrecorded bool) {
	if r == nil || turnID == "" {
		return 0, false
	}
	e := &r.ends
	e.mu.Lock()
	defer e.mu.Unlock()
	end := e.byTurn[turnID]
	if end == nil {
		return 0, false
	}
	return end.end.completedAt, true
}

// retryUnrecordedEnds writes threadID's ends ("" for every thread) again.
// A thread whose end it writes gets its row announced, and its queue
// flushed off this goroutine (flushAfterRecordedEnd): a caller may hold a
// send's locks.
func (r *Router) retryUnrecordedEnds(threadID string) {
	claimed, inFlight := r.ends.claim(threadID)
	settled := map[string]bool{}
	for _, end := range claimed {
		err := r.writeUnrecordedEnd(end)
		if r.finishUnrecordedEnd(end, err) {
			settled[end.threadID] = true
		}
	}
	for _, done := range inFlight {
		<-done
	}
	for settledThread := range settled {
		r.emitThreadRow(settledThread)
		r.goSettle(settledThread, func() { r.flushAfterRecordedEnd(settledThread) })
	}
}

// flushAfterRecordedEnd sends what queued behind a turn whose end was
// written late, unless a turn runs or is opening again (the retry before a
// turn's row), whose own end flushes the queue.
func (r *Router) flushAfterRecordedEnd(threadID string) {
	r.mu.Lock()
	st := r.threadStateIfPresent(threadID)
	busy := st != nil && (st.openTurnSet || st.currentRoundOpen || st.turnStarting != nil)
	r.mu.Unlock()
	if busy {
		return
	}
	r.maybeFlushQueueAtBoundary(threadID, "")
}

// writeUnrecordedEnd writes one end by its kind's rule.
func (r *Router) writeUnrecordedEnd(end *unrecordedEnd) error {
	switch end.kind {
	case endWriteFailed:
		r.mu.Lock()
		gate := r.turnSettleGate
		r.mu.Unlock()
		if gate != nil {
			gate(end.threadID)
		}
		return r.store.UpdateTurnCompleted(end.turnID, end.end.completedAt, end.end.stopReason,
			end.end.assistantMessageID, end.end.usageJSON, end.end.errorMessage)
	case endCrashed:
		_, err := r.store.RecoverCrashedTurnsByID([]string{end.turnID}, interruptedSummary, time.Now().UnixMilli())
		return err
	case endWriteRefused:
		return fmt.Errorf("triage: a refused turn end is not written again")
	}
	return fmt.Errorf("triage: unrecorded turn end of unknown kind %d", end.kind)
}

// finishUnrecordedEnd records how a write of end went and reports whether
// it is now in the store. A write the store finds no row for has nothing
// left open: the row is gone (the thread was deleted or reverted), or, for
// a crashed turn, it is no longer open.
func (r *Router) finishUnrecordedEnd(end *unrecordedEnd, err error) bool {
	e := &r.ends
	e.mu.Lock()
	done := end.writing
	end.writing = nil
	var report string
	recorded := err == nil || errors.Is(err, sql.ErrNoRows)
	if recorded {
		if e.byTurn[end.turnID] == end {
			delete(e.byTurn, end.turnID)
		}
	} else {
		if store.IsShownHistoryRefusal(err) {
			end.kind = endWriteRefused
		}
		if end.reported != err.Error() {
			end.reported = err.Error()
			report = end.reported
		}
	}
	e.mu.Unlock()
	close(done)
	if err != nil {
		log.Printf("triage: write the end of turn %s again: %v", end.turnID, err)
	}
	if report != "" {
		r.reportUnrecordedEnd(end.threadID, end.turnIndex, err)
	}
	return recorded && err == nil
}

// recordUnrecordedEnd keeps the end of turnID, whose write failed with
// err, and reports it on the thread. A refusal is kept and never written
// again; any other failure arms the retry.
func (r *Router) recordUnrecordedEnd(threadID string, turnIndex int, turnID string, end turnEnd, err error) {
	kind := endWriteFailed
	if store.IsShownHistoryRefusal(err) {
		kind = endWriteRefused
	}
	r.keepUnrecordedEnd(&unrecordedEnd{
		kind: kind, threadID: threadID, turnID: turnID, turnIndex: turnIndex, end: end, reported: err.Error(),
	})
	r.reportUnrecordedEnd(threadID, turnIndex, err)
}

// keepUnrecordedEnd adds end to the set, replacing an earlier end of the
// same turn, and arms the retry when it is one to write again.
func (r *Router) keepUnrecordedEnd(end *unrecordedEnd) {
	e := &r.ends
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.byTurn == nil {
		e.byTurn = make(map[string]*unrecordedEnd)
	}
	// A write of a prior end of the turn in flight finishes that entry
	// only (finishUnrecordedEnd); this one is the turn's newest end.
	e.byTurn[end.turnID] = end
	switch end.kind {
	case endWriteFailed, endCrashed:
		r.armEndRetryLocked()
	case endWriteRefused:
	}
}

// armEndRetryLocked schedules the next pass unless one is scheduled, the
// router has stopped them, or nothing is left to write. Caller holds
// r.ends.mu.
func (r *Router) armEndRetryLocked() {
	e := &r.ends
	if e.stopped || e.timer != nil || !e.writableLocked() {
		return
	}
	if e.delay == 0 {
		e.delay = r.delays.endRetry
	}
	e.wg.Add(1)
	e.timer = time.AfterFunc(e.delay, func() {
		defer e.wg.Done()
		r.endRetryPass()
	})
}

// writableLocked reports whether any end is one to write again.
func (e *unrecordedEnds) writableLocked() bool {
	for _, end := range e.byTurn {
		switch end.kind {
		case endWriteFailed, endCrashed:
			return true
		case endWriteRefused:
		}
	}
	return false
}

// endRetryPass is one timer pass: every end written again, then the next
// pass scheduled at twice the wait while anything is left to write.
func (r *Router) endRetryPass() {
	e := &r.ends
	e.mu.Lock()
	e.timer = nil
	stopped := e.stopped
	e.mu.Unlock()
	if !stopped {
		r.retryUnrecordedEnds("")
	}
	e.mu.Lock()
	if e.writableLocked() {
		e.delay = min(2*e.delay, maxEndRetryDelay)
		r.armEndRetryLocked()
	} else {
		e.delay = 0
	}
	passed := e.passed
	e.mu.Unlock()
	if passed != nil {
		passed()
	}
}

// StopTurnEndRetries cancels the scheduled pass, schedules no more, and
// waits out a pass already running, for a caller about to close the
// store. An end still unrecorded stays so in memory; the next boot's sweep
// settles its row as interrupted.
func (r *Router) StopTurnEndRetries() {
	if r == nil {
		return
	}
	e := &r.ends
	e.mu.Lock()
	e.stopped = true
	if e.timer != nil && e.timer.Stop() {
		e.wg.Done()
	}
	e.timer = nil
	e.mu.Unlock()
	e.wg.Wait()
}

// dropUnrecordedEnd forgets the unrecorded end of turnID because the turn
// runs again (a soft-closed turn the model continued). A write of it in
// flight is waited out first, so the reopen that follows lands after it
// and the turn is not settled under the continuation.
func (r *Router) dropUnrecordedEnd(turnID string) {
	e := &r.ends
	for {
		e.mu.Lock()
		end := e.byTurn[turnID]
		if end == nil {
			e.mu.Unlock()
			return
		}
		if done := end.writing; done != nil {
			waiting := e.dropWaiting
			e.mu.Unlock()
			if waiting != nil {
				waiting()
			}
			<-done
			continue
		}
		delete(e.byTurn, turnID)
		e.mu.Unlock()
		return
	}
}

// TurnEndFailureSummary is the thread error row of a turn whose end could
// not be recorded.
func TurnEndFailureSummary(err error) string {
	return "This turn's end could not be recorded: " + err.Error()
}

// reportUnrecordedEnd gives the thread an error row at the turn: the
// refusal's own row for a refusal. When the store refuses that row too, the
// funnel shows it live as unsaved and its error notice still reaches every
// client's sidebar (persistItemWithEmit).
func (r *Router) reportUnrecordedEnd(threadID string, turnIndex int, err error) {
	summary := TurnEndFailureSummary(err)
	if store.IsShownHistoryRefusal(err) {
		summary = ShownHistoryRefusalSummary(err)
	}
	if reportErr := r.persistProviderErrorItem(threadID, turnIndex, summary, nil, "", "", time.Now().UnixMilli()); reportErr != nil {
		log.Printf("triage: report the unrecorded end of %s/%d: %v", threadID, turnIndex, reportErr)
	}
}

// writeTurnEnd writes the end of turnID and, when the write fails, keeps
// it as unrecorded (recordUnrecordedEnd). A turn with no row is left open
// nowhere, so only the log keeps that. The three settle paths write a
// turn's end here. Returns the write's error.
func (r *Router) writeTurnEnd(threadID string, turnIndex int, turnID string, end turnEnd) error {
	err := r.store.UpdateTurnCompleted(turnID, end.completedAt, end.stopReason, end.assistantMessageID, end.usageJSON, end.errorMessage)
	if err == nil {
		return nil
	}
	log.Printf("triage: settle turn %s: %v", turnID, err)
	if !errors.Is(err, sql.ErrNoRows) {
		r.recordUnrecordedEnd(threadID, turnIndex, turnID, end, err)
	}
	return err
}

// oweCrashedTurns keeps the turns a failed boot sweep left open as
// unrecorded ends of the previous instance.
func (r *Router) oweCrashedTurns(turns []store.CrashedTurnRow) {
	for _, turn := range turns {
		r.keepUnrecordedEnd(&unrecordedEnd{
			kind: endCrashed, threadID: turn.ThreadID, turnID: turn.TurnID, turnIndex: turn.TurnIndex,
		})
	}
}
