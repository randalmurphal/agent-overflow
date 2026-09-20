package app

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/store"
)

// The unattended half of the request ledger: what runs with no tool call in
// flight. One ticker fires due reminders, ages out collected answers and
// deletes rows past the retention floor; one boot sweep settles what a
// restart interrupted.
//
// It is deliberately one loop rather than three. Each pass is two indexed
// queries against tables that are empty on most computers, and a single
// goroutine is one thing to cancel and join at shutdown.

const (
	// threadRequestSweepFloor is the shortest gap between two passes, and
	// so the reminder clock's resolution. A reminder is a message to the
	// person's own agent, so a second of slack is invisible. It is a floor
	// rather than a tick: the sweep sleeps until the ledger's next due
	// moment, and the floor is what keeps work the pass could not finish
	// (a settlement the store refused) from spinning the loop.
	threadRequestSweepFloor = time.Second
	// threadRequestExpiryInterval is how often the retention work runs. It
	// writes, so it runs on its own much slower clock, and it is the
	// longest the sweep ever sleeps.
	threadRequestExpiryInterval = 10 * time.Minute
	// threadRequestPollBatch bounds one pass over the remote rows.
	threadRequestPollBatch = 32
	// threadRequestPollFanOut is how many destinations one pass visits at
	// once, the bound the remote-jobs watcher uses for the same reason.
	threadRequestPollFanOut = 4
	// threadPollNormalDelay, threadPollWaitingDelay and threadPollErrorDelay
	// are the poll cadence, the remote-watch table applied to requests: a
	// token a call is parked on is visited faster because that call is
	// paying for the latency, and a destination that failed is left alone
	// for long enough that an offline computer costs one call a half minute.
	threadPollNormalDelay  = 5 * time.Second
	threadPollWaitingDelay = 2 * time.Second
	threadPollErrorDelay   = 30 * time.Second
	// threadPollLateCap is the ceiling of the settled row's cadence. A
	// settled request is polled for one thing only, a late reply, and
	// nobody is waiting on it: the delay doubles from the normal one up to
	// this, so a request answered an hour ago costs a call every ten
	// minutes rather than every five seconds.
	threadPollLateCap = 10 * time.Minute
	// threadLateReplyWindow is how long a settled request is still polled
	// for a late reply when the destination reported no deadline of its
	// own. It matches the destination's answer hold.
	threadLateReplyWindow = 24 * time.Hour
	// threadPollCallTimeout bounds one status call. It reads rows and one
	// live state per open request and never starts work, so it sits well
	// below the call timeout a forwarded tool gets.
	threadPollCallTimeout = 30 * time.Second
)

// startThreadRequestSweeps arms the ticker. It is called once, from the
// unattended-work startup phase, beside the remote watches it is modelled on.
func (a *App) startThreadRequestSweeps() {
	a.threadRequests.mu.Lock()
	if a.threadRequests.nudge != nil {
		a.threadRequests.mu.Unlock()
		return
	}
	nudge := make(chan struct{}, 1)
	a.threadRequests.nudge = nudge
	a.threadRequests.mu.Unlock()

	a.threadRequestsWG.Add(1)
	go func() {
		defer a.threadRequestsWG.Done()
		a.runThreadRequestSweeps(nudge)
	}()
}

// nudgeThreadRequestSweep asks for a pass now. A reminder due sooner than the
// next tick would otherwise wait it out. Non-blocking: a pass already pending
// covers this one.
func (a *App) nudgeThreadRequestSweep() {
	a.threadRequests.mu.Lock()
	nudge := a.threadRequests.nudge
	a.threadRequests.mu.Unlock()
	if nudge == nil {
		return
	}
	select {
	case nudge <- struct{}{}:
	default:
	}
}

