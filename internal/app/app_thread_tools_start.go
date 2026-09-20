package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadapp"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/usermessage"
)

// thread_spawn, thread_send and thread_ask: the three calls that start work
// in another thread. All three mint a request, so all three write a source
// row, accept a receipt and dispatch a message; what differs is only which
// thread the message goes to and how that thread came to exist.
//
// The order is receipt first, provider second, everywhere. A crash between
// them leaves a receipt with no turn, which the boot sweep settles
// `interrupted`: the caller learns the computer restarted. The reverse order
// would leave a running thread that no request names, which nothing can
// settle and nobody can cancel.

// threadRequestSendIDPrefix names the send a request wrote, so a cancel can
// find its queued message and a retry cannot send it twice.
const threadRequestSendIDPrefix = "thread-request:"

func threadRequestSendID(token string) string { return threadRequestSendIDPrefix + token }

// threadReceiptLocalOwner is the owner device of a receipt this computer
// accepted from one of its own threads. The authorization input of a receipt
// is the authenticated device that sent it; an in-process call has none.
const threadReceiptLocalOwner = "local"

// newThreadRequestToken mints the request's identity. It is the idempotency
// key of both tables and the string the model carries between calls, so it is
// minted once, by the caller, and never recomputed by a retry.
func newThreadRequestToken() string { return entityid.New() }

// threadRequestTurnKey names the turn a receipt's message was consumed by.
//
// It is not the provider's wire turn id: Claude has none, and the `turns`
// row's own id is not knowable at send time because it is minted when the
// turn starts. The AO turn index is, and it is stable for the life of the
// thread. One helper writes and reads it so the two sides cannot drift.
func threadRequestTurnKey(threadID string, turnIndex int) string {
	return threadID + "#" + strconv.Itoa(turnIndex)
}

// threadRequestTurnIndex reads back what threadRequestTurnKey wrote. A key
// from another thread or a malformed row reports false rather than a wrong
// turn.
func threadRequestTurnIndex(threadID, key string) (int, bool) {
	prefix := threadID + "#"
	if !strings.HasPrefix(key, prefix) {
		return 0, false
	}
	index, err := strconv.Atoi(key[len(prefix):])
	if err != nil || index < 0 {
		return 0, false
	}
	return index, true
}

// threadRequestOrigin is who a request is being accepted for.
//
// A local request's caller thread lives here, so everything about it is
// readable from the store. A request a paired computer forwarded has no
// row here at all, so the three things this computer would otherwise read
// from the caller travel with the call: its identity, its latest user
// message for the footer, and the authenticated device that owns the
// receipt.
type threadRequestOrigin struct {
	caller threadtools.Caller
	// foreign is true when the caller thread lives on another computer.
	foreign bool
	// ownerDevice authorizes the receipt: the authenticated device of the
	// calling computer, or threadReceiptLocalOwner for an in-process call.
	ownerDevice string
	// userMessage is the caller thread's latest message no agent wrote.
	// Read from the store for a local caller, carried for a foreign one.
	userMessage string
	// inherit is what a spawn takes from the calling thread where the call
	// overrides nothing. Project and checkout are not in it: they are
	// registered per computer and a spawn elsewhere names its own.
	inherit threadtools.SpawnDefaults
}

// localOrigin is the origin of a request one of this computer's own
// threads made. The caller thread is read back so a request is never
// accepted for a thread that has gone, and so a spawn inherits its
// provider settings from the row rather than from the tool call.
func (t threadToolsApp) localOrigin(caller threadtools.Caller) (threadRequestOrigin, error) {
	thread, err := t.localThread(caller.ThreadID)
	if err != nil {
		return threadRequestOrigin{}, err
	}
	return threadRequestOrigin{
		caller:      caller,
		ownerDevice: threadReceiptLocalOwner,
		inherit: threadtools.SpawnDefaults{
			Provider:    thread.Provider,
			Model:       thread.Model,
			Effort:      thread.ReasoningEffort,
			Mode:        thread.Mode,
			RuntimeMode: thread.RuntimeMode,
		},
	}, nil
}

// Spawn creates a visible thread, sends the prompt as its first message and
// returns the receipt.
func (t threadToolsApp) Spawn(ctx context.Context, caller threadtools.Caller, call threadtools.SpawnCall) (threadtools.RequestAck, error) {
	if t.remoteDestination(call.ComputerID) {
		return t.startRemoteRequest(ctx, caller, store.ThreadRequest{
			Kind:           store.ThreadRequestSpawn,
			OriginThreadID: call.FromThread,
			Notify:         call.Notify,
		}, call.ComputerID, "thread_spawn", call, call.WaitSeconds)
	}
	origin, err := t.localOrigin(caller)
	if err != nil {
		return threadtools.RequestAck{}, err
	}
	// Everything this computer can refuse is refused BEFORE the row exists,
	// so an unknown provider or project costs the model one call and leaves
	// no request behind to explain.
	if _, err := t.spawnThreadOptions(origin, call); err != nil {
		return threadtools.RequestAck{}, err
	}

	token := newThreadRequestToken()
	if err := t.app.store.InsertThreadRequest(store.ThreadRequest{
		Token:          token,
		CallerThreadID: caller.ThreadID,
		Kind:           store.ThreadRequestSpawn,
		OriginThreadID: call.FromThread,
		Notify:         call.Notify,
		State:          store.ThreadRequestUnconfirmed,
	}); err != nil {
		return threadtools.RequestAck{}, err
	}
	if _, err := t.acceptSpawn(ctx, origin, token, call); err != nil {
		return threadtools.RequestAck{}, err
	}
	return t.ackRequest(ctx, caller, token, call.WaitSeconds)
}

