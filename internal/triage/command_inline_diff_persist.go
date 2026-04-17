package triage

import (
	"encoding/json"
	"fmt"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
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

	thread, err := r.store.GetThread(evt.ThreadID)
	if err != nil {
		return fmt.Errorf("lookup thread for command inline diff: %w", err)
	}

	meta, diffData, ok := captureCommandExecutionToolResult(evt.Meta, thread.WorkspacePath)
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

func (r *Router) persistCommandInlineDiffToolResult(evt provider.ProviderEvent) error {
	if evt.ItemType != "command_execution" || evt.ItemID == "" {
		return nil
	}

	pending, ok := r.takePendingCommandInlineDiff(evt.ThreadID, evt.ItemID)
	if !ok || extractRuntimeCommandExitCode(evt.Meta) != 0 {
		return nil
	}

	return r.persistToolResult(evt, pending.Meta, pending.DiffData)
}

func (r *Router) persistToolResult(evt provider.ProviderEvent, meta ToolResultMeta, diffData []byte) error {
	if evt.ItemID == "" {
		return nil
	}

	now := evt.Timestamp.UnixMilli()
	if now == 0 {
		now = time.Now().UnixMilli()
	}

	turnIndex, err := r.store.LastTurnIndex(evt.ThreadID)
	if err != nil {
		return fmt.Errorf("tool result turn index: %w", err)
	}

	payloadID := toolResultPayloadID(evt.ItemID)
	meta, diffData = r.mergeToolResultPayload(payloadID, meta, diffData)
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal tool result meta: %w", err)
	}

	payload := store.Payload{
		ID:        payloadID,
		Kind:      toolResultPayloadKind,
		Meta:      string(metaJSON),
		Data:      diffData,
		CreatedAt: now,
	}
	if err := r.store.UpsertPayload(payload); err != nil {
		return fmt.Errorf("persist tool result payload: %w", err)
	}
	r.emitPayloadMeta(payloadID, evt.ThreadID, toolResultPayloadKind, string(metaJSON), now)

	item, found, err := r.store.GetItem(evt.ItemID)
	if err != nil {
		return fmt.Errorf("lookup tool result item: %w", err)
	}
	summary := summarizeToolResult(meta)
	if found {
		return r.store.UpdateItemPayload(item.ID, payloadID, summary, now)
	}

	_, err = r.store.AppendItem(store.Item{
		ID:        evt.ItemID,
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		Kind:      toolResultItemKind,
		Role:      "assistant",
		Summary:   summary,
		PayloadID: payloadID,
		CreatedAt: now,
	})
	return err
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
		Title:      firstNonEmpty(asTrimmedString(item["title"]), "Run command"),
		Detail:     parsed.NormalizedCommand,
		InlineDiff: inlineDiff,
	}
	meta.Preview = toolPreview(meta)
	return meta, []byte(unifiedDiff), true
}

func (r *Router) setPendingCommandInlineDiff(threadID, itemID string, pending pendingCommandInlineDiff) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pendingCommandDiffs[pendingCommandInlineDiffKey(threadID, itemID)] = pending
}

func (r *Router) takePendingCommandInlineDiff(threadID, itemID string) (pendingCommandInlineDiff, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := pendingCommandInlineDiffKey(threadID, itemID)
	pending, ok := r.pendingCommandDiffs[key]
	delete(r.pendingCommandDiffs, key)
	return pending, ok
}

func (r *Router) clearPendingCommandInlineDiff(threadID, itemID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.pendingCommandDiffs, pendingCommandInlineDiffKey(threadID, itemID))
}

func pendingCommandInlineDiffKey(threadID, itemID string) string {
	return threadID + ":" + itemID
}
