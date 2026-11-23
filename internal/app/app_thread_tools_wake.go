package app

import (
	"log"
	"time"

	"agent-overflow/internal/store"
)

// The wake: how a settled answer reaches the thread that asked for it as a
// message, when no call was parked to carry it. A wake is queued rather than
// sent, retried on its own clock when the caller cannot take it, and given
// up with the reason recorded.

// threadWakeSendID and threadWakeLateSendID name the two messages one request
// can deliver (store.ThreadWakeSendID).
func threadWakeSendID(token string) string     { return store.ThreadWakeSendID(token, false) }
func threadWakeLateSendID(token string) string { return store.ThreadWakeSendID(token, true) }

func threadWakeSendIDFor(token string, late bool) string {
	return store.ThreadWakeSendID(token, late)
}

// threadWakeFromSendID reads a wake's request token back out of its send id
// (store.ParseThreadWakeSendID).
func threadWakeFromSendID(sendID string) (token string, late, ok bool) {
	return store.ParseThreadWakeSendID(sendID)
}

// redeliverHandedOffWakes sends the answers or reminders a thread New
// Thread, Same Assistant superseded gave back (tokens, one message's) on to
// the assistant's current thread, which the handoff made their requests'
// caller: armed again, since nothing about the answers changed but where
// they go, and delivered together so they fold as they did. Any other
// thread's give-back stays disarmed.
func (a *App) redeliverHandedOffWakes(threadID string, tokens []string, late bool) {
	if len(tokens) == 0 {
		return
	}
	if _, ok := a.assistantCurrentThread(threadID); !ok {
		return
	}
	refs := make([]threadWakeRef, 0, len(tokens))
	for _, token := range tokens {
		if _, err := a.store.SetThreadRequestNotify(token, true); err != nil {
			logThreadRequestSweep("arm the handed-off wake of "+token, err)
			continue
		}
		refs = append(refs, threadWakeRef{token: token, late: late})
	}
	a.goThreadRequestSettling(func() { a.deliverThreadWakes(refs) })
}

// threadWakeRef names one of a request's two wakes: its first answer, or the
// late reply that followed it.
type threadWakeRef struct {
	token string
	late  bool
}

// deliverThreadWake puts one answer in the caller's message queue, as the
// message it would have received had it been waiting.
//
// Queued rather than sent: the caller is usually mid-turn, and a message that
// interrupted it would take the agent off what it is doing to read an answer
// it asked to be told about later. The queue delivers it at the next turn
// boundary, which is exactly what the ack promised.
func (a *App) deliverThreadWake(token string, late bool) {
	a.deliverThreadWakes([]threadWakeRef{{token: token, late: late}})
}

// deliverThreadWakes hands over the wakes one event decided together: the
// end of a turn that answered several requests, a restart that interrupted
// every open one, a retry pass. Wakes owed to one thread that say the same
// thing become one message naming every request it answers
// (threadWakeMessages), so a thread that sent three messages into one turn
// is told once that the turn ended, not three times in the same words.
func (a *App) deliverThreadWakes(refs []threadWakeRef) {
	claimed := make([]threadWakeRef, 0, len(refs))
	for _, ref := range refs {
		if a.claimThreadWake(ref.token, ref.late) {
			claimed = append(claimed, ref)
		}
	}
	a.deliverClaimedThreadWakes(claimed)
}

// deliverClaimedThreadWakes hands over wakes whose claims its caller already
// holds, and ends the claims. The retry pass claims under the same settle
// lock it books the attempt under (claimThreadWakeRetry).
func (a *App) deliverClaimedThreadWakes(refs []threadWakeRef) {
	defer func() {
		for _, ref := range refs {
			a.endThreadWake(ref.token, ref.late)
		}
	}()
	a.deliverThreadWakeMessages(a.threadWakeMessages(refs))
}

// claimThreadWake marks one wake in flight under the token's settle lock, or
// reports that a tool call holds the token. The collector decides a wake is
// owed before the delivery starts, and a call can register in between; that
// call's reply carries the answer, and its release brings the retry pass back
// if the reply did not, so a wake queued now would hand the agent the answer
// twice. The mark is what a reply reads under the same lock
// (claimInlineDelivery) while the wake is still writing its message.
func (a *App) claimThreadWake(token string, late bool) bool {
	unlock := a.threadRequestSettleLock(token)
	defer unlock()
	if a.requestWaitActive(token) {
		return false
	}
	a.markThreadWakeInFlight(token, late)
	return true
}

// markThreadWakeInFlight is the claim itself. The caller holds the token's
// settle lock and has found no call holding the token.
func (a *App) markThreadWakeInFlight(token string, late bool) {
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	if a.threadRequests.wakesInFlight == nil {
		a.threadRequests.wakesInFlight = make(map[string]int)
	}
	a.threadRequests.wakesInFlight[threadWakeSendIDFor(token, late)]++
}

