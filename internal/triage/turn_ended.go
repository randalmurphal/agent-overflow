package triage

import (
	"database/sql"
	"errors"
	"log"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
)

// Turns that ended with their end unrecorded.
//
// A turn's row is open (completed_at NULL) until the write of its end
// (store/turn_newest.go). The row stays open with nothing in this process
// running its turn when that write fails, when a failed boot sweep leaves a
// previous instance's row open, and when a start is refused after it wrote
// its row. Nothing writes the end again: a later boot's sweep records it.
//
// TurnEnded is the one owner of whether such a row's turn has ended: the
// thread's newest row is open and the thread has no live mark (liveLocked):
// no turn start in progress, no turn open under way, no open turn, no open
// round and no round being ended. Every turn open marks the thread before
// it writes or reopens a row, and every settle writes the row before it
// drops the marks, so a thread with no mark has no turn running its open
// row. Readers that decide running or rest take such a turn as the next
// boot's sweep will record it: interrupted, ended at its last activity,
// unread by the rule of a recorded interruption (store.NewestOpenTurn).
//
// Every live mark is dropped through releaseLiveLocked. When the last one
// drops, announceEnded tells the app (SetTurnEndedObserver) if the turn
// ended, and every client (provider:turn_end_unrecorded) if the thread was
// also not read since the turn's end: a client reads that as an
// interruption the user has not read, until the thread is read or starts
// a turn. A turn start's provider:turn_started (emitTurnStarted) is
// ordered after it by the thread's endedLock.

// turnEndedReads bounds the reads of TurnEnded: a thread whose turns keep
// opening while it reads is live.
const turnEndedReads = 3

// TurnEndUnrecordedEvent is the provider:turn_end_unrecorded payload: the
// thread's newest turn ended with its end unrecorded, and the thread was
// not read since it ended.
type TurnEndUnrecordedEvent struct {
	ThreadID string `json:"threadId"`
}

// SetTurnEndedObserver wires the callback told that threadID's last live
// mark dropped with its newest turn ended unrecorded (TurnEnded), read or
// not. It runs with no router lock held.
func (r *Router) SetTurnEndedObserver(fn func(threadID string)) {
	r.mu.Lock()
	r.turnEnded = fn
	r.mu.Unlock()
}

// liveLocked reports whether st holds a live mark: a turn start in
// progress, a turn open under way, an open turn, an open round or a round
// being ended. Caller holds r.mu.
func (st *threadState) liveLocked() bool {
	return st.turnStarting != nil || st.turnOpenings > 0 || st.openTurnSet || st.currentRoundOpen || st.endingRoundSet
}

// threadLiveLocked is liveLocked for threadID; a thread with no state has
// no mark. Caller holds r.mu.
func (r *Router) threadLiveLocked(threadID string) bool {
	st := r.threadStateIfPresent(threadID)
	return st != nil && st.liveLocked()
}

// releaseLiveLocked runs drop, which clears live marks of threadID, and
// reports whether it cleared the thread's last one. The caller announces
// that (announceEnded) once r.mu is released. Every clear of a live mark,
// and the delete of a thread's state, runs as such a drop
// (TestEveryLiveMarkDropsThroughRelease). Caller holds r.mu.
func (r *Router) releaseLiveLocked(threadID string, drop func()) bool {
	was := r.threadLiveLocked(threadID)
	drop()
	return was && !r.threadLiveLocked(threadID)
}

