package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"os"
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
	a.providerAccountMu.RLock()
	now := time.Now()
	var out []ManagedProviderAccount
	for _, providerName := range []string{string(provider.Claude), string(provider.Codex)} {
		active, hasActive := a.providerAccounts.Active(providerName, now)
		generation := a.providerAccounts.Generation(providerName)
		for _, account := range a.providerAccounts.List(providerName, now) {
			out = append(out, ManagedProviderAccount{
				Account:    account,
				Active:     hasActive && active.ID == account.ID,
				Generation: generation,
			})
		}
	}
	a.providerAccountMu.RUnlock()

	// Credential presence is filesystem I/O — and on macOS a Keychain read
	// per account. Resolved after the lock is released so listing accounts
	// cannot stall a session start behind it.
	for i := range out {
		out[i].NeedsLogin = !a.providerAccountCredentialUsable(
			out[i].Provider,
			out[i].ID,
			out[i].Active,
		)
	}
	if out == nil {
		out = []ManagedProviderAccount{}
	}
	return out, nil
}

// providerAccountCredentialUsable reports whether this account could be
// activated right now. The selected account is backed by the canonical
// store; every other account is backed by its saved slot. A presence
// check that itself fails is reported as usable so a transient
// filesystem error cannot make every account look signed out — the
// activation path still validates for real.
func (a *App) providerAccountCredentialUsable(providerName, accountID string, isActive bool) bool {
	if a.providerCredentials == nil {
		return true
	}
	present, err := a.providerCredentials.CredentialPresent(providerName, accountID, isActive)
	if err != nil {
		return true
	}
	return present
}

// errProviderAccountNeedsLogin describes the one recovery available when an
// account's saved credential is gone: sign in again. The metadata row stays,
// so the user is told what to do rather than shown the missing path.
func errProviderAccountNeedsLogin(providerName string, account provideraccounts.Account) error {
	label := account.Email
	if label == "" {
		label = account.DisplayName
	}
	if label == "" {
		label = account.ID
	}
	return fmt.Errorf(
		"the saved %s credentials for %s are gone; sign in to this account again to reconnect it",
		providerName,
		label,
	)
}