// acceptSpawn is the destination half of a spawn: create the thread,
// accept the receipt and send the prompt there.
//
// It runs unchanged for a spawn one of this computer's own threads made
// and for one a paired computer forwarded. Only the origin differs, and
// the source row it advances exists only on the computer that made the
// request.
func (t threadToolsApp) acceptSpawn(ctx context.Context, origin threadRequestOrigin, token string, call threadtools.SpawnCall) (string, error) {
	create, err := t.spawnThreadOptions(origin, call)
	if err != nil {
		return "", t.failRequest(token, err)
	}
	target, fresh, err := t.acceptRequest(origin, token, store.ThreadRequestSpawn, "", func() (store.Thread, error) {
		return t.createSpawnedThread(ctx, call, create)
	})
	if err != nil {
		return "", t.failRequest(token, err)
	}
	if !fresh {
		// A retry of a request this computer already accepted. The thread
		// exists and its prompt has already been sent or already settled;
		// nothing here runs a second time.
		return target, nil
	}
	if err := t.dispatchRequest(ctx, origin, token, target, call.Prompt, answerRequested(call.WaitSeconds, call.Notify)); err != nil {
		return "", t.failRequest(token, err)
	}
	return target, nil
}

// Send continues an existing thread as if the user had typed the message.
func (t threadToolsApp) Send(ctx context.Context, caller threadtools.Caller, call threadtools.SendCall) (threadtools.RequestAck, error) {
	if t.remoteDestination(call.ComputerID) {
		return t.startRemoteRequest(ctx, caller, store.ThreadRequest{
			Kind:           store.ThreadRequestSend,
			TargetThreadID: call.ThreadID,
			Notify:         call.Notify,
		}, call.ComputerID, "thread_send", call, call.WaitSeconds)
	}
	origin, err := t.localOrigin(caller)
	if err != nil {
		return threadtools.RequestAck{}, err
	}
	// Rechecked here and not only in the tools layer: resolution and this
	// call are separate moments, and a thread that became the caller's own
	// in between would queue a message only its own turn could answer.
	if err := t.refuseSelf(caller, call.ThreadID, "send to"); err != nil {
		return threadtools.RequestAck{}, err
	}
	target, err := t.sendTarget(call.ThreadID)
	if err != nil {
		return threadtools.RequestAck{}, err
	}

	token := newThreadRequestToken()
	if err := t.app.store.InsertThreadRequest(store.ThreadRequest{
		Token:          token,
		CallerThreadID: caller.ThreadID,
		Kind:           store.ThreadRequestSend,
		TargetThreadID: target.ID,
		Notify:         call.Notify,
		State:          store.ThreadRequestUnconfirmed,
	}); err != nil {
		return threadtools.RequestAck{}, err
	}
	if _, err := t.acceptSend(ctx, origin, token, call); err != nil {
		return threadtools.RequestAck{}, err
	}
	return t.ackRequest(ctx, caller, token, call.WaitSeconds)
}

// acceptSend is the destination half of a send: accept the receipt against
// the named thread and queue the message there.
func (t threadToolsApp) acceptSend(ctx context.Context, origin threadRequestOrigin, token string, call threadtools.SendCall) (string, error) {
	target, err := t.sendTarget(call.ThreadID)
	if err != nil {
		return "", t.failRequest(token, err)
	}
	if origin.foreign && target.ID == origin.caller.ThreadID {
		// A thread id that names the caller can only mean two threads with
		// the same id on two computers, which the ids rule out; refuse it
		// rather than queue a message a thread would answer for itself.
		return "", t.failRequest(token, errorsx.Public(threadtools.CodeSelfSend,
			"A thread cannot send to itself.", nil))
	}
	_, fresh, err := t.acceptRequest(origin, token, store.ThreadRequestSend, target.ID, nil)
	if err != nil {
		return "", t.failRequest(token, err)
	}
	if !fresh {
		return target.ID, nil
	}
	if err := t.dispatchRequest(ctx, origin, token, target.ID, call.Message, answerRequested(call.WaitSeconds, call.Notify)); err != nil {
		return "", t.failRequest(token, err)
	}
	return target.ID, nil
}

// sendTarget reads the thread a send addresses and proves this computer can
// run work in it, before anything durable is written for the request.
func (t threadToolsApp) sendTarget(threadID string) (store.Thread, error) {
	target, err := t.localThread(threadID)
	if err != nil {
		return store.Thread{}, err
	}
	if err := t.app.store.CheckThreadExecutionAccess(target); err != nil {
		return store.Thread{}, errorsx.Public(threadtools.CodeNotFound,
			fmt.Sprintf("Thread %s cannot run work on this computer: %v", target.ID, err), err)
	}
	return target, nil
}

