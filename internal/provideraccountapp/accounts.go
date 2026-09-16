package provideraccountapp

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/provideraccounts"

	"github.com/google/uuid"
)

type ManagedAccount struct {
	provideraccounts.Account
	Active     bool   `json:"active"`
	Generation uint64 `json:"generation"`
	// NeedsLogin marks a saved account that cannot be selected as it stands —
	// its credential is gone, is the provider's sign-out husk, or holds a
	// login whose OAuth session has expired — so selecting it cannot work
	// until the user signs in again. The card stays listed: its metadata and
	// quota history are still the user's record of that account, and
	// RefreshTokenExpiresAt tells the card which of those states it is in.
	NeedsLogin bool `json:"needsLogin"`
	// SignInRequired is set only by SwitchProviderAccount, and only when the
	// switch was DECLINED because the target's login has expired. The call
	// succeeds so the answer can carry this: nothing is broken, the account
	// simply needs a sign-in, and the client shows that on the card. It
	// never starts a sign-in on the user's behalf; that is the user's own
	// click.
	//
	// It is always false in listings, where NeedsLogin already says the card
	// needs repair and nothing is being attempted.
	SignInRequired bool `json:"signInRequired"`
}

func (m *Manager) ListProviderAccounts() ([]ManagedAccount, error) {
	if m.store == nil {
		return []ManagedAccount{}, nil
	}
	m.mu.RLock()
	now := time.Now()
	var out []ManagedAccount
	for _, providerName := range []string{string(provider.Claude), string(provider.Codex)} {
		active, hasActive := m.store.Active(providerName, now)
		generation := m.store.Generation(providerName)
		for _, account := range m.store.List(providerName, now) {
			out = append(out, ManagedAccount{
				Account:    account,
				Active:     hasActive && active.ID == account.ID,
				Generation: generation,
			})
		}
	}
	m.mu.RUnlock()

	// Credential presence is filesystem I/O — and on macOS a Keychain read
	// per account. Resolved after the lock is released so listing accounts
	// cannot stall a session start behind it.
	for i := range out {
		state := m.providerAccountCredentialState(
			out[i].Provider,
			out[i].ID,
			out[i].Active,
		)
		out[i].NeedsLogin = !state.Usable
		// The credential outranks the saved metadata here: an account that was
		// saved before AO captured session deadlines, or whose credential was
		// replaced outside AO, still gets its countdown from the bytes this
		// read already has in hand. Persisting it belongs to the paths that
		// move credentials, not to a listing.
		if state.LoginExpiresAt > 0 {
			out[i].RefreshTokenExpiresAt = state.LoginExpiresAt
		}
	}
	if out == nil {
		out = []ManagedAccount{}
	}
	return out, nil
}

// providerAccountCredentialState reads what one account's credential says
// about itself. The selected account is backed by the canonical store; every
// other account is backed by its saved slot. A read that itself fails is
// reported as usable so a transient filesystem error cannot make every account
// look signed out — the activation path still validates for real — and with no
// deadline, so a failed read never invents an expiry either.
//
// A slot holding the provider's sign-out husk, or a login whose OAuth session
// has ended, is unusable: it is a file where a working login used to be, and
// treating it as present is what let the card advertise a dead account as
// switchable.
func (m *Manager) providerAccountCredentialState(
	providerName, accountID string,
	isActive bool,
) provideraccounts.CredentialState {
	if m.credentials == nil {
		return provideraccounts.CredentialState{Usable: true}
	}
	state, err := m.credentials.InspectCredential(providerName, accountID, isActive)
	if err != nil {
		return provideraccounts.CredentialState{Usable: true}
	}
	return state
}

// errProviderAccountNeedsLogin describes the one recovery available when an
// account's saved credential is gone: sign in again. The metadata row stays,
// so the user is told what to do rather than shown the missing path.
func errProviderAccountNeedsLogin(providerName string, account provideraccounts.Account) error {
	return fmt.Errorf(
		"the saved %s credentials for %s are gone; sign in to this account again to reconnect it",
		providerName,
		describeProviderAccount(account),
	)
}

