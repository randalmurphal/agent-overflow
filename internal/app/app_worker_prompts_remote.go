package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/entityid"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// The prompts a worker on a paired computer routed to a thread here, the
// spawner that owns it there through its spawn's link. That computer holds
// them (app_worker_prompts_peer.go) and lists them on every agent_rows
// read of the spawn by its owner or an open pane. This computer keeps them in memory only:
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
func (a *App) noteRemotePrompts(origin attachedbackends.PeerRead, spawn store.ThreadRequest, listing ThreadAgentRows, err error) <-chan error {
	if err != nil && !threadAgentPeerRefused(err) {
		return nil
	}
	prompts := listing.Prompts
	if err != nil {
		prompts = nil
	}

	done := make(chan error, 1)
	if !a.workerPrompts.run(func() {
		if len(prompts) == 0 && !a.workerPrompts.remote.knows(spawn.Token) {
			done <- nil
			return
		}
		done <- origin.Commit(func() error { return a.reconcileRemotePrompts(spawn, prompts) })
	}) {
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
		if !prompt.closedAt.IsZero() && (prompt.state != remotePromptClosed || prompt.delivery == "") && now.Sub(prompt.closedAt) > workerPromptRetention {
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
			prompt.state, prompt.closedAt, prompt.held = remotePromptClosed, now, false
		case remotePromptReturned:
			prompt.state, prompt.closedAt = remotePromptClosed, now
		}
		if prompt.state == remotePromptClosed && prompt.delivery != "" {
			withdraw = append(withdraw, *prompt)
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
		case prompt.state == remotePromptRouted:
			deliver = append(deliver, key)
		case prompt.state == remotePromptClosed:
			// An earlier answer left it out, read before this one.
			prompt.state, prompt.closedAt, prompt.prompt, prompt.spawn = remotePromptRouted, time.Time{}, listing, spawn
			deliver = append(deliver, key)
		}
	}
	r.mu.Unlock()
	var errs []error
	for _, prompt := range withdraw {
		if err := a.withdrawRemotePromptWake(prompt); err != nil {
			errs = append(errs, err)
		}
	}
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
	if prompt.held && prompt.holder == holder {
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
		if err := a.withdrawRemotePromptWake(replaced); err != nil {
			r.mu.Lock()
			if p := r.byKey[key]; p != nil && p.delivery == current.delivery {
				p.holder, p.delivery, p.held = replaced.holder, replaced.delivery, replaced.held
			}
			r.mu.Unlock()
			return err
		}
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
