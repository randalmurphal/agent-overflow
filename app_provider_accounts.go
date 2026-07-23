package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/externalurl"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/provideraccounts"
	"agent-overflow/internal/providerstatus"

	"github.com/google/uuid"
)

type ManagedProviderAccount struct {
	provideraccounts.Account
	Active bool `json:"active"`
}

func (a *App) ListProviderAccounts() ([]ManagedProviderAccount, error) {
	if a.providerAccounts == nil {
		return []ManagedProviderAccount{}, nil
	}
	a.providerAccountMu.RLock()
	defer a.providerAccountMu.RUnlock()
	now := time.Now()
	var out []ManagedProviderAccount
	for _, providerName := range []string{string(provider.Claude), string(provider.Codex)} {
		active, hasActive := a.providerAccounts.Active(providerName, now)
		for _, account := range a.providerAccounts.List(providerName, now) {
			out = append(out, ManagedProviderAccount{
				Account: account,
				Active:  hasActive && active.ID == account.ID,
			})
		}
	}
	if out == nil {
		out = []ManagedProviderAccount{}
	}
	return out, nil
}

// LoginProviderAccount runs the provider's native browser login in an
// isolated native home, verifies the resulting identity, atomically activates
// it, and registers only non-secret metadata with Agent Overflow.
func (a *App) LoginProviderAccount(providerName string) (ManagedProviderAccount, error) {
	if a.shuttingDown.Load() {
		return ManagedProviderAccount{}, ErrShuttingDown
	}
	if err := validateManagedProvider(providerName); err != nil {
		return ManagedProviderAccount{}, err
	}
	if a.providerAccounts == nil || a.providerCredentials == nil {
		return ManagedProviderAccount{}, errors.New("provider account storage is unavailable")
	}

	binary := a.providerBinaryPath(providerName)
	a.providerAccountMu.Lock()
	if err := a.adoptCanonicalProviderAccountLocked(providerName, binary); err != nil {
		a.providerAccountMu.Unlock()
		return ManagedProviderAccount{}, fmt.Errorf("preserve current %s account: %w", providerName, err)
	}
	a.providerAccountMu.Unlock()

	candidateID := uuid.NewString()
	keepCandidate := false
	defer func() {
		if !keepCandidate {
			if cleanupErr := a.providerCredentials.RemoveProfile(providerName, candidateID); cleanupErr != nil {
				log.Printf("provider accounts: clean transient %s login profile: %v", providerName, cleanupErr)
			}
		}
	}()
	loginHome, err := a.providerCredentials.PrepareLoginHome(providerName, candidateID)
	if err != nil {
		return ManagedProviderAccount{}, fmt.Errorf("prepare %s login: %w", providerName, err)
	}
	switch providerName {
	case string(provider.Claude):
		executable, err := os.Executable()
		if err != nil {
			return ManagedProviderAccount{}, fmt.Errorf("locate browser bridge: %w", err)
		}
		if err := claude.Login(a.lifeCtx(), claude.LoginConfig{
			Binary:            binary,
			ConfigDir:         loginHome,
			BrowserExecutable: executable,
		}); err != nil {
			return ManagedProviderAccount{}, err
		}
	case string(provider.Codex):
		if err := codex.Login(a.lifeCtx(), codex.LoginConfig{
			Binary: binary,
			Env:    map[string]string{"CODEX_HOME": loginHome},
			OpenURL: func(rawURL string) error {
				return externalurl.Open(context.Background(), rawURL)
			},
		}); err != nil {
			return ManagedProviderAccount{}, err
		}
	}
	if err := a.providerCredentials.ReconcileProfile(providerName, candidateID); err != nil {
		return ManagedProviderAccount{}, fmt.Errorf("share %s login state: %w", providerName, err)
	}

	info, err := a.probeProviderAccountAtHome(providerName, binary, loginHome)
	if err != nil {
		return ManagedProviderAccount{}, fmt.Errorf("verify %s login: %w", providerName, err)
	}
	if err := a.providerCredentials.ReconcileProfile(providerName, candidateID); err != nil {
		return ManagedProviderAccount{}, fmt.Errorf("share %s account state: %w", providerName, err)
	}
	if providerName == string(provider.Claude) && providerstatus.ClaudeUnauthenticated(info) {
		return ManagedProviderAccount{}, errors.New("Claude login completed without an authenticated account")
	}

	a.providerAccountMu.Lock()
	accountLocked := true
	defer func() {
		if accountLocked {
			a.providerAccountMu.Unlock()
		}
	}()
	targetID := candidateID
	if existing, ok := a.providerAccounts.FindByEmail(providerName, info.Email); ok {
		targetID = existing.ID
		if targetID != candidateID {
			if err := a.providerCredentials.CopyProfile(providerName, candidateID, targetID); err != nil {
				return ManagedProviderAccount{}, err
			}
		}
	}
	current, hasCurrent := a.providerAccounts.Active(providerName, time.Now())
	currentID := ""
	if hasCurrent {
		currentID = current.ID
	}
	if err := a.providerCredentials.Activate(providerName, currentID, targetID); err != nil {
		return ManagedProviderAccount{}, fmt.Errorf("activate %s account: %w", providerName, err)
	}

	account, err := a.providerAccounts.UpsertAndActivate(accountFromInfo(targetID, providerName, info))
	if err != nil {
		if rollbackErr := a.rollbackProviderAccountActivation(providerName, currentID); rollbackErr != nil {
			return ManagedProviderAccount{}, fmt.Errorf("%w (credential rollback also failed: %v)", err, rollbackErr)
		}
		return ManagedProviderAccount{}, err
	}
	keepCandidate = targetID == candidateID
	a.invalidateProviderAccountProbe(providerName, binary)
	a.providerAccountMu.Unlock()
	accountLocked = false
	a.emitProviderAccount(providerName, account, info)
	if err := a.RefreshProviderAccountUsage(providerName, account.ID); err != nil {
		// Login and activation succeeded. Quota refresh is independently
		// retryable from Settings and must not roll the account back.
		a.emitProviderAccountUsageRefreshError(providerName, account.ID, err)
	}
	return ManagedProviderAccount{Account: account, Active: true}, nil
}

