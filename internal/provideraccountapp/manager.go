package provideraccountapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
	"agent-overflow/internal/settings"
)

// ProbeInvalidator evicts one provider identity cache entry after a managed
// credential change. Probe execution remains provider-specific Manager work.
type ProbeInvalidator interface {
	Invalidate(providerName string, key provider.ProbeCacheKey)
}

// RateLimitSink keeps the root-owned rate-limit cache and event stream behind
// the two operations account lifecycle needs.
type RateLimitSink interface {
	Publish(provider.RateLimitsSnapshot)
	Forget(providerName, accountID string)
}

// UsageBackoff is the account-scoped durable 429 ledger shared with the
// provider-wide legacy Claude probe.
type UsageBackoff interface {
	Remaining(providerName, accountID string) time.Duration
	Note(providerName, accountID string, err error)
}

// AccountSink publishes manager outcomes without giving account code access
// to root's event bus or wire DTOs.
type AccountSink interface {
	PublishAccount(providerName string, account provideraccounts.Account, info provider.AccountInfo, generation uint64)
	PublishCleared(providerName string, generation uint64)
	PublishUsageError(providerName, accountID string, err error)
}

// Deps are the narrow process/lifecycle ports used by Manager.
type Deps struct {
	Context           func() context.Context
	IsShuttingDown    func() bool
	ShutdownError     error
	CurrentSettings   func() settings.Settings
	ProviderBinary    func(providerName string) string
	BrowserExecutable func() (string, error)
	OpenBrowser       func(context.Context, string) error
	HTTPClient        func() *http.Client
	Sessions          SessionGateway
	Probes            ProbeInvalidator
	RateLimits        RateLimitSink
	Backoff           UsageBackoff
	Accounts          AccountSink
}

// Manager owns the complete managed-account consistency boundary. Every
// credential/store mutation, fingerprint, and reconcile lock lives here so a
// single-use credential rotation is one transaction under one lock domain.
type Manager struct {
	deps Deps

	mu           sync.RWMutex
	store        *provideraccounts.Store
	credentials  *provideraccounts.Credentials
	fingerprints map[string][32]byte

	claudeReconcileMu sync.Mutex
	codexReconcileMu  sync.Mutex
	auditPath         string
}

// NewManager constructs an unattached manager. Attach installs the two stores
// together during startup or focused-test setup; no App field retains either.
func NewManager(deps Deps) *Manager {
	return &Manager{deps: deps, fingerprints: make(map[string][32]byte)}
}

