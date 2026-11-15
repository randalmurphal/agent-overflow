package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/serialqueue"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/untrustedtext"
	"agent-overflow/internal/usermessage"
)

// A worker's question goes to the thread that spawned it (standing agents,
// ruling 19), whoever that is. A thread another thread on this computer
// spawned does not stop at the user while its spawner is not archived:
// triage stamps each of its approval and user-input prompts with the
// spawner (routeWorkerPrompt), the emit tap queues the prompt to the
// spawner as a badged wake, and the spawner's agent answers it with
// thread_reply on the token the wake carries. The prompt stays open in the
// worker, so the user can still answer it there, which settles the wake.
//
// Triage's RoutedTo is the one record of where an open prompt goes, and the
// registry follows its announcements: a prompt announced routed is
// registered under a fresh token and its wake queued, which retires the
// prompt's earlier registration and withdraws that wake; one announced as
// the user's retires it. Every change of route is triage.ReroutePrompts: a
// prompt its agent could not take (its wake never reached the session, or
// its queue was cleared), and every open prompt routed to a thread a
// handoff, an archive or a delete took away (reroutePromptsFrom), goes
// where the worker's prompts route now: the assistant's next thread, or the
// user.
//
// A registration names one prompt of one session of the worker
// (triage.PromptRef): a later session can ask under the same request id,
// and an earlier session's token answers nothing of it.
//
// The record of a routed prompt lives in memory only: a provider prompt
// lives no longer than the worker's session, and none survives a restart.

// workerPromptSendIDRoot names the wake that carries a worker's prompt; the
// token completes it. It is not a loop or watch wake's root, so the turn it
// starts is not a tick and notifies like any other.
const workerPromptSendIDRoot = "agent-ask:"

// workerPromptRetention is how long an answered or settled prompt is kept so
// a late or repeated thread_reply is told what happened to it.
const workerPromptRetention = 24 * time.Hour

type workerPromptState int

const (
	// workerPromptRouted: the wake is queued or delivered and the prompt is
	// open.
	workerPromptRouted workerPromptState = iota
	// workerPromptAnswering: the agent's answer is being written to the
	// worker's provider.
	workerPromptAnswering
	// workerPromptAnswered: the agent answered it.
	workerPromptAnswered
	// workerPromptSettled: the user answered it in the worker, the worker
	// stopped waiting, or it went to the user or to another thread.
	workerPromptSettled
)

type workerPrompt struct {
	token     string
	workerID  string
	agentID   string
	requestID string
	// sessionToken is the worker's session that asked.
	sessionToken string
	approval     *provider.ApprovalRequest
	question     *provider.UserInputRequest
	state        workerPromptState
	// answer is what the agent sent, so a retry of the same answer is
	// recognized.
	answer   string
	closedAt time.Time
}

func (p *workerPrompt) sendID() string { return workerPromptSendIDRoot + p.token }

func (p *workerPrompt) ref() triage.PromptRef {
	return triage.PromptRef{ThreadID: p.workerID, RequestID: p.requestID, SessionToken: p.sessionToken}
}

// workerPromptKey names one prompt of one worker.
type workerPromptKey struct{ workerID, requestID string }

// workerPrompts is the registry of routed prompts and the ordered queue
// their deliveries and settlements run on. The queue keeps a prompt's
// resolution behind its delivery, and runs both off the provider event
// worker: delivering takes the agent's thread lock and can start its
// session.
type workerPrompts struct {
	mu      sync.Mutex
	byToken map[string]*workerPrompt
	// current is the token of each prompt's latest registration. A prompt
	// that moved to another thread, or that a later session asked again, is
	// registered again under a new token; the earlier one is kept only to
	// answer a late reply.
	current map[workerPromptKey]string
	queue   serialqueue.Queue
}

// routeWorkerPrompt is triage's prompt router: the thread that spawned
// threadID (store.SpawnerOf), or nil when the prompt is the user's.
func (a *App) routeWorkerPrompt(threadID string) *provider.PromptRoute {
	agentID, title, found, err := a.store.SpawnerOf(threadID)
	if err != nil {
		log.Printf("worker prompts: %v", err)
		return nil
	}
	if !found {
		return nil
	}
	return &provider.PromptRoute{ThreadID: agentID, Title: title}
}