// errProviderAccountLoginExpired names the account whose OAuth session ran
// out. It is deliberately a different sentence from errProviderAccountNeedsLogin:
// nothing is missing or corrupt, the login simply reached the end of the
// ~30-day life its refresh token was issued with, and the user has done
// nothing wrong. It wraps errClaudeLoginExpired so callers can still test the
// verdict.
func errProviderAccountLoginExpired(providerName string, account provideraccounts.Account) error {
	return fmt.Errorf(
		"the %s login for %s expired; sign in to this account again to reconnect it (%w)",
		providerName,
		describeProviderAccount(account),
		errClaudeLoginExpired,
	)
}

func describeProviderAccount(account provideraccounts.Account) string {
	if account.Email != "" {
		return account.Email
	}
	if account.DisplayName != "" {
		return account.DisplayName
	}
	return account.ID
}

// mapProviderAccountLoginError restates a provider-level "this login is over"
// verdict as the account-scoped instruction the card can act on. Every other
// error passes through unchanged — only the two sentinels have a known
// recovery, and they are kept apart because the card says different things
// about a credential that vanished and a session that ended.
func mapProviderAccountLoginError(
	providerName string,
	account provideraccounts.Account,
	err error,
) error {
	if errors.Is(err, errClaudeLoginExpired) {
		return errProviderAccountLoginExpired(providerName, account)
	}
	if !errors.Is(err, errClaudeCredentialSignedOut) {
		return err
	}
	return errProviderAccountNeedsLogin(providerName, account)
}

