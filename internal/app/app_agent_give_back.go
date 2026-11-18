package app

import (
	"fmt"
	"log"
	"slices"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// Answers, reminders, remote completions and a spawner's answers to async
// questions that never reached the agent go back to their owners: from a
// drop, a session end, a failed resend and the boot, and never once the
// provider's echo confirmed them.

// ownedMessage is a message that belongs to a request or a watch: an answer,
// a reminder or a remote job's completion, by its send id. rowID is its queue
// row, empty when it has none left; unconfirmed marks one the provider was
// handed and never confirmed, whose row went at the write. ended marks one
// a session end or a failed Codex resend did not take in, rather than a
// person's Stop, archive or revert or a restart.
type ownedMessage struct {
	sendID      string
	rowID       string
	unconfirmed bool
	ended       bool
}

// withdrawal is the store scope that gives m back.
func (m ownedMessage) withdrawal() store.WakeWithdrawal {
	if m.unconfirmed {
		return store.WithdrawUnconfirmed
	}
	return store.WithdrawQueued
}

// ownedSendID reports an answer's, a reminder's or a remote completion's
// send id, or an agent's answer to its worker's async questions, whose
// owner is those questions (returnWorkerAnswer).
func ownedSendID(sendID string) bool {
	_, _, isAnswer := threadWakeFromSendID(sendID)
	_, _, isCompletion := remoteCompletionFromSendID(sendID)
	return isAnswer || isCompletion || isWorkerAnswer(sendID)
}

// giveBackToOwners gives answers, reminders and remote completions that
// never reached the agent back to their owners. A request is owed its answer
// again, disarmed, so thread_status returns it and nothing delivers it
// unasked; a watch is pending again, and the watcher delivers its completion
// as it does one that finishes later, starting the session for it; a
// spawner's answer to its worker's async questions puts them back to
// unanswered, routed again (returnWorkerAnswer). None of them goes to the
// composer: it is not the person's to send.
//
// A completion a session end gave back (ended) is delivered again on the
// thread request wake retry's ladder rather than at once, so a session that
// keeps ending before it takes the completion in is not started every few
// seconds; once the ladder is spent, the watch is dismissed with the reason
// and keeps the completion for remote_jobs.
//
// Every path that takes one out of the thread's delivery runs it: a
// wholesale drop (Stop, revert, teardown), a session that ended with it
// queued or unconfirmed, a Codex resend that failed, and the boot sweep. A
// drop runs it once the queue is gone, memory and dispatch included, because
// the watcher can queue a pending completion again within seconds and one
// queued before then would be dropped with the rest. Each message goes back
// by its row, so a message queued again after the caller read the queue is
// not taken; an unconfirmed one has no row left and goes back while its
// owner still reads queued. One the provider confirmed never goes back:
// its owner's store write refuses it and only removes a row that outlived
// the echo.
func (a *App) giveBackToOwners(threadID string, messages []ownedMessage) {
	returned := false
	a.retryOwnedEchoes(threadID)
	for _, m := range messages {
		if a.echoed(m.sendID) {
			// The provider took it in; only its confirmation is unwritten.
			continue
		}
		if isWorkerAnswer(m.sendID) {
			// Its questions say whether the provider took it in: its echo's
			// confirmation marks them delivered, and nothing returns them
			// then.
			logThreadRequestSweep("return "+m.sendID+" to its questions", a.returnWorkerAnswer(threadID, m.sendID, false))
			continue
		}
		if token, late, ok := threadWakeFromSendID(m.sendID); ok {
			// The message's requests go back together: its carrier and
			// every request that rode with it.
			returned, err := a.store.WithdrawThreadWake(token, late, m.rowID, m.withdrawal())
			logThreadRequestSweep("return "+m.sendID+" to its requests", err)
			if err == nil {
				a.redeliverHandedOffWakes(threadID, returned, late)
			}
			continue
		}
		if computerID, requestID, ok := remoteCompletionFromSendID(m.sendID); ok {
			var paced func(int64) store.CompletionReturn
			gaveUp := false
			if m.ended {
				paced = func(attempts int64) store.CompletionReturn {
					back := completionRedelivery(time.Now(), attempts)
					gaveUp = back.Issue != ""
					return back
				}
			}
			err := a.store.WithdrawRemoteCompletion(computerID, requestID, m.rowID, m.withdrawal(), paced)
			logThreadRequestSweep("return "+m.sendID+" to its watch", err)
			if err == nil && gaveUp {
				log.Printf("remote completion watches: giving up on delivering %s after %d session ends", m.sendID, threadWakeRetryAttempts+1)
			}
			returned = returned || err == nil
		}
	}
	if returned {
		// A completion given back is its watch's to deliver again.
		a.nudgeRemoteWatches()
		a.emitBackgroundChanged(threadID)
	}
	if a.heldReturnedForTest != nil {
		a.heldReturnedForTest(threadID)
	}
}

// confirmOwnedDeliveries records the provider's echo of the answers,
// reminders, completions and spawners' answers to async questions consumed
// sends carry: the second phase of their owners' delivery mark, after which
// nothing gives them back, with the row that carried an answer, which its
// request or its questions name from then on, whether or not the echo's own
// row write lands. It runs on
// the provider read loop under the flush anchor lock, so a person's send
// costs nothing but the id check. A confirmation whose write fails is kept
// and written again (app_thread_tools_bookkeeping.go), and until then the
// message reads consumed to every give-back.
func (a *App) confirmOwnedDeliveries(threadID string, sends []triage.ConsumedSend) {
	now := time.Now().UnixMilli()
	retried := false
	for _, send := range sends {
		for _, id := range send.SendIDs {
			if !ownedSendID(id) {
				continue
			}
			if !retried {
				a.retryOwnedEchoes(threadID)
				retried = true
			}
			if err := a.confirmOwnedEcho(id, ownedEcho{threadID: threadID, itemID: send.ItemID, at: now}); err != nil {
				log.Printf("thread tools: confirm %s in %s, kept to try again: %v", id, threadID, err)
			}
		}
	}
}

// giveBackUnconfirmedAtBoot gives back the answers, reminders and
// completions the previous process handed a provider that never confirmed
// them: their rows went at the provider write, and the session that held
// them is gone. A message whose row is still queued is the row's, which
// restoreDurableFlushQueueAtBoot gives back or keeps for the next boot. One
// whose echo a row proves is confirmed instead (confirmEchoedAtBoot).
func (a *App) giveBackUnconfirmedAtBoot() {
	byThread := make(map[string][]ownedMessage)
	requests, err := a.store.UnconfirmedThreadWakes()
	logThreadRequestSweep("list the unconfirmed thread wakes at boot", err)
	firsts := make(map[string]bool)
	for _, r := range requests {
		// A request that rode with another's first wake was the carrier's
		// message, given back once by the carrier's send id.
		if first := threadWakeSendID(r.WakeCarrier()); r.DeliveredAt != 0 && r.ConfirmedAt == 0 && !firsts[first] {
			firsts[first] = true
			byThread[r.CallerThreadID] = append(byThread[r.CallerThreadID], ownedMessage{sendID: first, unconfirmed: true})
		}
		if r.LateDeliveredAt != 0 && r.LateConfirmedAt == 0 {
			byThread[r.CallerThreadID] = append(byThread[r.CallerThreadID], ownedMessage{sendID: threadWakeLateSendID(r.Token), unconfirmed: true})
		}
	}
	watches, err := a.store.UnconfirmedRemoteCompletions()
	logThreadRequestSweep("list the unconfirmed remote completions at boot", err)
	for _, w := range watches {
		byThread[w.ThreadID] = append(byThread[w.ThreadID], ownedMessage{sendID: remoteCompletionSendID(w), unconfirmed: true})
	}
	for threadID, owned := range byThread {
		rows, err := a.store.ListFlushQueueItems(threadID)
		if err != nil {
			logThreadRequestSweep("list the queued messages of "+threadID+" at boot", err)
			continue
		}
		owned = slices.DeleteFunc(owned, func(m ownedMessage) bool {
			return slices.ContainsFunc(rows, func(row store.FlushQueueItem) bool { return row.SendID == m.sendID })
		})
		if owned = a.confirmEchoedAtBoot(threadID, owned); len(owned) > 0 {
			a.giveBackToOwners(threadID, owned)
		}
	}
}

// completionRedelivery paces a completion a session end gave back, after
// attempts earlier ones, on the thread request wake retry's ladder
// (retryUndeliveredThreadWakes): each return books the next delivery
// further out, and the one after the last attempt gives it up.
func completionRedelivery(now time.Time, attempts int64) store.CompletionReturn {
	if attempts >= threadWakeRetryAttempts {
		return store.CompletionReturn{Issue: fmt.Sprintf(
			"Not delivered: the conversation's session ended %d times before the agent took this completion in.", attempts+1)}
	}
	attempts++
	return store.CompletionReturn{NextCheck: now.Add(threadWakeRetryDelay(attempts)).UnixMilli(), Attempts: attempts}
}
