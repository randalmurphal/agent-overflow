package app

import (
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/provideraccountapp"
	"agent-overflow/internal/provideraccounts"
)

type providerAccountSelection = provideraccountapp.Selection

func (a *App) captureProviderAccountSelection(providerName string) providerAccountSelection {
	return a.ensureProviderAccountManager().Selection(providerName)
}

func (a *App) providerProbeCacheKeyForAccount(providerName, binary, accountID string) provider.ProbeCacheKey {
	if a.providerAccounts == nil {
		return provideraccountapp.ProbeCacheKeyForAccountEnv(binary, accountID, a.providerCustomEnv(providerName))
	}
	return a.providerAccounts.ProbeCacheKeyForAccount(providerName, binary, accountID)
}

func providerProbeCacheKeyForAccountEnv(binary, accountID string, customEnv map[string]string) provider.ProbeCacheKey {
	return provideraccountapp.ProbeCacheKeyForAccountEnv(binary, accountID, customEnv)
}

func (a *App) providerCustomEnv(providerName string) map[string]string {
	return a.currentSettings().ProviderEnvMap(providerName)
}

func (a *App) providerProbeEnv(providerName string, pins map[provider.ReservedEnv]string) provider.EnvInput {
	return a.providerAccounts.ProbeEnv(providerName, pins)
}

func (a *App) codexProbeConfig(binary string, pins map[provider.ReservedEnv]string) codex.ProbeConfig {
	return a.ensureProviderAccountManager().CodexProbeConfig(binary, pins)
}

func providerProbeWorkDir() string { return provideraccountapp.ProbeWorkDir() }

// reconcileProviderAccountBeforeSend reconciles an outside login before a
// send and refuses the send while the canonical login is unresolved.
func (a *App) reconcileProviderAccountBeforeSend(providerName string) error {
	if a.providerAccounts == nil {
		return nil
	}
	return a.providerAccounts.ReconcileBeforeSend(providerName)
}

func (a *App) accountIDForObservedIdentity(
	providerName, expectedAccountID string,
	info provider.AccountInfo,
) (string, *provideraccounts.Account, error) {
	return a.providerAccounts.ResolveObservedAccount(providerName, expectedAccountID, info)
}

func providerAccountInfo(account provideraccounts.Account) provider.AccountInfo {
	return provideraccountapp.AccountInfo(account)
}

type providerProbeRunner = provideraccountapp.ProbeRequest

func (a *App) runAccountProbe(r providerProbeRunner) (provider.AccountInfo, error) {
	return a.ensureProviderAccountManager().RunAccountProbe(r)
}

func (a *App) readCanonicalCredentialIfPresent(providerName string) (provideraccounts.CredentialSnapshot, bool, error) {
	return a.providerAccounts.ReadCanonicalCredential(providerName)
}

func (a *App) emitProviderAccountIfCurrent(
	providerName string,
	account provideraccounts.Account,
	info provider.AccountInfo,
) {
	a.providerAccounts.PublishAccountIfCurrent(providerName, account, info)
}

type providerAccountSessionGateway struct {
	app *App
}

var _ provideraccountapp.SessionGateway = providerAccountSessionGateway{}

// ApplySelection runs after every change of the selected account. The
// auto-switch coordinator hears of it first, so a refusal of a request a
// running turn sent on the previous account is not charged to the new one.
func (g providerAccountSessionGateway) ApplySelection(
	providerName, previousAccountID string,
	selection provideraccountapp.Selection,
) {
	g.app.accountSwitch().SelectionMoved(provider.UsageProviderFamily(providerName), previousAccountID, selection.AccountID)
	g.app.applyProviderAccountSelectionToSessions(
		providerName,
		selection.Generation,
		selection.AccountID,
	)
}

func (a *App) providerAccountSelectionLease(providerName string) *provideraccountapp.SelectionLease {
	if a.providerAccounts == nil {
		return provideraccountapp.NewSelectionLease(provideraccountapp.Selection{}, nil)
	}
	return a.providerAccounts.SelectionLease(providerName)
}

type providerAccountProbeInvalidator struct{ app *App }

func (i providerAccountProbeInvalidator) Invalidate(providerName string, key provider.ProbeCacheKey) {
	i.app.providerDiscoveryService().InvalidateProbe(providerName, key)
}

type providerAccountRateLimitSink struct{ app *App }

func (s providerAccountRateLimitSink) Publish(snapshot provider.RateLimitsSnapshot) {
	s.app.providerLifecycleService().EmitSnapshot(snapshot)
}

