package app

import (
	"encoding/json"
	"fmt"
	"strings"

	"agent-overflow/internal/provider/agy"
	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// isAgyNativePromptItem reports whether item corresponds to a native
// Antigravity USER_INPUT step (step_type=14) in the conversation SQLite DB:
// either a top-level completed visible user_text row or a top-level resolved
// AskUserQuestion whose answer dispatched a follow-up session/prompt.
func isAgyNativePromptItem(item store.Item) bool {
	if strings.TrimSpace(item.ParentID) != "" {
		return false
	}
	if item.Kind == "user_text" && item.Role == "user" && !store.IsWireOnlyUserItem(item) {
		return item.Status == "" || item.Status == "completed"
	}
	if item.Kind == "tool_call" && strings.TrimSpace(item.ToolName) == "AskUserQuestion" && item.Meta != "" {
		var meta struct {
			PromptDispatched bool `json:"prompt_dispatched"`
		}
		if json.Unmarshal([]byte(item.Meta), &meta) == nil && meta.PromptDispatched {
			return true
		}
	}
	return false
}

// agyPromptHintFromItem builds the content/media/timestamp hint used to match
// an AO prompt item against agy's steps table when a stored native step anchor
// is absent.
func agyPromptHintFromItem(item store.Item) agy.UserPromptHint {
	if item.Kind == "tool_call" && strings.TrimSpace(item.ToolName) == "AskUserQuestion" {
		return agy.UserPromptHint{
			CreatedAtMs: item.CreatedAt,
			IsQuestion:  true,
		}
	}
	hint := agy.UserPromptHint{
		Text:        item.Summary,
		CreatedAtMs: item.CreatedAt,
	}
	if meta, err := usermessage.FromItem(item); err == nil {
		if len(meta.Attachments) > 0 {
			hint.HasMedia = true
		}
		if strings.TrimSpace(meta.Command) != "" {
			hint.IsExpandedCommand = true
		}
	}
	return hint
}

// agyStoredStepAnchor returns the native step anchor stamped onto userItem's
// Meta (provider_parent_uuid) or its MessageAnchor copy. Any value carrying
// the "agy-step:" prefix is returned verbatim (so malformed agy anchors fail
// loudly inside ForkConversationBeforeTarget), while unrelated provider UUIDs
// (for example Claude parent UUIDs on a thread switched to agy) are ignored.
func agyStoredStepAnchor(anchor store.MessageAnchor, userItem store.Item) string {
	if raw := strings.TrimSpace(usermessage.ReadProviderParentUUID(userItem.Meta)); strings.HasPrefix(raw, "agy-step:") {
		return raw
	}
	if raw := strings.TrimSpace(anchor.ProviderParentUUID); strings.HasPrefix(raw, "agy-step:") {
		return raw
	}
	return ""
}

// buildAgyCutTarget builds the native Antigravity cut target for targetItemID
// within threadID's timeline items. It extracts any stored native step anchor
// ("agy-step:<idx>:<hash>") plus immediate prev/next prompt context and counts.
func (a *App) buildAgyCutTarget(threadID string, items []store.Item, anchor store.MessageAnchor, targetItemID string) (agy.UserPromptCutTarget, error) {
	prompts := make([]store.Item, 0, len(items))
	targetPos := -1
	for _, item := range items {
		if item.ID == targetItemID {
			targetPos = len(prompts)
			prompts = append(prompts, item)
			continue
		}
		if !isAgyNativePromptItem(item) {
			continue
		}
		prompts = append(prompts, item)
	}
	if targetPos < 0 {
		return agy.UserPromptCutTarget{}, fmt.Errorf("target user message %s not found in thread %s", targetItemID, threadID)
	}
	targetItem := prompts[targetPos]
	storedAnchor := agyStoredStepAnchor(anchor, targetItem)
	if storedAnchor == "" && anchor.UserItemID == "" {
		if dbAnchor, found, err := a.store.GetMessageAnchor(threadID, targetItem.ID); err == nil && found {
			storedAnchor = agyStoredStepAnchor(dbAnchor, targetItem)
		}
	}
	cut := agy.UserPromptCutTarget{
		Anchor:        storedAnchor,
		Target:        agyPromptHintFromItem(targetItem),
		PromptsBefore: targetPos,
		PromptsAfter:  len(prompts) - 1 - targetPos,
	}
	if targetPos > 0 {
		prev := agyPromptHintFromItem(prompts[targetPos-1])
		cut.Prev = &prev
	}
	if targetPos+1 < len(prompts) {
		next := agyPromptHintFromItem(prompts[targetPos+1])
		cut.Next = &next
	}
	return cut, nil
}

// forkAgyThread creates a native Antigravity conversation clone using SQLite
// VACUUM INTO so the child thread starts with its own independent conversation
// database. When atTurnIndex is set, steps from the first dropped user prompt
// onward are resolved by native step anchor (or exact content/neighbor match)
// and sliced inside the cloned snapshot; for a live tail fork
// (atTurnIndex == nil), preCutMaxIdx (captured before CreatePointerFork) pins
// the native step ceiling so output appended while committing the pointer fork
// cannot leak into the cloned DB.
func (a *App) forkAgyThread(source store.Thread, forkID string, atTurnIndex *int, preCutMaxIdx *int) (forkResumeState, error) {
	if source.SessionRef == "" {
		items, err := a.store.ListItems(source.ID)
		if err != nil {
			return forkResumeState{}, fmt.Errorf("fork agy thread: list items: %w", err)
		}
		for _, item := range items {
			if isAgyNativePromptItem(item) {
				return forkResumeState{}, fmt.Errorf("fork agy thread: source thread %q has conversation history but no Antigravity session ref", source.ID)
			}
		}
		return forkResumeState{}, nil
	}
	appDataDir := a.agyAppDataDir()
	if atTurnIndex != nil {
		items, err := a.store.ListItems(source.ID)
		if err != nil {
			return forkResumeState{}, fmt.Errorf("fork agy thread: list items: %w", err)
		}
		var dropItem *store.Item
		for i := range items {
			if !isAgyNativePromptItem(items[i]) {
				continue
			}
			if items[i].TurnIndex > *atTurnIndex {
				dropItem = &items[i]
				break
			}
		}
		if dropItem != nil {
			target, err := a.buildAgyCutTarget(source.ID, items, store.MessageAnchor{}, dropItem.ID)
			if err != nil {
				return forkResumeState{}, fmt.Errorf("fork agy thread: %w", err)
			}
			if target.PromptsBefore == 0 {
				return forkResumeState{}, nil
			}
			newSessionID, cleanup, err := agy.ForkConversationBeforeTarget(appDataDir, source.SessionRef, target)
			if err != nil {
				return forkResumeState{}, fmt.Errorf("fork agy thread: %w", err)
			}
			return forkResumeState{
				SessionRef: newSessionID,
				Cleanup:    cleanup,
			}, nil
		}
	}
	cutIdx := agy.CutKeepAll
	if preCutMaxIdx != nil {
		if *preCutMaxIdx < 0 {
			// Verify no completed native prompts exist before returning an
			// empty SessionRef: if the thread already has history, a missing
			// conversation DB on disk must fail rather than silently wiping
			// conversation context on the fork.
			checkThreadID := source.ID
			if strings.TrimSpace(forkID) != "" {
				checkThreadID = forkID
			}
			items, listErr := a.store.ListItems(checkThreadID)
			if listErr != nil {
				return forkResumeState{}, fmt.Errorf("fork agy thread: list items: %w", listErr)
			}
			for _, item := range items {
				if isAgyNativePromptItem(item) {
					return forkResumeState{}, fmt.Errorf("fork agy thread: source thread %q has conversation history but no Antigravity steps on disk", source.ID)
				}
			}
			// Source session has no committed steps on disk yet (e.g. forked
			// before Turn 1 wrote to SQLite); start the fork fresh.
			return forkResumeState{}, nil
		}
		cutIdx = *preCutMaxIdx
	} else if strings.TrimSpace(forkID) != "" {
		// Fallback when no pre-cut max step index was provided: check whether
		// the committed pointer fork has any native prompts and whether source
		// grew past the fork's last prompt.
		forkItems, err := a.store.ListItems(forkID)
		if err != nil {
			return forkResumeState{}, fmt.Errorf("fork agy thread: list committed fork items: %w", err)
		}
		keptPromptIDs := make(map[string]struct{}, len(forkItems))
		for _, item := range forkItems {
			if isAgyNativePromptItem(item) {
				keptPromptIDs[item.ID] = struct{}{}
			}
		}
		if len(keptPromptIDs) == 0 {
			return forkResumeState{}, nil
		}
		sourceItems, err := a.store.ListItems(source.ID)
		if err != nil {
			return forkResumeState{}, fmt.Errorf("fork agy thread: list source items: %w", err)
		}
		for i := range sourceItems {
			if !isAgyNativePromptItem(sourceItems[i]) {
				continue
			}
			if _, kept := keptPromptIDs[sourceItems[i].ID]; !kept {
				target, targetErr := a.buildAgyCutTarget(source.ID, sourceItems, store.MessageAnchor{}, sourceItems[i].ID)
				if targetErr != nil {
					return forkResumeState{}, fmt.Errorf("fork agy thread: %w", targetErr)
				}
				newSessionID, cleanup, forkErr := agy.ForkConversationBeforeTarget(appDataDir, source.SessionRef, target)
				if forkErr != nil {
					return forkResumeState{}, fmt.Errorf("fork agy thread: %w", forkErr)
				}
				return forkResumeState{
					SessionRef: newSessionID,
					Cleanup:    cleanup,
				}, nil
			}
		}
	}
	newSessionID, cleanup, err := agy.ForkConversation(appDataDir, source.SessionRef, cutIdx)
	if err != nil {
		return forkResumeState{}, fmt.Errorf("fork agy thread: %w", err)
	}
	return forkResumeState{
		SessionRef: newSessionID,
		Cleanup:    cleanup,
	}, nil
}

// forkAgyThreadBeforeMessage clones an Antigravity conversation cut strictly
// before the anchored user message item (resolving the cut inside the cloned
// snapshot in a single transaction by stored native step anchor or exact
// content/neighbor matching).
func (a *App) forkAgyThreadBeforeMessage(source store.Thread, anchor store.MessageAnchor, anchorItem store.Item) (forkResumeState, error) {
	items, err := a.store.ListItems(source.ID)
	if err != nil {
		return forkResumeState{}, fmt.Errorf("fork agy thread from message: list items: %w", err)
	}
	target, err := a.buildAgyCutTarget(source.ID, items, anchor, anchorItem.ID)
	if err != nil {
		return forkResumeState{}, fmt.Errorf("fork agy thread from message: %w", err)
	}
	if target.PromptsBefore == 0 {
		return forkResumeState{}, nil
	}
	if source.SessionRef == "" {
		return forkResumeState{}, fmt.Errorf("fork agy thread from message: source thread %q has prior history but no Antigravity session ref", source.ID)
	}
	appDataDir := a.agyAppDataDir()
	newSessionID, cleanup, err := agy.ForkConversationBeforeTarget(appDataDir, source.SessionRef, target)
	if err != nil {
		return forkResumeState{}, fmt.Errorf("fork agy thread from message: %w", err)
	}
	return forkResumeState{
		SessionRef: newSessionID,
		Cleanup:    cleanup,
	}, nil
}

// rollbackAgyThreadToMessage cuts an Antigravity thread's conversation before
// userItem by cloning and slicing the conversation DB and repointing SessionRef.
func (a *App) rollbackAgyThreadToMessage(thread store.Thread, anchor store.MessageAnchor, userItem store.Item) error {
	a.dropFlushQueue(thread.ID)
	latest, err := a.store.GetThread(thread.ID)
	if err != nil {
		return fmt.Errorf("agy rollback: reload thread %s: %w", thread.ID, err)
	}
	thread = latest
	items, err := a.store.ListItems(thread.ID)
	if err != nil {
		return fmt.Errorf("agy rollback: list items: %w", err)
	}
	target, err := a.buildAgyCutTarget(thread.ID, items, anchor, userItem.ID)
	if err != nil {
		return fmt.Errorf("agy rollback: %w", err)
	}
	if target.PromptsBefore == 0 {
		return a.writeSessionRef(thread.ID, "")
	}
	if thread.SessionRef == "" {
		return fmt.Errorf("agy rollback: thread %s has prior history but no Antigravity session ref", thread.ID)
	}
	appDataDir := a.agyAppDataDir()
	newSessionID, cleanup, err := agy.ForkConversationBeforeTarget(appDataDir, thread.SessionRef, target)
	if err != nil {
		return fmt.Errorf("agy rollback: clone sliced conversation: %w", err)
	}
	if err := a.writeSessionRef(thread.ID, newSessionID); err != nil {
		_ = cleanup()
		return fmt.Errorf("agy rollback: persist rolled-back session ref: %w", err)
	}
	return nil
}
