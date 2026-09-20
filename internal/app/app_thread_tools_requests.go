package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/usermessage"
)

// thread_status and thread_cancel: reading, waiting on and stopping requests
// the caller already made.
//
// Nothing here starts work. What it does own is the other half of delivery:
// an answer a call returns in its own reply is delivered, and saying so is
// what keeps a wake from arriving for something the model has already read.

// threadWatchKey is the wait key for a thread id, kept distinct from a token
// so one registry can hold both without a thread id ever matching a request.
func threadWatchKey(threadID string) string { return "thread:" + threadID }

// threadToolsPending holds work that must not run until the tool response has
// been written: the inline delivery marks, and the deletion of a scratch
// thread whose own agent is still inside the call.
//
// It rides the call context because the adapter methods below are the only
// ones that know what is owed, while the transport is the only thing that
// knows when the response actually left this computer: callThreadMCP hands
// the list to threadmcp.AfterResponse, which runs it after the final bytes
// are flushed.
//
// A response that failed to reach the model runs none of it. Nothing may be
// recorded as read by a reader that never received it, and a scratch thread
// must not be deleted under an agent whose tool call just failed: the answer
// stays undeliverable so the caller is woken for it instead, and the fork is
// dropped by the boot sweep, which takes every scratch thread.
type threadToolsPending struct {
	mu  sync.Mutex
	fns []func()
}

type threadToolsPendingKey struct{}

// withThreadToolsPending arms a call context to collect after-response work.
func withThreadToolsPending(ctx context.Context) (context.Context, *threadToolsPending) {
	pending := &threadToolsPending{}
	return context.WithValue(ctx, threadToolsPendingKey{}, pending), pending
}

func (p *threadToolsPending) add(fn func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fns = append(p.fns, fn)
}

// run executes what the call deferred, in order, once the response has been
// delivered. A response that never reached the model delivered nothing, so
// the list is dropped and said to be dropped.
func (p *threadToolsPending) run(delivered bool) {
	p.mu.Lock()
	fns := p.fns
	p.fns = nil
	p.mu.Unlock()
	if !delivered {
		if len(fns) > 0 {
			log.Printf("thread tools: the response was not delivered; %d deferred step(s) dropped", len(fns))
		}
		return
	}
	for _, fn := range fns {
		fn()
	}
}

// afterResponse defers work until the tool response is written. With no
// armed context (a direct in-process call, which has no response to wait
// for) it runs now, because deferring it would drop it.
func (t threadToolsApp) afterResponse(ctx context.Context, fn func()) {
	if pending, ok := ctx.Value(threadToolsPendingKey{}).(*threadToolsPending); ok {
		pending.add(fn)
		return
	}
	fn()
}

// ackRequest waits for a freshly dispatched request as long as the call asked
// to, then renders what it knows.
func (t threadToolsApp) ackRequest(ctx context.Context, caller threadtools.Caller, token string, waitSeconds int) (threadtools.RequestAck, error) {
	row, err := t.waitForRequest(ctx, caller, token, waitSeconds, 0)
	if err != nil {
		return threadtools.RequestAck{}, err
	}
	state := t.requestState(ctx, row)
	return threadtools.RequestAck{
		RequestState: state,
		Outcome:      threadRequestOutcome(row, state.State == threadtools.RequestBlocked),
	}, nil
}

// waitForRequest parks on one token and returns the row as it stands
// afterwards, arming the wake when a positive wait ended with nothing.
func (t threadToolsApp) waitForRequest(
	ctx context.Context, caller threadtools.Caller, token string, waitSeconds int, afterRevision int64,
) (store.ThreadRequest, error) {
	if _, err := t.app.waitRequest(ctx, caller.ThreadID, []string{token}, waitSeconds,
		func() (string, bool, error) { return t.settledOrBlocked(token, afterRevision) }); err != nil {
		return store.ThreadRequest{}, err
	}
	row, found, err := t.readRequestAndArmNotify(token, waitSeconds > 0)
	if err != nil {
		return store.ThreadRequest{}, err
	}
	if !found {
		return store.ThreadRequest{}, errorsx.Public(threadtools.CodeRequestUnknown,
			"That request is no longer on this computer.", nil)
	}
	return row, nil
}

