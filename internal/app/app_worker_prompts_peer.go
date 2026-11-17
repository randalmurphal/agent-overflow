package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
)

// The prompts this computer's workers route to a thread on a paired
// computer, whose owned spawn's link owns them (store.SpawnOwnerOf). They
// are pulled, never pushed: this computer cannot call the spawner's
// computer, and that computer already asks about its spawn at the poll's
// pace (agent_rows).
//
//   - A prompt announced routed there is registered held for that computer
//     (holdPeerWorkerPrompt) and nothing is queued here.
//   - agent_rows from the link's own device, for the link's source thread
//     and naming the link's spawn token, lists the link's held prompts
//     (ThreadAgentRows.Prompts), and renews the lease of those it names as
//     held (threadAgentPeerArgs.Held). Listing alone renews nothing.
//   - agent_prompt_reply answers one, as thread_reply does here
//     (answerRoutedPrompt); agent_prompt_return gives one back.
//   - Listing, renewing, answering and returning need the calling session
//     to hold approvals:respond, as answering the person's own prompts
//     does; agent_rows without it lists no prompt content.
//   - A lease hands a prompt back to where the worker's prompts route now,
//     the person when that is the same thread, once the lease has passed
//     since it was registered or last held, whichever is later: a
//     computer that stops answering, or keeps answering without taking
//     the prompt, leaves it with the person here.
//   - A link that ends hands its prompts back at once
//     (handBackLinkPrompts, through announceReleasedWorkers).
//
// One binding (remotePromptFor) is the only door for a renewal, an answer
// and a return.

// Forwarded tool names of the prompts held for a paired computer.
const (
	threadAgentPeerPromptReply  = "agent_prompt_reply"
	threadAgentPeerPromptReturn = "agent_prompt_return"
)

// workerPromptLease is how long a prompt stays routed to a paired
// computer's thread after it was registered or that computer last said it
// holds it (owner choice).
const workerPromptLease = 2 * time.Minute

// remotePromptHold is a prompt routed to a thread on a paired computer:
// the route, the link it was routed through, and its lease. heldAt, when
// that computer last said it holds the prompt, and timer are guarded by
// workerPrompts.mu.
type remotePromptHold struct {
	computerID, computerName, title string
	device, spawnToken              string
	registeredAt                    time.Time
	heldAt                          time.Time
	timer                           *time.Timer
}

// leaseEnd is when the lease runs out: lease after the later of the
// prompt's registration and its last renewal.
func (h *remotePromptHold) leaseEnd(lease time.Duration) time.Time {
	from := h.registeredAt
	if h.heldAt.After(from) {
		from = h.heldAt
	}
	return from.Add(lease)
}

// ThreadAgentPrompt is one prompt of the thread an agent_rows call lists,
// routed to the calling device's spawn thread: what that computer puts to
// its thread, and the token it answers with.
type ThreadAgentPrompt struct {
	Token string `json:"token"`
	// Title is the worker's title now.
	Title string `json:"title"`
	// Approval is set for an approval; Questions for a question.
	Approval  *ThreadAgentPromptApproval   `json:"approval,omitempty"`
	Questions []provider.UserInputQuestion `json:"questions,omitempty"`
}

// ThreadAgentPromptApproval is what an approval asks.
type ThreadAgentPromptApproval struct {
	ToolName    string `json:"toolName"`
	Description string `json:"description,omitempty"`
	Input       string `json:"input,omitempty"`
}

// threadPromptPeerArgs are agent_prompt_reply's and agent_prompt_return's
// arguments: the spawn whose link owns the worker, the prompt's token, and
// a reply's answer as thread_reply takes it.
type threadPromptPeerArgs struct {
	Spawn    string   `json:"spawn"`
	Token    string   `json:"token"`
	Decision string   `json:"decision,omitempty"`
	Text     string   `json:"text,omitempty"`
	Answers  []string `json:"answers,omitempty"`
}

// threadPromptPeerAnswer is agent_prompt_reply's answer: false when the
// answer repeated the one already written.
type threadPromptPeerAnswer struct {
	Accepted bool `json:"accepted"`
}

// errPeerPromptNotYours refuses a renewal, an answer or a return the
// binding does not admit.
var errPeerPromptNotYours = errorsx.Public(threadtools.CodeRequestNotYours,
	"That token names no prompt this computer routed to your thread through that spawn.", nil)

// promptLease is workerPromptLease, or the shorter one a test set.
func (a *App) promptLease() time.Duration {
	if a.workerPromptLeaseOverride > 0 {
		return a.workerPromptLeaseOverride
	}
	return workerPromptLease
}

