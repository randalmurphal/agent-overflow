package app

import (
	"agent-overflow/internal/attachedbackends"
	"context"
	"errors"
	"slices"
	"time"

	"agent-overflow/internal/agenthome"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
)

// The collection half of the request ledger: one status call to a paired
// computer, the answers it brings applied to this computer's rows, and the
// acknowledgements they owe that computer (store/thread_request_acks.go).

// pollThreadRequestsOn asks one destination about every token due for it,
// and about the other requests the same threads still have open there, and
// applies the answers.
//
// Those ride along because each row keeps its own clock: two requests a
// thread made seconds apart would otherwise be asked about in different
// passes for as long as they ran, and answers one turn or one restart wrote
// there would arrive here as separate messages. Asked together, they are
// collected together and told to their caller once.
//
// The call carries every acknowledgement owed to that computer, and names
// the revision each settled row already holds, which that computer answers
// without the text this one has (ThreadPeerPoll.Held).
//
// A computer this one holds no saved pairing with is asked nothing: its
// work ends (endUnpaired). A pass that cannot read the pairing puts off the
// rows and the due acknowledgements by the error delay. With no row to ask
// about, it sends the acknowledgements owed there alone.
func (a *App) pollThreadRequestsOn(now time.Time, computerID string, rows []store.ThreadRequest, dueAcks []store.ThreadRequestAck) {
	if len(rows) > threadPeerTokenLimit {
		rows = rows[:threadPeerTokenLimit]
	}
	ended, err := a.endUnpaired(computerID)
	if err != nil {
		logThreadRequestSweep("read the pairing with "+computerID, err)
		for _, row := range rows {
			a.rescheduleThreadRequest(row, threadPollErrorDelay, "This computer could not read its pairing with the other computer.")
		}
		a.deferThreadRequestAcks(dueAcks)
		return
	}
	if ended {
		return
	}
	if len(rows) == 0 {
		a.sendOwedThreadRequestAcks(computerID)
		return
	}
	if !slices.Contains(a.ownerUpdates.capable(), computerID) {
		rows = a.withOpenThreadRequestsOn(computerID, rows)
	}
	poll := ThreadPeerPoll{Tokens: make([]string, 0, len(rows))}
	for _, row := range rows {
		poll.Tokens = append(poll.Tokens, row.Token)
		if threadRequestSettled(row) && row.Revision > 0 {
			poll.Held = append(poll.Held, ThreadPeerAck{Token: row.Token, Revision: row.Revision})
		}
	}
	acks, err := a.store.OwedThreadRequestAcks(computerID, threadPeerTokenLimit)
	if err != nil {
		// They stay owed, and the call after this one carries them.
		logThreadRequestSweep("read the acknowledgements owed to "+computerID, err)
	}
	poll.Ack = threadPeerAcks(acks)
	read := a.ownerReads.start(a.lifeCtx(), computerID, poll.Tokens)
	defer read.close()
	reply, origin, err := a.readThreadRequestStatus(computerID, poll)
	if err != nil {
		a.failThreadRequestPoll(computerID, rows, acks, err)
		return
	}
	var wakes []threadWakeRef
	unlock := a.lockComputerPairing(computerID)
	err = read.commit(origin, "", nil, func() error {
		a.clearThreadRequestAcks(acks)
		heard := threadPeerHearing{at: now, v2: origin.ProvedGeneration()}
		answers := make(map[string]ThreadPeerRequest, len(reply.Requests))
		for _, answer := range reply.Requests {
			answers[answer.Token] = answer
		}
		for _, row := range rows {
			answer, found := answers[row.Token]
			if !found {
				// The destination answered without this token. Nothing is known
				// about it, so it waits for the next pass rather than being
				// settled on an answer nobody gave.
				a.rescheduleThreadRequest(row, threadPollErrorDelay, "The other computer did not report this request.")
				continue
			}
			owed, err := a.applyCurrentPeerRow(row.Token, computerID, answer, heard)
			wakes = append(wakes, owed...)
			if err != nil {
				logThreadRequestSweep("apply request "+row.Token, err)
				a.rescheduleThreadRequest(row, threadPollErrorDelay, err.Error())
				continue
			}
			a.scheduleNextThreadPoll(row, now)
		}
		return nil
	})
	unlock()
	if err != nil {
		if !errors.Is(err, errReadSuperseded) {
			a.failThreadRequestPoll(computerID, rows, acks, err)
		}
		return
	}

	// Together, as a turn end on this computer delivers them: one turn there
	// that answered several requests is one message here.
	a.deliverThreadWakes(wakes)
	// What this pass collected is acknowledged now rather than by the next
	// poll: a row that just settled may never be polled again.
	a.sendOwedThreadRequestAcks(computerID)
}

