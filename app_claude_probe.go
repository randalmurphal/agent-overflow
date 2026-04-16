package main

import (
	"context"
	"sync"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

// claudeProbeCache is a package-level cache shared across App instances
// within the process. Probe results depend only on the binary path and
// the ambient Claude auth state, not on App configuration, so a single
// cache per process is correct and mirrors how forge's capabilities
// probe behaves.
var (
	claudeProbeCacheOnce sync.Once
	claudeProbeCache     *claude.ProbeCache
)

func claudeAccountProbeCache() *claude.ProbeCache {
	claudeProbeCacheOnce.Do(func() {
		claudeProbeCache = claude.NewProbeCache(claude.DefaultProbeTTL)
	})
	return claudeProbeCache
}

// ProbeClaudeAccount spawns a short-lived Claude CLI subprocess with
// `--max-turns 0` and returns the authenticated account metadata from
// the emitted `system/init` message. Results are cached per binary path
// for 5 minutes. Zero tokens are consumed — the CLI aborts before any
// API call.
func (a *App) ProbeClaudeAccount() (provider.AccountInfo, error) {
	binary := a.providerBinaryPath(string(provider.Claude))

	cache := claudeAccountProbeCache()
	if cached, hit := cache.Get(binary); hit {
		return cached, nil
	}

	info, err := claude.ProbeAccount(context.Background(), claude.ProbeConfig{
		Binary: binary,
	})
	if err != nil {
		return provider.AccountInfo{}, err
	}

	cache.Set(binary, info)
	return info, nil
}
