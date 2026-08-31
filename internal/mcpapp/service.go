package mcpapp

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"agent-overflow/internal/claudeconfig"
	"agent-overflow/internal/codexconfig"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/logging"
	"agent-overflow/internal/mcpstatus"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
)

// Session is the provider-specific portion of one live App session.
type Session struct {
	Claude *claude.Session
	Codex  *codex.Session
}

// CodexLiveSession identifies a live Codex process for provider-global reloads.
type CodexLiveSession struct {
	ThreadID string
	Session  *codex.Session
}

// ClaudeLiveSession identifies a live Claude process in one workspace.
type ClaudeLiveSession struct {
	ThreadID string
	Session  *claude.Session
}

// Deps supplies root-owned lifecycle, session, credential, and event seams.
type Deps struct {
	Context              func() context.Context
	IsShuttingDown       func() bool
	StartSession         func(threadID string) error
	Session              func(threadID string) (Session, bool)
	CodexSessions        func() []CodexLiveSession
	ClaudeSessions       func(workspacePath string) []ClaudeLiveSession
	ProviderBinary       func(providerName string) string
	SessionProcessEnv    func(providerName string) map[string]string
	ReadClaudeCredential func() ([]byte, error)
	Emit                 func(eventchan.Channel, any)
	EmitThreadError      func(threadID, message string)
	EmitWireError        func(threadID, message string)
	Store                *store.Store
	Logger               *logging.Logger
	ClaudeConfig         *claudeconfig.Store
	ClaudeConfigPath     func() (string, error)
	CodexConfig          *codexconfig.Store
	CodexConfigPath      func() (string, error)
	StatusCache          *mcpstatus.Cache
	WorkspaceAuthStarter WorkspaceAuthStarter
	ShutdownError        error
}

// Service owns MCP configuration, status, OAuth, and live reload coordination.
type Service struct {
	deps   Deps
	store  *store.Store
	logger *logging.Logger

	claudeConfigOnce  sync.Once
	claudeConfigStore *claudeconfig.Store
	claudeConfigErr   error
	codexConfigOnce   sync.Once
	codexConfigStore  *codexconfig.Store
	codexConfigErr    error
	statusCacheOnce   sync.Once
	statusCache       *mcpstatus.Cache

	claudeOAuthPollsMu   sync.Mutex
	claudeOAuthPolls     map[string]*claudeMCPOAuthPoll
	workspaceAuthMu      sync.Mutex
	workspaceAuthFlows   map[workspaceMCPAuthKey]*workspaceMCPAuthRun
	workspaceAuthStarter WorkspaceAuthStarter
	codexReloadsMu       sync.Mutex
	codexReloads         map[string]*codexMCPReloadState
}

// New constructs an MCP application service from explicit root dependencies.
func New(deps Deps) *Service {
	return &Service{
		deps: deps, store: deps.Store, logger: deps.Logger,
		claudeConfigStore: deps.ClaudeConfig, codexConfigStore: deps.CodexConfig,
		statusCache: deps.StatusCache, workspaceAuthStarter: deps.WorkspaceAuthStarter,
	}
}

func (a *Service) lifeCtx() context.Context {
	if a != nil && a.deps.Context != nil {
		return a.deps.Context()
	}
	return context.Background()
}

func (a *Service) emit(channel eventchan.Channel, data any) {
	if a != nil && a.deps.Emit != nil {
		a.deps.Emit(channel, data)
	}
}

func (a *Service) providerBinaryPath(providerName string) string {
	if a == nil || a.deps.ProviderBinary == nil {
		return ""
	}
	return a.deps.ProviderBinary(providerName)
}

func (a *Service) sessionProcessEnv(providerName string) map[string]string {
	if a == nil || a.deps.SessionProcessEnv == nil {
		return nil
	}
	return a.deps.SessionProcessEnv(providerName)
}

func (a *Service) shutdownError() error {
	if a != nil && a.deps.ShutdownError != nil {
		return a.deps.ShutdownError
	}
	return context.Canceled
}

func (a *Service) session(threadID string) (Session, bool) {
	if a == nil || a.deps.Session == nil {
		return Session{}, false
	}
	return a.deps.Session(threadID)
}

func (a *Service) isShuttingDown() bool {
	return a != nil && a.deps.IsShuttingDown != nil && a.deps.IsShuttingDown()
}

func (a *Service) hasActiveSession(threadID string) bool {
	_, active := a.session(threadID)
	return active
}

func (a *Service) startSession(threadID string) error {
	if a == nil || a.deps.StartSession == nil {
		return ErrMCPSessionUnavailable
	}
	return a.deps.StartSession(threadID)
}

