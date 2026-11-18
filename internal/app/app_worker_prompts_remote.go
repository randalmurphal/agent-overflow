package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/usermessage"
)

// The prompts a worker on a paired computer routed to a thread here, the
// spawner that owns it there through its spawn's link. That computer holds
// them (app_worker_prompts_peer.go) and lists them on every agent_rows
// call this computer makes about the spawn (peerThreadAgentRows), which the
// poll makes at its own pace while the spawn is open or its thread has
// open work. This computer keeps them in memory only:
//
//   - A listed prompt not registered is registered and its wake queued to
//     its holder: the spawn's caller now (a handoff moves it), while that
//     thread can take a prompt (store.RemotePromptHolder). One that cannot
//     is given back at once (agent_prompt_return), unless the spawner's
//     archive or delete is on its way to the worker, which ends it there;
//     either way it is kept as returned until that computer stops listing
//     it.
//   - A registered prompt no longer listed is closed and its wake
//     withdrawn: the person answered it there, the worker stopped waiting,
//     or that computer's lease gave it to the person.
//   - Every agent_rows call names the prompts held here (heldRemotePrompts),
//     which renews their lease there: those whose wake is queued for, or
//     was taken in by, a holder that can answer. A wake that is dropped,
//     withdrawn or whose session start fails ends its hold, and the prompt
//     moves to the spawn's caller now when that changed, or goes back.
//   - thread_reply from the holder answers it there (agent_prompt_reply).
//   - A handoff, an archive, a delete, a rest or a pause of the holder
//     reconciles its prompts as a local prompt's are (reroutePromptsFrom):
//     a holder that still holds keeps them, a successor takes them, and
//     otherwise they go back.
//
// Each delivery has its own identity, the wake's send id
// agent-ask:<token>:<boot>-<n> (parseWorkerPromptWake), and a callback of an
// earlier delivery changes nothing. After a restart the registry is empty,
// a queued wake of the previous boot is discarded, and the next listing
// registers the prompt again under a new delivery.

// remotePromptCallTimeout bounds one agent_prompt_reply or
// agent_prompt_return: the answer is written there before the call returns.
const remotePromptCallTimeout = 30 * time.Second

type remotePromptState int

const (
	// remotePromptRouted: its wake is queued for, or was taken in by, its
	// holder, or is about to be.
	remotePromptRouted remotePromptState = iota
	// remotePromptAnswering: the holder's answer is on its way there.
	remotePromptAnswering
	// remotePromptAnswered: that computer took the holder's answer.
	remotePromptAnswered
	// remotePromptReturned: given back; that computer still lists it.
	remotePromptReturned
	// remotePromptClosed: that computer no longer lists it.
	remotePromptClosed
)

// remotePromptKey names one prompt: its computer and that computer's token.
type remotePromptKey struct{ computerID, token string }

// remoteWorkerPrompt is one prompt a paired computer's worker put to a
// thread here.
type remoteWorkerPrompt struct {
	key   remotePromptKey
	spawn store.ThreadRequest
	// computerName is that computer's name as this one knows it.
	computerName string
	prompt       ThreadAgentPrompt
	// holder is the thread its current wake went to, and delivery that
	// wake's identity; both empty before its first delivery.
	holder, delivery string
	// held says its wake reached the queue and nothing has ended it since.
	held bool
	// rejected marks a delivery its holder did not take while it answered:
	// an answer that does not land moves the prompt on then
	// (endRemoteAnswer).
	rejected bool
	state    remotePromptState
	answer   string
	closedAt time.Time
}

func (p *remoteWorkerPrompt) sendID() string {
	return workerPromptSendIDRoot + p.key.token + ":" + p.delivery
}

// remoteWorkerPrompts is the registry of the prompts paired computers'
// workers put to threads here. Its deliveries and reconciles run on the
// prompts' serial queue (workerPrompts.run).
type remoteWorkerPrompts struct {
	mu    sync.Mutex
	boot  string
	next  uint64
	byKey map[remotePromptKey]*remoteWorkerPrompt
}

// workerPromptWake is a worker prompt wake's send id read back: the
// registration's token and, for a paired computer's prompt, its delivery.
type workerPromptWake struct{ token, delivery string }