// Ask forks the target at its tail into a hidden read-only scratch thread
// and puts the question there.
//
// The fork is what makes an ask safe to send into a thread that is busy: the
// scratch thread has the target's whole history and none of its future, so
// the answer is about what that thread knows without the question ever
// landing in its transcript.
func (t threadToolsApp) Ask(ctx context.Context, caller threadtools.Caller, call threadtools.AskCall) (threadtools.RequestAck, error) {
	if t.remoteDestination(call.ComputerID) {
		return t.startRemoteRequest(ctx, caller, store.ThreadRequest{
			Kind:           store.ThreadRequestAsk,
			OriginThreadID: call.ThreadID,
			Notify:         call.Notify,
		}, call.ComputerID, "thread_ask", call, call.WaitSeconds)
	}
	origin, err := t.localOrigin(caller)
	if err != nil {
		return threadtools.RequestAck{}, err
	}
	if err := t.refuseSelf(caller, call.ThreadID, "ask"); err != nil {
		return threadtools.RequestAck{}, err
	}
	source, err := t.localThread(call.ThreadID)
	if err != nil {
		return threadtools.RequestAck{}, err
	}

	token := newThreadRequestToken()
	if err := t.app.store.InsertThreadRequest(store.ThreadRequest{
		Token:          token,
		CallerThreadID: caller.ThreadID,
		Kind:           store.ThreadRequestAsk,
		OriginThreadID: source.ID,
		Notify:         call.Notify,
		State:          store.ThreadRequestUnconfirmed,
	}); err != nil {
		return threadtools.RequestAck{}, err
	}
	if _, err := t.acceptAsk(ctx, origin, token, call); err != nil {
		return threadtools.RequestAck{}, err
	}
	return t.ackRequest(ctx, caller, token, call.WaitSeconds)
}

// acceptAsk is the destination half of an ask: fork the named thread into
// the hidden scratch thread the question is answered in.
func (t threadToolsApp) acceptAsk(ctx context.Context, origin threadRequestOrigin, token string, call threadtools.AskCall) (string, error) {
	source, err := t.localThread(call.ThreadID)
	if err != nil {
		return "", t.failRequest(token, err)
	}
	target, fresh, err := t.acceptRequest(origin, token, store.ThreadRequestAsk, "", func() (store.Thread, error) {
		return t.forkScratchThread(ctx, source, token)
	})
	if err != nil {
		return "", t.failRequest(token, err)
	}
	if !fresh {
		return target, nil
	}
	// An ask always wants an answer: that is what distinguishes it from a
	// send, and the footer must say so however the wait was configured.
	if err := t.dispatchRequest(ctx, origin, token, target, call.Question, true); err != nil {
		return "", t.failRequest(token, err)
	}
	return target, nil
}

// startRemoteRequest is the source half of a spawn, send or ask on another
// computer.
//
// The row is written `unconfirmed` first, so a reply lost on the way back
// leaves a token the poller can ask about rather than work nobody records.
// The destination owns everything the row cannot say: which thread ran, and
// what it answered.
func (t threadToolsApp) startRemoteRequest(
	ctx context.Context, caller threadtools.Caller, row store.ThreadRequest,
	computerID, tool string, call any, waitSeconds int,
) (threadtools.RequestAck, error) {
	origin, err := t.localOrigin(caller)
	if err != nil {
		return threadtools.RequestAck{}, err
	}
	peer, err := t.Peer(ctx, computerID)
	if err != nil {
		return threadtools.RequestAck{}, err
	}
	args, err := json.Marshal(call)
	if err != nil {
		return threadtools.RequestAck{}, fmt.Errorf("thread tools: encode %s for %s: %w", tool, computerID, err)
	}
	latest, _, err := t.app.store.LatestHumanUserText(caller.ThreadID)
	if err != nil {
		return threadtools.RequestAck{}, err
	}

	row.Token = newThreadRequestToken()
	row.CallerThreadID = caller.ThreadID
	row.TargetComputerID = computerID
	row.State = store.ThreadRequestUnconfirmed
	// The poll is the recovery path for a reply that never arrives, and it
	// must not run while this call is still being attempted: a destination
	// that has not been asked yet answers `unknown`, which for an
	// unconfirmed row means refused. The fence covers the whole attempt
	// sequence and is cut to the normal delay the moment the call returns,
	// which is what makes "no retry is in flight" durable.
	row.NextCheck = time.Now().Add(threadRequestAdmissionFence).UnixMilli()
	if err := t.app.store.InsertThreadRequest(row); err != nil {
		return threadtools.RequestAck{}, err
	}
	reply, uncertain, err := t.app.callThreadPeerRequest(ctx, peer.Computer(), ThreadPeerCall{
		Tool:        tool,
		Args:        args,
		Token:       row.Token,
		Source:      caller,
		UserMessage: latest,
		Inherit:     origin.inherit,
	})
	issue := ""
	if err != nil {
		_, issue, _ = threadErrorDetails(tool, err)
	}
	t.app.rescheduleThreadRequest(row, threadPollNormalDelay, issue)
	if err != nil {
		if !uncertain {
			return threadtools.RequestAck{}, t.settleUnconfirmedRequest(row.Token, err)
		}
		// Every attempt ended without an answer, so the destination may be
		// running this request. The model is given the token and told to
		// check it rather than an error it would answer by starting the
		// same work again; the poller reconciles the row either way.
		return t.ackRequest(ctx, caller, row.Token, waitSeconds)
	}
	if _, err := t.app.applyThreadPeerRequest(row.Token, computerID, reply); err != nil {
		return threadtools.RequestAck{}, err
	}
	return t.ackRequest(ctx, caller, row.Token, waitSeconds)
}

// threadPeerAdmissionAttempts is how many times one request-minting call is
// sent before the source stops waiting to hear whether it was accepted.
// Every attempt carries the same token, so a destination that already
// accepted answers with the acceptance it holds instead of starting a
// second piece of work.
const threadPeerAdmissionAttempts = 3

// threadRequestAdmissionFence keeps a freshly written source row out of the
// poller for as long as the attempts can take, plus the normal poll delay.
const threadRequestAdmissionFence = threadPeerCallTimeout*threadPeerAdmissionAttempts + threadPollNormalDelay