func (m *Manager) SwitchProviderAccount(providerName, accountID string) (ManagedAccount, error) {
	if m.shuttingDown() {
		return ManagedAccount{}, m.shutdownError()
	}
	if err := ValidateProvider(providerName); err != nil {
		return ManagedAccount{}, err
	}
	if m.store == nil || m.credentials == nil {
		return ManagedAccount{}, errors.New("provider account storage is unavailable")
	}
	endWork, err := m.beginWork(m.context())
	if err != nil {
		return ManagedAccount{}, err
	}
	defer endWork()
	reconcileMu := m.reconcileMutex(providerName)
	reconcileMu.Lock()
	reconcileLocked := true
	defer func() {
		if reconcileLocked {
			reconcileMu.Unlock()
		}
	}()
	if err := m.reconcileExternalProviderAccountWithMutexHeld(providerName); err != nil {
		return ManagedAccount{}, fmt.Errorf("check current %s account before switch: %w", providerName, err)
	}

	m.mu.Lock()
	accountLocked := true
	defer func() {
		if accountLocked {
			m.mu.Unlock()
		}
	}()
	current, hasCurrent := m.store.Active(providerName, time.Now())
	if hasCurrent && current.ID == accountID {
		return ManagedAccount{
			Account:    current,
			Active:     true,
			Generation: m.store.Generation(providerName),
		}, nil
	}
	target, exists := m.store.Get(providerName, accountID, time.Now())
	if !exists {
		return ManagedAccount{}, fmt.Errorf("%s account %q is not saved", providerName, accountID)
	}
	targetCredential, err := m.credentials.ReadCredentialSnapshot(
		providerName,
		accountID,
		false,
	)
	if err != nil {
		if provideraccounts.IsCredentialMissing(err) {
			return ManagedAccount{}, errProviderAccountNeedsLogin(providerName, target)
		}
		return ManagedAccount{}, fmt.Errorf(
			"read selected %s credentials: %w",
			providerName,
			err,
		)
	}
	// A husked slot is a login the provider already ended. Installing it into
	// the canonical store would retire the working account for a dead one and
	// leave the app looking signed in until the next request failed.
	if m.credentials.CredentialSignedOut(providerName, targetCredential.Data) {
		m.audit(
			"refused to switch to %s account %s: its saved credential is the provider's sign-out marker",
			providerName,
			accountID,
		)
		return ManagedAccount{}, errProviderAccountNeedsLogin(providerName, target)
	}
	// An expired login is not yet a husk — the tokens are all still there —
	// but activating it hands the provider a refresh token the server will
	// reject, and the CLI answers that rejection by blanking the credential.
	// So the switch stops here instead: the account is reported as needing a
	// sign-in, and the answer says so rather than failing, because nothing
	// went wrong.
	if m.credentials.CredentialLoginExpired(providerName, targetCredential.Data, time.Now()) {
		m.audit(
			"declined to switch to %s account %s: its login expired at %s",
			providerName,
			accountID,
			describeLoginExpiry(providerName, targetCredential.Data),
		)
		expired := m.withRecordedLoginExpiry(target, targetCredential.Data)
		return ManagedAccount{
			Account:        expired,
			Generation:     m.store.Generation(providerName),
			NeedsLogin:     true,
			SignInRequired: true,
		}, nil
	}
	currentID := ""
	if hasCurrent {
		currentID = current.ID
	}
	preserveID, currentCredential, err := m.reconciledActiveCredentialLocked(
		providerName,
		currentID,
		accountID,
	)
	if err != nil {
		return ManagedAccount{}, err
	}
	if err := m.credentials.ActivateWithSnapshot(
		providerName,
		preserveID,
		accountID,
		currentCredential,
	); err != nil {
		// The write layer re-reads the target slot, so it can meet a husk the
		// pre-check above did not see. Same verdict, same instruction — not a
		// raw internal error.
		if errors.Is(err, provideraccounts.ErrSignedOutCredential) {
			return ManagedAccount{}, errProviderAccountNeedsLogin(providerName, target)
		}
		return ManagedAccount{}, fmt.Errorf("switch %s account: %w", providerName, err)
	}
	if err := m.verifyCanonicalProviderCredentialLocked(
		providerName,
		targetCredential.Data,
	); err != nil {
		return ManagedAccount{}, err
	}
	account, err := m.store.Activate(providerName, accountID)
	if err != nil {
		if rollbackErr := m.rollbackProviderAccountActivation(providerName, accountID, currentID); rollbackErr != nil {
			return ManagedAccount{}, fmt.Errorf("%w (credential rollback also failed: %v)", err, rollbackErr)
		}
		return ManagedAccount{}, err
	}
	// Both accounts just had credential bytes committed for them: the incoming
	// one into the canonical store, the outgoing one into its slot. Record
	// what each says about its session deadline while the bytes are in hand.
	account = m.withRecordedLoginExpiry(account, targetCredential.Data)
	if currentCredential != nil {
		m.recordLoginExpiry(providerName, preserveID, currentCredential.Data)
	}
	binary := m.providerBinaryPath(providerName)
	m.rememberProviderCredentialFingerprintLocked(providerName, targetCredential.Data)
	generation := m.store.Generation(providerName)
	m.mu.Unlock()
	accountLocked = false
	// Probe invalidation derives the newly active account from Manager state.
	// Keep it outside mu: taking Selection's read side while this write side is
	// held self-deadlocks, and the invalidator is an external dependency anyway.
	m.invalidateProviderAccountProbe(providerName, binary)
	info := AccountInfo(account)
	m.emitProviderAccount(providerName, account, info, generation)
	// Which card is active is part of the listing, and the per-account frame
	// above carries no `active` flag for the card that just LOST it.
	m.emitProviderAccountsChanged()
	m.applySelection(providerName, generation, account.ID)
	reconcileMu.Unlock()
	reconcileLocked = false
	return ManagedAccount{Account: account, Active: true, Generation: generation}, nil
}