// sendOwedThreadRequestAcks sends one computer the acknowledgements owed to
// it on a call of their own, which every build of the destination reads
// (ThreadPeerPoll with no tokens). They are cleared once it takes them, and
// retried by the sweep after the error delay when it does not.
func (a *App) sendOwedThreadRequestAcks(computerID string) {
	acks, err := a.store.OwedThreadRequestAcks(computerID, threadPeerTokenLimit)
	if err != nil {
		logThreadRequestSweep("read the acknowledgements owed to "+computerID, err)
		return
	}
	if len(acks) == 0 {
		return
	}
	read := a.ownerReads.start(a.lifeCtx(), computerID, nil)
	_, origin, err := a.readThreadRequestStatus(computerID, ThreadPeerPoll{Ack: threadPeerAcks(acks)})
	if err == nil {
		err = read.commit(origin, "", nil, func() error { a.clearThreadRequestAcks(acks); return nil })
	}
	if err != nil {
		a.failThreadRequestAcks(computerID, acks)
	}
}

func threadPeerAcks(acks []store.ThreadRequestAck) []ThreadPeerAck {
	if len(acks) == 0 {
		return nil
	}
	out := make([]ThreadPeerAck, 0, len(acks))
	for _, ack := range acks {
		out = append(out, ThreadPeerAck{Token: ack.Token, Revision: ack.Revision})
	}
	return out
}

// clearThreadRequestAcks records that the destination took acks. A clear
// that fails leaves them owed, and they are sent again.
func (a *App) clearThreadRequestAcks(acks []store.ThreadRequestAck) {
	logThreadRequestSweep("clear acknowledgements the other computer took", a.store.ClearThreadRequestAcks(acks))
}

// failThreadRequestAcks answers a destination that did not take the acks
// owed to it: a computer no longer paired here is owed nothing more
// (endUnpaired); anything else is retried after the error delay.
func (a *App) failThreadRequestAcks(computerID string, acks []store.ThreadRequestAck) {
	if a.unpairedAfterFailure(computerID) {
		return
	}
	a.deferThreadRequestAcks(acks)
}

