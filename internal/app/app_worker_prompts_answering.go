package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
)

// An agent's answer to a prompt routed to its thread: the reply checked
// against the prompt, claimed, written to the worker's provider, and the one
// exit every answer ends through.

// answerWorkerPrompt is thread_reply on a worker prompt's token: one a
// worker here asked, or one a paired computer's worker put to this
// computer's thread (answerRemoteWorkerPrompt). Handled is false for any
// other token.
func (a *App) answerWorkerPrompt(ctx context.Context, caller threadtools.Caller, call threadtools.ReplyCall) (ack threadtools.ReplyAck, handled bool, err error) {
	prompt, found := a.workerPrompts.get(call.Token)
	if !found {
		return a.answerRemoteWorkerPrompt(ctx, caller, call)
	}
	if prompt.agentID != caller.ThreadID || prompt.remote != nil {
		ack = threadtools.ReplyAck{Token: call.Token, State: threadtools.RequestReplied, SourceThreadID: prompt.workerID, Prompt: true}
		return ack, true, errorsx.Public(threadtools.CodeRequestNotYours,
			"That token belongs to a prompt sent to another thread. Answer only the prompts that woke this thread.", nil)
	}
	ack, err = a.answerRoutedPrompt(ctx, prompt, call)
	return ack, true, err
}

// answerRoutedPrompt writes the answer of the thread a prompt is routed to,
// whose caller its door has checked: thread_reply here, or
// agent_prompt_reply from the paired computer the prompt is held for.
func (a *App) answerRoutedPrompt(ctx context.Context, prompt workerPrompt, call threadtools.ReplyCall) (ack threadtools.ReplyAck, err error) {
	ack = threadtools.ReplyAck{Token: call.Token, State: threadtools.RequestReplied, SourceThreadID: prompt.workerID, Prompt: true}
	answer := workerPromptAnswerKey(call)
	approval, question, err := workerPromptResponse(&prompt, call)
	if err != nil {
		return ack, err
	}
	if err := a.workerPrompts.beginAnswer(prompt.token, answer); err != nil {
		if errors.Is(err, errWorkerPromptRepeated) {
			return ack, nil
		}
		return ack, err
	}
	// A worker whose session ended took its prompts with it, without a
	// resolution to settle them by, and a prompt that moved is its new
	// recipient's.
	if !a.workerPromptOpenTo(prompt) {
		a.endPromptAnswer(prompt.token, workerPromptSettled)
		return ack, errWorkerPromptClosed
	}
	// The answer goes to the session that asked, or nowhere; an async
	// question's goes to the worker's queue, as the person's answers do.
	if hook := a.workerPromptAnsweringForTest; hook != nil {
		err = hook(prompt.token)
	}
	switch {
	case err != nil:
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
			a.endPromptAnswer(prompt.token, workerPromptSettled)
			return ack, errWorkerPromptClosed
		}
		a.endPromptAnswer(prompt.token, workerPromptRouted)
		log.Printf("worker prompts: answer %s of %s: %v", prompt.requestID, prompt.workerID, err)
		if errors.Is(err, provider.ErrApprovalDecisionUnavailable) {
			return ack, errorsx.Public(threadtools.CodeInvalidRequest,
				"The worker's provider does not offer that decision for this approval. Deny it, or ask the user.", nil)
		}
		return ack, errorsx.Public(threadtools.CodeInvalidRequest,
			"The answer did not reach the worker, and its prompt is still open. Try again, or ask the user.", nil)
	}
	a.endPromptAnswer(prompt.token, workerPromptAnswered)
	ack.Accepted = true
	return ack, nil
}

// endPromptAnswer is the one exit from answering. A prompt the answer left
// routed did not change hands meanwhile, since every other transition
// skips a prompt being answered, so this reconciles what happened: a hand
// back asked for meanwhile (a return, a wake that was not taken) hands it
// back now; a prompt no longer open to its agent (the worker stopped
// waiting, or the prompt moved) is handed back, which moves nothing for a
// closed one; and one held for a paired computer goes back unless its link
// still owns the worker.
func (a *App) endPromptAnswer(token string, state workerPromptState) {
	prompt, routed, handBack := a.workerPrompts.endAnswer(token, state)
	switch {
	case !routed:
	case handBack, !a.workerPromptOpenTo(prompt):
		a.handBackWorkerPrompt(token)
	case prompt.remote != nil && !a.promptLinkStands(prompt):
		a.handBackWorkerPrompt(token)
	case prompt.remote == nil:
		// A placement that changed while it was answered applies now.
		a.workerPrompts.run(func() { a.routeWorkerPromptsAgain(prompt.workerID) })
	}
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