func (a *App) SwitchProviderAccount(providerName, accountID string) (ManagedProviderAccount, error) {
	if a.shuttingDown.Load() {
		return ManagedProviderAccount{}, ErrShuttingDown
	}
	if err := validateManagedProvider(providerName); err != nil {
		return ManagedProviderAccount{}, err
	}
	if a.providerAccounts == nil || a.providerCredentials == nil {
		return ManagedProviderAccount{}, errors.New("provider account storage is unavailable")
	}

	a.providerAccountMu.Lock()
	accountLocked := true
	defer func() {
		if accountLocked {
			a.providerAccountMu.Unlock()
		}
	}()
	current, hasCurrent := a.providerAccounts.Active(providerName, time.Now())
	if hasCurrent && current.ID == accountID {
		return ManagedProviderAccount{Account: current, Active: true}, nil
	}
	if _, exists := a.providerAccounts.Get(providerName, accountID, time.Now()); !exists {
		return ManagedProviderAccount{}, fmt.Errorf("%s account %q is not saved", providerName, accountID)
	}
	currentID := ""
	if hasCurrent {
		currentID = current.ID
	}
	if err := a.providerCredentials.Activate(providerName, currentID, accountID); err != nil {
		return ManagedProviderAccount{}, fmt.Errorf("switch %s account: %w", providerName, err)
	}
	account, err := a.providerAccounts.Activate(providerName, accountID)
	if err != nil {
		if rollbackErr := a.rollbackProviderAccountActivation(providerName, currentID); rollbackErr != nil {
			return ManagedProviderAccount{}, fmt.Errorf("%w (credential rollback also failed: %v)", err, rollbackErr)
		}
		return ManagedProviderAccount{}, err
	}
	binary := a.providerBinaryPath(providerName)
	a.invalidateProviderAccountProbe(providerName, binary)
	a.providerAccountMu.Unlock()
	accountLocked = false
	info := provider.AccountInfo{
		Email:            account.Email,
		DisplayName:      account.DisplayName,
		SubscriptionType: account.SubscriptionType,
		TokenSource:      account.TokenSource,
		APIProvider:      account.APIProvider,
	}
	a.emitProviderAccount(providerName, account, info)
	return ManagedProviderAccount{Account: account, Active: true}, nil
}

