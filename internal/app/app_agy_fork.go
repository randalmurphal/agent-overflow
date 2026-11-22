package app

import (
	"fmt"

	"agent-overflow/internal/provider/agy"
	"agent-overflow/internal/store"
)

// forkAgyThread creates a native Antigravity conversation clone using SQLite
// VACUUM INTO so the child thread starts with its own independent conversation
// database. When atTurnIndex is nil, the full conversation is cloned; when set,
// steps from the first dropped user turn onward are sliced off.
func (a *App) forkAgyThread(source store.Thread, atTurnIndex *int) (forkResumeState, error) {
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
			if item.Kind != "user_text" || item.Role != "user" || store.IsWireOnlyUserItem(item) {
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
		if item.Kind != "user_text" || item.Role != "user" || store.IsWireOnlyUserItem(item) {
			continue
		}
		ordinal++
	}
	return ordinal, nil
}
