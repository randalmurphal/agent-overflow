package triage

import (
	"encoding/json"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

func (r *Router) handleContentBlockStart(evt provider.ProviderEvent) error {
	// Block starts are preserved from the provider so stop events can
	// deterministically settle the right streaming item, but the first
	// actual delta still owns item creation/index assignment.
	return nil
}

func (r *Router) handleContentBlockStop(evt provider.ProviderEvent) error {
	turnIndex, err := r.currentTurnIndex(evt.ThreadID)
	if err != nil {
		return err
	}
	scope := evt.ParentToolUseID
	switch r.blockTypeForStop(evt.ThreadID, turnIndex, scope, evt.Meta) {
	case "thinking":
		if signature := blockSignature(evt.Meta); signature != "" {
			_ = r.persistThinkingSignature(evt.ThreadID, turnIndex, scope, signature)
		}
		return r.settleStreamingThinking(evt.ThreadID, turnIndex, scope, statusCompleted)
	case "text":
		return r.settleStreamingText(evt.ThreadID, turnIndex, scope, statusCompleted)
	default:
		return nil
	}
}

func (r *Router) blockTypeForStop(threadID string, turnIndex int, scope string, raw json.RawMessage) string {
	if blockType := metaNestedString(raw, "blockType"); blockType != "" {
		return blockType
	}
	key := scopeCounterKey(threadID, turnIndex, scope)
	r.mu.Lock()
	defer r.mu.Unlock()
	// fall back to whichever block is currently active in the scope
	// (Claude content_block_stop omits the type on the wire).
	if r.activeThinkingBlocks[key] {
		return "thinking"
	}
	if r.activeTextBlocks[key] {
		return "text"
	}
	return ""
}

func blockSignature(raw json.RawMessage) string {
	if signature := metaNestedString(raw, "signature"); signature != "" {
		return signature
	}
	return metaNestedString(raw, "content_block", "signature")
}

func (r *Router) persistThinkingSignature(threadID string, turnIndex int, scope, signature string) error {
	itemID := thinkingItemID(turnIndex, scope, r.blockIndex(threadID, turnIndex, scope))
	item, found, err := r.store.GetThreadItem(threadID, itemID)
	if err != nil || !found {
		return err
	}
	itemMeta := map[string]any{}
	if item.Meta != "" && item.Meta != "{}" {
		_ = json.Unmarshal([]byte(item.Meta), &itemMeta)
	}
	itemMeta["signature"] = signature
	data, err := json.Marshal(itemMeta)
	if err != nil {
		return err
	}
	item.Meta = string(data)

	var payload *store.Payload
	if item.PayloadID != "" {
		metaRow, metaErr := r.store.GetPayloadMeta(item.PayloadID)
		if metaErr == nil {
			var payloadMeta ThinkingMeta
			_ = json.Unmarshal([]byte(metaRow.Meta), &payloadMeta)
			payloadMeta.Signature = signature
			payloadMetaJSON, marshalErr := json.Marshal(payloadMeta)
			if marshalErr != nil {
				return marshalErr
			}
			dataBytes, dataErr := r.store.GetPayloadData(item.PayloadID)
			if dataErr != nil {
				return dataErr
			}
			payload = &store.Payload{
				ID:        item.PayloadID,
				Kind:      metaRow.Kind,
				Meta:      string(payloadMetaJSON),
				Data:      dataBytes,
				CreatedAt: item.CreatedAt,
			}
		}
	}

	if err := r.persistItem(item, payload); err != nil {
		return err
	}
	return nil
}
