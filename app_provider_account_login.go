package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"agent-overflow/internal/externalurl"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/providerstatus"

	"github.com/google/uuid"
)

// This file owns the native-login saga: run the provider's own browser flow
// in a temporary home, verify what it produced, and land it in a slot and the
// canonical store — with a rollback for every step that can fail partway.

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
	preserveID, currentCredential, err := a.reconciledActiveCredentialLocked(
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
		preserveID,
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
		if rollbackErr := a.rollbackProviderAccountActivation(providerName, targetID, currentID); rollbackErr != nil {
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
