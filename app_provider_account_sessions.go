package main

import (
	"fmt"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// ensureProviderAccountReadyForSendLocked applies a provider account switch
// at the last safe moment before a new user turn. The caller holds the
// thread-action lock.
func (a *App) ensureProviderAccountReadyForSendLocked(thread store.Thread) error {
	selection := a.captureProviderAccountSelection(thread.Provider)
	return a.ensureProviderAccountSelectionReadyForSendLocked(thread, selection)
}

func (a *App) ensureProviderAccountSelectionReadyForSendLocked(
	thread store.Thread,
	selection providerAccountSelection,
) error {
	generation := selection.Generation
	sess, ok := a.sessionManager().get(thread.ID)
	if !ok || sess.credentialGeneration == generation {
		return nil
	}

	switch thread.Provider {
	case string(provider.Claude):
		// Claude re-reads the shared native credential store. This is the same
		// behavior as completing /login in another Claude process, so no
		// process interruption is needed.
		a.sessionManager().updateCredentials(
			thread.ID,
			sess.token,
			generation,
			selection.AccountID,
		)
		return nil
	case string(provider.Codex):
		if sess.liveness != nil && sess.liveness.activeTurns.Load() > 0 {
			return fmt.Errorf("Codex account switch is pending for this thread until its active turn finishes")
		}
		running, err := a.store.ListRunningBackgroundToolCalls(thread.ID)
		if err != nil {
			return fmt.Errorf("check Codex background work before account switch: %w", err)
		}
		if len(running) > 0 {
			return fmt.Errorf("Codex account switch is pending for this thread until its background work finishes")
		}
		if err := a.reconnectSessionLocked(thread.ID); err != nil {
			return fmt.Errorf("switch Codex account for this thread: %w", err)
		}
		current, exists := a.sessionManager().get(thread.ID)
		if !exists ||
			current.credentialGeneration != selection.Generation ||
			current.credentialAccountID != selection.AccountID {
			return fmt.Errorf("Codex account did not switch for this thread; retry after its reconnect finishes")
		}
		return nil
	default:
		return nil
	}
}

// lockProviderAccountForSendLocked returns a session whose credential
// generation matches a provider selection held stable by a read lock. The
// caller must invoke the returned unlock immediately after the provider write.
// Account switches take the write side of the same mutex, so a turn can be
// unambiguously ordered before or after a settings-side switch.
func (a *App) lockProviderAccountForSendLocked(thread store.Thread) (session, func(), error) {
	for {
		if err := a.ensureProviderAccountReadyForSendLocked(thread); err != nil {
			return session{}, nil, err
		}

		a.providerAccountMu.RLock()
		selection := a.providerAccountSelectionLocked(thread.Provider)
		sess, ok := a.sessionManager().get(thread.ID)
		if !ok {
			a.providerAccountMu.RUnlock()
			return session{}, nil, fmt.Errorf("no active session for thread %s", thread.ID)
		}
		if sess.credentialGeneration == selection.Generation &&
			sess.credentialAccountID == selection.AccountID {
			return sess, a.providerAccountMu.RUnlock, nil
		}
		// A switch landed between the readiness check and the read lock.
		// Release and retry so the switch is applied before dispatch.
		a.providerAccountMu.RUnlock()
	}
}
