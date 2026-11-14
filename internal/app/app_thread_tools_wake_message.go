package app

import (
	"cmp"
	"context"
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
// they were being written (store.QueueThreadWake): another route delivered
// it, or a late reply superseded it. The rebuild reads the rows as they are
// now, which drops that request and keeps the rest in one message. Every
// rebuild drops at least one request, and the passes are bounded anyway.
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
	// kind and text are what every row says: the wake's kind and its body.
	kind, text string
	rows       []store.ThreadRequest
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
// The key is the caller, the kind and the whole text. A late reply is never
// folded: it is an answer to one request, written after the others were
// already told. A wake that is no longer owed is dropped here, which is what
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
		kind, text, err := threadWakeContent(row, ref.late)
		if err != nil {
			a.failThreadWake(ref.token, "render the wake", err)
			continue
		}
		owed = append(owed, threadWakeMessage{late: ref.late, kind: kind, text: text, rows: []store.ThreadRequest{row}})
	}
	slices.SortStableFunc(owed, func(x, y threadWakeMessage) int {
		return cmp.Or(
			cmp.Compare(x.rows[0].CreatedAt, y.rows[0].CreatedAt),
			cmp.Compare(x.rows[0].Token, y.rows[0].Token),
		)
	})
	type sameWake struct{ caller, kind, text string }
	taking := make(map[sameWake]int, len(owed))
	messages := make([]threadWakeMessage, 0, len(owed))
	for _, message := range owed {
		if message.late {
			messages = append(messages, message)
			continue
		}
		row := message.rows[0]
		key := sameWake{caller: row.CallerThreadID, kind: message.kind, text: message.text}
		if index, ok := taking[key]; ok && len(messages[index].rows) < threadWakeMessageRequests {
			messages[index].rows = append(messages[index].rows, row)
			continue
		}
		taking[key] = len(messages)
		messages = append(messages, message)
	}
	return messages
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
	if err := a.store.CheckThreadExecutionAccess(thread); err != nil {
		fail("deliver to "+thread.ID, err)
		return true
	}
	wake := a.threadWake(message)
	origin, originThread := a.threadWakeAttribution(carrier, wake)
	body := wake.String()
	ctx, cancel := context.WithTimeout(a.lifeCtx(), threadWakeLockTimeout)
	defer cancel()
	if err := a.queueAgentNotice(ctx, agentNotice{
		threadID:     thread.ID,
		sendID:       threadWakeSendIDFor(carrier.Token, message.late),
		origin:       origin,
		originThread: originThread,
		prepare: func() (string, bool, error) {
			return body, true, a.unarchiveForThreadWake(carrier, message.late)
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
// It runs under the caller's action lock, which an archive holds while it
// flips the row and disarms the thread's wakes. So it reads the thread either
// before an archive or after the disarm: a wake that was on its way when the
// thread was archived finds its request disarmed and is not delivered, and
// only one re-armed since (a later wait or thread_status) unarchives.
func (a *App) unarchiveForThreadWake(carrier store.ThreadRequest, late bool) error {
	thread, err := a.store.GetThread(carrier.CallerThreadID)
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
	if !found || !row.Notify || !row.WakeOwed(late) {
		return fmt.Errorf("request %s: %w", carrier.Token, store.ErrThreadWakeNotOwed)
	}
	if _, err := a.unarchiveThreadLocked(thread.ID); err != nil {
		return fmt.Errorf("unarchive the caller thread: %w", err)
	}
	return nil
}

// threadWakeAttribution says who a wake's message is from. One that speaks
// for a single thread is attributed to that thread, for the same reason the
// request itself is attributed to its sender: a person reading the timeline
// can see where it came from. One that lists several threads is the app's
// own notice about them, and names them in its text.
func (a *App) threadWakeAttribution(carrier store.ThreadRequest, wake threadtools.Wake) (string, *usermessage.OriginThread) {
	if len(wake.Sources) != 1 {
		return usermessage.OriginApp, nil
	}
	origin := &usermessage.OriginThread{ThreadID: carrier.TargetThreadID, Token: carrier.Token, Title: wake.Sources[0].Title}
	if carrier.TargetComputerID != "" {
		origin.ComputerName, origin.ComputerID = a.threadTools().threadToolsBackendName(carrier.TargetComputerID)
	}
	return usermessage.OriginAgentThread, origin
}

// threadWake renders one message's wake from the fixed template: the thread
// or threads that answered, each with the requests of it this message
// answers.
//
// A thread on this computer is named from its row. A thread on another
// computer has no row here: its title is what that computer reported, kept on
// the request so a wake rendered after a restart still names it, and the
// computer is named the way every other row names it.
func (a *App) threadWake(message threadWakeMessage) threadtools.Wake {
	wake := threadtools.Wake{Kind: message.kind, Text: message.text}
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
				source.Computer, _ = a.threadTools().threadToolsBackendName(row.TargetComputerID)
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
		return threadtools.WakeCancelled, "", nil
	case row.State == store.ThreadRequestInterrupted:
		return threadtools.WakeInterrupted, string(row.Answer), nil
	case row.State == store.ThreadRequestExpired:
		return threadtools.WakeExpired, "", nil
	default:
		return "", "", fmt.Errorf("request %s is %s, which owes no wake", row.Token, row.State)
	}
}
