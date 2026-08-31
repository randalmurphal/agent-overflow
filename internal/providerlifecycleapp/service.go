package providerlifecycleapp

import (
	"context"
	"net/http"
	"sync"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/provideraccounts"
	"agent-overflow/internal/usagebackoff"
)

const (
	defaultProbeInterval = 2 * time.Minute
	probeMinInterval     = 30 * time.Second
)

// Selection is the managed account relevant to one provider-wide usage read.
type Selection struct {
	AccountID string
}

// RuntimeAccount is the sessionruntime projection needed for event
// attribution. The provider process remains the source of live session state.
type RuntimeAccount struct {
	Provider             string
	SessionToken         string
	CredentialGeneration uint64
	CredentialAccountID  string
	CredentialAccount    provider.AccountInfo
}

// SessionAccount is the provider account one live thread is actually using.
type SessionAccount struct {
	ThreadID   string
	Provider   string
	AccountID  string
	Account    provider.AccountInfo
	Generation uint64
}

// AccountDeps are metadata and credential transactions retained by
// provideraccountapp.
type AccountDeps struct {
	Selection         func(providerName string) Selection
	Reconcile         func(providerName string) error
	RefreshUsage      func(context.Context, string, string) error
	ResolveObserved   func(providerName, expectedAccountID string, info provider.AccountInfo) (string, *provideraccounts.Account, error)
	PublishObserved   func(providerName string, account provideraccounts.Account)
	Account           func(providerName, accountID string) (provideraccounts.Account, bool)
	List              func(providerName string) []provideraccounts.Account
	RememberRateLimit func(providerName, accountID string, snapshot provider.RateLimitsSnapshot) error
}

// SessionDeps are the narrow sessionruntime reads/writes used by lifecycle
// preprocessing. No provider I/O crosses this port.
type SessionDeps struct {
	Account        func(threadID string) (RuntimeAccount, bool)
	RecordActivity func(threadID, sessionToken string, kind provider.EventKind, content string, now time.Time)
}

// ClaudeDeps preserves Claude's HTTP usage path as a provider-specific port.
type ClaudeDeps struct {
	HTTPClient      func() *http.Client
	ProbeRateLimits func(context.Context, *http.Client) (provider.RateLimitsSnapshot, error)
}

// CodexDeps preserves Codex's app-server probe and identity reconciliation as
// a separate provider-specific path.
type CodexDeps struct {
	Binary      func() string
	ProbeConfig func(binary string) codex.ProbeConfig
	Probe       func(context.Context, codex.ProbeConfig) (provider.AccountInfo, error)
}

// Deps are lifecycle and projection capabilities supplied by root.
type Deps struct {
	Context        func() context.Context
	IsShuttingDown func() bool
	Emit           func(eventchan.Channel, any)
	Accounts       AccountDeps
	Sessions       SessionDeps
	Claude         ClaudeDeps
	Codex          CodexDeps
}

// Service owns every rate-limit cache, cadence, cooldown, and backoff ward.
type Service struct {
	deps Deps

	cacheMu sync.RWMutex
	cache   map[string]provider.RateLimitsSnapshot

	activityMu sync.Mutex
	activity   map[string]time.Time

	backoff usagebackoff.Ledger

	claudeGateOnce sync.Once
	claudeGate     *usageProbeGate
	codexGateOnce  sync.Once
	codexGate      *usageProbeGate
}

func New(deps Deps) *Service {
	return &Service{
		deps:     deps,
		cache:    make(map[string]provider.RateLimitsSnapshot),
		activity: make(map[string]time.Time),
	}
}

func (s *Service) context() context.Context {
	if s.deps.Context != nil {
		return s.deps.Context()
	}
	return context.Background()
}

func (s *Service) shuttingDown() bool {
	return s.deps.IsShuttingDown != nil && s.deps.IsShuttingDown()
}

// Backoff exposes the one durable account-scoped ledger to the managed-account
// usage transaction. The service retains its ownership and load path.
func (s *Service) Backoff() *usagebackoff.Ledger { return &s.backoff }

func (s *Service) Remaining(providerName, accountID string) time.Duration {
	return s.backoff.Remaining(providerName, accountID)
}

func (s *Service) Note(providerName, accountID string, err error) {
	s.backoff.Note(providerName, accountID, err)
}

// LoadBackoff restores durable server holds during startup.
func (s *Service) LoadBackoff(path string) { s.backoff.Load(path) }
