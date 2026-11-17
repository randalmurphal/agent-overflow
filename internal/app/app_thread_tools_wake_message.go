package app

import (
	"cmp"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/usermessage"
)

// The wake's message: which requests one message answers, what it says, who
// it is attributed to, and the one transaction that queues it.

// deliverThreadWakeMessages queues each message, and rebuilds the ones that
// were not written because a request they name stopped owing the wake while
// they were being written (store.QueueThreadWake: another route delivered
// it, or a late reply superseded it), or because New Thread, Same Assistant
// moved their requests to another thread after they were built. The rebuild
// reads the rows as they are now, which drops that request and keeps the
// rest in one message, or names the thread they moved to and where they
// were made. The passes are bounded.
func (a *App) deliverThreadWakeMessages(messages []threadWakeMessage) {
	for pass := 0; ; pass++ {
		var stale []threadWakeRef
		for _, message := range messages {
			if a.deliverThreadWakeMessage(message) {
				continue
			}
			for _, row := range message.rows {
				stale = append(stale, threadWakeRef{token: row.Token, late: message.late})
			}
		}
		if len(stale) == 0 {
			return
		}
		if pass == threadWakeRebuilds {
			for _, ref := range stale {
				a.failThreadWake(ref.token, "queue the wake", errThreadWakeStillChanging)
			}
			return
		}
		messages = a.threadWakeMessages(stale)
	}
}

// threadWakeRebuilds bounds how many times one delivery rebuilds messages
// whose requests changed under them before leaving the rest to the retry
// pass.
const threadWakeRebuilds = 4

var errThreadWakeStillChanging = errors.New("the requests it answers kept changing while it was written")

// threadWakeMessage is one message to one caller thread and the requests it
// answers. The first row is the carrier: the message is queued under its send
// id, and the rest record its token as the message that delivered them.
type threadWakeMessage struct {
	late bool
	// kind, text and cause are what every row says: the wake's kind, its
	// body and, for an interruption, its cause.
	kind, text string
	cause      threadtools.Cause
	// madeIn is the assistant's past thread every row was made in, empty
	// when its caller made it (store.ThreadRequest.MadeIn).
	madeIn string
	rows   []store.ThreadRequest
}

// threadWakeMessageRequests bounds how many requests one message answers. It
// is far above what a thread has open at once, so a restart is one notice,
// and low enough that the list of threads stays a fraction of the queue's
// message cap.
const threadWakeMessageRequests = 512

// threadWakeMessages reads the claimed wakes and folds the ones that say the
// same thing to the same thread into one message each, oldest request first,
// so a message names its requests in the order the thread made them however
// the settlements were observed.
//
// The key is the caller, the kind, the whole text, the cause and the past
// thread a handoff moved the request from: every line the message says outside its
// list of threads is every row's. A late reply is never folded: it is an
// answer to one request, written after the others were already told. A wake that is no longer owed is dropped here, which is what
// makes a repeated observation of one settlement a no-op, and so is one that
// was disarmed since it was decided (the caller was archived, deleted or
// moved).
func (a *App) threadWakeMessages(refs []threadWakeRef) []threadWakeMessage {
	owed := make([]threadWakeMessage, 0, len(refs))
	for _, ref := range refs {
		row, found, err := a.store.GetThreadRequest(ref.token)
		if err != nil {
			a.failThreadWake(ref.token, "read the request", err)
			continue
		}
		if !found || !row.Notify || !row.WakeOwed(ref.late) {
			continue
		}
		message, err := soloThreadWakeMessage(row, ref.late)
		if err != nil {
			a.failThreadWake(ref.token, "render the wake", err)
			continue
		}
		owed = append(owed, message)
	}
	slices.SortStableFunc(owed, func(x, y threadWakeMessage) int {
		return cmp.Or(
			cmp.Compare(x.rows[0].CreatedAt, y.rows[0].CreatedAt),
			cmp.Compare(x.rows[0].Token, y.rows[0].Token),
		)
	})
	type sameWake struct {
		caller, kind, text, madeIn string
		cause                      threadtools.Cause
	}
	taking := make(map[sameWake]int, len(owed))
	messages := make([]threadWakeMessage, 0, len(owed))
	for _, message := range owed {
		if message.late {
			messages = append(messages, message)
			continue
		}
		row := message.rows[0]
		key := sameWake{caller: row.CallerThreadID, kind: message.kind, text: message.text, madeIn: message.madeIn, cause: message.cause}
		if index, ok := taking[key]; ok && len(messages[index].rows) < threadWakeMessageRequests {
			messages[index].rows = append(messages[index].rows, row)
			continue
		}
		taking[key] = len(messages)
		messages = append(messages, message)
	}
	return messages
}

