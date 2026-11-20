package app

import (
	"context"
	"encoding/json"
	"errors"
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
// computer that spawned them, by the rule a local spawner's are
// (store.SpawnOwnerOf): always while the spawn's link owns the worker, and
// with a sidebar row in a run that thread started. They are pulled, never
// pushed: this computer cannot call the spawner's computer, and that
// computer already asks about its spawn at the poll's pace (agent_rows).
//
//   - A prompt announced routed there is registered held for that computer
//     (holdPeerWorkerPrompt) and nothing is queued here.
//   - agent_rows from the link's own device, for the link's source thread
//     and naming the link's spawn token, lists the link's held prompts
//     (ThreadAgentRows.Prompts).
//   - agent_prompt_reply answers one, as thread_reply does here
//     (answerRoutedPrompt); agent_prompt_return gives one back.
//   - Listing, renewing, answering and returning need the calling session
//     to hold approvals:respond, as answering the person's own prompts
//     does; agent_rows without it lists no prompt content.
//   - While the worker's prompts are that thread's, its prompt stays held
//     for the spawner there however long that computer takes: one that
//     stops answering leaves it waiting on the spawner, and the person here
//     can still answer it in the worker. A link that ends hands its prompts
//     back at once (handBackLinkPrompts, through announceReleasedWorkers), a
//     placement that routes them elsewhere moves them as it moves a local
//     spawner's (routeWorkerPromptsAgain), and a return from that computer
//     because no thread there can hold it hands it back. A return from a
//     computer that predates the reason is offered to it again later
//     (offerPeerPromptAgain).
//
// One binding (remotePromptFor) is the only door for an answer and a
// return.

// Forwarded tool names of the prompts held for a paired computer.
const (
	threadAgentPeerPromptReply  = "agent_prompt_reply"
	threadAgentPeerPromptReturn = "agent_prompt_return"
)

// peerPromptClosed is the code agent_prompt_reply refuses a prompt that is
// no longer open with.
const peerPromptClosed = "thread_prompt_closed"

// remotePromptHold is a prompt routed to a thread on a paired computer:
// the route, the link it was routed through, and when it was registered,
// which orders the link's listing.
type remotePromptHold struct {
	activity                        *ownerActivity
	computerID, computerName, title string
	device, spawnToken              string
	registeredAt                    time.Time
	// offerAt is when agent_rows lists it, zero for at once; returns is how
	// many times an older spawner's computer gave it back
	// (offerPeerPromptAgain).
	offerAt time.Time
	returns int
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
	// Reason is why a return gives the prompt back: peerPromptNoHolder, or
	// empty from a computer that predates it.
	Reason string `json:"reason,omitempty"`
}

// peerPromptNoHolder is a return's reason when no thread on the spawner's
// computer can hold the prompt.
const peerPromptNoHolder = "no_holder"

// How long a prompt an older spawner's computer gave back waits before it
// is offered again: the first wait, doubled with each return, up to the
// most.
const (
	peerPromptOfferFirst = time.Minute
	peerPromptOfferMost  = 30 * time.Minute
)

// peerPromptOfferDelay is the wait before the returns-th offer again.
func peerPromptOfferDelay(returns int) time.Duration {
	wait := peerPromptOfferFirst
	for i := 1; i < returns && wait < peerPromptOfferMost; i++ {
		wait *= 2
	}
	return min(wait, peerPromptOfferMost)
}

// offerPeerPromptAgain answers a return that names no reason, from a
// spawner's computer that predates it, which gives back a prompt its thread
// did not take now as well as one no thread there can hold. While the
// worker's prompts are still that spawn's thread's (promptLinkStands), the
// prompt stays its, routed there and read as waiting on it, and is
// registered again under a new token that agent_rows lists once the wait
// runs out (peerPromptOfferDelay): that computer takes a new token as a new
// prompt, and forgets the old one once it is no longer listed. Any other is
// handed back.
func (a *App) offerPeerPromptAgain(token string) error {
	prompt, found := a.workerPrompts.get(token)
	if !found || prompt.remote == nil || !a.promptLinkStands(prompt) {
		a.handBackWorkerPrompt(token)
		return nil
	}
	offered, ok := a.workerPrompts.reoffer(token, func(returns int) time.Time {
		return time.Now().Add(peerPromptOfferDelay(returns))
	})
	if !ok {
		// Answered, closed or moved meanwhile: that transition is its.
		return nil
	}
	log.Printf("worker prompts: %s of %s is offered to %s again at %s, after %d returns",
		offered.requestID, offered.workerID, offered.remote.computerID, offered.remote.offerAt.Format(time.RFC3339), offered.remote.returns)
	return nil
}

// threadPromptPeerAnswer is agent_prompt_reply's answer: false when the
// answer repeated the one already written.
type threadPromptPeerAnswer struct {
	Accepted bool `json:"accepted"`
}

// errPeerPromptNotYours refuses an answer or a return the binding does not
// admit.
var errPeerPromptNotYours = errorsx.Public(threadtools.CodeRequestNotYours,
	"That token names no prompt this computer routed to your thread through that spawn.", nil)

// holdPeerWorkerPrompt registers a prompt announced routed to a paired
// computer's thread, held for the link that owns the worker. A link that no
// longer routes there, or a failed read of it, hands the prompt back at
// once.
func (a *App) holdPeerWorkerPrompt(route provider.PromptRoute, prompt workerPrompt) {
	link, linked, err := a.store.SpawnOwnerOf(prompt.workerID)
	prompt.agentID = route.ThreadID
	prompt.remote = &remotePromptHold{
		computerID: route.ComputerID, computerName: route.ComputerName, title: route.Title, registeredAt: time.Now(),
	}
	if err == nil && linked {
		prompt.remote.device, prompt.remote.spawnToken = link.DeviceID, link.Token
		prompt.remote.activity = &a.ownerActivity
	}
	registered, replaced := a.workerPrompts.add(prompt, a.workerPromptOpen)
	if replaced != nil {
		a.withdrawWorkerPromptWake(*replaced)
	}
	if err != nil || !linked || link.ComputerID != route.ComputerID || link.ThreadID != route.ThreadID {
		log.Printf("worker prompts: %s of %s is not held for %s: its link is gone (%v)", prompt.requestID, prompt.workerID, route.ComputerID, err)
		a.handBackWorkerPrompt(registered.token)
	}
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

// remotePromptFor is the binding every answer and return passes:
// the prompt under promptToken is held for a paired computer through the
// spawn spawnToken names, that spawn, reached for the calling device and
// its source thread (store.ReachSpawnLink), names the prompt's worker, and
// the worker's prompts are that spawn's thread's now (spawnAsks).
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
	if !found || reach.TargetThreadID != prompt.workerID {
		return workerPrompt{}, errPeerPromptNotYours
	}
	asks, err := a.spawnAsks(prompt.workerID, spawnToken, device)
	if err != nil {
		return workerPrompt{}, err
	}
	if !asks {
		return workerPrompt{}, errPeerPromptNotYours
	}
	return prompt, nil
}

// spawnAsks reports whether threadID's prompts go to the thread on a paired
// computer whose spawn spawnToken names, made from device: the worker is
// owned through that spawn's link, or a run that thread started is open on
// it (store.SpawnOwnerOf).
func (a *App) spawnAsks(threadID, spawnToken, device string) (bool, error) {
	owner, found, err := a.store.SpawnOwnerOf(threadID)
	if err != nil {
		return false, fmt.Errorf("read whom thread %s asks: %w", threadID, err)
	}
	return found && owner.Token == spawnToken && owner.DeviceID == device, nil
}

// peerPromptsOf answers the prompts part of agent_rows. A call that names
// the spawn whose link reaches threadID, from that link's device and source
// thread, is its owner's contact, whatever the thread's placement
// (store.ReachSpawnLink). While the thread's prompts are that spawn's
// thread's (spawnAsks), a calling session that may answer prompts
// (approvals:respond) gets the ones held for it; one that may not gets
// none, and still reads the thread's blocked state from the row. Any other
// call lists none. The prompts a call names as held
// (threadAgentPeerArgs.Held) renew the lease of a computer on an older
// version; this one keeps no lease.
func (a *App) peerPromptsOf(ctx context.Context, owner, source, title string, args threadAgentPeerArgs) ([]ThreadAgentPrompt, error) {
	if args.Spawn == "" {
		return nil, nil
	}
	reach, found, err := a.store.ReachSpawnLink(args.Spawn, owner, source, time.Now().UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("read the link of thread %s: %w", args.ThreadID, err)
	}
	if !found || reach.TargetThreadID != args.ThreadID {
		return nil, nil
	}
	if asks, err := a.spawnAsks(args.ThreadID, args.Spawn, owner); err != nil || !asks {
		return nil, err
	}
	if a.requireScope(ctx, transport.ScopeApprovalsRespond, "") != nil {
		return nil, nil
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

// heldFor is the routed prompts of workerID held for the spawn spawnToken
// names and offered now, in the order they were registered.
func (w *workerPrompts) heldFor(spawnToken, workerID string) []workerPrompt {
	now := time.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	var held []workerPrompt
	for _, prompt := range w.byToken {
		if prompt.remote != nil && prompt.remote.spawnToken == spawnToken && prompt.workerID == workerID && prompt.state == workerPromptRouted &&
			!prompt.remote.offerAt.After(now) {
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
		if errors.Is(err, errWorkerPromptClosed) {
			return ThreadPeerReply{}, errorsx.Public(peerPromptClosed, errWorkerPromptClosed.Error(), nil)
		}
		if err != nil {
			return ThreadPeerReply{}, err
		}
		result = threadPromptPeerAnswer{Accepted: ack.Accepted}
	default:
		if args.Reason == peerPromptNoHolder {
			a.handBackWorkerPrompt(args.Token)
		} else if err := a.offerPeerPromptAgain(args.Token); err != nil {
			return ThreadPeerReply{}, err
		}
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