func (a *Service) emitErrorToThread(threadID, message string) {
	if a != nil && a.deps.EmitThreadError != nil {
		a.deps.EmitThreadError(threadID, message)
	}
}

func (a *Service) emitWireErrorToThread(threadID, message string) {
	if a != nil && a.deps.EmitWireError != nil {
		a.deps.EmitWireError(threadID, message)
	}
}

// mcpStatusCacheTTL bounds how long a provider-derived status entry
// stays "fresh" before the popup will hit the ephemeral fetcher on
// next read. Live thread sessions overwrite entries continuously
// (free path), so this only matters for inactive-thread reads. 30s
// keeps the popup snappy on rapid open/close without re-spawning the
// CLI every time, while ensuring a stale "needs-auth" after a real
// sign-in flips within a popup-open of the OAuth invalidate firing.
const mcpStatusCacheTTL = 30 * time.Second

// mcpStatus returns the lazy-init status cache. Tests building a
// bare *Service via &App{...} get a working cache on first call without
// pre-wiring; production wiring doesn't need an explicit init.
func (a *Service) mcpStatus() *mcpstatus.Cache {
	a.statusCacheOnce.Do(func() {
		if a.statusCache == nil {
			a.statusCache = mcpstatus.NewCache(mcpStatusCacheTTL, &appMCPStatusBus{app: a})
		}
	})
	return a.statusCache
}

// StatusCache exposes the shared status cache to the root session-event
// adapter. Provider notifications and binding reads must use the same cache.
func (a *Service) StatusCache() *mcpstatus.Cache {
	return a.mcpStatus()
}

// appMCPStatusBus wires every cache Put / Invalidate into the
// `mcp:status` event channel so the frontend store can update
// reactively without polling. Failure to emit (transport not yet
// wired during early startup) is silent — the cache state still
// stands; the UI hydrates on the next ListMcpServerStatuses call.
type appMCPStatusBus struct {
	app *Service
}

func (b *appMCPStatusBus) Emit(s mcpstatus.ServerStatus) {
	if b.app == nil {
		return
	}
	b.app.emit(eventchan.MCPStatus, s)
}

// claudeConfig returns the lazy-init Claude config-file adapter bound
// to ~/.claude.json by default. Tests can pre-populate
// a.claudeConfigStore before calling this; production callers rely on
// the default path.
func (a *Service) claudeConfig() (*claudeconfig.Store, error) {
	if a.claudeConfigStore != nil {
		return a.claudeConfigStore, nil
	}
	a.claudeConfigOnce.Do(func() {
		var path string
		var err error
		if a.deps.ClaudeConfigPath != nil {
			path, err = a.deps.ClaudeConfigPath()
		} else {
			var home string
			home, err = os.UserHomeDir()
			if err == nil {
				path = claudeconfig.PathForHome(home)
			}
		}
		if err != nil {
			a.claudeConfigErr = err
			return
		}
		a.claudeConfigStore = claudeconfig.New(path)
	})
	if a.claudeConfigErr != nil {
		return nil, fmt.Errorf("claude config: %w", a.claudeConfigErr)
	}
	return a.claudeConfigStore, nil
}

// ClaudeConfig exposes the provider-native config adapter to adjacent root
// services that share Claude's MCP configuration source.
func (a *Service) ClaudeConfig() (*claudeconfig.Store, error) {
	return a.claudeConfig()
}

// codexConfig returns the lazy-init Codex TOML adapter bound to
// ~/.codex/config.toml by default. Same test-injection pattern as
// claudeConfig.
func (a *Service) codexConfig() (*codexconfig.Store, error) {
	if a.codexConfigStore != nil {
		return a.codexConfigStore, nil
	}
	a.codexConfigOnce.Do(func() {
		var path string
		var err error
		if a.deps.CodexConfigPath != nil {
			path, err = a.deps.CodexConfigPath()
		} else {
			var home string
			home, err = os.UserHomeDir()
			if err == nil {
				path = codexconfig.PathForHome(home)
			}
		}
		if err != nil {
			a.codexConfigErr = err
			return
		}
		a.codexConfigStore = codexconfig.New(path)
	})
	if a.codexConfigErr != nil {
		return nil, fmt.Errorf("codex config: %w", a.codexConfigErr)
	}
	return a.codexConfigStore, nil
}

// CodexConfig exposes the provider-native config adapter for focused root
// integration tests and adjacent adapters.
func (a *Service) CodexConfig() (*codexconfig.Store, error) {
	return a.codexConfig()
}
