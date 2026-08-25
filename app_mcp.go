package main

import (
	"fmt"
	"time"

	"agent-overflow/internal/claudeconfig"
	"agent-overflow/internal/codexconfig"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/mcpstatus"
)

// mcpStatusCacheTTL bounds how long a provider-derived status entry
// stays "fresh" before the popup will hit the ephemeral fetcher on
// next read. Live thread sessions overwrite entries continuously
// (free path), so this only matters for inactive-thread reads. 30s
// keeps the popup snappy on rapid open/close without re-spawning the
// CLI every time, while ensuring a stale "needs-auth" after a real
// sign-in flips within a popup-open of the OAuth invalidate firing.
const mcpStatusCacheTTL = 30 * time.Second

// mcpStatus returns the lazy-init status cache. Tests building a
// bare *App via &App{...} get a working cache on first call without
// pre-wiring; production wiring doesn't need an explicit init.
func (a *App) mcpStatus() *mcpstatus.Cache {
	a.mcp.statusCacheOnce.Do(func() {
		a.mcp.statusCache = mcpstatus.NewCache(mcpStatusCacheTTL, &appMCPStatusBus{app: a})
	})
	return a.mcp.statusCache
}

// appMCPStatusBus wires every cache Put / Invalidate into the
// `mcp:status` event channel so the frontend store can update
// reactively without polling. Failure to emit (transport not yet
// wired during early startup) is silent — the cache state still
// stands; the UI hydrates on the next ListMcpServerStatuses call.
type appMCPStatusBus struct {
	app *App
}

func (b *appMCPStatusBus) Emit(s mcpstatus.ServerStatus) {
	if b.app == nil {
		return
	}
	b.app.emit(eventchan.MCPStatus, s)
}

// claudeConfig returns the lazy-init Claude config-file adapter bound
// to ~/.claude.json by default. Tests can pre-populate
// a.mcp.claudeConfigStore before calling this; production callers rely on
// the default path.
func (a *App) claudeConfig() (*claudeconfig.Store, error) {
	if a.mcp.claudeConfigStore != nil {
		return a.mcp.claudeConfigStore, nil
	}
	a.mcp.claudeConfigOnce.Do(func() {
		path, err := claudeconfig.DefaultPath()
		if err != nil {
			a.mcp.claudeConfigErr = err
			return
		}
		a.mcp.claudeConfigStore = claudeconfig.New(path)
	})
	if a.mcp.claudeConfigErr != nil {
		return nil, fmt.Errorf("claude config: %w", a.mcp.claudeConfigErr)
	}
	return a.mcp.claudeConfigStore, nil
}

// codexConfig returns the lazy-init Codex TOML adapter bound to
// ~/.codex/config.toml by default. Same test-injection pattern as
// claudeConfig.
func (a *App) codexConfig() (*codexconfig.Store, error) {
	if a.mcp.codexConfigStore != nil {
		return a.mcp.codexConfigStore, nil
	}
	a.mcp.codexConfigOnce.Do(func() {
		path, err := codexconfig.DefaultPath()
		if err != nil {
			a.mcp.codexConfigErr = err
			return
		}
		a.mcp.codexConfigStore = codexconfig.New(path)
	})
	if a.mcp.codexConfigErr != nil {
		return nil, fmt.Errorf("codex config: %w", a.mcp.codexConfigErr)
	}
	return a.mcp.codexConfigStore, nil
}
