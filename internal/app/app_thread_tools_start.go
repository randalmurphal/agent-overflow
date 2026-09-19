package app

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
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

// Spawn creates a visible thread, sends the prompt as its first message and
// returns the receipt.
func (t threadToolsApp) Spawn(ctx context.Context, caller threadtools.Caller, call threadtools.SpawnCall) (threadtools.RequestAck, error) {
	if err := t.requireLocalDestination(call.ComputerID); err != nil {
		return threadtools.RequestAck{}, err
	}
	callerThread, err := t.localThread(caller.ThreadID)
	if err != nil {
		return threadtools.RequestAck{}, err
	}
	// Everything this computer can refuse is refused BEFORE the row exists,
	// so an unknown provider or project costs the model one call and leaves
	// no request behind to explain.
	create, err := t.spawnThreadOptions(callerThread, call)
	if err != nil {
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

	target, err := t.acceptRequest(ctx, caller, token, store.ThreadRequestSpawn, "", func() (store.Thread, error) {
		return t.createSpawnedThread(ctx, call, create)
	})
	if err != nil {
		return threadtools.RequestAck{}, t.refuseRequest(token, err)
	}
	if err := t.dispatchRequest(ctx, caller, token, target, call.Prompt, answerRequested(call.WaitSeconds, call.Notify)); err != nil {
		return threadtools.RequestAck{}, t.refuseRequest(token, err)
	}
	return t.ackRequest(ctx, caller, token, call.WaitSeconds)
}

// Send continues an existing thread as if the user had typed the message.
func (t threadToolsApp) Send(ctx context.Context, caller threadtools.Caller, call threadtools.SendCall) (threadtools.RequestAck, error) {
	if err := t.requireLocalDestination(call.ComputerID); err != nil {
		return threadtools.RequestAck{}, err
	}
	// Rechecked here and not only in the tools layer: resolution and this
	// call are separate moments, and a thread that became the caller's own
	// in between would queue a message only its own turn could answer.
	if err := t.refuseSelf(caller, call.ThreadID, "send to"); err != nil {
		return threadtools.RequestAck{}, err
	}
	target, err := t.localThread(call.ThreadID)
	if err != nil {
		return threadtools.RequestAck{}, err
	}
	if err := t.app.store.CheckThreadExecutionAccess(target); err != nil {
		return threadtools.RequestAck{}, errorsx.Public(threadtools.CodeNotFound,
			fmt.Sprintf("Thread %s cannot run work on this computer: %v", target.ID, err), err)
	}
	if _, err := t.localThread(caller.ThreadID); err != nil {
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
	if _, err := t.acceptRequest(ctx, caller, token, store.ThreadRequestSend, target.ID, nil); err != nil {
		return threadtools.RequestAck{}, t.refuseRequest(token, err)
	}
	if err := t.dispatchRequest(ctx, caller, token, target.ID, call.Message, answerRequested(call.WaitSeconds, call.Notify)); err != nil {
		return threadtools.RequestAck{}, t.refuseRequest(token, err)
	}
	return t.ackRequest(ctx, caller, token, call.WaitSeconds)
}

// Ask forks the target at its tail into a hidden read-only scratch thread
// and puts the question there.
//
// The fork is what makes an ask safe to send into a thread that is busy: the
// scratch thread has the target's whole history and none of its future, so
// the answer is about what that thread knows without the question ever
// landing in its transcript.
func (t threadToolsApp) Ask(ctx context.Context, caller threadtools.Caller, call threadtools.AskCall) (threadtools.RequestAck, error) {
	if err := t.requireLocalDestination(call.ComputerID); err != nil {
		return threadtools.RequestAck{}, err
	}
	if err := t.refuseSelf(caller, call.ThreadID, "ask"); err != nil {
		return threadtools.RequestAck{}, err
	}
	source, err := t.localThread(call.ThreadID)
	if err != nil {
		return threadtools.RequestAck{}, err
	}
	if _, err := t.localThread(caller.ThreadID); err != nil {
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
	target, err := t.acceptRequest(ctx, caller, token, store.ThreadRequestAsk, "", func() (store.Thread, error) {
		return t.forkScratchThread(ctx, source, token)
	})
	if err != nil {
		return threadtools.RequestAck{}, t.refuseRequest(token, err)
	}
	// An ask always wants an answer: that is what distinguishes it from a
	// send, and the footer must say so however the wait was configured.
	if err := t.dispatchRequest(ctx, caller, token, target, call.Question, true); err != nil {
		return threadtools.RequestAck{}, t.refuseRequest(token, err)
	}
	return t.ackRequest(ctx, caller, token, call.WaitSeconds)
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

// requireLocalDestination refuses a computer id in this build. Cross-computer
// reach is its own phase: PairedComputers reports none, so the tools layer
// never produces an id, and one that arrives anyway is answered honestly
// rather than run here as if it had named this computer.
func (t threadToolsApp) requireLocalDestination(computerID string) error {
	if strings.TrimSpace(computerID) == "" {
		return nil
	}
	return errorsx.Public(threadtools.CodeUnreachable,
		fmt.Sprintf("Computer %s is not reachable in this build. Thread tools reach this computer's own threads.", computerID), nil)
}

// acceptRequest writes the destination row, creates the thread that answers
// when the kind has one to create, and advances the source row.
//
// The receipt is the record of record: a retry with the same token returns
// the existing acceptance and `create` never runs again, which is what makes
// a lost reply safe to retry with no second spawn, fork or message.
func (t threadToolsApp) acceptRequest(
	_ context.Context, caller threadtools.Caller, token, kind, targetThreadID string,
	create func() (store.Thread, error),
) (string, error) {
	receipt, created, err := t.app.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
		Token:              token,
		OwnerDeviceID:      threadReceiptLocalOwner,
		SourceComputerID:   caller.ComputerID,
		SourceComputerName: caller.ComputerName,
		SourceThreadID:     caller.ThreadID,
		SourceThreadTitle:  caller.Title,
		Kind:               kind,
		TargetThreadID:     targetThreadID,
	})
	if err != nil {
		return "", err
	}
	target := receipt.TargetThreadID
	if target == "" && create != nil {
		if !created {
			// The receipt exists with no thread: the attempt that accepted
			// it died between the two writes. The boot sweep settles it, and
			// a second thread now would be a second piece of work nobody
			// asked for.
			return "", errorsx.Public(threadtools.CodeInvalidRequest,
				"That request was accepted but its thread was never created. It is settled as interrupted; make the request again.", nil)
		}
		thread, err := create()
		if err != nil {
			return "", err
		}
		target = thread.ID
		if _, err := t.app.store.SetThreadReceiptTarget(token, target); err != nil {
			return "", err
		}
	}
	if target == "" {
		return "", fmt.Errorf("thread tools: request %s has no target thread", token)
	}
	if _, err := t.app.store.SetThreadRequestTarget(token, "", target); err != nil {
		return "", err
	}
	// Conditional: a request that already settled during dispatch keeps its
	// settled state, and a no-op means the other path won, not an error.
	if _, err := t.app.store.AdvanceThreadRequestState(token, store.ThreadRequestUnconfirmed, store.ThreadRequestAccepted); err != nil {
		return "", err
	}
	return target, nil
}

// refuseRequest settles a source row whose dispatch failed before the
// destination ever ran it, and returns the original error.
//
// The row is settled rather than left open because nothing else can settle
// it: no receipt is running, no turn will end, and no poller will ask. A row
// left `unconfirmed` would tell the agent to keep checking a token that never
// changes.
func (t threadToolsApp) refuseRequest(token string, cause error) error {
	message := cause.Error()
	if _, public, ok := errorsx.PublicDetails(cause); ok {
		message = public
	}
	if _, err := t.app.store.SettleThreadRequest(token, store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
		State:      store.ThreadRequestRefused,
		Answer:     []byte(message),
		AnswerKind: store.ThreadAnswerError,
	}); err != nil {
		log.Printf("thread tools: settle refused request %s: %v", token, err)
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
	ctx context.Context, caller threadtools.Caller, token, targetThreadID, text string, wantsAnswer bool,
) error {
	body, err := t.requestMessageBody(caller, token, text, wantsAnswer)
	if err != nil {
		return err
	}
	origin := &usermessage.OriginThread{
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
		OriginThread:  origin,
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
func (t threadToolsApp) markRequestRunning(token, targetThreadID string, item store.Item) {
	// The observer's gate is set BEFORE the row, so a turn that ends between
	// the two still reaches this token. The gate only says "look"; the store
	// row says whether there is anything to settle.
	t.app.noteReceiptRunning(targetThreadID, token)
	if _, err := t.app.store.MarkThreadReceiptRunning(token, item.ID, threadRequestTurnKey(targetThreadID, item.TurnIndex)); err != nil {
		log.Printf("thread tools: mark receipt %s running: %v", token, err)
		return
	}
	if _, err := t.app.store.AdvanceThreadRequestState(token, store.ThreadRequestAccepted, store.ThreadRequestRunning); err != nil {
		log.Printf("thread tools: advance request %s to running: %v", token, err)
	}
}

// requestMessageBody is the message the target thread reads: the sender's
// text, then the fixed footer. The footer is never hand-written here; both
// ends read the same template, and a variant wording is a variant contract.
func (t threadToolsApp) requestMessageBody(
	caller threadtools.Caller, token, text string, wantsAnswer bool,
) (string, error) {
	latest, _, err := t.app.store.LatestHumanUserText(caller.ThreadID)
	if err != nil {
		return "", err
	}
	footer := threadtools.Footer{
		Title:    caller.Title,
		ThreadID: caller.ThreadID,
		// Empty: the sender is on the receiver's own computer, which is the
		// only shape this build has, and so is always reachable from it.
		Computer:        "",
		Token:           token,
		AnswerRequested: wantsAnswer,
		SenderReachable: true,
		UserMessage:     latest,
	}
	return strings.TrimRight(text, "\n") + "\n\n" + footer.String(), nil
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
// thread when from_thread names one, a fresh thread otherwise.
func (t threadToolsApp) createSpawnedThread(
	ctx context.Context, call threadtools.SpawnCall, create CreateThreadOptions,
) (store.Thread, error) {
	if call.FromThread == "" {
		return t.app.CreateThread(ctx, create)
	}
	// A fork runs in its source's project and workspace with its source's
	// provider: that is what forking is, and the tools layer already refused
	// a from_thread on another computer.
	source, err := t.localThread(call.FromThread)
	if err != nil {
		return store.Thread{}, err
	}
	return t.app.forkThreadTail(ctx, source.ID, forkOptions{
		Mode:        call.Mode,
		RuntimeMode: call.RuntimeMode,
		Title:       call.Title,
	})
}

// spawnThreadOptions resolves what a spawn inherits and validates what it
// overrides, before anything durable exists.
//
// Every refusal names what this computer offers, because the caller cannot
// know another provider's model ids and a wrong guess should cost one call,
// not a discovery round trip.
func (t threadToolsApp) spawnThreadOptions(caller store.Thread, call threadtools.SpawnCall) (CreateThreadOptions, error) {
	if call.FromThread != "" {
		// A fork carries its source's settings; nothing here applies.
		return CreateThreadOptions{}, nil
	}
	opts := CreateThreadOptions{
		ProjectID:       caller.ProjectID,
		Title:           call.Title,
		Provider:        caller.Provider,
		Model:           caller.Model,
		Mode:            caller.Mode,
		ReasoningEffort: caller.ReasoningEffort,
		RuntimeMode:     caller.RuntimeMode,
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
