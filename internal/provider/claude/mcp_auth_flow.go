package claude

import (
	"context"
	"fmt"
	"sync"
	"time"

	"agent-overflow/internal/provider"
)

// MCPAuthConfig describes the temporary Claude process used only for an MCP
// OAuth round trip. ReadCredential protects Claude's startup token rotation
// before the process is closed. Production callers must provide it.
type MCPAuthConfig struct {
	Config
	ReadCredential func() ([]byte, error)
	RequestTimeout time.Duration
}

// MCPAuthFlow owns the temporary provider process and its loopback OAuth
// listener. It deliberately exposes only the MCP controls needed by the flow,
// so the app cannot accidentally use it as an untracked chat session.
type MCPAuthFlow struct {
	session   *Session
	watch     rotationWatch
	closeOnce sync.Once
	closeErr  error
}

// StartMCPAuth starts a temporary Claude process and begins the named server's
// OAuth handshake. It creates no Agent Overflow thread and sends no model turn.
func StartMCPAuth(ctx context.Context, cfg MCPAuthConfig, serverName string) (*MCPAuthFlow, *MCPAuthResult, error) {
	if err := provider.ValidateProbeWorkDir("claude", cfg.WorkDir); err != nil {
		return nil, nil, err
	}
	if cfg.ReadCredential == nil {
		return nil, nil, fmt.Errorf("claude: mcp auth credential reader required")
	}
	if cfg.RequestTimeout <= 0 {
		return nil, nil, fmt.Errorf("claude: mcp auth request timeout must be positive")
	}
	watch := armRotationWatch(cfg.ReadCredential, false, time.Now())
	// Closing stdin is what can lose a startup credential rotation. Keep the
	// process lifetime independent from caller cancellation when the watch is
	// armed. The owner still closes it on every terminal flow and on shutdown.
	spawnCtx := ctx
	if watch.budget() > 0 {
		spawnCtx = context.WithoutCancel(ctx)
	}
	session, err := NewSession(spawnCtx, "mcp-auth", cfg.Config, nil)
	if err != nil {
		return nil, nil, err
	}
	flow := &MCPAuthFlow{session: session, watch: watch}
	requestCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
	defer cancel()
	result, err := session.AuthenticateMCP(requestCtx, serverName)
	if err != nil {
		_ = flow.Close()
		return nil, nil, err
	}
	return flow, result, nil
}

// QueryStatus reads the temporary process's live MCP state.
func (f *MCPAuthFlow) QueryStatus(ctx context.Context) ([]MCPServerStatus, error) {
	if f == nil || f.session == nil {
		return nil, fmt.Errorf("claude: mcp auth flow unavailable")
	}
	return f.session.QueryMCPStatus(ctx)
}

// Close waits for any startup credential rotation to reach durable storage,
// then tears down the temporary provider process and callback listener.
func (f *MCPAuthFlow) Close() error {
	if f == nil {
		return nil
	}
	f.closeOnce.Do(func() {
		settleCtx, cancel := context.WithTimeout(context.Background(), rotationSettleTimeout)
		f.watch.settle(settleCtx)
		cancel()
		if f.session != nil {
			f.closeErr = f.session.Close()
		}
	})
	return f.closeErr
}