// soloThreadWakeMessage is the message one wake of row would be alone: its
// kind, text, cause and past thread, the key threadWakeMessages folds by.
func soloThreadWakeMessage(row store.ThreadRequest, late bool) (threadWakeMessage, error) {
	kind, text, err := threadWakeContent(row, late)
	if err != nil {
		return threadWakeMessage{}, err
	}
	message := threadWakeMessage{late: late, kind: kind, text: text, rows: []store.ThreadRequest{row}}
	if kind == threadtools.WakeInterrupted {
		message.cause = threadtools.Cause{Code: row.Cause, Arg: row.CauseArg}
	}
	if row.MadeIn() != row.CallerThreadID {
		message.madeIn = row.MadeIn()
	}
	return message, nil
}

// deliverThreadWakeMessage queues one message and records, in the same
// transaction, that it delivered every request it answers.
//
// It reports false only when the message was not written because one of its
// requests no longer owes the wake, which the caller answers by rebuilding
// it. Every other outcome is final here: delivered, or failed and noted on
// each request for the retry pass.
func (a *App) deliverThreadWakeMessage(message threadWakeMessage) bool {
	carrier := message.rows[0]
	tokens := make([]string, 0, len(message.rows))
	for _, row := range message.rows {
		tokens = append(tokens, row.Token)
	}
	fail := func(what string, cause error) {
		for _, token := range tokens {
			a.failThreadWake(token, what, cause)
		}
	}
	thread, err := a.store.GetThread(carrier.CallerThreadID)
	if errors.Is(err, sql.ErrNoRows) {
		// The thread is gone. The request rows are cascading away with it;
		// marking them keeps a concurrent reader from waiting on a wake that
		// will never be written.
		for _, token := range tokens {
			log.Printf("thread tools: request %s has no caller thread; nothing delivered", token)
			if _, markErr := a.store.MarkThreadRequestDelivered(token, store.ThreadWakeInline, 0, message.late); markErr != nil {
				log.Printf("thread tools: mark request %s undeliverable: %v", token, markErr)
			}
		}
		return true
	}
	if err != nil {
		// A read that failed says nothing about whether the thread exists.
		// The wakes stay owed and the retry pass comes back to them.
		fail("read the caller thread", err)
		return true
	}
	// A caller a delete claimed takes the wake into its queue, as every
	// app-written message (store.CheckThreadDeliverable).
	if err := a.store.CheckThreadDeliverable(thread); err != nil {
		fail("deliver to "+thread.ID, err)
		return true
	}
	// The message is attributed to the thread that answered, for the same
	// reason the request is attributed to its sender. One that lists several
	// threads is attributed to the carrier's, which threadWakeFor lists first
	// under its token, the one its echo confirms (ConfirmThreadWake), and
	// names them all in its facts.
	note, err := usermessage.Answer(a.threadWakeFor(message))
	if err != nil {
		fail("write the wake", err)
		return true
	}
	if hook := a.threadWakeRenderedForTest; hook != nil {
		hook(carrier.Token)
	}
	// The thread the notice is queued on: the assistant's current thread
	// when New Thread, Same Assistant superseded the caller the message was
	// built for. The message names its requests' caller and where they were
	// made as they stood when it was built, so one bound for another thread
	// is stale and is rebuilt from the requests as they are now, which the
	// handoff moved with the thread they were made in.
	recipient := thread.ID
	if err := a.queueAgentNotice(agentNotice{
		threadID:  thread.ID,
		sendID:    threadWakeSendIDFor(carrier.Token, message.late),
		recipient: func(threadID string) { recipient = threadID },
		prepare: func() (usermessage.AgentMessage, bool, error) {
			if recipient != thread.ID {
				return note, false, fmt.Errorf("request %s was built for %s and goes to %s: %w", carrier.Token, thread.ID, recipient, store.ErrThreadWakeNotOwed)
			}
			return note, true, a.unarchiveForThreadWake(recipient, carrier, message.late)
		},
		// One transaction writes the durable queue row and every delivery
		// mark, so a second observation of the same settlement cannot queue
		// the answer twice and a crash cannot lose it.
		persist: func(item store.FlushQueueItem) error {
			return a.store.QueueThreadWake(carrier.Token, tokens[1:], message.late, item)
		},
		startFailed: func(err error) string {
			return "A thread request answered, its message is queued, but the agent could not start: " + err.Error()
		},
	}); err != nil {
		if errors.Is(err, store.ErrThreadWakeNotOwed) {
			return false
		}
		fail("queue the wake", err)
	}
	return true
}

