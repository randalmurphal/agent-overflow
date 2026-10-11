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
	"agent-overflow/internal/untrustedtext"
	"agent-overflow/internal/usermessage"
)

// A worker's question goes to its agent (standing agents, ruling 19). A
// thread a standing agent spawned on this computer never stops at the user:
// triage stamps each of its approval and user-input prompts with the agent
// (routeWorkerPrompt), the emit tap queues the prompt to the agent as a
// badged wake, and the agent answers it with thread_reply on the token the
// wake carries. The prompt stays open in the worker, so the user can still
// answer it there, which settles the wake. A prompt the agent cannot take,
// because the wake never reached its session or its queue was cleared, is
// handed back to the user.
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

// Badge kinds of a worker's prompt wake.
const (
	workerPromptKindQuestion = "question"
	workerPromptKindApproval = "approval"
)

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
	// stopped waiting, or it went back to the user.
	workerPromptSettled
)

type workerPrompt struct {
	token     string
	workerID  string
	agentID   string
	requestID string
	approval  *provider.ApprovalRequest
	question  *provider.UserInputRequest
	state     workerPromptState
	// answer is what the agent sent, so a retry of the same answer is
	// recognized.
	answer   string
	closedAt time.Time
}

func (p *workerPrompt) sendID() string { return workerPromptSendIDRoot + p.token }

// workerPrompts is the registry of routed prompts and the ordered queue
// their deliveries and settlements run on. The queue keeps a prompt's
// resolution behind its delivery, and runs both off the provider event
// worker: delivering takes the agent's thread lock and can start its
// session.
type workerPrompts struct {
	mu      sync.Mutex
	byToken map[string]*workerPrompt
	queue   serialqueue.Queue
}

// routeWorkerPrompt is triage's prompt router: the standing agent that
// spawned threadID, or nil when the prompt is the user's.
func (a *App) routeWorkerPrompt(threadID string) *provider.PromptRoute {
	agentID, title, found, err := a.store.StandingAgentSpawnerOf(threadID)
	if err != nil {
		log.Printf("worker prompts: %v", err)
		return nil
	}
	if !found {
		return nil
	}
	return &provider.PromptRoute{ThreadID: agentID, Title: title}
}

// tapWorkerPrompt is the emit tap: a routed prompt is delivered to its
// agent, and any resolution settles a routed prompt it names.
func (a *App) tapWorkerPrompt(name eventchan.Channel, data any) {
	switch name {
	case eventchan.ProviderApproval:
		event, ok := data.(provider.ApprovalEvent)
		if !ok {
			return
		}
		switch {
		case event.Action == "request" && event.Request != nil && event.Request.RoutedTo != nil:
			request := *event.Request
			a.workerPrompts.queue.Go(func() {
				a.deliverWorkerPrompt(*request.RoutedTo, request.ThreadID, request.RequestID, &request, nil)
			})
		case event.Action == "resolve":
			threadID, requestID := event.ThreadID, event.RequestID
			a.workerPrompts.queue.Go(func() { a.settleWorkerPrompt(threadID, requestID) })
		}
	case eventchan.ProviderUserInput:
		event, ok := data.(provider.UserInputEvent)
		if !ok {
			return
		}
		switch {
		case event.Action == "request" && event.Request != nil && event.Request.RoutedTo != nil:
			request := *event.Request
			a.workerPrompts.queue.Go(func() {
				a.deliverWorkerPrompt(*request.RoutedTo, request.ThreadID, request.RequestID, nil, &request)
			})
		case event.Action == "resolve":
			threadID, requestID := event.ThreadID, event.RequestID
			a.workerPrompts.queue.Go(func() { a.settleWorkerPrompt(threadID, requestID) })
		}
	}
}

// deliverWorkerPrompt queues one routed prompt to its agent. A prompt
// answered before this runs is left alone; one the agent cannot take goes
// back to the user.
func (a *App) deliverWorkerPrompt(route provider.PromptRoute, workerID, requestID string, approval *provider.ApprovalRequest, question *provider.UserInputRequest) {
	if a.triage == nil || !a.triage.PendingPromptOpen(workerID, requestID) {
		return
	}
	prompt := a.workerPrompts.add(workerPrompt{
		workerID: workerID, agentID: route.ThreadID, requestID: requestID,
		approval: approval, question: question,
	}, a.triage.PendingPromptOpen)
	worker, err := a.store.GetThread(workerID)
	if err != nil {
		log.Printf("worker prompts: read worker %s: %v", workerID, err)
		a.handBackWorkerPrompt(prompt)
		return
	}
	kind := workerPromptKindQuestion
	if approval != nil {
		kind = workerPromptKindApproval
	}
	body := renderWorkerPromptWake(worker.Title, workerID, prompt)
	var abandoned error
	// The user may have answered, and the agent may have been archived,
	// before the prompt is queued and again before a session is started
	// for it.
	agentArchived := func() (bool, error) {
		agent, err := a.store.GetThread(route.ThreadID)
		if err != nil {
			return false, fmt.Errorf("read the thread: %w", err)
		}
		return agent.Archived, nil
	}
	err = a.queueAgentNotice(agentNotice{
		threadID:   route.ThreadID,
		sendID:     prompt.sendID(),
		origin:     usermessage.OriginAgentWake,
		originWake: &usermessage.OriginWake{Kind: kind, Label: worker.Title, ThreadID: workerID},
		prepare: func() (string, bool, error) {
			if !a.workerPrompts.isRouted(prompt.token) {
				return "", false, nil
			}
			archived, err := agentArchived()
			if err != nil {
				return "", false, err
			}
			if archived {
				abandoned = errors.New("the agent is archived")
				return "", false, nil
			}
			return body, true, nil
		},
		keep: func() (bool, error) {
			if !a.workerPrompts.isRouted(prompt.token) {
				return false, nil
			}
			archived, err := agentArchived()
			return !archived, err
		},
		withdrawn: func() { a.handBackWorkerPrompt(prompt) },
		startFailed: func(err error) string {
			return fmt.Sprintf("Thread %q is waiting on your answer and its prompt is queued here, but this thread's agent could not start: %v", worker.Title, err)
		},
	})
	if err == nil {
		err = abandoned
	}
	if err != nil {
		log.Printf("worker prompts: deliver %s of %s to %s: %v", requestID, workerID, route.ThreadID, err)
		a.handBackWorkerPrompt(prompt)
	}
}

