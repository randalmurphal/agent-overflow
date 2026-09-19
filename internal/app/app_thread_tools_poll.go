package app

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
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
	// threadRequestSweepInterval is the reminder clock's resolution. A
	// reminder is a message to the person's own agent, so a second of slack
	// is invisible and the query behind it is an index seek on an empty
	// table.
	threadRequestSweepInterval = time.Second
	// threadRequestExpiryInterval is how often the retention work runs. It
	// writes, so it runs on its own much slower clock.
	threadRequestExpiryInterval = 10 * time.Minute
	// threadRequestPollBatch bounds one pass over the remote rows.
	threadRequestPollBatch = 32
	// threadPollNormalDelay, threadPollWaitingDelay and threadPollErrorDelay
	// are the poll cadence, the remote-watch table applied to requests: a
	// token a call is parked on is visited faster because that call is
	// paying for the latency, and a destination that failed is left alone
	// for long enough that an offline computer costs one call a half minute.
	threadPollNormalDelay  = 5 * time.Second
	threadPollWaitingDelay = 2 * time.Second
	threadPollErrorDelay   = 30 * time.Second
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

func (a *App) runThreadRequestSweeps(nudge <-chan struct{}) {
	ticker := time.NewTicker(threadRequestSweepInterval)
	defer ticker.Stop()
	done := a.lifeCtx().Done()
	lastExpiry := time.Now()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
		case <-nudge:
		}
		if a.shuttingDown.Load() {
			return
		}
		now := time.Now()
		a.fireDueThreadReminders(now)
		a.pollRemoteThreadRequests(now)
		if now.Sub(lastExpiry) >= threadRequestExpiryInterval {
			lastExpiry = now
			a.expireThreadRequests(now)
		}
	}
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
}

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
	for _, computerID := range order {
		if a.lifeCtx().Err() != nil {
			return
		}
		a.pollThreadRequestsOn(computerID, byComputer[computerID])
	}
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
		a.rescheduleThreadRequest(row, a.threadPollDelay(row.Token), "")
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
	if err := a.store.RescheduleThreadRequest(row.Token, time.Now().Add(delay).UnixMilli(), attempts, issue); err != nil {
		logThreadRequestSweep("reschedule request "+row.Token, err)
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
	a.noteRemoteRequestLive(token, answer.Blocked, answer.Title)
	if answer.TargetThreadID != "" && answer.TargetThreadID != row.TargetThreadID {
		if _, err := a.store.SetThreadRequestTarget(token, computerID, answer.TargetThreadID); err != nil {
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
		// `finished` request can still take a late reply.
		return 0, nil
	}
	return a.collectRemoteThreadRequest(row, answer)
}

// settleUnknownRemoteRequest answers a token the destination does not hold.
//
// Before acceptance that means the request never landed there, which is a
// refusal. After it, the destination lost a receipt it had: swept past the
// retention floor, or restored from a backup. Neither can be collected, and
// the two read differently to the agent that is waiting.
func (a *App) settleUnknownRemoteRequest(row store.ThreadRequest) error {
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
func (a *App) collectRemoteThreadRequest(row store.ThreadRequest, answer ThreadPeerRequest) (int64, error) {
	unlock := a.threadRequestSettleLock(row.Token)
	defer unlock()
	// Re-read under the lock: a cancel on this computer can settle the row
	// between the poll's read and this write.
	current, found, err := a.store.GetThreadRequest(row.Token)
	if err != nil || !found {
		return 0, err
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
			return 0, err
		}
		if settled {
			a.forgetRemoteRequestLive(row.Token)
			a.finishThreadRequestCollection(current, false)
		}
	}
	if answer.LateReply != "" && len(current.LateReply) == 0 {
		stored, err := a.store.StoreThreadRequestLateReply(row.Token, []byte(answer.LateReply), answer.LateReplyAt)
		if err != nil {
			return 0, err
		}
		if stored {
			late, found, err := a.store.GetThreadRequest(row.Token)
			if err != nil {
				return 0, err
			}
			if found {
				a.finishThreadRequestCollection(late, true)
			}
		}
	}
	return answer.Revision, nil
}

// settleRemoteThreadRequest settles a source row for a reason the
// destination gave rather than an answer it produced, and delivers it.
func (a *App) settleRemoteThreadRequest(row store.ThreadRequest, settlement store.ThreadRequestSettlement) error {
	unlock := a.threadRequestSettleLock(row.Token)
	defer unlock()
	settled, err := a.store.SettleThreadRequest(row.Token, store.ThreadRequestOpenStates(), settlement)
	if err != nil {
		return err
	}
	if !settled {
		return nil
	}
	a.forgetRemoteRequestLive(row.Token)
	a.finishThreadRequestCollection(row, false)
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
		func() {
			unlock := a.threadRequestSettleLock(receipt.Token)
			defer unlock()
			if err := a.settleThreadReceipt(receipt.Token, store.ThreadReceiptOpenStates(), store.ThreadRequestSettlement{
				State:      store.ThreadReceiptInterrupted,
				Answer:     []byte("This computer restarted before the request finished."),
				AnswerKind: store.ThreadAnswerError,
			}); err != nil {
				logThreadRequestSweep("settle interrupted receipt "+receipt.Token, err)
			}
		}()
	}
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