// unarchiveForThreadWake brings an archived caller back to read a wake that
// is still armed: the answer the thread asked for arriving is the reason to
// bring it back, and a wake queued on an archived thread would sit unread
// behind a row the sidebar does not show.
//
// A thread that is not archived is not locked: an archive that lands after
// this read is refused by the queue write (errThreadArchived) or takes the
// wake back (withdrawQueuedMessages). An archived one is unarchived under
// the caller's action lock, which an archive holds while it flips the row
// and disarms the thread's wakes, so a wake that was on its way when the
// thread was archived finds its request disarmed and is not delivered, and
// only one re-armed since (a later wait or thread_status) unarchives. A
// handoff archives a thread under the same lock in the transaction that
// moves its requests to the successor, so a request whose caller is no
// longer threadID is refused here and rebuilt for the thread it moved to.
func (a *App) unarchiveForThreadWake(threadID string, carrier store.ThreadRequest, late bool) error {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return fmt.Errorf("read the caller thread: %w", err)
	}
	if !thread.Archived {
		return nil
	}
	unlock, err := a.threadLocks().LockCtx(a.lifeCtx(), threadID)
	if err != nil {
		return fmt.Errorf("lock the caller thread: %w", err)
	}
	defer unlock()
	thread, err = a.store.GetThread(threadID)
	if err != nil {
		return fmt.Errorf("read the caller thread: %w", err)
	}
	if !thread.Archived {
		return nil
	}
	row, found, err := a.store.GetThreadRequest(carrier.Token)
	if err != nil {
		return fmt.Errorf("read request %s: %w", carrier.Token, err)
	}
	if !found || !row.Notify || !row.WakeOwed(late) || row.CallerThreadID != threadID {
		return fmt.Errorf("request %s: %w", carrier.Token, store.ErrThreadWakeNotOwed)
	}
	if _, err := a.unarchiveThreadLocked(thread.ID); err != nil {
		return fmt.Errorf("unarchive the caller thread: %w", err)
	}
	return nil
}

