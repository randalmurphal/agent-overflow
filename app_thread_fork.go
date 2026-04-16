package main

import (
	"context"
	"fmt"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

func (a *App) ForkThread(sourceThreadID string) (store.Thread, error) {
	source, err := a.store.GetThread(sourceThreadID)
	if err != nil {
		return store.Thread{}, fmt.Errorf("fork thread: %w", err)
	}
	if err := a.ensureThreadCanFork(source); err != nil {
		return store.Thread{}, err
	}

	fork := buildForkedThread(source)
	if err := a.store.CreateThread(fork); err != nil {
		return store.Thread{}, fmt.Errorf("fork thread: create fork thread: %w", err)
	}

	if err := a.store.CloneThreadItems(source.ID, fork.ID); err != nil {
		a.cleanupForkThread(fork.ID)
		return store.Thread{}, fmt.Errorf("fork thread: clone timeline: %w", err)
	}

	sessionRef, pendingForkRef, err := a.resolveForkResumeState(source)
	if err != nil {
		a.cleanupForkThread(fork.ID)
		return store.Thread{}, err
	}
	fork.SessionRef = sessionRef
	fork.PendingForkRef = pendingForkRef
	fork.UpdatedAt = time.Now().UnixMilli()
	if err := a.store.UpdateThread(fork); err != nil {
		a.cleanupForkThread(fork.ID)
		return store.Thread{}, fmt.Errorf("fork thread: persist fork state: %w", err)
	}

	return fork, nil
}

func (a *App) cleanupForkThread(threadID string) {
	if threadID == "" {
		return
	}
	if err := a.store.DeleteThread(threadID); err != nil {
		// Cleanup is best-effort. The original error path remains the actionable one.
	}
}

func (a *App) ensureThreadCanFork(source store.Thread) error {
	items, err := a.store.ListItems(source.ID)
	if err != nil {
		return fmt.Errorf("fork thread: list source items: %w", err)
	}
	if len(items) == 0 {
		return fmt.Errorf("fork thread: thread %q has no messages and cannot be forked", source.ID)
	}
	return nil
}

func buildForkedThread(source store.Thread) store.Thread {
	now := time.Now().UnixMilli()
	return store.Thread{
		ID:                 uuid.NewString(),
		Title:              source.Title + " (fork)",
		Provider:           source.Provider,
		WorkspacePath:      source.WorkspacePath,
		Model:              source.Model,
		ProjectPath:        source.ProjectPath,
		WorktreePath:       source.WorktreePath,
		Branch:             source.Branch,
		InteractionMode:    source.InteractionMode,
		ForkedFromThreadID: source.ID,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
}

func (a *App) resolveForkResumeState(source store.Thread) (sessionRef string, pendingForkRef string, err error) {
	switch source.Provider {
	case string(provider.Codex):
		ref, err := a.forkCodexThread(source)
		if err != nil {
			return "", "", fmt.Errorf("fork thread: fork codex provider state: %w", err)
		}
		return ref, "", nil
	case string(provider.Claude):
		if source.SessionRef == "" {
			return "", "", fmt.Errorf("fork thread: source thread %q is missing a Claude session reference", source.ID)
		}
		return "", source.SessionRef, nil
	default:
		return "", "", fmt.Errorf("fork thread: unsupported provider %q", source.Provider)
	}
}

func (a *App) forkCodexThread(source store.Thread) (string, error) {
	const op = "fork codex thread"
	if source.SessionRef == "" {
		return "", fmt.Errorf("%s: source thread %q is missing a Codex thread reference", op, source.ID)
	}

	activeSession, ok := a.activeCodexSession(source.ID)
	if ok {
		return activeSession.Fork(context.Background())
	}

	tempSession, err := codex.NewSession(context.Background(), source.ID, codex.Config{
		Binary:         a.providerBinaryPath(source.Provider),
		Model:          source.Model,
		WorkDir:        source.WorkspacePath,
		ResumeThreadID: source.SessionRef,
		EventLogger:    a.logger,
	}, func(provider.ProviderEvent) {})
	if err != nil {
		return "", fmt.Errorf("%s: resume source thread: %w", op, err)
	}
	defer tempSession.Close()

	forkedThreadID, err := tempSession.Fork(context.Background())
	if err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}
	return forkedThreadID, nil
}

func (a *App) activeCodexSession(threadID string) (*codex.Session, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	sess, ok := a.sessions[threadID]
	if !ok || sess.codex == nil {
		return nil, false
	}
	return sess.codex, true
}
