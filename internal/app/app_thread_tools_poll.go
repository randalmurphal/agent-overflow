package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/agenthome"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// The unattended half of the request ledger: what runs with no tool call in
// flight. One ticker fires due reminders and the loops and watches of the
// wake supervisor (app_agent_wake_runs.go), ages out collected answers and
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
	// restRecoveryRetry is how soon the sweep tries again what the boot's
	// recovery of rest could not read (retryRestRecovery).
	restRecoveryRetry = 5 * time.Second
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

	// Refused only once shutdown has joined the request work.
	a.threadRequestsWG.Go(func() {
		// Before the first pass, so a fire missed while the app was closed
		// replays once with its count rather than as an ordinary due fire,
		// and a row a quit left running is recorded before anything claims
		// it.
		a.recoverAgentWakesAtBoot()
		// The async questions a restart left open go to their threads'
		// spawners again, and their threads read as waiting on them.
		a.seedAsyncQuestionPrompts()
		// The workers that wait on what survived the restart, recorded
		// once the loops and watches are recovered; then the requests held
		// for their threads' rest across the restart, those at rest now
		// settling.
		a.recoverRestAtBoot()
		a.runThreadRequestSweeps(nudge)
	})
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
		// What the boot's recovery of rest could not read, read again.
		a.retryRestRecovery()
		a.fireDueThreadReminders(now)
		a.fireDueAgentWakes(now)
		a.pollRemoteThreadRequests(now)
		a.refreshNestedOpenRemoteSpawns()
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
		if schedule.NestedOpen {
			// A settled spawn on another computer whose work there is still
			// open: its rows are asked for again at the poll's pace.
			due = append(due, store.ScheduledMoment{At: time.Now().Add(a.threadPollNormal()).UnixMilli(), Set: true})
		}
		if schedule.RestingRemote {
			// Settled spawns on other computers at rest: the next page of
			// them is asked about at the rest pass's pace.
			due = append(due, store.ScheduledMoment{At: a.nextRestPass().UnixMilli(), Set: true})
		}
		if !a.threadRequestPollInFlight() {
			due = append(due, schedule.Poll)
		}
		if a.restRecoveryPending() {
			due = append(due, store.ScheduledMoment{At: time.Now().Add(restRecoveryRetry).UnixMilli(), Set: true})
		}
		if loops, err := a.store.NextAgentWakeDue(); err != nil {
			logThreadRequestSweep("read the next loop or watch fire", err)
		} else {
			due = append(due, loops)
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
	a.startThreadRequestPoll(rows)
}