// parseWorkerPromptWake reads a worker prompt wake's send id: agent-ask:<token>
// for a prompt asked here, agent-ask:<token>:<delivery> for one a paired
// computer's worker asked.
func parseWorkerPromptWake(sendID string) (workerPromptWake, bool) {
	rest, ok := strings.CutPrefix(sendID, workerPromptSendIDRoot)
	if !ok || rest == "" {
		return workerPromptWake{}, false
	}
	token, delivery, _ := strings.Cut(rest, ":")
	return workerPromptWake{token: token, delivery: delivery}, true
}

// noteRemotePrompts takes up what one recorded agent_rows answer says of
// the prompts the spawn's worker routed here. A failed call says nothing;
// a refusal of the thread as not the spawn's says there are none.
func (a *App) noteRemotePrompts(spawn store.ThreadRequest, listing ThreadAgentRows, err error) <-chan error {
	if err != nil && !threadAgentPeerRefused(err) {
		return nil
	}
	prompts := listing.Prompts
	if err != nil {
		prompts = nil
	}
	if len(prompts) == 0 && !a.workerPrompts.remote.knows(spawn.Token) {
		return nil
	}
	done := make(chan error, 1)
	if !a.workerPrompts.run(func() { done <- a.reconcileRemotePrompts(spawn, prompts) }) {
		done <- context.Canceled
	}
	return done
}

// knows reports whether any prompt of the spawn is registered.
func (r *remoteWorkerPrompts) knows(spawnToken string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, prompt := range r.byKey {
		if prompt.spawn.Token == spawnToken {
			return true
		}
	}
	return false
}

