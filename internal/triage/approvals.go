// Package triage — approval-request coordination and item id derivation.
// This file holds the pending-approval map plumbing plus the helpers that
// convert approval events into persisted items and routed frontend events.

package triage

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/stringsx"
)

// pendingApprovalState carries the request as triage RESOLVED it, not
// as the wire sent it: Request.ParentToolUseID holds the subagent scope
// worked out at request time (see resolveInteractiveScope). The
// resolution has to survive until the decision arrives, because the
// row applyApprovalDecision may have to synthesize is created AFTER the
// asking agent's own row could be looked up — a declined tool that
// never ran has no persisted row to inherit a parent from.
type pendingApprovalState struct {
	Request provider.ApprovalRequest
	ItemID  string
}

func eventParentID(evt provider.ProviderEvent) string {
	return strings.TrimSpace(evt.ParentToolUseID)
}

// resolveInteractiveScope answers "which agent is asking?" for an
// interactive request that names a tool_use. Anything a subagent causes
// carries its scope (docs/specs/agent-visibility.md, Q10): the approval
// row has to nest under the agent's card and light its "needs approval"
// pill, exactly as `permission_denied` now does.
//
// Precedence, strongest evidence first:
//
//  1. a scope the PARSER resolved — Claude carries the asking agent's
//     `agent_id` on `can_use_tool`, which the parser maps through its
//     task_id ↔ tool_use_id table;
//  2. a scope the EVENT envelope carries;
//  3. the requested tool's OWN persisted row scope. `can_use_tool`
//     arrives before the tool runs but AFTER the assistant tool_use that
//     announced it, so the row normally exists and its parent_id already
//     is the attribution — for Agent launches and forked skills alike.
//
// A missing row leaves the request top-level, which is the honest
// record: better a main-timeline approval than one nested under a guess.
//
// permission_notices.go's lookupDeniedToolCall / deniedToolCallScope
// pair is this same resolution for the notice family; the two should
// collapse onto this helper.
func (r *Router) resolveInteractiveScope(evt provider.ProviderEvent, parserScope, toolUseID string) string {
	if scope := strings.TrimSpace(parserScope); scope != "" {
		return scope
	}
	if scope := eventParentID(evt); scope != "" {
		return scope
	}
	return r.persistedToolCallScope(evt.ThreadID, toolUseID)
}

// persistedToolCallScope reads a tool_call row's own parent_id. Empty
// when the id names nothing, names a non-tool row, or the read fails —
// a lookup error is logged and reads as "top level", because the prompt
// must still reach the user.
func (r *Router) persistedToolCallScope(threadID, toolUseID string) string {
	if r == nil || r.store == nil {
		return ""
	}
	toolUseID = strings.TrimSpace(toolUseID)
	if toolUseID == "" {
		return ""
	}
	item, found, err := r.store.GetThreadItem(threadID, toolUseID)
	if err != nil {
		log.Printf("triage: interactive scope lookup %s: %v", toolUseID, err)
		return ""
	}
	if !found || item.Kind != itemKindToolCall {
		return ""
	}
	return strings.TrimSpace(item.ParentID)
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
	rememberInteractiveRequestOrder(r.pendingApprovalOrder, threadID, approval.Request.RequestID)
}