func (a *App) RefreshProviderAccountUsage(providerName, accountID string) error {
	if err := validateManagedProvider(providerName); err != nil {
		return err
	}
	a.providerAccountMu.Lock()
	defer a.providerAccountMu.Unlock()
	return a.refreshProviderAccountUsageLocked(providerName, accountID)
}

func (a *App) refreshProviderAccountUsageLocked(providerName, accountID string) error {
	if a.providerAccounts == nil || a.providerCredentials == nil {
		return errors.New("provider account storage is unavailable")
	}
	if _, exists := a.providerAccounts.Get(providerName, accountID, time.Now()); !exists {
		return fmt.Errorf("%s account %q is not saved", providerName, accountID)
	}
	active, isActive := a.providerAccounts.Active(providerName, time.Now())
	isSelected := isActive && active.ID == accountID
	var providerHome string
	if isSelected {
		providerHome = a.providerAccountSelectionLocked(providerName).Home
	} else {
		var err error
		providerHome, err = a.providerCredentials.PrepareLoginHome(providerName, accountID)
		if err != nil {
			return err
		}
	}
	var (
		snapshot provider.RateLimitsSnapshot
		err      error
	)
	switch providerName {
	case string(provider.Claude):
		var credentialPath string
		if isSelected {
			credentialPath = a.providerAccountSelectionLocked(providerName).CredentialPath
		} else {
			var err error
			credentialPath, err = a.providerCredentials.ProfileCredentialPath(providerName, accountID)
			if err != nil {
				return err
			}
		}
		selection := providerAccountSelection{
			AccountID:        accountID,
			Home:             providerHome,
			CredentialPath:   credentialPath,
			CredentialActive: isSelected,
		}
		if !isSelected {
			selection.Env = map[string]string{"CLAUDE_CONFIG_DIR": providerHome}
		}
		snapshot, err = a.probeClaudeRateLimitsForSelection(a.lifeCtx(), selection)
		if err != nil {
			return err
		}
		if !isSelected {
			if err := a.providerCredentials.ReconcileProfile(providerName, accountID); err != nil {
				return fmt.Errorf("share Claude account state after usage refresh: %w", err)
			}
		}
	case string(provider.Codex):
		_, err := codex.ProbeAccount(a.lifeCtx(), codex.ProbeConfig{
			Binary: a.providerBinaryPath(providerName),
			Env:    map[string]string{"CODEX_HOME": providerHome},
			OnSnapshot: func(value provider.RateLimitsSnapshot) {
				snapshot = value
			},
		})
		if err != nil {
			return err
		}
		if len(snapshot.Limits) == 0 {
			return errors.New("Codex did not return usage limits")
		}
	}
	snapshot.Provider = providerName
	snapshot.AccountID = accountID
	a.emitRateLimitsSnapshot(snapshot)
	return nil
}

func (a *App) probeProviderAccountAtHome(providerName, binary, home string) (provider.AccountInfo, error) {
	switch providerName {
	case string(provider.Claude):
		return claude.ProbeAccount(a.lifeCtx(), claude.ProbeConfig{
			Binary: binary,
			Env:    map[string]string{"CLAUDE_CONFIG_DIR": home},
		})
	case string(provider.Codex):
		return codex.ProbeAccount(a.lifeCtx(), codex.ProbeConfig{
			Binary: binary,
			Env:    map[string]string{"CODEX_HOME": home},
		})
	default:
		return provider.AccountInfo{}, fmt.Errorf("unsupported provider %q", providerName)
	}
}

func (a *App) invalidateProviderAccountProbe(providerName, binary string) {
	switch providerName {
	case string(provider.Claude):
		claudeAccountProbeCache().Invalidate(a.providerProbeCacheKey(providerName, binary))
	case string(provider.Codex):
		codexAccountProbeCache().Invalidate(a.providerProbeCacheKey(providerName, binary))
	}
}

