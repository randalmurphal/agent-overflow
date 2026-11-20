package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// remotePromptNotTaken is a delivery whose wake did not reach its holder,
// or was dropped, withdrawn or never started: the prompt moves to the
// spawn's caller now when that changed, and otherwise waits for its
// holder's next turn (rerouteRemotePrompt). A delivery a later one
// replaced changes nothing.
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
// now: a different thread that can hold it gets a new delivery, the same
// one keeps it, parked until its next turn when it did not take it
// (reject), and with no thread that can hold it the prompt goes back. A
// non-empty delivery names the delivery this is about; a later one changes
// nothing. It runs on the prompts' serial queue.
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
	switch {
	case err != nil:
		a.emitErrorToThread(from, err.Error())
	case found && holder != from:
		if err := a.deliverRemotePrompt(key); err != nil {
			log.Printf("retry routed prompt %s: %v", key.token, err)
		}
	case found && !reject:
	case found:
		if a.workerPrompts.remote.park(key, delivery) {
			log.Printf("worker prompts: %s of %s waits for %s's next turn", key.token, key.computerID, holder)
		}
	default:
		if err := a.returnRemotePrompt(key, delivery); err != nil {
			a.emitErrorToThread(from, err.Error())
		}
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
// it. Nothing is sent for a worker this computer owes an archive or delete
// (owesWorkerEnd): that operation ends the prompt with the worker, so the
// person there is not asked about a worker on its way out.
func (a *App) returnRemotePrompt(key remotePromptKey, delivery string) error {
	r := &a.workerPrompts.remote
	r.mu.Lock()
	prompt := r.byKey[key]
	if prompt == nil || prompt.state != remotePromptRouted || (delivery != "" && prompt.delivery != delivery) {
		r.mu.Unlock()
		return nil
	}
	ending, err := a.owesWorkerEnd(prompt.spawn.Token)
	if err != nil {
		r.mu.Unlock()
		return err
	}
	prompt.state, prompt.held = remotePromptReturned, false
	returned := *prompt
	r.mu.Unlock()
	var withdrawal error
	if returned.delivery != "" {
		withdrawal = a.withdrawRemotePromptWake(returned)
	}
	if ending {
		return withdrawal
	}
	a.threadRequestsWG.Go(func() {
		ctx, cancel := context.WithTimeout(a.lifeCtx(), remotePromptCallTimeout)
		defer cancel()
		if err := a.callPeerPrompt(ctx, returned.spawn, threadAgentPeerPromptReturn,
			threadPromptPeerArgs{Spawn: returned.spawn.Token, Token: key.token}, &struct{}{}); err != nil {
			log.Printf("worker prompts: give %s back to %s: %v", key.token, key.computerID, err)
		}
	})
	return withdrawal
}

// An archive or delete can finish before the prompt queue processes the
// spawner's hand-back. Relinquish its existing holds while the end is owed.
func (a *App) relinquishEndingWorkerPrompts(op store.OwnerOp) error {
	r := &a.workerPrompts.remote
	if (op.Op != store.OwnerOpArchive && op.Op != store.OwnerOpDelete) || !r.knows(op.Token) {
		return nil
	}
	_, _, canAnswer, err := a.store.RemotePromptHolder(op.Token)
	if err != nil || canAnswer {
		return err
	}
	r.mu.Lock()
	var withdrawals []remoteWorkerPrompt
	for _, prompt := range r.byKey {
		if prompt.spawn.Token != op.Token {
			continue
		}
		if prompt.state == remotePromptRouted {
			prompt.state, prompt.held = remotePromptReturned, false
		}
		if prompt.state == remotePromptReturned && prompt.delivery != "" {
			withdrawals = append(withdrawals, *prompt)
		}
	}
	r.mu.Unlock()
	var failures []error
	for _, prompt := range withdrawals {
		if err := a.withdrawRemotePromptWake(prompt); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
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
		if err := a.withdrawRemotePromptWake(prompt); err != nil {
			a.emitErrorToThread(prompt.holder, err.Error())
		}
	}
}

// withdrawRemotePromptWake takes a delivery's wake out of its holder's
// queue, when it is still there. The removal hands back nothing: by then
// the delivery is not the prompt's current one, or the prompt is not
// routed.
func (a *App) withdrawRemotePromptWake(prompt remoteWorkerPrompt) error {
	if _, err := a.removeQueuedItem(a.lifeCtx(), prompt.holder, prompt.sendID()); err != nil {
		return fmt.Errorf("withdraw prompt %s from %s: %w", prompt.key.token, prompt.holder, err)
	}
	r := &a.workerPrompts.remote
	r.mu.Lock()
	if p := r.byKey[prompt.key]; p != nil && p.delivery == prompt.delivery && (p.state == remotePromptClosed || p.state == remotePromptReturned) {
		p.delivery = ""
	}
	r.mu.Unlock()
	return nil
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
	code, message, uncertain := threadErrorDetails("reply", err)
	if code == peerPromptClosed {
		r.mu.Lock()
		if r.byKey[key] != prompt || prompt.state != remotePromptAnswering {
			r.mu.Unlock()
			return ack, true, errWorkerPromptClosed
		}
		prompt.state, prompt.closedAt, prompt.held, prompt.refusedClosed = remotePromptClosed, time.Now(), false, true
		closed := *prompt
		r.mu.Unlock()
		if closed.delivery != "" {
			return ack, true, errors.Join(errWorkerPromptClosed, a.withdrawRemotePromptWake(closed))
		}
		return ack, true, errWorkerPromptClosed
	}
	a.endRemoteAnswer(key, remotePromptRouted)
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