// runThreadRequestSweeps runs a pass whenever the ledger says one is due.
//
// The clock is the ledger's own: after every pass the store reports the
// earliest moment anything is scheduled, and the sweep sleeps until then,
// until the retention clock comes round, or until a nudge says a row was
// written. A computer with no requests wakes twice an hour rather than
// three thousand times, and a due reminder is still served to the second.
func (a *App) runThreadRequestSweeps(nudge <-chan struct{}) {
	done := a.lifeCtx().Done()
	lastExpiry := time.Now()
	for {
		if a.shuttingDown.Load() {
			return
		}
		now := time.Now()
		a.fireDueThreadReminders(now)
		a.pollRemoteThreadRequests(now)
		a.retryUndeliveredThreadWakes(now)
		if now.Sub(lastExpiry) >= threadRequestExpiryInterval {
			lastExpiry = now
			a.expireThreadRequests(now)
		}
		// Drained before the schedule is read, so a nudge raised by this
		// pass's own writes is answered by the read rather than by another
		// pass. A nudge that lands after the read interrupts the sleep.
		select {
		case <-nudge:
		default:
		}
		select {
		case <-done:
			return
		case <-nudge:
		case <-time.After(a.threadRequestSweepDelay(lastExpiry)):
		}
	}
}

// threadRequestSweepDelay is how long the sweep sleeps before its next pass:
// until the ledger's next due moment, bounded below by the floor and above
// by the retention clock.
//
// A poll already in flight is left out of the answer. Its rows are still due
// by their own column, because the pass that owns them has not rescheduled
// them yet, and sleeping on that would be sleeping for no time at all; the
// poll nudges the sweep when it is done. The reminders and wake retries
// beside it stay in, so a reminder due during a slow poll is not held to
// the poll's timeout.
func (a *App) threadRequestSweepDelay(lastExpiry time.Time) time.Duration {
	delay := threadRequestExpiryInterval - time.Since(lastExpiry)
	schedule, err := a.store.NextThreadRequestWork()
	if err != nil {
		logThreadRequestSweep("read the next scheduled request work", err)
	} else {
		due := []store.ScheduledMoment{schedule.Reminder, schedule.Wake}
		if !a.threadRequestPollInFlight() {
			due = append(due, schedule.Poll)
		}
		for _, moment := range due {
			if !moment.Set {
				continue
			}
			if until := time.Until(time.UnixMilli(moment.At)); until < delay {
				delay = until
			}
		}
	}
	if delay < threadRequestSweepFloor {
		return threadRequestSweepFloor
	}
	return delay
}

// pollRemoteThreadRequests collects the requests this computer's threads
// made on paired computers.
//
// One RPC per destination per tick carries every token due for it, so a
// thread with twenty open asks on one laptop costs one round trip. The
// reply is the destination's own receipt state, which the rules below map
// onto the source row; nothing here decides what a settlement means.
//
// The pass runs off the sweep's own goroutine because an unreachable
// computer holds the call open for its whole timeout, and the reminder
// clock beside it ticks every second. One pass at a time: the due rows of
// a pass still in flight have not been rescheduled yet, and a second pass
// would ask about them again.
func (a *App) pollRemoteThreadRequests(now time.Time) {
	if a.backends == nil || !a.beginThreadRequestPoll() {
		return
	}
	rows, err := a.store.DueThreadRequestPolls(now.UnixMilli(), threadRequestPollBatch)
	if err != nil {
		a.endThreadRequestPoll()
		logThreadRequestSweep("read due request polls", err)
		return
	}
	if len(rows) == 0 {
		a.endThreadRequestPoll()
		return
	}
	if a.lifeCtx().Err() != nil {
		a.endThreadRequestPoll()
		return
	}
	a.threadRequestsWG.Add(1)
	go func() {
		defer a.threadRequestsWG.Done()
		defer a.endThreadRequestPoll()
		a.runThreadRequestPoll(rows)
	}()
}

// beginThreadRequestPoll claims the single poll slot.
func (a *App) beginThreadRequestPoll() bool {
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	if a.threadRequests.polling {
		return false
	}
	a.threadRequests.polling = true
	return true
}

func (a *App) endThreadRequestPoll() {
	a.threadRequests.mu.Lock()
	a.threadRequests.polling = false
	a.threadRequests.mu.Unlock()
	// The rows this pass rescheduled are the sweep's next deadline, and the
	// sweep left them out of its schedule while the pass held them.
	a.nudgeThreadRequestSweep()
}

// threadRequestPollInFlight reports whether a poll pass holds the rows it
// read. The sweep's schedule leaves them out for as long as it does.
func (a *App) threadRequestPollInFlight() bool {
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	return a.threadRequests.polling
}

