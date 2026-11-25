package provideraccountapp

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/provideraccounts"
)

// reconcileExternalProviderAccount detects a provider-native login completed
// outside Agent Overflow. The common path reads a small credential value and
// compares an in-memory digest. Only a changed (or not-yet-observed) value
// starts the provider's zero-token identity probe.
//
// The digest never leaves this process. Credential bytes remain owned by the
// provider-native store and are discarded immediately after hashing or copying
// into another provider-native credential slot.
//
// ctx bounds the admission, the wait for the provider's reconcile lock and
// the start of the identity probe; a probe that has started runs to its end.
func (m *Manager) reconcileExternalProviderAccount(ctx context.Context, providerName string) (canonicalState, error) {
	if m.store == nil || m.credentials == nil {
		return canonicalState{}, nil
	}
	if ValidateProvider(providerName) != nil {
		return canonicalState{}, nil
	}
	endWork, err := m.beginWork(ctx)
	if err != nil {
		return canonicalState{}, err
	}
	defer endWork()
	reconcileMu := m.reconcileMutex(providerName)
	if err := reconcileMu.LockContext(ctx); err != nil {
		return canonicalState{}, fmt.Errorf("wait for the %s account lock to check for an outside login: %w", providerName, err)
	}
	defer reconcileMu.Unlock()
	return m.reconcileSelectedLoginWithMutexHeld(ctx, providerName)
}

// ReconcileExternalProviderAccount adopts a provider-native login completed
// outside Agent Overflow under the provider-specific reconcile transaction.
// A login it cannot attribute is not an error here (see
// reconcileExternalProviderAccountWithMutexHeld).
func (m *Manager) ReconcileExternalProviderAccount(providerName string) error {
	_, err := m.reconcileExternalProviderAccount(m.context(), providerName)
	return err
}

// ReconcileExternalProviderAccountContext is ReconcileExternalProviderAccount
// for a caller with its own bound, such as the usage poll.
func (m *Manager) ReconcileExternalProviderAccountContext(ctx context.Context, providerName string) error {
	_, err := m.reconcileExternalProviderAccount(ctx, providerName)
	return err
}

// ReconcileBeforeSend is ReconcileExternalProviderAccount for a send: a
// canonical login reconciliation could not attribute to one saved account
// refuses the send with that verdict's sentence, since the provider would
// run the turn on a login nobody can name. Any other refusal of the
// canonical login (expired, signed out) is the provider's to answer, as it
// was.
func (m *Manager) ReconcileBeforeSend(providerName string) error {
	state, err := m.reconcileExternalProviderAccount(m.context(), providerName)
	if err != nil {
		return err
	}
	if errors.As(state.refusal, new(unresolvedLoginError)) {
		return state.refusal
	}
	return nil
}

// reconcileExternalProviderAccountWithMutexHeld is the reconciliation body for
// managed account mutations that must keep provider identity probes and usage
// refreshes ordered through their canonical credential write. The caller holds
// reconcileMutex(providerName). ctx bounds the start of the identity probe.
// A login it cannot attribute is not an error here: the selection stays, and
// the verdict is kept for the readings that refuse under it, so a switch,
// removal or sign-in that replaces it still goes through.
func (m *Manager) reconcileExternalProviderAccountWithMutexHeld(
	ctx context.Context,
	providerName string,
) error {
	_, err := m.reconcileSelectedLoginWithMutexHeld(ctx, providerName)
	return err
}

// reconcileSelectedLoginWithMutexHeld reconciles the canonical credential as
// it stands and returns what reconciliation decided about it; an absent
// credential decides nothing. The caller holds reconcileMutex(providerName).
func (m *Manager) reconcileSelectedLoginWithMutexHeld(
	ctx context.Context,
	providerName string,
) (canonicalState, error) {
	// A rotation another store's sweep recovered into the selected slot
	// goes back into the canonical store before anything reads it as the
	// selected login.
	if err := m.promoteActiveSlot(providerName); err != nil {
		return canonicalState{}, err
	}
	before, present, err := m.readCanonicalCredentialIfPresent(providerName)
	if err != nil {
		return canonicalState{}, err
	}
	if !present {
		return canonicalState{}, nil
	}
	return m.reconcileCanonicalWithMutexHeld(ctx, providerName, before)
}

