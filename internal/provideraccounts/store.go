package provideraccounts

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/provider"
)

const (
	stateFilename = "provider-accounts.json"
	stateVersion  = 1
)

// Account is safe frontend-facing metadata for one native provider login.
// Credential material is deliberately absent.
type Account struct {
	ID               string                       `json:"id"`
	Provider         string                       `json:"provider"`
	Email            string                       `json:"email,omitempty"`
	DisplayName      string                       `json:"displayName,omitempty"`
	SubscriptionType string                       `json:"subscriptionType,omitempty"`
	TokenSource      string                       `json:"tokenSource,omitempty"`
	APIProvider      string                       `json:"apiProvider,omitempty"`
	AddedAt          int64                        `json:"addedAt"`
	LastUsedAt       int64                        `json:"lastUsedAt"`
	RateLimits       *provider.RateLimitsSnapshot `json:"rateLimits,omitempty"`
}

// ProviderState is the persisted account set and current selection for one
// provider.
type ProviderState struct {
	ActiveAccountID string    `json:"activeAccountId,omitempty"`
	Generation      uint64    `json:"generation"`
	Accounts        []Account `json:"accounts"`
}

type persistedState struct {
	Version   int                      `json:"version"`
	Providers map[string]ProviderState `json:"providers"`
}

// Store persists account metadata only. It is safe for concurrent probe,
// login, switch, and rate-limit goroutines.
type Store struct {
	mu    sync.RWMutex
	path  string
	state persistedState
}

func NewStore(configDir string) (*Store, error) {
	if strings.TrimSpace(configDir) == "" {
		return nil, errors.New("provideraccounts: empty config directory")
	}
	s := &Store{
		path: filepath.Join(configDir, stateFilename),
		state: persistedState{
			Version:   stateVersion,
			Providers: make(map[string]ProviderState),
		},
	}
	var loaded persistedState
	found, err := atomicfile.ReadJSON(s.path, &loaded)
	if err != nil {
		return nil, fmt.Errorf("provideraccounts: load metadata: %w", err)
	}
	if !found {
		return s, nil
	}
	if loaded.Version != stateVersion {
		return nil, fmt.Errorf("provideraccounts: unsupported metadata version %d", loaded.Version)
	}
	if loaded.Providers == nil {
		loaded.Providers = make(map[string]ProviderState)
	}
	s.state = loaded
	return s, nil
}

func (s *Store) List(providerName string, now time.Time) []Account {
	s.mu.RLock()
	state := s.state.Providers[providerName]
	accounts := cloneAccounts(state.Accounts)
	s.mu.RUnlock()
	for i := range accounts {
		accounts[i].RateLimits = resetExpiredLimits(accounts[i].RateLimits, now)
	}
	sort.SliceStable(accounts, func(i, j int) bool {
		if accounts[i].ID == state.ActiveAccountID {
			return true
		}
		if accounts[j].ID == state.ActiveAccountID {
			return false
		}
		return accounts[i].LastUsedAt > accounts[j].LastUsedAt
	})
	return accounts
}

func (s *Store) Active(providerName string, now time.Time) (Account, bool) {
	s.mu.RLock()
	state := s.state.Providers[providerName]
	for _, account := range state.Accounts {
		if account.ID == state.ActiveAccountID {
			out := cloneAccount(account)
			s.mu.RUnlock()
			out.RateLimits = resetExpiredLimits(out.RateLimits, now)
			return out, true
		}
	}
	s.mu.RUnlock()
	return Account{}, false
}

func (s *Store) Get(providerName, accountID string, now time.Time) (Account, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, account := range s.state.Providers[providerName].Accounts {
		if account.ID == accountID {
			out := cloneAccount(account)
			out.RateLimits = resetExpiredLimits(out.RateLimits, now)
			return out, true
		}
	}
	return Account{}, false
}

func (s *Store) Generation(providerName string) uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.Providers[providerName].Generation
}

