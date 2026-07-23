package main

import (
	"path/filepath"
	"time"
)

type providerAccountSelection struct {
	Generation       uint64
	AccountID        string
	Home             string
	CredentialPath   string
	CredentialActive bool
	Env              map[string]string
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
	selection.CredentialActive = true
	if a.providerCredentials == nil {
		return selection
	}
	paths, err := a.providerCredentials.Paths(providerName)
	if err != nil {
		return selection
	}
	selection.CredentialPath = filepath.Join(paths.SharedHome, paths.CredentialFile)
	switch providerName {
	case "claude":
		// Claude intentionally follows the canonical native file so already
		// running processes get the provider's own /login-style hot swap.
		// Leave CLAUDE_CONFIG_DIR unset for the canonical home: on macOS,
		// merely setting it to ~/.claude selects a different hashed Keychain
		// service than Claude's native default.
		selection.Home = paths.SharedHome
	case "codex":
		// Pin each app-server to its account profile. Old servers can refresh
		// their own tokens without racing the newly selected account's auth.
		profileHome, profileErr := a.providerCredentials.ProfileHome(providerName, account.ID)
		if profileErr != nil {
			return providerAccountSelection{Generation: selection.Generation}
		}
		selection.Home = profileHome
		selection.CredentialPath = filepath.Join(profileHome, paths.CredentialFile)
		selection.Env = map[string]string{"CODEX_HOME": profileHome}
	}
	return selection
}

func (a *App) providerCredentialGeneration(providerName string) uint64 {
	return a.captureProviderAccountSelection(providerName).Generation
}

func (a *App) providerProbeCacheKey(providerName, binary string) string {
	accountID := ""
	if a.providerAccounts != nil {
		if account, ok := a.providerAccounts.Active(providerName, time.Now()); ok {
			accountID = account.ID
		}
	}
	if accountID == "" {
		accountID = "unmanaged"
	}
	return binary + "\x00account=" + accountID
}

func mergeProviderEnv(base, overrides map[string]string) map[string]string {
	if len(base) == 0 && len(overrides) == 0 {
		return nil
	}
	merged := make(map[string]string, len(base)+len(overrides))
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range overrides {
		merged[key] = value
	}
	return merged
}
