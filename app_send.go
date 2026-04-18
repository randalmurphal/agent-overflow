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

// sendThreadMuRegistry owns a mutex per thread so the "compute turn index
// / call provider / persist user item" sequence can't interleave for the
// same thread (Bug B11). Concurrent sends on DIFFERENT threads proceed
// in parallel — that's the whole point of splitting the lock from a
// global sendMu. Also avoids the audit #52 misattribution where two
// in-flight sends on one thread both attributed assistant replies to
// max(turn_index) instead of the turn that actually spoke.
var sendThreadMuRegistry = &threadMutexRegistry{
	mus: make(map[string]*sync.Mutex),
}

type threadMutexRegistry struct {
	mu  sync.Mutex
	mus map[string]*sync.Mutex
}

// lockFor returns an unlock function that must be called once the
// per-thread critical section completes. The registry caches one mutex
// per thread for the life of the process; threads come and go, but the
// memory footprint is tiny (one small struct per thread).
func (r *threadMutexRegistry) lockFor(threadID string) func() {
	r.mu.Lock()
	m, ok := r.mus[threadID]
	if !ok {
		m = &sync.Mutex{}
		r.mus[threadID] = m
	}
	r.mu.Unlock()
	m.Lock()
	return m.Unlock
}

func (a *App) sendMessage(threadID string, content string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	if a.sendMessageFn != nil {
		return a.sendMessageFn(threadID, content)
	}

	// Per-thread critical section: only one Send per thread at a time.
	// This keeps the lazy-session-start + read-turn-index + call-provider +
	// insert-user-item sequence atomic for a single thread while letting
	// different threads proceed in parallel. Held across the lazy start so
	// concurrent sends on a session-less thread don't race on spawn
	// ordering.
	unlock := sendThreadMuRegistry.lockFor(threadID)
	defer unlock()

	// A thread is session-less until the first message is sent — we don't
	// spawn the provider subprocess at thread creation. Lazy-start here so
	// the user's "new thread → type → send" flow works without an explicit
	// Start step. runSessionStart dedupes with any in-flight start kicked
	// off by SwitchThread auto-resume.
	a.mu.Lock()
	sess, ok := a.sessions[threadID]
	a.mu.Unlock()
	if !ok {
		if err := a.startSession(threadID); err != nil {
			return fmt.Errorf("send message: start session: %w", err)
		}
		a.mu.Lock()
		sess, ok = a.sessions[threadID]
		a.mu.Unlock()
		if !ok {
			return fmt.Errorf("send message: session unavailable after start for thread %s", threadID)
		}
	}

	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return fmt.Errorf("send message: get thread: %w", err)
	}
	hasPriorItems, err := a.store.HasItems(threadID)
	if err != nil {
		return fmt.Errorf("send message: check prior items: %w", err)
	}
	turnIndex, err := a.store.LastTurnIndex(threadID)
	if err != nil {
		return fmt.Errorf("send message: get turn index: %w", err)
	}
	turnIndex++
	a.maybeRenameTemporaryWorktreeBranch(threadID, content)

	// Bug B8: write to the provider FIRST; a failing Send must not
	// leave an orphan user item on a turn that never ran.
	if err := sendToProvider(sess, threadID, content); err != nil {
		return err
	}

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
		return fmt.Errorf("send message: persist user message: %w", err)
	}
	a.maybeGenerateThreadTitle(thread, content, hasPriorItems)
	return nil
}

// sendToProvider forwards the user content to the active provider
// session. Extracted so sendMessage can call the provider before
// touching the store (Bug B8) while keeping the switch/log behaviour
// in one place.
func sendToProvider(sess session, threadID, content string) error {
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