// tapWorkerPrompt is the emit tap: a prompt announced routed is delivered
// to its agent, and a prompt announced as the user's or resolved settles
// its routed registration.
func (a *App) tapWorkerPrompt(name eventchan.Channel, data any) {
	var prompt workerPrompt
	var route *provider.PromptRoute
	var action string
	switch name {
	case eventchan.ProviderApproval:
		event, ok := data.(provider.ApprovalEvent)
		if !ok {
			return
		}
		action, prompt = event.Action, workerPrompt{workerID: event.ThreadID, requestID: event.RequestID}
		if request := event.Request; request != nil {
			copied := *request
			route, prompt.approval = copied.RoutedTo, &copied
			prompt.workerID, prompt.requestID, prompt.sessionToken = copied.ThreadID, copied.RequestID, copied.SessionToken
		}
	case eventchan.ProviderUserInput:
		event, ok := data.(provider.UserInputEvent)
		if !ok {
			return
		}
		action, prompt = event.Action, workerPrompt{workerID: event.ThreadID, requestID: event.RequestID}
		if request := event.Request; request != nil {
			copied := *request
			route, prompt.question = copied.RoutedTo, &copied
			prompt.workerID, prompt.requestID, prompt.sessionToken = copied.ThreadID, copied.RequestID, copied.SessionToken
		}
	default:
		return
	}
	switch {
	case action == "request" && route != nil:
		to := *route
		a.workerPrompts.queue.Go(func() { a.deliverWorkerPrompt(to, prompt) })
	case action == "request" && (prompt.approval != nil || prompt.question != nil), action == "resolve":
		a.workerPrompts.queue.Go(func() { a.settleWorkerPrompt(prompt.workerID, prompt.requestID) })
	}
}

// deliverWorkerPrompt registers a prompt announced routed to route and
// queues its wake there. An announcement a later one overtook (the prompt
// was answered, or moved again) is left to that one; a prompt the agent
// cannot take moves on (handBackWorkerPrompt).
func (a *App) deliverWorkerPrompt(route provider.PromptRoute, prompt workerPrompt) {
	if a.triage == nil {
		return
	}
	if current, open := a.triage.PromptRoute(prompt.ref()); !open || current == nil || current.ThreadID != route.ThreadID {
		return
	}
	prompt.agentID = route.ThreadID
	registered, replaced := a.workerPrompts.add(prompt, a.workerPromptOpen)
	if replaced != nil {
		a.withdrawWorkerPromptWake(*replaced)
	}
	worker, err := a.store.GetThread(prompt.workerID)
	if err != nil {
		log.Printf("worker prompts: read worker %s: %v", prompt.workerID, err)
		a.handBackWorkerPrompt(registered.token)
		return
	}
	kind := usermessage.OriginWakeKindQuestion
	if prompt.approval != nil {
		kind = usermessage.OriginWakeKindApproval
	}
	body := renderWorkerPromptWake(worker.Title, prompt.workerID, &registered)
	live := func() bool { return a.workerPromptLive(registered.token) }
	// A thread New Thread, Same Assistant superseded after triage routed
	// the prompt there takes nothing, so a wake is queued only where triage
	// says the prompt is: the handoff moves the prompt on
	// (reroutePromptsFrom), and that announcement delivers it.
	moved := false
	// The user may have answered before the prompt is queued and again
	// before a session is started for it. An archive of the agent is the
	// queue's and the start's to hold (queueAgentNotice).
	err = a.queueAgentNotice(agentNotice{
		threadID:   route.ThreadID,
		recipient:  func(threadID string) { moved = threadID != route.ThreadID },
		sendID:     registered.sendID(),
		origin:     usermessage.OriginAgentWake,
		originWake: &usermessage.OriginWake{Kind: kind, Label: worker.Title, ThreadID: prompt.workerID},
		prepare: func() (string, bool, error) {
			if moved || !live() {
				return "", false, nil
			}
			return body, true, nil
		},
		keep:      func() (bool, error) { return live(), nil },
		withdrawn: func() { a.handBackWorkerPrompt(registered.token) },
		startFailed: func(err error) string {
			return fmt.Sprintf("Thread %q is waiting on your answer and its prompt is queued here, but this thread's agent could not start: %v", worker.Title, err)
		},
	})
	if err != nil {
		log.Printf("worker prompts: deliver %s of %s to %s: %v", prompt.requestID, prompt.workerID, route.ThreadID, err)
		a.handBackWorkerPrompt(registered.token)
	}
}

