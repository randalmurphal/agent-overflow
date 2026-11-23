// Package codexfake holds the earned rate-limit reset state ao-mockprovider's
// Codex app-server answers with. The harness seeds it; the mock reads and
// spends it. It lives in a file under the home directory every mock process
// inherits, the account probes included, which carry no harness environment
// (the same place updatefake.MockInstall keeps the mock's install state).
package codexfake

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"agent-overflow/internal/atomicfile"
)

const (
	stateDir  = ".ao-mockprovider"
	stateFile = "codex-reset-credits.json"
)

// Store is the reset state of the mock install under Home.
type Store struct {
	Home string
}

// Credit is one earned reset, in the app-server's terms.
type Credit struct {
	ID string `json:"id"`
	// Status is "available", "redeeming" or "redeemed".
	Status      string `json:"status"`
	GrantedAt   int64  `json:"grantedAt"`
	ExpiresAt   int64  `json:"expiresAt,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
}

// Account is the reset state of one account.
type Account struct {
	Credits []Credit `json:"credits"`
	// CountOnly answers the available count without the credit details, as
	// the backend does when its detail read fails.
	CountOnly bool `json:"countOnly,omitempty"`
	// FailNextConsume makes the next consume spend its credit and then answer
	// with an internal error: a backend reply that was lost.
	FailNextConsume bool `json:"failNextConsume,omitempty"`
	// Redeemed is the credit each idempotency key spent.
	Redeemed map[string]string `json:"redeemed,omitempty"`
	// Consumes counts consume requests that reached the account, every
	// outcome included.
	Consumes int `json:"consumes,omitempty"`
	// UsedPercent, when set, is reported as the account's 5-hour window,
	// resetting five hours after the read; a spend that resets sets it to 0.
	// Unset reports no window, as the mock does for every account.
	UsedPercent *int `json:"usedPercent,omitempty"`
}

// accountIDPattern bounds the record keys to what a credential names.
var accountIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.@-]{1,128}$`)

func (s Store) path() (string, error) {
	if s.Home == "" {
		return "", errors.New("codexfake: the mock install has no home directory")
	}
	return filepath.Join(s.Home, stateDir, stateFile), nil
}

func (s Store) load() (map[string]*Account, error) {
	path, err := s.path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]*Account{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("codexfake: read %s: %w", path, err)
	}
	accounts := map[string]*Account{}
	if err := json.Unmarshal(data, &accounts); err != nil {
		return nil, fmt.Errorf("codexfake: parse %s: %w", path, err)
	}
	return accounts, nil
}

func (s Store) save(accounts map[string]*Account) error {
	path, err := s.path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("codexfake: create %s: %w", filepath.Dir(path), err)
	}
	return atomicfile.WriteJSON(path, accounts)
}

// Set replaces one account's state; nil removes it, after which the account
// reports no reset summary.
func (s Store) Set(accountID string, account *Account) error {
	if !accountIDPattern.MatchString(accountID) {
		return fmt.Errorf("codexfake: %q is not an account id", accountID)
	}
	if account != nil {
		for i, credit := range account.Credits {
			if strings.TrimSpace(credit.ID) == "" {
				return fmt.Errorf("codexfake: credit %d has no id", i)
			}
			if !slices.Contains([]string{"available", "redeeming", "redeemed"}, credit.Status) {
				return fmt.Errorf("codexfake: credit %s has status %q", credit.ID, credit.Status)
			}
		}
	}
	accounts, err := s.load()
	if err != nil {
		return err
	}
	if account == nil {
		delete(accounts, accountID)
	} else {
		accounts[accountID] = account
	}
	return s.save(accounts)
}

// Get returns one account's state; nil when none is set.
func (s Store) Get(accountID string) (*Account, error) {
	accounts, err := s.load()
	if err != nil {
		return nil, err
	}
	return accounts[accountID], nil
}

// Clear removes every account's state.
func (s Store) Clear() error {
	path, err := s.path()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("codexfake: remove %s: %w", path, err)
	}
	return nil
}

// Summary is the `rateLimitResetCredits` value the account's rate-limit read
// carries: nil (JSON null) when no state is set.
func (s Store) Summary(accountID string) (any, error) {
	account, err := s.Get(accountID)
	if err != nil || account == nil {
		return nil, err
	}
	available := make([]Credit, 0, len(account.Credits))
	for _, credit := range account.Credits {
		if credit.Status == "available" {
			available = append(available, credit)
		}
	}
	type credit struct {
		ID          string  `json:"id"`
		ResetType   string  `json:"resetType"`
		Status      string  `json:"status"`
		GrantedAt   int64   `json:"grantedAt"`
		ExpiresAt   *int64  `json:"expiresAt"`
		Title       *string `json:"title"`
		Description *string `json:"description"`
	}
	summary := struct {
		AvailableCount int      `json:"availableCount"`
		Credits        []credit `json:"credits"`
	}{AvailableCount: len(available)}
	if account.CountOnly {
		return summary, nil
	}
	summary.Credits = []credit{}
	for _, c := range account.Credits {
		if c.Status == "redeemed" {
			continue
		}
		wire := credit{ID: c.ID, ResetType: "codexRateLimits", Status: c.Status, GrantedAt: c.GrantedAt}
		if c.ExpiresAt > 0 {
			expires := c.ExpiresAt
			wire.ExpiresAt = &expires
		}
		if c.Title != "" {
			title := c.Title
			wire.Title = &title
		}
		if c.Description != "" {
			description := c.Description
			wire.Description = &description
		}
		summary.Credits = append(summary.Credits, wire)
	}
	return summary, nil
}

// FiveHourWindow is the account's seeded 5-hour usage, nil when none is set.
func (s Store) FiveHourWindow(accountID string) (*int, error) {
	account, err := s.Get(accountID)
	if err != nil || account == nil {
		return nil, err
	}
	return account.UsedPercent, nil
}

// Consume spends the named credit the way the backend does: a key already
// redeemed answers alreadyRedeemed, and the credit must be available. AO
// names a credit on every consume, so the backend's pick for a consume that
// names none is not impersonated: it is an error. lost reports an armed
// FailNextConsume: the credit was spent and the answer is to be lost.
func (s Store) Consume(accountID, idempotencyKey, creditID string) (outcome string, lost bool, err error) {
	if creditID == "" {
		return "", false, errors.New("codexfake: a consume must name its credit")
	}
	accounts, err := s.load()
	if err != nil {
		return "", false, err
	}
	account := accounts[accountID]
	if account == nil {
		return "noCredit", false, nil
	}
	account.Consumes++
	defer func() {
		if err == nil {
			err = s.save(accounts)
		}
	}()
	if _, done := account.Redeemed[idempotencyKey]; done {
		return "alreadyRedeemed", false, nil
	}
	pick := slices.IndexFunc(account.Credits, func(credit Credit) bool {
		return credit.ID == creditID && credit.Status == "available"
	})
	if pick < 0 {
		return "noCredit", false, nil
	}
	account.Credits[pick].Status = "redeemed"
	if account.Redeemed == nil {
		account.Redeemed = map[string]string{}
	}
	account.Redeemed[idempotencyKey] = account.Credits[pick].ID
	lost = account.FailNextConsume
	account.FailNextConsume = false
	if account.UsedPercent != nil {
		refilled := 0
		account.UsedPercent = &refilled
	}
	return "reset", lost, nil
}