// deliverThreadWakeDetached hands one wake to the bounded set of goroutines
// the sweep delivers through.
//
// A delivery takes the caller thread's own lock and can start its session,
// which is arbitrarily slower than the pass that decided it was owed; done
// in line, one thread that is busy holds up every reminder behind it. It
// waits for a free slot rather than dropping the wake: the retry pass has
// already booked the attempt this delivery is, and a wake skipped here
// would spend that attempt without being tried.
func (a *App) deliverThreadWakeDetached(token string, late bool) {
	slots := a.threadWakeDeliverySlots()
	select {
	case slots <- struct{}{}:
	case <-a.lifeCtx().Done():
		return
	}
	a.threadRequestsWG.Add(1)
	go func() {
		defer a.threadRequestsWG.Done()
		defer func() { <-slots }()
		a.deliverThreadWake(token, late)
	}()
}

func (a *App) threadWakeDeliverySlots() chan struct{} {
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	if a.threadRequests.deliveries == nil {
		a.threadRequests.deliveries = make(chan struct{}, threadWakeDeliveryFanOut)
	}
	return a.threadRequests.deliveries
}

// threadWakeDeliveryFanOut is how many wakes the sweep hands over at once,
// the bound the poll fan-out uses for the same reason.
const threadWakeDeliveryFanOut = 4

