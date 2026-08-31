package app

import (
	"context"
	"errors"

	"agent-overflow/internal/provideraccountapp"
	"agent-overflow/internal/provideraccounts"
)

type ManagedProviderAccount struct {
	provideraccounts.Account
	Active     bool   `json:"active"`
	Generation uint64 `json:"generation"`
	// NeedsLogin marks a saved account whose credential is gone, so
	// selecting it cannot work until the user signs in again. The card
	// stays listed — its metadata and quota history are still the user's
	// record of that account — but it is honest about being unusable
	// instead of failing with a filesystem error on click.
	NeedsLogin bool `json:"needsLogin"`
}

func (a *App) ListProviderAccounts() ([]ManagedProviderAccount, error) {
	if a.providerAccounts == nil {
		return []ManagedProviderAccount{}, nil
	}
	accounts, err := a.providerAccounts.ListProviderAccounts()
	if err != nil {
		return nil, err
	}
	out := make([]ManagedProviderAccount, len(accounts))
	for i, account := range accounts {
		out[i] = managedProviderAccount(account)
	}
	return out, nil
}

func (a *App) SwitchProviderAccount(providerName, accountID string) (ManagedProviderAccount, error) {
	if a.providerAccounts == nil {
		return ManagedProviderAccount{}, errors.New("provider account storage is unavailable")
	}
	account, err := a.providerAccounts.SwitchProviderAccount(providerName, accountID)
	return managedProviderAccount(account), err
}

// LoginProviderAccount runs the provider's native browser login in a
// short-lived isolated home, retains only the resulting native credential,
// atomically activates it, and registers non-secret metadata.
func (a *App) LoginProviderAccount(
	providerName string,
) (_ ManagedProviderAccount, retErr error) {
	if a.providerAccounts == nil {
		return ManagedProviderAccount{}, errors.New("provider account storage is unavailable")
	}
	account, retErr := a.providerAccounts.LoginProviderAccount(providerName)
	return managedProviderAccount(account), retErr
}

// RemoveProviderAccount deletes one saved native login. Removing the selected
// account activates the next card in display order; removing the final account
// clears the provider's canonical credential. Existing Codex processes retain
// cached authentication until the normal safe reconnect-on-send gate.
func (a *App) RemoveProviderAccount(providerName, accountID string) error {
	if a.providerAccounts == nil {
		return errors.New("provider account storage is unavailable")
	}
	return a.providerAccounts.RemoveProviderAccount(providerName, accountID)
}

// RefreshProviderAccountUsage probes one saved account without changing the
// provider-wide selection.
//
// An inactive Codex account is probed from a short-lived home seeded with only
// its credential; a rotation there reaches the canonical home only after
// selection and fingerprint validation. Claude never uses a temporary home
// here: the selected account is probed in the canonical home (the only place
// its single-use rotation may happen — see probeSelectedClaudeRateLimits) and
// an inactive one is read over HTTP without refreshing at all — see
// probeInactiveClaudeRateLimits.
//
// This manual path deliberately bypasses the usage gate's cooldown — a user
// demand should not silently coalesce away. Server-imposed 429 backoffs are
// enforced inside refreshProviderAccountUsage, scoped to the one account that
// earned them: the throttle is per-bearer, so another account's card must stay
// refreshable while the selected account waits one out.
func (a *App) RefreshProviderAccountUsage(providerName, accountID string) error {
	if a.providerAccounts == nil {
		return errors.New("provider account storage is unavailable")
	}
	return a.providerAccounts.RefreshProviderAccountUsage(providerName, accountID)
}

func (a *App) refreshProviderAccountUsage(ctx context.Context, providerName, accountID string) error {
	if a.providerAccounts == nil {
		return errors.New("provider account storage is unavailable")
	}
	return a.providerAccounts.RefreshProviderAccountUsageContext(ctx, providerName, accountID)
}

func managedProviderAccount(account provideraccountapp.ManagedAccount) ManagedProviderAccount {
	return ManagedProviderAccount{
		Account: account.Account, Active: account.Active,
		Generation: account.Generation, NeedsLogin: account.NeedsLogin,
	}
}
