// Package triage — persistence half of the command inline diff pipeline.
// This file captures and retains the inline-diff preview derived at
// command_execution start, then attaches it to the completed tool-call
// item once the command finishes successfully.

package triage

import (
	"encoding/json"
	"fmt"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/stringsx"
)

type pendingCommandInlineDiff struct {
	ThreadID string
	Meta     ToolResultMeta
	DiffData []byte
}

func (r *Router) capturePendingCommandInlineDiff(evt provider.ProviderEvent) error {
	if evt.ItemType != "command_execution" || evt.ItemID == "" || len(evt.Meta) == 0 {
		return nil
	}

	_, workspacePath, err := r.store.GetThreadProviderWorkspace(evt.ThreadID)
	if err != nil {
		return fmt.Errorf("lookup thread for command inline diff: %w", err)
	}

	meta, diffData, ok := captureCommandExecutionToolResult(evt.Meta, workspacePath)
	if !ok {
		r.clearPendingCommandInlineDiff(evt.ThreadID, evt.ItemID)
		return nil
	}

	r.setPendingCommandInlineDiff(evt.ThreadID, evt.ItemID, pendingCommandInlineDiff{
		ThreadID: evt.ThreadID,
		Meta:     meta,
		DiffData: diffData,
	})
	return nil
}

// persistCommandInlineDiffToolResult attaches a captured command inline
// diff to the command's row: row/found as the handler carries them, and
// the row is returned as this leaves it.
func (r *Router) persistCommandInlineDiffToolResult(evt provider.ProviderEvent, row store.Item, found bool) (store.Item, bool, error) {
	if evt.ItemType != "command_execution" || evt.ItemID == "" {
		return row, found, nil
	}

	pending, ok := r.takePendingCommandInlineDiff(evt.ThreadID, evt.ItemID)
	if !ok || extractRuntimeCommandExitCode(evt.Meta) != 0 {
		return row, found, nil
	}

	return r.persistToolResult(evt, pending.Meta, pending.DiffData, row, found)
}

// persistToolResult links a tool result payload onto the tool's row, or
// creates the row when none exists (found=false). row is the tool's row
// as the handler carries it; the persisted row is returned.
func (r *Router) persistToolResult(evt provider.ProviderEvent, meta ToolResultMeta, diffData []byte, row store.Item, found bool) (store.Item, bool, error) {
	itemID := eventItemID(evt)
	if itemID == "" {
		return row, found, nil
	}

	now := eventTimestampMillis(evt)

	payloadID := ToolResultPayloadID(itemID)
	meta, diffData = r.mergeToolResultPayload(evt.ThreadID, payloadID, meta, diffData)
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return row, found, fmt.Errorf("marshal tool result meta: %w", err)
	}

	payload := store.Payload{
		ID:        payloadID,
		Kind:      toolResultPayloadKind,
		Meta:      string(metaJSON),
		Data:      diffData,
		CreatedAt: now,
	}
	summary := SummarizeToolResult(meta)
	if found {
		item := row
		item.PayloadID = payloadID
		item.Summary = summary
		item.UpdatedAt = now
		persisted, err := r.persistItemWithEmit(item, &payload, nil, true)
		if err != nil {
			return row, found, err
		}
		r.notifyDiffPayloadPersisted(evt.ThreadID, payloadID, meta, string(diffData))
		return persisted, true, nil
	}
	status := statusCompleted
	if evt.Kind == provider.EventToolComplete {
		status = CompletionStatus(DecodeToolCompleteMeta(evt.Meta))
	}

	turnIndex, err := r.turnIndexForEvent(evt)
	if err != nil {
		return row, found, fmt.Errorf("tool result turn index: %w", err)
	}
	newItem := store.Item{
		ID:        itemID,
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		Kind:      itemKindToolCall,
		Role:      "assistant",
		Status:    status,
		Summary:   summary,
		PayloadID: payloadID,
		ParentID:  eventParentID(evt),
		ToolName:  evt.ItemType,
		Decision:  r.takeApprovalDecision(evt.ThreadID, itemID),
		CreatedAt: now,
		UpdatedAt: now,
	}
	persisted, err := r.persistItemWithEmit(newItem, &payload, nil, true)
	if err != nil {
		return row, found, err
	}
	r.notifyDiffPayloadPersisted(evt.ThreadID, payloadID, meta, string(diffData))
	return persisted, true, nil
}

func captureCommandExecutionToolResult(raw json.RawMessage, workspaceRoot string) (ToolResultMeta, []byte, bool) {
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return ToolResultMeta{}, nil, false
	}

	item := asAnyMap(payload["item"])
	if item == nil {
		return ToolResultMeta{}, nil, false
	}

	command := extractRuntimeToolCommand(asAnyMap(item["data"]))
	if command == "" {
		return ToolResultMeta{}, nil, false
	}

	parsed := parseSupportedShellMutationCommand(command, workspaceRoot)
	if parsed == nil || hasDependentShellMutationPaths(parsed.Operations) {
		return ToolResultMeta{}, nil, false
	}

	captured, ok := captureShellMutationOperations(parsed.Operations, workspaceRoot)
	if !ok {
		return ToolResultMeta{}, nil, false
	}

	inlineDiff, unifiedDiff := buildCommandExecutionInlineDiffArtifact(captured)
	if inlineDiff == nil {
		return ToolResultMeta{}, nil, false
	}

	meta := ToolResultMeta{
		ItemType:   "command_execution",
		Title:      stringsx.FirstNonEmptyTrimmed(asTrimmedString(item["title"]), "Run command"),
		Detail:     parsed.NormalizedCommand,
		InlineDiff: inlineDiff,
	}
	meta.Preview = toolPreview(meta)
	return meta, []byte(unifiedDiff), true
}

func (r *Router) setPendingCommandInlineDiff(threadID, itemID string, pending pendingCommandInlineDiff) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.state(threadID)
	if st.pendingCommandDiffs == nil {
		st.pendingCommandDiffs = make(map[string]pendingCommandInlineDiff)
	}
	st.pendingCommandDiffs[itemID] = pending
}

func (r *Router) takePendingCommandInlineDiff(threadID, itemID string) (pendingCommandInlineDiff, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	st := r.threadStateIfPresent(threadID)
	if st == nil {
		return pendingCommandInlineDiff{}, false
	}
	pending, ok := st.pendingCommandDiffs[itemID]
	delete(st.pendingCommandDiffs, itemID)
	return pending, ok
}

func (r *Router) clearPendingCommandInlineDiff(threadID, itemID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.threadStateIfPresent(threadID); st != nil {
		delete(st.pendingCommandDiffs, itemID)
	}
}