func (s providerAccountRateLimitSink) Forget(providerName, accountID string) {
	s.app.providerLifecycleService().Forget(providerName, accountID)
}

type providerAccountEventSink struct{ app *App }

func (s providerAccountEventSink) PublishAccount(
	providerName string,
	account provideraccounts.Account,
	info provider.AccountInfo,
	generation uint64,
) {
	s.app.emit(eventchan.ProviderAccount, ProviderAccountEvent{
		Provider: providerName, AccountID: account.ID, Account: info, Generation: generation,
	})
}

func (s providerAccountEventSink) PublishCleared(providerName string, generation uint64) {
	s.app.emit(eventchan.ProviderAccount, ProviderAccountEvent{
		Provider: providerName, Generation: generation, Cleared: true,
	})
}

// PublishAccountsChanged is a payload-less refetch nudge: the listing carries
// per-account quota snapshots and a `needsLogin` verdict that only
// ListProviderAccounts can compute, so a frame trying to carry the set would
// be a second, divergent projection of it. Auto-switch forgets only the
// changed provider's sign-in results.
func (s providerAccountEventSink) PublishAccountsChanged(providerName string) {
	s.app.emit(eventchan.ProviderAccountsChanged, nil)
	s.app.accountSwitch().AccountsChanged(provider.UsageProviderFamily(providerName))
}

// PublishListingChanged is a refetch nudge for a change only the listing
// carries (a card's usage resets or unsettled spend). It has its own channel
// so a client re-reads the listing and nothing that follows the saved set:
// not auto-switch's sign-in results, the Claude command catalogs or the
// dictation sources.
func (s providerAccountEventSink) PublishListingChanged(string) {
	s.app.emit(eventchan.ProviderAccountListing, nil)
}

func (s providerAccountEventSink) PublishLimitsReset(providerName, accountID string) {
	s.app.accountSwitch().LimitsReset(provider.UsageProviderFamily(providerName), accountID)
}

func (s providerAccountEventSink) PublishSignIn(signIn provideraccountapp.SignIn) {
	s.app.emit(eventchan.ProviderAccountSignIn, signIn)
}

func (s providerAccountEventSink) PublishUsageError(providerName, accountID string, err error) {
	s.app.emit(eventchan.ProviderAccountUsageError, map[string]string{
		"provider": providerName, "accountId": accountID, "message": err.Error(),
	})
}

// PublishLogin pushes one sign-in transition. The state is already the wire
// shape GetProviderLoginState answers with, so a client that missed a frame
// and one that polled see the same thing.
func (s providerAccountEventSink) PublishLogin(state provideraccountapp.LoginState) {
	s.app.emit(eventchan.ProviderLogin, state)
}

func newProviderAccountManager(app *App) *provideraccountapp.Manager {
	return provideraccountapp.NewManager(provideraccountapp.Deps{
		Reaper:          app.groupReaper("provider account"),
		Context:         app.lifeCtx,
		BeginWork:       app.workAdmission.begin,
		IsShuttingDown:  func() bool { return app.shuttingDown.Load() },
		ShutdownError:   ErrShuttingDown,
		CurrentSettings: app.currentSettings,
		ProviderBinary:  app.providerBinaryPath,
		ClaudeRetention: app.claudeRetention,
		OpenBrowser:     app.openExternalURL,
		LoginSpawnEnv:   func() map[string]string { return app.providerExtraEnv },
		HTTPClient:      app.rateLimitProbeClient,
		Sessions:        providerAccountSessionGateway{app: app},
		Probes:          providerAccountProbeInvalidator{app: app},
		RateLimits:      providerAccountRateLimitSink{app: app},
		Backoff:         app.providerLifecycleService(),
		Accounts:        providerAccountEventSink{app: app},
	})
}

// ensureProviderAccountManager preserves focused bare-App probe fixtures. Real
// Apps construct the Manager in NewAppWithVersion before concurrent use
// begins, while the Once keeps the startup fixture's parallel Claude/Codex
// probes race-free.
func (a *App) ensureProviderAccountManager() *provideraccountapp.Manager {
	a.providerAccountsOnce.Do(func() {
		if a.providerAccounts == nil {
			a.providerAccounts = newProviderAccountManager(a)
		}
	})
	return a.providerAccounts
}

// providerCredentialPolicy is the one root construction adapter for the
// provider-native credential rules owned by provideraccountapp.
func providerCredentialPolicy() provideraccounts.Policy {
	return provideraccountapp.CredentialPolicy()
}