// runThreadRequestPoll groups the due rows by destination and asks each one
// about its whole set.
func (a *App) runThreadRequestPoll(rows []store.ThreadRequest) {
	order := make([]string, 0, 4)
	byComputer := make(map[string][]store.ThreadRequest, 4)
	for _, row := range rows {
		if row.TargetComputerID == "" {
			// A row with no destination is not a remote request and the due
			// index should not have returned it. Take it out of the poll
			// rather than call a computer that was never named.
			logThreadRequestSweep("poll request "+row.Token,
				errors.New("the row names no destination computer"))
			a.rescheduleThreadRequest(row, threadPollErrorDelay, "This request names no destination computer.")
			continue
		}
		if _, seen := byComputer[row.TargetComputerID]; !seen {
			order = append(order, row.TargetComputerID)
		}
		byComputer[row.TargetComputerID] = append(byComputer[row.TargetComputerID], row)
	}
	// Four destinations at a time, the bound the remote-jobs watcher uses:
	// one computer that is asleep holds its call open for the whole call
	// timeout, and the healthy destinations beside it must not wait it out.
	var wg sync.WaitGroup
	limit := make(chan struct{}, threadRequestPollFanOut)
	for _, computerID := range order {
		select {
		case limit <- struct{}{}:
		case <-a.lifeCtx().Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func(computerID string) {
			defer wg.Done()
			defer func() { <-limit }()
			a.pollThreadRequestsOn(computerID, byComputer[computerID])
		}(computerID)
	}
	wg.Wait()
}

// pollThreadRequestsOn asks one destination about every token due for it and
// applies the answers.
func (a *App) pollThreadRequestsOn(computerID string, rows []store.ThreadRequest) {
	if len(rows) > threadPeerTokenLimit {
		rows = rows[:threadPeerTokenLimit]
	}
	tokens := make([]string, 0, len(rows))
	for _, row := range rows {
		tokens = append(tokens, row.Token)
	}
	reply, err := a.callThreadRequestStatus(computerID, ThreadPeerPoll{Tokens: tokens})
	if err != nil {
		a.failThreadRequestPoll(computerID, rows, err)
		return
	}
	answers := make(map[string]ThreadPeerRequest, len(reply.Requests))
	for _, answer := range reply.Requests {
		answers[answer.Token] = answer
	}
	acks := make([]ThreadPeerAck, 0, len(rows))
	for _, row := range rows {
		answer, found := answers[row.Token]
		if !found {
			// The destination answered without this token. Nothing is known
			// about it, so it waits for the next pass rather than being
			// settled on an answer nobody gave.
			a.rescheduleThreadRequest(row, threadPollErrorDelay, "The other computer did not report this request.")
			continue
		}
		revision, err := a.applyThreadPeerRequest(row.Token, computerID, answer)
		if err != nil {
			logThreadRequestSweep("apply request "+row.Token, err)
			a.rescheduleThreadRequest(row, threadPollErrorDelay, err.Error())
			continue
		}
		if revision > 0 {
			acks = append(acks, ThreadPeerAck{Token: row.Token, Revision: revision})
		}
		a.scheduleNextThreadPoll(row)
	}
	if len(acks) == 0 {
		return
	}
	// The acknowledgement rides its own call: a row that just settled leaves
	// the due set, so there is no later poll of it to carry the ack, and the
	// destination may not drop an answer it has not been told was stored.
	if _, err := a.callThreadRequestStatus(computerID, ThreadPeerPoll{Ack: acks}); err != nil {
		logThreadRequestSweep("acknowledge collected requests on "+computerID, err)
	}
}

func (a *App) callThreadRequestStatus(computerID string, poll ThreadPeerPoll) (ThreadPeerPollReply, error) {
	ctx, cancel := context.WithTimeout(a.lifeCtx(), threadPollCallTimeout)
	defer cancel()
	var reply ThreadPeerPollReply
	if err := a.backends.CallThreadPeer(ctx, computerID, "ThreadToolRequestStatus", &reply, poll); err != nil {
		return ThreadPeerPollReply{}, a.threadOperationError("status", computerID, "", err)
	}
	return reply, nil
}

// failThreadRequestPoll answers one unreachable or refusing destination.
//
// A pairing that has ended is terminal: the requests against it can never
// be collected, so they settle rather than being retried forever. Anything
// else is a computer that is asleep, offline or busy, which is what the
// error backoff is for.
func (a *App) failThreadRequestPoll(computerID string, rows []store.ThreadRequest, cause error) {
	code, message, _ := threadErrorDetails("status", cause)
	if !threadPairingEnded(code) {
		for _, row := range rows {
			a.rescheduleThreadRequest(row, threadPollErrorDelay, message)
		}
		return
	}
	log.Printf("thread tools: pairing with %s ended; settling %d open request(s)", computerID, len(rows))
	for _, row := range rows {
		if err := a.settleRemoteThreadRequest(row, store.ThreadRequestSettlement{
			State:      store.ThreadRequestErrored,
			Answer:     []byte(message),
			AnswerKind: store.ThreadAnswerError,
		}); err != nil {
			logThreadRequestSweep("settle request "+row.Token+" after pairing ended", err)
		}
	}
}

// threadPollDelay is the cadence for one token: faster while a call is
// parked on it, because that call is paying for the latency.
func (a *App) threadPollDelay(token string) time.Duration {
	if a.requestWaitActive(token) {
		return threadPollWaitingDelay
	}
	return threadPollNormalDelay
}

func (a *App) rescheduleThreadRequest(row store.ThreadRequest, delay time.Duration, issue string) {
	attempts := row.Attempts + 1
	if issue == "" {
		attempts = 0
	}
	a.rescheduleThreadRequestAt(row.Token, delay, attempts, issue)
}

func (a *App) rescheduleThreadRequestAt(token string, delay time.Duration, attempts int64, issue string) {
	if err := a.store.RescheduleThreadRequest(token, time.Now().Add(delay).UnixMilli(), attempts, issue); err != nil {
		logThreadRequestSweep("reschedule request "+token, err)
		return
	}
	// The sweep sleeps on the ledger's schedule, and this row just changed
	// it. A reschedule from a dispatching call is the one that matters: it
	// is the first poll of a request made while nothing else was due.
	a.nudgeThreadRequestSweep()
}

// scheduleNextThreadPoll decides when the poller returns to one row it has
// just heard about, and retires it when there is nothing left to hear.
//
// An open request keeps the cadence its caller is paying for. A settled one
// is visited for one thing only, a late `thread_reply`, so its delay doubles
// towards the ten-minute ceiling and it leaves the poll for good once the
// late reply lands or the destination's hold on the request runs out.
func (a *App) scheduleNextThreadPoll(row store.ThreadRequest) {
	// Re-read: the answer this poll applied may have settled the row, and
	// the schedule a settled row gets is not the one it had.
	current, found, err := a.store.GetThreadRequest(row.Token)
	if err != nil {
		logThreadRequestSweep("read request "+row.Token+" to reschedule", err)
		return
	}
	if !found || !current.Polling {
		return
	}
	if !threadRequestSettled(current) {
		a.rescheduleThreadRequest(current, a.threadPollDelay(current.Token), "")
		return
	}
	if !threadLateReplyAwaited(current, time.Now()) {
		a.retireThreadRequestPoll(current.Token)
		return
	}
	// `attempts` counts the consecutive polls that reported nothing new,
	// which is what an error reschedule counts too, so a destination that
	// is also unreachable backs off on one clock rather than two.
	attempts := current.Attempts + 1
	a.rescheduleThreadRequestAt(current.Token, threadLatePollDelay(attempts), attempts, "")
}

// threadLateReplyAwaited reports whether a settled request could still be
// given a late reply by its destination: it has none yet, and the
// destination's hold on the answer has not run out.
func threadLateReplyAwaited(row store.ThreadRequest, now time.Time) bool {
	if len(row.LateReply) > 0 || row.State != store.ThreadRequestFinished {
		// Only a finished request takes a late reply; every other
		// settlement is the last word on it.
		return false
	}
	deadline := row.ExpiresAt
	if deadline == 0 {
		// A destination that reported no deadline still holds the answer
		// for its own hold, which is what this one assumes.
		deadline = row.SettledAt + threadLateReplyWindow.Milliseconds()
	}
	return now.UnixMilli() < deadline
}

// threadLatePollDelay doubles the normal cadence for each poll that reported
// nothing new, up to the ceiling.
func threadLatePollDelay(attempts int64) time.Duration {
	delay := threadPollNormalDelay
	for range attempts {
		delay *= 2
		if delay >= threadPollLateCap {
			return threadPollLateCap
		}
	}
	return delay
}

func (a *App) retireThreadRequestPoll(token string) {
	if _, err := a.store.RetireThreadRequestPoll(token); err != nil {
		logThreadRequestSweep("retire request "+token+" from the poll", err)
	}
}

// applyThreadPeerRequest writes what the destination reported onto the
// source row and returns the revision to acknowledge, zero when nothing was
// collected.
//
// It is the one place a peer's answer becomes this computer's record, so
// the confirmation a spawn, send or ask gets in its own reply and the one
// the poller gets a tick later cannot disagree.
func (a *App) applyThreadPeerRequest(token, computerID string, answer ThreadPeerRequest) (int64, error) {
	row, found, err := a.store.GetThreadRequest(token)
	if err != nil || !found {
		return 0, err
	}
	if !answer.Known {
		return 0, a.settleUnknownRemoteRequest(row)
	}
	a.noteRemoteRequestBlocked(token, answer.Blocked)
	if target := threadPeerTarget(row, answer); target != "" {
		if _, err := a.store.SetThreadRequestTarget(token, computerID, target, answer.Title); err != nil {
			return 0, err
		}
	}
	if !threadReceiptSettledState(answer.State) {
		// Still accepted or running there. The source row follows so
		// thread_status reads the same word on both computers.
		if answer.State == store.ThreadReceiptRunning {
			if _, err := a.store.AdvanceThreadRequestState(token, store.ThreadRequestUnconfirmed, store.ThreadRequestAccepted); err != nil {
				return 0, err
			}
			if _, err := a.store.AdvanceThreadRequestState(token, store.ThreadRequestAccepted, store.ThreadRequestRunning); err != nil {
				return 0, err
			}
			return 0, nil
		}
		if _, err := a.store.AdvanceThreadRequestState(token, store.ThreadRequestUnconfirmed, store.ThreadRequestAccepted); err != nil {
			return 0, err
		}
		return 0, nil
	}
	if threadRequestSettled(row) && answer.Revision <= row.Revision {
		// Already collected. The row stays in the poll only because a
		// `finished` request can still take a late reply, and a settled
		// request blocks nobody.
		a.forgetRemoteRequestBlocked(token)
		return 0, nil
	}
	return a.collectRemoteThreadRequest(row, answer)
}

// threadPeerTarget reports the target thread to record for one answer, empty
// when the row already names it under the name the destination gave it.
//
// The title is recorded beside the id because a thread on another computer
// has no row here: it is what names the target in a wake, in the origin chip
// on that wake, and in `thread_status`, including after a restart, which the
// live reading beside it does not survive.
func threadPeerTarget(row store.ThreadRequest, answer ThreadPeerRequest) string {
	target := answer.TargetThreadID
	if target == "" {
		target = row.TargetThreadID
	}
	switch {
	case target == "":
		return ""
	case target != row.TargetThreadID:
		// The destination named its thread, or moved the request to
		// another one.
		return target
	case answer.Title != "" && answer.Title != row.TargetThreadTitle:
		// Renamed there. An answer that carries no title at all leaves the
		// name this row already holds alone: a destination that has
		// deleted the thread still owes the answer a name.
		return target
	}
	return ""
}

// settleUnknownRemoteRequest answers a token the destination does not hold.
//
// Before acceptance that means the request never landed there, which is a
// refusal. After it, the destination lost a receipt it had: swept past the
// retention floor, or restored from a backup. Neither can be collected, and
// the two read differently to the agent that is waiting.
func (a *App) settleUnknownRemoteRequest(row store.ThreadRequest) error {
	if threadRequestSettled(row) {
		// Nothing left to settle, and nothing left to ask: a destination
		// that no longer holds the receipt cannot take the late reply this
		// row was still being polled for.
		a.retireThreadRequestPoll(row.Token)
		return nil
	}
	settlement := store.ThreadRequestSettlement{
		State:      store.ThreadRequestErrored,
		Answer:     []byte("The other computer no longer knows this request."),
		AnswerKind: store.ThreadAnswerError,
	}
	if row.State == store.ThreadRequestUnconfirmed {
		settlement.State = store.ThreadRequestRefused
		settlement.Answer = []byte("The other computer never accepted this request.")
	}
	return a.settleRemoteThreadRequest(row, settlement)
}

// collectRemoteThreadRequest copies one destination settlement onto the
// source row and hands it to the caller, exactly as the local collector
// does for a request that ran here.
//
// The settle lock covers the durable write and the delivery decision; the
// wakes it decides are delivered after it is released, because a wake takes
// the caller thread's own locks and can start its session.
func (a *App) collectRemoteThreadRequest(row store.ThreadRequest, answer ThreadPeerRequest) (int64, error) {
	wake, lateWake, err := a.collectRemoteThreadRequestLocked(row, answer)
	if err != nil {
		return 0, err
	}
	if wake {
		a.deliverThreadWake(row.Token, false)
	}
	if lateWake {
		a.deliverThreadWake(row.Token, true)
	}
	a.forgetRemoteRequestBlocked(row.Token)
	return answer.Revision, nil
}

// collectRemoteThreadRequestLocked is the locked half of the remote
// collection. It reports which of the row's two wakes it decided are owed.
func (a *App) collectRemoteThreadRequestLocked(row store.ThreadRequest, answer ThreadPeerRequest) (wake, lateWake bool, err error) {
	unlock := a.threadRequestSettleLock(row.Token)
	defer unlock()
	// Re-read under the lock: a cancel on this computer can settle the row
	// between the poll's read and this write.
	current, found, err := a.store.GetThreadRequest(row.Token)
	if err != nil || !found {
		return false, false, err
	}
	if !threadRequestSettled(current) {
		settled, err := a.store.SettleThreadRequest(row.Token, store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
			State:      threadRequestStateForReceipt(answer.State),
			Answer:     []byte(answer.Answer),
			AnswerKind: answer.AnswerKind,
			SettledAt:  answer.SettledAt,
			ExpiresAt:  answer.ExpiresAt,
		})
		if err != nil {
			return false, false, err
		}
		if settled {
			wake = a.finishThreadRequestCollection(row.Token, false)
		}
	}
	if answer.LateReply != "" && len(current.LateReply) == 0 {
		stored, err := a.store.StoreThreadRequestLateReply(row.Token, []byte(answer.LateReply), answer.LateReplyAt)
		if err != nil {
			return wake, false, err
		}
		if stored {
			lateWake = a.finishThreadRequestCollection(row.Token, true)
		}
	}
	return wake, lateWake, nil
}

