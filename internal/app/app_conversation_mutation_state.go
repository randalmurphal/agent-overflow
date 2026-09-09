package app

import (
	"agent-overflow/internal/eventchan"
	"fmt"
)

// ConversationMutationState is a read barrier after an uncertain mutation.
// It waits for the thread's action lock before inspecting accepted work.
type ConversationMutationState struct {
	TurnStartedSequence   uint64 `json:"turnStartedSequence"`
	TurnCompletedSequence uint64 `json:"turnCompletedSequence"`
	UserItemExists        bool   `json:"userItemExists"`
	SendAccepted          bool   `json:"sendAccepted"`
	HistoryRev            int64  `json:"historyRev"`
	ItemEventSequence     uint64 `json:"itemEventSequence"`
}

// GetConversationMutationState reconciles a lost reply without replaying a send
// or assuming that reconnect means provider cleanup has finished.
//
//ao:scope threads:operate
func (a *App) GetConversationMutationState(threadID, userItemID, sendID string) (ConversationMutationState, error) {
	unlock := a.threadLocks().Lock(threadID)
	defer unlock()
	stamp, exists, err := a.store.ThreadHistoryStamp(threadID)
	if err != nil {
		return ConversationMutationState{}, fmt.Errorf("reconcile conversation: %w", err)
	}
	if !exists {
		return ConversationMutationState{}, fmt.Errorf("reconcile conversation: thread no longer exists")
	}
	_, exists, err = a.store.GetThreadItem(threadID, userItemID)
	if err != nil {
		return ConversationMutationState{}, err
	}
	_, accepted, err := a.findRecordedSend(threadID, sendID)
	if err != nil {
		return ConversationMutationState{}, err
	}
	return ConversationMutationState{TurnStartedSequence: a.eventSequence(eventchan.ProviderTurnStarted), TurnCompletedSequence: a.eventSequence(eventchan.ProviderTurnCompleted), UserItemExists: exists, SendAccepted: accepted, HistoryRev: stamp.Rev, ItemEventSequence: a.itemEventSequence()}, nil
}