// readRequestAndArmNotify re-reads one request after a wait and arms the wake
// when that wait is ending with the request still open.
//
// Both halves run under the token's settle lock, which is what makes them
// exclusive with a settlement's delivery decision. Without it a settlement
// landing between the read and the arming finds no waiter and no armed wake,
// delivers nothing, and the arming then lands on a row nobody will collect
// again: settled, notify on, never delivered.
//
// Every end of a positive wait that leaves the row unsettled arms it, not
// only a timeout: a wait ended by a blocked target or by the caller's turn
// being interrupted owes the answer as a message just the same.
func (t threadToolsApp) readRequestAndArmNotify(token string, waited bool) (store.ThreadRequest, bool, error) {
	unlock := t.app.threadRequestSettleLock(token)
	defer unlock()
	row, found, err := t.app.store.GetThreadRequest(token)
	if err != nil || !found {
		return store.ThreadRequest{}, false, err
	}
	if !waited || threadRequestSettled(row) {
		return row, true, nil
	}
	if _, err := t.app.store.SetThreadRequestNotify(token, true); err != nil {
		return store.ThreadRequest{}, false, err
	}
	row.Notify = true
	return row, true, nil
}

// settledOrBlocked is the wait predicate a request is parked on: a settlement
// the caller has not seen, or a target that has stopped to ask a person
// something. The second is not a settlement; it ends the wait because the
// person owns the answer now and the agent should leave it to them.
func (t threadToolsApp) settledOrBlocked(token string, afterRevision int64) (string, bool, error) {
	row, found, err := t.app.store.GetThreadRequest(token)
	if err != nil {
		return "", false, err
	}
	if !found {
		return "", true, nil
	}
	if threadRequestSettled(row) && row.Revision > afterRevision {
		return token, true, nil
	}
	if t.app.threadRequestBlocked(row) {
		return token, true, nil
	}
	return "", false, nil
}

// RequestStates reads, and optionally waits on, the caller's requests by
// token or any threads by id.
func (t threadToolsApp) RequestStates(ctx context.Context, caller threadtools.Caller, call threadtools.StatusCall) (threadtools.StatusReport, error) {
	if len(call.ThreadIDs) > 0 {
		return t.watchThreads(ctx, caller, call)
	}
	rows := make([]store.ThreadRequest, 0, len(call.Tokens))
	for _, token := range call.Tokens {
		row, err := t.ownRequest(caller, token)
		if err != nil {
			return threadtools.StatusReport{}, err
		}
		rows = append(rows, row)
	}
	// Every token is waited on at once and the first to settle ends the
	// call: an agent watching three spawns wants the one that finished, not
	// the one it happened to list first.
	outcome, err := t.app.waitRequest(ctx, caller.ThreadID, call.Tokens, call.WaitSeconds, func() (string, bool, error) {
		for _, token := range call.Tokens {
			woke, done, err := t.settledOrBlocked(token, call.AfterRevision)
			if err != nil || done {
				return woke, done, err
			}
		}
		return "", false, nil
	})
	if err != nil {
		return threadtools.StatusReport{}, err
	}
	report := threadtools.StatusReport{WokeOn: outcome.WokeOn, TimedOut: outcome.TimedOut}
	for index, token := range call.Tokens {
		// Per token, not only the one that ended the call: every other token
		// this call waited on is still open and still owes its answer.
		row, found, err := t.readRequestAndArmNotify(token, call.WaitSeconds > 0)
		if err != nil {
			return threadtools.StatusReport{}, err
		}
		if !found {
			row = rows[index]
		}
		report.Requests = append(report.Requests, t.requestState(ctx, row))
	}
	return report, nil
}

