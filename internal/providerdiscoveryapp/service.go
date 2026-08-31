package providerdiscoveryapp

import (
	"context"
	"sync"

	"agent-overflow/internal/codexmodels"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/provideraccounts"
	"agent-overflow/internal/providerstatus"
	"agent-overflow/internal/settings"
)

// AccountSelection is the account identity dimension of a provider probe.
type AccountSelection struct {
	AccountID string
}

// AccountProbeRequest is the discovery side of the managed-account probe
// transaction. The injected runner keeps credential stability and adoption in
// provideraccountapp; discovery owns only the provider-specific wire request
// and the cache surrounding it.
type AccountProbeRequest struct {
	ProviderName    string
	Cache           *provider.ProbeCache
	Key             provider.ProbeCacheKey
	Probe           func(context.Context) (provider.AccountInfo, error)
	Unauthenticated func(provider.AccountInfo) bool
	EmitUnauth      func()
	AfterAdopt      func(provideraccounts.Account)
}

// Deps names the root capabilities used by provider discovery. Provider-
// specific probe constructors remain separate so Claude and Codex transactions
// cannot accidentally collapse into a lowest-common-denominator abstraction.
type Deps struct {
	CurrentSettings func() settings.Settings
	SettingsService func() *settings.Service
	ProviderBinary  func(providerName string) string
	Selection       func(providerName string) AccountSelection
	ProbeKey        func(providerName, binary, accountID string) provider.ProbeCacheKey
	ProbeKeyForEnv  func(binary, accountID string, customEnv map[string]string) provider.ProbeCacheKey
	RunAccountProbe func(AccountProbeRequest) (provider.AccountInfo, error)
	ClaudeConfig    func(binary string) claude.ProbeConfig
	CodexConfig     func(binary string) codex.ProbeConfig
	EmitStatus      func(providerstatus.Event)
	EmitRateLimits  func(provider.RateLimitsSnapshot)
	DetectProvider  func(providerName, binary string) provider.ProviderStatus
	ProbeClaude     func(context.Context, claude.ProbeConfig) (provider.AccountInfo, error)
	ProbeCodex      func(context.Context, codex.ProbeConfig) (provider.AccountInfo, error)
}

// Caches is the bounded process-wide discovery cache set. Probe answers are
// keyed by every environment dimension and expire; Codex model rows are TTL'd
// by binary. Sharing them avoids duplicate subprocesses when tests or startup
// glue briefly construct more than one App in one process.
type Caches struct {
	Claude      *provider.ProbeCache
	Codex       *provider.ProbeCache
	CodexModels *codexmodels.Cache
}

var (
	defaultCachesMu sync.Mutex
	defaultCaches   *Caches
)

// DefaultCaches returns the shared bounded discovery caches.
func DefaultCaches() *Caches {
	defaultCachesMu.Lock()
	defer defaultCachesMu.Unlock()
	if defaultCaches == nil {
		defaultCaches = newCaches()
	}
	return defaultCaches
}

// ResetDefaultCachesForTest replaces the shared caches. Callers must invoke it
// before constructing the App under test, never concurrently with a probe.
func ResetDefaultCachesForTest() {
	defaultCachesMu.Lock()
	defaultCaches = newCaches()
	defaultCachesMu.Unlock()
}

func newCaches() *Caches {
	return &Caches{
		Claude:      provider.NewProbeCache(claude.DefaultProbeTTL),
		Codex:       provider.NewProbeCache(codex.DefaultProbeTTL),
		CodexModels: codexmodels.New(),
	}
}

// Service owns provider-discovery coordination and its bounded caches.
type Service struct {
	deps   Deps
	caches *Caches
}

// New constructs a discovery service around explicit provider-specific ports.
func New(deps Deps, caches *Caches) *Service {
	if caches == nil {
		caches = DefaultCaches()
	}
	if deps.DetectProvider == nil {
		deps.DetectProvider = provider.DetectProvider
	}
	if deps.ProbeClaude == nil {
		deps.ProbeClaude = claude.ProbeAccount
	}
	if deps.ProbeCodex == nil {
		deps.ProbeCodex = codex.ProbeAccount
	}
	return &Service{deps: deps, caches: caches}
}