// canonicalState is what reconciliation decided about the canonical
// credential, for a caller about to use it as the selected account's login.
type canonicalState struct {
	// credential is the canonical credential the decision is about: the
	// snapshot the caller passed when it was already recorded or judged,
	// otherwise the one the identification accepted.
	credential provideraccounts.CredentialSnapshot
	// identified reports that reconciliation named credential's login, so
	// the selection may have moved.
	identified bool
	// refusal is why credential is not the selected account's login, the
	// sentence a reading under it answers with; nil when it is.
	refusal error
}

// reconcileCanonicalWithMutexHeld reconciles the canonical credential before,
// already read under the reconcile lock the caller holds. A recorded
// credential, or one already judged unattributable, starts no probe.
func (m *Manager) reconcileCanonicalWithMutexHeld(
	ctx context.Context,
	providerName string,
	before provideraccounts.CredentialSnapshot,
) (canonicalState, error) {
	m.mu.RLock()
	recorded := m.canonicalRecordedLocked(providerName, before.Bytes())
	unresolved := m.unresolvedLocked(providerName, before.Bytes())
	m.mu.RUnlock()
	if recorded || unresolved != nil {
		return canonicalState{credential: before, refusal: unresolved}, nil
	}
	return m.identifyCanonicalWithMutexHeld(ctx, providerName, before)
}

// identifyCanonicalWithMutexHeld names the login of an unrecorded canonical
// credential and, when it is one saved account's, makes that account the
// selected one.
func (m *Manager) identifyCanonicalWithMutexHeld(
	ctx context.Context,
	providerName string,
	before provideraccounts.CredentialSnapshot,
) (canonicalState, error) {
	if m.credentials.CredentialLoginExpired(providerName, before.Bytes(), time.Now()) {
		// An expired login is a change this reconciliation cannot identify:
		// the probe that would name it is a CLI start, and starting the CLI on
		// an expired refresh token is what converts it into the husk below.
		// Nothing is recorded, so the sign-in that repairs the account still
		// reconciles as a change. Not an error: every account mutation runs
		// this first, and failing here would mean the user could neither
		// switch away from the dead account nor sign it back in.
		return canonicalState{credential: before, refusal: errClaudeLoginExpired}, nil
	}
	if providerName == string(provider.Claude) && claude.CredentialsSignedOut(before.Bytes()) {
		// claude >= 2.1.219 blanks the canonical credential in place when its
		// startup token refresh fails (spike 2026-08-03). That husk is a
		// sign-out, not an external login: adopting it would copy unusable
		// bytes over the active account's saved slot, the write that
		// destroyed a saved login on 2026-08-03. Nothing is recorded, so a
		// later real login still reads as a change and reconciles.
		m.audit("claude canonical credential found signed out: it is the CLI's sign-out marker")
		return canonicalState{credential: before, refusal: errClaudeCredentialSignedOut}, nil
	}

	// An external login process does not participate in mu, so
	// the identity has to be paired with a canonical credential value that did
	// not move across the probe — otherwise one account's email could be stored
	// against another account's tokens. runStableAccountProbe enforces that
	// pairing and, unlike a single before/after comparison, tolerates the
	// provider legitimately rotating the token while its probe initializes.
	binary := m.providerBinaryPath(providerName)
	info, credential, err := m.runStableAccountProbe(
		ctx,
		providerName,
		m.canonicalProviderAccountProbe(providerName, binary),
		m.identifyClaudeCredential,
	)
	if err != nil {
		return canonicalState{}, fmt.Errorf("identify externally selected %s account: %w", providerName, err)
	}
	if credential == nil {
		// The login was signed out while it was being identified. There is
		// nothing to adopt, and the next reconciliation sees the absence.
		return canonicalState{refusal: fmt.Errorf(
			"%s was signed out while the app checked who is signed in", providerLabel(providerName),
		)}, nil
	}
	fingerprint := sha256.Sum256(credential.Bytes())

	m.mu.Lock()
	// Serialize against Agent Overflow's own account switch and repeat the
	// stability check inside that boundary before changing metadata.
	current, err := m.credentials.ReadCredentialSnapshot(providerName, "", true)
	if err != nil {
		m.mu.Unlock()
		return canonicalState{}, fmt.Errorf("recheck active %s credentials before activation: %w", providerName, err)
	}
	if sha256.Sum256(current.Bytes()) != fingerprint {
		m.mu.Unlock()
		return canonicalState{}, fmt.Errorf("%s credentials changed before account activation; retry", providerName)
	}

	previous, _ := m.store.Active(providerName, time.Now())
	account, signIn, changed, err := m.reconcileObservedAccountLocked(providerName, info, current)
	var unresolved unresolvedLoginError
	if errors.As(err, &unresolved) {
		m.markUnresolvedLocked(providerName, current.Bytes(), err)
		m.mu.Unlock()
		m.audit("left the canonical %s login unattributed: %v", providerName, err)
		return canonicalState{credential: current, identified: true, refusal: err}, nil
	}
	if err == nil {
		m.recordCanonicalLocked(providerName, current.Bytes())
	}
	generation := m.store.Generation(providerName)
	m.mu.Unlock()
	if err != nil {
		return canonicalState{}, err
	}
	state := canonicalState{credential: current, identified: true}
	if !changed {
		return state, nil
	}

	m.invalidateProviderAccountProbe(providerName, binary)
	m.emitProviderAccount(providerName, account, info, generation)
	m.emitReconciledSignIn(signIn)

	// Claude processes intentionally follow the canonical native credential
	// store, so every live Claude session adopts the observed account without
	// a restart. Codex app-servers cache auth and remain attributed to their
	// old account until the existing reconnect-on-next-send gate runs; the
	// gateway leaves their sessions alone.
	m.applySelection(providerName, generation, previous.ID, account.ID)
	return state, nil
}