// callThreadPeerRequest forwards one request-minting call and returns the
// receipt the destination reports.
//
// A failure that leaves the acceptance unknown is retried with the same
// token, because what was lost is the reply and not the work. `uncertain`
// is true when every attempt ended that way: the destination may be running
// the request, so the row stays open for the poller rather than being
// refused here.
func (a *App) callThreadPeerRequest(ctx context.Context, computer threadtools.Computer, call ThreadPeerCall) (ThreadPeerRequest, bool, error) {
	for attempt := 1; ; attempt++ {
		rpc, cancel := context.WithTimeout(ctx, threadPeerCallTimeout)
		var reply ThreadPeerReply
		err := a.backends.CallThreadPeer(rpc, computer.ID, "ThreadToolCall", &reply, call)
		cancel()
		if err != nil {
			code, _, uncertain := threadErrorDetails(call.Tool, err)
			if uncertain && !threadRequestNeverSent(code) && attempt < threadPeerAdmissionAttempts {
				continue
			}
			return ThreadPeerRequest{}, uncertain, a.threadOperationError(call.Tool, computer.ID, "", err)
		}
		if reply.Request == nil {
			return ThreadPeerRequest{}, false, errorsx.Public(threadtools.CodeUnreachable,
				fmt.Sprintf("%s accepted %s without reporting the request.", nameOfComputer(computer), call.Tool), nil)
		}
		return *reply.Request, false, nil
	}
}

// settleUnconfirmedRequest ends a remote request whose call failed with an
// answer the destination gave, and returns the original error.
//
// A refusal raised before the destination could see the call is `refused`:
// nothing ran there, and the row would otherwise be polled forever against
// a computer that already said why. A refusal the destination itself wrote
// is left `unconfirmed` for the poller, which reads that computer's own
// record of the token rather than trusting one failed call. A call whose
// answer never came back does not reach here at all: it is unconfirmed to
// the model, with the token.
func (t threadToolsApp) settleUnconfirmedRequest(token string, cause error) error {
	code, message, _ := threadErrorDetails("request", cause)
	if !threadRequestNeverSent(code) {
		return cause
	}
	if _, err := t.app.store.SettleThreadRequest(token, store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
		State:      store.ThreadRequestRefused,
		Answer:     []byte(message),
		AnswerKind: store.ThreadAnswerError,
	}); err != nil {
		log.Printf("thread tools: settle unsent request %s: %v", token, err)
	}
	return cause
}

// answerRequested is what the footer's wording turns on: the sender waits, or
// asked to be told later. Neither means nobody will read a reply, which is why
// the token is given either way.
func answerRequested(waitSeconds int, notify bool) bool { return waitSeconds > 0 || notify }

func (t threadToolsApp) refuseSelf(caller threadtools.Caller, threadID, verb string) error {
	if threadID != caller.ThreadID {
		return nil
	}
	return errorsx.Public(threadtools.CodeSelfSend,
		fmt.Sprintf("A thread cannot %s itself. Use thread_remind to wake yourself later, or thread_spawn to hand the work to a new thread.", verb), nil)
}

// acceptRequest writes the destination row, creates the thread that answers
// when the kind has one to create, and advances the source row when it is on
// this computer.
//
// The receipt is the record of record: a retry with the same token returns
// the existing acceptance and `create` never runs again, which is what makes
// a lost reply safe to retry with no second spawn, fork or message. `fresh`
// is false for exactly that retry, so the dispatch does not run twice
// either.
func (t threadToolsApp) acceptRequest(
	origin threadRequestOrigin, token, kind, targetThreadID string,
	create func() (store.Thread, error),
) (target string, fresh bool, err error) {
	caller := origin.caller
	receipt, created, err := t.app.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
		Token:              token,
		OwnerDeviceID:      origin.ownerDevice,
		SourceComputerID:   caller.ComputerID,
		SourceComputerName: caller.ComputerName,
		SourceThreadID:     caller.ThreadID,
		SourceThreadTitle:  caller.Title,
		Kind:               kind,
		TargetThreadID:     targetThreadID,
	})
	if err != nil {
		return "", false, err
	}
	target = receipt.TargetThreadID
	if target == "" && create != nil {
		if !created {
			// The receipt exists with no thread: the attempt that accepted
			// it died between the two writes. The boot sweep settles it, and
			// a second thread now would be a second piece of work nobody
			// asked for.
			return "", false, errorsx.Public(threadtools.CodeInvalidRequest,
				"That request was accepted but its thread was never created. It is settled as interrupted; make the request again.", nil)
		}
		thread, err := create()
		if err != nil {
			return "", false, err
		}
		target = thread.ID
		if _, err := t.app.store.SetThreadReceiptTarget(token, target); err != nil {
			return "", false, err
		}
	}
	if target == "" {
		return "", false, fmt.Errorf("thread tools: request %s has no target thread", token)
	}
	if !origin.foreign {
		if _, err := t.app.store.SetThreadRequestTarget(token, "", target); err != nil {
			return "", false, err
		}
		// Conditional: a request that already settled during dispatch keeps
		// its settled state, and a no-op means the other path won, not an
		// error.
		if _, err := t.app.store.AdvanceThreadRequestState(token, store.ThreadRequestUnconfirmed, store.ThreadRequestAccepted); err != nil {
			return "", false, err
		}
	} else {
		// This thread now answers a paired computer's request, so it serves
		// the thread tools whether or not this computer's own switch is on.
		t.app.refreshThreadToolsAdmission(target)
	}
	return target, created, nil
}

