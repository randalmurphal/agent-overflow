package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/usermessage"
)

// The wake: how a settled answer reaches the thread that asked for it as a
// message, when no call was parked to carry it. A wake is queued rather than
// sent, retried on its own clock when the caller cannot take it, and given
// up with the reason recorded.

// threadWakeSendID and threadWakeLateSendID name the two messages one request
// can deliver. They are distinct so a late reply is never mistaken for a
// retry of the first wake and dropped by send admission.
func threadWakeSendID(token string) string     { return "thread-wake:" + token }
func threadWakeLateSendID(token string) string { return "thread-wake-late:" + token }

func threadWakeSendIDFor(token string, late bool) string {
	if late {
		return threadWakeLateSendID(token)
	}
	return threadWakeSendID(token)
}

// deliverThreadWake puts the answer in the caller's message queue, as the
// message it would have received had it been waiting.
//
// Queued rather than sent: the caller is usually mid-turn, and a message that
// interrupted it would take the agent off what it is doing to read an answer
// it asked to be told about later. The queue delivers it at the next turn
// boundary, which is exactly what the ack promised.
func (a *App) deliverThreadWake(token string, late bool) {
	row, found, err := a.store.GetThreadRequest(token)
	if err != nil || !found {
		if err != nil {
			a.failThreadWake(token, "read the request", err)
		}
		return
	}
	if late && row.LateDeliveredAt != 0 {
		return
	}
	if !late && row.DeliveredAt != 0 {
		return
	}
	thread, err := a.store.GetThread(row.CallerThreadID)
	if errors.Is(err, sql.ErrNoRows) {
		// The thread is gone. The request row is cascading away with it;
		// marking it keeps a concurrent reader from waiting on a wake that
		// will never be written.
		log.Printf("thread tools: request %s has no caller thread; nothing delivered", token)
		if _, markErr := a.store.MarkThreadRequestDelivered(token, store.ThreadWakeInline, 0, late); markErr != nil {
			log.Printf("thread tools: mark request %s undeliverable: %v", token, markErr)
		}
		return
	}
	if err != nil {
		// A read that failed says nothing about whether the thread exists.
		// The wake stays owed and the retry pass comes back to it.
		a.failThreadWake(token, "read the caller thread", err)
		return
	}
	if thread.Archived {
		// The answer the thread asked for arriving is exactly the reason to
		// bring it back; a wake queued on an archived thread would sit unread
		// behind a row the sidebar does not show.
		if _, err := a.UnarchiveThread(thread.ID); err != nil {
			a.failThreadWake(token, "unarchive the caller thread", err)
			return
		}
	}
	if err := a.store.CheckThreadExecutionAccess(thread); err != nil {
		a.failThreadWake(token, "deliver to "+thread.ID, err)
		return
	}
	body, err := a.threadWakeBody(row, late)
	if err != nil {
		a.failThreadWake(token, "render the wake", err)
		return
	}
	ctx, cancel := context.WithTimeout(a.lifeCtx(), threadWakeLockTimeout)
	defer cancel()
	if err := a.queueAgentNotice(ctx, agentNotice{
		threadID: thread.ID,
		sendID:   threadWakeSendIDFor(token, late),
		// The wake is a message the app wrote on the answering thread's
		// behalf, and it is attributed to that thread for the same reason
		// the request itself is attributed to the sender: a person reading
		// the timeline can see where it came from.
		origin:       string(usermessage.OriginAgentThread),
		originThread: a.threadWakeOrigin(row),
		prepare:      func() (string, bool, error) { return body, true, nil },
		// One transaction writes the durable queue row and the delivery
		// mark, so a second observation of the same settlement cannot queue
		// the answer twice and a crash cannot lose it.
		persist: func(item store.FlushQueueItem) error {
			return a.store.QueueThreadWake(token, late, item)
		},
		startFailed: func(err error) string {
			return "A thread request answered, its message is queued, but the agent could not start: " + err.Error()
		},
	}); err != nil {
		a.failThreadWake(token, "queue the wake", err)
	}
}

