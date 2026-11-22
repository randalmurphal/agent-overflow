package triage

import (
	"database/sql"
	"errors"
	"log"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
)

// Turn ends the store did not take.
//
// A turn's row is open (completed_at NULL) until the write of its end, and
// every durable reader takes an open newest row for a running turn
// (store/turn_newest.go). When that write fails, or the boot sweep fails to
// settle the rows a previous instance left open, the turn has still ended.
// Triage keeps that fact, by thread and turn, as its one owner. Nothing
// writes the end again: the row stays open until a later boot's sweep
// records it as interrupted.
//
//   - Both live-state snapshots carry it (TurnEndUnrecorded) and
//     provider:turn_end_unrecorded announces each change of it, so the
//     thread tools, a pane, every connected sidebar and a client that
//     connects later read the thread as error at rest, not as running.
//     None of them depends on the thread's error row being saved.
//   - The app does not read such a turn's open row as a running turn
//     (UnrecordedTurnEnd, App.turnRuns).
//   - A thread's facts are forgotten when it starts a turn, whose row is
//     then its newest or which runs on the open row it adopted; a turn's,
//     when a soft-closed turn is continued; and a thread's, when the
//     thread is deleted (ForgetUnrecordedTurnEnds).
//
// The set is router state under r.mu. endsPublish is held across each
// change and its announcement, so the frames go out in the order the set
// changed; it is taken before r.mu, never under it.

// unrecordedEnd is one turn whose end the store does not hold.
type unrecordedEnd struct {
	turnIndex int
	// endedAt is when the provider ended the turn, 0 for a turn a failed
	// boot sweep left open.
	endedAt int64
}

// TurnEndUnrecordedEvent is the provider:turn_end_unrecorded payload:
// whether the thread has a turn whose end the store does not hold.
type TurnEndUnrecordedEvent struct {
	ThreadID   string `json:"threadId"`
	Unrecorded bool   `json:"unrecorded"`
}

// turnEnd is what the write of a turn's end records
// (store.UpdateTurnCompleted).
type turnEnd struct {
	completedAt                                             int64
	stopReason, assistantMessageID, usageJSON, errorMessage string
}

// writeTurnEnd writes the end of turnID. When the write fails the turn has
// still ended: the fact is kept (noteUnrecordedEnd) and the thread gets an
// error row that says why, if the store takes that. A turn with no row is
// left open nowhere, so only the log keeps that. The three settle paths
// write a turn's end here. Returns the write's error.
func (r *Router) writeTurnEnd(threadID string, turnIndex int, turnID string, end turnEnd) error {
	err := r.store.UpdateTurnCompleted(turnID, end.completedAt, end.stopReason, end.assistantMessageID, end.usageJSON, end.errorMessage)
	if err == nil {
		return nil
	}
	log.Printf("triage: settle turn %s: %v", turnID, err)
	if errors.Is(err, sql.ErrNoRows) {
		return err
	}
	r.noteUnrecordedEnd(threadID, map[string]unrecordedEnd{turnID: {turnIndex: turnIndex, endedAt: end.completedAt}})
	r.reportUnrecordedEnd(threadID, turnIndex, err)
	return err
}

// noteCrashedTurns keeps the turns a failed boot sweep left open as ended:
// the previous instance ran them, and no provider continues them.
func (r *Router) noteCrashedTurns(turns []store.CrashedTurnRow) {
	byThread := map[string]map[string]unrecordedEnd{}
	for _, turn := range turns {
		if byThread[turn.ThreadID] == nil {
			byThread[turn.ThreadID] = map[string]unrecordedEnd{}
		}
		byThread[turn.ThreadID][turn.TurnID] = unrecordedEnd{turnIndex: turn.TurnIndex}
	}
	for threadID, ends := range byThread {
		r.noteUnrecordedEnd(threadID, ends)
	}
}

// noteUnrecordedEnd adds ends, by turn id, to threadID's facts and
// announces the thread's change.
func (r *Router) noteUnrecordedEnd(threadID string, ends map[string]unrecordedEnd) {
	r.endsPublish.Lock()
	defer r.endsPublish.Unlock()
	r.mu.Lock()
	had := len(r.unrecordedEnds[threadID]) > 0
	if r.unrecordedEnds == nil {
		r.unrecordedEnds = map[string]map[string]unrecordedEnd{}
	}
	if r.unrecordedEnds[threadID] == nil {
		r.unrecordedEnds[threadID] = map[string]unrecordedEnd{}
	}
	for turnID, end := range ends {
		r.unrecordedEnds[threadID][turnID] = end
	}
	r.mu.Unlock()
	if !had {
		r.emit(eventchan.ProviderTurnEndUnrecorded, TurnEndUnrecordedEvent{ThreadID: threadID, Unrecorded: true})
	}
}

// forgetUnrecordedEnds drops threadID's fact of turnID, or every fact of the
// thread when turnID is empty, and announces the thread's change.
func (r *Router) forgetUnrecordedEnds(threadID, turnID string) {
	r.endsPublish.Lock()
	defer r.endsPublish.Unlock()
	r.mu.Lock()
	ends := r.unrecordedEnds[threadID]
	had := len(ends) > 0
	if turnID == "" {
		delete(r.unrecordedEnds, threadID)
	} else if had {
		delete(ends, turnID)
		if len(ends) == 0 {
			delete(r.unrecordedEnds, threadID)
		}
	}
	gone := had && len(r.unrecordedEnds[threadID]) == 0
	r.mu.Unlock()
	if gone {
		r.emit(eventchan.ProviderTurnEndUnrecorded, TurnEndUnrecordedEvent{ThreadID: threadID, Unrecorded: false})
	}
}

// ForgetUnrecordedTurnEnds drops every fact of a thread that was deleted:
// its rows are gone.
func (r *Router) ForgetUnrecordedTurnEnds(threadID string) {
	if r == nil || threadID == "" {
		return
	}
	r.forgetUnrecordedEnds(threadID, "")
}

// UnrecordedTurnEnd reports whether turnID of threadID has ended with the
// store not holding its end, and when it ended: 0 for a turn a failed boot
// sweep left open. Its row reads open, and the app does not read it as a
// running turn (App.turnRuns).
func (r *Router) UnrecordedTurnEnd(threadID, turnID string) (endedAt int64, unrecorded bool) {
	if r == nil || threadID == "" || turnID == "" {
		return 0, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	end, unrecorded := r.unrecordedEnds[threadID][turnID]
	return end.endedAt, unrecorded
}

// TurnEndFailureSummary is the thread error row of a turn whose end could
// not be recorded.
func TurnEndFailureSummary(err error) string {
	return "This turn's end could not be recorded: " + err.Error()
}

// reportUnrecordedEnd gives the thread an error row at the turn, the
// refusal's own row for a refusal, which explains the failure in a pane.
// The thread's status does not depend on it: the fact is the live state's.
func (r *Router) reportUnrecordedEnd(threadID string, turnIndex int, err error) {
	summary := TurnEndFailureSummary(err)
	if store.IsShownHistoryRefusal(err) {
		summary = ShownHistoryRefusalSummary(err)
	}
	if reportErr := r.persistProviderErrorItem(threadID, turnIndex, summary, nil, "", "", time.Now().UnixMilli()); reportErr != nil {
		log.Printf("triage: report the unrecorded end of %s/%d: %v", threadID, turnIndex, reportErr)
	}
}
