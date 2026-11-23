package providerdiscoveryapp

import (
	"context"
	"sync"

	"agent-overflow/internal/claudemodels"
	"agent-overflow/internal/codexmodels"
	"agent-overflow/internal/procutil"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/agy"
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
	// AfterAdopt runs once the runner has accepted and cached the identity,
	// before it emits `provider:account` for it.
	AfterAdopt func(provideraccounts.Account)
	// ResetCredits, for a Codex probe, returns the resets the probe's
	// rate-limit answer named and whether that answer arrived; the runner
	// records them on the account it adopts (provideraccountapp.ProbeRequest).
	ResetCredits func() (*codex.ResetCredits, bool)
}

// Deps names the root capabilities used by provider discovery. Provider-
// specific probe constructors remain separate so Claude and Codex transactions
// cannot accidentally collapse into a lowest-common-denominator abstraction.
// ClaudeProbe and CodexProbe return a canonical account probe's cache key and
// the configuration it runs with, from one read of the settings.
type Deps struct {
	// Reaper ends a model-list process if the backend dies first.
	Reaper                     procutil.Reaper
	CurrentSettings            func() settings.Settings
	SettingsService            func() *settings.Service
	ProviderBinary             func(providerName string) string
	Selection                  func(providerName string) AccountSelection
	ProbeKey                   func(providerName, binary, accountID string) provider.ProbeCacheKey
	ProbeKeyForEnv             func(binary, accountID string, customEnv map[string]string) provider.ProbeCacheKey
	RunAccountProbe            func(AccountProbeRequest) (provider.AccountInfo, error)
	ClaudeProbe                func(binary, accountID string) (provider.ProbeCacheKey, claude.ProbeConfig)
	CodexProbe                 func(binary, accountID string) (provider.ProbeCacheKey, codex.ProbeConfig)
	AgyProbe                   func(binary, accountID string) (provider.ProbeCacheKey, agy.ProbeConfig)
	EmitStatus                 func(providerstatus.Event)
	EmitRateLimits             func(provider.RateLimitsSnapshot)
	DetectProvider             func(providerName, binary string) provider.ProviderStatus
	ProbeClaude                func(context.Context, claude.ProbeConfig) (provider.AccountInfo, error)
	ProbeCodex                 func(context.Context, codex.ProbeConfig) (provider.AccountInfo, error)
	ProbeAgy                   func(context.Context, agy.ProbeConfig) (provider.AccountInfo, error)
	CheckClaudeTransferAccount func(context.Context, string) error
	CheckCodexTransferAccount  func(context.Context, string) error
	CheckAgyTransferAccount    func(context.Context, string) error
	// RememberClaudeCatalog persists the model answer of the probe identity
	// key (its adopted account included) so the next cold start serves the
	// enriched picker before its own probe lands. Optional: a focused test
	// may leave it nil.
	RememberClaudeCatalog func(key provider.ProbeCacheKey, snapshot claudemodels.Snapshot)
	// ProbeAgyModels performs a live zero-token model catalog probe against
	// the given Antigravity binary (borrowing a pooled *acp.Conn when live).
	ProbeAgyModels func(ctx context.Context, binary string) ([]provider.ModelInfo, error)
}

// Caches is the bounded process-wide discovery cache set. Probe answers are
// keyed by every environment dimension and expire; Codex and Agy model rows are
// TTL'd by binary. Sharing them avoids duplicate subprocesses when tests or
// startup glue briefly construct more than one App in one process.
type Caches struct {
	Claude      *provider.ProbeCache
	Codex       *provider.ProbeCache
	Agy         *provider.ProbeCache
	CodexModels *codexmodels.Cache
	AgyModels   *agy.CatalogCache
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
		defaultCaches = NewCaches()
	}
	return defaultCaches
}

// ResetDefaultCachesForTest replaces the shared caches. Callers must invoke it
// before constructing the App under test, never concurrently with a probe.
func ResetDefaultCachesForTest() {
	defaultCachesMu.Lock()
	defaultCaches = NewCaches()
	defaultCachesMu.Unlock()
}

// NewCaches returns empty discovery caches for a Service that must not share
// the process-wide ones.
func NewCaches() *Caches {
	return &Caches{
		Claude:      provider.NewProbeCache(claude.DefaultProbeTTL),
		Codex:       provider.NewProbeCache(codex.DefaultProbeTTL),
		Agy:         provider.NewProbeCache(codex.DefaultProbeTTL),
		CodexModels: codexmodels.New(),
		AgyModels:   agy.NewCatalogCache(),
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
	if deps.ProbeAgy == nil {
		deps.ProbeAgy = agy.ProbeAccount
	}
	return &Service{deps: deps, caches: caches}
}
