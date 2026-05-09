package triage

import (
	"encoding/json"
	"strings"
	"time"

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

func decodeUserInputResolvedMeta(raw json.RawMessage) (requestID string, decision string, answers map[string]provider.UserInputAnswer) {
	requestID, decision, _ = decodeApprovalResolvedMeta(raw)
	if len(raw) == 0 {
		return requestID, decision, nil
	}
	var payload struct {
		Answers map[string]provider.UserInputAnswer `json:"answers"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return requestID, decision, nil
	}
	return requestID, decision, payload.Answers
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
	if request.ToolUseID == "" && request.ToolName == "user_input" {
		request.ToolUseID = evt.ItemID
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
	// User-input requests are a sidebar-bump boundary alongside
	// user_text persist and turn settle: the agent is paused waiting
	// for the user. Resolutions ride on the user's submitted answer,
	// not on this request, so they don't bump separately.
	requestedAt := eventTimestampMillis(evt)
	r.bumpThreadActivity(evt.ThreadID, requestedAt, "user_input request")
	r.emit("provider:user_input", provider.UserInputEvent{
		Action:      "request",
		ThreadID:    evt.ThreadID,
		Request:     &request,
		RequestedAt: requestedAt,
	})
	return nil
}

func (r *Router) handleUserInputResolved(evt provider.ProviderEvent) error {
	requestID, decision, answers := decodeUserInputResolvedMeta(evt.Meta)
	if requestID == "" {
		requestID = evt.ItemID
	}
	request, ok := r.takePendingUserInput(evt.ThreadID, requestID)
	if ok {
		if err := r.completeCodexUserInputToolCall(evt, request, decision, answers); err != nil {
			return err
		}
	}
	r.emit("provider:user_input", provider.UserInputEvent{
		Action:    "resolve",
		ThreadID:  evt.ThreadID,
		RequestID: requestID,
		Decision:  decision,
	})
	return nil
}

func (r *Router) completeCodexUserInputToolCall(evt provider.ProviderEvent, request provider.UserInputRequest, decision string, answers map[string]provider.UserInputAnswer) error {
	if r.store == nil || request.ToolUseID == "" {
		return nil
	}
	if strings.TrimSpace(request.ToolName) != "user_input" {
		return nil
	}
	codexThread, err := r.isCodexThread(evt.ThreadID)
	if err != nil {
		return err
	}
	if !codexThread {
		return nil
	}
	if answers == nil {
		answers = map[string]provider.UserInputAnswer{}
	}

	metaFields := map[string]any{
		"toolName": "request_user_input",
		"answers":  answers,
		"decision": decision,
	}
	if decision != "" && decision != "answered" {
		metaFields["is_error"] = true
	}
	meta, _ := json.Marshal(metaFields)

	timestamp := evt.Timestamp
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	turnID := request.TurnID
	if turnID == "" {
		turnID = evt.TurnID
	}
	return r.handleToolComplete(provider.ProviderEvent{
		Kind:      provider.EventToolComplete,
		ThreadID:  evt.ThreadID,
		TurnID:    turnID,
		ItemID:    request.ToolUseID,
		ItemType:  "request_user_input",
		Meta:      meta,
		Timestamp: timestamp,
	})
}