// canonicalProviderAccountProbe asks the provider who is logged in to its
// canonical native home. It deliberately sets no config-home override: Claude
// keys "is this the default home" off the variable being absent rather than off
// its value, and on macOS a non-default home hashes into a different Keychain
// service.
func (m *Manager) canonicalProviderAccountProbe(
	providerName string,
	binary string,
) func(context.Context) (provider.AccountInfo, error) {
	return func(ctx context.Context) (provider.AccountInfo, error) {
		switch providerName {
		case string(provider.Claude):
			return claude.ProbeAccount(ctx, m.claudeProbeConfig(binary, nil))
		case string(provider.Codex):
			return codex.ProbeIdentity(ctx, m.codexProbeConfig(binary, nil))
		case string(provider.Agy):
			return m.probeCanonicalAgy(ctx, binary, m.providerCustomEnv(providerName))
		default:
			return provider.AccountInfo{}, fmt.Errorf("unsupported provider %q", providerName)
		}
	}
}

// resolveObservedLoginLocked names the saved account an observed canonical
// login belongs to: found is false for a login no saved account has, which
// adoption appends. A login it cannot attribute to one saved account is an
// unresolvedLoginError. Every judge of an unrecorded canonical login asks
// this: reconciliation, and the selected Claude refresh of the rotation the
// CLI published there. The caller holds mu (write side: a Codex adoption may
// backfill legacy workspaces).
func (m *Manager) resolveObservedLoginLocked(
	providerName string,
	info provider.AccountInfo,
) (target provideraccounts.Account, found bool, err error) {
	active, hasActive := m.store.Active(providerName, time.Now())
	email := strings.TrimSpace(info.Email)
	if email == "" {
		// Codex documents email as nullable, API-key auth has no user email,
		// and a Claude token without profile access names nobody. Such a
		// login is attributed only to a selected account that has no saved
		// identity either, and only while nothing the login carries (a Codex
		// workspace) contradicts it; any other selected account is somebody,
		// and nothing proves this login is theirs.
		if !hasActive || !blankEmailAccountTakes(active, info) {
			selected := "the selected account"
			if hasActive {
				selected = describeProviderAccount(active)
			}
			return provideraccounts.Account{}, false, errIdentitylessLogin(providerName, selected)
		}
		return active, true, nil
	}
	// An adoption may be the first sighting of a second workspace on a
	// known email; resolve legacy blank-org Codex rows from their slots
	// first so the blank cannot match the wrong workspace.
	m.backfillCodexOrgIDs(providerName, email)
	target, found, findErr := m.findAccountByObservedIdentity(providerName, info)
	switch {
	case errors.Is(findErr, provideraccounts.ErrAmbiguousIdentity):
		// Several saved accounts share this email and the observation
		// names no organization: nothing says which one it is, the
		// selected one included.
		return provideraccounts.Account{}, false, errAmbiguousLogin(providerName, info)
	case findErr != nil:
		return provideraccounts.Account{}, false, fmt.Errorf(
			"resolve observed %s account %s: %w",
			providerName,
			DescribeObservedAccount(info),
			findErr,
		)
	}
	if !found && hasActive && blankEmailAccountTakes(active, info) {
		// Enrich an account adopted before identity was available rather
		// than manufacturing a duplicate the first time the provider
		// returns email.
		return active, true, nil
	}
	return target, found, nil
}