// threadWakeLockTimeout bounds the wait for the caller thread's lock. A wake
// that cannot have it now is owed just the same, and the retry pass carries
// it.
const threadWakeLockTimeout = 10 * time.Second

// failThreadWake records why one wake could not be handed over. The reason
// is kept on the row, so the retry that gives up can say what stopped it and
// a person reading the ledger can see the same thing.
func (a *App) failThreadWake(token, what string, cause error) {
	log.Printf("thread tools: wake %s could not %s: %v", token, what, cause)
	if err := a.store.NoteThreadRequestWakeIssue(token, "Could not "+what+": "+cause.Error()); err != nil {
		logThreadRequestSweep("note wake issue for "+token, err)
	}
	// The retry pass owns this row now, and the sweep may be asleep.
	a.nudgeThreadRequestSweep()
}

// retryUndeliveredThreadWakes hands over the wakes a settlement could not.
//
// A settlement and its wake are two transactions, and the second one has ends
// that deliver nothing: a crash between them, a caller thread that could not
// be unarchived or locked in time, a render that failed. Nothing else revisits
// those rows, because the pollers read open requests and this one is settled,
// so the answer would sit in the ledger unread forever. The delivery mark is
// conditional, so a retry of a wake that did land is a no-op.
//
// Each row carries its own clock and attempt count. The delay doubles from
// half a minute towards the ten-minute ceiling, and a wake that has used its
// attempts is abandoned with the reason recorded rather than retried for the
// life of the row: what stops a wake is the caller thread's own state, which
// no number of retries changes.
func (a *App) retryUndeliveredThreadWakes(now time.Time) {
	rows, err := a.store.UndeliveredThreadRequestWakes(now.UnixMilli(), threadRequestWakeRetryBatch)
	if err != nil {
		logThreadRequestSweep("read undelivered wakes", err)
		return
	}
	for _, row := range rows {
		owed := undeliveredThreadWakes(row)
		if len(owed) == 0 {
			continue
		}
		if row.WakeAttempts >= threadWakeRetryAttempts {
			a.abandonThreadWake(row)
			continue
		}
		attempts := row.WakeAttempts + 1
		// Booked before the attempt: a delivery that fails, or that is
		// still holding a lock when the next pass comes round, must not
		// bring this row back at once.
		if err := a.store.ScheduleThreadRequestWake(row.Token,
			now.Add(threadWakeRetryDelay(attempts)).UnixMilli(), attempts); err != nil {
			logThreadRequestSweep("schedule the wake retry for "+row.Token, err)
			continue
		}
		for _, late := range owed {
			stillOwed := func() bool {
				unlock := a.threadRequestSettleLock(row.Token)
				defer unlock()
				return a.finishThreadRequestCollection(row.Token, late)
			}()
			if stillOwed {
				a.deliverThreadWakeDetached(row.Token, late)
			}
		}
	}
}

// abandonThreadWake ends the retry of a wake that cannot be handed over.
//
// The answer is not lost: it stays on the row and `thread_status` returns it
// with the reason the message never arrived. What is given up is the message,
// and with it the row's place in the undelivered set.
func (a *App) abandonThreadWake(row store.ThreadRequest) {
	issue := row.WakeIssue
	if issue == "" {
		issue = "The caller thread could not take this answer as a message."
	}
	abandoned, err := a.store.AbandonThreadRequestWake(row.Token, issue)
	if err != nil {
		logThreadRequestSweep("abandon the wake for "+row.Token, err)
		return
	}
	if abandoned {
		log.Printf("thread tools: giving up on the wake for request %s after %d attempts: %s",
			row.Token, row.WakeAttempts, issue)
	}
}

// undeliveredThreadWakes names which of one row's two wakes are still owed.
func undeliveredThreadWakes(row store.ThreadRequest) []bool {
	owed := make([]bool, 0, 2)
	if row.SettledAt != 0 && row.DeliveredAt == 0 {
		owed = append(owed, false)
	}
	if len(row.LateReply) > 0 && row.LateDeliveredAt == 0 {
		owed = append(owed, true)
	}
	return owed
}

