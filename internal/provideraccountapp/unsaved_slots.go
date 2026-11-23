package provideraccountapp

import (
	"errors"
	"fmt"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
)

// accountReservation is a new account's id with its durable reservation
// (provideraccounts.ReserveAccountID). The operation that made it settles it
// when it fails, by the same rules the boot uses.
type accountReservation struct {
	manager      *Manager
	providerName string
	accountID    string
}

// reserveAccount is the only source of a new account id. Every failure
// after it returns must reach settleOnFailure.
func (m *Manager) reserveAccount(providerName string) (*accountReservation, error) {
	home, err := m.providerSlotHome(providerName)
	if err != nil {
		return nil, err
	}
	accountID, err := m.store.ReserveAccountID(providerName, home)
	if err != nil {
		return nil, err
	}
	return &accountReservation{manager: m, providerName: providerName, accountID: accountID}, nil
}

// ID is the reserved account id.
func (r *accountReservation) ID() string { return r.accountID }

// settleOnFailure settles the reservation when *retErr is set and joins the
// settle's error into it: a saved row keeps its slot and clears the
// reservation; otherwise the slot is removed and then the reservation
// cleared, and a removal that fails keeps the reservation for the next boot.
// A nil reservation (the login matched a saved account) does nothing.
func (r *accountReservation) settleOnFailure(retErr *error) {
	if r == nil || *retErr == nil {
		return
	}
	*retErr = errors.Join(*retErr, r.manager.settleReservations(r.providerName, r.accountID))
}

// SettleReservedSlots settles, for each provider, the reservations this data
// root made under the provider home it runs against: the slots of sign-ins
// that never saved their account. It deletes no other slot; every other slot
// in the shared provider home is another data root's, or an earlier
// release's. Both providers are settled whatever either reports; the error
// joins every slot that could not be removed and every malformed
// reservation.
func (m *Manager) SettleReservedSlots() error {
	if !m.available() {
		return fmt.Errorf("provider account storage is unavailable")
	}
	var errs []error
	for _, providerName := range []string{string(provider.Claude), string(provider.Codex)} {
		if err := m.settleReservations(providerName, ""); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// settleReservations settles providerName's reservations under its current
// home, or accountID's alone when it is set, and audits each outcome as it
// was committed.
func (m *Manager) settleReservations(providerName, accountID string) error {
	home, err := m.providerSlotHome(providerName)
	if err != nil {
		return err
	}
	remove := func(id string) error { return m.credentials.RemoveAccount(providerName, id) }
	var settlements []provideraccounts.Settlement
	if accountID == "" {
		settlements, err = m.store.SettleReservations(providerName, home, remove)
	} else {
		settlements, err = m.store.SettleReservation(providerName, accountID, home, remove)
	}
	for _, settlement := range settlements {
		m.auditSettlement(providerName, settlement)
	}
	if err != nil {
		return fmt.Errorf("settle the saved %s logins of unfinished sign-ins under %s: %w", providerName, home, err)
	}
	return nil
}

func (m *Manager) auditSettlement(providerName string, settlement provideraccounts.Settlement) {
	cleared := "its reservation is cleared"
	if !settlement.Cleared {
		cleared = "saving the account metadata failed, so the next boot settles its reservation again if it is still there"
	}
	switch settlement.Outcome {
	case provideraccounts.SettlementRemoved:
		m.audit("removed %s credential slot %s of a sign-in this data root never saved; %s", providerName, settlement.AccountID, cleared)
	case provideraccounts.SettlementListed:
		m.audit("kept %s credential slot %s: a saved account names it; %s", providerName, settlement.AccountID, cleared)
	case provideraccounts.SettlementRemoveFailed:
		m.audit("could not remove %s credential slot %s of a sign-in this data root never saved; its reservation stays for the next boot", providerName, settlement.AccountID)
	case provideraccounts.SettlementMalformed:
		m.audit("left %s reservation %s alone: it does not say where it was made", providerName, settlement.AccountID)
	}
}

// providerSlotHome is the provider home this process writes providerName's
// slots under.
func (m *Manager) providerSlotHome(providerName string) (string, error) {
	paths, err := m.credentials.Paths(providerName)
	if err != nil {
		return "", err
	}
	return paths.SharedHome, nil
}