// LoginProviderAccount runs the provider's native browser login in a
// short-lived isolated home, retains only the resulting native credential,
// atomically activates it, and registers non-secret metadata.
func (a *App) LoginProviderAccount(
	providerName string,
) (_ ManagedProviderAccount, retErr error) {
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
	reconcileMu := a.providerCredentialReconcileMutex(providerName)
	reconcileMu.Lock()
	a.providerAccountMu.Lock()
	if err := a.adoptCanonicalProviderAccountLocked(providerName, binary); err != nil {
		a.providerAccountMu.Unlock()
		reconcileMu.Unlock()
		return ManagedProviderAccount{}, fmt.Errorf("preserve current %s account: %w", providerName, err)
	}
	a.providerAccountMu.Unlock()
	reconcileMu.Unlock()

	loginHome, err := a.providerCredentials.NewEphemeralHome(providerName)
	if err != nil {
		return ManagedProviderAccount{}, fmt.Errorf("prepare %s login: %w", providerName, err)
	}
	defer func() {
		if cleanupErr := loginHome.Cleanup(); cleanupErr != nil {
			retErr = errors.Join(
				retErr,
				fmt.Errorf("clean temporary %s login home: %w", providerName, cleanupErr),
			)
		}
	}()

	switch providerName {
	case string(provider.Claude):
		executable, err := os.Executable()
		if err != nil {
			return ManagedProviderAccount{}, fmt.Errorf("locate browser bridge: %w", err)
		}
		if err := claude.Login(a.lifeCtx(), claude.LoginConfig{
			Binary:            binary,
			ConfigDir:         loginHome.Path,
			BrowserExecutable: executable,
		}); err != nil {
			return ManagedProviderAccount{}, err
		}
	case string(provider.Codex):
		if err := codex.Login(a.lifeCtx(), codex.LoginConfig{
			Binary: binary,
			Env:    map[string]string{"CODEX_HOME": loginHome.Path},
			OpenURL: func(rawURL string) error {
				return externalurl.Open(context.Background(), rawURL)
			},
		}); err != nil {
			return ManagedProviderAccount{}, err
		}
	}

	info, err := a.probeProviderAccountAtHome(providerName, binary, loginHome.Path)
	if err != nil {
		return ManagedProviderAccount{}, fmt.Errorf("verify %s login: %w", providerName, err)
	}
	if providerName == string(provider.Claude) && providerstatus.ClaudeUnauthenticated(info) {
		return ManagedProviderAccount{}, errors.New("Claude login completed without an authenticated account")
	}
	loginCredential, err := a.providerCredentials.ReadEphemeralCredential(loginHome)
	if err != nil {
		return ManagedProviderAccount{}, fmt.Errorf("capture %s login credentials: %w", providerName, err)
	}
	if err := loginHome.Cleanup(); err != nil {
		return ManagedProviderAccount{}, fmt.Errorf("clean temporary %s login home: %w", providerName, err)
	}
	reconcileMu.Lock()
	reconcileLocked := true
	defer func() {
		if reconcileLocked {
			reconcileMu.Unlock()
		}
	}()
	if err := a.reconcileExternalProviderAccountWithMutexHeld(providerName); err != nil {
		return ManagedProviderAccount{}, fmt.Errorf(
			"check current %s account before activation: %w",
			providerName,
			err,
		)
	}

	a.providerAccountMu.Lock()
	accountLocked := true
	defer func() {
		if accountLocked {
			a.providerAccountMu.Unlock()
		}
	}()

	targetID := uuid.NewString()
	if existing, ok := a.providerAccounts.FindByEmail(providerName, info.Email); ok {
		targetID = existing.ID
	}
	// Capture before the write so a failure downstream restores exactly
	// what was here — including the case where re-logging into a saved
	// account finds its slot momentarily unreadable. Rolling that back by
	// deletion would cost the user the very account they were repairing.
	savedTarget, err := a.providerCredentials.CaptureAccountCredential(providerName, targetID)
	if err != nil {
		return ManagedProviderAccount{}, err
	}
	if err := a.providerCredentials.WriteAccountCredential(
		providerName,
		targetID,
		loginCredential.Data,
	); err != nil {
		return ManagedProviderAccount{}, fmt.Errorf("save %s login credentials: %w", providerName, err)
	}
	restoreTarget := func() error {
		return a.providerCredentials.RestoreAccountCredential(savedTarget)
	}

	current, hasCurrent := a.providerAccounts.Active(providerName, time.Now())
	currentID := ""
	if hasCurrent {
		currentID = current.ID
	}
	currentCredential, err := a.reconciledActiveCredentialLocked(
		providerName,
		currentID,
		targetID,
	)
	if err != nil {
		if restoreErr := restoreTarget(); restoreErr != nil {
			return ManagedProviderAccount{}, fmt.Errorf(
				"%w (saved credential rollback also failed: %v)",
				err,
				restoreErr,
			)
		}
		return ManagedProviderAccount{}, err
	}
	if err := a.providerCredentials.ActivateWithSnapshot(
		providerName,
		currentID,
		targetID,
		currentCredential,
	); err != nil {
		if restoreErr := restoreTarget(); restoreErr != nil {
			return ManagedProviderAccount{}, fmt.Errorf(
				"activate %s account: %w (saved credential rollback also failed: %v)",
				providerName,
				err,
				restoreErr,
			)
		}
		return ManagedProviderAccount{}, fmt.Errorf("activate %s account: %w", providerName, err)
	}
	if err := a.verifyCanonicalProviderCredentialLocked(
		providerName,
		loginCredential.Data,
	); err != nil {
		if restoreErr := restoreTarget(); restoreErr != nil {
			return ManagedProviderAccount{}, errors.Join(err, restoreErr)
		}
		return ManagedProviderAccount{}, err
	}

	account, err := a.providerAccounts.UpsertAndActivateCredential(
		accountFromInfo(targetID, providerName, info),
	)
	if err != nil {
		var rollbackErrs []error
		if restoreErr := restoreTarget(); restoreErr != nil {
			rollbackErrs = append(rollbackErrs, restoreErr)
		}
		if rollbackErr := a.rollbackProviderAccountActivation(providerName, currentID); rollbackErr != nil {
			rollbackErrs = append(rollbackErrs, rollbackErr)
		}
		if len(rollbackErrs) > 0 {
			return ManagedProviderAccount{}, fmt.Errorf(
				"%w (credential rollback also failed: %v)",
				err,
				errors.Join(rollbackErrs...),
			)
		}
		return ManagedProviderAccount{}, err
	}

	a.invalidateProviderAccountProbe(providerName, binary)
	a.rememberProviderCredentialFingerprintLocked(providerName, loginCredential.Data)
	generation := a.providerAccounts.Generation(providerName)
	a.providerAccountMu.Unlock()
	accountLocked = false
	reconcileMu.Unlock()
	reconcileLocked = false
	a.emitProviderAccount(providerName, account, info, generation)
	a.applyProviderAccountSelectionToSessions(providerName, generation, account.ID)
	if err := a.RefreshProviderAccountUsage(providerName, account.ID); err != nil {
		// Login and activation succeeded. Quota refresh is independently
		// retryable from Settings and must not roll the account back.
		a.emitProviderAccountUsageRefreshError(providerName, account.ID, err)
	}
	return ManagedProviderAccount{Account: account, Active: true, Generation: generation}, nil
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
	reconcileMu := a.providerCredentialReconcileMutex(providerName)
	reconcileMu.Lock()
	reconcileLocked := true
	defer func() {
		if reconcileLocked {
			reconcileMu.Unlock()
		}
	}()
	if err := a.reconcileExternalProviderAccountWithMutexHeld(providerName); err != nil {
		return ManagedProviderAccount{}, fmt.Errorf("check current %s account before switch: %w", providerName, err)
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
		return ManagedProviderAccount{
			Account:    current,
			Active:     true,
			Generation: a.providerAccounts.Generation(providerName),
		}, nil
	}
	target, exists := a.providerAccounts.Get(providerName, accountID, time.Now())
	if !exists {
		return ManagedProviderAccount{}, fmt.Errorf("%s account %q is not saved", providerName, accountID)
	}
	targetCredential, err := a.providerCredentials.ReadCredentialSnapshot(
		providerName,
		accountID,
		false,
	)
	if err != nil {
		if provideraccounts.IsCredentialMissing(err) {
			return ManagedProviderAccount{}, errProviderAccountNeedsLogin(providerName, target)
		}
		return ManagedProviderAccount{}, fmt.Errorf(
			"read selected %s credentials: %w",
			providerName,
			err,
		)
	}
	currentID := ""
	if hasCurrent {
		currentID = current.ID
	}
	currentCredential, err := a.reconciledActiveCredentialLocked(
		providerName,
		currentID,
		accountID,
	)
	if err != nil {
		return ManagedProviderAccount{}, err
	}
	if err := a.providerCredentials.ActivateWithSnapshot(
		providerName,
		currentID,
		accountID,
		currentCredential,
	); err != nil {
		return ManagedProviderAccount{}, fmt.Errorf("switch %s account: %w", providerName, err)
	}
	if err := a.verifyCanonicalProviderCredentialLocked(
		providerName,
		targetCredential.Data,
	); err != nil {
		return ManagedProviderAccount{}, err
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
	a.rememberProviderCredentialFingerprintLocked(providerName, targetCredential.Data)
	generation := a.providerAccounts.Generation(providerName)
	a.providerAccountMu.Unlock()
	accountLocked = false
	info := providerAccountInfo(account)
	a.emitProviderAccount(providerName, account, info, generation)
	a.applyProviderAccountSelectionToSessions(providerName, generation, account.ID)
	reconcileMu.Unlock()
	reconcileLocked = false
	return ManagedProviderAccount{Account: account, Active: true, Generation: generation}, nil
}

// reconciledActiveCredentialLocked captures the credential value already
// associated with currentAccountID. The caller holds providerAccountMu after a
// successful external-login reconciliation. Passing this exact snapshot into
// activation prevents a concurrent native login from being preserved under
// the stale account ID.
func (a *App) reconciledActiveCredentialLocked(
	providerName string,
	currentAccountID string,
	targetAccountID string,
) (*provideraccounts.CredentialSnapshot, error) {
	if currentAccountID == "" || currentAccountID == targetAccountID {
		return nil, nil
	}
	snapshot, err := a.verifiedActiveCredentialLocked(providerName)
	if err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func (a *App) verifiedActiveCredentialLocked(
	providerName string,
) (provideraccounts.CredentialSnapshot, error) {
	snapshot, err := a.providerCredentials.ReadCredentialSnapshot(providerName, "", true)
	if err != nil {
		return provideraccounts.CredentialSnapshot{}, fmt.Errorf(
			"capture current %s credentials before account change: %w",
			providerName,
			err,
		)
	}
	fingerprint := sha256.Sum256(snapshot.Data)
	known, ok := a.providerCredentialFingerprints[providerName]
	if !ok || known != fingerprint {
		return provideraccounts.CredentialSnapshot{}, fmt.Errorf(
			"%s credentials changed while updating accounts; retry",
			providerName,
		)
	}
	return snapshot, nil
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

func (a *App) adoptCurrentProviderAccount(
	providerName string,
	info provider.AccountInfo,
	credential *provideraccounts.CredentialSnapshot,
) (provideraccounts.Account, bool, error) {
	a.providerAccountMu.Lock()
	defer a.providerAccountMu.Unlock()
	if a.providerAccounts == nil || a.providerCredentials == nil {
		return provideraccounts.Account{}, false, nil
	}
	if credential == nil {
		return provideraccounts.Account{}, false, nil
	}
	if err := a.verifyCanonicalProviderCredentialLocked(
		providerName,
		credential.Data,
	); err != nil {
		return provideraccounts.Account{}, false, err
	}
	if _, hasActive := a.providerAccounts.Active(providerName, time.Now()); !hasActive &&
		strings.TrimSpace(info.Email) == "" {
		accountID := uuid.NewString()
		saved, err := a.providerCredentials.CaptureAccountCredential(providerName, accountID)
		if err != nil {
			return provideraccounts.Account{}, false, err
		}
		if err := a.providerCredentials.WriteAccountCredential(
			providerName,
			accountID,
			credential.Data,
		); err != nil {
			return provideraccounts.Account{}, false, fmt.Errorf(
				"save existing %s credentials: %w",
				providerName,
				err,
			)
		}
		account, err := a.providerAccounts.UpsertAndActivate(
			accountFromInfo(accountID, providerName, info),
		)
		if err != nil {
			cleanupErr := a.providerCredentials.RestoreAccountCredential(saved)
			return provideraccounts.Account{}, false, errors.Join(err, cleanupErr)
		}
		a.rememberProviderCredentialFingerprintLocked(providerName, credential.Data)
		return account, true, nil
	}
	account, changed, err := a.reconcileObservedAccountLocked(
		providerName,
		info,
		*credential,
	)
	if err != nil {
		return provideraccounts.Account{}, false, err
	}
	a.rememberProviderCredentialFingerprintLocked(providerName, credential.Data)
	return account, changed, nil
}

func (a *App) adoptCanonicalProviderAccountLocked(providerName, binary string) error {
	if _, ok := a.providerAccounts.Active(providerName, time.Now()); ok {
		return nil
	}
	info, credential, err := a.runStableAccountProbe(
		providerName,
		canonicalProviderAccountProbe(providerName, binary),
	)
	if err != nil {
		return err
	}
	if credential == nil {
		return nil
	}
	if err := a.verifyCanonicalProviderCredentialLocked(
		providerName,
		credential.Data,
	); err != nil {
		return err
	}
	// Resolve the account ID before writing the credential. A canonical
	// login we are adopting may already be saved under this email from an
	// earlier session; minting a fresh ID here would put the credential in
	// a slot the metadata never references, leaving the real account on a
	// stale credential until the next startup prune deleted the copy.
	accountID := uuid.NewString()
	if existing, ok := a.providerAccounts.FindByEmail(providerName, info.Email); ok {
		accountID = existing.ID
	}
	saved, err := a.providerCredentials.CaptureAccountCredential(providerName, accountID)
	if err != nil {
		return err
	}
	restore := func() {
		if cleanupErr := a.providerCredentials.RestoreAccountCredential(saved); cleanupErr != nil {
			log.Printf("provider accounts: restore adoption credential: %v", cleanupErr)
		}
	}
	if err := a.providerCredentials.WriteAccountCredential(
		providerName,
		accountID,
		credential.Data,
	); err != nil {
		restore()
		return err
	}
	if _, err := a.providerAccounts.UpsertAndActivate(accountFromInfo(accountID, providerName, info)); err != nil {
		restore()
		return err
	}
	a.rememberProviderCredentialFingerprintLocked(providerName, credential.Data)
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

func providerAccountInfo(account provideraccounts.Account) provider.AccountInfo {
	return provider.AccountInfo{
		Email:            account.Email,
		DisplayName:      account.DisplayName,
		SubscriptionType: account.SubscriptionType,
		TokenSource:      account.TokenSource,
		APIProvider:      account.APIProvider,
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