// ownRequest reads one of the caller's own requests. A token that belongs to
// another thread is refused rather than answered: a request record says what
// another conversation is doing.
func (t threadToolsApp) ownRequest(caller threadtools.Caller, token string) (store.ThreadRequest, error) {
	row, found, err := t.app.store.GetThreadRequest(token)
	if err != nil {
		return store.ThreadRequest{}, err
	}
	if !found {
		return store.ThreadRequest{}, errorsx.Public(threadtools.CodeRequestUnknown,
			fmt.Sprintf("No request on this computer carries token %s. List this thread's requests with thread_status and no arguments.", token), nil)
	}
	if row.CallerThreadID != caller.ThreadID {
		return store.ThreadRequest{}, errorsx.Public(threadtools.CodeRequestNotYours,
			fmt.Sprintf("Token %s belongs to a request another thread made.", token), nil)
	}
	return row, nil
}

// watchThreads answers the thread_ids half of thread_status: the live state
// of threads the caller is interested in, waiting until one of them rests or
// stops to ask a person something.
func (t threadToolsApp) watchThreads(ctx context.Context, caller threadtools.Caller, call threadtools.StatusCall) (threadtools.StatusReport, error) {
	for _, threadID := range call.ThreadIDs {
		if _, err := t.localThread(threadID); err != nil {
			return threadtools.StatusReport{}, err
		}
	}
	keys := make([]string, 0, len(call.ThreadIDs))
	for _, threadID := range call.ThreadIDs {
		keys = append(keys, threadWatchKey(threadID))
	}
	outcome, err := t.app.waitRequest(ctx, caller.ThreadID, keys, call.WaitSeconds, func() (string, bool, error) {
		for _, threadID := range call.ThreadIDs {
			state, err := t.threadStateNow(ctx, threadID)
			if err != nil {
				return "", false, err
			}
			if threadtools.Resting(state.State) || state.State == threadtools.StatePendingApproval || state.State == threadtools.StateAwaitingInput {
				return threadID, true, nil
			}
		}
		return "", false, nil
	})
	if err != nil {
		return threadtools.StatusReport{}, err
	}
	report := threadtools.StatusReport{WokeOn: outcome.WokeOn, TimedOut: outcome.TimedOut}
	for _, threadID := range call.ThreadIDs {
		state, err := t.threadStateNow(ctx, threadID)
		if err != nil {
			return threadtools.StatusReport{}, err
		}
		report.Threads = append(report.Threads, state)
	}
	return report, nil
}

func (t threadToolsApp) threadStateNow(ctx context.Context, threadID string) (threadtools.ThreadState, error) {
	thread, err := t.localThread(threadID)
	if err != nil {
		return threadtools.ThreadState{}, err
	}
	live, err := t.LiveState(ctx, threadID)
	if err != nil {
		return threadtools.ThreadState{}, err
	}
	projected := t.projectThread(thread)
	state := threadtools.State(projected, live)
	return threadtools.ThreadState{
		ThreadID: thread.ID,
		Title:    thread.Title,
		State:    state,
		Resting:  threadtools.Resting(state),
	}, nil
}