// blankEmailAccountTakes reports that an account saved without an email may
// be named by an observed login: nothing the login carries contradicts what
// is known of the account. It is the one rule for such an account, whether
// the login names an email or not.
func blankEmailAccountTakes(account provideraccounts.Account, info provider.AccountInfo) bool {
	return strings.TrimSpace(account.Email) == "" &&
		!provideraccounts.AccountIdentity(account).Contradicts(provideraccounts.IdentityFromInfo(info))
}

// reconcileObservedAccountLocked maps an observed provider identity onto the
// saved credential set and activates it, because the canonical credential is
// what every provider process runs on. signIn says what that did to the saved
// set; a rotation of the selected account's login reports none of it. A
// login it cannot attribute to one saved account is an unresolvedLoginError,
// returned before anything is written. The caller has verified that the
// canonical credential value is stable and holds mu.
func (m *Manager) reconcileObservedAccountLocked(
	providerName string,
	info provider.AccountInfo,
	credential provideraccounts.CredentialSnapshot,
) (
	account provideraccounts.Account,
	signIn SignIn,
	changed bool,
	retErr error,
) {
	active, hasActive := m.store.Active(providerName, time.Now())
	email := strings.TrimSpace(info.Email)
	if email == "" && !hasActive {
		// No identity and nothing selected: there is no safe assignment.
		return active, SignIn{}, false, nil
	}
	// The provider's sign-out marker is no login at all, whoever it names:
	// refused before it is weighed as an identity, as the slot write would.
	if m.credentials.CredentialSignedOut(providerName, credential.Bytes()) {
		return provideraccounts.Account{}, SignIn{}, false, fmt.Errorf(
			"preserve externally selected %s credentials: %w", providerName, provideraccounts.ErrSignedOutCredential,
		)
	}
	target, found, err := m.resolveObservedLoginLocked(providerName, info)
	if err != nil {
		return provideraccounts.Account{}, SignIn{}, false, err
	}
	if !found {
		reservation, err := m.reserveAccount(providerName)
		if err != nil {
			return provideraccounts.Account{}, SignIn{}, false, err
		}
		defer reservation.settleOnFailure(&retErr)
		target.ID = reservation.ID()
		target.Provider = providerName
	}
	if !hasActive || target.ID != active.ID {
		// The observed login is another account's: its adoption moves the
		// selection. A use that writes a refreshed login as it ends fails the
		// stability check below, and the next reconciliation reads it.
		m.selectionChanging(providerName)
	}
	var previous []byte
	if found {
		// Only classifies the outcome: a slot that cannot be read is treated
		// as holding no deadline, which reports no renewal.
		previous, _ = m.credentials.ReadCredential(providerName, target.ID, false)
	}
	saved, err := m.credentials.CaptureAccountCredential(providerName, target.ID)
	if err != nil {
		return provideraccounts.Account{}, SignIn{}, false, err
	}
	restoreCredentialOnError := true
	defer func() {
		if !restoreCredentialOnError || retErr == nil {
			return
		}
		if cleanupErr := m.credentials.RestoreAccountCredential(saved, credential.Bytes()); cleanupErr != nil {
			retErr = errors.Join(
				retErr,
				fmt.Errorf(
					"restore saved %s credential %s after external login failure: %w",
					providerName,
					target.ID,
					cleanupErr,
				),
			)
		}
	}()

	if err := m.credentials.WriteAccountCredential(
		providerName,
		target.ID,
		credential.Bytes(),
	); err != nil {
		return provideraccounts.Account{}, SignIn{}, false, fmt.Errorf(
			"preserve externally selected %s credentials: %w",
			providerName,
			err,
		)
	}
	stable, err := m.credentials.ReadCredentialSnapshot(providerName, "", true)
	if err != nil {
		return provideraccounts.Account{}, SignIn{}, false, fmt.Errorf(
			"recheck externally selected %s credentials: %w",
			providerName,
			err,
		)
	}
	if sha256.Sum256(stable.Bytes()) != sha256.Sum256(credential.Bytes()) {
		return provideraccounts.Account{}, SignIn{}, false, fmt.Errorf(
			"%s credentials changed before metadata activation; retry",
			providerName,
		)
	}

	updated := target
	if email != "" {
		updated = AccountFromInfo(target.ID, providerName, info)
	}
	// Antigravity refreshes its 1-hour OAuth access token in-process using the
	// same refresh token and writes the rotated access token to the canonical
	// store. When the login continues the already-selected account's refresh
	// token, the live agy acp process already holds the refreshed token in
	// memory: advancing Generation would treat a routine hourly token refresh
	// as an account switch, blocking mid-turn steers until turn end and
	// restarting the live session afterwards.
	sameAgyTokenRefresh := providerName == string(provider.Agy) &&
		found && hasActive && target.ID == active.ID &&
		agyLoginContinues(credential.Bytes(), previous)
	if sameAgyTokenRefresh {
		account, err = m.store.UpsertAndActivate(updated)
	} else {
		account, err = m.store.UpsertAndActivateCredential(updated)
	}
	if err != nil {
		return provideraccounts.Account{}, SignIn{}, false, fmt.Errorf(
			"activate externally selected %s account metadata: %w",
			providerName,
			err,
		)
	}
	account = m.withRecordedLoginExpiry(account, credential.Bytes())
	restoreCredentialOnError = false
	signIn = SignIn{
		Provider: providerName,
		Account:  account,
		Added:    !found,
		Renewed:  found && CredentialLoginRenewed(providerName, previous, credential.Bytes()),
		Switched: hasActive && active.ID != account.ID,
		Active:   true,
	}
	if sameAgyTokenRefresh {
		return account, signIn, false, nil
	}
	// Reaching this point means the canonical credential fingerprint changed.
	// Publish the new generation even when provider identity metadata stayed
	// identical so Codex threads reconnect before their next safe send.
	changed = true
	return account, signIn, changed, nil
}