// failRequest settles a source row on this computer whose acceptance or
// dispatch failed before the destination ever ran it, and returns the
// original error.
//
// The row is settled rather than left open because nothing else can settle
// it: no receipt is running, no turn will end, and no poller will ask. A row
// left `unconfirmed` would tell the agent to keep checking a token that never
// changes. A request a paired computer made has no row here at all; its
// source settles it from the refusal this returns.
//
// A receipt accepted before the failure is settled too, through the settle
// door, which drops the scratch thread an ask had already forked and tells a
// paired computer's poller what happened. Without it a forwarded request
// whose dispatch failed would leave a receipt open on this computer forever.
// The source row is settled FIRST so a local request keeps the word for what
// happened to it, `refused`, rather than the `errored` its own receipt
// collects.
func (t threadToolsApp) failRequest(token string, cause error) error {
	message := cause.Error()
	if _, public, ok := errorsx.PublicDetails(cause); ok {
		message = public
	}
	row, found, err := t.app.store.GetThreadRequest(token)
	if err != nil {
		log.Printf("thread tools: read request %s to refuse it: %v", token, err)
	}
	if found && row.TargetComputerID == "" {
		// A row naming another computer is a request THIS computer sent
		// there; a failure accepting a forwarded call is not its settlement.
		if _, err := t.app.store.SettleThreadRequest(token, store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
			State:      store.ThreadRequestRefused,
			Answer:     []byte(message),
			AnswerKind: store.ThreadAnswerError,
		}); err != nil {
			log.Printf("thread tools: settle refused request %s: %v", token, err)
		}
	}
	if err := t.app.settleThreadReceipt(token, store.ThreadReceiptOpenStates(), store.ThreadRequestSettlement{
		State:      store.ThreadReceiptErrored,
		Answer:     []byte(message),
		AnswerKind: store.ThreadAnswerError,
	}); err != nil {
		log.Printf("thread tools: settle refused receipt %s: %v", token, err)
	}
	return cause
}

// dispatchRequest sends one request's message into its target thread, as the
// user's own message with the request's footer appended.
//
// The send is composer-shaped on purpose: a busy thread queues the message at
// its turn boundary instead of interrupting it, and an idle one starts a turn
// now. The receipt moves to `running` when the message reaches the provider:
// from the returned row on the idle path, and from the queue's durable
// settlement on the queued one, so a message merged into another queued send
// still reports the turn it joined.
func (t threadToolsApp) dispatchRequest(
	ctx context.Context, origin threadRequestOrigin, token, targetThreadID, text string, wantsAnswer bool,
) error {
	caller := origin.caller
	body, err := t.requestMessageBody(origin, token, text, wantsAnswer)
	if err != nil {
		return err
	}
	attribution := &usermessage.OriginThread{
		ComputerID:   caller.ComputerID,
		ComputerName: caller.ComputerName,
		ThreadID:     caller.ThreadID,
		Title:        caller.Title,
		Token:        token,
	}
	sendID := threadRequestSendID(token)
	item, err := t.app.sendMessageWithOptions(ctx, targetThreadID, body, sendMessageOptions{
		SendID:            sendID,
		ReconcileBySendID: true,
		QueueIfActive:     true,
		// The target's composer holds whatever the person at that thread
		// typed and has not sent. An agent's message must never take it.
		PreserveDraft: true,
		Origin:        usermessage.OriginAgentThread,
		OriginThread:  attribution,
		onDurable: func() {
			record, found, err := t.app.findRecordedSend(targetThreadID, sendID)
			if err != nil {
				log.Printf("thread tools: resolve dispatched request %s: %v", token, err)
				return
			}
			if !found || !record.dispatched {
				// The message was restored into the composer instead of
				// being sent (a session death, a restart). Nothing ran, so
				// the receipt stays `accepted` and the boot sweep settles it
				// interrupted.
				return
			}
			t.markRequestRunning(token, targetThreadID, record.item)
		},
	})
	if err != nil {
		return err
	}
	if item.ID != "" {
		t.markRequestRunning(token, targetThreadID, item)
	}
	return nil
}

// markRequestRunning binds the receipt to the turn that consumed its message.
// Until it applies nothing can settle the request, which is exactly right:
// the message is still on a queue.
//
// It runs under the token's settle lock, so the observer's gate and the row
// transition are one step as far as a turn end is concerned: without that the
// observer could read the receipt as `accepted`, decide there is nothing to
// watch, and drop the gate the write is about to need. A turn that finished
// before the binding landed is settled here, because by then no turn end is
// coming.
func (t threadToolsApp) markRequestRunning(token, targetThreadID string, item store.Item) {
	var work threadSettlementWork
	func() {
		unlock := t.app.threadRequestSettleLock(token)
		defer unlock()
		// The observer's gate is set BEFORE the row, so a turn that ends
		// between the two still reaches this token. The gate only says
		// "look"; the store row says whether there is anything to settle.
		t.app.noteReceiptRunning(targetThreadID, token)
		bound, err := t.app.store.MarkThreadReceiptRunning(token, item.ID, threadRequestTurnKey(targetThreadID, item.TurnIndex))
		if err != nil {
			log.Printf("thread tools: mark receipt %s running: %v", token, err)
			return
		}
		if _, err := t.app.store.AdvanceThreadRequestState(token, store.ThreadRequestAccepted, store.ThreadRequestRunning); err != nil {
			log.Printf("thread tools: advance request %s to running: %v", token, err)
		}
		if !bound {
			// The receipt was cancelled or settled while the message was on
			// its way to the provider. Whoever settled it owns it.
			return
		}
		work = t.settleIfTurnAlreadyEnded(token, targetThreadID, item.TurnIndex)
	}()
	// This runs inside the target thread's own send or queue dispatch, which
	// holds that thread's locks. The deferred work takes thread locks of its
	// own, so it never runs on this goroutine.
	t.app.runThreadSettlementWorkDetached(work)
}