// startThreadRequestPoll runs one pass over rows off the sweep, releasing
// the poll slot when it ends or when shutdown refuses it.
func (a *App) startThreadRequestPoll(rows []store.ThreadRequest) {
	if a.lifeCtx().Err() != nil || !a.threadRequestsWG.Go(func() {
		defer a.endThreadRequestPoll()
		a.runThreadRequestPoll(rows)
	}) {
		a.endThreadRequestPoll()
	}
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
// already booked the attempt a claimed delivery is, and a wake skipped here
// would spend that attempt without being tried.
func (a *App) deliverThreadWakeDetached(token string, late bool) {
	a.runThreadWakeDetached(func() { a.deliverThreadWake(token, late) }, func() {})
}

// deliverClaimedThreadWakesDetached is the same for the wakes of one caller
// thread the retry pass has claimed. The claims are held while the wakes wait
// for a slot, so a reply in that time says the message is on its way, and
// they end if the app stops first.
func (a *App) deliverClaimedThreadWakesDetached(refs []threadWakeRef) {
	a.runThreadWakeDetached(func() { a.deliverClaimedThreadWakes(refs) }, func() {
		for _, ref := range refs {
			a.endThreadWake(ref.token, ref.late)
		}
	})
}

func (a *App) runThreadWakeDetached(deliver, dropped func()) {
	// Checked before the slot: once shutdown has begun the select below
	// could still pick a free slot over Done.
	if a.lifeCtx().Err() != nil {
		dropped()
		return
	}
	slots := a.threadWakeDeliverySlots()
	select {
	case slots <- struct{}{}:
	case <-a.lifeCtx().Done():
		dropped()
		return
	}
	if !a.goThreadRequestSettling(func() {
		defer func() { <-slots }()
		deliver()
	}) {
		<-slots
		dropped()
	}
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
	// What a spawn there runs, its own agents included, changes with no
	// frame here: each poll of one asks for its Agents rows again, at the
	// poll's own pace, and an answer that changed emits a frame.
	for _, row := range rows {
		if row.Kind == store.ThreadRequestSpawn && row.TargetComputerID != "" {
			a.refreshPeerAgentRows(row, 0)
		}
	}
}

// pollThreadRequestsOn asks one destination about every token due for it,
// and about the other requests the same threads still have open there, and
// applies the answers.
//
// Those ride along because each row keeps its own clock: two requests a
// thread made seconds apart would otherwise be asked about in different
// passes for as long as they ran, and answers one turn or one restart wrote
// there would arrive here as separate messages. Asked together, they are
// collected together and told to their caller once.
func (a *App) pollThreadRequestsOn(computerID string, rows []store.ThreadRequest) {
	if len(rows) > threadPeerTokenLimit {
		rows = rows[:threadPeerTokenLimit]
	}
	rows = a.withOpenThreadRequestsOn(computerID, rows)
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
	var wakes []threadWakeRef
	for _, row := range rows {
		answer, found := answers[row.Token]
		if !found {
			// The destination answered without this token. Nothing is known
			// about it, so it waits for the next pass rather than being
			// settled on an answer nobody gave.
			a.rescheduleThreadRequest(row, threadPollErrorDelay, "The other computer did not report this request.")
			continue
		}
		revision, owed, err := a.applyThreadPeerRequest(row.Token, computerID, answer)
		wakes = append(wakes, owed...)
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
	// Together, as a turn end on this computer delivers them: one turn there
	// that answered several requests is one message here.
	a.deliverThreadWakes(wakes)
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

// withOpenThreadRequestsOn adds, to the requests due on a computer, the
// other requests their callers still have open there, up to the limit one
// status call carries. Another thread's requests keep their own clock: a
// wake folds per caller, so asking about them early would buy nothing. A
// read that fails costs only the batching: the due rows are polled as they
// are.
func (a *App) withOpenThreadRequestsOn(computerID string, due []store.ThreadRequest) []store.ThreadRequest {
	room := threadPeerTokenLimit - len(due)
	if room <= 0 {
		return due
	}
	polled := make(map[string]struct{}, len(due))
	var callers []string
	for _, row := range due {
		polled[row.Token] = struct{}{}
		if !slices.Contains(callers, row.CallerThreadID) {
			callers = append(callers, row.CallerThreadID)
		}
	}
	// Every due row can also be open, so the read asks for enough to fill
	// the room after they are skipped.
	open, err := a.store.OpenThreadRequestPollsOn(computerID, callers, threadPeerTokenLimit)
	if err != nil {
		logThreadRequestSweep("read open requests on "+computerID, err)
		return due
	}
	for _, row := range open {
		if room == 0 {
			break
		}
		if _, seen := polled[row.Token]; seen {
			continue
		}
		due = append(due, row)
		room--
	}
	return due
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
	var wakes []threadWakeRef
	for _, row := range rows {
		owed, err := a.settleRemoteThreadRequestOwing(row, store.ThreadRequestSettlement{
			State:      store.ThreadRequestErrored,
			Answer:     []byte(message),
			AnswerKind: store.ThreadAnswerError,
		})
		wakes = append(wakes, owed...)
		if err != nil {
			logThreadRequestSweep("settle request "+row.Token+" after pairing ended", err)
		}
	}
	a.deliverThreadWakes(wakes)
}

// threadPollDelay is the cadence for one token: faster while a call is
// parked on it, because that call is paying for the latency.
func (a *App) threadPollDelay(token string) time.Duration {
	normal := a.threadPollNormal()
	if a.requestWaitActive(token) {
		return min(threadPollWaitingDelay, normal)
	}
	return normal
}

// threadPollNormal is threadPollNormalDelay, or the shorter cadence an
// isolated test boot set (IsolationConfig.ThreadRequestPoll).
func (a *App) threadPollNormal() time.Duration {
	if a.threadPollOverride > 0 {
		return a.threadPollOverride
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
	a.rescheduleThreadRequestAt(current.Token, threadLatePollDelay(a.threadPollNormal(), attempts), attempts, "")
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
func threadLatePollDelay(normal time.Duration, attempts int64) time.Duration {
	delay := normal
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
// collected, and the wakes the write owes its caller thread. The caller
// delivers those, with the wakes of whatever else it applied in the same pass.
//
// It is the one place a peer's answer becomes this computer's record, so
// the confirmation a spawn, send or ask gets in its own reply and the one
// the poller gets a tick later cannot disagree.
func (a *App) applyThreadPeerRequest(token, computerID string, answer ThreadPeerRequest) (int64, []threadWakeRef, error) {
	row, found, err := a.store.GetThreadRequest(token)
	if err != nil || !found {
		return 0, nil, err
	}
	if !answer.Known {
		wakes, err := a.settleUnknownRemoteRequest(row)
		return 0, wakes, err
	}
	a.noteRemoteRequestBlocked(token, remoteBlocked{
		blocked: answer.Blocked, routedTo: answer.BlockedRoutedTo, routedToCaller: answer.BlockedRoutedToCaller, waitingOn: answer.WaitingOn,
	})
	if target := threadPeerTarget(row, answer); target != "" {
		if _, err := a.store.SetThreadRequestTarget(token, computerID, target, answer.Title, answer.Provider); err != nil {
			return 0, nil, err
		}
	}
	// Whichever answer first states them, the call's own reply or a poll
	// that recovered a lost one: the destination's receipt recorded them
	// once, when it named the thread.
	if err := a.store.RecordThreadRequestInstructions(token, answer.Instructions); err != nil {
		return 0, nil, err
	}
	if !store.ThreadReceiptSettled(answer.State) {
		// Still accepted or running there. The source row follows so
		// thread_status reads the same word on both computers.
		if answer.State == store.ThreadReceiptRunning {
			if _, err := a.store.AdvanceThreadRequestState(token, store.ThreadRequestUnconfirmed, store.ThreadRequestAccepted); err != nil {
				return 0, nil, err
			}
			if _, err := a.store.AdvanceThreadRequestState(token, store.ThreadRequestAccepted, store.ThreadRequestRunning); err != nil {
				return 0, nil, err
			}
			return 0, nil, nil
		}
		if _, err := a.store.AdvanceThreadRequestState(token, store.ThreadRequestUnconfirmed, store.ThreadRequestAccepted); err != nil {
			return 0, nil, err
		}
		return 0, nil, nil
	}
	if threadRequestSettled(row) && answer.Revision <= row.Revision {
		// Already collected. The row stays in the poll only because a
		// `finished` request can still take a late reply, and a settled
		// request blocks nobody. A destination still waiting for the
		// acknowledgement lost the one sent after the collection, so it is
		// sent again: the row holds every revision up to its own.
		a.forgetRemoteRequestBlocked(token)
		if answer.CollectedRevision < answer.Revision {
			return answer.Revision, nil, nil
		}
		return 0, nil, nil
	}
	return a.collectRemoteThreadRequest(row, answer)
}

// collectThreadPeerReply applies an answer that came back on a call's own
// reply rather than from the poller, and acknowledges what it collected.
// The ack cannot wait for a poll: a row this answer settled is polled at
// most once more, and that poll finds it already collected, so nothing
// would tell the destination the answer was stored.
func (a *App) collectThreadPeerReply(token, computerID string, answer ThreadPeerRequest) error {
	revision, wakes, err := a.applyThreadPeerRequest(token, computerID, answer)
	a.deliverThreadWakes(wakes)
	if err != nil || revision == 0 {
		return err
	}
	ack := ThreadPeerPoll{Ack: []ThreadPeerAck{{Token: token, Revision: revision}}}
	if _, err := a.callThreadRequestStatus(computerID, ack); err != nil {
		logThreadRequestSweep("acknowledge collected request "+token+" on "+computerID, err)
	}
	return nil
}

// threadPeerTarget reports the target thread to record for one answer, empty
// when the row already names it under the name the destination gave it.
//
// The title is recorded beside the id because a thread on another computer
// has no row here: it is what names the target in a wake, as the sender
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
func (a *App) settleUnknownRemoteRequest(row store.ThreadRequest) ([]threadWakeRef, error) {
	if threadRequestSettled(row) {
		// Nothing left to settle, and nothing left to ask: a destination
		// that no longer holds the receipt cannot take the late reply this
		// row was still being polled for.
		a.retireThreadRequestPoll(row.Token)
		return nil, nil
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
	return a.settleRemoteThreadRequestOwing(row, settlement)
}

// collectRemoteThreadRequest copies one destination answer onto the source
// row, settlement and late reply together, and returns the revision to
// acknowledge and the wakes it owes the caller thread.
//
// The settle lock covers the durable write and the delivery decision; the
// wakes it decides are delivered by the caller after it is released, because
// a wake takes the caller thread's own locks and can start its session.
func (a *App) collectRemoteThreadRequest(row store.ThreadRequest, answer ThreadPeerRequest) (int64, []threadWakeRef, error) {
	wake, late, err := a.collectRemoteThreadRequestLocked(row, answer)
	if err != nil {
		return 0, nil, err
	}
	a.forgetRemoteRequestBlocked(row.Token)
	var wakes []threadWakeRef
	if wake {
		wakes = append(wakes, threadWakeRef{token: row.Token, late: late})
	}
	return answer.Revision, wakes, nil
}

// collectRemoteThreadRequestLocked is the locked half of the remote
// collection. It reports the wake it decided is owed.
func (a *App) collectRemoteThreadRequestLocked(row store.ThreadRequest, answer ThreadPeerRequest) (wake, late bool, err error) {
	unlock := a.threadRequestSettleLock(row.Token)
	defer unlock()
	// Re-read under the lock: the wake decision reads `notify`, which a
	// cancel or an archive on this computer can change after the poll's read.
	current, found, err := a.store.GetThreadRequest(row.Token)
	if err != nil || !found {
		return false, false, err
	}
	replyArms := (answer.AnswerKind == store.ThreadAnswerReply || answer.LateReply != "") && a.replyArmsWake(current)
	restUnreported, err := a.remoteRestUnreported(current, answer)
	if err != nil {
		return false, false, err
	}
	collected, err := a.store.CollectThreadRequest(row.Token, store.ThreadReceiptAnswer{
		RestUnreported: restUnreported,
		Revision:       answer.Revision,
		State:          answer.State,
		Answer:         []byte(answer.Answer),
		AnswerKind:     answer.AnswerKind,
		SettledAt:      answer.SettledAt,
		ExpiresAt:      answer.ExpiresAt,
		Agent:          remoteAnswerAgent(answer.AnswerAgent),
		LateReply:      []byte(answer.LateReply),
		LateReplyAt:    answer.LateReplyAt,
		LateReplyAgent: remoteAnswerAgent(answer.LateReplyAgent),
	}, store.ThreadRequestWake{ReplyArms: replyArms})
	if err != nil {
		return false, false, err
	}
	wake, late = a.collectionWake(row.Token, collected)
	return wake, late, nil
}

// remoteRestUnreported reports whether answer settles row `finished` from
// a computer that cannot report its thread's rest (ThreadPeerRequest.RestRule)
// for a request that waits for that rest here: a spawn, or a send to a
// thread its caller spawned there.
func (a *App) remoteRestUnreported(row store.ThreadRequest, answer ThreadPeerRequest) (bool, error) {
	if answer.RestRule || answer.State != store.ThreadReceiptFinished || answer.AnswerKind != store.ThreadAnswerFinal {
		return false, nil
	}
	switch row.Kind {
	case store.ThreadRequestSpawn:
		return true, nil
	case store.ThreadRequestSend:
		return a.store.CallerSpawnedRemote(row.CallerThreadID, row.TargetComputerID, row.TargetThreadID)
	default:
		return false, nil
	}
}

// remoteAnswerAgent is the standing agent another computer reported as
// answering, its role bounded as a footer's carried role is.
func remoteAnswerAgent(agent *ThreadPeerAgent) *store.ThreadAnswerAgent {
	if agent == nil {
		return nil
	}
	return &store.ThreadAnswerAgent{Role: agenthome.BoundRole(agent.Role)}
}

// settleRemoteThreadRequest settles a source row for a reason the
// destination gave rather than an answer it produced, and delivers it. The
// wake is delivered after the settle lock is released.
func (a *App) settleRemoteThreadRequest(row store.ThreadRequest, settlement store.ThreadRequestSettlement) error {
	wakes, err := a.settleRemoteThreadRequestOwing(row, settlement)
	a.deliverThreadWakes(wakes)
	return err
}

// settleRemoteThreadRequestOwing is settleRemoteThreadRequest for a caller
// settling several rows in one pass: it returns the wake the settlement owes
// instead of delivering it, so the pass delivers them together.
func (a *App) settleRemoteThreadRequestOwing(row store.ThreadRequest, settlement store.ThreadRequestSettlement) ([]threadWakeRef, error) {
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
		return nil, err
	}
	a.forgetRemoteRequestBlocked(row.Token)
	if !wake {
		return nil, nil
	}
	return []threadWakeRef{{token: row.Token}}, nil
}

// expireThreadRequests is the retention pass: answers nobody collected are
// dropped a day after they were written, rows are deleted at the floor (a
// spawn's lineage is kept with its payload cleared), remote job watches with
// nothing left to deliver go at the same floor, and answer files nobody
// read go with them.
func (a *App) expireThreadRequests(now time.Time) {
	if dropped, err := a.store.ExpireThreadRequestAnswers(now.UnixMilli()); err != nil {
		logThreadRequestSweep("expire answers", err)
	} else if dropped > 0 {
		log.Printf("thread tools: dropped %d uncollected answer(s) past their hold", dropped)
	}
	floor := now.AddDate(0, 0, -store.ThreadRequestRetentionDays).UnixMilli()
	if swept, released, err := a.store.DeleteThreadRequestsBefore(floor); err != nil {
		logThreadRequestSweep("delete requests past retention", err)
	} else {
		if swept > 0 {
			log.Printf("thread tools: deleted or cleared %d request row(s) past the %d-day floor", swept, store.ThreadRequestRetentionDays)
		}
		// A worker whose paired owner went silent past the floor was
		// released to a sidebar row here.
		for _, threadID := range released {
			a.announcePlaced(threadID)
		}
	}
	if swept, err := a.store.DeleteRemoteWatchesBefore(floor); err != nil {
		logThreadRequestSweep("delete remote job watches past retention", err)
	} else if swept > 0 {
		log.Printf("thread tools: deleted %d finished remote job watch(es) past the %d-day floor", swept, store.ThreadRequestRetentionDays)
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

// bootThreadRequestSweep is what the boot sweep's settlements owe, held from
// the sweep to the activation gate: the wakes to deliver and the scratch
// threads the last run left.
type bootThreadRequestSweep struct {
	works   []threadSettlementWork
	scratch []store.ScratchThread
}

// sweepThreadRequestsAtBoot settles what the last run left open.
//
// A receipt in `accepted` or `running` describes work that was either queued
// and never sent, or running in a process that is gone. Neither can settle
// itself any more, so both settle here as `interrupted`, with the request
// each one answers on this computer. A request this computer made of its own
// thread that is still open after that has nothing left to settle it, and
// settles here too.
//
// It writes rows only, which restoring the database undoes, so it runs before
// the activation gate and before any session can start. What the settlements
// owe waits for the gate (finishThreadRequestSweepAtBoot): a wake can start
// the caller's session, and deleting a scratch thread deletes files.
func (a *App) sweepThreadRequestsAtBoot() {
	endPhase := a.bootPhase("app.sweep_thread_requests", "Settling agent requests from the last run")
	defer endPhase()
	var errs []error
	receipts, err := a.store.ListOpenThreadRequestReceipts()
	if err != nil {
		errs = append(errs, err)
	}
	works := make([]threadSettlementWork, 0, len(receipts))
	// No held request settles at rest before the sweeps recovered what its
	// thread waits on (recoverRestAtBoot).
	a.barRestAtBoot()
	for _, receipt := range receipts {
		// A request the last run held for its thread's rest is held again:
		// its turn ended, and the restart does not end what it waits on.
		settlement, held, err := a.heldReceiptAtBoot(receipt)
		if err != nil {
			errs = append(errs, fmt.Errorf("read whether receipt %s waits for rest: %w", receipt.Token, err))
		} else if held {
			continue
		}
		work, _, err := a.settleReceiptUnderLock(receipt.Token, store.ThreadReceiptOpenStates(), settlement, dropScratchThread)
		if err != nil {
			errs = append(errs, fmt.Errorf("settle interrupted receipt %s: %w", receipt.Token, err))
			continue
		}
		works = append(works, work)
	}
	requests, err := a.store.ListOpenLocalThreadRequests()
	if err != nil {
		errs = append(errs, err)
	}
	for _, row := range requests {
		work, err := a.settleOrphanedThreadRequest(row)
		if err != nil {
			errs = append(errs, fmt.Errorf("settle orphaned request %s: %w", row.Token, err))
		}
		works = append(works, work)
	}
	// Read after the settlements, which every request a scratch thread could
	// answer is now part of.
	scratch, err := a.store.ListScratchThreads()
	if err != nil {
		errs = append(errs, err)
	}
	a.threadRequests.mu.Lock()
	a.threadRequests.boot = bootThreadRequestSweep{works: works, scratch: scratch}
	a.threadRequests.mu.Unlock()
	if err := errors.Join(errs...); err != nil {
		a.bootPhaseFailed(fmt.Errorf("thread requests: %w", err))
	}
}

// settleOrphanedThreadRequest settles, at boot, a request this computer made
// of its own thread that nothing else can settle: its receipt was settled
// without it by an older build, or was never written because the process
// died first. A settled receipt is the answer, collected whole with its late
// reply by the same transaction every local collection runs; no receipt is a
// restart.
func (a *App) settleOrphanedThreadRequest(row store.ThreadRequest) (threadSettlementWork, error) {
	unlock := a.threadRequestSettleLock(row.Token)
	defer unlock()
	_, found, err := a.store.GetThreadRequestReceipt(row.Token)
	if err != nil {
		return threadSettlementWork{}, err
	}
	work := threadSettlementWork{token: row.Token, targetThread: row.TargetThreadID}
	if found {
		// An open receipt collects nothing: the receipt sweep could not
		// settle it and reported why, and its own settlement collects this
		// row.
		collected, err := a.store.CollectLocalThreadReceipt(row.Token, store.ThreadRequestWake{ReplyArms: a.replyArmsWake(row)})
		if err != nil || !collected.Any() {
			return threadSettlementWork{}, err
		}
		work.wake, work.late = a.collectionWake(row.Token, collected)
		return work, nil
	}
	settled, err := a.store.SettleThreadRequest(row.Token, store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
		State:      store.ThreadRequestInterrupted,
		Answer:     []byte(threadtools.InterruptRestart),
		AnswerKind: store.ThreadAnswerError,
	})
	if err != nil || !settled {
		return threadSettlementWork{}, err
	}
	work.wake = a.finishThreadRequestCollection(row.Token, false)
	return work, nil
}

// finishThreadRequestSweepAtBoot does what the boot sweep's settlements owe,
// once the activation gate is open: it delivers their wakes, and any the last
// run settled but never handed over, then deletes the scratch threads the
// last run left. An ask's fork exists only to answer one request, and every
// request it could answer is settled. The wakes go first, so a wake about an
// ask can still name the fork.
//
// The scratch threads are the ones the sweep listed, so a fork a new ask made
// since the gate opened is not among them.
func (a *App) finishThreadRequestSweepAtBoot() {
	a.threadRequests.mu.Lock()
	boot := a.threadRequests.boot
	a.threadRequests.boot = bootThreadRequestSweep{}
	a.threadRequests.mu.Unlock()
	endPhase := a.bootPhase("app.finish_thread_request_sweep", "Delivering agent request answers from the last run")
	defer endPhase()
	// Together, so a thread with many requests open across the restart is
	// told in one message which of them it interrupted.
	a.runThreadSettlementWorks(boot.works)
	// A wake the last run settled but never handed over is owed from the
	// moment this one starts, not at the first retry tick.
	a.retryUndeliveredThreadWakes(time.Now())
	var errs []error
	for _, row := range boot.scratch {
		// The settlement's own work may have deleted it already.
		if _, found, err := a.store.GetScratchThread(row.ThreadID); err != nil {
			errs = append(errs, err)
			continue
		} else if !found {
			continue
		}
		if err := a.deleteThread(row.ThreadID); err != nil {
			errs = append(errs, fmt.Errorf("delete scratch thread %s: %w", row.ThreadID, err))
			continue
		}
		if _, err := a.store.DeleteScratchThread(row.ThreadID); err != nil {
			errs = append(errs, fmt.Errorf("delete scratch row %s: %w", row.ThreadID, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		a.bootPhaseFailed(fmt.Errorf("thread requests: %w", err))
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
