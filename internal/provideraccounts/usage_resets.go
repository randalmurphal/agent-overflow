package provideraccounts

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// UsageResets is the list of Codex usage resets an account holds, as its
// last reading named them, for the account card to offer.
type UsageResets struct {
	// Revision moves on every change of the list and when a spend begins, so
	// a spend can refuse a list the person did not see (BeginResetSpend). It
	// never goes back, also when the list becomes empty.
	Revision uint64 `json:"revision"`
	// Options are earliest-expiring first. Empty means the reading listed
	// none.
	Options []UsageResetOption `json:"options"`
	// Unlisted is how many available resets the reading counted beyond the
	// ones it listed: the backend caps its list. Nothing names them, so the
	// card shows them and offers no spend of them.
	Unlisted int `json:"unlisted,omitempty"`
}

// UsageResetOption is one reset the account holds.
type UsageResetOption struct {
	// ID names the credit to Codex. It is never empty and is unique within
	// the list.
	ID string `json:"id"`
	// Kind is the provider's kind of reset (Codex's resetType). The card
	// groups by it; Label is display only.
	Kind string `json:"kind"`
	// Label is what the person reads, such as "Full reset".
	Label string `json:"label"`
	// Refills says, in words, what spending it clears.
	Refills string `json:"refills,omitempty"`
	// ExpiresAt is when it expires, in Unix seconds; 0 when it does not.
	ExpiresAt int64 `json:"expiresAt,omitempty"`
	// Usable is false when Codex says it cannot be used now, and Reason then
	// says why.
	Usable bool   `json:"usable"`
	Reason string `json:"reason,omitempty"`
}

// ResetReading is the resets one reading of an account named.
type ResetReading struct {
	Options  []UsageResetOption
	Unlisted int
}

// PendingReset is a spend whose consume was attempted and that Codex has not
// settled: it may have used the reset. An account has at most one. While it
// stands no other spend of the account begins, and Finish sends the same Key
// and CreditID again, which never takes a second credit.
//
// It is written immediately before its consume (BeginResetSpend) and ends
// only when Codex settles a request sent under its key, whatever the outcome,
// when the request that first carried it is refused or never sent
// (SettleResetSpend), or when the account is removed (Remove). A reading
// never ends it: a credit gone from the list may be gone to this spend.
type PendingReset struct {
	// ID names this spend to the card and to Finish. It is not the key,
	// which stays in the backend.
	ID       string `json:"id"`
	Key      string `json:"key"`
	CreditID string `json:"creditId"`
	// Label and Refills are the option's when the spend began, for the card.
	Label   string `json:"label"`
	Refills string `json:"refills,omitempty"`
	// StartedAt is Unix milliseconds.
	StartedAt int64 `json:"startedAt"`
}

func cloneUsageResets(resets *UsageResets) *UsageResets {
	if resets == nil {
		return nil
	}
	copy := *resets
	copy.Options = slices.Clone(resets.Options)
	return &copy
}

var (
	// errResetReading refuses a reading that names an option without an id
	// or names one twice (a spend names its option by ID), or that counts
	// fewer than no unlisted resets.
	errResetReading = errors.New("provideraccounts: usage reset options need distinct ids, and the unlisted count can't be negative")
	// ErrResetListStale refuses a spend chosen from a list that is no longer
	// the account's: another revision, or an option it no longer holds.
	ErrResetListStale = errors.New("provideraccounts: the usage reset list changed")
)

// ResetPendingError refuses a spend while another is unsettled.
type ResetPendingError struct{ Pending PendingReset }

func (e *ResetPendingError) Error() string {
	return fmt.Sprintf("provideraccounts: a %s spend is not settled yet", e.Pending.Label)
}

// ResetUnusableError refuses a spend of an option Codex says cannot be used
// now.
type ResetUnusableError struct{ Option UsageResetOption }

func (e *ResetUnusableError) Error() string {
	return fmt.Sprintf("provideraccounts: the %s can't be used now: %s", e.Option.Label, e.Option.Reason)
}

