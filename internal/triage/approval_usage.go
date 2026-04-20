package triage

import (
	"encoding/json"
	"fmt"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

type pendingApprovalState struct {
	Request provider.ApprovalRequest
	ItemID  string
}

func providerScopedItemID(threadID, raw string) string {
	_ = threadID
	raw = strings.TrimSpace(raw)
	return raw
}

func eventParentID(evt provider.ProviderEvent) string {
	return providerScopedItemID(evt.ThreadID, evt.ParentToolUseID)
}

func approvalStateKey(threadID, requestID string) string {
	return threadID + ":" + requestID
}

func approvalDecisionKey(threadID, itemID string) string {
	return threadID + ":" + itemID
}

func (r *Router) setPendingApproval(threadID string, approval pendingApprovalState) {
	if approval.Request.RequestID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pendingApprovals[approvalStateKey(threadID, approval.Request.RequestID)] = approval
}

func (r *Router) takePendingApproval(threadID, requestID string) (pendingApprovalState, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := approvalStateKey(threadID, requestID)
	approval, ok := r.pendingApprovals[key]
	if ok {
		delete(r.pendingApprovals, key)
	}
	return approval, ok
}

func (r *Router) rememberApprovalDecision(threadID, itemID, decision string) {
	if threadID == "" || itemID == "" || decision == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pendingApprovalItems[approvalDecisionKey(threadID, itemID)] = decision
}

func (r *Router) takeApprovalDecision(threadID, itemID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := approvalDecisionKey(threadID, itemID)
	decision := r.pendingApprovalItems[key]
	delete(r.pendingApprovalItems, key)
	return decision
}

func (r *Router) peekApprovalDecision(threadID, itemID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pendingApprovalItems[approvalDecisionKey(threadID, itemID)]
}

func decodeApprovalRequest(raw json.RawMessage) provider.ApprovalRequest {
	if len(raw) == 0 {
		return provider.ApprovalRequest{}
	}
	var approval provider.ApprovalRequest
	if json.Unmarshal(raw, &approval) != nil {
		return provider.ApprovalRequest{}
	}
	return approval
}

func decodeApprovalResolvedMeta(raw json.RawMessage) (requestID, decision string) {
	if len(raw) == 0 {
		return "", ""
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(raw, &payload) != nil {
		return "", ""
	}
	requestID = firstNonEmptyString(
		readJSONID(payload["providerRequestId"]),
		readJSONID(payload["requestId"]),
	)
	decision = firstNonEmptyString(
		readJSONString(payload["decision"]),
		readJSONNestedString(payload["resolution"], "decision"),
	)
	return requestID, decision
}

func readJSONID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		return asString
	}
	var asInt int64
	if json.Unmarshal(raw, &asInt) == nil {
		return fmt.Sprintf("%d", asInt)
	}
	return ""
}

func readJSONString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	return ""
}

