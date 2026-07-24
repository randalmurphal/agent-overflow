package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/provideraccounts"
)

// RefreshProviderAccountUsage probes one saved account without changing the
// provider-wide selection. Every probe runs in a short-lived home seeded with
// only that account's credential. A rotated selected credential reaches the
// canonical home only after selection and fingerprint validation.
func (a *App) RefreshProviderAccountUsage(
	providerName,
	accountID string,
) (retErr error) {
	return a.refreshProviderAccountUsage(a.lifeCtx(), providerName, accountID)
}

func (a *App) refreshProviderAccountUsage(
	ctx context.Context,
	providerName,
	accountID string,
) (retErr error) {
	if err := validateManagedProvider(providerName); err != nil {
		return err
	}
	if a.providerAccounts == nil || a.providerCredentials == nil {
		return errors.New("provider account storage is unavailable")
	}
	refreshMu := a.providerCredentialReconcileMutex(providerName)
	refreshMu.Lock()
	defer refreshMu.Unlock()

	a.providerAccountMu.RLock()
	account, exists := a.providerAccounts.Get(providerName, accountID, time.Now())
	if !exists {
		a.providerAccountMu.RUnlock()
		return fmt.Errorf("%s account %q is not saved", providerName, accountID)
	}
	active, isActive := a.providerAccounts.Active(providerName, time.Now())
	isSelected := isActive && active.ID == accountID
	generation := a.providerAccounts.Generation(providerName)
	selection := providerAccountSelection{
		Generation: generation,
		AccountID:  accountID,
		Account:    providerAccountInfo(account),
	}
	var credential provideraccounts.CredentialSnapshot
	var err error
	if isSelected {
		selection = a.providerAccountSelectionLocked(providerName)
		credential, err = a.providerCredentials.ReadCredentialSnapshot(
			providerName,
			"",
			true,
		)
	} else {
		credential, err = a.providerCredentials.ReadCredentialSnapshot(
			providerName,
			accountID,
			false,
		)
	}
	if err != nil {
		a.providerAccountMu.RUnlock()
		return fmt.Errorf("read saved %s credentials for usage refresh: %w", providerName, err)
	}
	a.providerAccountMu.RUnlock()

	ephemeral, err := a.providerCredentials.NewEphemeralHomeWithCredential(
		providerName,
		credential.Data,
	)
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := ephemeral.Cleanup(); cleanupErr != nil {
			retErr = errors.Join(retErr, cleanupErr)
		}
	}()

	var (
		snapshot            provider.RateLimitsSnapshot
		refreshedCredential []byte
		updatedAccount      *provideraccounts.Account
	)
	switch providerName {
	case string(provider.Claude):
		claudeSelection := claudeRateLimitSelection{
			providerAccountSelection: selection,
			EphemeralHome:            ephemeral,
			Env: map[string]string{
				"CLAUDE_CONFIG_DIR": ephemeral.Path,
			},
		}
		snapshot, refreshedCredential, err = a.probeClaudeRateLimitsForSelection(
			ctx,
			claudeSelection,
		)
		if err != nil {
			return err
		}
	case string(provider.Codex):
		info, probeErr := codex.ProbeAccount(ctx, codex.ProbeConfig{
			Binary: a.providerBinaryPath(providerName),
			Env:    map[string]string{"CODEX_HOME": ephemeral.Path},
			OnSnapshot: func(value provider.RateLimitsSnapshot) {
				snapshot = value
			},
		})
		if probeErr != nil {
			return probeErr
		}
		if len(snapshot.Limits) == 0 {
			return errors.New("Codex did not return usage limits")
		}
		refreshed, readErr := a.providerCredentials.ReadEphemeralCredential(ephemeral)
		if readErr != nil {
			return fmt.Errorf("capture refreshed Codex credentials: %w", readErr)
		}
		refreshedCredential = refreshed.Data
		selection.Account = info
	}

	a.providerAccountMu.Lock()
	if err := a.validateProviderUsageRefreshSelectionLocked(
		providerName,
		accountID,
		generation,
		isSelected,
	); err != nil {
		a.providerAccountMu.Unlock()
		return err
	}
	if isSelected {
		if err := a.verifyCanonicalProviderCredentialLocked(
			providerName,
			credential.Data,
		); err != nil {
			a.providerAccountMu.Unlock()
			return err
		}
	}
	if providerName == string(provider.Codex) {
		observedAccountID, updated, identityErr := a.accountIDForObservedIdentity(
			providerName,
			accountID,
			selection.Account,
		)
		if identityErr != nil {
			a.providerAccountMu.Unlock()
			return identityErr
		}
		if observedAccountID != accountID {
			a.providerAccountMu.Unlock()
			return fmt.Errorf(
				"Codex account %q authenticated as saved account %q",
				accountID,
				observedAccountID,
			)
		}
		updatedAccount = updated
	}
	if len(refreshedCredential) > 0 {
		if isSelected {
			credentialChanged := !bytes.Equal(refreshedCredential, credential.Data)
			if commitErr := a.providerCredentials.CommitSelectedCredential(
				providerName,
				accountID,
				refreshedCredential,
			); commitErr != nil {
				delete(a.providerCredentialFingerprints, providerName)
				a.providerAccountMu.Unlock()
				return commitErr
			}
			if verifyErr := a.verifyCanonicalProviderCredentialLocked(
				providerName,
				refreshedCredential,
			); verifyErr != nil {
				a.providerAccountMu.Unlock()
				return verifyErr
			}
			if providerName == string(provider.Codex) && credentialChanged {
				advancedAccount, advanceErr := a.providerAccounts.AdvanceActiveCredential(
					providerName,
					accountID,
				)
				if advanceErr != nil {
					delete(a.providerCredentialFingerprints, providerName)
					a.providerAccountMu.Unlock()
					return fmt.Errorf(
						"record refreshed %s credentials: %w",
						providerName,
						advanceErr,
					)
				}
				generation = a.providerAccounts.Generation(providerName)
				updatedAccount = &advancedAccount
			}
			a.rememberProviderCredentialFingerprintLocked(
				providerName,
				refreshedCredential,
			)
		} else if writeErr := a.providerCredentials.WriteAccountCredential(
			providerName,
			accountID,
			refreshedCredential,
		); writeErr != nil {
			a.providerAccountMu.Unlock()
			return fmt.Errorf("save refreshed %s credentials: %w", providerName, writeErr)
		}
	} else if isSelected {
		if writeErr := a.providerCredentials.WriteAccountCredential(
			providerName,
			accountID,
			credential.Data,
		); writeErr != nil {
			a.providerAccountMu.Unlock()
			return fmt.Errorf(
				"save current %s credentials: %w",
				providerName,
				writeErr,
			)
		}
		a.rememberProviderCredentialFingerprintLocked(providerName, credential.Data)
	}
	a.providerAccountMu.Unlock()
	if updatedAccount != nil {
		a.emitProviderAccount(
			providerName,
			*updatedAccount,
			providerAccountInfo(*updatedAccount),
			generation,
		)
	}
	snapshot.Provider = providerName
	snapshot.AccountID = accountID
	a.emitRateLimitsSnapshot(snapshot)
	return nil
}

func (a *App) validateProviderUsageRefreshSelectionLocked(
	providerName string,
	accountID string,
	generation uint64,
	wasSelected bool,
) error {
	if a.providerAccounts.Generation(providerName) != generation {
		return fmt.Errorf("%s account changed while refreshing usage; retry", providerName)
	}
	if _, exists := a.providerAccounts.Get(providerName, accountID, time.Now()); !exists {
		return fmt.Errorf("%s account %q was removed while refreshing usage", providerName, accountID)
	}
	active, hasActive := a.providerAccounts.Active(providerName, time.Now())
	isSelected := hasActive && active.ID == accountID
	if isSelected != wasSelected {
		return fmt.Errorf("%s account selection changed while refreshing usage; retry", providerName)
	}
	return nil
}
