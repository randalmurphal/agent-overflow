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

//ao:scope access:admin
//ao:route home
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

//ao:scope access:admin
//ao:route home
func (a *App) SwitchProviderAccount(providerName, accountID string) (ManagedProviderAccount, error) {
	if a.providerAccounts == nil {
		return ManagedProviderAccount{}, errors.New("provider account storage is unavailable")
	}
	account, err := a.providerAccounts.SwitchProviderAccount(providerName, accountID)
	return managedProviderAccount(account), err
}

// The four sign-in calls. A provider login is a SESSION rather than one long
// call, because the person finishing it may not be at this machine: the link
// has to reach them and their answer has to come back to the flow still
// holding it open. Each returns as soon as it has done its part; every
// transition after that arrives on `provider:login`.
//
// StartProviderLogin begins one and returns as soon as there is something to
// show — for `browser` a page is opened on this host, for `remote` a link (and
// on Codex a device code) comes back to be shown wherever the caller is.
// Whatever was already running for this provider is cancelled.
//
//ao:scope access:admin
//ao:route home
func (a *App) StartProviderLogin(
	providerName string,
	method provideraccountapp.LoginMethod,
) (provideraccountapp.LoginState, error) {
	if a.providerAccounts == nil {
		return provideraccountapp.IdleLoginState(providerName),
			errors.New("provider account storage is unavailable")
	}
	return a.providerAccounts.StartProviderLogin(providerName, method)
}

// GetProviderLoginState is how a client that just mounted, or just
// reconnected, rejoins a sign-in that is already running. It never fails: a
// provider with nothing in flight is idle.
//
//ao:scope access:admin
//ao:route home
func (a *App) GetProviderLoginState(providerName string) provideraccountapp.LoginState {
	if a.providerAccounts == nil {
		return provideraccountapp.IdleLoginState(providerName)
	}
	return a.providerAccounts.ProviderLoginState(providerName)
}

// SubmitProviderLoginCode hands back the code Claude's sign-in page shows
// after approval. Codex has no counterpart: its device flow finishes on the
// ChatGPT page.
//
//ao:scope access:admin
//ao:route home
func (a *App) SubmitProviderLoginCode(
	providerName, code string,
) (provideraccountapp.LoginState, error) {
	if a.providerAccounts == nil {
		return provideraccountapp.IdleLoginState(providerName),
			errors.New("provider account storage is unavailable")
	}
	return a.providerAccounts.SubmitProviderLoginCode(providerName, code)
}

// CancelProviderLogin ends a sign-in and leaves the provider idle.
//
//ao:scope access:admin
//ao:route home
func (a *App) CancelProviderLogin(providerName string) provideraccountapp.LoginState {
	if a.providerAccounts == nil {
		return provideraccountapp.IdleLoginState(providerName)
	}
	return a.providerAccounts.CancelProviderLogin(providerName)
}

// RemoveProviderAccount deletes one saved native login. Removing the selected
// account activates the next card in display order; removing the final account
// clears the provider's canonical credential. Existing Codex processes retain
// cached authentication until the normal safe reconnect-on-send gate.
//
//ao:scope access:admin
//ao:route home
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
//
//ao:scope access:admin
//ao:route home
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
