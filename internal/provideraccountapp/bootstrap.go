package provideraccountapp

import (
	"fmt"
	"time"
)

// ReconcileProviderHome stamps the metadata with its credential home and
// recovers crash-orphaned provider rotations. The slots of unfinished
// sign-ins are SettleReservedSlots's.
func (m *Manager) ReconcileProviderHome(userHome string) error {
	if !m.available() {
		return fmt.Errorf("provider account storage is unavailable")
	}
	if err := m.store.ClaimProviderHome(userHome); err != nil {
		return fmt.Errorf("bind provider account metadata to credential home: %w", err)
	}

	selected := make(map[string]string)
	for _, providerName := range ManagedAccountProviders {
		if active, ok := m.store.Active(providerName, time.Now()); ok {
			selected[providerName] = active.ID
		}
	}
	sweepResults, sweepErr := m.credentials.SweepEphemeralCredentials(time.Now(), selected)
	for _, result := range sweepResults {
		if result.Action == "skipped" {
			continue
		}
		canonical := ""
		if result.Canonical {
			canonical = ", canonical credential included"
		}
		m.audit("ephemeral %s sweep: %s crash-orphaned home %s (account %q%s)", result.Provider, result.Action, result.ConfigHome, result.AccountID, canonical)
	}
	if sweepErr != nil {
		m.audit("ephemeral sweep errors: %v", sweepErr)
	}
	for providerName, accountID := range selected {
		if err := m.promoteSelectedSlot(providerName, accountID); err != nil {
			m.audit("%v", err)
		}
	}
	return nil
}

// promoteActiveSlot is promoteSelectedSlot for whichever account is
// selected now. Every path that reads the canonical credential as the
// selected login calls it first, under the provider's reconcile mutex.
func (m *Manager) promoteActiveSlot(providerName string) error {
	if !m.available() {
		return nil
	}
	m.mu.RLock()
	active, ok := m.store.Active(providerName, time.Now())
	m.mu.RUnlock()
	if !ok {
		return nil
	}
	return m.promoteSelectedSlot(providerName, active.ID)
}

// promoteSelectedSlot brings the canonical credential up to the selected
// account's slot when another store's boot sweep recovered a rotation there
// and the canonical store still holds the seed it consumed
// (provideraccounts.PromoteSlotOverCanonical). The caller holds the
// provider's reconcile mutex or runs before any can.
func (m *Manager) promoteSelectedSlot(providerName, accountID string) error {
	promoted, changed, err := m.credentials.PromoteSlotOverCanonical(providerName, accountID)
	if err != nil {
		return fmt.Errorf("restore the saved %s credential of account %s to the canonical home: %w", providerName, accountID, err)
	}
	if !changed {
		return nil
	}
	m.mu.Lock()
	m.rememberProviderCredentialFingerprintLocked(providerName, promoted.Bytes())
	m.mu.Unlock()
	m.audit("restored the later saved %s credential of account %s to the canonical home", providerName, accountID)
	return nil
}
