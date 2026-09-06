package app

import (
	"context"
	"net/http"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/provideraccountapp"
	"agent-overflow/internal/provideraccounts"
	"agent-overflow/internal/providerdiscoveryapp"
	"agent-overflow/internal/providerlifecycleapp"
	"agent-overflow/internal/settings"
)

func (a *App) providerDiscoveryService() *providerdiscoveryapp.Service {
	a.providerDiscoveryOnce.Do(func() {
		caches := a.providerDiscoveryCaches
		if caches == nil {
			caches = providerdiscoveryapp.DefaultCaches()
		}
		a.providerDiscovery = providerdiscoveryapp.New(providerdiscoveryapp.Deps{
			CurrentSettings: a.currentSettings,
			SettingsService: func() *settings.Service { return a.settings },
			ProviderBinary:  a.providerBinaryPath,
			Selection: func(providerName string) providerdiscoveryapp.AccountSelection {
				selection := a.captureProviderAccountSelection(providerName)
				return providerdiscoveryapp.AccountSelection{AccountID: selection.AccountID}
			},
			ProbeKey:       a.providerProbeCacheKeyForAccount,
			ProbeKeyForEnv: providerProbeCacheKeyForAccountEnv,
			RunAccountProbe: func(request providerdiscoveryapp.AccountProbeRequest) (provider.AccountInfo, error) {
				return a.runAccountProbe(provideraccountapp.ProbeRequest{
					ProviderName:    request.ProviderName,
					Cache:           request.Cache,
					Key:             request.Key,
					Probe:           request.Probe,
					Unauthenticated: request.Unauthenticated,
					EmitUnauth:      request.EmitUnauth,
					AfterAdopt:      request.AfterAdopt,
				})
			},
			ClaudeConfig: func(binary string) claude.ProbeConfig {
				return a.claudeProbeConfig(binary, nil)
			},
			CodexConfig: func(binary string) codex.ProbeConfig {
				return a.codexProbeConfig(binary, nil)
			},
			EmitStatus:     a.emitProviderStatus,
			EmitRateLimits: a.emitRateLimitsSnapshot,
			CheckClaudeTransferAccount: func(ctx context.Context, binary string) error {
				return a.ensureProviderAccountManager().CheckClaudeTransferAccount(ctx, binary)
			},
			CheckCodexTransferAccount: func(ctx context.Context, binary string) error {
				return a.ensureProviderAccountManager().CheckCodexTransferAccount(ctx, binary)
			},
		}, caches)
	})
	return a.providerDiscovery
}

func (a *App) providerLifecycleService() *providerlifecycleapp.Service {
	a.providerLifecycleOnce.Do(func() {
		a.providerLifecycle = providerlifecycleapp.New(providerlifecycleapp.Deps{
			Context:        a.lifeCtx,
			IsShuttingDown: func() bool { return a.shuttingDown.Load() },
			Emit:           a.emit,
			Accounts: providerlifecycleapp.AccountDeps{
				Selection: func(providerName string) providerlifecycleapp.Selection {
					selection := a.captureProviderAccountSelection(providerName)
					return providerlifecycleapp.Selection{AccountID: selection.AccountID}
				},
				Reconcile: a.reconcileExternalProviderAccount,
				RefreshUsage: func(ctx context.Context, providerName, accountID string) error {
					return a.ensureProviderAccountManager().RefreshProviderAccountUsageContext(
						ctx, providerName, accountID,
					)
				},
				ResolveObserved: a.accountIDForObservedIdentity,
				PublishObserved: func(providerName string, account provideraccounts.Account) {
					a.emitProviderAccountIfCurrent(
						providerName, account, providerAccountInfo(account),
					)
				},
				Account: func(providerName, accountID string) (provideraccounts.Account, bool) {
					if a.providerAccounts == nil {
						return provideraccounts.Account{}, false
					}
					return a.providerAccounts.Account(providerName, accountID)
				},
				List: func(providerName string) []provideraccounts.Account {
					if a.providerAccounts == nil {
						return nil
					}
					return a.providerAccounts.MetadataAccounts(providerName)
				},
				RememberRateLimit: func(providerName, accountID string, snapshot provider.RateLimitsSnapshot) error {
					if a.providerAccounts == nil {
						return nil
					}
					return a.providerAccounts.RememberRateLimits(providerName, accountID, snapshot)
				},
			},
			Sessions: providerlifecycleapp.SessionDeps{
				Account: func(threadID string) (providerlifecycleapp.RuntimeAccount, bool) {
					snapshot, ok := a.sessionManager().runtime.Account(threadID)
					return providerlifecycleapp.RuntimeAccount{
						Provider: snapshot.Provider, SessionToken: snapshot.SessionToken,
						CredentialGeneration: snapshot.CredentialGeneration,
						CredentialAccountID:  snapshot.CredentialAccountID,
						CredentialAccount:    snapshot.CredentialAccount,
					}, ok
				},
				RecordActivity: func(threadID, token string, kind provider.EventKind, content string, now time.Time) {
					a.sessionManager().runtime.RecordActivity(threadID, token, kind, content, now)
					switch kind {
					case provider.EventTurnStart, provider.EventTurnComplete:
						a.turnActivityUnixNano.Store(now.UnixNano())
					case provider.EventSessionStatus:
						if content == "disconnected" {
							a.turnActivityUnixNano.Store(now.UnixNano())
						}
					}
				},
			},
			Claude: providerlifecycleapp.ClaudeDeps{
				HTTPClient: a.rateLimitProbeClient,
				ProbeRateLimits: func(ctx context.Context, client *http.Client) (provider.RateLimitsSnapshot, error) {
					home, err := a.providerHome()
					if err != nil {
						return provider.RateLimitsSnapshot{}, err
					}
					return claude.ProbeRateLimits(ctx, client, home)
				},
			},
			Codex: providerlifecycleapp.CodexDeps{
				Binary: func() string { return a.providerBinaryPath(string(provider.Codex)) },
				ProbeConfig: func(binary string) codex.ProbeConfig {
					return a.codexProbeConfig(binary, nil)
				},
				Probe: codex.ProbeAccount,
			},
		})
	})
	return a.providerLifecycle
}

var _ provideraccountapp.UsageBackoff = (*providerlifecycleapp.Service)(nil)