// requestState renders one source row, and records the inline delivery that
// rendering it performs.
func (t threadToolsApp) requestState(ctx context.Context, row store.ThreadRequest) threadtools.RequestState {
	state := threadtools.RequestState{
		Token:      row.Token,
		Kind:       row.Kind,
		ThreadID:   row.TargetThreadID,
		ComputerID: row.TargetComputerID,
		State:      row.State,
		AnswerKind: row.AnswerKind,
		Revision:   row.Revision,
		Notify:     row.Notify,
		Delivered:  row.DeliveredHow,
		ExpiresAt:  row.ExpiresAt,
		DueAt:      row.DueAt,
	}
	// The newest revision is the answer: a late reply supersedes the final
	// message the caller was already told about.
	answer, late := row.Answer, false
	if len(row.LateReply) > 0 {
		answer, late = row.LateReply, true
		state.AnswerKind = threadtools.AnswerReply
	}
	state.Answer = string(answer)
	if !threadRequestSettled(row) && t.app.threadRequestBlocked(row) {
		state.State = threadtools.RequestBlocked
	}
	if row.TargetThreadID != "" && row.TargetComputerID != "" {
		// A thread on another computer has no row here. Its title is what
		// that computer reported when it was last polled.
		state.Title = t.app.remoteRequestLiveState(row.Token).title
	} else if row.TargetThreadID != "" {
		if thread, err := t.app.store.GetThread(row.TargetThreadID); err == nil {
			state.Title = thread.Title
		} else if row.OriginThreadID != "" {
			// An ask's scratch fork is deleted once it has answered. The
			// thread it was forked from is what the caller asked about, and
			// naming it is more useful than naming nothing.
			if origin, err := t.app.store.GetThread(row.OriginThreadID); err == nil {
				state.Title = origin.Title
			}
		}
	}
	state.WakeQueued = t.wakeQueued(row, late)
	if len(answer) > 0 && threadRequestSettled(row) && !threadRequestDelivered(row, late) {
		// Reading the answer here IS the delivery, but only once the model
		// has it: the mark runs after the response is written. An open row
		// can carry an answer before anything settles it (a reminder holds
		// its note from the moment it is armed), and reading that is not a
		// delivery: the wake it owes has not happened yet.
		state.Delivered = store.ThreadWakeInline
		token := row.Token
		t.afterResponse(ctx, func() {
			unlock := t.app.threadRequestSettleLock(token)
			defer unlock()
			if _, err := t.app.store.MarkThreadRequestDelivered(token, store.ThreadWakeInline, 0, late); err != nil {
				log.Printf("thread tools: mark request %s delivered inline: %v", token, err)
			}
		})
	}
	return state
}

func threadRequestDelivered(row store.ThreadRequest, late bool) bool {
	if late {
		return row.LateDeliveredAt != 0
	}
	return row.DeliveredAt != 0
}

// wakeQueued reports whether a message carrying this answer is still on its
// way to the caller, so a reply that also carries it can say so rather than
// leaving the agent to read the same answer twice without knowing why.
//
// It spans both halves of the queued path, because an agent reading this is
// mid-turn by definition and a live thread's queue hands its message to the
// provider at once: the durable queue row holds the wake only while the
// thread has no session to take it, and after that the dispatched user row
// waits in the provider's own queue until the turn boundary. The echo that
// stamps provider_item_id on that row is the model reading the message, so an
// unstamped row is a message that has not arrived yet. Probing the queue row
// alone answers no for almost the whole window this notice exists for.
func (t threadToolsApp) wakeQueued(row store.ThreadRequest, late bool) bool {
	if !threadRequestDelivered(row, late) || row.DeliveredHow != store.ThreadWakeQueued {
		return false
	}
	record, found, err := t.app.findRecordedSend(row.CallerThreadID, threadWakeSendIDFor(row.Token, late))
	if err != nil {
		log.Printf("thread tools: probe queued wake %s: %v", row.Token, err)
		return false
	}
	if !found {
		return false
	}
	if !record.dispatched {
		return true
	}
	return usermessage.ReadProviderItemID(record.item.Meta) == ""
}

// ListRequests lists the caller's own requests, open first then newest first.
func (t threadToolsApp) ListRequests(ctx context.Context, caller threadtools.Caller, call threadtools.ListCall) (threadtools.RequestListing, error) {
	limit := call.Limit
	if limit <= 0 {
		limit = threadtools.DefaultRequestListLimit
	}
	// One row past the page answers More without a second query.
	rows, err := t.app.store.ListThreadRequestsByCaller(caller.ThreadID, limit+1, call.Offset)
	if err != nil {
		return threadtools.RequestListing{}, err
	}
	listing := threadtools.RequestListing{More: len(rows) > limit}
	if listing.More {
		rows = rows[:limit]
	}
	for _, row := range rows {
		listing.Requests = append(listing.Requests, t.requestState(ctx, row))
	}
	return listing, nil
}