// reconciledActiveCredentialLocked captures the credential value already
// associated with currentAccountID so activation can preserve it into that
// account's slot. Returns the account ID to preserve under — "" when there is
// nothing to preserve — paired with the snapshot, so callers thread both
// straight into ActivateWithSnapshot and cannot pass an ID without its value.
// The caller holds mu after a successful external-login
// reconciliation. Passing this exact snapshot into activation prevents a
// concurrent native login from being preserved under the stale account ID.
//
// A canonical credential the CLI blanked after a failed refresh reports
// errActiveCredentialSignedOut from the verify below and is mapped to
// "nothing to preserve" here: the outgoing slot already holds its last saved
// pair, and the husk must not overwrite it. Refusing the mutation instead
// locks the user out of every switch and delete until an external login
// replaces the file (incident 2026-08-03).
func (m *Manager) reconciledActiveCredentialLocked(
	providerName string,
	currentAccountID string,
	targetAccountID string,
) (string, *provideraccounts.CredentialSnapshot, error) {
	if currentAccountID == "" || currentAccountID == targetAccountID {
		return "", nil, nil
	}
	snapshot, err := m.verifiedActiveCredentialLocked(providerName)
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

func (m *Manager) verifiedActiveCredentialLocked(
	providerName string,
) (provideraccounts.CredentialSnapshot, error) {
	snapshot, err := m.credentials.ReadCredentialSnapshot(providerName, "", true)
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
	known, ok := m.fingerprints[providerName]
	if !ok || known != fingerprint {
		return provideraccounts.CredentialSnapshot{}, fmt.Errorf(
			"%s credentials changed while updating accounts; retry",
			providerName,
		)
	}
	return snapshot, nil
}

// recordLoginExpiry keeps one account's stored OAuth session deadline equal to
// what the credential AO has just captured or committed for it says. Every
// place that moves credential bytes for an account calls it, so the metadata
// answer never drifts from the bytes: a login, an external adoption, a switch,
// and the usage refresh that commits a rotation.
//
// The deadline is metadata (see provideraccounts.Account), and recording it is
// NOT part of the credential transaction. The bytes are already durable by the
// time this runs; failing the user's operation because a metadata write failed
// would undo nothing and report the wrong cause, so the failure is audited and
// the operation stands. The next capture retries it.
func (m *Manager) recordLoginExpiry(
	providerName, accountID string,
	credential []byte,
) (provideraccounts.Account, bool) {
	if m.store == nil || accountID == "" {
		return provideraccounts.Account{}, false
	}
	updated, changed, err := m.store.NoteRefreshTokenExpiry(
		providerName,
		accountID,
		loginExpiresAtMillis(providerName, credential),
	)
	if err != nil {
		m.audit(
			"record %s account %s login expiry: %v",
			providerName,
			accountID,
			err,
		)
		return provideraccounts.Account{}, false
	}
	return updated, changed
}

// withRecordedLoginExpiry is recordLoginExpiry for the callers that are about
// to publish the account row, so the frame they emit carries the deadline the
// credential just supplied rather than the previous one.
func (m *Manager) withRecordedLoginExpiry(
	account provideraccounts.Account,
	credential []byte,
) provideraccounts.Account {
	if updated, changed := m.recordLoginExpiry(account.Provider, account.ID, credential); changed {
		return updated
	}
	return account
}

// describeLoginExpiry renders an account's session deadline for the audit log.
func describeLoginExpiry(providerName string, credential []byte) string {
	expiresAt := loginExpiresAtMillis(providerName, credential)
	if expiresAt <= 0 {
		return "an unreadable time"
	}
	return time.UnixMilli(expiresAt).Format(time.RFC3339)
}

func (m *Manager) probeProviderAccountAtHome(providerName, binary, home string) (provider.AccountInfo, error) {
	switch providerName {
	case string(provider.Claude):
		return claude.ProbeAccount(m.context(), m.claudeProbeConfig(
			binary,
			map[string]string{"CLAUDE_CONFIG_DIR": home},
		))
	case string(provider.Codex):
		return codex.ProbeAccount(m.context(), m.codexProbeConfig(
			binary,
			map[string]string{"CODEX_HOME": home},
		))
	default:
		return provider.AccountInfo{}, fmt.Errorf("unsupported provider %q", providerName)
	}
}

func (m *Manager) invalidateProviderAccountProbe(providerName, binary string) {
	if m.deps.Probes != nil {
		m.deps.Probes.Invalidate(providerName, m.providerProbeCacheKey(providerName, binary))
	}
}

func (m *Manager) adoptCurrentProviderAccount(
	providerName string,
	info provider.AccountInfo,
	credential *provideraccounts.CredentialSnapshot,
) (provideraccounts.Account, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.store == nil || m.credentials == nil {
		return provideraccounts.Account{}, false, nil
	}
	if credential == nil {
		return provideraccounts.Account{}, false, nil
	}
	if err := m.verifyCanonicalProviderCredentialLocked(
		providerName,
		credential.Data,
	); err != nil {
		return provideraccounts.Account{}, false, err
	}
	if _, hasActive := m.store.Active(providerName, time.Now()); !hasActive &&
		strings.TrimSpace(info.Email) == "" {
		accountID := uuid.NewString()
		saved, err := m.credentials.CaptureAccountCredential(providerName, accountID)
		if err != nil {
			return provideraccounts.Account{}, false, err
		}
		if err := m.credentials.WriteAccountCredential(
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
		account, err := m.store.UpsertAndActivate(
			AccountFromInfo(accountID, providerName, info),
		)
		if err != nil {
			cleanupErr := m.credentials.RestoreAccountCredential(saved)
			return provideraccounts.Account{}, false, errors.Join(err, cleanupErr)
		}
		account = m.withRecordedLoginExpiry(account, credential.Data)
		m.rememberProviderCredentialFingerprintLocked(providerName, credential.Data)
		return account, true, nil
	}
	account, changed, err := m.reconcileObservedAccountLocked(
		providerName,
		info,
		*credential,
	)
	if err != nil {
		return provideraccounts.Account{}, false, err
	}
	m.rememberProviderCredentialFingerprintLocked(providerName, credential.Data)
	return account, changed, nil
}

func (m *Manager) adoptCanonicalProviderAccountLocked(providerName, binary string) error {
	if _, ok := m.store.Active(providerName, time.Now()); ok {
		return nil
	}
	// Identifying an expired login costs it: the probe is a CLI start, the CLI
	// starts by refreshing, and that refresh answers invalid_grant and blanks
	// the credential. There is nothing here worth adopting anyway — whatever
	// is in the canonical store needs a fresh sign-in, which is what the
	// caller is on its way to running. Same shape as the husk case in
	// reconcileExternalProviderAccountWithMutexHeld: not an error, because
	// refusing here would lock the user out of the very sign-in that repairs
	// it.
	if expired, err := m.canonicalLoginExpired(providerName); err != nil {
		return err
	} else if expired {
		m.audit(
			"skipped adopting the canonical %s login: its login has expired and probing it would sign it out",
			providerName,
		)
		return nil
	}
	info, credential, err := m.runStableAccountProbe(
		providerName,
		m.canonicalProviderAccountProbe(providerName, binary),
	)
	if err != nil {
		return err
	}
	if credential == nil {
		return nil
	}
	if err := m.verifyCanonicalProviderCredentialLocked(
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
	m.backfillCodexOrgIDs(providerName, info.Email)
	existing, found, err := m.findAccountByObservedIdentity(providerName, info)
	if err != nil {
		return fmt.Errorf(
			"resolve current %s login %s against saved accounts: %w",
			providerName,
			DescribeObservedAccount(info),
			err,
		)
	}
	if found {
		accountID = existing.ID
	}
	saved, err := m.credentials.CaptureAccountCredential(providerName, accountID)
	if err != nil {
		return err
	}
	restore := func() {
		if cleanupErr := m.credentials.RestoreAccountCredential(saved); cleanupErr != nil {
			log.Printf("provider accounts: restore adoption credential: %v", cleanupErr)
		}
	}
	if err := m.credentials.WriteAccountCredential(
		providerName,
		accountID,
		credential.Data,
	); err != nil {
		restore()
		return err
	}
	if _, err := m.store.UpsertAndActivate(AccountFromInfo(accountID, providerName, info)); err != nil {
		restore()
		return err
	}
	m.recordLoginExpiry(providerName, accountID, credential.Data)
	m.rememberProviderCredentialFingerprintLocked(providerName, credential.Data)
	return nil
}

// rollbackProviderAccountActivation reinstates currentID's credential as
// canonical after a failed activation. canonicalHolderID names the account
// whose credential the canonical store holds at this moment (the activation
// that is being unwound), so Activate can preserve those bytes — including
// any rotation a live CLI performed since — into that account's slot instead
// of destroying them. Pass "" when canonical holds nothing attributable.
func (m *Manager) rollbackProviderAccountActivation(providerName, canonicalHolderID, currentID string) error {
	if currentID != "" {
		return m.credentials.Activate(providerName, canonicalHolderID, currentID)
	}
	return m.credentials.RemoveActive(providerName)
}

func (m *Manager) publishUsageError(providerName, accountID string, err error) {
	if m.deps.Accounts != nil {
		m.deps.Accounts.PublishUsageError(providerName, accountID, err)
	}
}
