package main

import (
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
)

type providerAccountSelection struct {
	Generation uint64
	AccountID  string
	Account    provider.AccountInfo
}

type claudeRateLimitSelection struct {
	providerAccountSelection
	EphemeralHome *provideraccounts.EphemeralHome
	Env           map[string]string
}

func (a *App) captureProviderAccountSelection(providerName string) providerAccountSelection {
	a.providerAccountMu.RLock()
	defer a.providerAccountMu.RUnlock()
	return a.providerAccountSelectionLocked(providerName)
}

func (a *App) providerAccountSelectionLocked(providerName string) providerAccountSelection {
	if a.providerAccounts == nil {
		return providerAccountSelection{}
	}
	selection := providerAccountSelection{
		Generation: a.providerAccounts.Generation(providerName),
	}
	account, ok := a.providerAccounts.Active(providerName, time.Now())
	if !ok {
		return selection
	}
	selection.AccountID = account.ID
	selection.Account = providerAccountInfo(account)
	return selection
}

func (a *App) providerProbeCacheKey(providerName, binary string) string {
	accountID := ""
	if a.providerAccounts != nil {
		if account, ok := a.providerAccounts.Active(providerName, time.Now()); ok {
			accountID = account.ID
		}
	}
	return providerProbeCacheKeyForAccount(binary, accountID)
}

func providerProbeCacheKeyForAccount(binary, accountID string) string {
	if accountID == "" {
		accountID = "unmanaged"
	}
	return binary + "\x00account=" + accountID
}