// threadWakeFor renders one message's wake from the fixed template: the thread
// or threads that answered, each with the requests of it this message
// answers.
//
// A thread on this computer is named from its row. A thread on another
// computer has no row here: its title is what that computer reported, kept on
// the request so a wake rendered after a restart still names it, and the
// computer is named the way every other row names it.
func (a *App) threadWakeFor(message threadWakeMessage) threadtools.Wake {
	wake := threadtools.Wake{Kind: message.kind, Text: message.text, Cause: message.cause}
	// A handoff made the assistant's current thread the caller and kept the
	// thread that asked; the wake says which one that was.
	if message.madeIn != "" {
		carrier := message.rows[0]
		wake.MadeIn, wake.Now = message.madeIn, carrier.CallerThreadID
		if current, ok := a.assistantCurrentThread(carrier.CallerThreadID); ok {
			wake.Now = current
		}
	}
	type target struct{ computer, thread string }
	index := make(map[target]int, len(message.rows))
	now := time.Now().UnixMilli()
	for _, row := range message.rows {
		answeredAt := row.SettledAt
		if message.late {
			answeredAt = row.LateReplyAt
		}
		var age time.Duration
		if answeredAt > 0 {
			age = time.Duration(now-answeredAt) * time.Millisecond
		}
		key := target{computer: row.TargetComputerID, thread: row.TargetThreadID}
		at, seen := index[key]
		if !seen {
			source := threadtools.WakeSource{ThreadID: row.TargetThreadID}
			if row.TargetComputerID != "" {
				source.Title = row.TargetThreadTitle
				source.Computer, source.ComputerID = a.threadTools().threadToolsBackendName(row.TargetComputerID)
			} else if row.TargetThreadID != "" {
				if thread, err := a.store.GetThread(row.TargetThreadID); err == nil {
					source.Title = thread.Title
				}
			}
			at = len(wake.Sources)
			index[key] = at
			wake.Sources = append(wake.Sources, source)
		}
		wake.Sources[at].Tokens = append(wake.Sources[at].Tokens, row.Token)
		// A standing agent that answered is named as one, from what the
		// answer this wake carries recorded, so a thread on either computer
		// reads the same.
		agent := row.AnswerAgent
		if message.late {
			agent = row.LateReplyAgent
		}
		if agent != nil && wake.Sources[at].Agent == nil {
			wake.Sources[at].Agent = &threadtools.SenderAgent{Role: agent.Role}
		}
		// The oldest answer is the age stated: it is the one that has waited.
		wake.Sources[at].Age = max(wake.Sources[at].Age, age)
	}
	return wake
}

// threadWakeContent is what one wake says: its kind and its body. The kind
// is what the settled state means to the caller, which is not always what it
// means to the destination: a reminder is `finished` on both sides and reads
// as neither a reply nor a thread's last message.
func threadWakeContent(row store.ThreadRequest, late bool) (kind, text string, err error) {
	switch {
	case late:
		return threadtools.WakeReply, string(row.LateReply), nil
	case row.Kind == store.ThreadRequestRemind:
		return threadtools.WakeReminder, string(row.Answer), nil
	case row.State == store.ThreadRequestReplied:
		return threadtools.WakeReply, string(row.Answer), nil
	case row.State == store.ThreadRequestFinished:
		return threadtools.WakeFinished, string(row.Answer), nil
	case row.State == store.ThreadRequestErrored, row.State == store.ThreadRequestRefused:
		return threadtools.WakeErrored, string(row.Answer), nil
	case row.State == store.ThreadRequestCancelled:
		// A cancel the agent made itself disarms the wake; one that still
		// owes it was made for the agent, and the answer says by whom.
		return threadtools.WakeCancelled, string(row.Answer), nil
	case row.State == store.ThreadRequestInterrupted && row.AnswerKind == store.ThreadAnswerFinal:
		// A person's Stop: the answer is what the turn said before it
		// (turnSettlement).
		return threadtools.WakeStopped, string(row.Answer), nil
	case row.State == store.ThreadRequestInterrupted:
		// The text is what the turn had written, or with no cause the
		// whole reason as an earlier build recorded it.
		return threadtools.WakeInterrupted, string(row.Answer), nil
	case row.State == store.ThreadRequestExpired:
		return threadtools.WakeExpired, "", nil
	default:
		return "", "", fmt.Errorf("request %s is %s, which owes no wake", row.Token, row.State)
	}
}