// workerPromptOpen reports whether the prompt ref names is still open in
// the worker's session that asked it.
func (a *App) workerPromptOpen(ref triage.PromptRef) bool {
	_, open := a.triage.PromptRoute(ref)
	return open
}

// workerPromptLive reports whether the registration under token is routed
// and its prompt is open and still routed to its agent.
func (a *App) workerPromptLive(token string) bool {
	prompt, found := a.workerPrompts.get(token)
	return found && prompt.state == workerPromptRouted && a.workerPromptOpenTo(prompt)
}

// workerPromptOpenTo reports whether a registration's prompt is open and
// still routed to the registration's agent.
func (a *App) workerPromptOpenTo(prompt workerPrompt) bool {
	if a.triage == nil {
		return false
	}
	route, open := a.triage.PromptRoute(prompt.ref())
	return open && route != nil && route.ThreadID == prompt.agentID
}

// settleWorkerPrompt records that a routed prompt was resolved, or was
// announced as the user's. Unless the agent's own answer resolved it, the
// user answered it in the worker, the worker stopped waiting or the prompt
// went to the user, and a wake still queued is withdrawn.
func (a *App) settleWorkerPrompt(workerID, requestID string) {
	prompt, settled := a.workerPrompts.settle(workerID, requestID)
	if !settled {
		return
	}
	a.withdrawWorkerPromptWake(prompt)
}

// withdrawWorkerPromptWake takes a closed registration's wake out of its
// agent's queue, when it is still there.
func (a *App) withdrawWorkerPromptWake(prompt workerPrompt) {
	if _, err := a.removeQueuedItem(a.lifeCtx(), prompt.agentID, prompt.sendID()); err != nil {
		log.Printf("worker prompts: withdraw the wake for %s of %s: %v", prompt.requestID, prompt.workerID, err)
	}
}

// handBackWorkerPrompt moves on the prompt registered under token, which its
// agent could not take: to where the worker's prompts route now, the
// assistant's current thread when New Thread, Same Assistant superseded the
// agent's, and otherwise the user. Only that registration is settled: one
// that is already closed, or that a later registration of the same prompt
// replaced, moves nothing.
func (a *App) handBackWorkerPrompt(token string) {
	prompt, settled := a.workerPrompts.settleToken(token)
	if !settled || a.triage == nil {
		return
	}
	if a.triage.ReroutePrompts(prompt.agentID, prompt.ref()) > 0 {
		log.Printf("worker prompts: %s of %s moved on: %s could not take it", prompt.requestID, prompt.workerID, prompt.agentID)
	}
}

// reroutePromptsFrom moves every open prompt routed to threadID, which a
// handoff, an archive or a delete took away, whatever became of its wake:
// to the assistant's next thread, or to the user when the worker's prompts
// route to nobody now.
func (a *App) reroutePromptsFrom(threadID string) {
	if a.triage == nil {
		return
	}
	if moved := a.triage.ReroutePrompts(threadID); moved > 0 {
		log.Printf("worker prompts: %d prompts routed to %s moved on", moved, threadID)
	}
}

// handBackWorkerPromptWake hands back the prompt a dropped wake carried.
// False when sendID is not a worker prompt's wake.
func (a *App) handBackWorkerPromptWake(sendID string) bool {
	token, ok := strings.CutPrefix(sendID, workerPromptSendIDRoot)
	if !ok {
		return false
	}
	a.handBackWorkerPrompt(token)
	return true
}

