package provideraccounts

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/uuid"
)

// Reserved slots: which saved slots a data root may delete without a user
// asking.
//
// Every data root on a machine shares the provider home and its slot
// directory, so a slot that this root's account list does not name is
// usually another root's live login. The only slot a root can prove it owns
// without a listing is one for an id it minted itself: ReserveAccountID
// persists the id, with this data root and the provider home its slot is
// written under, before any slot byte for it exists, and inserting the
// account's row clears it. A reservation still held names a sign-in that
// never saved its account. Only the data root that made it settles it, and
// only against the home it names; a copied metadata file or a run against
// another home leaves it exactly as it is. Slots of earlier releases carry no
// reservation and are never deleted.

// ReservedSlot is where one reservation was made.
type ReservedSlot struct {
	// Root is the absolute directory of the metadata store that reserved
	// the id.
	Root string `json:"root"`
	// Home is the absolute provider home (Paths.SharedHome) that the id's
	// slot is written under.
	Home string `json:"home"`
}

// ReserveAccountID mints the id of a new account and persists it as reserved
// under this store's data root and providerHome before returning it, so a
// crash anywhere between the slot write and the row insert leaves a slot this
// root can find and delete. It is the only source of a new account id. A
// failed save returns an error and no id.
func (s *Store) ReserveAccountID(providerName, providerHome string) (string, error) {
	if strings.TrimSpace(providerName) == "" {
		return "", errors.New("provideraccounts: reserve an account id for an empty provider")
	}
	if !filepath.IsAbs(providerHome) {
		return "", fmt.Errorf("provideraccounts: reserve a %s account id under a provider home that is not absolute: %q", providerName, providerHome)
	}
	accountID := uuid.NewString()
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, existed := s.state.Providers[providerName]
	state := cloneProviderState(previous)
	if state.ReservedSlots == nil {
		state.ReservedSlots = make(map[string]ReservedSlot)
	}
	state.ReservedSlots[accountID] = ReservedSlot{Root: s.root, Home: filepath.Clean(providerHome)}
	s.state.Providers[providerName] = state
	if err := s.saveLocked(); err != nil {
		restoreProviderState(s.state.Providers, providerName, previous, existed)
		return "", fmt.Errorf("provideraccounts: reserve a new %s account id: %w", providerName, err)
	}
	return accountID, nil
}

// clearReservation forgets accountID's reservation once its row exists. The
// caller holds mu and saves state.
func (state *ProviderState) clearReservation(accountID string) {
	delete(state.ReservedSlots, accountID)
	if len(state.ReservedSlots) == 0 {
		state.ReservedSlots = nil
	}
}

// SettlementOutcome is what settling one reservation did.
type SettlementOutcome string

const (
	// SettlementRemoved: no saved account names the id, and its slot is
	// gone.
	SettlementRemoved SettlementOutcome = "removed"
	// SettlementListed: a saved account names the id, so its slot holds that
	// account's login and was left alone.
	SettlementListed SettlementOutcome = "listed"
	// SettlementRemoveFailed: the slot could not be removed. The
	// reservation stays for the next settle.
	SettlementRemoveFailed SettlementOutcome = "remove_failed"
	// SettlementMalformed: the reservation does not say where it was made.
	// Nothing was deleted and it stays.
	SettlementMalformed SettlementOutcome = "malformed"
)

// Settlement reports one reservation's settle.
type Settlement struct {
	AccountID string
	Outcome   SettlementOutcome
	// Cleared reports that the reservation is gone from the saved metadata.
	// A removal or a listing whose save failed has Cleared false.
	Cleared bool
}

// SettleReservation settles accountID's reservation, the one a failed
// operation of this store made under providerHome: the same rules as
// SettleReservations, for that id alone. An id with no reservation here
// settles nothing.
func (s *Store) SettleReservation(providerName, accountID, providerHome string, remove func(accountID string) error) ([]Settlement, error) {
	return s.settle(providerName, providerHome, remove, func(id string) bool { return id == accountID })
}

// SettleReservations settles every reservation that this data root made
// under providerHome. A reservation whose account a saved row names is
// cleared and its slot kept; any other has its slot deleted by remove and is
// then cleared. A failed remove keeps the reservation for the next settle. A
// reservation made by another data root or under another home is left
// exactly as it is and not reported; a malformed one deletes nothing, stays,
// and is reported. The error joins every failed removal, every malformed
// reservation and a failed save.
//
// The listing check, the removals and the save all happen under the store
// lock, so no account row can appear for an id between the check and the
// removal of its slot. remove must not call back into the store.
func (s *Store) SettleReservations(providerName, providerHome string, remove func(accountID string) error) ([]Settlement, error) {
	return s.settle(providerName, providerHome, remove, func(string) bool { return true })
}

func (s *Store) settle(
	providerName, providerHome string,
	remove func(accountID string) error,
	selected func(accountID string) bool,
) ([]Settlement, error) {
	home := filepath.Clean(providerHome)
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, existed := s.state.Providers[providerName]
	if len(previous.ReservedSlots) == 0 {
		return nil, nil
	}
	state := cloneProviderState(previous)
	var settlements []Settlement
	var errs []error
	for _, accountID := range slices.Sorted(maps.Keys(previous.ReservedSlots)) {
		if !selected(accountID) {
			continue
		}
		settlement, err := s.settleLocked(&state, providerName, home, accountID, remove)
		if err != nil {
			errs = append(errs, err)
		}
		if settlement.Outcome != "" {
			settlements = append(settlements, settlement)
		}
	}
	cleared := false
	for _, settlement := range settlements {
		cleared = cleared || settlement.Cleared
	}
	if cleared {
		s.state.Providers[providerName] = state
		if err := s.saveLocked(); err != nil {
			restoreProviderState(s.state.Providers, providerName, previous, existed)
			for i := range settlements {
				settlements[i].Cleared = false
			}
			errs = append(errs, fmt.Errorf("provideraccounts: save the settled %s reservations: %w", providerName, err))
		}
	}
	return settlements, errors.Join(errs...)
}

// settleLocked settles one reservation in state. A reservation of another
// data root or another home returns a zero Settlement: it is not this
// call's to settle. The caller holds mu and saves state.
func (s *Store) settleLocked(
	state *ProviderState,
	providerName, home, accountID string,
	remove func(accountID string) error,
) (Settlement, error) {
	reserved := state.ReservedSlots[accountID]
	if !filepath.IsAbs(reserved.Root) || !filepath.IsAbs(reserved.Home) {
		return Settlement{AccountID: accountID, Outcome: SettlementMalformed}, fmt.Errorf(
			"provideraccounts: %s reservation %s names no absolute data root and provider home (root %q, home %q); nothing was deleted",
			providerName, accountID, reserved.Root, reserved.Home,
		)
	}
	if filepath.Clean(reserved.Root) != s.root || filepath.Clean(reserved.Home) != home {
		return Settlement{}, nil
	}
	if indexOfAccount(state.Accounts, accountID) >= 0 {
		state.clearReservation(accountID)
		return Settlement{AccountID: accountID, Outcome: SettlementListed, Cleared: true}, nil
	}
	if err := remove(accountID); err != nil {
		return Settlement{AccountID: accountID, Outcome: SettlementRemoveFailed}, fmt.Errorf(
			"provideraccounts: remove the %s slot of unsaved account %s: %w", providerName, accountID, err,
		)
	}
	state.clearReservation(accountID)
	return Settlement{AccountID: accountID, Outcome: SettlementRemoved, Cleared: true}, nil
}
