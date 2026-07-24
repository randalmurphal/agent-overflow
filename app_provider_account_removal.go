package main

import (
	"errors"
	"fmt"
	"time"

	"agent-overflow/internal/provideraccounts"
)

// RemoveProviderAccount deletes one saved native login. Removing the selected
// account activates the next card in display order; removing the final account
// clears the provider's canonical credential. Existing Codex processes retain
// cached authentication until the normal safe reconnect-on-send gate.
func (a *App) RemoveProviderAccount(providerName, accountID string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	if err := validateManagedProvider(providerName); err != nil {
		return err
	}
	if a.providerAccounts == nil || a.providerCredentials == nil {
		return errors.New("provider account storage is unavailable")
	}
	reconcileMu := a.providerCredentialReconcileMutex(providerName)
	reconcileMu.Lock()
	defer reconcileMu.Unlock()
	if err := a.reconcileExternalProviderAccountWithMutexHeld(providerName); err != nil {
		return fmt.Errorf("check current %s account before removal: %w", providerName, err)
	}

	a.providerAccountMu.Lock()
	accountLocked := true
	defer func() {
		if accountLocked {
			a.providerAccountMu.Unlock()
		}
	}()

	accounts := a.providerAccounts.List(providerName, time.Now())
	removeIndex := -1
	for i := range accounts {
		if accounts[i].ID == accountID {
			removeIndex = i
			break
		}
	}
	if removeIndex < 0 {
		return fmt.Errorf("%s account %q is not saved", providerName, accountID)
	}
	active, removingActive := a.providerAccounts.Active(providerName, time.Now())
	removingActive = removingActive && active.ID == accountID

	var replacement provideraccounts.Account
	if removingActive && len(accounts) > 1 {
		replacement = accounts[(removeIndex+1)%len(accounts)]
	}
	var replacementCredential provideraccounts.CredentialSnapshot
	if replacement.ID != "" {
		var err error
		replacementCredential, err = a.providerCredentials.ReadCredentialSnapshot(
			providerName,
			replacement.ID,
			false,
		)
		if err != nil {
			return fmt.Errorf("read replacement %s credentials: %w", providerName, err)
		}
	}

	savedSnapshot, savedErr := a.providerCredentials.ReadCredentialSnapshot(
		providerName,
		accountID,
		false,
	)
	hasSavedSnapshot := savedErr == nil
	if savedErr != nil && !provideraccounts.IsCredentialMissing(savedErr) {
		return fmt.Errorf("read saved %s account before removal: %w", providerName, savedErr)
	}

	var activeSnapshot provideraccounts.CredentialSnapshot
	hasActiveSnapshot := false
	if removingActive {
		var err error
		activeSnapshot, err = a.verifiedActiveCredentialLocked(providerName)
		if err == nil {
			hasActiveSnapshot = true
			savedSnapshot = activeSnapshot
			hasSavedSnapshot = true
		} else if !provideraccounts.IsCredentialMissing(err) {
			return err
		}

		if replacement.ID != "" {
			currentID := ""
			var current *provideraccounts.CredentialSnapshot
			if hasActiveSnapshot {
				currentID = accountID
				current = &activeSnapshot
			}
			if err := a.providerCredentials.ActivateWithSnapshot(
				providerName,
				currentID,
				replacement.ID,
				current,
			); err != nil {
				return fmt.Errorf("activate replacement %s account: %w", providerName, err)
			}
			if err := a.verifyCanonicalProviderCredentialLocked(
				providerName,
				replacementCredential.Data,
			); err != nil {
				return err
			}
		} else if hasActiveSnapshot {
			if err := a.providerCredentials.RemoveActive(providerName); err != nil {
				return fmt.Errorf("sign out final %s account: %w", providerName, err)
			}
		}
	}

	rollback := func(cause error) error {
		var rollbackErrs []error
		if hasSavedSnapshot {
			if err := a.providerCredentials.WriteAccountCredential(
				providerName,
				accountID,
				savedSnapshot.Data,
			); err != nil {
				rollbackErrs = append(rollbackErrs, fmt.Errorf("restore saved credentials: %w", err))
			}
		}
		if removingActive {
			var err error
			if hasSavedSnapshot {
				err = a.providerCredentials.Activate(providerName, "", accountID)
			} else {
				err = a.providerCredentials.RemoveActive(providerName)
			}
			if err != nil {
				rollbackErrs = append(rollbackErrs, fmt.Errorf("restore active credentials: %w", err))
			}
		}
		if len(rollbackErrs) > 0 {
			return fmt.Errorf("%w (credential rollback failed: %v)", cause, errors.Join(rollbackErrs...))
		}
		return cause
	}

	if err := a.providerCredentials.RemoveAccount(providerName, accountID); err != nil {
		return rollback(fmt.Errorf("remove saved %s credentials: %w", providerName, err))
	}
	if err := a.providerAccounts.Remove(providerName, accountID, replacement.ID); err != nil {
		return rollback(err)
	}

	binary := a.providerBinaryPath(providerName)
	a.invalidateProviderAccountProbe(providerName, binary)
	if removingActive {
		if replacement.ID == "" {
			delete(a.providerCredentialFingerprints, providerName)
		} else {
			a.rememberProviderCredentialFingerprintLocked(
				providerName,
				replacementCredential.Data,
			)
		}
	}
	generation := a.providerAccounts.Generation(providerName)
	a.providerAccountMu.Unlock()
	accountLocked = false
	a.forgetRateLimitsSnapshot(providerName, accountID)

	if !removingActive {
		return nil
	}
	if replacement.ID == "" {
		a.emitProviderAccountCleared(providerName, generation)
		a.applyProviderAccountSelectionToSessions(providerName, generation, "")
		return nil
	}
	a.emitProviderAccount(
		providerName,
		replacement,
		providerAccountInfo(replacement),
		generation,
	)
	a.applyProviderAccountSelectionToSessions(providerName, generation, replacement.ID)
	return nil
}