func readJSONNestedString(raw json.RawMessage, path ...string) string {
	if len(raw) == 0 || len(path) == 0 {
		return ""
	}
	var current any
	if json.Unmarshal(raw, &current) != nil {
		return ""
	}
	for _, segment := range path {
		obj, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		next, ok := obj[segment]
		if !ok {
			return ""
		}
		current = next
	}
	value, _ := current.(string)
	return value
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func approvalItemID(evt provider.ProviderEvent, request provider.ApprovalRequest) string {
	if request.ToolUseID != "" {
		return providerScopedItemID(evt.ThreadID, request.ToolUseID)
	}
	if evt.ItemID != "" && evt.ItemID != request.RequestID {
		return providerScopedItemID(evt.ThreadID, evt.ItemID)
	}
	return ""
}

func isToolApproval(request provider.ApprovalRequest, itemID string) bool {
	switch request.Kind {
	case "command", "file-read", "file-change", "permission":
		return true
	}
	return itemID != ""
}

func approvalSummary(request provider.ApprovalRequest) string {
	summary := buildToolCallSummary(toolStartMeta{
		ToolName: request.ToolName,
		Input:    request.Input,
	}, request.ToolName)
	if summary != "" {
		return summary
	}
	return firstNonEmptyString(request.Description, request.Title, request.ToolName, "tool")
}

func approvalDeclinesExecution(decision string) bool {
	switch decision {
	case "declined", "timeout":
		return true
	default:
		return false
	}
}

func approvalLosesExecution(decision string) bool {
	return decision == "lost"
}

func (r *Router) handleApprovalRequest(evt provider.ProviderEvent) error {
	request := decodeApprovalRequest(evt.Meta)
	if request.RequestID == "" {
		request.RequestID = evt.ItemID
	}
	if request.ThreadID == "" {
		request.ThreadID = evt.ThreadID
	}
	if request.TurnID == "" {
		request.TurnID = evt.TurnID
	}

	itemID := approvalItemID(evt, request)
	r.setPendingApproval(evt.ThreadID, pendingApprovalState{
		Request: request,
		ItemID:  itemID,
	})
	r.emit("provider:approval", provider.ApprovalEvent{
		Action:   "request",
		ThreadID: evt.ThreadID,
		Request:  &request,
	})
	return nil
}

func (r *Router) handleApprovalResolved(evt provider.ProviderEvent) error {
	requestID, decision := decodeApprovalResolvedMeta(evt.Meta)
	if requestID == "" {
		requestID = evt.ItemID
	}

	pending, _ := r.takePendingApproval(evt.ThreadID, requestID)
	itemID := pending.ItemID
	if itemID == "" {
		itemID = approvalItemID(evt, pending.Request)
	}

	if itemID != "" && decision != "" {
		r.rememberApprovalDecision(evt.ThreadID, itemID, decision)
		if err := r.applyApprovalDecision(evt.ThreadID, itemID, pending.Request, decision, eventTimestampMillis(evt)); err != nil {
			return err
		}
	}

	r.emit("provider:approval", provider.ApprovalEvent{
		Action:    "resolve",
		ThreadID:  evt.ThreadID,
		RequestID: requestID,
		Decision:  decision,
	})
	return nil
}

func (r *Router) applyApprovalDecision(
	threadID, itemID string,
	request provider.ApprovalRequest,
	decision string,
	now int64,
) error {
	if itemID == "" || decision == "" {
		return nil
	}

	item, found, err := r.store.GetThreadItem(threadID, itemID)
	if err != nil {
		return fmt.Errorf("approval item lookup: %w", err)
	}

	if found {
		item.Decision = decision
		if item.ToolName == "" {
			item.ToolName = request.ToolName
		}
		if item.Summary == "" {
			item.Summary = approvalSummary(request)
		}
		if approvalDeclinesExecution(decision) && item.Status != statusCompleted && item.Status != statusErrored {
			item.Status = "declined"
		}
		if approvalLosesExecution(decision) && item.Status != statusCompleted && item.Status != "declined" {
			item.Status = statusErrored
		}
		item.UpdatedAt = now
		if err := r.persistItem(item, nil); err != nil {
			return fmt.Errorf("approval item update: %w", err)
		}
		r.takeApprovalDecision(threadID, itemID)
		return nil
	}

	if !isToolApproval(request, itemID) || !approvalDeclinesExecution(decision) {
		return nil
	}

	item, err = r.newToolCallItem(
		threadID,
		itemID,
		firstNonEmptyString(request.ToolName, "tool"),
		approvalSummary(request),
		"declined",
		now,
	)
	if err != nil {
		return fmt.Errorf("approval synthetic item: %w", err)
	}
	item.Decision = decision
	item.ParentID = ""
	if approvalLosesExecution(decision) {
		item.Status = statusErrored
	}
	if err := r.persistItem(item, nil); err != nil {
		return fmt.Errorf("approval synthetic item persist: %w", err)
	}
	r.takeApprovalDecision(threadID, itemID)
	return nil
}

func decodeContextWindow(raw json.RawMessage) (provider.ContextWindow, bool) {
	if len(raw) == 0 {
		return provider.ContextWindow{}, false
	}
	var window provider.ContextWindow
	if json.Unmarshal(raw, &window) == nil {
		if window.UsedTokens != 0 || window.MaxTokens != 0 || window.UsedPercentage != 0 || window.TotalProcessed != 0 {
			return window, true
		}
	}

	var usage provider.TokenUsage
	if json.Unmarshal(raw, &usage) == nil {
		used := usage.InputTokens + usage.OutputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens
		if used > 0 {
			return provider.ContextWindow{
				UsedTokens: used,
			}, true
		}
	}
	return provider.ContextWindow{}, false
}

func encodeContextWindow(window provider.ContextWindow) string {
	data, err := json.Marshal(map[string]any{
		"usedTokens":     window.UsedTokens,
		"maxTokens":      window.MaxTokens,
		"contextPercent": window.UsedPercentage,
		"totalProcessed": window.TotalProcessed,
	})
	if err != nil {
		return ""
	}
	return string(data)
}

func (r *Router) handleCompaction(evt provider.ProviderEvent) error {
	now := eventTimestampMillis(evt)
	turnIndex, err := r.currentTurnIndex(evt.ThreadID)
	if err != nil {
		return fmt.Errorf("compaction turn index: %w", err)
	}

	item := store.Item{
		ID:        nextCompactionID(turnIndex),
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		Kind:      "compaction",
		Role:      "system",
		Status:    statusCompleted,
		Summary:   firstNonEmptyString(strings.TrimSpace(evt.Content), "Context compacted"),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := r.persistItem(item, nil); err != nil {
		return err
	}
	if err := r.store.ClearLastTokenUsage(evt.ThreadID); err != nil {
		return fmt.Errorf("compaction clear usage: %w", err)
	}
	r.emit("provider:usage", provider.UsageEvent{
		Action:   "reset",
		ThreadID: evt.ThreadID,
	})
	return nil
}
