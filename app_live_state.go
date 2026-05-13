package main

import (
	"fmt"
	"strings"

	"agent-overflow/internal/flushqueue"
	"agent-overflow/internal/provider"
)

// ThreadLiveState is the backend-owned live projection a freshly loaded
// frontend needs after refresh/reconnect. SQLite remains the history cache;
// this shape carries only in-memory session state that should mirror what
// active provider processes are doing right now.
type ThreadLiveState struct {
	ThreadID     string                              `json:"threadId"`
	ActiveTurn   *LiveStateActiveTurn                `json:"activeTurn,omitempty"`
	QueueItems   []QueuedItem                        `json:"queueItems"`
	FlushedItems []QueueFlushedItem                  `json:"flushedItems"`
	Interactive  provider.PendingInteractiveRequests `json:"interactive"`
	Todo         *LiveStateTodo                      `json:"todo,omitempty"`
}

type LiveStateActiveTurn struct {
	ThreadID  string `json:"threadId"`
	TurnID    string `json:"turnId"`
	TurnIndex int    `json:"turnIndex"`
	StartedAt int64  `json:"startedAt"`
}

type LiveStateTodo struct {
	ThreadID  string              `json:"threadId"`
	Steps     []LiveStateTodoStep `json:"steps"`
	UpdatedAt int64               `json:"updatedAt"`
}

type LiveStateTodoStep struct {
	Step   string `json:"step"`
	Status string `json:"status"`
}

// GetThreadLiveState returns the current server-side live state for a thread.
// It is the refresh/reconnect companion to the push events emitted during a
// normal uninterrupted frontend session.
//
// LocalOnly: the payload can expose pending prompts, tool approvals, drafted
// queued messages, attachment ids, and provider session state. It belongs in
// the same loopback-only class as GetQueueState and ListPendingInteractiveRequests.
func (a *App) GetThreadLiveState(threadID string) (ThreadLiveState, error) {
	threadID = strings.TrimSpace(threadID)
	state := ThreadLiveState{
		ThreadID:     threadID,
		QueueItems:   []QueuedItem{},
		FlushedItems: []QueueFlushedItem{},
		Interactive: provider.PendingInteractiveRequests{
			Approvals:  []provider.ApprovalRequest{},
			UserInputs: []provider.UserInputRequest{},
		},
	}
	if threadID == "" {
		return state, fmt.Errorf("get thread live state: empty thread id")
	}
	if _, err := a.store.GetThread(threadID); err != nil {
		return state, fmt.Errorf("get thread live state: %w", err)
	}
	if a.triage == nil {
		return state, nil
	}

	live := a.triage.LiveStateSnapshotForThread(threadID)
	if live.ActiveTurn != nil {
		state.ActiveTurn = &LiveStateActiveTurn{
			ThreadID:  live.ActiveTurn.ThreadID,
			TurnID:    live.ActiveTurn.TurnID,
			TurnIndex: live.ActiveTurn.TurnIndex,
			StartedAt: live.ActiveTurn.StartedAt,
		}
	}
	if live.Todo != nil {
		steps := make([]LiveStateTodoStep, 0, len(live.Todo.Steps))
		for _, step := range live.Todo.Steps {
			steps = append(steps, LiveStateTodoStep{
				Step:   step.Step,
				Status: step.Status,
			})
		}
		state.Todo = &LiveStateTodo{
			ThreadID:  live.Todo.ThreadID,
			Steps:     steps,
			UpdatedAt: live.Todo.UpdatedAt,
		}
	}
	state.Interactive = live.Interactive
	for _, item := range live.QueueItems {
		state.QueueItems = append(state.QueueItems, flushqueue.ItemFromTriage(threadID, item))
	}
	for _, item := range live.FlushedItems {
		state.FlushedItems = append(state.FlushedItems, QueueFlushedItem{
			QueueItemID: item.QueueItemID,
			UserItemID:  item.UserItemID,
			Message:     item.Message,
		})
	}
	return state, nil
}
