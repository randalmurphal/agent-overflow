package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/provideraccounts"

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
// store; every other account is backed by its saved slot. A check that
// itself fails is reported as usable so a transient filesystem error
// cannot make every account look signed out — the activation path still
// validates for real.
//
// A slot holding the provider's sign-out husk is unusable: it is a file
// where a login used to be, and treating it as present is what let the
// card advertise a dead account as switchable.
func (a *App) providerAccountCredentialUsable(providerName, accountID string, isActive bool) bool {
	if a.providerCredentials == nil {
		return true
	}
	usable, err := a.providerCredentials.CredentialUsable(providerName, accountID, isActive)
	if err != nil {
		return true
	}
	return usable
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

// providerSignedOutDetector is the provider-aware "these bytes are a
// sign-out, not a login" predicate every Credentials store is constructed
// with. It is the ONE place the provider-specific shape is named, so boot,
// harness, and fixtures all refuse husks the same way; the constructor
// requiring it is what makes a store that silently accepts the bytes that
// destroy a slot impossible to build by omission.
func providerSignedOutDetector(providerName string, data []byte) bool {
	return providerName == string(provider.Claude) && claude.CredentialsSignedOut(data)
}

// providerCredentialChainPosition is the provider-aware "how far along its
// rotation chain are these bytes" reading that keeps a saved slot from being
// moved backwards onto a retired token. Claude's OAuth expiry is the signal;
// Codex has no single-use rotation to protect, so it reports none.
func providerCredentialChainPosition(providerName string, data []byte) (int64, bool) {
	if providerName != string(provider.Claude) {
		return 0, false
	}
	return claude.CredentialExpiresAt(data)
}

// providerCredentialPolicy is the ONE place the provider-specific credential
// rules are named, so boot, harness, and fixtures all enforce them the same
// way.
func providerCredentialPolicy() provideraccounts.Policy {
	return provideraccounts.Policy{
		SignedOut:     providerSignedOutDetector,
		ChainPosition: providerCredentialChainPosition,
	}
}

// mapProviderAccountLoginError restates a provider-level "this login no longer
// exists" verdict as the account-scoped instruction the card can act on. Every
// other error passes through unchanged — only the one sentinel has a known
// recovery.
func mapProviderAccountLoginError(
	providerName string,
	account provideraccounts.Account,
	err error,
) error {
	if !errors.Is(err, errClaudeCredentialSignedOut) {
		return err
	}
	return errProviderAccountNeedsLogin(providerName, account)
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
	// A husked slot is a login the provider already ended. Installing it into
	// the canonical store would retire the working account for a dead one and
	// leave the app looking signed in until the next request failed.
	if a.providerCredentials.CredentialSignedOut(providerName, targetCredential.Data) {
		a.auditAccountEvent(
			"refused to switch to %s account %s: its saved credential is the provider's sign-out marker",
			providerName,
			accountID,
		)
		return ManagedProviderAccount{}, errProviderAccountNeedsLogin(providerName, target)
	}
	currentID := ""
	if hasCurrent {
		currentID = current.ID
	}
	preserveID, currentCredential, err := a.reconciledActiveCredentialLocked(
		providerName,
		currentID,
		accountID,
	)
	if err != nil {
		return ManagedProviderAccount{}, err
	}
	if err := a.providerCredentials.ActivateWithSnapshot(
		providerName,
		preserveID,
		accountID,
		currentCredential,
	); err != nil {
		// The write layer re-reads the target slot, so it can meet a husk the
		// pre-check above did not see. Same verdict, same instruction — not a
		// raw internal error.
		if errors.Is(err, provideraccounts.ErrSignedOutCredential) {
			return ManagedProviderAccount{}, errProviderAccountNeedsLogin(providerName, target)
		}
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
		if rollbackErr := a.rollbackProviderAccountActivation(providerName, accountID, currentID); rollbackErr != nil {
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
// associated with currentAccountID so activation can preserve it into that
// account's slot. Returns the account ID to preserve under — "" when there is
// nothing to preserve — paired with the snapshot, so callers thread both
// straight into ActivateWithSnapshot and cannot pass an ID without its value.
// The caller holds providerAccountMu after a successful external-login
// reconciliation. Passing this exact snapshot into activation prevents a
// concurrent native login from being preserved under the stale account ID.
//
// A canonical credential the CLI blanked after a failed refresh reports
// errActiveCredentialSignedOut from the verify below and is mapped to
// "nothing to preserve" here: the outgoing slot already holds its last saved
// pair, and the husk must not overwrite it. Refusing the mutation instead
// locks the user out of every switch and delete until an external login
// replaces the file (incident 2026-08-03).
func (a *App) reconciledActiveCredentialLocked(
	providerName string,
	currentAccountID string,
	targetAccountID string,
) (string, *provideraccounts.CredentialSnapshot, error) {
	if currentAccountID == "" || currentAccountID == targetAccountID {
		return "", nil, nil
	}
	snapshot, err := a.verifiedActiveCredentialLocked(providerName)
	if errors.Is(err, errActiveCredentialSignedOut) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	return currentAccountID, &snapshot, nil
}

// errActiveCredentialSignedOut reports that the canonical credential is the
// blank husk claude >= 2.1.219 leaves behind after a failed token refresh — a
// sign-out by the provider, not a concurrent login. Account mutations treat
// it as "no credential worth preserving" and proceed; treating it as
// interference would refuse every switch and delete forever, because nothing
// inside the app ever replaces the husk.
var errActiveCredentialSignedOut = errors.New("active provider credential was signed out by the provider")

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
	// Checked before the fingerprint: a husk can never match the remembered
	// fingerprint, and "changed; retry" would misdiagnose a sign-out as a
	// concurrent login.
	if providerName == string(provider.Claude) && claude.CredentialsSignedOut(snapshot.Data) {
		return provideraccounts.CredentialSnapshot{}, errActiveCredentialSignedOut
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
		return claude.ProbeAccount(a.lifeCtx(), a.claudeProbeConfig(
			binary,
			map[string]string{"CLAUDE_CONFIG_DIR": home},
		))
	case string(provider.Codex):
		return codex.ProbeAccount(a.lifeCtx(), a.codexProbeConfig(
			binary,
			map[string]string{"CODEX_HOME": home},
		))
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
		a.canonicalProviderAccountProbe(providerName, binary),
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
	// login we are adopting may already be saved under this identity from
	// an earlier session; minting a fresh ID here would put the credential
	// in a slot the metadata never references, leaving the real account on
	// a stale credential until the next startup prune deleted the copy.
	accountID := uuid.NewString()
	a.backfillCodexOrgIDs(providerName, info.Email)
	existing, found, err := a.findAccountByObservedIdentity(providerName, info)
	if err != nil {
		return fmt.Errorf(
			"resolve current %s login %s against saved accounts: %w",
			providerName,
			describeObservedAccount(info),
			err,
		)
	}
	if found {
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

// rollbackProviderAccountActivation reinstates currentID's credential as
// canonical after a failed activation. canonicalHolderID names the account
// whose credential the canonical store holds at this moment (the activation
// that is being unwound), so Activate can preserve those bytes — including
// any rotation a live CLI performed since — into that account's slot instead
// of destroying them. Pass "" when canonical holds nothing attributable.
func (a *App) rollbackProviderAccountActivation(providerName, canonicalHolderID, currentID string) error {
	if currentID != "" {
		return a.providerCredentials.Activate(providerName, canonicalHolderID, currentID)
	}
	return a.providerCredentials.RemoveActive(providerName)
}

func accountFromInfo(accountID, providerName string, info provider.AccountInfo) provideraccounts.Account {
	return provideraccounts.Account{
		ID:               accountID,
		Provider:         providerName,
		Email:            strings.TrimSpace(info.Email),
		OrgID:            strings.TrimSpace(info.OrgID),
		OrgName:          strings.TrimSpace(info.OrgName),
		DisplayName:      strings.TrimSpace(info.DisplayName),
		SubscriptionType: strings.TrimSpace(info.SubscriptionType),
		TokenSource:      strings.TrimSpace(info.TokenSource),
		APIProvider:      strings.TrimSpace(info.APIProvider),
	}
}

func providerAccountInfo(account provideraccounts.Account) provider.AccountInfo {
	return provider.AccountInfo{
		Email:            account.Email,
		OrgID:            account.OrgID,
		OrgName:          account.OrgName,
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
	a.emit(eventchan.ProviderAccountUsageError, map[string]string{
		"provider":  providerName,
		"accountId": accountID,
		"message":   err.Error(),
	})
}