// settleIfTurnAlreadyEnded settles a receipt whose turn completed before the
// receipt was observable as running. The observer's gate was unset when that
// turn ended, so nothing else will ever look at it again.
//
// The caller holds the token's settle lock.
func (t threadToolsApp) settleIfTurnAlreadyEnded(token, targetThreadID string, turnIndex int) threadSettlementWork {
	turn, found, err := t.app.store.GetTurnByThreadIndex(targetThreadID, turnIndex)
	if err != nil {
		log.Printf("thread tools: read turn %d of %s for request %s: %v", turnIndex, targetThreadID, token, err)
		return threadSettlementWork{}
	}
	if !found || turn.CompletedAt == nil {
		return threadSettlementWork{}
	}
	settlement, err := t.app.turnSettlement(targetThreadID, turn)
	if err != nil {
		log.Printf("thread tools: read the answer of request %s: %v", token, err)
		return threadSettlementWork{}
	}
	work, _, err := t.app.settleThreadReceiptLocked(token, []string{store.ThreadReceiptRunning}, settlement, dropScratchThread)
	if err != nil {
		log.Printf("thread tools: settle request %s on a finished turn: %v", token, err)
		return threadSettlementWork{}
	}
	return work
}

// requestMessageBody is the message the target thread reads: the sender's
// text, then the fixed footer. The footer is never hand-written here; both
// ends read the same template, and a variant wording is a variant contract.
func (t threadToolsApp) requestMessageBody(
	origin threadRequestOrigin, token, text string, wantsAnswer bool,
) (string, error) {
	caller := origin.caller
	latest := origin.userMessage
	if !origin.foreign {
		var err error
		latest, _, err = t.app.store.LatestHumanUserText(caller.ThreadID)
		if err != nil {
			return "", err
		}
	}
	footer := threadtools.Footer{
		Title:    caller.Title,
		ThreadID: caller.ThreadID,
		// Named only for a sender on another computer: a local sender is on
		// the receiver's own computer and naming it would read as a second
		// machine. Either way the responder can reach it, because the
		// pairing the request arrived over is the route back.
		Computer:        t.senderComputerName(origin),
		Token:           token,
		AnswerRequested: wantsAnswer,
		SenderReachable: t.senderReachable(origin),
		UserMessage:     latest,
	}
	return strings.TrimRight(text, "\n") + "\n\n" + footer.String(), nil
}

// senderComputerName is what the footer calls the sender's computer: what
// THIS computer calls the pairing, not what the sender calls itself, so the
// responder reads a name its own thread_options lists.
func (t threadToolsApp) senderComputerName(origin threadRequestOrigin) string {
	if !origin.foreign {
		return ""
	}
	name, _ := t.threadToolsBackendName(origin.caller.ComputerID)
	if name != "" {
		return name
	}
	return origin.caller.ComputerName
}

// senderReachable reports whether the responder can reach the sender's
// computer, which is what the footer's return-navigation lines depend on.
//
// Pairing is directional: a request arrives over the SENDER's credential
// for this computer, which says nothing about this computer holding one for
// it. A sender on this computer is always reachable; a foreign one only
// when this computer's own pairing set names it.
func (t threadToolsApp) senderReachable(origin threadRequestOrigin) bool {
	if !origin.foreign {
		return true
	}
	_, paired := t.threadToolsPairing(origin.caller.ComputerID)
	return paired
}

// forkScratchThread makes the hidden thread an ask is answered in and records
// what it is for. Read-only WHATEVER the source runs: an ask is a question,
// and a fork that could write would act on a workspace whose owner never
// agreed to this conversation.
func (t threadToolsApp) forkScratchThread(ctx context.Context, source store.Thread, token string) (store.Thread, error) {
	fork, err := t.app.forkThreadTail(ctx, source.ID, forkOptions{
		Mode:        threadmode.ModeScratch,
		RuntimeMode: string(provider.RuntimeReadOnly),
		Title:       threadToolsAskTitle(source.Title),
	})
	if err != nil {
		return store.Thread{}, err
	}
	if err := t.app.store.InsertScratchThread(store.ScratchThread{
		ThreadID:       fork.ID,
		SourceThreadID: source.ID,
		ReturnMode:     scratchReturnMode(source.Mode),
		RequestToken:   token,
	}); err != nil {
		// The fork exists and nothing owns it yet. Take it back rather than
		// leave a scratch thread no sweep knows about.
		if deleteErr := t.app.DeleteThread(fork.ID); deleteErr != nil {
			log.Printf("thread tools: delete orphaned scratch fork %s: %v", fork.ID, deleteErr)
		}
		return store.Thread{}, err
	}
	return fork, nil
}

func threadToolsAskTitle(sourceTitle string) string {
	title := strings.TrimSpace(sourceTitle)
	if title == "" {
		title = "thread"
	}
	return "Ask: " + title
}

// scratchReturnMode is the mode a Keep promotion returns a scratch fork to,
// for both of its creators: an agent's ask and `/side-chat`. The table's
// CHECK refuses `scratch` itself, and a hidden workflow mode is not
// something a person can keep, so both fall back to chat, which is what a
// promoted side conversation actually is.
func scratchReturnMode(sourceMode string) string {
	switch sourceMode {
	case threadmode.ModeChat, threadmode.ModePlan:
		return sourceMode
	default:
		return threadmode.ModeChat
	}
}