// answerWorkerPrompt is thread_reply on a worker prompt's token. Handled is
// false for any other token.
func (a *App) answerWorkerPrompt(ctx context.Context, caller threadtools.Caller, call threadtools.ReplyCall) (ack threadtools.ReplyAck, handled bool, err error) {
	prompt, found := a.workerPrompts.get(call.Token)
	if !found {
		return threadtools.ReplyAck{}, false, nil
	}
	ack = threadtools.ReplyAck{Token: call.Token, State: threadtools.RequestReplied, SourceThreadID: prompt.workerID, Prompt: true}
	if prompt.agentID != caller.ThreadID {
		return ack, true, errorsx.Public(threadtools.CodeRequestNotYours,
			"That token belongs to a prompt sent to another thread. Answer only the prompts that woke this thread.", nil)
	}
	answer := workerPromptAnswerKey(call)
	approval, question, err := workerPromptResponse(&prompt, call)
	if err != nil {
		return ack, true, err
	}
	if err := a.workerPrompts.beginAnswer(prompt.token, answer); err != nil {
		if errors.Is(err, errWorkerPromptRepeated) {
			return ack, true, nil
		}
		return ack, true, err
	}
	// A worker whose session ended took its prompts with it, without a
	// resolution to settle them by, and a prompt that moved is its new
	// recipient's.
	if !a.workerPromptOpenTo(prompt) {
		a.workerPrompts.endAnswer(prompt.token, workerPromptSettled)
		return ack, true, errWorkerPromptClosed
	}
	// The answer goes to the session that asked, or nowhere.
	if approval != nil {
		err = a.respondToApproval(ctx, prompt.workerID, prompt.sessionToken, *approval)
	} else {
		err = a.respondToUserInput(ctx, prompt.workerID, prompt.sessionToken, *question)
	}
	if err != nil {
		if errors.Is(err, transport.ErrAlreadyHandled) {
			a.workerPrompts.endAnswer(prompt.token, workerPromptSettled)
			return ack, true, errWorkerPromptClosed
		}
		a.workerPrompts.endAnswer(prompt.token, workerPromptRouted)
		log.Printf("worker prompts: answer %s of %s: %v", prompt.requestID, prompt.workerID, err)
		if errors.Is(err, provider.ErrApprovalDecisionUnavailable) {
			return ack, true, errorsx.Public(threadtools.CodeInvalidRequest,
				"The worker's provider does not offer that decision for this approval. Deny it, or ask the user.", nil)
		}
		return ack, true, errorsx.Public(threadtools.CodeInvalidRequest,
			"The answer did not reach the worker, and its prompt is still open. Try again, or ask the user.", nil)
	}
	a.workerPrompts.endAnswer(prompt.token, workerPromptAnswered)
	ack.Accepted = true
	return ack, true, nil
}

var (
	errWorkerPromptClosed = errorsx.Public(threadtools.CodeInvalidRequest,
		"That prompt is no longer open: the user answered it in the worker, or the worker stopped waiting. Check the worker with thread_status before acting on it.", nil)
	errWorkerPromptAnswering = errorsx.Public(threadtools.CodeInvalidRequest,
		"An answer to that prompt is being delivered now.", nil)
	errWorkerPromptAnswered = errorsx.Public(threadtools.CodeInvalidRequest,
		"You already answered that prompt differently. Reply once per token.", nil)
	errWorkerPromptRepeated = errors.New("worker prompt: the same answer again")
)

