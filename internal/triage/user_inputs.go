package triage

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/stringsx"
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
	// Same attribution rule as an approval (approvals.go
	// resolveInteractiveScope): a question a SUBAGENT raised belongs on
	// its card. Resolved once here so the frontend event, the pending
	// snapshot a reconnect hydrates from, and the Codex synthetic row
	// completeCodexUserInputToolCall mints all carry one answer.
	request.ParentToolUseID = r.resolveInteractiveScope(evt, request.ParentToolUseID, request.ToolUseID)
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
		if err := r.persistResolvedUserInput(evt, request, decision, answers); err != nil {
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

// persistResolvedUserInput writes the user's submitted answers onto the
// persisted tool-call row. The two providers reach this point with different
// row lifecycles, so the write-back differs by provider — dispatched on
// isCodexThread, the same provider seam the tool-completion path uses:
//
//   - Codex `request_user_input` is a synthetic tool call AO fabricates to
//     represent the prompt. This resolve IS its only completion, so the row is
//     flipped to completed (completeCodexUserInputToolCall).
//   - Claude `AskUserQuestion` is a real CLI tool that emits its own
//     `tool_result` completion later. Here we merge the answers (and the
//     normalized question list) onto the still-running launch row and leave its
//     status untouched — see mergeUserInputAnswersIntoLaunch.
func (r *Router) persistResolvedUserInput(evt provider.ProviderEvent, request provider.UserInputRequest, decision string, answers map[string]provider.UserInputAnswer) error {
	if r.store == nil {
		return nil
	}
	codexThread, err := r.isCodexThread(evt.ThreadID)
	if err != nil {
		return err
	}
	if codexThread {
		return r.completeCodexUserInputToolCall(evt, request, decision, answers)
	}
	return r.mergeUserInputAnswersIntoLaunch(evt, request, answers)
}

// mergeUserInputAnswersIntoLaunch additively merges the user's AskUserQuestion
// answers into the existing launch row's meta, where the frontend card reads
// them (item.meta.answers, keyed by normalized question id).
//
// It also refreshes item.meta.input.questions with the normalized question list.
// The launch row was created from the raw tool_use input, which carries no
// per-question id, so two questions sharing a header would both resolve to the
// first answer (the card falls back to header matching). The request we hold
// here already carries NormalizeUserInputQuestions' deduped ids (Scope/Scope-2,
// set in parse_control.go), and the card prefers q.id over q.header
// (askUserQuestionData.ts answersForQuestion), so persisting the normalized list
// disambiguates duplicate headers. It is identical to the raw list for the
// common distinct-header case (id == header), where it only adds the id field.
//
// It deliberately does NOT touch status: Claude sends its own tool_result
// completion for the same tool_use id, and persistToolCallCompletion's
// terminal-status guard would drop that real completion if this resolve had
// already flipped the row terminal. This is the same "refresh meta on the
// running launch row, let the later wire completion settle status" shape
// persistToolCallCompletion uses for backgrounded placeholders
// (tool_lifecycle.go). The deep meta merge guarantees `answers`, the refreshed
// `input`, and the later `tool_result` coexist on the row regardless of arrival
// order.
func (r *Router) mergeUserInputAnswersIntoLaunch(evt provider.ProviderEvent, request provider.UserInputRequest, answers map[string]provider.UserInputAnswer) error {
	if r.store == nil || request.ToolUseID == "" || len(answers) == 0 {
		return nil
	}
	launch, found, err := r.store.GetThreadItem(evt.ThreadID, request.ToolUseID)
	if err != nil {
		return fmt.Errorf("user input answer lookup %s: %w", request.ToolUseID, err)
	}
	if !found || launch.Kind != itemKindToolCall {
		return nil
	}
	merge := map[string]any{"answers": answers}
	// Guard non-empty so a malformed request can never blank out the questions
	// the card renders; in practice a registered request always has questions.
	if len(request.Questions) > 0 {
		merge["input"] = map[string]any{"questions": request.Questions}
	}
	meta, err := json.Marshal(merge)
	if err != nil {
		return fmt.Errorf("user input answer marshal %s: %w", request.ToolUseID, err)
	}
	launch.Meta = mergeItemMetaJSON(launch.Meta, json.RawMessage(meta))
	launch.UpdatedAt = eventTimestampMillis(evt)
	return r.persistItem(launch, nil)
}

func (r *Router) completeCodexUserInputToolCall(evt provider.ProviderEvent, request provider.UserInputRequest, decision string, answers map[string]provider.UserInputAnswer) error {
	if r.store == nil || request.ToolUseID == "" {
		return nil
	}
	if strings.TrimSpace(request.ToolName) != "user_input" {
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
		Kind:     provider.EventToolComplete,
		ThreadID: evt.ThreadID,
		TurnID:   turnID,
		ItemID:   request.ToolUseID,
		ItemType: "request_user_input",
		// The synthetic row is the prompt's only timeline presence, so
		// it carries the scope resolved at request time — otherwise a
		// child agent's question renders as the main agent's.
		ParentToolUseID: stringsx.FirstNonEmptyTrimmed(request.ParentToolUseID, evt.ParentToolUseID),
		Meta:            meta,
		Timestamp:       timestamp,
	})
}