// createSpawnedThread makes the thread a spawn runs in: a fork of another
// thread when from_thread names one, a fresh thread otherwise. A group the
// call names is joined once the thread exists, through the organize patch
// the sidebar's own grouping uses, so the group is created in the NEW
// thread's project: a fork's source project, or the destination's.
func (t threadToolsApp) createSpawnedThread(
	ctx context.Context, call threadtools.SpawnCall, create CreateThreadOptions,
) (store.Thread, error) {
	thread, err := t.createSpawnedThreadUngrouped(ctx, call, create)
	if err != nil || call.Group == "" {
		return thread, err
	}
	group := call.Group
	applied, err := t.app.applyThreadOrganizePatch(ctx, thread.ID, threadapp.OrganizePatch{Group: &group})
	if err != nil {
		return store.Thread{}, errorsx.Public(threadtools.CodeInvalidRequest,
			fmt.Sprintf("The thread was created as %s but could not join group %q: %v", thread.ID, group, err), err)
	}
	return applied.Thread, nil
}

func (t threadToolsApp) createSpawnedThreadUngrouped(
	ctx context.Context, call threadtools.SpawnCall, create CreateThreadOptions,
) (store.Thread, error) {
	if call.FromThread == "" {
		return t.app.CreateThread(ctx, create)
	}
	// A fork runs in its source's project and workspace with its source's
	// provider: that is what forking is, and the tools layer already refused
	// a from_thread on another computer. Everything else on the new thread
	// was resolved in spawnThreadOptions from the caller's settings and the
	// call's overrides, so it reaches the fork here rather than being
	// dropped for the source's.
	source, err := t.localThread(call.FromThread)
	if err != nil {
		return store.Thread{}, err
	}
	return t.app.forkThreadTail(ctx, source.ID, forkOptions{
		Mode:        create.Mode,
		RuntimeMode: create.RuntimeMode,
		Title:       create.Title,
		Model:       create.Model,
		Effort:      create.ReasoningEffort,
	})
}

// spawnThreadOptions resolves what a spawn inherits and validates what it
// overrides, before anything durable exists.
//
// Every refusal names what this computer offers, because the caller cannot
// know another provider's model ids and a wrong guess should cost one call,
// not a discovery round trip.
func (t threadToolsApp) spawnThreadOptions(origin threadRequestOrigin, call threadtools.SpawnCall) (CreateThreadOptions, error) {
	if call.FromThread != "" {
		return t.forkSpawnOptions(origin, call)
	}
	// A spawn a paired computer forwarded has no caller thread here, so
	// there is no project or checkout to inherit; the call names them and
	// the tools layer refuses it beforehand when it does not.
	var caller store.Thread
	if !origin.foreign {
		var err error
		caller, err = t.localThread(origin.caller.ThreadID)
		if err != nil {
			return CreateThreadOptions{}, err
		}
	}
	opts := CreateThreadOptions{
		ProjectID:       caller.ProjectID,
		Title:           call.Title,
		Provider:        origin.inherit.Provider,
		Model:           origin.inherit.Model,
		Mode:            origin.inherit.Mode,
		ReasoningEffort: origin.inherit.Effort,
		RuntimeMode:     origin.inherit.RuntimeMode,
	}
	if call.ProjectID != "" {
		if _, err := t.app.store.GetProject(call.ProjectID); err != nil {
			return CreateThreadOptions{}, errorsx.Public(threadtools.CodeNotFound,
				fmt.Sprintf("There is no project %s on this computer. thread_options lists the projects it has.", call.ProjectID), err)
		}
		opts.ProjectID = call.ProjectID
	}
	if opts.ProjectID == "" {
		return CreateThreadOptions{}, errorsx.Public(threadtools.CodeInvalidRequest,
			"A spawn needs a project: this thread has none to inherit. thread_options lists this computer's projects.", nil)
	}
	if call.Mode != "" {
		opts.Mode = call.Mode
	}
	if call.RuntimeMode != "" {
		opts.RuntimeMode = call.RuntimeMode
	}
	if err := t.applySpawnModel(&opts, call); err != nil {
		return CreateThreadOptions{}, err
	}
	if err := t.applySpawnWorkspace(&opts, caller, call); err != nil {
		return CreateThreadOptions{}, err
	}
	return opts, nil
}

// forkSpawnOptions resolves what a `from_thread` spawn runs with.
//
// A fork keeps its source's project, workspace and provider session; every
// other setting defaults to the CALLER's and is overridden by the call
// (docs/specs/agent-thread-tools.md, thread_spawn). Provider is the one axis
// a fork cannot move: a thread is locked to its provider once it holds items
// because the sessions are not interchangeable, so an explicit provider is
// refused here rather than accepted and ignored.
//
// Model and effort follow the caller only when the caller runs the source's
// provider. Across providers the caller's model names nothing this fork could
// start, so the source's stands, and an explicit one is validated against the
// source's provider like any other spawn.
func (t threadToolsApp) forkSpawnOptions(origin threadRequestOrigin, call threadtools.SpawnCall) (CreateThreadOptions, error) {
	source, err := t.localThread(call.FromThread)
	if err != nil {
		return CreateThreadOptions{}, err
	}
	if call.Provider != "" && call.Provider != source.Provider {
		return CreateThreadOptions{}, errorsx.Public(threadtools.CodeInvalidRequest, fmt.Sprintf(
			"A fork of %s resumes that thread's %s session, so provider is the one setting from_thread cannot change. Drop provider, or spawn a fresh %s thread instead of forking.",
			source.ID, source.Provider, call.Provider), nil)
	}
	opts := CreateThreadOptions{
		Title:           call.Title,
		Provider:        source.Provider,
		Model:           source.Model,
		ReasoningEffort: source.ReasoningEffort,
		RuntimeMode:     origin.inherit.RuntimeMode,
	}
	if origin.inherit.Provider == source.Provider {
		opts.Model, opts.ReasoningEffort = origin.inherit.Model, origin.inherit.Effort
	}
	// A caller that is itself hidden (a scratch thread answering an ask) has
	// no mode a visible thread may take, so the source's stands.
	if threadmode.IsPostCreationMode(origin.inherit.Mode) {
		opts.Mode = origin.inherit.Mode
	}
	if call.Mode != "" {
		opts.Mode = call.Mode
	}
	if call.RuntimeMode != "" {
		opts.RuntimeMode = call.RuntimeMode
	}
	if err := t.applySpawnModel(&opts, call); err != nil {
		return CreateThreadOptions{}, err
	}
	return opts, nil
}

