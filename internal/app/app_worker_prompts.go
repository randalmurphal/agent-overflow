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
	"agent-overflow/internal/store"
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
// the user's retires it. A prompt its agent could not take (its wake never
// reached the session, or its queue was cleared) is rejected
// (triage.RejectPrompt): it goes where the worker's prompts route now, or
// to the user when that is the same agent. A handoff, an archive or a
// delete reconciles the prompts routed to the thread it changed
// (reroutePromptsFrom, triage.ReroutePrompts): a route that still holds is
// kept, and the others go to the assistant's next thread, or the user.
//
// A registration names one prompt occurrence (triage.PromptRef): a session
// can ask again under a request id it resolved, and a replacement session
// under its predecessor's, and an earlier occurrence's token answers,
// settles and releases nothing of a later one.
//
// The record of a routed prompt lives in memory only: a provider prompt
// lives no longer than the worker's session, and none survives a restart.
// A Codex worker's async questions are its prompts too, and outlive both;
// app_worker_prompts_async.go keeps where each goes and routes them again
// at boot.

// workerPromptSendIDRoot names the wake that carries a worker's prompt; the
// token completes it. It is not a loop or watch wake's root, so the turn it
// starts is not a tick and notifies like any other.
const workerPromptSendIDRoot = "agent-ask:"

// workerAnswerSendIDRoot names the message that carries an agent's answer
// to its worker's async questions; the prompt's token completes it.
const workerAnswerSendIDRoot = "agent-answer:"

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
	// occurrence is the prompt's (provider.ApprovalRequest.Occurrence).
	occurrence provider.PromptOccurrence
	approval   *provider.ApprovalRequest
	question   *provider.UserInputRequest
	// async marks a Codex async question prompt: question holds its
	// unanswered questions, and its route is asyncQuestionRoutes'.
	async bool
	state workerPromptState
	// answer is what the agent sent, so a retry of the same answer is
	// recognized.
	answer   string
	closedAt time.Time
}

func (p *workerPrompt) sendID() string { return workerPromptSendIDRoot + p.token }

func (p *workerPrompt) ref() triage.PromptRef {
	return triage.PromptRef{ThreadID: p.workerID, RequestID: p.requestID, Occurrence: p.occurrence}
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
	// gate guards closed, which shutdown sets before it joins the queue
	// (drainWorkerPrompts): from then on nothing runs on it, so nothing
	// reads SQLite after it closes.
	gate   sync.Mutex
	closed bool
	// async is where each open async question prompt goes.
	async asyncQuestionRoutes
}