// emitReconciledSignIn publishes an adopted login's outcome. An added account
// or a moved selection also changes the listing, which no per-account frame
// can express.
func (m *Manager) emitReconciledSignIn(signIn SignIn) {
	if signIn.Added || signIn.Switched {
		m.emitProviderAccountsChanged(signIn.Provider)
	}
	m.emitSignIn(signIn)
}

// verifyCanonicalProviderCredentialLocked confirms an activation still owns
// the canonical native store before metadata is committed. External provider
// login processes do not share mu, so every managed write needs
// this explicit compare. The caller holds mu.
func (m *Manager) verifyCanonicalProviderCredentialLocked(
	providerName string,
	expected []byte,
) error {
	current, err := m.credentials.ReadCredential(providerName, "", true)
	if err != nil {
		m.forgetCanonicalLocked(providerName)
		return fmt.Errorf("verify active %s credentials: %w", providerName, err)
	}
	if sha256.Sum256(current) != sha256.Sum256(expected) {
		m.forgetCanonicalLocked(providerName)
		return fmt.Errorf(
			"%s credentials changed during account activation; retry",
			providerName,
		)
	}
	return nil
}

// accountIDForObservedIdentity prevents a usage snapshot from being stamped
// onto the selected metadata account when the fresh provider probe identifies
// a different login. Empty-email modes cannot be disambiguated and retain the
// expected saved-account assignment. The expected account holds its claim as
// long as nothing observed CONTRADICTS it (Identity.Contradicts) — same
// email on a provably different organization is a different login and
// reassigns exactly like a different email does.
//
// Once attributed, the observation's metadata is recorded on that account.
// The returned account is non-nil only when the stored row changed, so the
// caller publishes a card only then.
func (m *Manager) accountIDForObservedIdentity(
	providerName string,
	expectedAccountID string,
	info provider.AccountInfo,
) (string, *provideraccounts.Account, error) {
	email := strings.TrimSpace(info.Email)
	if email == "" || m.store == nil {
		return expectedAccountID, nil, nil
	}
	saved, err := m.attributeObservedIdentity(providerName, expectedAccountID, info)
	if err != nil {
		return "", nil, err
	}
	updated, err := m.recordObservedMetadata(providerName, saved, info)
	if err != nil {
		return "", nil, err
	}
	return saved.ID, updated, nil
}