// holdPeerWorkerPrompt registers a prompt announced routed to a paired
// computer's thread, held for the link that owns the worker, and arms its
// lease. A link that no longer routes there, or a failed read of it, hands
// the prompt back at once.
func (a *App) holdPeerWorkerPrompt(route provider.PromptRoute, prompt workerPrompt) {
	link, linked, err := a.store.SpawnOwnerOf(prompt.workerID)
	prompt.agentID = route.ThreadID
	prompt.remote = &remotePromptHold{
		computerID: route.ComputerID, computerName: route.ComputerName, title: route.Title, registeredAt: time.Now(),
	}
	if err == nil && linked {
		prompt.remote.device, prompt.remote.spawnToken = link.DeviceID, link.Token
	}
	registered, replaced := a.workerPrompts.add(prompt, a.workerPromptOpen)
	if replaced != nil {
		a.withdrawWorkerPromptWake(*replaced)
	}
	if err != nil || !linked || link.ComputerID != route.ComputerID || link.ThreadID != route.ThreadID {
		log.Printf("worker prompts: %s of %s is not held for %s: its link is gone (%v)", prompt.requestID, prompt.workerID, route.ComputerID, err)
		a.handBackWorkerPrompt(registered.token)
		return
	}
	a.armPromptLease(registered.token, a.promptLease())
}

// armPromptLease checks the lease of the prompt held under token after d,
// in place of a check already armed.
func (a *App) armPromptLease(token string, d time.Duration) {
	w := &a.workerPrompts
	w.mu.Lock()
	defer w.mu.Unlock()
	prompt := w.byToken[token]
	if prompt == nil || prompt.remote == nil || prompt.state != workerPromptRouted {
		return
	}
	if prompt.remote.timer != nil {
		prompt.remote.timer.Stop()
	}
	prompt.remote.timer = time.AfterFunc(d, func() { w.run(func() { a.expirePromptLease(token) }) })
}

// promptLinkStands reports whether the link a held prompt was routed
// through still owns its worker and routes its prompts to the prompt's
// agent on that computer (store.SpawnOwnerOf). A failed read is a link
// that does not stand: the prompt goes to the person here.
func (a *App) promptLinkStands(prompt workerPrompt) bool {
	link, linked, err := a.store.SpawnOwnerOf(prompt.workerID)
	if err != nil {
		log.Printf("worker prompts: %v", err)
	}
	return err == nil && linked && link.Token == prompt.remote.spawnToken && link.DeviceID == prompt.remote.device &&
		link.ComputerID == prompt.remote.computerID && link.ThreadID == prompt.agentID
}

// expirePromptLease hands back the prompt held under token once its lease
// has run out, and checks again when it was renewed meanwhile. A prompt
// being answered is checked when its answer ends (endPromptAnswer).
func (a *App) expirePromptLease(token string) {
	w := &a.workerPrompts
	w.mu.Lock()
	prompt := w.byToken[token]
	if prompt == nil || prompt.remote == nil || prompt.state != workerPromptRouted {
		w.mu.Unlock()
		return
	}
	left := time.Until(prompt.remote.leaseEnd(a.promptLease()))
	w.mu.Unlock()
	if left > 0 {
		a.armPromptLease(token, left)
		return
	}
	log.Printf("worker prompts: %s of %s goes back: %s did not hold it", prompt.requestID, prompt.workerID, prompt.remote.computerID)
	a.handBackWorkerPrompt(token)
}

// stopLeases stops every armed lease, at shutdown.
func (w *workerPrompts) stopLeases() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, prompt := range w.byToken {
		if prompt.remote != nil && prompt.remote.timer != nil {
			prompt.remote.timer.Stop()
		}
	}
}

// remotePromptFor is the binding every renewal, answer and return passes:
// the prompt under promptToken is held for a paired computer through the
// spawn spawnToken names, and that spawn's link, reached for the calling
// device and its source thread (store.ReachSpawnLink), still owns its
// worker.
func (a *App) remotePromptFor(device, source, spawnToken, promptToken string) (workerPrompt, error) {
	prompt, found := a.workerPrompts.get(promptToken)
	if !found || prompt.remote == nil || prompt.remote.spawnToken != spawnToken ||
		prompt.remote.device != device || prompt.agentID != source {
		return workerPrompt{}, errPeerPromptNotYours
	}
	reach, found, err := a.store.ReachSpawnLink(spawnToken, device, source, time.Now().UnixMilli())
	if err != nil {
		return workerPrompt{}, fmt.Errorf("read the link of thread %s: %w", prompt.workerID, err)
	}
	if !found || !reach.Owned || reach.TargetThreadID != prompt.workerID {
		return workerPrompt{}, errPeerPromptNotYours
	}
	return prompt, nil
}

// peerPromptsOf answers the prompts part of agent_rows. A call that names
// the spawn whose link reaches threadID, from that link's device and source
// thread, is its owner's contact, whatever the thread's placement
// (store.ReachSpawnLink). While the link owns the thread, a calling session
// that may answer prompts (approvals:respond) gets the link's held prompts,
// with the lease of each it says it holds renewed; one that may not gets
// none and renews nothing, and still reads the thread's blocked state from
// the row. Any other call lists none.
func (a *App) peerPromptsOf(ctx context.Context, owner, source, title string, args threadAgentPeerArgs) ([]ThreadAgentPrompt, error) {
	if args.Spawn == "" {
		return nil, nil
	}
	reach, found, err := a.store.ReachSpawnLink(args.Spawn, owner, source, time.Now().UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("read the link of thread %s: %w", args.ThreadID, err)
	}
	if !found || !reach.Owned || reach.TargetThreadID != args.ThreadID {
		return nil, nil
	}
	if a.requireScope(ctx, transport.ScopeApprovalsRespond, "") != nil {
		return nil, nil
	}
	for _, token := range args.Held {
		if _, err := a.remotePromptFor(owner, source, args.Spawn, token); err == nil {
			a.workerPrompts.renew(token, time.Now())
		}
	}
	var out []ThreadAgentPrompt
	for _, prompt := range a.workerPrompts.heldFor(args.Spawn, args.ThreadID) {
		if !a.workerPromptOpenTo(prompt) {
			continue
		}
		out = append(out, peerPromptWire(title, prompt))
	}
	return out, nil
}