// workerPromptResponse turns the agent's reply into the provider response
// the prompt takes.
func workerPromptResponse(prompt *workerPrompt, call threadtools.ReplyCall) (*provider.ApprovalResponse, *provider.UserInputResponse, error) {
	if prompt.approval != nil {
		if len(call.Answers) > 0 {
			return nil, nil, errorsx.Public(threadtools.CodeInvalidRequest,
				"That is an approval, not a question: pass decision allow or deny, with your reason in text.", nil)
		}
		response := provider.ApprovalResponse{RequestID: prompt.requestID}
		switch call.Decision {
		case threadtools.DecisionAllow:
			switch prompt.approval.Kind {
			case "mcp-elicitation":
				return nil, nil, errorsx.Public(threadtools.CodeInvalidRequest,
					"That approval is a form only the user can fill in. Deny it, or ask the user to answer it in the worker.", nil)
			case "permission":
				response.Permissions = prompt.approval.Permissions
				response.Scope = "turn"
			}
			response.Decision = "accept"
		case threadtools.DecisionDeny:
			response.Decision = "decline"
			if prompt.approval.Kind == "mcp-elicitation" {
				response.Elicitation = &provider.ElicitationResolution{Action: "decline"}
			}
		default:
			return nil, nil, errorsx.Public(threadtools.CodeInvalidRequest,
				"That is an approval: pass decision allow or deny, with your reason in text.", nil)
		}
		return &response, nil, nil
	}
	if call.Decision != "" {
		return nil, nil, errorsx.Public(threadtools.CodeInvalidRequest,
			"That is a question, not an approval: put your answer in text, or one answer per part in answers.", nil)
	}
	questions := prompt.question.Questions
	values := call.Answers
	switch {
	case len(values) > 0 && len(values) != len(questions):
		return nil, nil, errorsx.Public(threadtools.CodeInvalidRequest,
			fmt.Sprintf("That question has %d parts and answers holds %d. Give one answer per part, in order.", len(questions), len(values)), nil)
	case len(values) == 0 && len(questions) != 1:
		return nil, nil, errorsx.Public(threadtools.CodeInvalidRequest,
			fmt.Sprintf("That question has %d parts. Pass answers, one per part, in order.", len(questions)), nil)
	case len(values) == 0:
		values = []string{call.Text}
	}
	answers := make(map[string]provider.UserInputAnswer, len(questions))
	for i, q := range questions {
		answers[q.ID] = provider.UserInputAnswer{values[i]}
	}
	return nil, &provider.UserInputResponse{RequestID: prompt.requestID, Decision: "accept", Answers: answers}, nil
}

// workerPromptAnswerKey is what makes two replies the same answer.
func workerPromptAnswerKey(call threadtools.ReplyCall) string {
	return strings.Join(append([]string{call.Decision, call.Text}, call.Answers...), "\x00")
}

// renderWorkerPromptWake renders the wake that puts a worker's prompt to
// its agent: what the worker asks, quoted as data, and how to answer.
func renderWorkerPromptWake(workerTitle, workerID string, prompt *workerPrompt) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Thread %s (%s), which you spawned, is waiting on your answer. %s\n",
		untrustedtext.Quote(workerTitle, 0), workerID, agentWakeDataNotice)
	var data strings.Builder
	if prompt.approval != nil {
		request := prompt.approval
		fmt.Fprintf(&data, "It asks for approval to use %s.\n", request.ToolName)
		if request.Description != "" {
			fmt.Fprintf(&data, "%s\n", request.Description)
		}
		if len(request.Input) > 0 {
			fmt.Fprintf(&data, "input: %s\n", request.Input)
		}
	} else {
		for i, q := range prompt.question.Questions {
			fmt.Fprintf(&data, "%d. %s: %s\n", i+1, q.Header, q.Question)
			for _, option := range q.Options {
				if option.Description != "" {
					fmt.Fprintf(&data, "   option: %s (%s)\n", option.Label, option.Description)
				} else {
					fmt.Fprintf(&data, "   option: %s\n", option.Label)
				}
			}
			if q.MultiSelect {
				data.WriteString("   (several options may be chosen; join them with commas)\n")
			}
		}
	}
	writeQuotedLines(&out, newCappedText(data.String(), agentWakeOutputCap))
	if prompt.approval != nil {
		fmt.Fprintf(&out, "Answer with thread_reply, token %s, once: decision allow or deny, and your reason in text.", prompt.token)
	} else if len(prompt.question.Questions) == 1 {
		fmt.Fprintf(&out, "Answer with thread_reply, token %s, once: your answer in text.", prompt.token)
	} else {
		fmt.Fprintf(&out, "Answer with thread_reply, token %s, once: one answer per part in answers, in order, and a summary in text.", prompt.token)
	}
	out.WriteString(" If it is not yours to decide, ask the user yourself, as one question with this context. The user can also answer in that thread, which settles this and refuses your reply.")
	return out.String()
}

