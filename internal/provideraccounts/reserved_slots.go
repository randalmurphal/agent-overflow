package provideraccounts

import (
	"errors"
	"fmt"
	"maps"
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
// persists the id before any slot byte for it exists, inserting the
// account's row clears it, and a reservation still held at boot names a
// sign-in that never saved its account. ReleaseReservedSlots deletes those
// slots and nothing else. Slots of earlier releases carry no reservation and
// are never pruned.

// ReserveAccountID mints the id of a new account and persists it as reserved
// before returning it, so a crash anywhere between the slot write and the
// row insert leaves a slot this root can find and delete. It is the only
// source of a new account id. A failed save returns an error and no id.
func (s *Store) ReserveAccountID(providerName string) (string, error) {
	if strings.TrimSpace(providerName) == "" {
		return "", errors.New("provideraccounts: reserve an account id for an empty provider")
	}
	accountID := uuid.NewString()
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, existed := s.state.Providers[providerName]
	state := cloneProviderState(previous)
	if state.ReservedSlots == nil {
		state.ReservedSlots = make(map[string]bool)
	}
	state.ReservedSlots[accountID] = true
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

// ReservedSlotRelease reports what ReleaseReservedSlots did with each
// reservation it settled.
type ReservedSlotRelease struct {
	// Removed are the ids whose slot was deleted and whose reservation is
	// gone.
	Removed []string
	// Listed are the ids that a saved account names. Their reservation is
	// gone and their slot is untouched: it holds that account's login.
	Listed []string
}

// ReleaseReservedSlots settles every reservation this store holds for
// providerName: an id that a saved account names loses its reservation and
// keeps its slot; any other id has its slot deleted by remove and then
// loses its reservation. A failed remove keeps that id reserved, so the
// next call retries it, and its error is returned with the others.
//
// The listing check, the removals and the save all happen under the store
// lock, so no account row can appear for an id between the check and the
// removal of its slot. remove must not call back into the store.
func (s *Store) ReleaseReservedSlots(providerName string, remove func(accountID string) error) (ReservedSlotRelease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, existed := s.state.Providers[providerName]
	if len(previous.ReservedSlots) == 0 {
		return ReservedSlotRelease{}, nil
	}
	state := cloneProviderState(previous)
	var release ReservedSlotRelease
	var errs []error
	for _, accountID := range slices.Sorted(maps.Keys(previous.ReservedSlots)) {
		if indexOfAccount(state.Accounts, accountID) >= 0 {
			state.clearReservation(accountID)
			release.Listed = append(release.Listed, accountID)
			continue
		}
		if err := remove(accountID); err != nil {
			errs = append(errs, fmt.Errorf("remove %s slot %s: %w", providerName, accountID, err))
			continue
		}
		state.clearReservation(accountID)
		release.Removed = append(release.Removed, accountID)
	}
	if len(release.Removed) > 0 || len(release.Listed) > 0 {
		s.state.Providers[providerName] = state
		if err := s.saveLocked(); err != nil {
			// The slots are gone; their reservations stay, and the next call
			// finds nothing to delete for them and clears them.
			restoreProviderState(s.state.Providers, providerName, previous, existed)
			errs = append(errs, err)
		}
	}
	return release, errors.Join(errs...)
}