// SetUsageResets records the resets a reading of the account named. An
// unchanged list writes nothing and copies nothing. It returns the row only
// when it changed. A removed account is not an error: there is nothing to
// record.
func (s *Store) SetUsageResets(providerName, accountID string, reading ResetReading) (Account, bool, error) {
	if err := checkReading(reading); err != nil {
		return Account{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, existed := s.state.Providers[providerName]
	index := indexOfAccount(previous.Accounts, accountID)
	if index < 0 || !listChanged(previous.Accounts[index].UsageResets, reading) {
		return Account{}, false, nil
	}
	state := cloneProviderState(previous)
	setResetList(&state.Accounts[index], reading)
	s.state.Providers[providerName] = state
	if err := s.saveLocked(); err != nil {
		restoreProviderState(s.state.Providers, providerName, previous, existed)
		return Account{}, false, err
	}
	return cloneAccount(state.Accounts[index]), true, nil
}

func checkReading(reading ResetReading) error {
	if reading.Unlisted < 0 {
		return errResetReading
	}
	for i := range reading.Options {
		if strings.TrimSpace(reading.Options[i].ID) == "" {
			return errResetReading
		}
		for j := range i {
			if reading.Options[i].ID == reading.Options[j].ID {
				return errResetReading
			}
		}
	}
	return nil
}

func listChanged(current *UsageResets, reading ResetReading) bool {
	return current == nil || current.Unlisted != reading.Unlisted || !slices.Equal(current.Options, reading.Options)
}

func readingOf(resets *UsageResets) ResetReading {
	if resets == nil {
		return ResetReading{}
	}
	return ResetReading{Options: resets.Options, Unlisted: resets.Unlisted}
}

// setResetList writes reading as the row's list and moves its revision.
func setResetList(account *Account, reading ResetReading) {
	next := &UsageResets{Options: slices.Clone(reading.Options), Unlisted: reading.Unlisted}
	if next.Options == nil {
		next.Options = []UsageResetOption{}
	}
	if account.UsageResets != nil {
		next.Revision = account.UsageResets.Revision
	}
	next.Revision++
	account.UsageResets = next
}

// PendingReset returns the account's unsettled spend.
func (s *Store) PendingReset(providerName, accountID string) (PendingReset, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	pending, ok := s.state.Providers[providerName].PendingResets[accountID]
	return pending, ok
}

// CheckResetSpend answers whether a spend of optionID, chosen from the list
// at revision, would begin now, and records nothing. Its refusals are
// BeginResetSpend's.
func (s *Store) CheckResetSpend(providerName, accountID string, revision uint64, optionID string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, _, err := checkResetSpend(s.state.Providers[providerName], providerName, accountID, revision, optionID)
	return err
}

// checkResetSpend is the one rule for whether a spend may begin: the account
// is saved, has no unsettled spend, still holds the list at revision, and that
// list's optionID is usable. It returns the account's row index and the
// option.
func checkResetSpend(state ProviderState, providerName, accountID string, revision uint64, optionID string) (int, UsageResetOption, error) {
	index := indexOfAccount(state.Accounts, accountID)
	if index < 0 {
		return -1, UsageResetOption{}, fmt.Errorf("provideraccounts: account %q not found for %s", accountID, providerName)
	}
	if current, ok := state.PendingResets[accountID]; ok {
		return -1, UsageResetOption{}, &ResetPendingError{Pending: current}
	}
	resets := state.Accounts[index].UsageResets
	if resets == nil || resets.Revision != revision {
		return -1, UsageResetOption{}, ErrResetListStale
	}
	at := slices.IndexFunc(resets.Options, func(option UsageResetOption) bool { return option.ID == optionID })
	if at < 0 {
		return -1, UsageResetOption{}, ErrResetListStale
	}
	option := resets.Options[at]
	if !option.Usable {
		return -1, UsageResetOption{}, &ResetUnusableError{Option: option}
	}
	return index, option, nil
}

// BeginResetSpend records a spend of optionID, chosen from the list at
// revision, immediately before its consume is written. start carries the
// spend's ID, Key and StartedAt; the credit, label and refills come from the
// option. In the same save it moves the revision, so from here on no request
// chosen from the old list passes, whatever fails later. The refusals are
// checkResetSpend's: *ResetPendingError, ErrResetListStale and
// *ResetUnusableError.
func (s *Store) BeginResetSpend(providerName, accountID string, revision uint64, optionID string, start PendingReset) (PendingReset, error) {
	if strings.TrimSpace(start.ID) == "" || strings.TrimSpace(start.Key) == "" {
		return PendingReset{}, errors.New("provideraccounts: a reset spend needs an id and a key")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, existed := s.state.Providers[providerName]
	index, option, err := checkResetSpend(previous, providerName, accountID, revision, optionID)
	if err != nil {
		return PendingReset{}, err
	}
	pending := start
	pending.CreditID, pending.Label, pending.Refills = option.ID, option.Label, option.Refills
	state := cloneProviderState(previous)
	setResetList(&state.Accounts[index], readingOf(state.Accounts[index].UsageResets))
	if state.PendingResets == nil {
		state.PendingResets = map[string]PendingReset{}
	}
	state.PendingResets[accountID] = pending
	s.state.Providers[providerName] = state
	if err := s.saveLocked(); err != nil {
		restoreProviderState(s.state.Providers, providerName, previous, existed)
		return PendingReset{}, err
	}
	return pending, nil
}

// SettleResetSpend ends the account's unsettled spend under key in one save,
// and with creditGone (Codex's answer left the spend's credit used or gone)
// drops that credit from the stored list in the same save. A spend that is
// not the account's (another key, none at all) is left alone. The reading
// after the answer is recorded on its own (SetUsageResets), so it can't keep
// the answer from settling.
func (s *Store) SettleResetSpend(providerName, accountID, key string, creditGone bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, existed := s.state.Providers[providerName]
	index := indexOfAccount(previous.Accounts, accountID)
	pending, ok := previous.PendingResets[accountID]
	if index < 0 || !ok || pending.Key != key {
		return false, nil
	}
	state := cloneProviderState(previous)
	delete(state.PendingResets, accountID)
	if len(state.PendingResets) == 0 {
		state.PendingResets = nil
	}
	account := &state.Accounts[index]
	if creditGone && account.UsageResets != nil {
		reading := readingOf(account.UsageResets)
		kept := slices.DeleteFunc(slices.Clone(reading.Options), func(option UsageResetOption) bool {
			return option.ID == pending.CreditID
		})
		if len(kept) != len(reading.Options) {
			reading.Options = kept
			setResetList(account, reading)
		}
	}
	s.state.Providers[providerName] = state
	if err := s.saveLocked(); err != nil {
		restoreProviderState(s.state.Providers, providerName, previous, existed)
		return false, err
	}
	return true, nil
}

func clonePendingResets(pending map[string]PendingReset) map[string]PendingReset {
	return maps.Clone(pending)
}
