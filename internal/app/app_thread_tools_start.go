package app

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"agent-overflow/internal/agenthome"
	"agent-overflow/internal/entityid"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
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
			"home_files is only for an assistant's current thread, and this thread is not one. Put what the receiver needs in the message itself.", nil)
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
	level, err := t.refuseSpawnPastLimit(caller.ThreadID)
	if err != nil {
		return threadtools.RequestAck{}, err
	}
	if t.remoteDestination(call.ComputerID) {
		call.Level = level
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
	done, err := t.app.admitWork(caller.ThreadID, caller.Session)
	if err != nil {
		return threadtools.RequestAck{}, err
	}
	token := newThreadRequestToken()
	_, err = t.acceptSpawn(ctx, origin, token, call)
	done()
	if err != nil {
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
//
// A refusal before acceptRequest settles nothing, here and in acceptSend
// and acceptAsk: this attempt has written no row, and a receipt already
// under the token is another attempt's, which may be making its thread.
func (t threadToolsApp) acceptSpawn(ctx context.Context, origin threadRequestOrigin, token string, call threadtools.SpawnCall) (string, error) {
	placement, err := threadtools.ResolveSpawnPlacement(call.Placement, call.Group)
	if err != nil {
		return "", threadRequestRefusal(err, "")
	}
	call.Placement = placement
	call.Level = carriedLevel(origin, call.Level)
	if err := t.refuseCarriedLevelPastLimit(call.Level); err != nil {
		return "", threadRequestRefusal(err, "")
	}
	origin.tracksSpawn = origin.foreign && call.Level > 0
	if placement != threadtools.PlacementOwned {
		call.Level = 0
	}
	create, err := t.spawnThreadOptions(ctx, origin, call)
	if err != nil {
		return "", threadRequestRefusal(err, "")
	}
	// A paired computer's owned spawn is owned here through its receipt, the
	// link, when it carries its level: a computer that sends no level
	// predates ownership across computers, and the placement alone cannot
	// tell, since an omitted one resolves to owned. Any other is standalone,
	// or grouped by its group.
	link := origin.foreign && call.Placement == threadtools.PlacementOwned && call.Level >= 1
	create.owned = call.Placement == threadtools.PlacementOwned && (!origin.foreign || link)
	create.lineage = origin.lineage()
	ctx = acceptedRequestContext(ctx)
	target, fresh, err := t.acceptRequest(origin, store.ThreadRequest{
		Token:          token,
		Kind:           store.ThreadRequestSpawn,
		OriginThreadID: call.FromThread,
		Notify:         call.Notify,
	}, func() (store.Thread, madeWithThread, error) {
		return t.createSpawnedThread(ctx, call, create)
	}, create.owned)
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
		// The thread exists and stays: it is the caller's to continue.
		return "", t.settleRefusal(token, threadRequestRefusal(err, target))
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
	target, err := t.sendTarget(call.ThreadID)
	if err != nil {
		return threadtools.RequestAck{}, err
	}
	// Rechecked here and not only in the tools layer: resolution and this
	// call are separate moments, and a thread that became the caller's own
	// in between would queue a message only its own turn could answer. A
	// send to an assistant's past thread goes to its current one, which may
	// be the caller.
	call.ThreadID = target.ID
	if err := t.refuseSelf(caller, call.ThreadID, "send to"); err != nil {
		return threadtools.RequestAck{}, err
	}
	done, err := t.app.admitWork(caller.ThreadID, caller.Session)
	if err != nil {
		return threadtools.RequestAck{}, err
	}
	defer done()
	// The target is a thread here that already exists, so the request and
	// its acceptance are one write: the source row, already accepted, and
	// the receipt. A paired computer's send is accepted by acceptSend.
	token := newThreadRequestToken()
	// A send to a worker at rest starts a run of its own: a run of the
	// caller's the rest pass has not ended yet ends first, so the send does
	// not join it.
	if open, err := t.app.store.CallerRunOpen(caller.ThreadID, "", target.ID); err != nil {
		return threadtools.RequestAck{}, err
	} else if open {
		if err := t.app.endRunsAtRest(target.ID); err != nil {
			return threadtools.RequestAck{}, err
		}
	}
	if _, err := t.app.store.AcceptLocalThreadRequest(store.ThreadRequest{
		Token:          token,
		CallerThreadID: caller.ThreadID,
		Kind:           store.ThreadRequestSend,
		TargetThreadID: target.ID,
		Notify:         call.Notify,
		State:          store.ThreadRequestAccepted,
	}, threadReceiptFor(origin, token, store.ThreadRequestSend, target.ID, false)); err != nil {
		return threadtools.RequestAck{}, err
	}
	if err := t.dispatchRequest(ctx, origin, token, store.ThreadRequestSend, target.ID, call.Message, answerRequested(call.WaitSeconds, call.Notify)); err != nil {
		return threadtools.RequestAck{}, t.failRequest(token, err)
	}
	done()
	return t.ackRequest(ctx, caller, token, call.WaitSeconds)
}

// acceptSend is the destination half of a send a paired computer forwarded:
// accept the receipt against the named thread and queue the message there.
func (t threadToolsApp) acceptSend(ctx context.Context, origin threadRequestOrigin, token string, call threadtools.SendCall) (string, error) {
	target, err := t.sendTarget(call.ThreadID)
	if err != nil {
		return "", threadRequestRefusal(err, "")
	}
	if origin.foreign && target.ID == origin.caller.ThreadID {
		// A thread id that names the caller can only mean two threads with
		// the same id on two computers, which the ids rule out; refuse it
		// rather than queue a message a thread would answer for itself.
		return "", errorsx.Public(threadtools.CodeSelfSend, "A thread cannot send to itself.", nil)
	}
	ctx = acceptedRequestContext(ctx)
	_, fresh, err := t.acceptRequest(origin, store.ThreadRequest{
		Token:          token,
		Kind:           store.ThreadRequestSend,
		TargetThreadID: target.ID,
	}, nil, false)
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
// run work in it, before anything durable is written for the request. A
// thread New Thread, Same Assistant superseded is addressed through its
// assistant's current thread.
func (t threadToolsApp) sendTarget(threadID string) (store.Thread, error) {
	if current, ok := t.app.assistantCurrentThread(threadID); ok {
		threadID = current
	}
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
		// The other computer has no ask here to count, so the copy's
		// level travels with the call, as an owned spawn's does.
		place, err := t.app.subThreadPlaceOf(caller.ThreadID)
		if err != nil {
			return threadtools.RequestAck{}, err
		}
		call.Level = place.level + 1
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
	if _, err := t.localThread(call.ThreadID); err != nil {
		return threadtools.RequestAck{}, err
	}
	done, err := t.app.admitWork(caller.ThreadID, caller.Session)
	if err != nil {
		return threadtools.RequestAck{}, err
	}
	token := newThreadRequestToken()
	_, err = t.acceptAsk(ctx, origin, token, call)
	done()
	if err != nil {
		return threadtools.RequestAck{}, err
	}
	return t.ackRequest(ctx, caller, token, call.WaitSeconds)
}

// acceptAsk is the destination half of an ask: fork the named thread into
// the hidden scratch thread the question is answered in.
func (t threadToolsApp) acceptAsk(ctx context.Context, origin threadRequestOrigin, token string, call threadtools.AskCall) (string, error) {
	source, err := t.localThread(call.ThreadID)
	if err != nil {
		return "", threadRequestRefusal(err, "")
	}
	ctx = acceptedRequestContext(ctx)
	target, fresh, err := t.acceptRequest(origin, store.ThreadRequest{
		Token:          token,
		Kind:           store.ThreadRequestAsk,
		OriginThreadID: source.ID,
		Notify:         call.Notify,
	}, func() (store.Thread, madeWithThread, error) {
		thread, err := t.app.forkScratchThread(ctx, source, scratchForkOptions{
			TitlePrefix: askScratchTitlePrefix,
			// Read-only WHATEVER the source runs: an ask is a question, and
			// a fork that could write would act on a workspace whose owner
			// never agreed to this conversation.
			RuntimeMode:  string(provider.RuntimeReadOnly),
			RequestToken: token,
			Lineage:      origin.lineage(),
			// A local ask's copy counts from the asker by the ask itself
			// (store.SubThreadLevel); a paired computer's carries its level.
			SpawnLevel: carriedLevel(origin, call.Level),
		})
		return thread, madeWithThread{}, err
	}, false)
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

// acceptedRequestContext is the context a request runs on from its receipt
// on: the caller's values without its cancellation. A retry with the same
// token never creates or dispatches again, so an accepted request abandoned
// mid-dispatch would leave its thread without the message for good.
func acceptedRequestContext(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
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
