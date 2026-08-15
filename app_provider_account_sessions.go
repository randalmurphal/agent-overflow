package main

import (
	"context"
	"fmt"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// providerSessionMatchesAccount reports whether a live session is already
// running under the account a new turn would be sent as.
//
// Both fields are load-bearing. The generation moves on every credential write
// (a re-login, a refreshed slot) even when the account id does not, and the
// account id changes without the generation ever going backwards, so either one
// alone admits a session serving the wrong credentials.
func providerSessionMatchesAccount(sess session, selection providerAccountSelection) bool {
	return sess.credentialGeneration == selection.Generation &&
		sess.credentialAccountID == selection.AccountID
}

// ensureProviderAccountReadyForSendLocked applies a provider account switch
// at the last safe moment before a new user turn. The caller holds the
// thread-action lock.
func (a *App) ensureProviderAccountReadyForSendLocked(thread store.Thread) error {
	if err := a.reconcileExternalProviderAccount(thread.Provider); err != nil {
		return fmt.Errorf("check %s account before send: %w", thread.Provider, err)
	}
	selection := a.captureProviderAccountSelection(thread.Provider)
	return a.ensureProviderAccountSelectionReadyForSendLocked(thread, selection)
}

func (a *App) ensureProviderAccountSelectionReadyForSendLocked(
	thread store.Thread,
	selection providerAccountSelection,
) error {
	generation := selection.Generation
	sess, ok := a.sessionManager().get(thread.ID)
	if !ok || providerSessionMatchesAccount(sess, selection) {
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
			selection.Account,
		)
		a.emitProviderSessionAccount(thread.ID)
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
		if err := a.reconnectSessionLocked(context.Background(), thread.ID); err != nil {
			return fmt.Errorf("switch Codex account for this thread: %w", err)
		}
		current, exists := a.sessionManager().get(thread.ID)
		if !exists || !providerSessionMatchesAccount(current, selection) {
			return fmt.Errorf("Codex account did not switch for this thread; retry after its reconnect finishes")
		}
		return nil
	default:
		return nil
	}
}

// applyProviderAccountSelectionToSessions projects a successful provider-wide
// account activation onto live sessions whose credential behavior changes
// immediately. Claude rereads the canonical native store without a process
// restart; Codex app-servers intentionally retain their cached account until
// the safe reconnect-on-send gate.
func (a *App) applyProviderAccountSelectionToSessions(
	providerName string,
	generation uint64,
	accountID string,
) {
	if providerName != string(provider.Claude) {
		return
	}
	var accountInfo provider.AccountInfo
	if accountID != "" && a.providerAccounts != nil {
		if account, ok := a.providerAccounts.Get(providerName, accountID, time.Now()); ok {
			accountInfo = providerAccountInfo(account)
		}
	}
	for _, threadID := range a.sessionManager().updateProviderCredentials(
		providerName,
		generation,
		accountID,
		accountInfo,
	) {
		if accountID == "" {
			a.emitProviderSessionDisconnected(threadID, providerName)
		} else {
			a.emitProviderSessionAccount(threadID)
		}
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
		if providerSessionMatchesAccount(sess, selection) {
			return sess, a.providerAccountMu.RUnlock, nil
		}
		// A switch landed between the readiness check and the read lock.
		// Release and retry so the switch is applied before dispatch.
		a.providerAccountMu.RUnlock()
	}
}
