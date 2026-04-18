package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

// ForkThread copies a source thread's timeline into a new fork and wires
// the provider-specific resume state. The whole sequence is atomic from
// the caller's point of view: if any step fails, the partially-created
// fork is torn down so no half-forked rows linger.
//
// The "atomic unit" is emulated in the app layer rather than a single
// SQLite transaction because the fork flow crosses a boundary — it has
// to talk to the Codex provider to fork a live session, which can fail
// independently of the DB. Wrapping the whole sequence in sql.Tx would
// hold a DB transaction open across a network-speed operation and break
// the rest of the store's single-connection model. Instead, we compose
// with a best-effort rollback: cleanupForkThread removes the fork row
// (CASCADE drops cloned items + drafts), and cleanup errors are joined
// to the primary error so the caller sees both.
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
		return store.Thread{}, errors.Join(
			fmt.Errorf("fork thread: clone timeline: %w", err),
			a.cleanupForkThread(fork.ID),
		)
	}

	sessionRef, pendingForkRef, err := a.resolveForkResumeState(source)
	if err != nil {
		return store.Thread{}, errors.Join(err, a.cleanupForkThread(fork.ID))
	}
	fork.SessionRef = sessionRef
	fork.PendingForkRef = pendingForkRef
	fork.UpdatedAt = time.Now().UnixMilli()
	if err := a.store.UpdateThread(fork); err != nil {
		return store.Thread{}, errors.Join(
			fmt.Errorf("fork thread: persist fork state: %w", err),
			a.cleanupForkThread(fork.ID),
		)
	}

	return fork, nil
}

// cleanupForkThread removes the fork row created by a failed fork. The
// FK CASCADE on items.thread_id, thread_drafts.thread_id, and
// thread_checkpoints.thread_id handles the cloned timeline and any
// derived state. Returns nil on success OR when the row was already gone
// (ErrNoRows is treated as idempotent). Any other error is returned so
// the caller can errors.Join it with the primary fork error — swallowing
// cleanup failures lets orphan fork rows accumulate silently.
func (a *App) cleanupForkThread(threadID string) error {
	if threadID == "" {
		return nil
	}
	if err := a.store.DeleteThread(threadID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("fork thread cleanup: delete fork %s: %w", threadID, err)
	}
	return nil
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
		ProjectID:          source.ProjectID,
		Title:              source.Title + " (fork)",
		Provider:           source.Provider,
		WorkspacePath:      source.WorkspacePath,
		Model:              source.Model,
		WorktreePath:       source.WorktreePath,
		Branch:             source.Branch,
		Mode:               source.Mode,
		ReasoningEffort:    source.ReasoningEffort,
		FastMode:           source.FastMode,
		ContextWindow:      source.ContextWindow,
		RuntimeMode:        source.RuntimeMode,
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
