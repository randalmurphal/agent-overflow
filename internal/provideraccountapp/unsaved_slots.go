package provideraccountapp

import "fmt"

// removeUnsavedSlots deletes the slots of sign-ins this store started whose
// account was never saved: a crash between the slot write and the row
// insert, or a failed sign-in whose rollback could not remove its slot. It
// deletes nothing else; every other slot in the shared provider home is
// another data root's, or an earlier release's (provideraccounts
// reserved_slots.go). A slot that cannot be removed stays reserved for the
// next boot, and its error fails this one.
func (m *Manager) removeUnsavedSlots(providerName string) error {
	release, err := m.store.ReleaseReservedSlots(providerName, func(accountID string) error {
		return m.credentials.RemoveAccount(providerName, accountID)
	})
	for _, accountID := range release.Listed {
		m.audit("kept %s credential slot %s: a saved account names it, so its reservation was cleared without deleting it", providerName, accountID)
	}
	for _, accountID := range release.Removed {
		m.audit("removed %s credential slot %s: a sign-in this data root started never saved its account", providerName, accountID)
	}
	if err != nil {
		return fmt.Errorf("remove the %s credential slots of unfinished sign-ins: %w", providerName, err)
	}
	return nil
}