// ExportAnswer writes one request's whole answer to this computer's export
// directory. The answer outlives the thread that wrote it, so this works
// after an ask's scratch fork is long gone.
func (t threadToolsApp) ExportAnswer(ctx context.Context, caller threadtools.Caller, token string) (threadtools.ExportFile, error) {
	row, err := t.ownRequest(caller, token)
	if err != nil {
		return threadtools.ExportFile{}, err
	}
	answer := row.Answer
	late := false
	if len(row.LateReply) > 0 {
		answer, late = row.LateReply, true
	}
	if len(answer) == 0 {
		return threadtools.ExportFile{}, errorsx.Public(threadtools.CodeInvalidRequest,
			fmt.Sprintf("Request %s has no answer to write: it is %s.", token, row.State), nil)
	}
	if t.app.configDir == "" {
		return threadtools.ExportFile{}, errorsx.Public(threadtools.CodeInvalidRequest,
			"This computer has no data directory configured, so an answer cannot be written to a file.", nil)
	}
	dir := filepath.Join(t.app.configDir, threadExportDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return threadtools.ExportFile{}, fmt.Errorf("thread tools: create export directory: %w", err)
	}
	path := filepath.Join(dir, "answer-"+token+".txt")
	if err := os.WriteFile(path, answer, 0o600); err != nil {
		return threadtools.ExportFile{}, fmt.Errorf("thread tools: write answer file: %w", err)
	}
	digest := sha256.Sum256(answer)
	if !threadRequestDelivered(row, late) {
		t.afterResponse(ctx, func() {
			unlock := t.app.threadRequestSettleLock(token)
			defer unlock()
			if _, err := t.app.store.MarkThreadRequestDelivered(token, store.ThreadWakeInline, 0, late); err != nil {
				log.Printf("thread tools: mark request %s delivered to file: %v", token, err)
			}
		})
	}
	return threadtools.ExportFile{Path: path, Size: int64(len(answer)), SHA256: hex.EncodeToString(digest[:])}, nil
}

// Cancel stops one request the caller made, or interrupts a thread it
// started.
func (t threadToolsApp) Cancel(ctx context.Context, caller threadtools.Caller, call threadtools.CancelCall) (threadtools.CancelReport, error) {
	if call.Token != "" {
		// The token names its own destination: the row records where the
		// request went, and a computer_id the caller passed with it is
		// either the same computer or a mistake.
		return t.cancelRequest(ctx, caller, call.Token)
	}
	if t.remoteDestination(call.ComputerID) {
		return t.cancelRemoteThread(ctx, caller, call)
	}
	return t.cancelThread(ctx, caller, call.ThreadID)
}

func (t threadToolsApp) cancelRequest(ctx context.Context, caller threadtools.Caller, token string) (threadtools.CancelReport, error) {
	row, err := t.ownRequest(caller, token)
	if err != nil {
		return threadtools.CancelReport{}, err
	}
	report := threadtools.CancelReport{Token: token, ThreadID: row.TargetThreadID, ComputerID: row.TargetComputerID}
	if threadRequestSettled(row) {
		report.State, report.Effect = row.State, threadtools.EffectNothing
		return report, nil
	}
	if row.TargetComputerID != "" {
		return t.cancelRemoteRequest(ctx, caller, row)
	}
	// A reminder is nothing but a row: there is no work to stop, and the
	// caller cancelling it wants it gone rather than settled as cancelled.
	if row.Kind == store.ThreadRequestRemind {
		if _, err := t.app.store.DeleteThreadRequest(token); err != nil {
			return threadtools.CancelReport{}, err
		}
		t.app.wakeRequestWaits(token)
		report.State, report.Effect = threadtools.RequestCancelled, threadtools.EffectReminderDropped
		return report, nil
	}

	// The caller is cancelling its own request, and the report it is about
	// to read says what happened. Clearing the notify flag first is what
	// keeps the cancellation from also arriving later as an unread message
	// about work the caller stopped on purpose.
	if row.Notify {
		if _, err := t.app.store.SetThreadRequestNotify(token, false); err != nil {
			return threadtools.CancelReport{}, err
		}
	}
	effect, err := t.app.stopThreadRequestWork(ctx, token)
	if err != nil {
		return threadtools.CancelReport{}, err
	}
	report.Effect = effect
	report.State = threadtools.RequestCancelled
	if settled, found, err := t.app.store.GetThreadRequest(token); err == nil && found {
		report.State = settled.State
	}
	return report, nil
}

