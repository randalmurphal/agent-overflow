package triage

import (
	"encoding/json"

	"agent-overflow/internal/provider"
)

func (r *Router) handleContentBlockStart(evt provider.ProviderEvent) error {
	// Parent-content resume re-round (Claude 2.1.154+): a parent
	// content block starting while the logical turn is already settled
	// and no round is open means the model emitted a soft round-close
	// (parent message_delta stop_reason) and is now resuming the SAME
	// turn with a fresh message — no intervening result/system.init. The
	// soft-close already cleared the working indicator; re-open the wire
	// round so it lights back up. Gated on parent_tool_use_id == "" so
	// subagent content during a legitimate local_agent-outlives wait
	// (invariant 27) never re-arms the parent's round. The settled +
	// no-open-round guards inside maybeReopenSettledRound make this a
	// no-op for ordinary mid-round block starts. eventParentID trims so
	// a whitespace-only parent id still reads as a parent resume — the
	// same canonical parent-scoping the sibling handleContentBlockStop
	// and turnCountsAsThreadActivity use.
	if eventParentID(evt) == "" {
		r.maybeReopenSettledRound(evt)
	}
	// Block starts are otherwise preserved from the provider so stop
	// events can deterministically settle the right streaming item, but
	// the first actual delta still owns item creation/index assignment.
	return nil
}

func (r *Router) handleContentBlockStop(evt provider.ProviderEvent) error {
	turnIndex, err := r.turnIndexForEvent(evt)
	if err != nil {
		return err
	}
	scope := eventParentID(evt)
	// content_block_stop is the freeze hot path: a thinking block ending
	// or a text block ending fires SQLite-write-heavy settle work that
	// would otherwise stall the provider read-loop. Async dispatch lets
	// the next provider event flow immediately; settleTurnStreaming at
	// turn boundary still waits on every in-flight scope before the
	// turns row commits.
	// ONE decode of the stop envelope for both readers below it: the block
	// type and the delivery marker are two top-level strings on the same
	// object, and this is the freeze hot path.
	stop := decodeBlockStopMeta(evt.Meta)
	switch r.blockTypeForStop(evt.ThreadID, turnIndex, scope, evt.ItemID, stop.BlockType) {
	case "thinking":
		r.settleStreamingThinkingAsync(evt.ThreadID, turnIndex, scope, evt.ItemID, statusCompleted, evt.Content, evt.ContentPresent)
		return nil
	case "text":
		r.settleStreamingTextAsync(evt.ThreadID, turnIndex, scope, evt.ItemID, statusCompleted, evt.Content, evt.ContentPresent, blockDeliveryMeta(stop.Delivery))
		return nil
	default:
		return nil
	}
}

// blockStopMeta is the content_block_stop envelope's two routing fields.
type blockStopMeta struct {
	// BlockType is the wire's own `blockType`, empty on Claude (which omits
	// it) — blockTypeForStop then falls back to the active-block maps.
	BlockType string `json:"blockType"`
	// Delivery is Codex's `delivery` marker; see blockDeliveryMeta.
	Delivery string `json:"delivery"`
}

func decodeBlockStopMeta(raw json.RawMessage) blockStopMeta {
	if len(raw) == 0 {
		return blockStopMeta{}
	}
	var meta blockStopMeta
	if json.Unmarshal(raw, &meta) != nil {
		return blockStopMeta{}
	}
	return meta
}

// blockDeliveryMeta lifts the stop envelope's `delivery` onto row meta.
// Codex stamps `delivery: "async"` on an agentMessage the model sent
// mid-turn via send_user_message_async (0.149+); the frontend renders that
// row as an interim note rather than the turn's answer. Absent on every
// other block, so nil keeps the persisted meta byte-identical to before.
func blockDeliveryMeta(delivery string) json.RawMessage {
	if delivery == "" {
		return nil
	}
	out, err := json.Marshal(map[string]string{"delivery": delivery})
	if err != nil {
		return nil
	}
	return out
}

func (r *Router) blockTypeForStop(threadID string, turnIndex int, scope, providerItemID, blockType string) string {
	if blockType != "" {
		return blockType
	}
	key := activeStreamKey(turnIndex, scope, providerItemID)
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.threadStateIfPresent(threadID)
	if st == nil {
		return ""
	}
	// fall back to whichever block is currently active in the scope
	// (Claude content_block_stop omits the type on the wire).
	if st.activeThinkingBlocks[key] {
		return "thinking"
	}
	if st.activeTextBlocks[key] {
		return "text"
	}
	if providerItemID != "" {
		return ""
	}
	for key, ref := range st.activeThinkingBlockRefs {
		if ref.turnIndex == turnIndex && ref.scope == scope && st.activeThinkingBlocks[key] {
			return "thinking"
		}
	}
	for key, ref := range st.activeTextBlockRefs {
		if ref.turnIndex == turnIndex && ref.scope == scope && st.activeTextBlocks[key] {
			return "text"
		}
	}
	return ""
}
