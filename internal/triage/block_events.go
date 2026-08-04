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
	switch r.blockTypeForStop(evt.ThreadID, turnIndex, scope, evt.ItemID, evt.Meta) {
	case "thinking":
		r.settleStreamingThinkingAsync(evt.ThreadID, turnIndex, scope, evt.ItemID, statusCompleted, evt.Content, evt.ContentPresent)
		return nil
	case "text":
		r.settleStreamingTextAsync(evt.ThreadID, turnIndex, scope, evt.ItemID, statusCompleted, evt.Content, evt.ContentPresent)
		return nil
	default:
		return nil
	}
}

func (r *Router) blockTypeForStop(threadID string, turnIndex int, scope, providerItemID string, raw json.RawMessage) string {
	if blockType := metaNestedString(raw, "blockType"); blockType != "" {
		return blockType
	}
	key := activeStreamKey(threadID, turnIndex, scope, providerItemID)
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
	if providerItemID != "" {
		return ""
	}
	for key, ref := range r.activeThinkingBlockRefs {
		if ref.threadID == threadID && ref.turnIndex == turnIndex && ref.scope == scope && r.activeThinkingBlocks[key] {
			return "thinking"
		}
	}
	for key, ref := range r.activeTextBlockRefs {
		if ref.threadID == threadID && ref.turnIndex == turnIndex && ref.scope == scope && r.activeTextBlocks[key] {
			return "text"
		}
	}
	return ""
}
