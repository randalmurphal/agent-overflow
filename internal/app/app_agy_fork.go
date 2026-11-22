package app

import (
	"encoding/json"
	"fmt"
	"strings"

	"agent-overflow/internal/provider/agy"
	"agent-overflow/internal/store"
)

// isAgyNativePromptItem reports whether item corresponds to a native
// Antigravity USER_INPUT step (step_type=14) in the conversation SQLite DB:
// either a visible user_text row or a resolved AskUserQuestion whose answer
// dispatched a follow-up session/prompt.
func isAgyNativePromptItem(item store.Item) bool {
	if item.Kind == "user_text" && item.Role == "user" && !store.IsWireOnlyUserItem(item) {
		return true
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

// forkAgyThread creates a native Antigravity conversation clone using SQLite
// VACUUM INTO so the child thread starts with its own independent conversation
// database. When atTurnIndex is set, steps from the first dropped user turn
// onward are sliced off; for a live tail fork (atTurnIndex == nil) with a
// committed pointer forkID, steps beyond the pointer fork's committed prompt
// count are sliced off so a concurrently streaming source cannot leak into the
// fork.
func (a *App) forkAgyThread(source store.Thread, forkID string, atTurnIndex *int) (forkResumeState, error) {
	if source.SessionRef == "" {
		return forkResumeState{}, nil
	}
	appDataDir := a.agyAppDataDir()
	cutIdx := -1
	if atTurnIndex != nil {
		items, err := a.store.ListItems(source.ID)
		if err != nil {
			return forkResumeState{}, fmt.Errorf("fork agy thread: list items: %w", err)
		}
		userOrdinal := 0
		foundDrop := false
		for _, item := range items {
			if !isAgyNativePromptItem(item) {
				continue
			}
			if item.TurnIndex > *atTurnIndex {
				foundDrop = true
				break
			}
			userOrdinal++
		}
		if foundDrop {
			if userOrdinal == 0 {
				return forkResumeState{}, nil
			}
			idx, hasPrefix, err := agy.FindCutIndexBeforeUserTurn(appDataDir, source.SessionRef, userOrdinal)
			if err != nil {
				return forkResumeState{}, fmt.Errorf("fork agy thread: locate cut index: %w", err)
			}
			if !hasPrefix {
				return forkResumeState{}, nil
			}
			cutIdx = idx
		}
	} else if strings.TrimSpace(forkID) != "" {
		// Bound live tail forks to the exact prompts committed into the
		// pointer fork so turns appended to the streaming source after
		// CreatePointerFork cannot leak into the cloned native DB.
		forkItems, err := a.store.ListItems(forkID)
		if err != nil {
			return forkResumeState{}, fmt.Errorf("fork agy thread: list committed fork items: %w", err)
		}
		committedPrompts := 0
		for _, item := range forkItems {
			if isAgyNativePromptItem(item) {
				committedPrompts++
			}
		}
		if committedPrompts > 0 {
			idx, hasPrefix, err := agy.FindCutIndexBeforeUserTurn(appDataDir, source.SessionRef, committedPrompts)
			if err != nil {
				return forkResumeState{}, fmt.Errorf("fork agy thread: locate live tail cut index: %w", err)
			}
			if !hasPrefix {
				return forkResumeState{}, nil
			}
			cutIdx = idx
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
// before the anchored user message item.
func (a *App) forkAgyThreadBeforeMessage(source store.Thread, _ store.MessageAnchor, anchorItem store.Item) (forkResumeState, error) {
	if source.SessionRef == "" {
		return forkResumeState{}, nil
	}
	userOrdinal, err := a.agyUserOrdinalBeforeItem(source.ID, anchorItem.ID)
	if err != nil {
		return forkResumeState{}, fmt.Errorf("fork agy thread from message: %w", err)
	}
	if userOrdinal == 0 {
		return forkResumeState{}, nil
	}
	appDataDir := a.agyAppDataDir()
	cutIdx, hasPrefix, err := agy.FindCutIndexBeforeUserTurn(appDataDir, source.SessionRef, userOrdinal)
	if err != nil {
		return forkResumeState{}, fmt.Errorf("fork agy thread from message: locate cut index: %w", err)
	}
	if !hasPrefix {
		return forkResumeState{}, nil
	}
	newSessionID, cleanup, err := agy.ForkConversation(appDataDir, source.SessionRef, cutIdx)
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
func (a *App) rollbackAgyThreadToMessage(thread store.Thread, _ store.MessageAnchor, userItem store.Item) error {
	a.dropFlushQueue(thread.ID)
	if thread.SessionRef == "" {
		return a.writeSessionRef(thread.ID, "")
	}
	userOrdinal, err := a.agyUserOrdinalBeforeItem(thread.ID, userItem.ID)
	if err != nil {
		return fmt.Errorf("agy rollback: %w", err)
	}
	if userOrdinal == 0 {
		return a.writeSessionRef(thread.ID, "")
	}
	appDataDir := a.agyAppDataDir()
	cutIdx, hasPrefix, err := agy.FindCutIndexBeforeUserTurn(appDataDir, thread.SessionRef, userOrdinal)
	if err != nil {
		return fmt.Errorf("agy rollback: locate cut index: %w", err)
	}
	if !hasPrefix {
		return a.writeSessionRef(thread.ID, "")
	}
	newSessionID, cleanup, err := agy.ForkConversation(appDataDir, thread.SessionRef, cutIdx)
	if err != nil {
		return fmt.Errorf("agy rollback: clone sliced conversation: %w", err)
	}
	if err := a.writeSessionRef(thread.ID, newSessionID); err != nil {
		_ = cleanup()
		return fmt.Errorf("agy rollback: persist rolled-back session ref: %w", err)
	}
	return nil
}

func (a *App) agyUserOrdinalBeforeItem(threadID, targetItemID string) (int, error) {
	items, err := a.store.ListItems(threadID)
	if err != nil {
		return 0, err
	}
	ordinal := 0
	for _, item := range items {
		if item.ID == targetItemID {
			return ordinal, nil
		}
		if !isAgyNativePromptItem(item) {
			continue
		}
		ordinal++
	}
	return ordinal, nil
}
