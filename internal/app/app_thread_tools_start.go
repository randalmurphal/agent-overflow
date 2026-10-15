package app

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"

	"agent-overflow/internal/agenthome"
	"agent-overflow/internal/entityid"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
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
	// agent is set when the caller is a standing agent: read from the
	// store for a local caller, carried for a foreign one.
	agent *threadtools.SenderAgent
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
	agent, err := t.senderAgent(caller.ThreadID)
	if err != nil {
		return threadRequestOrigin{}, err
	}
	return threadRequestOrigin{
		caller:      caller,
		agent:       agent,
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

// senderAgent names a thread of this computer as a standing agent, with the
// role line of its instructions, or returns nil for any other thread. A role
// that cannot be read is logged and left out: the message still names the
// sender as an agent.
func (t threadToolsApp) senderAgent(threadID string) (*threadtools.SenderAgent, error) {
	row, found, err := t.app.store.GetStandingAgent(threadID)
	if err != nil || !found {
		return nil, err
	}
	agent := &threadtools.SenderAgent{}
	if row.Home != "" {
		role, err := agenthome.RoleLine(row.Home)
		if err != nil {
			log.Printf("thread tools: read the role line of standing agent %s from %s: %v", threadID, row.Home, err)
		}
		agent.Role = role
	}
	return agent, nil
}

// answerAgent is senderAgent for the thread that answers request token,
// read when the answer is written so the wake names the agent as it was then,
// from either computer. A store read that fails is logged and the answer is
// settled as an ordinary thread's: the answer matters more than its label,
// and a settlement is not retried for one.
func (t threadToolsApp) answerAgent(token, threadID string) *store.ThreadAnswerAgent {
	if threadID == "" {
		return nil
	}
	agent, err := t.senderAgent(threadID)
	if err != nil {
		log.Printf("thread tools: request %s: read whether %s is a standing agent; the answer records none: %v", token, threadID, err)
		return nil
	}
	if agent == nil {
		return nil
	}
	return &store.ThreadAnswerAgent{Role: agent.Role}
}

// attachHomeFiles prepends the files home_files names, read from the calling
// standing agent's home, to the text a spawn or a send delivers. It runs on
// the caller's computer before anything is written or forwarded, so every
// refusal costs one call and a forwarded call carries the text alone.
func (t threadToolsApp) attachHomeFiles(caller threadtools.Caller, files []string, text string) (string, error) {
	if len(files) == 0 {
		return text, nil
	}
	row, found, err := t.app.store.GetStandingAgent(caller.ThreadID)
	if err != nil {
		return "", err
	}
	if !found || row.Home == "" {
		return "", errorsx.Public(threadtools.CodeInvalidRequest,
			"home_files is only for a standing agent, a thread the user promoted, and this thread is not one. Put what the receiver needs in the message itself.", nil)
	}
	attached, err := agenthome.ReadAttached(row.Home, files)
	if err != nil {
		return "", errorsx.Public(threadtools.CodeInvalidRequest, err.Error()+".", err)
	}
	joined := attached + "\n\n" + text
	if len(joined) > threadtools.MaxPromptBytes {
		return "", errorsx.Public(threadtools.CodeInvalidRequest, fmt.Sprintf(
			"The message with its home_files comes to %d bytes, past the %d a message can carry. Attach fewer files or shorten the message.",
			len(joined), threadtools.MaxPromptBytes), nil)
	}
	return joined, nil
}

// Spawn creates a visible thread, sends the prompt as its first message and
// returns the receipt.
func (t threadToolsApp) Spawn(ctx context.Context, caller threadtools.Caller, call threadtools.SpawnCall) (threadtools.RequestAck, error) {
	prompt, err := t.attachHomeFiles(caller, call.HomeFiles, call.Prompt)
	if err != nil {
		return threadtools.RequestAck{}, err
	}
	call.Prompt, call.HomeFiles = prompt, nil
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
	if _, err := t.spawnThreadOptions(ctx, origin, call); err != nil {
		return threadtools.RequestAck{}, err
	}
	if call.Group != "" {
		// A standing agent's spawned threads nest under its own sidebar row
		// (store.ErrSpawnedChildGrouped); the group would be refused once
		// the thread existed, so it is refused before.
		if _, standing, err := t.app.store.GetStandingAgent(caller.ThreadID); err != nil {
			return threadtools.RequestAck{}, err
		} else if standing {
			return threadtools.RequestAck{}, errorsx.Public(threadtools.CodeInvalidRequest,
				"This thread is a standing agent, and the threads it spawns nest under its own sidebar row rather than in a group. Omit group.", nil)
		}
	}
	return t.startLocalRequest(ctx, caller, store.ThreadRequest{
		Kind:           store.ThreadRequestSpawn,
		OriginThreadID: call.FromThread,
		Notify:         call.Notify,
	}, call.WaitSeconds, func(token string) (string, error) {
		return t.acceptSpawn(ctx, origin, token, call)
	})
}

// startLocalRequest is the source half of a spawn, send or ask this computer
// answers itself: write the row, accept it here, and return the receipt the
// caller reads.
//
// The row is written before the acceptance, as the remote path writes it: the
// token exists before any work can run under it, so a failure part way
// through names a request the caller can ask about rather than leaving a
// thread nothing records. Everything the call can refuse without writing has
// been refused by the time this runs.
func (t threadToolsApp) startLocalRequest(
	ctx context.Context, caller threadtools.Caller, row store.ThreadRequest,
	waitSeconds int, accept func(token string) (string, error),
) (threadtools.RequestAck, error) {
	row.Token = newThreadRequestToken()
	row.CallerThreadID = caller.ThreadID
	row.State = store.ThreadRequestUnconfirmed
	if err := t.app.store.InsertThreadRequest(row); err != nil {
		return threadtools.RequestAck{}, err
	}
	if _, err := accept(row.Token); err != nil {
		return threadtools.RequestAck{}, err
	}
	return t.ackRequest(ctx, caller, row.Token, waitSeconds)
}

// acceptSpawn is the destination half of a spawn: create the thread,
// accept the receipt and send the prompt there.
//
// It runs unchanged for a spawn one of this computer's own threads made
// and for one a paired computer forwarded. Only the origin differs, and
// the source row it advances exists only on the computer that made the
// request.
func (t threadToolsApp) acceptSpawn(ctx context.Context, origin threadRequestOrigin, token string, call threadtools.SpawnCall) (string, error) {
	create, err := t.spawnThreadOptions(ctx, origin, call)
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
	if err := t.dispatchRequest(ctx, origin, token, store.ThreadRequestSpawn, target, call.Prompt, answerRequested(call.WaitSeconds, call.Notify)); err != nil {
		return "", t.failRequest(token, err)
	}
	return target, nil
}

// Send continues an existing thread as if the user had typed the message.
func (t threadToolsApp) Send(ctx context.Context, caller threadtools.Caller, call threadtools.SendCall) (threadtools.RequestAck, error) {
	message, err := t.attachHomeFiles(caller, call.HomeFiles, call.Message)
	if err != nil {
		return threadtools.RequestAck{}, err
	}
	call.Message, call.HomeFiles = message, nil
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
	return t.startLocalRequest(ctx, caller, store.ThreadRequest{
		Kind:           store.ThreadRequestSend,
		TargetThreadID: target.ID,
		Notify:         call.Notify,
	}, call.WaitSeconds, func(token string) (string, error) {
		return t.acceptSend(ctx, origin, token, call)
	})
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
	if err := t.dispatchRequest(ctx, origin, token, store.ThreadRequestSend, target.ID, call.Message, answerRequested(call.WaitSeconds, call.Notify)); err != nil {
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
	return t.startLocalRequest(ctx, caller, store.ThreadRequest{
		Kind:           store.ThreadRequestAsk,
		OriginThreadID: source.ID,
		Notify:         call.Notify,
	}, call.WaitSeconds, func(token string) (string, error) {
		return t.acceptAsk(ctx, origin, token, call)
	})
}

// acceptAsk is the destination half of an ask: fork the named thread into
// the hidden scratch thread the question is answered in.
func (t threadToolsApp) acceptAsk(ctx context.Context, origin threadRequestOrigin, token string, call threadtools.AskCall) (string, error) {
	source, err := t.localThread(call.ThreadID)
	if err != nil {
		return "", t.failRequest(token, err)
	}
	target, fresh, err := t.acceptRequest(origin, token, store.ThreadRequestAsk, "", func() (store.Thread, error) {
		return t.app.forkScratchThread(ctx, source, scratchForkOptions{
			TitlePrefix: askScratchTitlePrefix,
			// Read-only WHATEVER the source runs: an ask is a question, and
			// a fork that could write would act on a workspace whose owner
			// never agreed to this conversation.
			RuntimeMode:  string(provider.RuntimeReadOnly),
			RequestToken: token,
		})
	})
	if err != nil {
		return "", t.failRequest(token, err)
	}
	if !fresh {
		return target, nil
	}
	// An ask always wants an answer: that is what distinguishes it from a
	// send, and the footer must say so however the wait was configured.
	if err := t.dispatchRequest(ctx, origin, token, store.ThreadRequestAsk, target, call.Question, true); err != nil {
		return "", t.failRequest(token, err)
	}
	return target, nil
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
			// it died between the two writes. Settle it here rather than
			// leave it for the next boot sweep, which is the only other
			// thing that would ever look at it; a second thread now would be
			// a second piece of work nobody asked for.
			if err := t.app.settleThreadReceipt(token, store.ThreadReceiptOpenStates(), store.ThreadRequestSettlement{
				State:      store.ThreadReceiptInterrupted,
				Answer:     []byte(threadReceiptNeverStarted),
				AnswerKind: store.ThreadAnswerError,
			}); err != nil {
				return "", false, err
			}
			return "", false, errorsx.Public(threadtools.CodeInvalidRequest,
				threadReceiptNeverStarted+" It is settled as interrupted; make the request again.", nil)
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
		// No title: a thread on this computer is named from its own row.
		if _, err := t.app.store.SetThreadRequestTarget(token, "", target, "", ""); err != nil {
			return "", false, err
		}
		if created && kind == store.ThreadRequestSpawn {
			// The row went out at creation without a spawner; now the
			// request names it, so the sidebar can nest the thread.
			t.app.broadcastThreadRowByID(target)
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

// threadReceiptNeverStarted is what a receipt accepted without its thread
// says on both ends: the answer stored on the settled receipt, and the
// refusal the retry that found it reads.
const threadReceiptNeverStarted = threadtools.InterruptNeverStarted

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
			// What the receipt records below, which another computer
			// would collect.
			Agent: t.answerAgent(token, row.TargetThreadID),
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
	ctx context.Context, origin threadRequestOrigin, token, kind, targetThreadID, text string, wantsAnswer bool,
) error {
	// The message drives the target thread in the mode that thread already
	// runs in, which is what SendMessageWithOptions judges for the composer.
	// A forwarded request arrives on a paired computer's session, so the
	// same gate has to stand here or thread_send would be the way around it.
	if err := t.app.requireAutonomyForThread(ctx, targetThreadID, ""); err != nil {
		return err
	}
	caller := origin.caller
	body, err := t.requestMessageBody(origin, token, targetThreadID, text, wantsAnswer)
	if err != nil {
		return err
	}
	attribution := &usermessage.OriginThread{
		ComputerID:   caller.ComputerID,
		ComputerName: caller.ComputerName,
		ThreadID:     caller.ThreadID,
		Title:        caller.Title,
		Token:        token,
		Kind:         kind,
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
		onDurable:     func() { t.requestDispatched(token, targetThreadID) },
	})
	if err != nil {
		return err
	}
	if item.ID != "" {
		t.markRequestRunning(token, targetThreadID, item)
	}
	return nil
}

// requestDispatched settles a queued request message once it is durable: it
// binds the receipt to the turn that consumed the message.
func (t threadToolsApp) requestDispatched(token, targetThreadID string) {
	record, found, err := t.app.findRecordedSend(targetThreadID, threadRequestSendID(token))
	if err != nil {
		log.Printf("thread tools: resolve dispatched request %s: %v", token, err)
		return
	}
	if !found || !record.dispatched {
		// The message did not reach the provider: a restart, or a session
		// end that gave it back to its queue (requeueRequest). Nothing ran,
		// so the receipt stays `accepted`.
		return
	}
	t.markRequestRunning(token, targetThreadID, record.item)
}

// returnRequest takes a request whose message a turn did not take in back to
// `accepted`: a person's Stop un-sent the turn, or the session ended before
// the agent read it. Nothing answered it, so its sender is told nothing; the
// request is owed again and requeueRequest puts its message back on the
// queue. It reports false when the request was settled or cancelled first,
// and then it owes nothing. It takes the token's settle lock, so a turn end
// reading the receipt sees it running or accepted, never in between.
func (t threadToolsApp) returnRequest(token string) bool {
	unlock := t.app.threadRequestSettleLock(token)
	defer unlock()
	returned, err := t.app.store.ReturnThreadReceipt(token)
	if err != nil {
		log.Printf("thread tools: return request %s: %v", token, err)
		return false
	}
	if !returned {
		return false
	}
	// A request another computer made has no row here; its caller reads
	// the receipt.
	if _, err := t.app.store.AdvanceThreadRequestState(token, store.ThreadRequestRunning, store.ThreadRequestAccepted); err != nil {
		log.Printf("thread tools: return request %s to accepted: %v", token, err)
	}
	return true
}

// requeueRequest puts a returned request's message (body, footer included)
// back on its target's queue, attributed as dispatchRequest attributed it. It
// is held: the Stop or the session end that gave it back does not start a
// turn for it, and the thread's next turn boundary or session start sends it.
func (t threadToolsApp) requeueRequest(token, targetThreadID, body string) error {
	receipt, found, err := t.app.store.GetThreadRequestReceipt(token)
	if err != nil {
		return fmt.Errorf("requeue request %s: %w", token, err)
	}
	if !found || receipt.State != store.ThreadReceiptAccepted {
		return nil
	}
	_, err = t.app.registerQueueItem(targetThreadID, body, SendMessageOptions{SendID: threadRequestSendID(token)}, injectedQueueOptions{
		preserveDraft: true,
		held:          true,
		origin:        usermessage.OriginAgentThread,
		originThread: &usermessage.OriginThread{
			ComputerID:   receipt.SourceComputerID,
			ComputerName: receipt.SourceComputerName,
			ThreadID:     receipt.SourceThreadID,
			Title:        receipt.SourceThreadTitle,
			Token:        token,
			Kind:         receipt.Kind,
		},
		onDurable: func() { t.requestDispatched(token, targetThreadID) },
	})
	if err != nil {
		return fmt.Errorf("requeue request %s: %w", token, err)
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
	settlement, err := t.app.turnSettlement(targetThreadID, turn, false)
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
	origin threadRequestOrigin, token, targetThreadID, text string, wantsAnswer bool,
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
		Agent:           origin.agent,
	}
	if origin.agent != nil && !origin.foreign {
		// A worker's questions and approvals reach the standing agent that
		// spawned it on this computer (StandingAgentSpawnerOf); a sender
		// on another computer never routes them.
		spawner, _, found, err := t.app.store.StandingAgentSpawnerOf(targetThreadID)
		if err != nil {
			return "", err
		}
		footer.RoutesPrompts = found && spawner == caller.ThreadID
	}
	// The body is the sender's text and the footer is this computer's; a
	// body line that opens like the footer is quoted so only the footer
	// reads as one.
	return threadtools.QuoteFooterMimics(strings.TrimRight(text, "\n")) + "\n\n" + footer.String(), nil
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