func (a *App) deferThreadRequestAcks(acks []store.ThreadRequestAck) {
	if err := a.store.DeferThreadRequestAcks(acks, time.Now().Add(threadPollErrorDelay).UnixMilli()); err != nil {
		logThreadRequestSweep("put off acknowledgements the other computer did not take", err)
		return
	}
	a.nudgeThreadRequestSweep()
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

// callThreadRequestStatusProving is callThreadRequestStatus that also
// reports whether the destination that answered is on thread-tools.v2,
// whose answer marks a token a handler there may still admit.
func (a *App) readThreadRequestStatus(computerID string, poll ThreadPeerPoll) (ThreadPeerPollReply, attachedbackends.PeerRead, error) {
	ctx, cancel := context.WithTimeout(a.lifeCtx(), threadPollCallTimeout)
	defer cancel()
	var reply ThreadPeerPollReply
	origin, err := a.backends.ReadThreadPeer(ctx, computerID, transport.CapabilityThreadTools, transport.CapabilityThreadToolsV2,
		"ThreadToolRequestStatus", &reply, poll)
	if err != nil {
		return ThreadPeerPollReply{}, origin, a.threadOperationError("status", computerID, "", err)
	}
	return reply, origin, nil
}

// failThreadRequestPoll answers one unreachable or refusing destination.
//
// A computer no longer paired here is terminal (endUnpaired): nothing there
// can be collected again, so its requests settle and leave the poll rather
// than being retried forever. Anything else is a computer that is asleep,
// offline or busy, which is what the error backoff is for.
func (a *App) failThreadRequestPoll(computerID string, rows []store.ThreadRequest, acks []store.ThreadRequestAck, cause error) {
	if a.unpairedAfterFailure(computerID) {
		return
	}
	_, message, _ := threadErrorDetails("status", cause)
	for _, row := range rows {
		a.rescheduleThreadRequest(row, threadPollErrorDelay, message)
	}
	a.deferThreadRequestAcks(acks)
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
func (a *App) scheduleNextThreadPoll(row store.ThreadRequest, observed time.Time) {
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
	if !threadLateReplyAwaited(current, observed) {
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
	if len(row.LateReply) > 0 || row.LateReplyExpired || row.State != store.ThreadRequestFinished {
		// Only a finished request takes a late reply, and only one; every
		// other settlement is the last word on it.
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
// source row, with the acknowledgement it owes that computer, and returns
// the wakes the write owes its caller thread. The caller delivers those,
// with the wakes of whatever else it applied in the same pass, and sends
// what is owed.
//
// It is the one place a peer's answer becomes this computer's record, so
// the confirmation a spawn, send or ask gets in its own reply and the one
// the poller gets a tick later cannot disagree. It applies inside the
// computer's fence, and only while a pairing with that computer is saved:
// an answer that arrives after the pairing's end is the end's to answer,
// and changes nothing here.
func (a *App) applyThreadPeerRequest(token, computerID string, answer ThreadPeerRequest, heard threadPeerHearing) ([]threadWakeRef, error) {
	unlock := a.lockComputerPairing(computerID)
	defer unlock()
	paired, err := a.pairingSaved(computerID)
	if err != nil || !paired {
		return nil, err
	}
	return a.applyCurrentPeerRow(token, computerID, answer, heard)
}

func (a *App) applyCurrentPeerRow(token, computerID string, answer ThreadPeerRequest, heard threadPeerHearing) ([]threadWakeRef, error) {
	row, found, err := a.store.GetThreadRequest(token)
	if err != nil || !found || row.TargetComputerID != computerID {
		return nil, err
	}
	return a.applyKnownPeerRow(row, answer, heard)
}

func (a *App) applyKnownPeerRow(row store.ThreadRequest, answer ThreadPeerRequest, heard threadPeerHearing) ([]threadWakeRef, error) {
	token, computerID := row.Token, row.TargetComputerID
	if !answer.Known {
		return a.settleUnknownRemoteRequest(row, answer, heard)
	}
	a.noteRemoteRequestBlocked(token, remoteBlocked{
		blocked: answer.Blocked, routedTo: answer.BlockedRoutedTo, routedToCaller: answer.BlockedRoutedToCaller,
		routedToThread: answer.BlockedRoutedToThread, waitingOn: answer.WaitingOn,
	})
	if target := threadPeerTarget(row, answer); target != "" {
		if _, err := a.store.SetThreadRequestTarget(token, computerID, target, answer.Title, answer.Provider); err != nil {
			return nil, err
		}
	}
	// Whichever answer first states them, the call's own reply or a poll
	// that recovered a lost one: the destination's receipt recorded them
	// once, when it named the thread.
	if err := a.store.RecordThreadRequestInstructions(token, answer.Instructions); err != nil {
		return nil, err
	}
	if !store.ThreadReceiptSettled(answer.State) {
		// Still accepted or running there. The source row follows so
		// thread_status reads the same word on both computers.
		if answer.State == store.ThreadReceiptRunning {
			if _, err := a.store.AdvanceThreadRequestState(token, store.ThreadRequestUnconfirmed, store.ThreadRequestAccepted); err != nil {
				return nil, err
			}
			_, err := a.store.AdvanceThreadRequestState(token, store.ThreadRequestAccepted, store.ThreadRequestRunning)
			return nil, err
		}
		_, err := a.store.AdvanceThreadRequestState(token, store.ThreadRequestUnconfirmed, store.ThreadRequestAccepted)
		return nil, err
	}
	if threadRequestSettled(row) && answer.Revision <= row.Revision {
		// Already collected. The row stays in the poll only because a
		// `finished` request can still take a late reply, and a settled
		// request blocks nobody. A destination still waiting for the
		// acknowledgement never took the one this computer cleared, so it
		// is owed again: the row holds every revision up to its own.
		a.forgetRemoteRequestBlocked(token)
		if answer.CollectedRevision < answer.Revision {
			_, err := a.store.OweThreadRequestAck(token, answer.Revision)
			return nil, err
		}
		return nil, nil
	}
	return a.collectRemoteThreadRequest(row, answer)
}

// collectThreadPeerReply applies an answer that came back on a call's own
// reply rather than from the poller, and sends the acknowledgements owed to
// that computer, this one's among them, rather than leave it to a poll the
// row it settled may never get.
func (a *App) collectThreadPeerReply(token, computerID string, answer ThreadPeerRequest) error {
	// A call's own reply marks no admission, so an unknown token in it is
	// judged by the deadline alone.
	wakes, err := a.applyThreadPeerRequest(token, computerID, answer, threadPeerHearing{at: time.Now()})
	a.deliverThreadWakes(wakes)
	if err != nil {
		return err
	}
	a.sendOwedThreadRequestAcks(computerID)
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

// threadPeerHearing is how an answer was heard: at, the moment it is judged
// at, and v2, whether the destination that gave it is on thread-tools.v2,
// whose status answer marks a token a handler there may still admit.
type threadPeerHearing struct {
	at time.Time
	v2 bool
}

// threadRequestNeverStarted reports whether an answer that the destination
// holds no receipt for an unconfirmed row's token is conclusive: no attempt
// started the request there and none still can. Either the destination is
// on thread-tools.v2 and has no handler in flight for it, or the last
// moment any attempt's handler could admit it has passed.
func threadRequestNeverStarted(row store.ThreadRequest, answer ThreadPeerRequest, heard threadPeerHearing) bool {
	return (heard.v2 && !answer.Admitting) || heard.at.UnixMilli() >= row.AdmitUntil
}

// settleUnknownRemoteRequest answers a token the destination does not hold.
//
// Before acceptance that means the request never landed there, which is a
// refusal once the answer is conclusive (threadRequestNeverStarted); until
// then the row waits for the next poll, and nothing is reported. After
// acceptance, the destination lost a receipt it had: swept past the
// retention floor, or restored from a backup. Neither can be collected, and
// the two read differently to the agent that is waiting.
func (a *App) settleUnknownRemoteRequest(row store.ThreadRequest, answer ThreadPeerRequest, heard threadPeerHearing) ([]threadWakeRef, error) {
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
		if !threadRequestNeverStarted(row, answer, heard) {
			return nil, nil
		}
		settlement.State = store.ThreadRequestRefused
		settlement.Answer = []byte("The other computer never accepted this request.")
	}
	return a.settleRemoteThreadRequestOwing(row, settlement)
}

// collectRemoteThreadRequest copies one destination answer onto the source
// row, settlement and late reply together, with the acknowledgement it
// owes, and returns the wakes it owes the caller thread.
//
// The settle lock covers the durable write and the delivery decision; the
// wakes it decides are delivered by the caller after it is released, because
// a wake takes the caller thread's own locks and can start its session.
func (a *App) collectRemoteThreadRequest(row store.ThreadRequest, answer ThreadPeerRequest) ([]threadWakeRef, error) {
	wake, late, err := a.collectRemoteThreadRequestLocked(row, answer)
	if err != nil {
		return nil, err
	}
	a.forgetRemoteRequestBlocked(row.Token)
	if !wake {
		return nil, nil
	}
	return []threadWakeRef{{token: row.Token, late: late}}, nil
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
	text, cause := remoteAnswerCause(answer)
	collected, err := a.store.CollectThreadRequest(row.Token, store.ThreadReceiptAnswer{
		RestUnreported: restUnreported,
		Revision:       answer.Revision,
		State:          answer.State,
		Answer:         []byte(text),
		AnswerKind:     answer.AnswerKind,
		Cause:          cause.Code,
		CauseArg:       cause.Arg,
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

// remoteAnswerCause is the text and cause a paired computer's answer is
// recorded with: an interruption's cause it reports in this computer's
// vocabulary, with what the turn had written as the text; otherwise its
// answer, whole and with no cause, which is what a computer that reports no
// cause, or one this computer does not know, gives.
func remoteAnswerCause(answer ThreadPeerRequest) (string, threadtools.Cause) {
	if reported := answer.Cause; reported != nil {
		if cause := (threadtools.Cause{Code: reported.Code, Arg: reported.Arg}); cause.Known() {
			return reported.Written, cause
		}
	}
	return answer.Answer, threadtools.Cause{}
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

// settleRemoteThreadRequestOwing settles a source row for a reason the
// destination gave rather than an answer it produced, and returns the wake
// the settlement owes, which the caller delivers once the settle lock is
// released, with the wakes of whatever else it settled in the same pass.
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