// threadWakeRetryDelay doubles the base delay for each attempt already made,
// up to the ceiling.
func threadWakeRetryDelay(attempts int64) time.Duration {
	delay := threadWakeRetryBase
	for range attempts - 1 {
		delay *= 2
		if delay >= threadWakeRetryCap {
			return threadWakeRetryCap
		}
	}
	return delay
}

// threadWakeRetryBase, threadWakeRetryCap and threadWakeRetryAttempts pace
// and bound the retry; threadRequestWakeRetryBatch bounds one pass. This is
// a recovery net behind a delivery that normally happens with the
// settlement, so it is slow and it ends.
const (
	threadWakeRetryBase         = 30 * time.Second
	threadWakeRetryCap          = 10 * time.Minute
	threadWakeRetryAttempts     = 8
	threadRequestWakeRetryBatch = 32
)

// threadWakeOrigin attributes a wake to the thread that answered, from the
// same two sources threadWakeBody reads: a thread on this computer has a
// row here, and a thread on another computer is named by what its own
// computer reported when the request's target was recorded.
func (a *App) threadWakeOrigin(row store.ThreadRequest) *usermessage.OriginThread {
	origin := &usermessage.OriginThread{ThreadID: row.TargetThreadID, Token: row.Token}
	if row.TargetComputerID != "" {
		origin.Title = row.TargetThreadTitle
		origin.ComputerName, origin.ComputerID = a.threadTools().threadToolsBackendName(row.TargetComputerID)
		return origin
	}
	if row.TargetThreadID != "" {
		if thread, err := a.store.GetThread(row.TargetThreadID); err == nil {
			origin.Title = thread.Title
		}
	}
	return origin
}

// threadWakeBody renders one wake from the fixed template. The kind is what
// the settled state means to the caller, which is not always what it means to
// the destination: a reminder is `finished` on both sides and reads as
// neither a reply nor a thread's last message.
func (a *App) threadWakeBody(row store.ThreadRequest, late bool) (string, error) {
	wake := threadtools.Wake{
		Token:    row.Token,
		ThreadID: row.TargetThreadID,
	}
	if row.SettledAt > 0 {
		wake.Age = time.Duration(time.Now().UnixMilli()-row.SettledAt) * time.Millisecond
	}
	if row.TargetComputerID != "" {
		// A thread on another computer has no row here: its title is what
		// that computer reported, kept on the request so a wake rendered
		// after a restart still names it, and the computer is named the way
		// every other row names it.
		wake.Title = row.TargetThreadTitle
		wake.Computer, _ = a.threadTools().threadToolsBackendName(row.TargetComputerID)
	} else if row.TargetThreadID != "" {
		if thread, err := a.store.GetThread(row.TargetThreadID); err == nil {
			wake.Title = thread.Title
		}
	}
	switch {
	case late:
		wake.Kind, wake.Text = threadtools.WakeReply, string(row.LateReply)
		wake.Age = time.Duration(time.Now().UnixMilli()-row.LateReplyAt) * time.Millisecond
	case row.Kind == store.ThreadRequestRemind:
		wake.Kind, wake.Text = threadtools.WakeReminder, string(row.Answer)
	case row.State == store.ThreadRequestReplied:
		wake.Kind, wake.Text = threadtools.WakeReply, string(row.Answer)
	case row.State == store.ThreadRequestFinished:
		wake.Kind, wake.Text = threadtools.WakeFinished, string(row.Answer)
	case row.State == store.ThreadRequestErrored, row.State == store.ThreadRequestRefused:
		wake.Kind, wake.Text = threadtools.WakeErrored, string(row.Answer)
	case row.State == store.ThreadRequestCancelled:
		wake.Kind = threadtools.WakeCancelled
	case row.State == store.ThreadRequestInterrupted:
		wake.Kind = threadtools.WakeInterrupted
	case row.State == store.ThreadRequestExpired:
		wake.Kind = threadtools.WakeExpired
	default:
		return "", fmt.Errorf("request %s is %s, which owes no wake", row.Token, row.State)
	}
	return wake.String(), nil
}