// run queues job on the prompts' serial queue, unless shutdown closed it:
// a prompt it would deliver or settle dies with the process, and the boot
// routes the async questions again.
func (w *workerPrompts) run(job func()) {
	w.gate.Lock()
	defer w.gate.Unlock()
	if !w.closed {
		w.queue.Go(job)
	}
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
// its routed registration. A change to a thread's async questions
// reconciles its async question prompts, and so does every thread frame but
// a patch: a thread made, copied, moved here or forked, an archive or an
// unarchive, a delete, a placement, a handoff; and so does a transfer that
// moves a thread away.
func (a *App) tapWorkerPrompt(name eventchan.Channel, data any) {
	if event, ok := data.(triage.ThreadUpdateEvent); ok {
		if event.Action != triage.ThreadActionPatch {
			id := event.ID
			if event.Thread != nil {
				id = event.Thread.ID
			}
			a.syncAsyncQuestionPrompts(id)
		}
		return
	}
	switch name {
	case eventchan.ProviderAsyncQuestionsChanged:
		if event, ok := data.(map[string]string); ok {
			a.syncAsyncQuestionPrompts(event["threadId"])
		}
		return
	case eventchan.UserMessageReverted:
		if event, ok := data.(UserMessageRevertedEvent); ok {
			a.syncAsyncQuestionPrompts(event.ThreadID)
		}
		return
	case eventchan.ThreadTransfer:
		// A move takes the thread from this computer once it commits, with
		// no thread frame here.
		if row, ok := data.(store.ThreadTransfer); ok && row.Direction == "outgoing" && row.Kind == "move" {
			a.syncAsyncQuestionPrompts(row.ThreadID)
		}
		return
	}
	var prompt workerPrompt
	var route *provider.PromptRoute
	var action string
	switch name {
	case eventchan.ProviderApproval:
		event, ok := data.(provider.ApprovalEvent)
		if !ok {
			return
		}
		action, prompt = event.Action, workerPrompt{workerID: event.ThreadID, requestID: event.RequestID, occurrence: event.Occurrence}
		if request := event.Request; request != nil {
			copied := *request
			route, prompt.approval = copied.RoutedTo, &copied
			prompt.workerID, prompt.requestID, prompt.occurrence = copied.ThreadID, copied.RequestID, copied.Occurrence
		}
	case eventchan.ProviderUserInput:
		event, ok := data.(provider.UserInputEvent)
		if !ok {
			return
		}
		action, prompt = event.Action, workerPrompt{workerID: event.ThreadID, requestID: event.RequestID, occurrence: event.Occurrence}
		if request := event.Request; request != nil {
			copied := *request
			route, prompt.question = copied.RoutedTo, &copied
			prompt.workerID, prompt.requestID, prompt.occurrence = copied.ThreadID, copied.RequestID, copied.Occurrence
		}
	default:
		return
	}
	switch {
	case action == "request" && route != nil:
		to := *route
		a.workerPrompts.run(func() { a.deliverWorkerPrompt(to, prompt) })
	case action == "request" && (prompt.approval != nil || prompt.question != nil), action == "resolve":
		a.workerPrompts.run(func() { a.settleWorkerPrompt(prompt.workerID, prompt.requestID, prompt.occurrence) })
	}
}

// deliverWorkerPrompt registers a prompt announced routed to route and
// queues its wake there. An announcement a later one overtook (the prompt
// was answered, or moved again) is left to that one; a prompt the agent
// cannot take moves on (handBackWorkerPrompt).
func (a *App) deliverWorkerPrompt(route provider.PromptRoute, prompt workerPrompt) {
	if current, open := a.workerPromptRoute(prompt); !open || current == nil || current.ThreadID != route.ThreadID {
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
	message := workerPromptMessage(worker.Title, prompt.workerID, &registered)
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
		threadID:  route.ThreadID,
		recipient: func(threadID string) { moved = threadID != route.ThreadID },
		sendID:    registered.sendID(),
		prepare: func() (usermessage.AgentMessage, bool, error) {
			return message, !moved && live(), nil
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

// workerPromptRoute is where a registration's prompt is routed now, nil
// when it is the user's, and whether it is still open: in the worker's
// session that asked it, or for an async question, in the store.
func (a *App) workerPromptRoute(prompt workerPrompt) (*provider.PromptRoute, bool) {
	if prompt.async {
		return a.asyncQuestionRoute(prompt.ref())
	}
	if a.triage == nil {
		return nil, false
	}
	return a.triage.PromptRoute(prompt.ref())
}

// workerPromptOpen reports whether a registration's prompt is still open.
func (a *App) workerPromptOpen(prompt workerPrompt) bool {
	_, open := a.workerPromptRoute(prompt)
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
	route, open := a.workerPromptRoute(prompt)
	return open && route != nil && route.ThreadID == prompt.agentID
}

// settleWorkerPrompt records that the prompt occurrence names was resolved,
// or was announced as the user's. Unless the agent's own answer resolved
// it, the user answered it in the worker, the worker stopped waiting or the
// prompt went to the user, and a wake still queued is withdrawn.
func (a *App) settleWorkerPrompt(workerID, requestID string, occurrence provider.PromptOccurrence) {
	prompt, settled := a.workerPrompts.settle(workerID, requestID, occurrence)
	if !settled {
		return
	}
	a.withdrawWorkerPromptWake(prompt)
}

// settleEndedWorkerPrompts closes the prompts a worker's session took with
// it when it ended, which no resolution settles, and withdraws their wakes
// still queued for the agent, so the agent is not asked what nobody waits
// on. Its async questions are not its session's and stay open. It runs on
// the prompts' serial queue, after any delivery announced before the end.
func (a *App) settleEndedWorkerPrompts(workerID string) {
	a.workerPrompts.run(func() {
		for _, prompt := range a.workerPrompts.settleWorker(workerID) {
			a.withdrawWorkerPromptWake(prompt)
		}
	})
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
	if !settled {
		return
	}
	moved := false
	switch {
	case prompt.async:
		moved = a.rerouteAsyncQuestions(prompt.agentID, []triage.PromptRef{prompt.ref()}, true) > 0
	case a.triage != nil:
		moved = a.triage.RejectPrompt(prompt.agentID, prompt.ref())
	}
	if moved {
		log.Printf("worker prompts: %s of %s moved on: %s could not take it", prompt.requestID, prompt.workerID, prompt.agentID)
	}
}

// reroutePromptsFrom reconciles every open prompt routed to threadID, which
// a handoff, an archive or a delete changed, whatever became of its wake: a
// prompt the worker's prompts still route to threadID stays, as one does
// when an unarchive reopened it before a delayed archive cleanup ran, and
// the others go to the assistant's next thread, or to the user when the
// worker's prompts route to nobody now.
func (a *App) reroutePromptsFrom(threadID string) {
	moved := a.rerouteAsyncQuestions(threadID, nil, false)
	if a.triage != nil {
		moved += a.triage.ReroutePrompts(threadID)
	}
	if moved > 0 {
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
	// The answer goes to the session that asked, or nowhere; an async
	// question's goes to the worker's queue, as the person's answers do.
	switch {
	case prompt.async:
		err = a.answerAsyncWorkerPrompt(ctx, prompt, *question)
	case approval != nil:
		approval.Occurrence = prompt.occurrence
		err = a.RespondToApproval(ctx, prompt.workerID, *approval)
	default:
		question.Occurrence = prompt.occurrence
		err = a.RespondToUserInput(ctx, prompt.workerID, *question)
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
		// An async question is answered by a message, which says
		// something for each part.
		if prompt.async && strings.TrimSpace(values[i]) == "" {
			return nil, nil, errorsx.Public(threadtools.CodeInvalidRequest,
				"Give an answer for every part of that question.", nil)
		}
		answers[q.ID] = provider.UserInputAnswer{values[i]}
	}
	return nil, &provider.UserInputResponse{RequestID: prompt.requestID, Decision: "accept", Answers: answers}, nil
}

// workerPromptAnswerKey is what makes two replies the same answer.
func workerPromptAnswerKey(call threadtools.ReplyCall) string {
	return strings.Join(append([]string{call.Decision, call.Text}, call.Answers...), "\x00")
}

// workerPromptMessage is the wake that puts a worker's prompt to its agent:
// what the worker asks, quoted as data, and how to answer, headed in its
// row by the prompt's first line.
func workerPromptMessage(workerTitle, workerID string, prompt *workerPrompt) usermessage.AgentMessage {
	kind := usermessage.ReportQuestion
	if prompt.approval != nil {
		kind = usermessage.ReportApproval
	}
	notice := fmt.Sprintf("Thread %s (%s), which you spawned, is waiting on your answer. %s",
		untrustedtext.Quote(workerTitle, 0), workerID, agentWakeDataNotice)
	var out strings.Builder
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
			if q.Header == "" {
				fmt.Fprintf(&data, "%d. %s\n", i+1, q.Question)
			} else {
				fmt.Fprintf(&data, "%d. %s: %s\n", i+1, q.Header, q.Question)
			}
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
	return usermessage.Report(kind, usermessage.Reporter{Label: workerTitle, ThreadID: workerID},
		notice, out.String(), firstDataLine(data.String()))
}

// add registers a prompt under a fresh token as the prompt's current
// registration, and returns the registration it replaced, closed if it was
// routed, whose wake its caller withdraws. It first forgets prompts closed
// past their retention and closes the routed ones whose prompt is gone, so
// the registry holds no more than the open prompts and a day of closed
// ones.
func (w *workerPrompts) add(prompt workerPrompt, open func(workerPrompt) bool) (registered workerPrompt, replaced *workerPrompt) {
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
		if existing.state == workerPromptRouted && !open(*existing) {
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
// resolution in the worker of occurrence upTo. Occurrences only grow, so a
// registration of a later prompt under the same request id is not upTo's
// to close, and zero, a resolution that named no prompt, closes nothing.
// Settled is false when it is not routed, including when the agent is
// answering or answered it: that answer is what resolved it.
func (w *workerPrompts) settle(workerID, requestID string, upTo provider.PromptOccurrence) (workerPrompt, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	prompt := w.byToken[w.current[workerPromptKey{workerID: workerID, requestID: requestID}]]
	if prompt == nil || prompt.occurrence > upTo || prompt.state != workerPromptRouted {
		return workerPrompt{}, false
	}
	prompt.state, prompt.closedAt = workerPromptSettled, time.Now()
	return *prompt, true
}

// settleWorker closes every routed registration of the prompts workerID's
// session asked, which leaves its async questions.
func (w *workerPrompts) settleWorker(workerID string) []workerPrompt {
	w.mu.Lock()
	defer w.mu.Unlock()
	var closed []workerPrompt
	now := time.Now()
	for _, prompt := range w.byToken {
		if prompt.workerID != workerID || prompt.async || prompt.state != workerPromptRouted {
			continue
		}
		prompt.state, prompt.closedAt = workerPromptSettled, now
		closed = append(closed, *prompt)
	}
	return closed
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

// drainWorkerPrompts waits for queued deliveries, settlements and
// reconciles to finish.
func (a *App) drainWorkerPrompts(ctx context.Context, timeout time.Duration) error {
	return drainSerialQueue(ctx, &a.workerPrompts.queue, timeout)
}

// closeWorkerPrompts is shutdown's join of the prompts' queue, before
// SQLite closes: nothing queued from here on runs, and what was queued
// finishes.
func (a *App) closeWorkerPrompts(ctx context.Context, timeout time.Duration) error {
	a.workerPrompts.gate.Lock()
	a.workerPrompts.closed = true
	a.workerPrompts.gate.Unlock()
	return a.drainWorkerPrompts(ctx, timeout)
}