// Attach transfers the metadata and credential stores into the Manager.
// Startup calls it before any account goroutine is started.
func (m *Manager) Attach(store *provideraccounts.Store, credentials *provideraccounts.Credentials, auditPath string) error {
	if (store == nil) != (credentials == nil) {
		return errors.New("provider account metadata and credential stores must be attached together")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.store != nil || m.credentials != nil {
		return errors.New("provider account stores are already attached")
	}
	m.store = store
	m.credentials = credentials
	m.auditPath = auditPath
	return nil
}

func (m *Manager) available() bool {
	return m != nil && m.store != nil && m.credentials != nil
}

func (m *Manager) context() context.Context {
	if m.deps.Context != nil {
		return m.deps.Context()
	}
	return context.Background()
}

func (m *Manager) shuttingDown() bool {
	return m.deps.IsShuttingDown != nil && m.deps.IsShuttingDown()
}

func (m *Manager) shutdownError() error {
	if m.deps.ShutdownError != nil {
		return m.deps.ShutdownError
	}
	return errors.New("application is shutting down")
}

func (m *Manager) currentSettings() settings.Settings {
	if m.deps.CurrentSettings != nil {
		return m.deps.CurrentSettings()
	}
	return settings.Settings{}
}

func (m *Manager) providerBinaryPath(providerName string) string {
	if m.deps.ProviderBinary != nil {
		return m.deps.ProviderBinary(providerName)
	}
	return ""
}

func (m *Manager) reconcileMutex(providerName string) *sync.Mutex {
	switch providerName {
	case string(provider.Claude):
		return &m.claudeReconcileMu
	case string(provider.Codex):
		return &m.codexReconcileMu
	default:
		panic(fmt.Sprintf("provider-account reconcile mutex requested for unsupported provider %q", providerName))
	}
}

// Selection returns one provider's current managed selection.
func (m *Manager) Selection(providerName string) Selection {
	if m == nil {
		return Selection{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.selectionLocked(providerName)
}

func (m *Manager) selectionLocked(providerName string) Selection {
	if m.store == nil {
		return Selection{}
	}
	selection := Selection{Generation: m.store.Generation(providerName)}
	account, ok := m.store.Active(providerName, time.Now())
	if !ok {
		return selection
	}
	selection.AccountID = account.ID
	selection.Account = AccountInfo(account)
	return selection
}

// SelectionLease holds the Manager's read side across one provider write.
func (m *Manager) SelectionLease(providerName string) *SelectionLease {
	if m == nil {
		return NewSelectionLease(Selection{}, nil)
	}
	m.mu.RLock()
	return NewSelectionLease(m.selectionLocked(providerName), m.mu.RUnlock)
}

// Account returns saved account metadata without exposing the owned Store.
func (m *Manager) Account(providerName, accountID string) (provideraccounts.Account, bool) {
	if m == nil || m.store == nil {
		return provideraccounts.Account{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.store.Get(providerName, accountID, time.Now())
}

// ActiveAccount returns the selected metadata account.
func (m *Manager) ActiveAccount(providerName string) (provideraccounts.Account, bool) {
	if m == nil || m.store == nil {
		return provideraccounts.Account{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.store.Active(providerName, time.Now())
}

// Generation returns the current provider credential generation.
func (m *Manager) Generation(providerName string) uint64 {
	if m == nil || m.store == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.store.Generation(providerName)
}

// MetadataAccounts snapshots saved metadata for root-owned cache hydration.
func (m *Manager) MetadataAccounts(providerName string) []provideraccounts.Account {
	if m == nil || m.store == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.store.List(providerName, time.Now())
}

// List is the root cache-hydration read adapter. now is accepted to preserve
// the Store call shape while ownership stays behind Manager.
func (m *Manager) List(providerName string, now time.Time) []provideraccounts.Account {
	_ = now
	return m.MetadataAccounts(providerName)
}

// Get is the root session/event metadata read adapter.
func (m *Manager) Get(providerName, accountID string, now time.Time) (provideraccounts.Account, bool) {
	_ = now
	return m.Account(providerName, accountID)
}

// Active is the root attribution read adapter.
func (m *Manager) Active(providerName string, now time.Time) (provideraccounts.Account, bool) {
	_ = now
	return m.ActiveAccount(providerName)
}

// RememberRateLimits persists root's normalized last-known snapshot.
func (m *Manager) RememberRateLimits(providerName, accountID string, snapshot provider.RateLimitsSnapshot) error {
	if m == nil || m.store == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.store.RememberRateLimits(providerName, accountID, snapshot)
}

func (m *Manager) applySelection(providerName string, generation uint64, accountID string) {
	if m.deps.Sessions == nil {
		return
	}
	selection := Selection{Generation: generation, AccountID: accountID}
	if accountID != "" {
		if account, ok := m.Account(providerName, accountID); ok {
			selection.Account = AccountInfo(account)
		}
	}
	m.deps.Sessions.ApplySelection(providerName, selection)
}

func (m *Manager) publishRateLimits(snapshot provider.RateLimitsSnapshot) {
	if m.deps.RateLimits != nil {
		m.deps.RateLimits.Publish(snapshot)
	}
}

func (m *Manager) forgetRateLimits(providerName, accountID string) {
	if m.deps.RateLimits != nil {
		m.deps.RateLimits.Forget(providerName, accountID)
	}
}

// ReadCanonicalCredential is the narrow credential read used by MCP auth.
func (m *Manager) ReadCanonicalCredential(providerName string) (provideraccounts.CredentialSnapshot, bool, error) {
	if m == nil || m.credentials == nil {
		return provideraccounts.CredentialSnapshot{}, false, errors.New("provider credential store unavailable")
	}
	return m.readCanonicalCredentialIfPresent(providerName)
}