// threadCancelTimeout bounds one forwarded cancel. It matches the remote
// command stop: a cancel that cannot be delivered must not hold the calling
// turn while an offline computer times out its whole call budget.
const threadCancelTimeout = 20 * time.Second

// cancelRemoteRequest stops one request on the computer that accepted it.
//
// The destination settles its own receipt, because it is the only side that
// can take back a queued message or interrupt a turn. The source row is
// settled from the receipt the reply carries, through the same collector a
// poll would have used.
func (t threadToolsApp) cancelRemoteRequest(ctx context.Context, caller threadtools.Caller, row store.ThreadRequest) (threadtools.CancelReport, error) {
	report := threadtools.CancelReport{Token: row.Token, ThreadID: row.TargetThreadID, ComputerID: row.TargetComputerID}
	if row.Notify {
		if _, err := t.app.store.SetThreadRequestNotify(row.Token, false); err != nil {
			return threadtools.CancelReport{}, err
		}
		row.Notify = false
	}
	if _, err := t.Peer(ctx, row.TargetComputerID); err != nil {
		return threadtools.CancelReport{}, err
	}
	call, cancel := context.WithTimeout(ctx, threadCancelTimeout)
	defer cancel()
	var reply ThreadPeerReply
	// The whole caller travels, computer included: the destination names
	// the thread and the computer a forwarded request came from, and a
	// cancel is a forwarded request like any other.
	source := caller
	if source.ThreadID == "" {
		source.ThreadID = row.CallerThreadID
	}
	err := t.app.backends.CallThreadPeer(call, row.TargetComputerID, "ThreadToolCall", &reply, ThreadPeerCall{
		Tool:   "thread_cancel",
		Token:  row.Token,
		Source: source,
	})
	if err != nil {
		return threadtools.CancelReport{}, t.app.threadOperationError("cancel", row.TargetComputerID, row.TargetThreadID, err)
	}
	report.Effect = reply.Effect
	if report.Effect == "" {
		report.Effect = threadtools.EffectNothing
	}
	if reply.Request != nil {
		if _, err := t.app.applyThreadPeerRequest(row.Token, row.TargetComputerID, *reply.Request); err != nil {
			return threadtools.CancelReport{}, err
		}
	}
	report.State = threadtools.RequestCancelled
	if settled, found, err := t.app.store.GetThreadRequest(row.Token); err == nil && found {
		report.State = settled.State
	}
	return report, nil
}

// cancelRemoteThread interrupts a thread on another computer the caller
// started there. The lineage check runs on the destination, against the
// receipts it holds for this source.
func (t threadToolsApp) cancelRemoteThread(ctx context.Context, caller threadtools.Caller, call threadtools.CancelCall) (threadtools.CancelReport, error) {
	if _, err := t.Peer(ctx, call.ComputerID); err != nil {
		return threadtools.CancelReport{}, err
	}
	args, err := json.Marshal(threadtools.CancelCall{ThreadID: call.ThreadID})
	if err != nil {
		return threadtools.CancelReport{}, fmt.Errorf("thread tools: encode cancel for %s: %w", call.ComputerID, err)
	}
	rpc, cancel := context.WithTimeout(ctx, threadCancelTimeout)
	defer cancel()
	var reply ThreadPeerReply
	err = t.app.backends.CallThreadPeer(rpc, call.ComputerID, "ThreadToolCall", &reply, ThreadPeerCall{
		Tool:   "thread_cancel",
		Args:   args,
		Token:  newThreadRequestToken(),
		Source: caller,
	})
	if err != nil {
		return threadtools.CancelReport{}, t.app.threadOperationError("cancel", call.ComputerID, call.ThreadID, err)
	}
	report := threadtools.CancelReport{
		ThreadID:   call.ThreadID,
		ComputerID: call.ComputerID,
		State:      threadtools.RequestRunning,
		Effect:     reply.Effect,
	}
	if report.Effect == "" || report.Effect == threadtools.EffectNothing {
		report.Effect = threadtools.EffectNothing
		report.State = threadtools.RequestFinished
	}
	return report, nil
}