// applySpawnModel resolves provider, model and effort against THIS computer's
// catalogs. A provider override with no model takes that provider's default,
// because the two are one choice and the caller cannot be expected to know
// the other provider's slugs.
func (t threadToolsApp) applySpawnModel(opts *CreateThreadOptions, call threadtools.SpawnCall) error {
	if call.Provider != "" && call.Provider != opts.Provider {
		opts.Provider = call.Provider
		// The inherited model and effort belong to the provider that was
		// replaced, and naming them to the new one would be a refusal the
		// caller did not earn.
		opts.Model, opts.ReasoningEffort = "", ""
	}
	option, err := t.providerOption(opts.Provider)
	if err != nil || len(option.Models) == 0 {
		return errorsx.Public(threadtools.CodeInvalidRequest,
			fmt.Sprintf("This computer offers no models for %q. thread_options lists what it has.", opts.Provider), err)
	}
	if call.Model != "" {
		opts.Model = call.Model
	}
	if opts.Model == "" {
		opts.Model = option.DefaultModel
	}
	var model threadtools.ModelOption
	for _, candidate := range option.Models {
		if candidate.Slug == opts.Model {
			model = candidate
			break
		}
	}
	if model.Slug == "" {
		return errorsx.Public(threadtools.CodeInvalidRequest,
			fmt.Sprintf("%s does not offer model %q on this computer. It offers: %s.",
				option.Name, opts.Model, strings.Join(threadToolsModelSlugs(option.Models), ", ")), nil)
	}
	if call.Effort != "" {
		opts.ReasoningEffort = call.Effort
	}
	if len(model.Efforts) == 0 {
		// The model has no effort dimension; an inherited one would be
		// coerced away anyway, and an explicit one is worth saying so about.
		if call.Effort != "" {
			return errorsx.Public(threadtools.CodeInvalidRequest,
				fmt.Sprintf("Model %s has no reasoning effort setting on this computer.", model.Slug), nil)
		}
		opts.ReasoningEffort = ""
		return nil
	}
	if opts.ReasoningEffort == "" {
		opts.ReasoningEffort = model.DefaultEffort
	}
	for _, effort := range model.Efforts {
		if effort == opts.ReasoningEffort {
			return nil
		}
	}
	return errorsx.Public(threadtools.CodeInvalidRequest,
		fmt.Sprintf("Model %s does not offer effort %q. It offers: %s.",
			model.Slug, opts.ReasoningEffort, strings.Join(model.Efforts, ", ")), nil)
}

func threadToolsModelSlugs(models []threadtools.ModelOption) []string {
	slugs := make([]string, 0, len(models))
	for _, model := range models {
		slugs = append(slugs, model.Slug)
	}
	return slugs
}

// applySpawnWorkspace picks the checkout the new thread runs in: a fresh
// worktree on a named branch, a named checkout of the project, or the
// caller's own.
func (t threadToolsApp) applySpawnWorkspace(opts *CreateThreadOptions, caller store.Thread, call threadtools.SpawnCall) error {
	if call.WorktreeBranch != "" {
		// The sidebar's own door: thread creation cuts the worktree and
		// records its path and branch. Nothing here may name a path as well,
		// or the two would describe different checkouts.
		opts.WorktreeBranch = call.WorktreeBranch
		return nil
	}
	if call.WorkspacePath != "" {
		projects, err := t.projectOptions(opts.ProjectID)
		if err != nil {
			return err
		}
		paths := []string{}
		for _, project := range projects {
			if project.ID != opts.ProjectID {
				continue
			}
			for _, workspace := range project.Workspaces {
				paths = append(paths, workspace.Path)
				if workspace.Path != call.WorkspacePath {
					continue
				}
				opts.WorkspaceOverride = workspace.Path
				if workspace.Worktree {
					opts.WorktreePath = workspace.Path
					opts.Branch = workspace.Branch
				}
				return nil
			}
		}
		return errorsx.Public(threadtools.CodeInvalidRequest,
			fmt.Sprintf("That project has no checkout at %s. It has: %s. Pass worktree to cut a fresh one instead.",
				call.WorkspacePath, strings.Join(paths, ", ")), nil)
	}
	// Inherit the caller's checkout, worktree included: a spawn that names no
	// workspace belongs where its caller is working.
	if caller.ProjectID != opts.ProjectID {
		return nil
	}
	if caller.WorktreePath != "" {
		opts.WorktreePath = caller.WorktreePath
		opts.WorkspaceOverride = caller.WorktreePath
		opts.Branch = caller.Branch
		return nil
	}
	if caller.WorkspacePath != "" {
		opts.WorkspaceOverride = caller.WorkspacePath
	}
	return nil
}
