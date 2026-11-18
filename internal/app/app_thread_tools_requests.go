package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
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
//
// Releases are the exception: a wait registration held until the response
// is settled is released whether or not it was delivered, after the marks,
// so a wake it held off becomes owed only once the marks are recorded.
type threadToolsPending struct {
	mu       sync.Mutex
	fns      []func()
	releases []func()
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

func (p *threadToolsPending) hold(release func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.releases = append(p.releases, release)
}

// run executes what the call deferred, in order, once the response has been
// delivered. A response that never reached the model delivered nothing, so
// the list is dropped and said to be dropped. The releases run either way,
// last.
func (p *threadToolsPending) run(delivered bool) {
	p.mu.Lock()
	fns, releases := p.fns, p.releases
	p.fns, p.releases = nil, nil
	p.mu.Unlock()
	if delivered {
		for _, fn := range fns {
			fn()
		}
	} else if len(fns) > 0 {
		log.Printf("thread tools: the response was not delivered; %d deferred step(s) dropped", len(fns))
	}
	for _, release := range releases {
		release()
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

// releaseAfterResponse runs a wait's release once the tool response has been
// written or dropped, after the deliveries the call recorded. Callers defer it
// right after waitRequest returns, so a direct in-process call, which has no
// response to wait for, releases when it returns.
func (t threadToolsApp) releaseAfterResponse(ctx context.Context, release func()) {
	if pending, ok := ctx.Value(threadToolsPendingKey{}).(*threadToolsPending); ok {
		pending.hold(release)
		return
	}
	release()
}

// ackRequest waits for a freshly dispatched request as long as the call asked
// to, then renders what it knows.
func (t threadToolsApp) ackRequest(ctx context.Context, caller threadtools.Caller, token string, waitSeconds int) (threadtools.RequestAck, error) {
	row, release, err := t.waitForRequest(ctx, caller, token, waitSeconds, 0)
	defer t.releaseAfterResponse(ctx, release)
	if err != nil {
		return threadtools.RequestAck{}, err
	}
	state, err := t.requestState(ctx, row)
	if err != nil {
		return threadtools.RequestAck{}, err
	}
	ack := threadtools.RequestAck{
		RequestState: state,
		Outcome:      threadRequestOutcome(row, state.State == threadtools.RequestBlocked),
	}
	if row.Kind == store.ThreadRequestSpawn {
		// Recorded with the request when its thread was named, here or on
		// the computer that ran it, never read from the thread now.
		if ack.Instructions, err = t.app.store.ThreadRequestSpawnInstructions(token); err != nil {
			return threadtools.RequestAck{}, err
		}
	}
	return ack, nil
}

// waitForRequest parks on one token and returns the row as it stands
// afterwards, arming the wake when a positive wait ended with nothing. The
// caller renders the row and then hands the release to
// releaseAfterResponse.
func (t threadToolsApp) waitForRequest(
	ctx context.Context, caller threadtools.Caller, token string, waitSeconds int, afterRevision int64,
) (store.ThreadRequest, func(), error) {
	_, release, err := t.app.waitRequest(ctx, caller.ThreadID, []string{token}, waitSeconds,
		func() (string, bool, error) { return t.settledOrBlocked(token, afterRevision) })
	if err != nil {
		return store.ThreadRequest{}, release, err
	}
	row, found, err := t.readRequestAndArmNotify(token, waitSeconds > 0)
	if err != nil {
		return store.ThreadRequest{}, release, err
	}
	if !found {
		return store.ThreadRequest{}, release, errorsx.Public(threadtools.CodeRequestUnknown,
			"That request is no longer on this computer.", nil)
	}
	return row, release, nil
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
	if err := refuseForeignLedgerRead(ctx, "a token"); err != nil {
		return threadtools.StatusReport{}, err
	}
	rows, err := t.ownRequests(caller, call.Tokens)
	if err != nil {
		return threadtools.StatusReport{}, err
	}
	// Every token is waited on at once and the first to settle ends the
	// call: an agent watching three spawns wants the one that finished, not
	// the one it happened to list first.
	outcome, release, err := t.app.waitRequest(ctx, caller.ThreadID, call.Tokens, call.WaitSeconds, func() (string, bool, error) {
		for _, token := range call.Tokens {
			woke, done, err := t.settledOrBlocked(token, call.AfterRevision)
			if err != nil || done {
				return woke, done, err
			}
		}
		return "", false, nil
	})
	defer t.releaseAfterResponse(ctx, release)
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
		state, err := t.requestState(ctx, row)
		if err != nil {
			return threadtools.StatusReport{}, err
		}
		report.Requests = append(report.Requests, state)
	}
	return report, nil
}

// ownRequest reads one of the caller's own requests.
func (t threadToolsApp) ownRequest(caller threadtools.Caller, token string) (store.ThreadRequest, error) {
	rows, err := t.ownRequests(caller, []string{token})
	if err != nil {
		return store.ThreadRequest{}, err
	}
	return rows[0], nil
}

// ownRequests reads the caller's own requests in one query and returns them
// in the order the call named them, which is the order the report renders.
//
// A token that belongs to another thread is refused rather than answered: a
// request record says what another conversation is doing. An unknown token is
// refused too, so a status call names the one token it could not find instead
// of quietly reporting fewer rows than it was asked about.
func (t threadToolsApp) ownRequests(caller threadtools.Caller, tokens []string) ([]store.ThreadRequest, error) {
	found, err := t.app.store.GetThreadRequests(tokens)
	if err != nil {
		return nil, err
	}
	byToken := make(map[string]store.ThreadRequest, len(found))
	for _, row := range found {
		byToken[row.Token] = row
	}
	rows := make([]store.ThreadRequest, 0, len(tokens))
	for _, token := range tokens {
		row, ok := byToken[token]
		if !ok {
			return nil, errorsx.Public(threadtools.CodeRequestUnknown,
				fmt.Sprintf("No request on this computer carries token %s. List this thread's requests with thread_status and no arguments.", token), nil)
		}
		if row.CallerThreadID != caller.ThreadID {
			return nil, errorsx.Public(threadtools.CodeRequestNotYours,
				fmt.Sprintf("Token %s belongs to a request another thread made.", token), nil)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// watchThreads answers the thread_ids half of thread_status: the live state
// of threads the caller is interested in, waiting until one of them rests,
// stops to ask a person something, or can no longer be read here.
func (t threadToolsApp) watchThreads(ctx context.Context, caller threadtools.Caller, call threadtools.StatusCall) (threadtools.StatusReport, error) {
	keys := make([]string, 0, len(call.ThreadIDs))
	for _, threadID := range call.ThreadIDs {
		keys = append(keys, threadWatchKey(threadID))
	}
	outcome, release, err := t.app.waitRequest(ctx, caller.ThreadID, keys, call.WaitSeconds, func() (string, bool, error) {
		for _, threadID := range call.ThreadIDs {
			state := t.watchedThread(ctx, threadID)
			// A thread deleted or moved away will not rest here.
			if state.Error != "" || state.Resting || state.State == threadtools.StatePendingApproval || state.State == threadtools.StateAwaitingInput {
				return threadID, true, nil
			}
		}
		return "", false, nil
	})
	defer t.releaseAfterResponse(ctx, release)
	if err != nil {
		return threadtools.StatusReport{}, err
	}
	report := threadtools.StatusReport{WokeOn: outcome.WokeOn, TimedOut: outcome.TimedOut}
	for _, threadID := range call.ThreadIDs {
		report.Threads = append(report.Threads, t.watchedThread(ctx, threadID))
	}
	return report, nil
}

// watchedThread is one watched thread's row: its state as it stands, or why
// it cannot be read here, so a thread deleted or moved away during a wait
// costs its siblings nothing.
func (t threadToolsApp) watchedThread(ctx context.Context, threadID string) threadtools.ThreadState {
	state, err := t.threadStateNow(ctx, threadID)
	if err != nil {
		return threadtools.FailedRow(threadID, err)
	}
	return state
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
	out := threadtools.ThreadState{ThreadID: thread.ID, Title: thread.Title, State: threadtools.State(projected, live)}
	if threadtools.Resting(out.State) {
		// Its turn is over; it rests once nothing it started is left
		// (threadRest), the rest a spawn's request settles on.
		rest, err := t.app.threadRest(threadID)
		if err != nil {
			return threadtools.ThreadState{}, err
		}
		if rest.unrest == workerBlocked {
			// An async question nothing else shows.
			out.State = threadtools.StateAwaitingInput
		}
		out.WaitingOn = rest.waitingLine()
	}
	out.Resting = threadtools.Rests(out)
	return out, nil
}

// threadRequestStateWords and threadAnswerKindWords map the store's rows onto
// the vocabulary the tools speak. The two sets are spelled the same today and
// are owned by different packages, so the mapping is what keeps a rename on
// either side from quietly reaching the model as a word its tool schema never
// declared. `blocked` has no entry: it is derived at read time from the
// target's live state and never stored.
var (
	threadRequestStateWords = map[string]string{
		store.ThreadRequestUnconfirmed: threadtools.RequestUnconfirmed,
		store.ThreadRequestAccepted:    threadtools.RequestAccepted,
		store.ThreadRequestRunning:     threadtools.RequestRunning,
		store.ThreadRequestReplied:     threadtools.RequestReplied,
		store.ThreadRequestFinished:    threadtools.RequestFinished,
		store.ThreadRequestErrored:     threadtools.RequestErrored,
		store.ThreadRequestCancelled:   threadtools.RequestCancelled,
		store.ThreadRequestInterrupted: threadtools.RequestInterrupted,
		store.ThreadRequestExpired:     threadtools.RequestExpired,
		store.ThreadRequestRefused:     threadtools.RequestRefused,
	}
	threadAnswerKindWords = map[string]string{
		"":                      "",
		store.ThreadAnswerReply: threadtools.AnswerReply,
		store.ThreadAnswerFinal: threadtools.AnswerFinal,
		store.ThreadAnswerError: threadtools.AnswerError,
		store.ThreadAnswerNote:  threadtools.AnswerNote,
	}
)

func threadRequestStateWord(state string) string {
	return threadToolsWord("state", threadRequestStateWords, state)
}

func threadAnswerKindWord(kind string) string {
	return threadToolsWord("answer kind", threadAnswerKindWords, kind)
}

// threadToolsWord translates one stored value. An unmapped one is passed
// through rather than blanked, because the caller can still read it, and
// logged, because it means a vocabulary moved without this map.
func threadToolsWord(what string, words map[string]string, stored string) string {
	if word, ok := words[stored]; ok {
		return word
	}
	log.Printf("thread tools: request %s %q is not in the tools vocabulary", what, stored)
	return stored
}

// requestState renders one source row, and records the inline delivery that
// rendering it performs.
func (t threadToolsApp) requestState(ctx context.Context, row store.ThreadRequest) (threadtools.RequestState, error) {
	state := t.requestStateView(row)
	late := len(row.LateReply) > 0
	if len(state.Answer) > 0 && threadRequestSettled(row) && !threadRequestDelivered(row, late) {
		// Reading the answer here IS the delivery, but only once the model
		// has it: the mark runs after the response is written. An open row
		// can carry an answer before anything settles it (a reminder holds
		// its note from the moment it is armed), and reading that is not a
		// delivery: the wake it owes has not happened yet.
		claim, err := t.claimInlineDelivery(ctx, row, late)
		if err != nil {
			return threadtools.RequestState{}, err
		}
		switch {
		case claim.inline:
			state.Delivered = store.ThreadWakeInline
			return state, nil
		case claim.inFlight:
			// The reply still carries the answer, and says that the
			// message carrying it is on its way.
			state.Delivered, state.WakeQueued = store.ThreadWakeQueued, true
			return state, nil
		}
		row = claim.row
		state.Delivered = row.DeliveryRoute(late)
	}
	state.WakeQueued = t.wakeQueued(row, late)
	return state, nil
}

// requestStateView renders one source row as thread_status reports it, and
// does nothing else: it records no delivery, so a reader that is not the
// calling agent (the activity rail) can show a settled answer without
// taking the wake it owes.
func (t threadToolsApp) requestStateView(row store.ThreadRequest) threadtools.RequestState {
	// The newest revision is the answer: a late reply supersedes the final
	// message the caller was already told about.
	answer, late := row.Answer, len(row.LateReply) > 0
	if late {
		answer = row.LateReply
	}
	state := requestStateFrom(row, answer, late, t.requestTitle(row), t.app.threadRequestBlocked(row))
	state.WaitingOn = t.app.requestWaitingOn(row)
	return state
}

// requestStateFrom is requestStateView's derivation from what it reads:
// the newer answer and whether it is a late reply, the target's title and
// whether the target waits on a person. The Agents listing reads those for
// all its rows at once and derives each row here.
func requestStateFrom(row store.ThreadRequest, answer []byte, lateReply bool, title string, blocked bool) threadtools.RequestState {
	state := threadtools.RequestState{
		Token:      row.Token,
		Kind:       row.Kind,
		ThreadID:   row.TargetThreadID,
		ComputerID: row.TargetComputerID,
		Title:      title,
		State:      threadRequestStateWord(row.State),
		AnswerKind: threadAnswerKindWord(row.AnswerKind),
		Revision:   row.Revision,
		Notify:     row.Notify,
		Delivered:  row.DeliveryRoute(row.LateDeliveredAt != 0),
		ExpiresAt:  row.ExpiresAt,
		DueAt:      row.DueAt,
		Answer:     string(answer),
		WakeIssue:  row.WakeIssue,
		// Kept as a late reply supersedes it: the computer still could not
		// say whether the thread came to rest.
		RestUnreported: row.RestUnreported,
	}
	if lateReply {
		state.AnswerKind = threadtools.AnswerReply
	}
	if !threadRequestSettled(row) && blocked {
		state.State = threadtools.RequestBlocked
	}
	return state
}

// requestTitle names a request's target thread.
func (t threadToolsApp) requestTitle(row store.ThreadRequest) string {
	if row.TargetThreadID == "" {
		return ""
	}
	if row.TargetComputerID != "" {
		return requestTitleFrom(row, nil)
	}
	ids := []string{row.TargetThreadID}
	if row.OriginThreadID != "" {
		ids = append(ids, row.OriginThreadID)
	}
	names := map[string]store.ThreadName{}
	for _, id := range ids {
		if thread, err := t.app.store.GetThread(id); err == nil {
			names[id] = store.ThreadName{Title: thread.Title, Provider: thread.Provider}
			break
		}
	}
	return requestTitleFrom(row, names)
}

// requestTitleFrom names a request's target from the names of the threads
// it reads.
func requestTitleFrom(row store.ThreadRequest, names map[string]store.ThreadName) string {
	switch {
	case row.TargetThreadID == "":
		return ""
	case row.TargetComputerID != "":
		// A thread on another computer has no row here. Its title is what
		// that computer reported when the target was recorded.
		return row.TargetThreadTitle
	}
	if name, ok := names[row.TargetThreadID]; ok {
		return name.Title
	}
	if row.OriginThreadID != "" {
		// An ask's scratch fork is deleted once it has answered. The thread
		// it was forked from is what the caller asked about, and naming it
		// is more useful than naming nothing.
		return names[row.OriginThreadID].Title
	}
	return ""
}

// inlineClaim is what a reply found when it went to deliver an answer.
type inlineClaim struct {
	// inline is set when this reply delivers the answer.
	inline bool
	// inFlight is set when a wake is writing the answer's message now.
	inFlight bool
	// row is the request as read under the settle lock.
	row store.ThreadRequest
}

// claimInlineDelivery decides, under the token's settle lock, whether this
// call's reply delivers an answer. The row is read again because a wake can
// have queued the answer since the call read it, and the in-flight mark is
// read because a wake the collector decided before this call registered can
// be writing its message now. A claim holds the token until the reply is
// written, so a wake that starts after it backs off (claimThreadWake), and
// records the delivery once the reply is written.
func (t threadToolsApp) claimInlineDelivery(ctx context.Context, row store.ThreadRequest, late bool) (inlineClaim, error) {
	claim, release, err := func() (inlineClaim, func(), error) {
		unlock := t.app.threadRequestSettleLock(row.Token)
		defer unlock()
		current, found, err := t.app.store.GetThreadRequest(row.Token)
		if err != nil {
			return inlineClaim{}, nil, fmt.Errorf("thread tools: read request %s for delivery: %w", row.Token, err)
		}
		if !found {
			// Deleted with its caller thread since the call read it: no
			// wake can be owed, and the reply is all the agent gets.
			return inlineClaim{inline: true, row: row}, func() {}, nil
		}
		if threadRequestDelivered(current, late) {
			return inlineClaim{row: current}, nil, nil
		}
		if t.app.threadWakeInFlight(row.Token, late) {
			return inlineClaim{inFlight: true, row: current}, nil, nil
		}
		_, _, release := t.app.beginRequestWait(ctx, current.CallerThreadID, []string{row.Token})
		return inlineClaim{inline: true, row: current}, release, nil
	}()
	if err != nil || !claim.inline {
		return claim, err
	}
	// The mark before the release: a call with no response to wait for
	// runs both now, and the hold must outlast the mark.
	t.markDeliveredInline(ctx, row.Token, late)
	t.releaseAfterResponse(ctx, release)
	return claim, nil
}

// markDeliveredInline records that the model read an answer inside this
// call's own response: the state render and the file export are the same
// delivery, and both mark it only once the response is written, so nothing is
// recorded as delivered that the model never received.
func (t threadToolsApp) markDeliveredInline(ctx context.Context, token string, late bool) {
	t.afterResponse(ctx, func() {
		unlock := t.app.threadRequestSettleLock(token)
		defer unlock()
		if _, err := t.app.store.MarkThreadRequestDelivered(token, store.ThreadWakeInline, 0, late); err != nil {
			log.Printf("thread tools: mark request %s delivered inline: %v", token, err)
		}
	})
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
// A queued delivery is on its way from its mark until the provider's echo
// of the message, which is the model reading it: in the durable queue, and
// then in the provider's own queue until the turn boundary. The echo
// confirms the delivery on every request the message answers
// (store.ConfirmThreadWake); one whose confirmation failed to write is kept
// until it does (echoed). A give-back clears the mark.
func (t threadToolsApp) wakeQueued(row store.ThreadRequest, late bool) bool {
	if !threadRequestDelivered(row, late) || row.DeliveryRoute(late) != store.ThreadWakeQueued {
		return false
	}
	confirmed, carrier := row.ConfirmedAt, row.WakeCarrier()
	if late {
		confirmed, carrier = row.LateConfirmedAt, row.Token
	}
	return confirmed == 0 && !t.app.echoed(threadWakeSendIDFor(carrier, late))
}

// ListRequests lists the caller's own requests, open first then newest first.
func (t threadToolsApp) ListRequests(ctx context.Context, caller threadtools.Caller, call threadtools.ListCall) (threadtools.RequestListing, error) {
	if err := refuseForeignLedgerRead(ctx, "a thread's requests"); err != nil {
		return threadtools.RequestListing{}, err
	}
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
		state, err := t.requestState(ctx, row)
		if err != nil {
			return threadtools.RequestListing{}, err
		}
		listing.Requests = append(listing.Requests, state)
	}
	return listing, nil
}

// ExportAnswer writes one request's whole answer to this computer's export
// directory. The answer outlives the thread that wrote it, so this works
// after an ask's scratch fork is long gone.
func (t threadToolsApp) ExportAnswer(ctx context.Context, caller threadtools.Caller, token string) (threadtools.ExportFile, error) {
	if err := refuseForeignLedgerRead(ctx, "a token"); err != nil {
		return threadtools.ExportFile{}, err
	}
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
		// The file is the delivery unless a wake is already carrying the
		// answer; either way the agent asked for the file and gets it.
		if _, err := t.claimInlineDelivery(ctx, row, late); err != nil {
			return threadtools.ExportFile{}, err
		}
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
		return t.cancelRemoteSpawnedThread(ctx, caller, call)
	}
	return t.cancelThread(ctx, caller, call.ThreadID)
}

// cancelRemoteSpawnedThread is an agent's thread_cancel by thread id of a
// thread on a paired computer. The destination brings a thread the caller
// spawned there to rest; the caller's spawns of it are disarmed first, as a
// local one's are, since the agent reads what happened in the report. A
// destination that only interrupted, being older or the thread not a spawn,
// leaves the spawns as they were.
func (t threadToolsApp) cancelRemoteSpawnedThread(ctx context.Context, caller threadtools.Caller, call threadtools.CancelCall) (threadtools.CancelReport, error) {
	spawns, err := t.app.store.CallerSpawnsOf(caller.ThreadID, call.ComputerID, call.ThreadID)
	if err != nil {
		return threadtools.CancelReport{}, err
	}
	var disarmed []string
	for _, spawn := range spawns {
		if !spawn.Notify {
			continue
		}
		if _, err := t.app.store.SetThreadRequestNotify(spawn.Token, false); err != nil {
			return threadtools.CancelReport{}, err
		}
		disarmed = append(disarmed, spawn.Token)
	}
	report, err := t.cancelRemoteThread(ctx, caller, call, false)
	if err == nil && report.Effect == threadtools.EffectThreadStopped {
		report.State = threadtools.RequestCancelled
		return report, nil
	}
	for _, token := range disarmed {
		if _, rearmErr := t.app.store.SetThreadRequestNotify(token, true); rearmErr != nil {
			err = errors.Join(err, rearmErr)
		}
	}
	return report, err
}

func (t threadToolsApp) cancelRequest(ctx context.Context, caller threadtools.Caller, token string) (threadtools.CancelReport, error) {
	row, err := t.ownRequest(caller, token)
	if err != nil {
		return threadtools.CancelReport{}, err
	}
	report := threadtools.CancelReport{Token: token, ThreadID: row.TargetThreadID, ComputerID: row.TargetComputerID}
	// A cancelled request may still have work its first cancel did not reach
	// stopping; a repeat finishes it. Every other settled state is final.
	if threadRequestSettled(row) && (row.State != store.ThreadRequestCancelled || row.Kind == store.ThreadRequestRemind) {
		report.State, report.Effect = threadRequestStateWord(row.State), threadtools.EffectNothing
		return report, nil
	}
	if row.TargetComputerID != "" {
		return t.cancelRemoteRequest(ctx, caller, row, false)
	}
	// A reminder is nothing but a row: there is no work to stop, and the
	// caller cancelling it wants it gone rather than settled as cancelled.
	if row.Kind == store.ThreadRequestRemind {
		if _, err := t.app.dropThreadReminder(row); err != nil {
			return threadtools.CancelReport{}, err
		}
		report.State, report.Effect = threadtools.RequestCancelled, threadtools.EffectReminderDropped
		return report, nil
	}

	// The caller is cancelling its own request, and the report it is about
	// to read says what happened. The cancel's settlement disarms the wake
	// (stopThreadRequestWork), so the cancellation does not also arrive later
	// as an unread message, and a cancel that fails leaves it armed.
	effect, err := t.app.stopThreadRequestWork(ctx, token)
	if err != nil {
		return threadtools.CancelReport{}, err
	}
	report.Effect = effect
	report.State = threadtools.RequestCancelled
	if settled, found, err := t.app.store.GetThreadRequest(token); err == nil && found {
		report.State = threadRequestStateWord(settled.State)
	}
	return report, nil
}

// threadCancelTimeout bounds one forwarded cancel. It matches the remote
// command stop: a cancel that cannot be delivered must not hold the calling
// turn while an offline computer times out its whole call budget
// (threadPeerCallWait). The destination settles its own receipt, so a cancel
// whose answer is lost to the bound is collected by the next poll.
const threadCancelTimeout = 20 * time.Second

// cancelRemoteRequest stops one request on the computer that accepted it.
//
// The destination settles its own receipt, because it is the only side that
// can take back a queued message or interrupt a turn. The source row is
// settled from the receipt the reply carries, through the same collector a
// poll would have used.
//
// userStop is a person's Stop in the Agents tray: the wake stays armed, so
// the caller's agent is told, and the destination words the settlement as
// the user's.
func (t threadToolsApp) cancelRemoteRequest(ctx context.Context, caller threadtools.Caller, row store.ThreadRequest, userStop bool) (threadtools.CancelReport, error) {
	report := threadtools.CancelReport{Token: row.Token, ThreadID: row.TargetThreadID, ComputerID: row.TargetComputerID, RemoteRequest: true}
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
		Tool:     "thread_cancel",
		Token:    row.Token,
		Source:   source,
		UserStop: userStop,
	})
	if err != nil {
		// The wake stays armed: the destination was not reached, so the work
		// is still running there and the caller is still owed its answer. A
		// cancel that disarmed the notify first would leave a request that
		// finishes later with nobody to tell.
		return threadtools.CancelReport{}, t.app.threadOperationError("cancel", row.TargetComputerID, row.TargetThreadID, err)
	}
	// Disarmed only now, and before the reply's settlement is applied: the
	// report the caller is about to read says what happened, so the same
	// cancellation must not arrive again as an unread message.
	if row.Notify && !userStop {
		if _, err := t.app.store.SetThreadRequestNotify(row.Token, false); err != nil {
			return threadtools.CancelReport{}, err
		}
	}
	report.Effect = reply.Effect
	if report.Effect == "" {
		report.Effect = threadtools.EffectNothing
	}
	if reply.Request != nil {
		settlement := *reply.Request
		// A person stopped it: the source says so whatever the destination
		// wrote, since a computer that predates the user Stop words the
		// cancel as the sender's.
		if userStop && settlement.State == store.ThreadReceiptCancelled {
			settlement.Answer = threadRequestUserStop
		}
		if err := t.app.collectThreadPeerReply(row.Token, row.TargetComputerID, settlement); err != nil {
			return threadtools.CancelReport{}, err
		}
	}
	report.State = threadtools.RequestCancelled
	if settled, found, err := t.app.store.GetThreadRequest(row.Token); err == nil && found {
		report.State = threadRequestStateWord(settled.State)
	}
	return report, nil
}

// cancelRemoteThread interrupts a thread on another computer the caller
// started there. The lineage check runs on the destination, against the
// receipts it holds for this source.
//
// userStop marks a Stop a person made from the caller's Agents tray, so the
// stopped thread's timeline names the user and not the caller's agent.
func (t threadToolsApp) cancelRemoteThread(ctx context.Context, caller threadtools.Caller, call threadtools.CancelCall, userStop bool) (threadtools.CancelReport, error) {
	if _, err := t.Peer(ctx, call.ComputerID); err != nil {
		return threadtools.CancelReport{}, err
	}
	args, err := json.Marshal(threadtools.CancelCall{ThreadID: call.ThreadID})
	if err != nil {
		return threadtools.CancelReport{}, fmt.Errorf("thread tools: encode cancel for %s: %w", call.ComputerID, err)
	}
	// The destination checks lineage against the thread that made the
	// request, which a handoff may have replaced with this caller.
	source := caller
	if madeIn, err := t.app.store.RemoteRequestSource(caller.ThreadID, call.ComputerID, call.ThreadID); err != nil {
		return threadtools.CancelReport{}, err
	} else if madeIn != "" && madeIn != caller.ThreadID {
		source = t.app.threadAgentCaller(madeIn)
	}
	rpc, cancel := context.WithTimeout(ctx, threadCancelTimeout)
	defer cancel()
	var reply ThreadPeerReply
	err = t.app.backends.CallThreadPeer(rpc, call.ComputerID, "ThreadToolCall", &reply, ThreadPeerCall{
		Tool:     "thread_cancel",
		Args:     args,
		Token:    newThreadRequestToken(),
		Source:   source,
		UserStop: userStop,
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

// cancelThread stops a thread the caller started, sent to or asked. The
// lineage is the request ledger: without a row linking the two, a thread is
// none of this caller's business. A thread the caller spawned is brought to
// rest (restSpawnedThread); one it only sent to or asked has its running
// turn interrupted.
func (t threadToolsApp) cancelThread(ctx context.Context, caller threadtools.Caller, threadID string) (threadtools.CancelReport, error) {
	thread, err := t.localThread(threadID)
	if err != nil {
		return threadtools.CancelReport{}, err
	}
	spawns, err := t.app.store.CallerSpawnsOf(caller.ThreadID, "", thread.ID)
	if err != nil {
		return threadtools.CancelReport{}, err
	}
	if len(spawns) > 0 {
		return t.restSpawnedThread(ctx, thread.ID, spawns, threadRestStop(caller.Title, caller.ThreadID, ""))
	}
	linked, err := t.app.store.CallerRequestedOf(caller.ThreadID, thread.ID)
	if err != nil {
		return threadtools.CancelReport{}, err
	}
	if !linked {
		return threadtools.CancelReport{}, threadNotYoursToInterrupt(threadID)
	}
	effect, err := t.interruptRunningThread(ctx, thread.ID, stopByThread(caller.Title, caller.ThreadID, ""))
	if err != nil {
		return threadtools.CancelReport{}, err
	}
	report := threadtools.CancelReport{ThreadID: thread.ID, State: threadtools.RequestRunning, Effect: effect}
	if effect == threadtools.EffectNothing {
		report.State = threadtools.RequestFinished
	}
	return report, nil
}

// interruptRunningThread is what both cancel-by-thread doors do once lineage
// is settled: a thread with no turn in flight has nothing to stop, and one
// that is working gets that turn interrupted.
//
// "In flight" is the turn the interrupt itself would cut
// (SampleInterrupt): the open turn after any turn start in progress, or
// the turn of a send the provider already holds. The live state's active
// round opens later than both, so a stop gated on it would report nothing
// to stop for a turn that then runs to its end.
func (t threadToolsApp) interruptRunningThread(ctx context.Context, threadID string, stop turnStop) (string, error) {
	if t.app.triage == nil || t.app.triage.SampleInterrupt(threadID).CutTurn() < 0 {
		return threadtools.EffectNothing, nil
	}
	if err := t.app.interruptTurnCtx(ctx, threadID, stop); err != nil {
		return "", err
	}
	return threadtools.EffectInterrupted, nil
}

// interruptForeignThread is the destination half of a thread_cancel by
// thread id: the same lineage rule as cancelThread, read from the receipts
// this computer holds for the calling thread rather than from source rows
// it does not have. A thread the caller spawned here is brought to rest; the
// source disarmed its own spawn's wake before it asked.
func (t threadToolsApp) interruptForeignThread(ctx context.Context, origin threadRequestOrigin, threadID string, userStop bool) (string, error) {
	thread, err := t.localThread(threadID)
	if err != nil {
		return "", err
	}
	receipts, err := t.app.store.ListThreadRequestReceiptsForThread(thread.ID)
	if err != nil {
		return "", err
	}
	linked, spawned := false, false
	for _, receipt := range receipts {
		if receipt.OwnerDeviceID == origin.ownerDevice && receipt.SourceThreadID == origin.caller.ThreadID {
			linked = true
			spawned = spawned || receipt.Kind == store.ThreadRequestSpawn
		}
	}
	if !linked {
		return "", threadNotYoursToInterrupt(threadID)
	}
	if spawned {
		by := userRestStop()
		if !userStop {
			by = threadRestStop(origin.caller.Title, origin.caller.ThreadID, t.app.stoppingComputerName(origin.caller.ComputerID))
		}
		report, err := t.restSpawnedThread(ctx, thread.ID, nil, by)
		return report.Effect, err
	}
	return t.interruptRunningThread(ctx, thread.ID, t.foreignThreadStop(origin.caller, userStop))
}

// foreignThreadStop is who a paired computer's thread-id cancel names as
// stopping the turn: the user for a Stop from the Agents tray, otherwise
// the thread that made the call.
func (t threadToolsApp) foreignThreadStop(caller threadtools.Caller, userStop bool) turnStop {
	if userStop {
		return stopFromAgentsTray()
	}
	return stopByThread(caller.Title, caller.ThreadID, t.app.stoppingComputerName(caller.ComputerID))
}

// threadNotYoursToInterrupt is the refusal both lineage checks give. The
// caller reads it either way, so it says the same thing on both computers.
func threadNotYoursToInterrupt(threadID string) error {
	return errorsx.Public(threadtools.CodeNotYours, fmt.Sprintf(
		"This thread did not start %s, so it cannot interrupt it. Interrupt the threads you spawned, sent to or asked.",
		threadID), nil)
}

// restSpawnedThread is a thread_cancel by thread id of a thread the caller
// spawned: the caller's spawns of it are disarmed, since the calling agent
// reads what happened in this call's report, and the thread is brought to
// rest with the stop attributed as by names it. A step that failed fails the
// call; the steps after it still ran.
func (t threadToolsApp) restSpawnedThread(ctx context.Context, threadID string, spawns []store.ThreadRequest, by restStop) (threadtools.CancelReport, error) {
	var errs []error
	for _, spawn := range spawns {
		if !spawn.Notify {
			continue
		}
		if _, err := t.app.store.SetThreadRequestNotify(spawn.Token, false); err != nil {
			errs = append(errs, fmt.Errorf("disarm the wake of %s: %w", spawn.Token, err))
		}
	}
	var result ThreadAgentStop
	t.app.restThreadHere(ctx, threadID, by, map[string]bool{threadID: true}, &result)
	for _, failure := range result.Failures {
		errs = append(errs, errors.New(failure))
	}
	if err := errors.Join(errs...); err != nil {
		return threadtools.CancelReport{}, fmt.Errorf("thread tools: stop thread %s: %w", threadID, err)
	}
	return threadtools.CancelReport{ThreadID: threadID, State: threadtools.RequestCancelled, Effect: threadtools.EffectThreadStopped}, nil
}

// threadCancelLineagePage bounds one read of the requests a caller made.
const threadCancelLineagePage = 200

// threadRequestExportAge is how long an answer file with no reader is kept.
// The tool tells the model to read it now; a file still there a day later was
// never read.
const threadRequestExportAge = 24 * time.Hour