// cancelThread interrupts a thread the caller started, sent to or asked. The
// lineage is the request ledger: without a row linking the two, a thread is
// none of this caller's business.
func (t threadToolsApp) cancelThread(ctx context.Context, caller threadtools.Caller, threadID string) (threadtools.CancelReport, error) {
	thread, err := t.localThread(threadID)
	if err != nil {
		return threadtools.CancelReport{}, err
	}
	linked, err := t.callerStartedThread(caller.ThreadID, threadID)
	if err != nil {
		return threadtools.CancelReport{}, err
	}
	if !linked {
		return threadtools.CancelReport{}, errorsx.Public(threadtools.CodeNotYours,
			fmt.Sprintf("This thread did not start %s, so it cannot interrupt it. Interrupt the threads you spawned, sent to or asked.", threadID), nil)
	}
	report := threadtools.CancelReport{ThreadID: thread.ID, State: threadtools.RequestRunning, Effect: threadtools.EffectNothing}
	live, err := t.LiveState(ctx, threadID)
	if err != nil {
		return threadtools.CancelReport{}, err
	}
	if !live.ActiveTurn {
		report.State = threadtools.RequestFinished
		return report, nil
	}
	if err := t.app.interruptTurnCtx(ctx, threadID); err != nil {
		return threadtools.CancelReport{}, err
	}
	report.Effect = threadtools.EffectInterrupted
	return report, nil
}

// interruptForeignThread is the destination half of a thread_cancel by
// thread id: the same lineage rule as cancelThread, read from the receipts
// this computer holds for the calling thread rather than from source rows
// it does not have.
func (t threadToolsApp) interruptForeignThread(ctx context.Context, origin threadRequestOrigin, threadID string) (string, error) {
	thread, err := t.localThread(threadID)
	if err != nil {
		return "", err
	}
	receipts, err := t.app.store.ListThreadRequestReceiptsForThread(thread.ID)
	if err != nil {
		return "", err
	}
	linked := false
	for _, receipt := range receipts {
		if receipt.OwnerDeviceID == origin.ownerDevice && receipt.SourceThreadID == origin.caller.ThreadID {
			linked = true
			break
		}
	}
	if !linked {
		return "", errorsx.Public(threadtools.CodeNotYours,
			fmt.Sprintf("That thread did not start %s, so it cannot interrupt it. Interrupt the threads you spawned, sent to or asked.", threadID), nil)
	}
	live, err := t.LiveState(ctx, thread.ID)
	if err != nil {
		return "", err
	}
	if !live.ActiveTurn {
		return threadtools.EffectNothing, nil
	}
	if err := t.app.interruptTurnCtx(ctx, thread.ID); err != nil {
		return "", err
	}
	return threadtools.EffectInterrupted, nil
}

// callerStartedThread reports whether any request, settled or not, links this
// caller to that thread.
func (t threadToolsApp) callerStartedThread(callerThreadID, threadID string) (bool, error) {
	offset := 0
	for offset < threadCancelLineageScan {
		rows, err := t.app.store.ListThreadRequestsByCaller(callerThreadID, threadCancelLineagePage, offset)
		if err != nil {
			return false, err
		}
		for _, row := range rows {
			if row.TargetThreadID == threadID || row.OriginThreadID == threadID {
				return true, nil
			}
		}
		if len(rows) < threadCancelLineagePage {
			return false, nil
		}
		offset += len(rows)
	}
	return false, nil
}

// threadCancelLineagePage and threadCancelLineageScan bound the lineage walk.
// A thread that made more requests than this and is asking about one older
// than all of them is past the point where an interrupt is the right tool.
const (
	threadCancelLineagePage = 200
	threadCancelLineageScan = 2000
)

// threadRequestExportAge is how long an answer file with no reader is kept.
// The tool tells the model to read it now; a file still there a day later was
// never read.
const threadRequestExportAge = 24 * time.Hour