func (a *App) adoptCurrentProviderAccount(providerName string, info provider.AccountInfo) (provideraccounts.Account, bool) {
	a.providerAccountMu.Lock()
	defer a.providerAccountMu.Unlock()
	if a.providerAccounts == nil || a.providerCredentials == nil {
		return provideraccounts.Account{}, false
	}
	if account, ok := a.providerAccounts.Active(providerName, time.Now()); ok {
		return account, true
	}
	accountID := uuid.NewString()
	if _, err := a.providerCredentials.PrepareLoginHome(providerName, accountID); err != nil {
		log.Printf("provider accounts: prepare adoption profile for %s: %v", providerName, err)
		return provideraccounts.Account{}, false
	}
	if err := a.providerCredentials.ImportActive(providerName, accountID); err != nil {
		if !provideraccounts.IsCredentialMissing(err) {
			log.Printf("provider accounts: snapshot existing %s login: %v", providerName, err)
		}
		if cleanupErr := a.providerCredentials.RemoveProfile(providerName, accountID); cleanupErr != nil {
			log.Printf("provider accounts: clean unused %s adoption profile: %v", providerName, cleanupErr)
		}
		return provideraccounts.Account{}, false
	}
	account, err := a.providerAccounts.UpsertAndActivate(accountFromInfo(accountID, providerName, info))
	if err != nil {
		log.Printf("provider accounts: register existing %s login: %v", providerName, err)
		if cleanupErr := a.providerCredentials.RemoveProfile(providerName, accountID); cleanupErr != nil {
			log.Printf("provider accounts: clean failed %s adoption profile: %v", providerName, cleanupErr)
		}
		return provideraccounts.Account{}, false
	}
	return account, true
}

func (a *App) adoptCanonicalProviderAccountLocked(providerName, binary string) error {
	if _, ok := a.providerAccounts.Active(providerName, time.Now()); ok {
		return nil
	}
	if _, err := a.providerCredentials.ReadCredential(providerName, "", true); provideraccounts.IsCredentialMissing(err) {
		return nil
	} else if err != nil {
		return err
	}
	activePath, err := a.providerCredentials.ActiveCredentialPath(providerName)
	if err != nil {
		return err
	}

	var info provider.AccountInfo
	var probeErr error
	if providerName == string(provider.Claude) {
		info, probeErr = claude.ProbeAccount(a.lifeCtx(), claude.ProbeConfig{Binary: binary})
	} else {
		info, probeErr = a.probeProviderAccountAtHome(providerName, binary, filepath.Dir(activePath))
	}
	if probeErr != nil {
		log.Printf("provider accounts: identify existing %s login before switch: %v", providerName, probeErr)
		info.DisplayName = "Previous account"
	}
	accountID := uuid.NewString()
	if _, err := a.providerCredentials.PrepareLoginHome(providerName, accountID); err != nil {
		return err
	}
	if err := a.providerCredentials.ImportActive(providerName, accountID); err != nil {
		if cleanupErr := a.providerCredentials.RemoveProfile(providerName, accountID); cleanupErr != nil {
			log.Printf("provider accounts: clean failed adoption profile: %v", cleanupErr)
		}
		return err
	}
	if _, err := a.providerAccounts.UpsertAndActivate(accountFromInfo(accountID, providerName, info)); err != nil {
		if cleanupErr := a.providerCredentials.RemoveProfile(providerName, accountID); cleanupErr != nil {
			log.Printf("provider accounts: clean failed adoption profile: %v", cleanupErr)
		}
		return err
	}
	return nil
}

func (a *App) rollbackProviderAccountActivation(providerName, currentID string) error {
	if currentID != "" {
		return a.providerCredentials.Activate(providerName, "", currentID)
	}
	return a.providerCredentials.RemoveActive(providerName)
}

func accountFromInfo(accountID, providerName string, info provider.AccountInfo) provideraccounts.Account {
	return provideraccounts.Account{
		ID:               accountID,
		Provider:         providerName,
		Email:            strings.TrimSpace(info.Email),
		DisplayName:      strings.TrimSpace(info.DisplayName),
		SubscriptionType: strings.TrimSpace(info.SubscriptionType),
		TokenSource:      strings.TrimSpace(info.TokenSource),
		APIProvider:      strings.TrimSpace(info.APIProvider),
	}
}

func validateManagedProvider(providerName string) error {
	switch providerName {
	case string(provider.Claude), string(provider.Codex):
		return nil
	default:
		return fmt.Errorf("unsupported account provider %q", providerName)
	}
}

func (a *App) emitProviderAccountUsageRefreshError(providerName, accountID string, err error) {
	a.emit("provider:account_usage_error", map[string]string{
		"provider":  providerName,
		"accountId": accountID,
		"message":   err.Error(),
	})
}