// endThreadWake clears the mark, under the settle lock so that a reply reads
// either the mark or the row the wake wrote, never neither.
func (a *App) endThreadWake(token string, late bool) {
	unlock := a.threadRequestSettleLock(token)
	defer unlock()
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	key := threadWakeSendIDFor(token, late)
	if a.threadRequests.wakesInFlight[key] <= 1 {
		delete(a.threadRequests.wakesInFlight, key)
		return
	}
	a.threadRequests.wakesInFlight[key]--
}

// threadWakeInFlight reports whether a wake for this answer is between its
// claim and its end. The caller holds the token's settle lock.
func (a *App) threadWakeInFlight(token string, late bool) bool {
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	return a.threadRequests.wakesInFlight[threadWakeSendIDFor(token, late)] > 0
}

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
// be unarchived, a render or a queue write that failed, a tool call that held
// the token and whose reply did not carry the answer. Nothing else revisits
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
	callers := make([]string, 0, len(rows))
	byCaller := make(map[string][]threadWakeRef, len(rows))
	for _, row := range rows {
		owed := undeliveredThreadWakes(row)
		if len(owed) == 0 {
			continue
		}
		claimed, abandon := a.claimThreadWakeRetry(row, owed, now)
		if abandon {
			a.abandonThreadWake(row)
			continue
		}
		if len(claimed) == 0 {
			continue
		}
		if _, seen := byCaller[row.CallerThreadID]; !seen {
			callers = append(callers, row.CallerThreadID)
		}
		for _, late := range claimed {
			byCaller[row.CallerThreadID] = append(byCaller[row.CallerThreadID], threadWakeRef{token: row.Token, late: late})
		}
	}
	// One delivery per caller thread: its wakes fold into as few messages as
	// they can, and a caller whose lock is held does not keep the others
	// waiting behind it.
	for _, caller := range callers {
		a.deliverClaimedThreadWakesDetached(byCaller[caller])
	}
}

// claimThreadWakeRetry decides one row's retry under its settle lock. It
// books the attempt and claims each wake still owed in one critical section,
// or, when a call holds the token, books nothing.
//
// A call registers its wait without the settle lock, so the section is what
// orders the two. A call registered before the claim holds the token: its
// reply carries the answer, and its release nudges the sweep, which finds the
// row due because nothing was booked. A call registered after the claim reads
// it under the same lock (claimInlineDelivery) and leaves the delivery to the
// wake. Booking first and claiming later let a call land in between: the wake
// backed off with the attempt spent, and the release's pass found the row not
// due until the backoff ran out.
//
// The attempt is booked before the delivery runs, so a delivery that fails,
// or that is still in flight when the next pass comes round, does not bring
// the row back at once.
func (a *App) claimThreadWakeRetry(row store.ThreadRequest, owed []bool, now time.Time) (claimed []bool, abandon bool) {
	unlock := a.threadRequestSettleLock(row.Token)
	defer unlock()
	stillOwed := make([]bool, 0, len(owed))
	for _, late := range owed {
		if a.finishThreadRequestCollection(row.Token, late) {
			stillOwed = append(stillOwed, late)
		}
	}
	// Read after the collection, so a call that registered during this
	// section holds the token as well as one that registered before it.
	if a.requestWaitActive(row.Token) {
		return nil, false
	}
	if row.WakeAttempts >= threadWakeRetryAttempts {
		return nil, true
	}
	attempts := row.WakeAttempts + 1
	booked, err := a.store.ScheduleThreadRequestWake(row.Token,
		now.Add(a.threadWakeRetryDelay(attempts)).UnixMilli(), attempts)
	if err != nil {
		logThreadRequestSweep("schedule the wake retry for "+row.Token, err)
		return nil, false
	}
	// The pass read the row before a delivery that has landed since, such as
	// the one a reminder fired in this pass starts.
	if !booked {
		return nil, false
	}
	for _, late := range stillOwed {
		a.markThreadWakeInFlight(row.Token, late)
	}
	return stillOwed, false
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
	for _, late := range []bool{false, true} {
		if row.WakeOwed(late) {
			owed = append(owed, late)
		}
	}
	return owed
}

// threadWakeRetryDelay doubles the base delay for each attempt already made,
// up to the ceiling. The base is threadWakeRetryBase, or the one an isolated
// test boot set (IsolationConfig.ThreadWakeRetryBase); the ceiling is
// threadWakeRetryCeiling times the base either way.
func (a *App) threadWakeRetryDelay(attempts int64) time.Duration {
	base := threadWakeRetryBase
	if a.wakeRetryBaseOverride > 0 {
		base = a.wakeRetryBaseOverride
	}
	ceiling := base * threadWakeRetryCeiling
	delay := base
	for range attempts - 1 {
		delay *= 2
		if delay >= ceiling {
			return ceiling
		}
	}
	return delay
}

// threadWakeRetryBase, threadWakeRetryCeiling (the cap, as a multiple of the
// base: ten minutes) and threadWakeRetryAttempts pace and bound the retry;
// threadRequestWakeRetryBatch bounds one pass. This is a recovery net behind
// a delivery that normally happens with the settlement, so it is slow and it
// ends.
const (
	threadWakeRetryBase         = 30 * time.Second
	threadWakeRetryCeiling      = 20
	threadWakeRetryAttempts     = 8
	threadRequestWakeRetryBatch = 32
)