// add registers a prompt under a fresh token as the prompt's current
// registration, and returns the registration it replaced, closed if it was
// routed, whose wake its caller withdraws. It first forgets prompts closed
// past their retention and closes the routed ones whose prompt is gone, so
// the registry holds no more than the open prompts and a day of closed
// ones.
func (w *workerPrompts) add(prompt workerPrompt, open func(triage.PromptRef) bool) (registered workerPrompt, replaced *workerPrompt) {
	prompt.token = newThreadRequestToken()
	now := time.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.byToken == nil {
		w.byToken = make(map[string]*workerPrompt)
	}
	if w.current == nil {
		w.current = make(map[workerPromptKey]string)
	}
	if previous := w.byToken[w.current[prompt.key()]]; previous != nil {
		if previous.state == workerPromptRouted {
			previous.state, previous.closedAt = workerPromptSettled, now
		}
		retired := *previous
		replaced = &retired
	}
	for token, existing := range w.byToken {
		if existing.state == workerPromptRouted && !open(existing.ref()) {
			existing.state, existing.closedAt = workerPromptSettled, now
		}
		if !existing.closedAt.IsZero() && now.Sub(existing.closedAt) > workerPromptRetention {
			delete(w.byToken, token)
			if key := existing.key(); w.current[key] == token {
				delete(w.current, key)
			}
		}
	}
	stored := prompt
	w.byToken[stored.token] = &stored
	w.current[stored.key()] = stored.token
	return stored, replaced
}

func (p *workerPrompt) key() workerPromptKey {
	return workerPromptKey{workerID: p.workerID, requestID: p.requestID}
}

// get is the registration under token as it stands.
func (w *workerPrompts) get(token string) (workerPrompt, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	prompt := w.byToken[token]
	if prompt == nil {
		return workerPrompt{}, false
	}
	return *prompt, true
}

// settle closes the current registration of a worker's prompt, for a
// resolution in the worker. Settled is false when it is not routed,
// including when the agent is answering or answered it: that answer is what
// resolved it.
func (w *workerPrompts) settle(workerID, requestID string) (workerPrompt, bool) {
	w.mu.Lock()
	token := w.current[workerPromptKey{workerID: workerID, requestID: requestID}]
	w.mu.Unlock()
	if token == "" {
		return workerPrompt{}, false
	}
	return w.settleToken(token)
}

// settleToken closes the registration under token when it is routed.
func (w *workerPrompts) settleToken(token string) (workerPrompt, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	prompt := w.byToken[token]
	if prompt == nil || prompt.state != workerPromptRouted {
		return workerPrompt{}, false
	}
	prompt.state, prompt.closedAt = workerPromptSettled, time.Now()
	return *prompt, true
}

// beginAnswer claims a routed prompt for the agent's answer.
func (w *workerPrompts) beginAnswer(token, answer string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	prompt := w.byToken[token]
	if prompt == nil {
		return errWorkerPromptClosed
	}
	switch prompt.state {
	case workerPromptRouted:
		prompt.state, prompt.answer = workerPromptAnswering, answer
		return nil
	case workerPromptAnswering:
		return errWorkerPromptAnswering
	case workerPromptAnswered:
		if prompt.answer == answer {
			return errWorkerPromptRepeated
		}
		return errWorkerPromptAnswered
	default:
		return errWorkerPromptClosed
	}
}

// endAnswer records how the agent's answer ended. An answer that did not
// reach the worker leaves the prompt routed, unless a later registration
// replaced this one meanwhile: then the prompt is that one's.
func (w *workerPrompts) endAnswer(token string, state workerPromptState) {
	w.mu.Lock()
	defer w.mu.Unlock()
	prompt := w.byToken[token]
	if prompt == nil {
		return
	}
	if state == workerPromptRouted && w.current[prompt.key()] != token {
		state = workerPromptSettled
	}
	prompt.state = state
	if state == workerPromptRouted {
		prompt.answer = ""
		return
	}
	prompt.closedAt = time.Now()
}

// drainWorkerPrompts lets queued deliveries and settlements finish before
// SQLite closes.
func (a *App) drainWorkerPrompts(ctx context.Context, timeout time.Duration) error {
	return drainSerialQueue(ctx, &a.workerPrompts.queue, timeout)
}