// attributeObservedIdentity names the saved account an observation that
// carries an email describes: the expected account while the observation
// confirms it, otherwise the saved account matching it, otherwise an
// expected account saved before its email was known.
func (m *Manager) attributeObservedIdentity(
	providerName string,
	expectedAccountID string,
	info provider.AccountInfo,
) (provideraccounts.Account, error) {
	observed := provideraccounts.IdentityFromInfo(info)
	expected, hasExpected := m.store.Get(providerName, expectedAccountID, time.Now())
	if hasExpected && provideraccounts.AccountIdentity(expected).Confirms(observed) {
		return expected, nil
	}
	resolved, found, err := m.findAccountByObservedIdentity(providerName, info)
	if err != nil {
		return provideraccounts.Account{}, fmt.Errorf(
			"resolve observed %s account %s: %w",
			providerName,
			DescribeObservedAccount(info),
			err,
		)
	}
	if found {
		return resolved, nil
	}
	if hasExpected && blankEmailAccountTakes(expected, info) {
		return expected, nil
	}
	return provideraccounts.Account{}, fmt.Errorf(
		"%s identity probe returned unsaved account %s instead of selected account",
		providerName,
		DescribeObservedAccount(info),
	)
}

// recordObservedMetadata writes what an attributed observation reports over
// the saved row: each non-blank field replaces the saved value, a blank keeps
// it, and the organization name follows the store's OrgNameReported rule.
// It returns the stored row only when it changed.
func (m *Manager) recordObservedMetadata(
	providerName string,
	saved provideraccounts.Account,
	info provider.AccountInfo,
) (*provideraccounts.Account, error) {
	observed := AccountFromInfo(saved.ID, providerName, info)
	metadata := saved
	for _, field := range []struct {
		saved    *string
		observed string
	}{
		{&metadata.Email, observed.Email},
		{&metadata.DisplayName, observed.DisplayName},
		{&metadata.SubscriptionType, observed.SubscriptionType},
		{&metadata.TokenSource, observed.TokenSource},
		{&metadata.APIProvider, observed.APIProvider},
		{&metadata.OrgID, observed.OrgID},
		{&metadata.OrgName, observed.OrgName},
	} {
		if field.observed != "" {
			*field.saved = field.observed
		}
	}
	if observed.OrgNameReported {
		metadata.OrgName = observed.OrgName
		metadata.OrgNameReported = true
	}
	updated, err := m.store.UpdateMetadata(metadata)
	if err != nil {
		return nil, fmt.Errorf("save observed %s account identity: %w", providerName, err)
	}
	// The row's pointer fields are carried over, never written here: compare
	// what this write can change.
	before, after := saved, updated
	before.RateLimits, after.RateLimits = nil, nil
	before.UsageResets, after.UsageResets = nil, nil
	if before == after {
		return nil, nil
	}
	return &updated, nil
}

// ResolveObservedAccount attributes a provider-reported identity while
// keeping metadata lookup/backfill inside the Manager lock domain.
func (m *Manager) ResolveObservedAccount(
	providerName, expectedAccountID string,
	info provider.AccountInfo,
) (string, *provideraccounts.Account, error) {
	return m.accountIDForObservedIdentity(providerName, expectedAccountID, info)
}
