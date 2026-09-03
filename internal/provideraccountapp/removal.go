package provideraccountapp

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
func (m *Manager) RemoveProviderAccount(providerName, accountID string) error {
	if m.shuttingDown() {
		return m.shutdownError()
	}
	if err := ValidateProvider(providerName); err != nil {
		return err
	}
	if m.store == nil || m.credentials == nil {
		return errors.New("provider account storage is unavailable")
	}
	reconcileMu := m.reconcileMutex(providerName)
	reconcileMu.Lock()
	defer reconcileMu.Unlock()
	if err := m.reconcileExternalProviderAccountWithMutexHeld(providerName); err != nil {
		return fmt.Errorf("check current %s account before removal: %w", providerName, err)
	}

	m.mu.Lock()
	accountLocked := true
	defer func() {
		if accountLocked {
			m.mu.Unlock()
		}
	}()

	accounts := m.store.List(providerName, time.Now())
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
	active, removingActive := m.store.Active(providerName, time.Now())
	removingActive = removingActive && active.ID == accountID

	var replacement provideraccounts.Account
	if removingActive && len(accounts) > 1 {
		// The next card in display order inherits the selection — skipping any
		// slot the provider signed out. Activating a husk can only be refused,
		// and refusing here is a lockout: the user could not remove the active
		// account until they first repaired an unrelated dead one. When no
		// usable replacement exists, removal signs the provider out, exactly
		// as removing the final account does.
		for offset := 1; offset < len(accounts); offset++ {
			candidate := accounts[(removeIndex+offset)%len(accounts)]
			usable, usableErr := m.credentials.CredentialUsable(
				providerName,
				candidate.ID,
				false,
			)
			if usableErr != nil {
				// A read that failed is not a verdict — the activation below
				// still validates for real.
				usable = true
			}
			if usable {
				replacement = candidate
				break
			}
		}
	}
	var replacementCredential provideraccounts.CredentialSnapshot
	if replacement.ID != "" {
		var err error
		replacementCredential, err = m.credentials.ReadCredentialSnapshot(
			providerName,
			replacement.ID,
			false,
		)
		if err != nil {
			return fmt.Errorf("read replacement %s credentials: %w", providerName, err)
		}
	}

	savedSnapshot, savedErr := m.credentials.ReadCredentialSnapshot(
		providerName,
		accountID,
		false,
	)
	if savedErr != nil && !provideraccounts.IsCredentialMissing(savedErr) {
		return fmt.Errorf("read saved %s account before removal: %w", providerName, savedErr)
	}
	// A husked slot counts as holding nothing: the write layer refuses to
	// persist a husk, so treating it as restorable would turn a clean unwind
	// into a rollback error that buries the real cause. Skipping the rewrite
	// leaves the same state either way — an account that needs a fresh login.
	hasSavedSnapshot := savedErr == nil &&
		!m.credentials.CredentialSignedOut(providerName, savedSnapshot.Data)

	var activeSnapshot provideraccounts.CredentialSnapshot
	hasActiveSnapshot := false
	activeSignedOut := false
	if removingActive {
		var err error
		activeSnapshot, err = m.verifiedActiveCredentialLocked(providerName)
		switch {
		case err == nil:
			hasActiveSnapshot = true
			savedSnapshot = activeSnapshot
			hasSavedSnapshot = true
		case errors.Is(err, errActiveCredentialSignedOut):
			// The CLI blanked the canonical credential after a failed refresh.
			// There is nothing worth preserving and the husk must not reach any
			// slot; the removal itself proceeds — refusing here was the
			// "cannot even delete the bricked account" half of the 2026-08-03
			// lockout. The rollback below still restores the slot's own bytes.
			activeSignedOut = true
		case !provideraccounts.IsCredentialMissing(err):
			return err
		}

		if replacement.ID != "" {
			currentID := ""
			var current *provideraccounts.CredentialSnapshot
			if hasActiveSnapshot {
				currentID = accountID
				current = &activeSnapshot
			}
			if err := m.credentials.ActivateWithSnapshot(
				providerName,
				currentID,
				replacement.ID,
				current,
			); err != nil {
				return fmt.Errorf("activate replacement %s account: %w", providerName, err)
			}
			if err := m.verifyCanonicalProviderCredentialLocked(
				providerName,
				replacementCredential.Data,
			); err != nil {
				return err
			}
		} else if hasActiveSnapshot || activeSignedOut {
			// The husk counts here too: removing the final account promises a
			// cleared canonical credential, and a leftover husk is still a
			// file claiming a login exists.
			if err := m.credentials.RemoveActive(providerName); err != nil {
				return fmt.Errorf("sign out final %s account: %w", providerName, err)
			}
		}
	}

	rollback := func(cause error) error {
		var rollbackErrs []error
		if hasSavedSnapshot {
			if err := m.credentials.WriteAccountCredential(
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
				// Canonical holds the replacement's credential at this point
				// (its activation succeeded above); name it so any rotation a
				// live CLI performed since is preserved into its slot rather
				// than destroyed by the reinstatement below.
				err = m.rollbackProviderAccountActivation(providerName, replacement.ID, accountID)
			} else {
				err = m.credentials.RemoveActive(providerName)
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

	if err := m.credentials.RemoveAccount(providerName, accountID); err != nil {
		return rollback(fmt.Errorf("remove saved %s credentials: %w", providerName, err))
	}
	if err := m.store.Remove(providerName, accountID, replacement.ID); err != nil {
		return rollback(err)
	}

	binary := m.providerBinaryPath(providerName)
	if removingActive {
		if replacement.ID == "" {
			delete(m.fingerprints, providerName)
		} else {
			m.rememberProviderCredentialFingerprintLocked(
				providerName,
				replacementCredential.Data,
			)
		}
	}
	generation := m.store.Generation(providerName)
	m.mu.Unlock()
	accountLocked = false
	m.invalidateProviderAccountProbe(providerName, binary)
	m.forgetRateLimits(providerName, accountID)
	// BEFORE the branch, not inside it: removing an account that was not the
	// active one took every early return below and published nothing at all,
	// so the card stayed on every other client's Settings screen until reload.
	m.emitProviderAccountsChanged()

	if !removingActive {
		return nil
	}
	if replacement.ID == "" {
		m.emitProviderAccountCleared(providerName, generation)
		m.applySelection(providerName, generation, "")
		return nil
	}
	m.emitProviderAccount(
		providerName,
		replacement,
		AccountInfo(replacement),
		generation,
	)
	m.applySelection(providerName, generation, replacement.ID)
	return nil
}