// settleRemoteThreadRequest settles a source row for a reason the
// destination gave rather than an answer it produced, and delivers it. The
// wake is delivered after the settle lock is released.
func (a *App) settleRemoteThreadRequest(row store.ThreadRequest, settlement store.ThreadRequestSettlement) error {
	wake, err := func() (bool, error) {
		unlock := a.threadRequestSettleLock(row.Token)
		defer unlock()
		settled, err := a.store.SettleThreadRequest(row.Token, store.ThreadRequestOpenStates(), settlement)
		if err != nil || !settled {
			return false, err
		}
		return a.finishThreadRequestCollection(row.Token, false), nil
	}()
	if err != nil {
		return err
	}
	if wake {
		a.deliverThreadWake(row.Token, false)
	}
	a.forgetRemoteRequestBlocked(row.Token)
	return nil
}

// threadReceiptSettledState reports whether a destination state is one the
// source can collect. It is the receipt's own vocabulary, which differs
// from the source's only in the two words a destination never uses.
func threadReceiptSettledState(state string) bool {
	switch state {
	case "", store.ThreadReceiptAccepted, store.ThreadReceiptRunning:
		return false
	}
	return true
}

// expireThreadRequests is the retention pass: answers nobody collected are
// dropped a day after they were written, rows are deleted at the floor, and
// answer files nobody read go with them.
func (a *App) expireThreadRequests(now time.Time) {
	if dropped, err := a.store.ExpireThreadRequestAnswers(now.UnixMilli()); err != nil {
		logThreadRequestSweep("expire answers", err)
	} else if dropped > 0 {
		log.Printf("thread tools: dropped %d uncollected answer(s) past their hold", dropped)
	}
	floor := now.AddDate(0, 0, -store.ThreadRequestRetentionDays).UnixMilli()
	if deleted, err := a.store.DeleteThreadRequestsBefore(floor); err != nil {
		logThreadRequestSweep("delete requests past retention", err)
	} else if deleted > 0 {
		log.Printf("thread tools: deleted %d request row(s) past the %d-day floor", deleted, store.ThreadRequestRetentionDays)
	}
	a.sweepThreadExports(now)
}

