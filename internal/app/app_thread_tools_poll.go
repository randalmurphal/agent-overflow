package app

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

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
	// recovery of rest, or a held request's read of rest, could not read
	// (retryRestRecovery, retryFailedRestReads).
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
		a.recoverThreadRequestsAtBoot()
		a.runThreadRequestSweeps(nudge)
	})
}

// recoverThreadRequestsAtBoot is what the sweep recovers before its first
// pass.
func (a *App) recoverThreadRequestsAtBoot() {
	a.recoverAgentWakesAtBoot()
	// The async questions a restart left open go to their threads'
	// spawners again, and their threads read as waiting on them.
	a.seedAsyncQuestionPrompts()
	// With assistants off, what the last run's pause had not reached
	// settles as the pause settles it, before any held request may settle
	// at rest.
	a.settlePausedAssistantRequests()
	// The workers that wait on what survived the restart, recorded once
	// the loops and watches are recovered; then the requests held for their
	// threads' rest across the restart, those at rest now settling.
	a.recoverRestAtBoot()
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
		// What the boot's recovery of rest, or a held request's read of
		// rest, could not read, read again.
		a.retryRestRecovery()
		a.retryFailedRestReads()
		a.retryThreadAgentEmits(now)
		a.fireDueThreadReminders(now)
		a.fireDueAgentWakes(now)
		logThreadRequestSweep("start owner waits", a.refreshOwnerPeers())
		a.pollRemoteThreadRequests(now)
		a.deliverOwedOwnerOps(now)
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
		case <-a.sweepSleep(a.threadRequestSweepDelay(lastExpiry)):
		}
	}
}

// sweepSleep is the request sweep's sleep of d.
func (a *App) sweepSleep(d time.Duration) <-chan time.Time {
	if sleep := a.sweepSleepForTest; sleep != nil {
		return sleep(d)
	}
	return time.After(d)
}