// TurnEnded reports whether threadID's newest turn row is open with its
// turn ended (the file comment), and the row as it reads then. It reads
// the thread's marks, then the row: a mark means a turn may run the row.
// Every mark is set under a turn opening (markTurnOpeningLocked), which
// counts itself (openingsMarked) in the same lock hold, so an unchanged
// count after the row read means no mark appeared while it ran; a changed
// one reads again. When turns keep opening across turnEndedReads reads,
// the thread is live. No store read runs under r.mu. A router with no
// store, as some tests build, has no rows, so no turn of it ended
// unrecorded.
func (r *Router) TurnEnded(threadID string) (store.EndedTurn, bool, error) {
	if r == nil || r.store == nil || threadID == "" {
		return store.EndedTurn{}, false, nil
	}
	identity := r.identity(threadID)
	marks := func() (bool, uint64, func(string)) {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.threadLiveLocked(threadID), identity.openingsMarked, r.turnEndedReadGate
	}
	for range turnEndedReads {
		live, opened, gate := marks()
		if live {
			return store.EndedTurn{}, false, nil
		}
		if gate != nil {
			gate(threadID)
		}
		turn, open, err := r.store.NewestOpenTurn(threadID, time.Now().UnixMilli())
		if err != nil {
			return store.EndedTurn{}, false, err
		}
		if _, openedSince, _ := marks(); openedSince != opened {
			continue
		}
		return turn, open, nil
	}
	return store.EndedTurn{}, false, nil
}

// announceEnded tells the app and every client that threadID's turn ended
// unrecorded (the file comment), once its last live mark dropped. A
// failed read is logged: the thread then reads as its row does until the
// next reader asks.
func (r *Router) announceEnded(threadID string) {
	lock := &r.identity(threadID).endedLock
	lock.Lock()
	turn, ended, err := r.TurnEnded(threadID)
	if ended && turn.Unread {
		r.emit(eventchan.ProviderTurnEndUnrecorded, TurnEndUnrecordedEvent{ThreadID: threadID})
	}
	lock.Unlock()
	if err != nil {
		log.Printf("triage: read whether the turn of %s ended: %v", threadID, err)
		return
	}
	if !ended {
		return
	}
	r.mu.Lock()
	observer := r.turnEnded
	r.mu.Unlock()
	if observer != nil {
		observer(threadID)
	}
}

// emitTurnStarted announces a round's start, after any end of the
// thread's turn already decided (announceEnded).
func (r *Router) emitTurnStarted(snapshot ActiveTurnSnapshot) {
	lock := &r.identity(snapshot.ThreadID).endedLock
	lock.Lock()
	defer lock.Unlock()
	r.emit(eventchan.ProviderTurnStarted, TurnStartedEvent(snapshot))
}

// turnEnd is what the write of a turn's end records
// (store.UpdateTurnCompleted).
type turnEnd struct {
	completedAt                                             int64
	stopReason, assistantMessageID, usageJSON, errorMessage string
}

// writeTurnEnd writes the end of turnID. When the write fails the turn has
// still ended: its row stays open, which TurnEnded reads as ended once the
// thread's marks drop, and the thread gets an error row that says why, if
// the store takes that. A turn with no row is left open nowhere, so only
// the log keeps that. The three settle paths write a turn's end here.
// Returns the write's error.
func (r *Router) writeTurnEnd(threadID string, turnIndex int, turnID string, end turnEnd) error {
	err := r.store.UpdateTurnCompleted(turnID, end.completedAt, end.stopReason, end.assistantMessageID, end.usageJSON, end.errorMessage)
	if err == nil {
		return nil
	}
	log.Printf("triage: settle turn %s: %v", turnID, err)
	if errors.Is(err, sql.ErrNoRows) {
		return err
	}
	r.reportUnrecordedEnd(threadID, turnIndex, err)
	return err
}

// TurnEndFailureSummary is the thread error row of a turn whose end could
// not be recorded.
func TurnEndFailureSummary(err error) string {
	return "This turn's end could not be recorded: " + err.Error()
}

// reportUnrecordedEnd gives the thread an error row at the turn, the
// refusal's own row for a refusal, which explains the failure in a pane.
// Saved, it makes the thread read Failed by the failed-turn rule, before
// and after a restart.
func (r *Router) reportUnrecordedEnd(threadID string, turnIndex int, err error) {
	summary := TurnEndFailureSummary(err)
	if store.IsShownHistoryRefusal(err) {
		summary = ShownHistoryRefusalSummary(err)
	}
	if reportErr := r.persistProviderErrorItem(threadID, turnIndex, summary, nil, "", "", time.Now().UnixMilli()); reportErr != nil {
		log.Printf("triage: report the unrecorded end of %s/%d: %v", threadID, turnIndex, reportErr)
	}
}
