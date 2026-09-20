package app

import (
	"context"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// thread_remind: the one request with no destination. The clock settles it,
// which is why it needs no receipt, no target thread and no dispatch.
//
// It is a request rather than a timer because everything a request already
// does is what a reminder needs: it survives a restart as a row, it lists
// beside the caller's other open work, thread_cancel drops it, and the same
// collector delivers it.

// Remind arms a clock-settled request that wakes the caller later.
func (t threadToolsApp) Remind(_ context.Context, caller threadtools.Caller, call threadtools.RemindCall) (threadtools.RequestAck, error) {
	if _, err := t.localThread(caller.ThreadID); err != nil {
		return threadtools.RequestAck{}, err
	}
	if call.DueAtUnixMs <= 0 {
		return threadtools.RequestAck{}, errorsx.Public(threadtools.CodeInvalidRequest,
			"A reminder needs a time in the future.", nil)
	}
	token := newThreadRequestToken()
	if err := t.app.store.InsertThreadRequest(store.ThreadRequest{
		Token:          token,
		CallerThreadID: caller.ThreadID,
		Kind:           store.ThreadRequestRemind,
		DueAt:          call.DueAtUnixMs,
		// The note is the answer, written now: the sweep that fires the
		// reminder has nothing to ask anyone for, and storing it here means
		// a restart between arming and firing cannot lose the text.
		Answer:     []byte(call.Note),
		AnswerKind: store.ThreadAnswerNote,
		// A reminder is the wake. Without notify it would arm nothing and
		// fire into a wait that is long over.
		Notify: true,
		State:  store.ThreadRequestAccepted,
	}); err != nil {
		return threadtools.RequestAck{}, err
	}
	// The sweep may be sleeping past this reminder's due time; waking it
	// keeps a reminder for ten seconds from now from waiting out the tick.
	t.app.nudgeThreadRequestSweep()
	return threadtools.RequestAck{
		RequestState: threadtools.RequestState{
			Token:    token,
			Kind:     store.ThreadRequestRemind,
			State:    store.ThreadRequestAccepted,
			Notify:   true,
			Revision: 0,
		},
		Outcome: threadtools.OutcomeBackgrounded,
	}, nil
}

// fireDueThreadReminders settles every reminder whose time has come. It is
// the whole of a reminder's destination side: the note the caller wrote is
// the answer, and the collector delivers it exactly as it delivers a reply.
func (a *App) fireDueThreadReminders(now time.Time) {
	rows, err := a.store.DueThreadReminders(now.UnixMilli(), threadReminderBatch)
	if err != nil {
		logThreadRequestSweep("read due reminders", err)
		return
	}
	for _, row := range rows {
		a.fireThreadReminder(row, now)
	}
}

func (a *App) fireThreadReminder(row store.ThreadRequest, now time.Time) {
	if a.settleThreadReminder(row, now) {
		a.deliverThreadWake(row.Token, false)
	}
}

// settleThreadReminder settles one due reminder under its settle lock and
// reports whether its wake is owed. The wake itself is delivered by the
// caller with the lock released, because delivery takes the caller thread's
// own locks.
func (a *App) settleThreadReminder(row store.ThreadRequest, now time.Time) bool {
	unlock := a.threadRequestSettleLock(row.Token)
	defer unlock()

	settled, err := a.store.SettleThreadRequest(row.Token, []string{store.ThreadRequestAccepted}, store.ThreadRequestSettlement{
		State:      store.ThreadRequestFinished,
		Answer:     row.Answer,
		AnswerKind: store.ThreadAnswerNote,
		SettledAt:  now.UnixMilli(),
	})
	if err != nil {
		logThreadRequestSweep("settle reminder "+row.Token, err)
		return false
	}
	if !settled {
		// Cancelled or already fired while this sweep was reading.
		return false
	}
	// The collector re-reads the row under the settle lock: thread_status
	// can have disarmed the wake since the sweep listed this row, and the
	// collector is the one place that decides whether a message is owed.
	return a.finishThreadRequestCollection(row.Token, false)
}

// threadReminderBatch bounds one sweep's work. Reminders that miss a batch
// are due on the next tick, a second later.
const threadReminderBatch = 32
