package app

import (
	"context"
	"fmt"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// ProviderSessionAccountEvent is the account a live provider process is
// currently using for one thread. It is deliberately separate from the
// provider-global selected account: an active Codex process can keep using the
// old account while the newly selected account waits for its safe reconnect.
type ProviderSessionAccountEvent struct {
	ThreadID   string               `json:"threadId"`
	Provider   string               `json:"provider"`
	AccountID  string               `json:"accountId,omitempty"`
	Account    provider.AccountInfo `json:"account"`
	Generation uint64               `json:"generation,omitempty"`
	Connected  bool                 `json:"connected"`
}

// providerSessionMatchesAccount reports whether a live session is already
// running under the account a new turn would be sent as.
//
// Both fields are load-bearing. The generation moves on every credential write
// (a re-login, a refreshed slot) even when the account id does not, and the
// account id changes without the generation ever going backwards, so either one
// alone admits a session serving the wrong credentials.
func providerSessionMatchesAccount(sess session, selection providerAccountSelection) bool {
	return sess.CredentialGeneration == selection.Generation &&
		sess.CredentialAccountID == selection.AccountID
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
			sess.Token,
			generation,
			selection.AccountID,
			selection.Account,
		)
		a.emitProviderSessionAccount(thread.ID)
		return nil
	case string(provider.Codex):
		if sess.Liveness != nil && sess.Liveness.ActiveTurns.Load() > 0 {
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

		lease := a.providerAccountSelectionLease(thread.Provider)
		selection := providerAccountSelection{
			Generation: lease.Selection.Generation,
			AccountID:  lease.Selection.AccountID,
			Account:    lease.Selection.Account,
		}
		sess, ok := a.sessionManager().get(thread.ID)
		if !ok {
			lease.Release()
			return session{}, nil, fmt.Errorf("no active session for thread %s", thread.ID)
		}
		if providerSessionMatchesAccount(sess, selection) {
			return sess, lease.Release, nil
		}
		// A switch landed between the readiness check and the read lock.
		// Release and retry so the switch is applied before dispatch.
		lease.Release()
	}
}

func (a *App) providerSessionAccount(threadID string) *ProviderSessionAccountEvent {
	projection, ok := a.providerLifecycleService().SessionAccount(threadID)
	if !ok {
		return nil
	}
	return &ProviderSessionAccountEvent{
		ThreadID: projection.ThreadID, Provider: projection.Provider,
		AccountID: projection.AccountID, Account: projection.Account,
		Generation: projection.Generation, Connected: true,
	}
}

func (a *App) emitProviderSessionAccount(threadID string) {
	if event := a.providerSessionAccount(threadID); event != nil {
		a.emit(eventchan.ProviderSessionAccount, *event)
	}
}

func (a *App) emitProviderSessionDisconnected(threadID, providerName string) {
	a.emit(eventchan.ProviderSessionAccount, ProviderSessionAccountEvent{
		ThreadID:  threadID,
		Provider:  providerName,
		Connected: false,
	})
}