// threadRequestSweepDelay is how long the sweep sleeps before its next pass:
// until the ledger's next due moment, bounded below by the floor and above
// by the retention clock. A read of rest owed again and the next loop or
// watch fire bound it whether or not the ledger's schedule reads.
//
// A poll already in flight is left out of the answer. Its rows are still due
// by their own column, because the pass that owns them has not rescheduled
// them yet, and sleeping on that would be sleeping for no time at all; the
// poll nudges the sweep when it is done. The reminders and wake retries
// beside it stay in, so a reminder due during a slow poll is not held to
// the poll's timeout.
func (a *App) threadRequestSweepDelay(lastExpiry time.Time) time.Duration {
	delay := threadRequestExpiryInterval - time.Since(lastExpiry)
	var due []store.ScheduledMoment
	if retry := a.threadAgentEmits.nextRetry(); !retry.IsZero() {
		due = append(due, store.ScheduledMoment{At: retry.UnixMilli(), Set: true})
	}
	if a.restRecoveryPending() || a.restReadsFailed() {
		due = append(due, store.ScheduledMoment{At: time.Now().Add(restRecoveryRetry).UnixMilli(), Set: true})
	}
	if loops, err := a.store.NextAgentWakeDue(); err != nil {
		logThreadRequestSweep("read the next loop or watch fire", err)
	} else {
		due = append(due, loops)
	}
	schedule, err := a.store.NextThreadRequestWork(a.ownerUpdates.capable())
	if err != nil {
		logThreadRequestSweep("read the next scheduled request work", err)
	} else {
		due = append(due, schedule.Reminder, schedule.Wake)
		if !a.threadRequestPollInFlight() {
			due = append(due, schedule.Poll)
		}
		if !a.ownerOpDrainInFlight() {
			// What archives and deletes still owe paired computers, on the
			// same terms as the poll's rows.
			due = append(due, schedule.OwnerOp)
		}
	}
	for _, moment := range due {
		if !moment.Set {
			continue
		}
		if until := time.Until(time.UnixMilli(moment.At)); until < delay {
			delay = until
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
//
// A computer owed an acknowledgement whose retry is due and that has no
// row due is sent its acknowledgements alone.
func (a *App) pollRemoteThreadRequests(now time.Time) {
	if a.backends == nil || !a.beginThreadRequestPoll() {
		return
	}
	rows, err := a.store.DueThreadRequestPolls(now.UnixMilli(), threadRequestPollBatch, a.ownerUpdates.capable())
	if err != nil {
		a.endThreadRequestPoll()
		logThreadRequestSweep("read due request polls", err)
		return
	}
	acks, err := a.store.DueThreadRequestAcks(now.UnixMilli(), threadRequestPollBatch)
	if err != nil {
		logThreadRequestSweep("read due request acknowledgements", err)
	}
	if len(rows) == 0 && len(acks) == 0 {
		a.endThreadRequestPoll()
		return
	}
	a.startThreadRequestPoll(now, rows, acks)
}

// startThreadRequestPoll runs one pass at now over rows, and over the
// computers owed the due acknowledgements, off the sweep, releasing the poll
// slot when it ends or when shutdown refuses it.
func (a *App) startThreadRequestPoll(now time.Time, rows []store.ThreadRequest, acks []store.ThreadRequestAck) {
	if a.lifeCtx().Err() != nil || !a.threadRequestsWG.Go(func() {
		defer a.endThreadRequestPoll()
		a.runThreadRequestPoll(now, rows, acks)
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
// about its whole set; a computer owed a due acknowledgement with no row due
// is sent the acknowledgements owed to it. now is the pass's moment, which an
// answer that a token is unknown is judged at.
func (a *App) runThreadRequestPoll(now time.Time, rows []store.ThreadRequest, acks []store.ThreadRequestAck) {
	order := make([]string, 0, 4)
	byComputer := make(map[string][]store.ThreadRequest, 4)
	dueAcks := make(map[string][]store.ThreadRequestAck, 4)
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
	for _, ack := range acks {
		if _, seen := byComputer[ack.ComputerID]; !seen {
			order = append(order, ack.ComputerID)
			byComputer[ack.ComputerID] = nil
		}
		dueAcks[ack.ComputerID] = append(dueAcks[ack.ComputerID], ack)
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
			a.pollThreadRequestsOn(now, computerID, byComputer[computerID], dueAcks[computerID])
		}(computerID)
	}
	wg.Wait()

}

// expireThreadRequests is the retention pass: answers nobody collected are
// dropped a day after they were written, rows are deleted at the floor (a
// spawn's lineage is kept with its payload cleared, and a spawn whose
// thread here is not at rest is left whole: sweepThreadRequestsBefore),
// remote job watches with nothing left to deliver go at the same floor, and
// answer files nobody read go with them.
func (a *App) expireThreadRequests(now time.Time) {
	if dropped, err := a.store.ExpireThreadRequestAnswers(now.UnixMilli()); err != nil {
		logThreadRequestSweep("expire answers", err)
	} else if dropped > 0 {
		log.Printf("thread tools: dropped %d uncollected answer(s) past their hold", dropped)
	}
	floor := now.AddDate(0, 0, -store.ThreadRequestRetentionDays).UnixMilli()
	if swept, released, err := a.sweepThreadRequestsBefore(floor); err != nil {
		logThreadRequestSweep("delete requests past retention", err)
	} else {
		if swept > 0 {
			log.Printf("thread tools: deleted or cleared %d request row(s) past the %d-day floor", swept, store.ThreadRequestRetentionDays)
		}
		// A worker whose paired owner went silent past the floor was
		// released to a sidebar row here, and its prompts are the person's.
		a.announceReleasedWorkers(released)
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
	a.peerExports.forgetBefore(now.Add(-threadRequestExportAge))
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
	settled, err := a.store.SettleThreadRequest(row.Token, store.ThreadRequestOpenStates(),
		interruptedSettlement(threadtools.Cause{Code: threadtools.CauseRestart}, ""))
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
