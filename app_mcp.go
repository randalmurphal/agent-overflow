package main

import (
	"fmt"
	"time"

	"agent-overflow/internal/claudeconfig"
	"agent-overflow/internal/codexconfig"
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
	a.mcpStatusCacheOnce.Do(func() {
		a.mcpStatusCache = mcpstatus.NewCache(mcpStatusCacheTTL, &appMCPStatusBus{app: a})
	})
	return a.mcpStatusCache
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
	b.app.emit("mcp:status", s)
}

// claudeConfig returns the lazy-init Claude config-file adapter bound
// to ~/.claude.json by default. Tests can pre-populate
// a.claudeConfigStore before calling this; production callers rely on
// the default path.
func (a *App) claudeConfig() (*claudeconfig.Store, error) {
	if a.claudeConfigStore != nil {
		return a.claudeConfigStore, nil
	}
	a.claudeConfigOnce.Do(func() {
		path, err := claudeconfig.DefaultPath()
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

// codexConfig returns the lazy-init Codex TOML adapter bound to
// ~/.codex/config.toml by default. Same test-injection pattern as
// claudeConfig.
func (a *App) codexConfig() (*codexconfig.Store, error) {
	if a.codexConfigStore != nil {
		return a.codexConfigStore, nil
	}
	a.codexConfigOnce.Do(func() {
		path, err := codexconfig.DefaultPath()
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
