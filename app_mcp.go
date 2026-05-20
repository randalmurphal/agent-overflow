package main

import (
	"fmt"
	"time"

	"agent-overflow/internal/claudeconfig"
	"agent-overflow/internal/codexconfig"
	"agent-overflow/internal/mcpprobe"
)

// MCP probe cache TTLs. Stdio entries take longer to refresh
// (subprocess fan-out) so we keep them around for 10 minutes; HTTP/SSE
// entries are cheap to re-check (sub-ms in the spike) but their status
// can change behind a kicked OAuth token, so 60s. Explicit Invalidate
// on CRUD + OAuth completion is what keeps the popup honest; TTL is
// the safety net.
const (
	mcpProbeStdioTTL = 10 * time.Minute
	mcpProbeHTTPTTL  = 60 * time.Second
)

// mcpProbe returns the lazy-init probe cache. Tests building a bare
// *App via &App{...} get a working cache on first call without
// pre-wiring; production wiring doesn't need an explicit init.
func (a *App) mcpProbe() *mcpprobe.Cache {
	a.mcpProbeCacheOnce.Do(func() {
		a.mcpProbeCache = mcpprobe.NewCache(mcpProbeStdioTTL, mcpProbeHTTPTTL)
	})
	return a.mcpProbeCache
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
