package main

import (
	"context"
	"fmt"
	"time"

	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

func (a *App) sendMessage(threadID string, content string) error {
	if a.sendMessageFn != nil {
		return a.sendMessageFn(threadID, content)
	}

	a.mu.Lock()
	sess, ok := a.sessions[threadID]
	if !ok {
		a.mu.Unlock()
		return fmt.Errorf("no active session for thread %s", threadID)
	}

	// Hold the mutex across the turn-index read/update so concurrent sends do
	// not persist duplicate user item positions.
	turnIndex, err := a.store.LastTurnIndex(threadID)
	if err != nil {
		a.mu.Unlock()
		return fmt.Errorf("send message: get turn index: %w", err)
	}
	turnIndex++

	now := time.Now().UnixMilli()
	userItem := store.Item{
		ID:        uuid.New().String(),
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		ItemIndex: 0,
		Kind:      "text",
		Role:      "user",
		Summary:   content,
		CreatedAt: now,
	}
	if err := a.store.InsertItem(userItem); err != nil {
		a.mu.Unlock()
		return fmt.Errorf("send message: persist user message: %w", err)
	}
	a.mu.Unlock()

	switch {
	case sess.claude != nil:
		return sess.claude.Send(context.Background(), content)
	case sess.codex != nil:
		return sess.codex.Send(context.Background(), content)
	default:
		return fmt.Errorf("session has no provider")
	}
}
