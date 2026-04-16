package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

// sendMu serialises the read-turn-index / insert-item sequence so concurrent
// sends on different threads don't block unrelated App operations behind a.mu.
// SQLite itself serialises the INSERT (SetMaxOpenConns(1)), but this mutex
// keeps the read+write atomic from our side.
var sendMu sync.Mutex

func (a *App) sendMessage(threadID string, content string) error {
	if a.sendMessageFn != nil {
		return a.sendMessageFn(threadID, content)
	}

	// Grab the session reference under a.mu (fast), then release immediately.
	a.mu.Lock()
	sess, ok := a.sessions[threadID]
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("no active session for thread %s", threadID)
	}

	// DB operations happen outside a.mu so other App methods are not blocked.
	sendMu.Lock()
	turnIndex, err := a.store.LastTurnIndex(threadID)
	if err != nil {
		sendMu.Unlock()
		return fmt.Errorf("send message: get turn index: %w", err)
	}
	turnIndex++
	a.maybeRenameTemporaryWorktreeBranch(threadID, content)

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
		sendMu.Unlock()
		return fmt.Errorf("send message: persist user message: %w", err)
	}
	sendMu.Unlock()

	switch {
	case sess.claude != nil:
		return sess.claude.Send(context.Background(), content)
	case sess.codex != nil:
		return sess.codex.Send(context.Background(), content)
	default:
		log.Printf("send message: session for thread %s has no provider", threadID)
		return fmt.Errorf("session has no provider")
	}
}