// reconcileRemotePrompts registers and delivers the spawn's listed prompts
// that are new, or that an earlier answer left out, and closes those no
// longer listed, withdrawing their wakes. It runs on the prompts' serial
// queue.
func (a *App) reconcileRemotePrompts(spawn store.ThreadRequest, listed []ThreadAgentPrompt) error {
	r := &a.workerPrompts.remote
	now := time.Now()
	byToken := make(map[string]ThreadAgentPrompt, len(listed))
	for _, prompt := range listed {
		byToken[prompt.Token] = prompt
	}
	var deliver []remotePromptKey
	var withdraw []remoteWorkerPrompt
	r.mu.Lock()
	if r.byKey == nil {
		r.byKey = make(map[remotePromptKey]*remoteWorkerPrompt)
	}
	for key, prompt := range r.byKey {
		if !prompt.closedAt.IsZero() && now.Sub(prompt.closedAt) > workerPromptRetention {
			delete(r.byKey, key)
			continue
		}
		if prompt.spawn.Token != spawn.Token {
			continue
		}
		if _, still := byToken[key.token]; still {
			continue
		}
		switch prompt.state {
		case remotePromptRouted:
			if prompt.delivery != "" {
				withdraw = append(withdraw, *prompt)
			}
			prompt.state, prompt.closedAt, prompt.held = remotePromptClosed, now, false
		case remotePromptReturned:
			prompt.state, prompt.closedAt = remotePromptClosed, now
		}
	}
	name := a.remoteComputerNames()[spawn.TargetComputerID]
	for token, listing := range byToken {
		key := remotePromptKey{computerID: spawn.TargetComputerID, token: token}
		prompt := r.byKey[key]
		switch {
		case prompt == nil:
			r.byKey[key] = &remoteWorkerPrompt{key: key, spawn: spawn, computerName: name, prompt: listing}
			deliver = append(deliver, key)
		case prompt.state == remotePromptRouted && !prompt.held && prompt.delivery == "":
			deliver = append(deliver, key)
		case prompt.state == remotePromptClosed:
			// An earlier answer left it out, read before this one.
			prompt.state, prompt.closedAt, prompt.prompt, prompt.spawn = remotePromptRouted, time.Time{}, listing, spawn
			deliver = append(deliver, key)
		}
	}
	r.mu.Unlock()
	for _, prompt := range withdraw {
		a.withdrawRemotePromptWake(prompt)
	}
	var errs []error
	for _, key := range deliver {
		if err := a.deliverRemotePrompt(key); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// deliverRemotePrompt queues the wake of a routed prompt to its spawn's
// caller now, under a new delivery, and withdraws the wake of the delivery
// it replaces; a prompt nobody here can take goes back. It runs on the
// prompts' serial queue.
func (a *App) deliverRemotePrompt(key remotePromptKey) error {
	r := &a.workerPrompts.remote
	r.mu.Lock()
	prompt := r.byKey[key]
	if prompt == nil || prompt.state != remotePromptRouted {
		r.mu.Unlock()
		return nil
	}
	spawnToken := prompt.spawn.Token
	r.mu.Unlock()
	holder, _, found, err := a.store.RemotePromptHolder(spawnToken)
	if err != nil {
		return err
	}
	if !found {
		a.returnRemotePrompt(key, "")
		return nil
	}
	r.mu.Lock()
	prompt = r.byKey[key]
	if prompt == nil || prompt.state != remotePromptRouted {
		r.mu.Unlock()
		return nil
	}
	replaced := *prompt
	if r.boot == "" {
		r.boot = entityid.New()
	}
	r.next++
	prompt.holder, prompt.delivery, prompt.held = holder, r.boot+"-"+strconv.FormatUint(r.next, 10), false
	current := *prompt
	r.mu.Unlock()
	if replaced.delivery != "" {
		a.withdrawRemotePromptWake(replaced)
	}
	message, err := workerPromptMessage(usermessage.Reporter{
		Label: current.prompt.Title, ThreadID: current.spawn.TargetThreadID,
		Computer: current.computerName, ComputerID: key.computerID,
	}, current.workerPrompt())
	if err != nil {
		r.retryDelivery(key, current.delivery)
		return fmt.Errorf("prepare prompt %s: %w", key.token, err)
	}
	live := func() bool { return r.live(key, current.delivery) }
	queued := false
	err = a.queueAgentNotice(agentNotice{
		threadID:  holder,
		recipient: func(threadID string) { r.moveTo(key, current.delivery, threadID) },
		sendID:    current.sendID(),
		prepare: func() (usermessage.AgentMessage, bool, error) {
			queued = live()
			return message, queued, nil
		},
		// A wake keep withdraws is no longer the prompt's current delivery,
		// so its withdrawal moves nothing on.
		keep:   func() (bool, error) { return live(), nil },
		failed: func() { a.remotePromptNotTaken(key, current.delivery) },
		startFailed: func(err error) string {
			return fmt.Sprintf("Thread %q on %s is waiting on this thread's answer, but this thread's agent could not start: %v. The prompt goes back to the person there.",
				current.prompt.Title, current.computerName, err)
		},
	})
	if err != nil {
		r.retryDelivery(key, current.delivery)
		return fmt.Errorf("queue prompt %s for %s: %w", key.token, holder, err)
	}
	if queued {
		r.hold(key, current.delivery)
	}
	return nil
}

func (r *remoteWorkerPrompts) retryDelivery(key remotePromptKey, delivery string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.byKey[key]; p != nil && p.state == remotePromptRouted && p.delivery == delivery {
		p.delivery, p.holder, p.held = "", "", false
	}
}

// workerPrompt is the registration as the wake renders it.
func (p *remoteWorkerPrompt) workerPrompt() *workerPrompt {
	rendered := &workerPrompt{token: p.key.token}
	if approval := p.prompt.Approval; approval != nil {
		rendered.approval = &provider.ApprovalRequest{ToolName: approval.ToolName, Description: approval.Description}
		if approval.Input != "" {
			rendered.approval.Input = json.RawMessage(approval.Input)
		}
		return rendered
	}
	rendered.question = &provider.UserInputRequest{Questions: p.prompt.Questions}
	return rendered
}

// live reports whether delivery is the routed prompt's current one.
func (r *remoteWorkerPrompts) live(key remotePromptKey, delivery string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	prompt := r.byKey[key]
	return prompt != nil && prompt.state == remotePromptRouted && prompt.delivery == delivery
}

// hold records that delivery's wake reached its holder's queue.
func (r *remoteWorkerPrompts) hold(key remotePromptKey, delivery string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if prompt := r.byKey[key]; prompt != nil && prompt.state == remotePromptRouted && prompt.delivery == delivery {
		prompt.held = true
	}
}

// moveTo records the thread delivery's wake is queued in: the holder's
// assistant's current thread when New Thread, Same Assistant superseded it.
func (r *remoteWorkerPrompts) moveTo(key remotePromptKey, delivery, threadID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if prompt := r.byKey[key]; prompt != nil && prompt.delivery == delivery {
		prompt.holder = threadID
	}
}

// remotePromptNotTaken is a delivery whose wake did not reach its holder,
// or was dropped, withdrawn or never started: its hold ends, and the
// prompt moves on (rerouteRemotePrompt). A delivery a later one replaced
// changes nothing.
func (a *App) remotePromptNotTaken(key remotePromptKey, delivery string) {
	r := &a.workerPrompts.remote
	r.mu.Lock()
	prompt := r.byKey[key]
	if prompt != nil && prompt.state == remotePromptAnswering && prompt.delivery == delivery {
		prompt.held, prompt.rejected = false, true
	}
	if prompt == nil || prompt.state != remotePromptRouted || prompt.delivery != delivery {
		r.mu.Unlock()
		return
	}
	prompt.held = false
	r.mu.Unlock()
	a.workerPrompts.run(func() { a.rerouteRemotePrompt(key, delivery, true) })
}

// handBackRemotePromptWake is handBackWorkerPromptWake for a paired
// computer's prompt.
func (a *App) handBackRemotePromptWake(wake workerPromptWake) {
	r := &a.workerPrompts.remote
	r.mu.Lock()
	var key remotePromptKey
	for candidate, prompt := range r.byKey {
		if candidate.token == wake.token && prompt.delivery == wake.delivery {
			key = candidate
		}
	}
	r.mu.Unlock()
	if key.token != "" {
		a.remotePromptNotTaken(key, wake.delivery)
	}
}

// rerouteRemotePrompt reconciles a routed prompt with its spawn's caller
// now: a different thread that can take it gets a new delivery, the same
// one keeps it unless it could not take it (reject), and otherwise it goes
// back. A non-empty delivery names the delivery this is about; a later
// one changes nothing. It runs on the prompts' serial queue.
func (a *App) rerouteRemotePrompt(key remotePromptKey, delivery string, reject bool) {
	r := &a.workerPrompts.remote
	r.mu.Lock()
	prompt := r.byKey[key]
	if prompt == nil || prompt.state != remotePromptRouted || (delivery != "" && prompt.delivery != delivery) {
		r.mu.Unlock()
		return
	}
	from, spawnToken := prompt.holder, prompt.spawn.Token
	r.mu.Unlock()
	holder, _, found, err := a.store.RemotePromptHolder(spawnToken)
	if err != nil {
		log.Printf("worker prompts: %v", err)
	}
	switch {
	case err == nil && found && holder != from:
		if err := a.deliverRemotePrompt(key); err != nil {
			log.Printf("retry routed prompt %s: %v", key.token, err)
		}
	case err == nil && found && !reject:
	default:
		a.returnRemotePrompt(key, delivery)
	}
}

// rerouteRemotePromptsFrom reconciles the routed prompts held by threadID,
// which a handoff, an archive, a delete, a rest or a pause changed
// (reroutePromptsFrom). One being answered is reconciled when its answer
// ends (endRemoteAnswer).
func (a *App) rerouteRemotePromptsFrom(threadID string) {
	r := &a.workerPrompts.remote
	r.mu.Lock()
	var keys []remotePromptKey
	for key, prompt := range r.byKey {
		if prompt.state == remotePromptRouted && prompt.holder == threadID {
			keys = append(keys, key)
		}
	}
	r.mu.Unlock()
	for _, key := range keys {
		a.workerPrompts.run(func() { a.rerouteRemotePrompt(key, "", false) })
	}
}

// returnRemotePrompt gives a routed prompt back to the computer that holds
// its worker, which puts it to the person there, and withdraws its wake
// here. The prompt is kept as returned until that computer stops listing
// it. The return is best effort: a prompt this computer no longer names as
// held goes back there when its lease runs out. Nothing is sent for a
// worker this computer owes an archive or delete (owesWorkerEnd): that
// operation ends the prompt with the worker, so the person there is not
// asked about a worker on its way out, and the lease covers one that
// cannot arrive.
func (a *App) returnRemotePrompt(key remotePromptKey, delivery string) {
	r := &a.workerPrompts.remote
	r.mu.Lock()
	prompt := r.byKey[key]
	if prompt == nil || prompt.state != remotePromptRouted || (delivery != "" && prompt.delivery != delivery) {
		r.mu.Unlock()
		return
	}
	prompt.state, prompt.held = remotePromptReturned, false
	returned := *prompt
	r.mu.Unlock()
	if returned.delivery != "" {
		a.withdrawRemotePromptWake(returned)
	}
	ending, err := a.owesWorkerEnd(returned.spawn.Token)
	if err != nil {
		log.Printf("worker prompts: %v", err)
	}
	if ending {
		return
	}
	a.threadRequestsWG.Go(func() {
		ctx, cancel := context.WithTimeout(a.lifeCtx(), remotePromptCallTimeout)
		defer cancel()
		if err := a.callPeerPrompt(ctx, returned.spawn, threadAgentPeerPromptReturn,
			threadPromptPeerArgs{Spawn: returned.spawn.Token, Token: key.token}, &struct{}{}); err != nil {
			log.Printf("worker prompts: give %s back to %s: %v", key.token, key.computerID, err)
		}
	})
}

// forgetRemotePrompts closes the prompts a computer no longer paired put to
// threads here, an answer on its way included, and withdraws their wakes.
// Nothing is sent to it. A late thread_reply on one is told it is closed.
// It runs on the prompts' serial queue (forgetPeerAgentRows).
func (a *App) forgetRemotePrompts(computerID string) {
	r := &a.workerPrompts.remote
	now := time.Now()
	var withdraw []remoteWorkerPrompt
	r.mu.Lock()
	for key, prompt := range r.byKey {
		if key.computerID != computerID || prompt.state == remotePromptClosed {
			continue
		}
		if prompt.state == remotePromptRouted && prompt.delivery != "" {
			withdraw = append(withdraw, *prompt)
		}
		prompt.state, prompt.closedAt, prompt.held = remotePromptClosed, now, false
	}
	r.mu.Unlock()
	for _, prompt := range withdraw {
		a.withdrawRemotePromptWake(prompt)
	}
}

// withdrawRemotePromptWake takes a delivery's wake out of its holder's
// queue, when it is still there. The removal hands back nothing: by then
// the delivery is not the prompt's current one, or the prompt is not
// routed.
func (a *App) withdrawRemotePromptWake(prompt remoteWorkerPrompt) {
	if _, err := a.removeQueuedItem(a.lifeCtx(), prompt.holder, prompt.sendID()); err != nil {
		log.Printf("worker prompts: withdraw the wake for %s of %s: %v", prompt.key.token, prompt.key.computerID, err)
	}
}

// heldRemotePrompts names the spawn's prompts held here, for its next
// agent_rows call: routed or being answered, with the wake in its holder's
// queue or taken in.
func (a *App) heldRemotePrompts(spawn store.ThreadRequest) []string {
	r := &a.workerPrompts.remote
	r.mu.Lock()
	defer r.mu.Unlock()
	var held []string
	for key, prompt := range r.byKey {
		if prompt.spawn.Token == spawn.Token && key.computerID == spawn.TargetComputerID &&
			prompt.isHeld() {
			held = append(held, key.token)
		}
	}
	return held
}

// answerRemoteWorkerPrompt is thread_reply on a paired computer's prompt
// token: from its holder, the answer goes to that computer
// (agent_prompt_reply), which refuses what its own thread_reply would. An
// answer whose call fails without that computer answering is tried once
// more; still uncertain, the agent is told it may not have arrived.
// Handled is false for any other token.
func (a *App) answerRemoteWorkerPrompt(ctx context.Context, caller threadtools.Caller, call threadtools.ReplyCall) (ack threadtools.ReplyAck, handled bool, err error) {
	r := &a.workerPrompts.remote
	answer := workerPromptAnswerKey(call)
	r.mu.Lock()
	var prompt *remoteWorkerPrompt
	for key, candidate := range r.byKey {
		if key.token == call.Token {
			prompt = candidate
		}
	}
	if prompt == nil {
		r.mu.Unlock()
		return threadtools.ReplyAck{}, false, nil
	}
	ack = threadtools.ReplyAck{Token: call.Token, State: threadtools.RequestReplied,
		SourceThreadID: prompt.spawn.TargetThreadID, SourceComputer: prompt.computerName,
		SourceComputerID: prompt.key.computerID, Prompt: true}
	if prompt.holder != caller.ThreadID {
		r.mu.Unlock()
		return ack, true, errorsx.Public(threadtools.CodeRequestNotYours,
			"That token belongs to a prompt sent to another thread. Answer only the prompts that woke this thread.", nil)
	}
	switch prompt.state {
	case remotePromptRouted:
		prompt.state, prompt.answer = remotePromptAnswering, answer
	case remotePromptAnswering:
		err = errWorkerPromptAnswering
	case remotePromptAnswered:
		if prompt.answer != answer {
			err = errWorkerPromptAnswered
		}
	default:
		err = errWorkerPromptClosed
	}
	state, key, spawn, name := prompt.state, prompt.key, prompt.spawn, prompt.computerName
	r.mu.Unlock()
	if err != nil || state == remotePromptAnswered {
		return ack, true, err
	}
	args := threadPromptPeerArgs{Spawn: spawn.Token, Token: call.Token, Decision: call.Decision, Text: call.Text, Answers: call.Answers}
	var out threadPromptPeerAnswer
	err = a.callPeerPrompt(ctx, spawn, threadAgentPeerPromptReply, args, &out)
	if _, _, uncertain := threadErrorDetails("reply", err); err != nil && uncertain && ctx.Err() == nil {
		err = a.callPeerPrompt(ctx, spawn, threadAgentPeerPromptReply, args, &out)
	}
	if err == nil {
		a.endRemoteAnswer(key, remotePromptAnswered)
		ack.Accepted = out.Accepted
		return ack, true, nil
	}
	a.endRemoteAnswer(key, remotePromptRouted)
	code, message, uncertain := threadErrorDetails("reply", err)
	if !uncertain {
		return ack, true, errorsx.Public(code, message, err)
	}
	log.Printf("worker prompts: answer %s of %s: %v", call.Token, key.computerID, err)
	return ack, true, errorsx.Public(code, fmt.Sprintf(
		"Your answer may not have reached the worker on %s (%s). The prompt stays open there until that computer says otherwise; the same answer again is safe.",
		name, message), err)
}

// endRemoteAnswer is the one exit from answering. A prompt the answer left
// routed was skipped by every reconcile meanwhile, so it is reconciled now
// with its spawn's caller (rerouteRemotePrompt): a handoff moves it to the
// successor, a holder archived, deleted, at rest or paused gives it back,
// and a delivery the holder did not take meanwhile moves it on.
func (a *App) endRemoteAnswer(key remotePromptKey, state remotePromptState) {
	routed, rejected := a.workerPrompts.remote.endAnswer(key, state)
	if routed {
		a.workerPrompts.run(func() { a.rerouteRemotePrompt(key, "", rejected) })
	}
}

// endAnswer records how the holder's answer ended, for endRemoteAnswer
// alone, and returns whether the prompt is routed again and whether its
// delivery was not taken meanwhile.
func (r *remoteWorkerPrompts) endAnswer(key remotePromptKey, state remotePromptState) (bool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prompt := r.byKey[key]
	if prompt == nil || prompt.state != remotePromptAnswering {
		return false, false
	}
	prompt.state = state
	rejected := prompt.rejected
	prompt.rejected = false
	if state == remotePromptRouted {
		prompt.answer = ""
		return true, rejected
	}
	prompt.held, prompt.closedAt = false, time.Now()
	return false, false
}

// callPeerPrompt forwards agent_prompt_reply or agent_prompt_return to the
// spawn's computer, from the thread its receipt names.
func (a *App) callPeerPrompt(ctx context.Context, spawn store.ThreadRequest, tool string, args threadPromptPeerArgs, out any) error {
	if a.backends == nil {
		return errors.New("no paired computers are reachable")
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("encode %s: %w", tool, err)
	}
	call, cancel := context.WithTimeout(ctx, remotePromptCallTimeout)
	defer cancel()
	var reply ThreadPeerReply
	if err := a.backends.CallThreadPeer(call, spawn.TargetComputerID, "ThreadToolCall", &reply, ThreadPeerCall{
		Tool: tool, Args: encoded, Token: newThreadRequestToken(), Source: a.threadAgentCaller(spawn.MadeIn()),
	}); err != nil {
		return err
	}
	if err := json.Unmarshal(reply.Result, out); err != nil {
		return fmt.Errorf("read %s from %s: %w", tool, spawn.TargetComputerID, err)
	}
	return nil
}