func (s *Store) FindByEmail(providerName, email string) (Account, bool) {
	email = strings.TrimSpace(email)
	if email == "" {
		return Account{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, account := range s.state.Providers[providerName].Accounts {
		if strings.EqualFold(account.Email, email) {
			return cloneAccount(account), true
		}
	}
	return Account{}, false
}

// UpsertAndActivate registers account, deduplicating by a non-empty email
// within the provider, and makes it current. The caller owns credential-file
// activation and must call this only after that succeeds.
func (s *Store) UpsertAndActivate(account Account) (Account, error) {
	return s.upsertAndActivate(account, false)
}

// UpsertAndActivateCredential is UpsertAndActivate for a verified canonical
// credential replacement. It advances the selection generation even when the
// provider identity is unchanged so auth-caching processes (Codex app-server)
// reconnect before their next turn.
func (s *Store) UpsertAndActivateCredential(account Account) (Account, error) {
	return s.upsertAndActivate(account, true)
}

func (s *Store) upsertAndActivate(
	account Account,
	credentialChanged bool,
) (Account, error) {
	if err := validateAccount(account); err != nil {
		return Account{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	previous, existed := s.state.Providers[account.Provider]
	state := cloneProviderState(previous)
	now := time.Now().UnixMilli()
	index := -1
	for i := range state.Accounts {
		if state.Accounts[i].ID == account.ID ||
			(account.Email != "" && strings.EqualFold(state.Accounts[i].Email, account.Email)) {
			index = i
			break
		}
	}
	if index >= 0 {
		existing := state.Accounts[index]
		account.ID = existing.ID
		account.AddedAt = existing.AddedAt
		if account.RateLimits == nil {
			account.RateLimits = existing.RateLimits
		}
	} else if account.AddedAt == 0 {
		account.AddedAt = now
	}
	account.LastUsedAt = now
	if index >= 0 {
		state.Accounts[index] = cloneAccount(account)
	} else {
		state.Accounts = append(state.Accounts, cloneAccount(account))
	}
	if credentialChanged || state.ActiveAccountID != account.ID {
		state.Generation++
	}
	state.ActiveAccountID = account.ID
	s.state.Providers[account.Provider] = state
	if err := s.saveLocked(); err != nil {
		restoreProviderState(s.state.Providers, account.Provider, previous, existed)
		return Account{}, err
	}
	return cloneAccount(account), nil
}

func (s *Store) Activate(providerName, accountID string) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, existed := s.state.Providers[providerName]
	state := cloneProviderState(previous)
	for i := range state.Accounts {
		if state.Accounts[i].ID != accountID {
			continue
		}
		if state.ActiveAccountID != accountID {
			state.Generation++
		}
		state.ActiveAccountID = accountID
		state.Accounts[i].LastUsedAt = time.Now().UnixMilli()
		s.state.Providers[providerName] = state
		if err := s.saveLocked(); err != nil {
			restoreProviderState(s.state.Providers, providerName, previous, existed)
			return Account{}, err
		}
		return cloneAccount(state.Accounts[i]), nil
	}
	return Account{}, fmt.Errorf("provideraccounts: account %q not found for %s", accountID, providerName)
}

// AdvanceActiveCredential records that the selected account's canonical
// credential changed without changing the selected account. Auth-caching
// provider processes use the generation to reconnect before their next turn.
func (s *Store) AdvanceActiveCredential(providerName, accountID string) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	previous, existed := s.state.Providers[providerName]
	state := cloneProviderState(previous)
	if state.ActiveAccountID != accountID {
		return Account{}, fmt.Errorf(
			"provideraccounts: account %q is not active for %s",
			accountID,
			providerName,
		)
	}
	for i := range state.Accounts {
		if state.Accounts[i].ID != accountID {
			continue
		}
		state.Generation++
		s.state.Providers[providerName] = state
		if err := s.saveLocked(); err != nil {
			restoreProviderState(s.state.Providers, providerName, previous, existed)
			return Account{}, err
		}
		return cloneAccount(state.Accounts[i]), nil
	}
	return Account{}, fmt.Errorf(
		"provideraccounts: active account %q not found for %s",
		accountID,
		providerName,
	)
}

// Remove deletes one saved account and, when that account is active, moves the
// selection to replacementAccountID. An empty replacement is valid only when
// the removed account is the provider's final saved account. Credential
// activation/deletion is owned by the caller and must complete first.
func (s *Store) Remove(providerName, accountID, replacementAccountID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	previous, existed := s.state.Providers[providerName]
	state := cloneProviderState(previous)
	index := -1
	for i := range state.Accounts {
		if state.Accounts[i].ID == accountID {
			index = i
			break
		}
	}
	if index < 0 {
		return fmt.Errorf("provideraccounts: account %q not found for %s", accountID, providerName)
	}

	removingActive := state.ActiveAccountID == accountID
	if !removingActive && replacementAccountID != "" {
		return errors.New("provideraccounts: replacement account is only valid when removing the active account")
	}
	if removingActive {
		replacementIndex := -1
		for i := range state.Accounts {
			if state.Accounts[i].ID == replacementAccountID {
				replacementIndex = i
				break
			}
		}
		switch {
		case len(state.Accounts) == 1 && replacementAccountID != "":
			return errors.New("provideraccounts: final account cannot have a replacement")
		case len(state.Accounts) > 1 && (replacementIndex < 0 || replacementIndex == index):
			return fmt.Errorf(
				"provideraccounts: replacement account %q not found for %s",
				replacementAccountID,
				providerName,
			)
		}
		state.ActiveAccountID = replacementAccountID
		state.Generation++
		if replacementIndex >= 0 {
			state.Accounts[replacementIndex].LastUsedAt = time.Now().UnixMilli()
		}
	}

	state.Accounts = append(state.Accounts[:index], state.Accounts[index+1:]...)
	s.state.Providers[providerName] = state
	if err := s.saveLocked(); err != nil {
		restoreProviderState(s.state.Providers, providerName, previous, existed)
		return err
	}
	return nil
}

// UpdateMetadata enriches one saved account without changing the active
// selection, generation, timestamps, or cached rate limits. It is used when a
// newer provider account/read response supplies identity fields that were
// absent in legacy metadata.
func (s *Store) UpdateMetadata(account Account) (Account, error) {
	if err := validateAccount(account); err != nil {
		return Account{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, existed := s.state.Providers[account.Provider]
	state := cloneProviderState(previous)
	index := -1
	for i := range state.Accounts {
		if state.Accounts[i].ID != account.ID &&
			account.Email != "" &&
			strings.EqualFold(state.Accounts[i].Email, account.Email) {
			return Account{}, fmt.Errorf(
				"provideraccounts: email %q already belongs to account %q for %s",
				account.Email,
				state.Accounts[i].ID,
				account.Provider,
			)
		}
		if state.Accounts[i].ID == account.ID {
			index = i
		}
	}
	if index < 0 {
		return Account{}, fmt.Errorf(
			"provideraccounts: account %q not found for %s",
			account.ID,
			account.Provider,
		)
	}
	existing := state.Accounts[index]
	account.AddedAt = existing.AddedAt
	account.LastUsedAt = existing.LastUsedAt
	account.RateLimits = existing.RateLimits
	state.Accounts[index] = cloneAccount(account)
	s.state.Providers[account.Provider] = state
	if err := s.saveLocked(); err != nil {
		restoreProviderState(s.state.Providers, account.Provider, previous, existed)
		return Account{}, err
	}
	return cloneAccount(account), nil
}

func (s *Store) RememberRateLimits(providerName, accountID string, snapshot provider.RateLimitsSnapshot) error {
	if accountID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, existed := s.state.Providers[providerName]
	state := cloneProviderState(previous)
	for i := range state.Accounts {
		if state.Accounts[i].ID != accountID {
			continue
		}
		copy := cloneSnapshot(snapshot)
		copy.Provider = providerName
		copy.AccountID = accountID
		state.Accounts[i].RateLimits = &copy
		s.state.Providers[providerName] = state
		if err := s.saveLocked(); err != nil {
			restoreProviderState(s.state.Providers, providerName, previous, existed)
			return err
		}
		return nil
	}
	return nil
}

func (s *Store) saveLocked() error {
	if err := atomicfile.WriteJSON(s.path, s.state); err != nil {
		return fmt.Errorf("provideraccounts: save metadata: %w", err)
	}
	return nil
}

func validateAccount(account Account) error {
	if strings.TrimSpace(account.ID) == "" {
		return errors.New("provideraccounts: account id is required")
	}
	if strings.TrimSpace(account.Provider) == "" {
		return errors.New("provideraccounts: provider is required")
	}
	return nil
}

func cloneAccounts(accounts []Account) []Account {
	out := make([]Account, len(accounts))
	for i := range accounts {
		out[i] = cloneAccount(accounts[i])
	}
	return out
}

func cloneProviderState(state ProviderState) ProviderState {
	state.Accounts = cloneAccounts(state.Accounts)
	return state
}

func restoreProviderState(
	providers map[string]ProviderState,
	providerName string,
	previous ProviderState,
	existed bool,
) {
	if existed {
		providers[providerName] = previous
		return
	}
	delete(providers, providerName)
}

func cloneAccount(account Account) Account {
	if account.RateLimits != nil {
		copy := cloneSnapshot(*account.RateLimits)
		account.RateLimits = &copy
	}
	return account
}

func cloneSnapshot(snapshot provider.RateLimitsSnapshot) provider.RateLimitsSnapshot {
	snapshot.Limits = append([]provider.RateLimitEntry(nil), snapshot.Limits...)
	return snapshot
}

func resetExpiredLimits(snapshot *provider.RateLimitsSnapshot, now time.Time) *provider.RateLimitsSnapshot {
	if snapshot == nil {
		return nil
	}
	copy := cloneSnapshot(*snapshot)
	nowUnix := now.Unix()
	for i := range copy.Limits {
		entry := &copy.Limits[i]
		if entry.ResetsAt <= 0 || entry.ResetsAt > nowUnix {
			continue
		}
		entry.UsedPercent = 0
		if entry.WindowMins <= 0 {
			entry.ResetsAt = 0
			continue
		}
		windowSeconds := int64(entry.WindowMins) * 60
		elapsedWindows := (nowUnix-entry.ResetsAt)/windowSeconds + 1
		entry.ResetsAt += elapsedWindows * windowSeconds
	}
	return &copy
}