func (r *Router) takePendingApproval(threadID, requestID string) (pendingApprovalState, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := approvalStateKey(threadID, requestID)
	approval, ok := r.pendingApprovals[key]
	if ok {
		delete(r.pendingApprovals, key)
		removeInteractiveRequestOrder(r.pendingApprovalOrder, threadID, requestID)
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

func decodeApprovalResolvedMeta(raw json.RawMessage) (requestID, decision string, updatedInput json.RawMessage) {
	if len(raw) == 0 {
		return "", "", nil
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(raw, &payload) != nil {
		return "", "", nil
	}
	requestID = stringsx.FirstNonEmptyTrimmed(
		readJSONID(payload["providerRequestId"]),
		readJSONID(payload["requestId"]),
	)
	decision = stringsx.FirstNonEmptyTrimmed(
		readJSONString(payload["decision"]),
		readJSONNestedString(payload["resolution"], "decision"),
	)
	if raw, ok := payload["updatedInput"]; ok && len(raw) > 0 {
		updatedInput = raw
	}
	return requestID, decision, updatedInput
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

func approvalItemID(evt provider.ProviderEvent, request provider.ApprovalRequest) string {
	if request.ToolUseID != "" {
		return strings.TrimSpace(request.ToolUseID)
	}
	if evt.ItemID != "" && evt.ItemID != request.RequestID {
		return strings.TrimSpace(evt.ItemID)
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
	summary := BuildToolCallSummary(ToolStartMeta{
		ToolName: request.ToolName,
		Input:    request.Input,
	}, request.ToolName)
	if summary != "" {
		return summary
	}
	return stringsx.FirstNonEmptyTrimmed(request.Description, request.Title, request.ToolName, "tool")
}

func approvalDeclinesExecution(decision string) bool {
	return decision == statusDeclined
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
	request.ParentToolUseID = r.resolveInteractiveScope(evt, request.ParentToolUseID, itemID)
	r.setPendingApproval(evt.ThreadID, pendingApprovalState{
		Request: request,
		ItemID:  itemID,
	})
	// Approval requests are one of the three sidebar-bump boundaries
	// (alongside user_text persist and turn settle): the agent is
	// blocked waiting on the user. Resolutions don't bump — the user's
	// reply lands as a user_text upsert which already bumps activity.
	requestedAt := eventTimestampMillis(evt)
	r.bumpThreadActivity(evt.ThreadID, requestedAt, "approval request")
	r.emit("provider:approval", provider.ApprovalEvent{
		Action:      "request",
		ThreadID:    evt.ThreadID,
		Request:     &request,
		RequestedAt: requestedAt,
	})
	return nil
}

func (r *Router) handleApprovalResolved(evt provider.ProviderEvent) error {
	requestID, decision, updatedInput := decodeApprovalResolvedMeta(evt.Meta)
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
		// When the user amended the input, overlay it onto the request so
		// applyApprovalDecision builds the summary against the MODIFIED
		// input rather than the original. applyApprovalDecision clones
		// request by value internally, so this mutation is scoped.
		if decision == "amended" && len(updatedInput) > 0 {
			pending.Request.Input = updatedInput
		}
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
		// On an amended decision the stored summary must reflect the
		// MODIFIED input — overwrite whatever the tool_call launch wrote
		// so the row renders what will actually run.
		if decision == "amended" && len(request.Input) > 0 {
			if refreshed := approvalSummary(request); refreshed != "" {
				item.Summary = refreshed
			}
		}
		if approvalDeclinesExecution(decision) && item.Status != statusCompleted && item.Status != statusErrored {
			item.Status = statusDeclined
		}
		if approvalLosesExecution(decision) && item.Status != statusCompleted && item.Status != statusDeclined {
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
		stringsx.FirstNonEmptyTrimmed(request.ToolName, "tool"),
		approvalSummary(request),
		statusDeclined,
		now,
	)
	if err != nil {
		return fmt.Errorf("approval synthetic item: %w", err)
	}
	item.Decision = decision
	// The synthesized row stands in for a tool that never ran, so it
	// inherits the scope resolved when the prompt was raised. Nothing
	// else can supply it: the tool has no persisted row of its own —
	// that absence is why we are here.
	item.ParentID = strings.TrimSpace(request.ParentToolUseID)
	if approvalLosesExecution(decision) {
		item.Status = statusErrored
	}
	if err := r.persistItem(item, nil); err != nil {
		return fmt.Errorf("approval synthetic item persist: %w", err)
	}
	r.takeApprovalDecision(threadID, itemID)
	return nil
}
