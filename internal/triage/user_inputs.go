package triage

import (
	"encoding/json"

	"agent-overflow/internal/provider"
)

func decodeUserInputRequest(raw json.RawMessage) provider.UserInputRequest {
	if len(raw) == 0 {
		return provider.UserInputRequest{}
	}
	var request provider.UserInputRequest
	if json.Unmarshal(raw, &request) != nil {
		return provider.UserInputRequest{}
	}
	return request
}

func (r *Router) setPendingUserInput(threadID string, request provider.UserInputRequest) {
	if request.RequestID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pendingUserInputs[approvalStateKey(threadID, request.RequestID)] = request
	rememberInteractiveRequestOrder(r.pendingUserInputOrder, threadID, request.RequestID)
}

func (r *Router) takePendingUserInput(threadID, requestID string) (provider.UserInputRequest, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := approvalStateKey(threadID, requestID)
	request, ok := r.pendingUserInputs[key]
	if ok {
		delete(r.pendingUserInputs, key)
		removeInteractiveRequestOrder(r.pendingUserInputOrder, threadID, requestID)
	}
	return request, ok
}

func (r *Router) handleUserInputRequest(evt provider.ProviderEvent) error {
	request := decodeUserInputRequest(evt.Meta)
	if request.RequestID == "" {
		request.RequestID = evt.ItemID
	}
	if request.ThreadID == "" {
		request.ThreadID = evt.ThreadID
	}
	if request.TurnID == "" {
		request.TurnID = evt.TurnID
	}
	if request.RequestID == "" || request.ThreadID == "" || len(request.Questions) == 0 {
		return r.handleError(provider.ProviderEvent{
			Kind:      provider.EventError,
			ThreadID:  evt.ThreadID,
			Content:   "provider sent an invalid user-input request",
			Timestamp: evt.Timestamp,
		})
	}

	r.setPendingUserInput(evt.ThreadID, request)
	r.emit("provider:user_input", provider.UserInputEvent{
		Action:   "request",
		ThreadID: evt.ThreadID,
		Request:  &request,
	})
	return nil
}

func (r *Router) handleUserInputResolved(evt provider.ProviderEvent) error {
	requestID, decision, _ := decodeApprovalResolvedMeta(evt.Meta)
	if requestID == "" {
		requestID = evt.ItemID
	}
	r.takePendingUserInput(evt.ThreadID, requestID)
	r.emit("provider:user_input", provider.UserInputEvent{
		Action:    "resolve",
		ThreadID:  evt.ThreadID,
		RequestID: requestID,
		Decision:  decision,
	})
	return nil
}
