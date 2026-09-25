package triage

import (
	"fmt"
	"maps"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
)

// An agent's open prompts (docs/architecture/turn-lifecycle.md, §Agent-owned
// rows). An approval or question an agent asked, and the decision an
// answer left for a tool row not written yet, are the agent's like its
// rows: a turn's end leaves them open and the agent's end settles them. A
// prompt's scope is the one resolved when it was raised
// (resolveInteractiveScope); the agent that owns that scope asked it.

// eachOpenRequestScopeLocked calls add with the scope of every open prompt
// and waiting decision. Caller holds r.mu.
func eachOpenRequestScopeLocked(st *threadState, add func(string)) {
	for _, pending := range st.pendingApprovals {
		add(pending.Request.ParentToolUseID)
	}
	for _, request := range st.pendingUserInputs {
		add(request.ParentToolUseID)
	}
	for _, remembered := range st.pendingApprovalItems {
		add(remembered.scope)
	}
}

// dropTurnRequestsLocked forgets the prompts a turn's end leaves
// unanswered and the decisions no tool row took, except those in
// agentScopes, which wait for their agent's end (settleAgentRequests). A
// prompt still open when its turn ends was never answered: the subprocess
// died, a fatal error ended the turn, or the model moved on; the next turn
// must not inherit its request id. The answer records of the prompts that
// stay open stay with them. Caller holds r.mu.
func dropTurnRequestsLocked(st *threadState, agentScopes map[string]bool) {
	maps.DeleteFunc(st.pendingApprovals, func(_ string, pending pendingApprovalState) bool {
		return !agentScopes[pending.Request.ParentToolUseID]
	})
	maps.DeleteFunc(st.pendingUserInputs, func(_ string, request provider.UserInputRequest) bool {
		return !agentScopes[request.ParentToolUseID]
	})
	maps.DeleteFunc(st.pendingApprovalItems, func(_ string, remembered approvalDecision) bool {
		return !agentScopes[remembered.scope]
	})
	maps.DeleteFunc(st.answeredRequests, func(requestID string, _ struct{}) bool {
		_, approval := st.pendingApprovals[requestID]
		_, question := st.pendingUserInputs[requestID]
		return !approval && !question
	})
	st.pendingApprovalOrder = keepOpenRequests(st.pendingApprovalOrder, st.pendingApprovals)
	st.pendingUserInputOrder = keepOpenRequests(st.pendingUserInputOrder, st.pendingUserInputs)
}

// keepOpenRequests is order without the request ids open no longer holds.
func keepOpenRequests[V any](order []string, open map[string]V) []string {
	kept := order[:0]
	for _, requestID := range order {
		if _, ok := open[requestID]; ok {
			kept = append(kept, requestID)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

// settleAgentRequests ends the prompts agent rootID asked: an ended agent
// takes no answer. Each open prompt goes with a "lost" resolution, the one
// the provider sends when it abandons a prompt and on which every client
// drops it. Its answer record stays until the turn boundary, as a resolved
// prompt's does, so a second answer is still refused. A decision no tool
// row took goes: the agent's end settled that row. A thread with no open
// prompt asks the store nothing.
func (r *Router) settleAgentRequests(threadID, rootID string) error {
	r.mu.Lock()
	var scopes []string
	if st := r.threadStateIfPresent(threadID); st != nil {
		seen := make(map[string]bool)
		eachOpenRequestScopeLocked(st, func(scope string) {
			if scope != "" && !seen[scope] {
				seen[scope] = true
				scopes = append(scopes, scope)
			}
		})
	}
	r.mu.Unlock()
	if len(scopes) == 0 {
		return nil
	}
	owners, err := r.store.AgentOwners(threadID, scopes)
	if err != nil {
		return fmt.Errorf("owners of the open prompts of %s: %w", threadID, err)
	}
	asked := func(scope string) bool { return scope != "" && owners[scope] == rootID }

	var approvals, questions []string
	r.mu.Lock()
	if st := r.threadStateIfPresent(threadID); st != nil {
		for _, requestID := range st.pendingApprovalOrder {
			if pending, ok := st.pendingApprovals[requestID]; ok && asked(pending.Request.ParentToolUseID) {
				approvals = append(approvals, requestID)
				delete(st.pendingApprovals, requestID)
			}
		}
		for _, requestID := range st.pendingUserInputOrder {
			if request, ok := st.pendingUserInputs[requestID]; ok && asked(request.ParentToolUseID) {
				questions = append(questions, requestID)
				delete(st.pendingUserInputs, requestID)
			}
		}
		maps.DeleteFunc(st.pendingApprovalItems, func(_ string, remembered approvalDecision) bool {
			return asked(remembered.scope)
		})
		st.pendingApprovalOrder = keepOpenRequests(st.pendingApprovalOrder, st.pendingApprovals)
		st.pendingUserInputOrder = keepOpenRequests(st.pendingUserInputOrder, st.pendingUserInputs)
	}
	r.mu.Unlock()

	for _, requestID := range approvals {
		r.emit(eventchan.ProviderApproval, provider.ApprovalEvent{
			Action: "resolve", ThreadID: threadID, RequestID: requestID, Decision: "lost",
		})
	}
	for _, requestID := range questions {
		r.emit(eventchan.ProviderUserInput, provider.UserInputEvent{
			Action: "resolve", ThreadID: threadID, RequestID: requestID, Decision: "lost",
		})
	}
	return nil
}