// settleWorkerPrompt records that a routed prompt was resolved. Unless the
// agent's own answer resolved it, the user answered it in the worker or the
// worker stopped waiting, and a wake still queued is withdrawn.
func (a *App) settleWorkerPrompt(workerID, requestID string) {
	prompt, settled := a.workerPrompts.settle(workerID, requestID)
	if !settled {
		return
	}
	if _, err := a.removeQueuedItem(a.lifeCtx(), prompt.agentID, prompt.sendID()); err != nil {
		log.Printf("worker prompts: withdraw the wake for %s of %s: %v", requestID, workerID, err)
	}
}

// handBackWorkerPrompt gives a prompt its agent could not take to the user.
func (a *App) handBackWorkerPrompt(prompt *workerPrompt) {
	if _, settled := a.workerPrompts.settle(prompt.workerID, prompt.requestID); !settled {
		return
	}
	if a.triage != nil && a.triage.UnroutePrompt(prompt.workerID, prompt.requestID) {
		log.Printf("worker prompts: %s of %s went back to the user: %s could not take it", prompt.requestID, prompt.workerID, prompt.agentID)
	}
}

// handBackWorkerPromptWake hands back the prompt a dropped wake carried.
// False when sendID is not a worker prompt's wake.
func (a *App) handBackWorkerPromptWake(sendID string) bool {
	token, ok := strings.CutPrefix(sendID, workerPromptSendIDRoot)
	if !ok {
		return false
	}
	if prompt := a.workerPrompts.get(token); prompt != nil {
		a.handBackWorkerPrompt(prompt)
	}
	return true
}

// answerWorkerPrompt is thread_reply on a worker prompt's token. Handled is
// false for any other token.
func (a *App) answerWorkerPrompt(ctx context.Context, caller threadtools.Caller, call threadtools.ReplyCall) (ack threadtools.ReplyAck, handled bool, err error) {
	prompt := a.workerPrompts.get(call.Token)
	if prompt == nil {
		return threadtools.ReplyAck{}, false, nil
	}
	ack = threadtools.ReplyAck{Token: call.Token, State: threadtools.RequestReplied, SourceThreadID: prompt.workerID, Prompt: true}
	if prompt.agentID != caller.ThreadID {
		return ack, true, errorsx.Public(threadtools.CodeRequestNotYours,
			"That token belongs to a prompt sent to another thread. Answer only the prompts that woke this thread.", nil)
	}
	answer := workerPromptAnswerKey(call)
	approval, question, err := workerPromptResponse(prompt, call)
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
	// resolution to settle them by.
	if a.triage == nil || !a.triage.PendingPromptOpen(prompt.workerID, prompt.requestID) {
		a.workerPrompts.endAnswer(prompt.token, workerPromptSettled)
		return ack, true, errWorkerPromptClosed
	}
	if approval != nil {
		err = a.RespondToApproval(ctx, prompt.workerID, *approval)
	} else {
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

// add registers a prompt under a fresh token. It first forgets prompts
// closed past their retention and marks closed the routed ones whose
// prompt is gone, so the registry holds no more than the open prompts and a
// day of closed ones.
func (w *workerPrompts) add(prompt workerPrompt, open func(threadID, requestID string) bool) *workerPrompt {
	prompt.token = newThreadRequestToken()
	now := time.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.byToken == nil {
		w.byToken = make(map[string]*workerPrompt)
	}
	for token, existing := range w.byToken {
		if existing.state == workerPromptRouted && !open(existing.workerID, existing.requestID) {
			existing.state, existing.closedAt = workerPromptSettled, now
		}
		if !existing.closedAt.IsZero() && now.Sub(existing.closedAt) > workerPromptRetention {
			delete(w.byToken, token)
		}
	}
	stored := prompt
	w.byToken[stored.token] = &stored
	return &stored
}

func (w *workerPrompts) get(token string) *workerPrompt {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.byToken[token]
}

func (w *workerPrompts) isRouted(token string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	prompt := w.byToken[token]
	return prompt != nil && prompt.state == workerPromptRouted
}

// settle closes a routed prompt by its worker and request. Settled is false
// when no routed prompt matches, including one the agent is answering or
// answered: that answer is what resolved it.
func (w *workerPrompts) settle(workerID, requestID string) (*workerPrompt, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, prompt := range w.byToken {
		if prompt.workerID != workerID || prompt.requestID != requestID {
			continue
		}
		if prompt.state != workerPromptRouted {
			return prompt, false
		}
		prompt.state, prompt.closedAt = workerPromptSettled, time.Now()
		return prompt, true
	}
	return nil, false
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

// endAnswer records how the agent's answer ended.
func (w *workerPrompts) endAnswer(token string, state workerPromptState) {
	w.mu.Lock()
	defer w.mu.Unlock()
	prompt := w.byToken[token]
	if prompt == nil {
		return
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
