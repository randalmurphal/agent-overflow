package app

import (
	"context"
	"errors"
	"log"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccountapp"
	"agent-overflow/internal/provideraccounts"
)

type ManagedProviderAccount struct {
	provideraccounts.Account
	Active     bool   `json:"active"`
	Generation uint64 `json:"generation"`
	// NeedsLogin marks a saved account that cannot be selected as it stands —
	// its credential is gone, is the provider's sign-out husk, or holds a
	// login whose OAuth session has expired — so selecting it cannot work
	// until the user signs in again. The card stays listed: its metadata and
	// quota history are still the user's record of that account, and
	// refreshTokenExpiresAt tells the card which of those states it is in.
	NeedsLogin bool `json:"needsLogin"`
	// SignInRequired is set only by SwitchProviderAccount, and only when the
	// switch was DECLINED because the target's login had already expired: the
	// call succeeds, nothing was activated, and the card is to show that the
	// account needs a sign-in. The client never starts one on its own.
	SignInRequired bool `json:"signInRequired"`
	// AutoSwitch reports that the account is in the provider's auto-switch
	// pool.
	AutoSwitch bool `json:"autoSwitch"`
	// PendingReset is the account's usage reset spend Codex has not settled,
	// which the card offers to finish. Set in listings only.
	PendingReset *provideraccountapp.UnsettledReset `json:"pendingReset,omitempty"`
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
	account, err := a.switchProviderAccount(providerName, accountID, false)
	return managedProviderAccount(account), err
}

// switchProviderAccount selects a saved account, as the person's choice or as
// an automatic one, and runs what follows every switch.
func (a *App) switchProviderAccount(
	providerName, accountID string,
	automatic bool,
) (provideraccountapp.ManagedAccount, error) {
	if a.providerAccounts == nil {
		return provideraccountapp.ManagedAccount{}, errors.New("provider account storage is unavailable")
	}
	selectAccount := a.providerAccounts.SwitchProviderAccount
	if automatic {
		selectAccount = a.providerAccounts.AutoSwitchProviderAccount
	}
	account, err := selectAccount(providerName, accountID)
	// A declined switch activated nothing, so there is no new identity to
	// learn — and the catalog probe would spawn the CLI for an answer the
	// canonical home already gave.
	if err == nil && !account.SignInRequired {
		a.probeSwitchedProviderAccount(providerName)
		// The meters and auto-switch otherwise hold this account's last
		// reading until the next poll.
		a.providerLifecycleService().RequestReading(providerName)
	}
	return account, err
}

// ReorderProviderAccounts sets the order one provider's saved accounts are
// listed in. accountIDs must name every saved account exactly once.
//
//ao:scope access:admin
//ao:route home
func (a *App) ReorderProviderAccounts(providerName string, accountIDs []string) error {
	if a.providerAccounts == nil {
		return errors.New("provider account storage is unavailable")
	}
	return a.providerAccounts.ReorderProviderAccounts(providerName, accountIDs)
}

// SetProviderAccountAutoSwitch puts one saved account in or out of the
// provider's auto-switch pool.
//
//ao:scope access:admin
//ao:route home
func (a *App) SetProviderAccountAutoSwitch(providerName, accountID string, enabled bool) error {
	if a.providerAccounts == nil {
		return errors.New("provider account storage is unavailable")
	}
	return a.providerAccounts.SetProviderAccountAutoSwitch(providerName, accountID, enabled)
}

// probeSwitchedProviderAccount runs the zero-token identity probe for the
// account a switch just activated. The switch emits `provider:account` from
// saved metadata without probing, and Claude's model catalog is learned per
// probe identity, so until this lands the new account is served the previous
// identity's catalog. Codex's catalog is keyed by binary, so its switch needs
// no probe. Fire-and-forget like the startup probe: nothing waits on it, and
// the runner reports failures on `provider:status`.
func (a *App) probeSwitchedProviderAccount(providerName string) {
	if a.shuttingDown.Load() {
		return
	}
	if providerName != string(provider.Claude) && providerName != string(provider.Agy) {
		return
	}
	probe := a.switchedAccountProbeFn
	if probe == nil {
		switch providerName {
		case string(provider.Claude):
			probe = func() error {
				_, err := a.ProbeClaudeAccount()
				return err
			}
		case string(provider.Agy):
			probe = func() error {
				a.refreshAgyModelCatalog()
				_, err := a.RecheckAgyAccount()
				return err
			}
		}
	}
	go func() {
		if err := probe(); err != nil {
			log.Printf("provider accounts: probe switched %s account: %v", providerName, err)
		}
	}()
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

// SpendProviderAccountReset spends one of a saved account's usage resets,
// chosen from its card's list at revision, then reads the account's usage
// again. A list that changed since the person saw it is refused, and so is
// any spend while the account has one Codex has not settled.
//
// Once this request's reset landed and the reading after it was recorded,
// auto-switch forgets the usage-limit refusals it holds for the account.
//
//ao:scope access:admin
//ao:route home
//ao:budget 1m
func (a *App) SpendProviderAccountReset(
	ctx context.Context,
	providerName, accountID, optionID string,
	revision uint64,
) (provideraccountapp.ResetSpendResult, error) {
	if a.providerAccounts == nil {
		return provideraccountapp.ResetSpendResult{}, errors.New("provider account storage is unavailable")
	}
	return a.providerAccounts.SpendProviderAccountReset(ctx, providerName, accountID, optionID, revision)
}

// FinishProviderAccountReset sends the account's unsettled reset spend
// spendID (its card's pendingReset.id) again under the same key, which never
// spends a second reset, then reads the account's usage again. When spendID
// is not the account's unsettled spend it is refused and nothing is sent.
//
//ao:scope access:admin
//ao:route home
//ao:budget 1m
func (a *App) FinishProviderAccountReset(
	ctx context.Context,
	providerName, accountID, spendID string,
) (provideraccountapp.ResetSpendResult, error) {
	if a.providerAccounts == nil {
		return provideraccountapp.ResetSpendResult{}, errors.New("provider account storage is unavailable")
	}
	return a.providerAccounts.FinishProviderAccountReset(ctx, providerName, accountID, spendID)
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
		SignInRequired: account.SignInRequired, AutoSwitch: account.AutoSwitch,
		PendingReset: account.PendingReset,
	}
}