// sweepThreadExports deletes the two kinds of export file nobody keeps: an
// answer a day after it was written, and a transcript rendered for a paired
// computer a day after it was rendered.
//
// The tool tells the model to read an answer file in the same reply, so one
// still there a day later was never read. A peer export is a copy awaiting
// a transfer that either happened long ago or never will. A transcript this
// computer's own agent asked for is a person's artefact and is left alone.
func (a *App) sweepThreadExports(now time.Time) {
	if a.configDir == "" {
		return
	}
	dir := filepath.Join(a.configDir, threadExportDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			logThreadRequestSweep("read export directory", err)
		}
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || (!strings.HasPrefix(name, threadAnswerExportPrefix) && !strings.HasPrefix(name, threadPeerExportPrefix)) {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < threadRequestExportAge {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			logThreadRequestSweep("remove stale export file", err)
		}
	}
}

// threadAnswerExportPrefix distinguishes one request's answer from a whole
// transcript in the same directory.
const threadAnswerExportPrefix = "answer-"

// sweepThreadRequestsAtBoot settles what the last run left open.
//
// A receipt in `accepted` or `running` describes work that was either queued
// and never sent, or running in a process that is gone. Neither can settle
// itself any more, so both settle here as `interrupted` and their callers are
// told. The scratch threads go too: an ask's fork exists only to answer one
// request, and every request it could answer has just been settled.
func (a *App) sweepThreadRequestsAtBoot() {
	receipts, err := a.store.ListOpenThreadRequestReceipts()
	if err != nil {
		logThreadRequestSweep("list open receipts at boot", err)
	}
	for _, receipt := range receipts {
		if err := a.settleThreadReceipt(receipt.Token, store.ThreadReceiptOpenStates(), store.ThreadRequestSettlement{
			State:      store.ThreadReceiptInterrupted,
			Answer:     []byte("This computer restarted before the request finished."),
			AnswerKind: store.ThreadAnswerError,
		}); err != nil {
			logThreadRequestSweep("settle interrupted receipt "+receipt.Token, err)
		}
	}
	// A wake the last run settled but never handed over is owed from the
	// moment this one starts, not at the first retry tick.
	a.retryUndeliveredThreadWakes(time.Now())
	rows, err := a.store.ListScratchThreads()
	if err != nil {
		logThreadRequestSweep("list scratch threads at boot", err)
		return
	}
	for _, row := range rows {
		if err := a.DeleteThread(row.ThreadID); err != nil {
			logThreadRequestSweep("delete scratch thread "+row.ThreadID, err)
			continue
		}
		if _, err := a.store.DeleteScratchThread(row.ThreadID); err != nil {
			logThreadRequestSweep("delete scratch row "+row.ThreadID, err)
		}
	}
}

// logThreadRequestSweep reports what an unattended pass could not do. Nothing
// here has a caller to return an error to, and a silent sweep failure is a
// request that never settles.
func logThreadRequestSweep(what string, err error) {
	if err == nil {
		return
	}
	log.Printf("thread tools sweep: %s: %v", what, err)
}