// peerPromptWire is a held prompt as agent_rows lists it.
func peerPromptWire(title string, prompt workerPrompt) ThreadAgentPrompt {
	wire := ThreadAgentPrompt{Token: prompt.token, Title: title}
	if request := prompt.approval; request != nil {
		wire.Approval = &ThreadAgentPromptApproval{ToolName: request.ToolName, Description: request.Description, Input: string(request.Input)}
		return wire
	}
	if prompt.question != nil {
		wire.Questions = prompt.question.Questions
	}
	return wire
}

// renew records that the computer a prompt is held for holds it now.
func (w *workerPrompts) renew(token string, now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if prompt := w.byToken[token]; prompt != nil && prompt.remote != nil && prompt.state == workerPromptRouted {
		prompt.remote.heldAt = now
	}
}

// heldFor is the routed prompts of workerID held for the spawn spawnToken
// names, in the order they were registered.
func (w *workerPrompts) heldFor(spawnToken, workerID string) []workerPrompt {
	w.mu.Lock()
	defer w.mu.Unlock()
	var held []workerPrompt
	for _, prompt := range w.byToken {
		if prompt.remote != nil && prompt.remote.spawnToken == spawnToken && prompt.workerID == workerID && prompt.state == workerPromptRouted {
			held = append(held, *prompt)
		}
	}
	slices.SortFunc(held, func(x, y workerPrompt) int {
		if c := x.remote.registeredAt.Compare(y.remote.registeredAt); c != 0 {
			return c
		}
		return strings.Compare(x.token, y.token)
	})
	return held
}

// servePeerPrompt answers agent_prompt_reply and agent_prompt_return.
func (a *App) servePeerPrompt(ctx context.Context, owner string, call ThreadPeerCall) (ThreadPeerReply, error) {
	source := strings.TrimSpace(call.Source.ThreadID)
	var args threadPromptPeerArgs
	if err := json.Unmarshal(call.Args, &args); err != nil || !entityid.Valid(source) || args.Spawn == "" || args.Token == "" {
		return ThreadPeerReply{}, errorsx.Public(threadtools.CodeInvalidRequest,
			"A prompt call must name the thread it came from, its spawn and the prompt's token.", err)
	}
	if err := a.requireScope(ctx, transport.ScopeApprovalsRespond, "answer or give back a prompt routed to your thread"); err != nil {
		return ThreadPeerReply{}, err
	}
	prompt, err := a.remotePromptFor(owner, source, args.Spawn, args.Token)
	if err != nil {
		return ThreadPeerReply{}, err
	}
	var result any
	switch call.Tool {
	case threadAgentPeerPromptReply:
		ack, err := a.answerRoutedPrompt(ctx, prompt, threadtools.ReplyCall{
			Token: args.Token, Text: args.Text, Decision: args.Decision, Answers: args.Answers,
		})
		if err != nil {
			return ThreadPeerReply{}, err
		}
		result = threadPromptPeerAnswer{Accepted: ack.Accepted}
	default:
		a.handBackWorkerPrompt(args.Token)
		result = struct{}{}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return ThreadPeerReply{}, fmt.Errorf("thread tools: encode %s result: %w", call.Tool, err)
	}
	return ThreadPeerReply{Result: encoded}, nil
}

// handBackLinkPrompts hands back at once the prompts held for paired
// computers by the workers among threadIDs, whose links just ended
// (announceReleasedWorkers, its only caller). Each goes where its worker's
// prompts route now, the person for a worker no link owns.
func (a *App) handBackLinkPrompts(threadIDs []string) {
	if len(threadIDs) == 0 {
		return
	}
	a.workerPrompts.run(func() {
		for _, token := range a.workerPrompts.heldByWorkers(threadIDs) {
			a.handBackWorkerPrompt(token)
		}
	})
}

// heldByWorkers is the tokens of the routed prompts held for a paired
// computer whose worker is among workerIDs. One being answered is checked
// against its link when its answer ends (endPromptAnswer).
func (w *workerPrompts) heldByWorkers(workerIDs []string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var tokens []string
	for token, prompt := range w.byToken {
		if prompt.remote != nil && prompt.state == workerPromptRouted && slices.Contains(workerIDs, prompt.workerID) {
			tokens = append(tokens, token)
		}
	}
	return tokens
}
